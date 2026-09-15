package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type probeClass string

const (
	probeFree    probeClass = "free"
	probePremium probeClass = "premium"
	probeUnknown probeClass = "unknown"
	probeTTL                = 24 * time.Hour
	probeDelay              = 2 * time.Second
)

type probeEntry struct {
	Class     probeClass
	CheckedAt time.Time
}

type probeSnapshot struct {
	Entries   map[string]probeEntry
	Running   bool
	StartedAt time.Time
	Finished  time.Time
	Error     string
}

var (
	probeMu    sync.RWMutex
	probeState = seededProbeSnapshot()
)

func seededProbeSnapshot() probeSnapshot {
	now := time.Now()
	entries := map[string]probeEntry{}
	// 2026-09-14 measured reality: these call fine on a zero-balance account.
	for _, id := range []string{"qwen3.8-flash", "hy3", "mimo-v2.5"} {
		entries[id] = probeEntry{Class: probeFree, CheckedAt: now}
	}
	// glm-5.3-flash became credit-gated (400 insufficient_user_quota, 102
	// credits/call): label it premium immediately after restart so the status
	// page does not claim it free. fallback.go still keeps it switchable.
	entries["glm-5.3-flash"] = probeEntry{Class: probePremium, CheckedAt: now}
	return probeSnapshot{Entries: entries}
}

func resetProbeStateForTest() {
	probeMu.Lock()
	probeState = seededProbeSnapshot()
	probeMu.Unlock()
}

func setProbeClassForTest(id string, class probeClass) {
	probeMu.Lock()
	probeState.Entries[id] = probeEntry{Class: class, CheckedAt: time.Now()}
	probeMu.Unlock()
}

func probeClassForModel(id string) probeClass {
	probeMu.RLock()
	entry, ok := probeState.Entries[id]
	probeMu.RUnlock()
	if !ok {
		return probeUnknown
	}
	return entry.Class
}

func currentProbeSnapshot() probeSnapshot {
	probeMu.RLock()
	defer probeMu.RUnlock()
	out := probeState
	out.Entries = make(map[string]probeEntry, len(probeState.Entries))
	for id, entry := range probeState.Entries {
		out.Entries[id] = entry
	}
	return out
}

func probeCacheFresh(now, finished time.Time) bool {
	return !finished.IsZero() && now.Sub(finished) < probeTTL
}

func applyProbeResult(old probeEntry, class probeClass, checkedAt time.Time) probeEntry {
	if class == probeUnknown && (old.Class == probeFree || old.Class == probePremium) {
		return old
	}
	return probeEntry{Class: class, CheckedAt: checkedAt}
}

func classifyProbeResponse(resp pluginapi.HTTPResponse) probeClass {
	text := strings.ToLower(string(resp.Body))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var payload struct {
			Choices []json.RawMessage `json:"choices"`
		}
		if json.Unmarshal(resp.Body, &payload) == nil && len(payload.Choices) > 0 {
			return probeFree
		}
		return probeUnknown
	}
	if resp.StatusCode == http.StatusForbidden && (strings.Contains(text, "deposit required") || strings.Contains(text, "access_denied")) {
		return probePremium
	}
	if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusPaymentRequired) && upstreamQuotaExceeded(text) {
		return probePremium
	}
	return probeUnknown
}

func makeProbePayload(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     false,
		"max_tokens": 16,
		"messages": []map[string]string{{
			"role": "user", "content": "Reply OK.",
		}},
	})
	return body
}

func ensureProbeAsync(force bool) bool {
	probeMu.Lock()
	if probeState.Running || (!force && probeState.Error == "" && probeCacheFresh(time.Now(), probeState.Finished)) {
		probeMu.Unlock()
		return false
	}
	probeMu.Unlock()

	// Read the credential synchronously while the host RPC request is active;
	// the background goroutine performs ordinary Go HTTP only and never calls
	// host RPC after the management/model request has returned.
	_, apiKey, errText := firstActiveBAICredential()
	if errText != "" {
		probeMu.Lock()
		probeState.Error = errText
		probeState.Finished = time.Now()
		probeMu.Unlock()
		return false
	}

	probeMu.Lock()
	if probeState.Running {
		probeMu.Unlock()
		return false
	}
	probeState.Running = true
	probeState.StartedAt = time.Now()
	probeState.Error = ""
	probeMu.Unlock()
	go runProbe(apiKey)
	return true
}

