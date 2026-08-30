package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 动态端点映射（routing.go）
//
// 读取 ZCode 控制面 GET https://zcode.z.ai/api/v1/agent/configs 返回的
// data.proxyEndpoint.mapping（from → to 数组），把匹配的请求 URL 精确改写为
// 服务端下发的目标端点。
//
// 约束：
//   - 成功快照缓存 5 分钟；失败冷却 30 秒；拉取超时 3 秒；最多 256 条映射。
//   - exact URL 匹配：协议 + 小写 host + 端口（默认 443）+ 去尾斜杠路径。
//   - 目标 host 仅允许 open.bigmodel.cn / api.z.ai / zcode.z.ai。
//   - fail-open：任何拉取/解析/匹配失败都保留固定端点，绝不改写。
//   - 观察模式（默认）：只记录命中映射，不切换端点。
//
// 本文件复用 executor.go 的 executeHTTP 与 identity.go 的控制面身份头；
// executor 主链路在发送前统一调用 resolve，默认 observe、显式 active 才改写。
const (
	agentConfigsURL        = "https://zcode.z.ai/api/v1/agent/configs"
	routingSuccessTTL      = 5 * time.Minute
	routingFailureCooldown = 30 * time.Second
	routingRequestTimeout  = 3 * time.Second
	routingMaxMapping      = 256
	routingMaxObservations = 64
	routingObserveMode     = "observe"
	routingActiveMode      = "active"
)

// routingAllowedTargetHosts is the allowlist for mapping targets. Any other
// host (e.g. an attacker-controlled server) is rejected, keeping credentials on
// official Z.AI / BigModel endpoints.
var routingAllowedTargetHosts = map[string]bool{
	"open.bigmodel.cn": true,
	"api.z.ai":         true,
	"zcode.z.ai":       true,
}

type routingEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type routingSnapshot struct {
	ExpiresAt time.Time
	Mapping   map[string]string // routing key -> target URL (without query)
}

type routedURL struct {
	Routed bool
	URL    string
}

type routingObservation struct {
	From string
	To   string
	At   time.Time
}

type routingStatus struct {
	Mode        string
	Snapshot    bool
	Entries     int
	ExpiresAt   time.Time
	LastSuccess time.Time
	LastError   string
	Observed    []routingObservation
	Fixed       []routingFixedPreview
}

// routingFixedPreview describes what the current snapshot would do to one of the
// plugin's fixed upstream endpoints (used by the observe-only management panel).
type routingFixedPreview struct {
	Name string
	From string
	To   string // mapped target; empty means keep the fixed endpoint
}

// endpointRouting is the process-wide routing state. mu guards snapshot /
// cooldown / mode / observations; refreshMu guards the single-flight refresh.
type endpointRouting struct {
	mu          sync.RWMutex
	snapshot    *routingSnapshot
	retryAfter  time.Time
	lastSuccess time.Time
	lastError   string
	mode        string
	observed    []routingObservation

	now    func() time.Time
	httpDo func(ctx context.Context, client pluginapi.HostHTTPClient, hostCallbackID string, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)

	refreshMu   sync.Mutex
	refreshing  bool
	refreshDone chan struct{}
}

func newEndpointRouting(mode string) *endpointRouting {
	if mode != routingActiveMode {
		mode = routingObserveMode
	}
	return &endpointRouting{
		mode:   mode,
		now:    time.Now,
		httpDo: executeHTTP,
	}
}

// endpointRouter is the default process-wide router in observe mode: it fetches
// and records server mappings but only switches endpoints after explicit active mode.
var endpointRouter = newEndpointRouting(routingObserveMode)

// normalizeRoutingPath strips trailing slashes; a lone "/" stays "/".
func normalizeRoutingPath(path string) string {
	if path == "" || path == "/" {
		return "/"
	}
	if trimmed := strings.TrimRight(path, "/"); trimmed != "" {
		return trimmed
	}
	return "/"
}

// routingKey builds the exact-match lookup key:
// scheme://lowercase-host:port + normalized path. Only https URLs are routable.
func routingKey(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Scheme != "https" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port + normalizeRoutingPath(u.Path), true
}

// validPlainHTTPS reports whether raw is a plain https URL with no userinfo,
// query or fragment (mirrors the reference client's mapping validation).
func validPlainHTTPS(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return true
}

