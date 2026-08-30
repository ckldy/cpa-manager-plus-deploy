package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	baiModelsURL    = "https://api.b.ai/v1/models"
	baiBalanceProbe = "https://api.b.ai/v1/models" // authenticated models call doubles as credential check
)

var (
	baiStatusMu    sync.Mutex
	baiStatusCache = map[string]baiSnapshot{}
)

type baiSnapshot struct {
	Refreshed time.Time
	Error     string
	Models    []string
}

type baiStatusPage struct {
	Accounts []baiStatusAccount
	Models   []string
	Active   int
	Disabled int
}

type baiStatusAccount struct {
	AuthIndex string
	Name      string
	Label     string
	State     string
	Detail    string
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	JSON      json.RawMessage `json:"json"`
}

// handleManagement renders the plugin status page and handles its actions.
func handleManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	path := strings.TrimSuffix(req.Path, "/")
	path = strings.TrimPrefix(path, "/v0/resource/plugins/bai")
	path = strings.TrimPrefix(path, "/v0/management/plugins/bai")
	if path == "/status" && req.Method == http.MethodGet {
		page, err := loadBAIStatus()
		if err != nil {
			return managementResponse(http.StatusServiceUnavailable, "text/plain; charset=utf-8", []byte("无法读取 B.AI 账号状态："+err.Error()))
		}
		if req.Query.Get("refresh") == "1" {
			refreshBAISnapshots(page)
		}
		ensureProbeAsync(req.Query.Get("probe") == "1")
		return managementResponse(http.StatusOK, "text/html; charset=utf-8", renderBAIStatusPage(page))
	}
	return managementResponse(http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found"))
}

func loadBAIStatus() (baiStatusPage, error) {
	page := baiStatusPage{}
	raw, err := callHostRPC(pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return page, err
	}
	var list hostAuthListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return page, err
	}
	for _, file := range list.Files {
		if !strings.EqualFold(strings.TrimSpace(file.Provider), ProviderBAI) {
			continue
		}
		account := baiStatusAccount{
			AuthIndex: file.AuthIndex,
			Name:      file.Name,
			Label:     file.Label,
			State:     "active",
			Detail:    "凭证就绪",
		}
		if file.Disabled {
			account.State = "disabled"
			account.Detail = "已禁用"
			page.Disabled++
		} else {
			page.Active++
		}
		page.Accounts = append(page.Accounts, account)
	}
	sort.Slice(page.Accounts, func(i, j int) bool { return page.Accounts[i].Name < page.Accounts[j].Name })
	return page, nil
}

// refreshBAISnapshots calls /v1/models with each stored key: a 200 both
// validates the credential and returns the live model list.
func refreshBAISnapshots(page baiStatusPage) {
	for _, account := range page.Accounts {
		if account.AuthIndex == "" || account.State == "disabled" {
			continue
		}
		snapshot := refreshBAISnapshot(account.AuthIndex)
		baiStatusMu.Lock()
		baiStatusCache[account.AuthIndex] = snapshot
		baiStatusMu.Unlock()
	}
}