func runProbe(apiKey string) {
	finish := func(errText string) {
		probeMu.Lock()
		probeState.Running = false
		probeState.Finished = time.Now()
		probeState.Error = errText
		probeMu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	modelsResp, err := executeHTTP(ctx, nil, "", pluginapi.HTTPRequest{
		Method: http.MethodGet, URL: baiModelsURL, Headers: baiHeaders(apiKey),
	})
	cancel()
	if err != nil || modelsResp.StatusCode < 200 || modelsResp.StatusCode >= 300 {
		finish("无法获取上游模型列表")
		return
	}
	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(modelsResp.Body, &listed) != nil {
		finish("上游模型列表无法解析")
		return
	}
	upstreamIDs := make([]string, 0, len(listed.Data))
	available := make(map[string]bool, len(listed.Data))
	for _, item := range listed.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		available[id] = true
		upstreamIDs = append(upstreamIDs, id)
	}

	probeMu.Lock()
	for id := range probeState.Entries {
		if !available[id] {
			delete(probeState.Entries, id)
		}
	}
	probeMu.Unlock()

	ids := probeCandidateIDs(upstreamIDs)
	for i, id := range ids {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		resp, callErr := executeHTTP(ctx, nil, "", pluginapi.HTTPRequest{
			Method: http.MethodPost, URL: baiUpstreamURL, Headers: baiHeaders(apiKey), Body: makeProbePayload(id),
		})
		cancel()
		if callErr != nil {
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			finish("B.AI 凭据无效")
			return
		}
		class := classifyProbeResponse(resp)
		now := time.Now()
		probeMu.Lock()
		old := probeState.Entries[id]
		probeState.Entries[id] = applyProbeResult(old, class, now)
		probeMu.Unlock()
		if resp.StatusCode == http.StatusTooManyRequests {
			finish("上游限流，本轮已停止并保留旧结果")
			return
		}
		if i+1 < len(ids) {
			time.Sleep(probeDelay)
		}
	}
	finish("")
}

func firstActiveBAICredential() (authIndex, apiKey, errText string) {
	raw, err := callHostRPC(pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return "", "", "无法读取 B.AI 凭据列表"
	}
	var list hostAuthListResponse
	if json.Unmarshal(raw, &list) != nil {
		return "", "", "B.AI 凭据列表无法解析"
	}
	for _, file := range list.Files {
		if file.Disabled || !strings.EqualFold(strings.TrimSpace(file.Provider), ProviderBAI) || file.AuthIndex == "" {
			continue
		}
		raw, err = callHostRPC(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: file.AuthIndex})
		if err != nil {
			continue
		}
		var response hostAuthGetResponse
		var storage authStorage
		if json.Unmarshal(raw, &response) == nil && json.Unmarshal(response.JSON, &storage) == nil && strings.TrimSpace(storage.APIKey) != "" {
			return file.AuthIndex, storage.APIKey, ""
		}
	}
	return "", "", "没有可用的 B.AI 凭据"
}

func probeCandidateIDs(upstreamIDs []string) []string {
	seen := make(map[string]struct{}, len(upstreamIDs))
	ids := make([]string, 0, len(upstreamIDs))
	for _, rawID := range upstreamIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func catalogModelIDs() []string {
	ids := []string{
		"glm-5.3-flash", "deepseek-v4-flash", "qwen3.8-flash", "hy3",
		"claude-opus-4.8", "claude-opus-5", "claude-sonnet-4.6", "claude-sonnet-5", "claude-haiku-4.5",
		"gpt-5.6-terra", "gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4",
		"gemini-3.1-pro", "gemini-3.5-flash", "gemini-3.6-flash",
		"deepseek-v4-pro", "glm-5.3", "glm-5.2", "kimi-k3", "minimax-m3", "qwen3.8-max",
	}
	sort.Strings(ids)
	return ids
}