// parseRoutingMapping decodes the agent-configs body into a routing table. Any
// malformed entry, non-zero business code, duplicate from-key, disallowed target
// host or oversized table rejects the whole snapshot (fail-open).
func parseRoutingMapping(body []byte) (map[string]string, error) {
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Data struct {
			ProxyEndpoint struct {
				Mapping []routingEntry `json:"mapping"`
			} `json:"proxyEndpoint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode agent configs: %w", err)
	}
	code, err := parseBusinessCode(envelope.Code)
	if err != nil {
		return nil, fmt.Errorf("agent configs business code: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("agent configs business code %d", code)
	}
	entries := envelope.Data.ProxyEndpoint.Mapping
	if len(entries) > routingMaxMapping {
		return nil, fmt.Errorf("agent configs mapping too large: %d entries", len(entries))
	}
	mapping := make(map[string]string, len(entries))
	for _, entry := range entries {
		if !validPlainHTTPS(entry.From) {
			return nil, fmt.Errorf("agent configs mapping.from is not a plain https URL: %q", entry.From)
		}
		fromKey, ok := routingKey(entry.From)
		if !ok {
			return nil, fmt.Errorf("agent configs mapping.from invalid: %q", entry.From)
		}
		if !validPlainHTTPS(entry.To) {
			return nil, fmt.Errorf("agent configs mapping.to is not a plain https URL: %q", entry.To)
		}
		toURL, err := url.Parse(entry.To)
		if err != nil {
			return nil, fmt.Errorf("agent configs mapping.to invalid: %q", entry.To)
		}
		if !routingAllowedTargetHosts[strings.ToLower(toURL.Hostname())] {
			return nil, fmt.Errorf("agent configs mapping.to host %q is not allowed", toURL.Hostname())
		}
		if toURL.Port() != "" && toURL.Port() != "443" {
			return nil, fmt.Errorf("agent configs mapping.to must use HTTPS port 443")
		}
		if _, dup := mapping[fromKey]; dup {
			return nil, fmt.Errorf("agent configs mapping duplicate from: %q", entry.From)
		}
		mapping[fromKey] = toURL.Scheme + "://" + toURL.Host + normalizeRoutingPath(toURL.Path)
	}
	return mapping, nil
}

// controlPlaneConfigHeaders returns the header set for the unauthenticated
// agent-configs fetch: the identity set minus X-ZCode-Agent (mirroring the
// official client's config-fetch set) plus an explicit Accept.
func controlPlaneConfigHeaders() http.Header {
	h := buildControlPlaneHeaders("")
	h.Del("X-ZCode-Agent")
	h.Set("Accept", "application/json")
	return h
}

// resolve applies the routing table to rawURL. It never throws: on any error or
// miss it returns the original URL. In observe mode it records the would-be
// rewrite and still returns the original URL.
func (r *endpointRouting) resolve(ctx context.Context, client pluginapi.HostHTTPClient, hostCallbackID, rawURL string) routedURL {
	key, ok := routingKey(rawURL)
	if !ok {
		return routedURL{Routed: false, URL: rawURL}
	}
	r.ensureFresh(ctx, client, hostCallbackID)

	r.mu.RLock()
	snapshot := r.snapshot
	mode := r.mode
	r.mu.RUnlock()

	if snapshot == nil {
		return routedURL{Routed: false, URL: rawURL}
	}
	target, matched := snapshot.Mapping[key]
	if !matched {
		return routedURL{Routed: false, URL: rawURL}
	}
	rewritten := applyRoutingQuery(target, rawURL)
	if mode != routingActiveMode {
		r.recordObservation(rawURL, rewritten)
		return routedURL{Routed: false, URL: rawURL}
	}
	return routedURL{Routed: true, URL: rewritten}
}

// lookup returns the current snapshot's mapping for rawURL without triggering a
// refresh and without recording an observation (read-only preview).
func (r *endpointRouting) lookup(rawURL string) (string, bool) {
	key, ok := routingKey(rawURL)
	if !ok {
		return "", false
	}
	r.mu.RLock()
	snapshot := r.snapshot
	r.mu.RUnlock()
	if snapshot == nil {
		return "", false
	}
	target, ok := snapshot.Mapping[key]
	return target, ok
}

// applyRoutingQuery rewrites target's URL to carry the request's query string,
// matching the reference client (rewritten.search = parsed.search).
func applyRoutingQuery(target, rawURL string) string {
	requestURL, err := url.Parse(rawURL)
	if err != nil {
		return target
	}
	t, err := url.Parse(target)
	if err != nil {
		return target
	}
	t.RawQuery = requestURL.RawQuery
	return t.String()
}

func (r *endpointRouting) recordObservation(from, to string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, routingObservation{From: from, To: to, At: r.now()})
	if len(r.observed) > routingMaxObservations {
		r.observed = r.observed[len(r.observed)-routingMaxObservations:]
	}
}

// ensureFresh refreshes the snapshot when it is stale or the failure cooldown
// has elapsed; otherwise it returns immediately (cache hit).
func (r *endpointRouting) ensureFresh(ctx context.Context, client pluginapi.HostHTTPClient, hostCallbackID string) {
	r.mu.RLock()
	now := r.now()
	fresh := (r.snapshot != nil && now.Before(r.snapshot.ExpiresAt)) || now.Before(r.retryAfter)
	r.mu.RUnlock()
	if fresh {
		return
	}
	r.runRefresh(ctx, client, hostCallbackID)
}

// runRefresh executes a single refresh, coalescing concurrent callers onto one
// in-flight request.
func (r *endpointRouting) runRefresh(ctx context.Context, client pluginapi.HostHTTPClient, hostCallbackID string) {
	r.refreshMu.Lock()
	if r.refreshing {
		done := r.refreshDone
		r.refreshMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	r.refreshing = true
	r.refreshDone = make(chan struct{})
	r.refreshMu.Unlock()

	r.doRefresh(ctx, client, hostCallbackID)

	r.refreshMu.Lock()
	r.refreshing = false
	close(r.refreshDone)
	r.refreshDone = nil
	r.refreshMu.Unlock()
}

// doRefresh fetches and applies the agent-configs mapping. Success installs a
// fresh snapshot (5 min TTL); any failure arms the 30 s failure cooldown while
// the previous snapshot (if any) stays in place (fail-open).
func (r *endpointRouting) doRefresh(ctx context.Context, client pluginapi.HostHTTPClient, hostCallbackID string) {
	reqCtx, cancel := context.WithTimeout(ctx, routingRequestTimeout)
	defer cancel()
	resp, err := r.httpDo(reqCtx, client, hostCallbackID, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     agentConfigsURL,
		Headers: controlPlaneConfigHeaders(),
	})
	now := r.now()
	if err != nil {
		r.setRefreshFailure(now, "agent configs 拉取失败")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.setRefreshFailure(now, fmt.Sprintf("agent configs HTTP %d", resp.StatusCode))
		return
	}
	mapping, perr := parseRoutingMapping(resp.Body)
	if perr != nil {
		r.setRefreshFailure(now, perr.Error())
		return
	}
	r.mu.Lock()
	r.snapshot = &routingSnapshot{ExpiresAt: now.Add(routingSuccessTTL), Mapping: mapping}
	r.retryAfter = time.Time{}
	r.lastSuccess = now
	r.lastError = ""
	r.mu.Unlock()
}

func (r *endpointRouting) setRefreshFailure(now time.Time, message string) {
	r.mu.Lock()
	r.retryAfter = now.Add(routingFailureCooldown)
	r.lastError = message
	r.mu.Unlock()
}

func (r *endpointRouting) status() routingStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := routingStatus{
		Mode:        r.mode,
		Snapshot:    r.snapshot != nil,
		LastSuccess: r.lastSuccess,
		LastError:   r.lastError,
	}
	if r.snapshot != nil {
		st.Entries = len(r.snapshot.Mapping)
		st.ExpiresAt = r.snapshot.ExpiresAt
	}
	st.Observed = append([]routingObservation(nil), r.observed...)
	return st
}

func (r *endpointRouting) setMode(mode string) error {
	if mode != routingActiveMode && mode != routingObserveMode {
		return fmt.Errorf("invalid routing mode %q", mode)
	}
	r.mu.Lock()
	r.mode = mode
	r.mu.Unlock()
	return nil
}

// ---- package-level helpers (management / tests) ----

// forceRefreshEndpointRouting bypasses the success TTL and failure cooldown so a
// manual "拉取映射" always hits the server. Returns an error only when the fetch
// failed and there is no prior snapshot to fall back on.
func forceRefreshEndpointRouting(ctx context.Context, client pluginapi.HostHTTPClient, hostCallbackID string) error {
	endpointRouter.runRefresh(ctx, client, hostCallbackID)
	endpointRouter.mu.RLock()
	hasSnapshot := endpointRouter.snapshot != nil
	errText := endpointRouter.lastError
	endpointRouter.mu.RUnlock()
	if !hasSnapshot && errText != "" {
		return fmt.Errorf("%s", errText)
	}
	return nil
}

func endpointRoutingMode() string {
	endpointRouter.mu.RLock()
	defer endpointRouter.mu.RUnlock()
	return endpointRouter.mode
}

func setEndpointRoutingMode(mode string) error {
	return endpointRouter.setMode(mode)
}

func endpointRoutingStatus() routingStatus {
	st := endpointRouter.status()
	st.Fixed = fixedRoutingPreviews()
	return st
}

// fixedRoutingPreviews reports, for each of the plugin's fixed upstream
// endpoints, what the current snapshot would map it to (empty = keep fixed).
func fixedRoutingPreviews() []routingFixedPreview {
	endpoints := []struct{ name, raw string }{
		{"Coding Plan", codingPlanUpstreamURL},
		{"API Key", apiKeyUpstreamURL},
		{"BigModel", bigModelCodingUpstreamURL},
	}
	out := make([]routingFixedPreview, 0, len(endpoints))
	for _, e := range endpoints {
		p := routingFixedPreview{Name: e.name, From: e.raw}
		if to, ok := endpointRouter.lookup(e.raw); ok {
			p.To = to
		}
		out = append(out, p)
	}
	return out
}