func refreshBAISnapshot(authIndex string) baiSnapshot {
	raw, err := callHostRPC(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return baiSnapshot{Refreshed: time.Now(), Error: "无法读取账号凭证"}
	}
	var response hostAuthGetResponse
	var storage authStorage
	if json.Unmarshal(raw, &response) != nil || json.Unmarshal(response.JSON, &storage) != nil {
		return baiSnapshot{Refreshed: time.Now(), Error: "无法读取账号凭证"}
	}
	if strings.TrimSpace(storage.APIKey) == "" {
		return baiSnapshot{Refreshed: time.Now(), Error: "B.AI API Key 缺失"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	resp, err := executeHTTP(ctx, nil, "", pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     baiModelsURL,
		Headers: baiHeaders(storage.APIKey),
	})
	if err != nil {
		return baiSnapshot{Refreshed: time.Now(), Error: "刷新网络失败"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return baiSnapshot{Refreshed: time.Now(), Error: "凭据校验失败（" + classifyUpstreamStatus(resp.StatusCode, resp.Body) + "）"}
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(resp.Body, &payload) != nil {
		return baiSnapshot{Refreshed: time.Now(), Error: "模型列表响应无法解析"}
	}
	models := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			models = append(models, id)
		}
	}
	sort.Strings(models)
	return baiSnapshot{Refreshed: time.Now(), Models: models}
}

func renderBAIStatusPage(page baiStatusPage) []byte {
	probe := currentProbeSnapshot()
	probeStateText := "未探测"
	if probe.Running {
		probeStateText = "探测中"
	} else if !probe.Finished.IsZero() {
		probeStateText = "最近完成于 " + probe.Finished.Local().Format("2006-01-02 15:04")
		if probe.Error != "" {
			probeStateText += " · " + probe.Error
		} else {
			probeStateText += " · 下次自动更新 " + probe.Finished.Add(probeTTL).Local().Format("2006-01-02 15:04")
		}
	}
	groups := map[probeClass][]string{probeFree: {}, probePremium: {}, probeUnknown: {}}
	for _, id := range catalogModelIDs() {
		class := probeUnknown
		if entry, ok := probe.Entries[id]; ok {
			class = entry.Class
		}
		groups[class] = append(groups[class], id)
	}
	formatGroup := func(class probeClass) string {
		if len(groups[class]) == 0 {
			return "无"
		}
		escaped := make([]string, len(groups[class]))
		for i, id := range groups[class] {
			escaped[i] = html.EscapeString(id)
		}
		return strings.Join(escaped, "、")
	}

	var accounts strings.Builder
	if len(page.Accounts) == 0 {
		accounts.WriteString(`<div class="empty">还没有 B.AI 凭证。在 CPA 凭证管理中导入 <code>bai:你的API_KEY</code> 或纯 Key 文本。</div>`)
	}
	for _, account := range page.Accounts {
		stateLabel, stateClass := "可路由", "good"
		if account.State == "disabled" {
			stateLabel, stateClass = "已禁用", "muted"
		}
		snapshotText := "尚未刷新"
		baiStatusMu.Lock()
		snapshot, ok := baiStatusCache[account.AuthIndex]
		baiStatusMu.Unlock()
		if ok {
			if snapshot.Error != "" {
				snapshotText = snapshot.Error
			} else {
				snapshotText = fmt.Sprintf("上游可用模型 %d 个 · 刷新于 %s", len(snapshot.Models), snapshot.Refreshed.Local().Format("15:04"))
			}
		}
		accounts.WriteString(fmt.Sprintf(`<article class="account-card"><div class="card-head"><div><h3>%s</h3><p>%s</p></div><span class="state %s">%s</span></div><p class="card-detail">%s</p></article>`,
			html.EscapeString(account.Name), html.EscapeString(account.Label), stateClass, stateLabel, html.EscapeString(snapshotText)))
	}
	modelList := "（刷新后显示上游实时模型列表）"
	baiStatusMu.Lock()
	for _, snapshot := range baiStatusCache {
		if snapshot.Error == "" && len(snapshot.Models) > 0 {
			escaped := make([]string, len(snapshot.Models))
			for i, m := range snapshot.Models {
				escaped[i] = html.EscapeString(m)
			}
			modelList = strings.Join(escaped, "、")
			break
		}
	}
	baiStatusMu.Unlock()
	body := fmt.Sprintf(`<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>B.AI 账号状态</title><style>
:root{color-scheme:light dark;--bg:#f5f7fa;--panel:#fff;--line:#d7dee7;--text:#17202a;--muted:#667381;--green:#16865d;--blue:#0969da;--button:#fff;--button-line:#c7d0da;--good-bg:#dff7eb;--good-text:#116b4c;--muted-bg:#e9edf2;--muted-text:#52606d}@media (prefers-color-scheme:dark){:root{--bg:#101418;--panel:#171d23;--line:#2b353f;--text:#edf2f6;--muted:#9ba9b5;--green:#37c58a;--blue:#72b8ff;--button:#222b34;--button-line:#3a4651;--good-bg:#173b30;--good-text:#75e1b0;--muted-bg:#2b333b;--muted-text:#bac5cf}}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}main{max-width:900px;margin:0 auto;padding:28px 22px 48px}h1{font-size:24px}p{color:var(--muted);line-height:1.6}.refresh{display:inline-block;background:var(--button);border:1px solid var(--button-line);color:var(--text);border-radius:6px;padding:9px 12px;text-decoration:none;margin-right:8px}.account-card{background:var(--panel);border:1px solid var(--line);border-radius:9px;padding:17px;margin-top:14px}.state{display:inline-block;border-radius:999px;padding:4px 9px;font-size:12px;font-weight:700}.good{background:var(--good-bg);color:var(--good-text)}.muted{background:var(--muted-bg);color:var(--muted-text)}.card-detail{color:var(--muted);font-size:12px}.empty{border:1px dashed var(--line);border-radius:9px;color:var(--muted);padding:30px;text-align:center}code{color:var(--blue)}.models{background:var(--panel);border:1px solid var(--line);border-radius:9px;padding:16px;margin-top:22px;line-height:1.8;overflow-wrap:anywhere}.free{color:var(--green)}</style></head><body><main><header><h1>B.AI 账号状态</h1><p>已注册的 B.AI API Key、上游可用性与实时模型列表。凭证内容不会显示在此页面。</p><a class="refresh" href="?refresh=1">刷新状态</a><a class="refresh" href="?probe=1">强制探测免押模型</a></header><div>%s</div><section class="models"><strong>免押探测</strong><p>%s</p><p class="free"><strong>free：</strong>%s</p><p><strong>premium：</strong>%s</p><p><strong>unknown：</strong>%s</p><p><small>模型名称中的 (free) 表示当前账号最近一次可靠探测无需充值，不代表永久免费。</small></p></section><section class="models"><strong>上游模型列表</strong><p>%s</p></section></main></body></html>`,
		accounts.String(), html.EscapeString(probeStateText), formatGroup(probeFree), formatGroup(probePremium), formatGroup(probeUnknown), modelList)
	return []byte(body)
}

func managementResponse(status int, contentType string, body []byte) ([]byte, error) {
	headers := http.Header{
		"Cache-Control": {"no-store"},
		"Content-Type":  {contentType},
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Body:       body,
		Headers:    headers,
	})
}
