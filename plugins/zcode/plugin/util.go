package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	oauthCallbackPath     = "/v0/resource/plugins/zcode/callback"
	oauthCallbackMaxBody  = 8 * 1024
	oauthCallbackMaxField = 4096
	oauthBridgeOrigin     = "https://chat.z.ai"
	// 模板值：构建前替换为你自己部署的公开 Origin（用于回调 Origin 白名单）。
	oauthFirstPartyOrigin = "https://cpa.example.com"
)

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	JSON      json.RawMessage `json:"json"`
}

type zcodeQuotaSnapshot struct {
	Total     *float64
	Used      *float64
	Remaining *float64
	Plan      string
	Money     bool
	Refreshed time.Time
	Error     string
}

var zcodeQuotas = struct {
	sync.RWMutex
	items map[string]zcodeQuotaSnapshot
}{items: make(map[string]zcodeQuotaSnapshot)}

var quotaHTTPDo = executeHTTP

type zcodeStatusAccount struct {
	AuthIndex  string
	FileName   string
	Name       string
	Credential string
	State      string
	Detail     string
	UpdatedAt  time.Time
	Success    int64
	Failed     int64
}

type zcodeStatusPage struct {
	Accounts    []zcodeStatusAccount
	Active      int
	Unavailable int
	Disabled    int
	NoEvidence  int
	Success     int64
	Failed      int64
	QuotaTried  int
	QuotaOK     int
	QuotaFailed int
}

func loadZCodeStatus() (zcodeStatusPage, error) {
	raw, err := callHostRPC(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return zcodeStatusPage{}, err
	}
	var list hostAuthListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return zcodeStatusPage{}, fmt.Errorf("decode credential list: %w", err)
	}

	page := zcodeStatusPage{}
	for _, entry := range list.Files {
		if !strings.EqualFold(entry.Provider, ProviderZCode) && !strings.EqualFold(entry.Type, ProviderZCode) {
			continue
		}
		account := zcodeStatusAccount{
			AuthIndex: entry.AuthIndex,
			FileName:  entry.Name,
			Name:      firstString(entry.Label, entry.Account, entry.Email, entry.Name, "ZCode account"),
			State:     "active",
			Detail:    firstString(entry.StatusMessage, "等待连通性验证"),
			UpdatedAt: entry.UpdatedAt,
			Success:   entry.Success,
			Failed:    entry.Failed,
		}
		if account.UpdatedAt.IsZero() {
			account.UpdatedAt = entry.ModTime
		}
		if entry.Disabled {
			account.State, account.Detail = "disabled", "已在 CPA 中禁用"
			page.Disabled++
		} else if entry.Unavailable {
			account.State, account.Detail = "unavailable", firstString(entry.StatusMessage, "CPA 当前标记为不可用")
			page.Unavailable++
		} else {
			page.Active++
		}
		if entry.Success == 0 && entry.Failed == 0 {
			page.NoEvidence++
		}
		account.Credential = credentialKind(entry.AuthIndex)
		page.Accounts = append(page.Accounts, account)
		page.Success += entry.Success
		page.Failed += entry.Failed
	}
	sort.Slice(page.Accounts, func(i, j int) bool { return page.Accounts[i].Name < page.Accounts[j].Name })
	return page, nil
}

const (
	zcodeBillingCurrentURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/current?app_version=3.10.0&platform=linux-x64"
	zcodeBillingBalanceURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"
	bigModelQuotaURL       = "https://open.bigmodel.cn/api/monitor/usage/quota/limit"
	bigModelBalanceURL     = "https://open.bigmodel.cn/api/biz/account/query-customer-account-report"
)

func refreshZCodeQuotas(page zcodeStatusPage) (tried, succeeded, failed int) {
	for _, account := range page.Accounts {
		if account.AuthIndex == "" {
			continue
		}
		tried++
		snapshot := refreshZCodeQuota(account.AuthIndex)
		zcodeQuotas.Lock()
		zcodeQuotas.items[account.AuthIndex] = snapshot
		zcodeQuotas.Unlock()
		if snapshot.Error == "" {
			succeeded++
		} else {
			failed++
		}
	}
	return
}

func refreshZCodeQuota(authIndex string) zcodeQuotaSnapshot {
	raw, err := callHostRPC(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return zcodeQuotaSnapshot{Refreshed: time.Now(), Error: "无法读取账号凭证"}
	}
	var response hostAuthGetResponse
	var storage authStorage
	if json.Unmarshal(raw, &response) != nil || json.Unmarshal(response.JSON, &storage) != nil {
		return zcodeQuotaSnapshot{Refreshed: time.Now(), Error: "无法读取账号凭证"}
	}
	if strings.EqualFold(storage.Provider, "bigmodel") {
		return refreshBigModelQuota(storage.APIKey)
	}
	if strings.TrimSpace(storage.ZCodeJWTToken) == "" {
		return zcodeQuotaSnapshot{Refreshed: time.Now(), Error: "仅 Coding Plan JWT 支持额度刷新"}
	}
	headers := zcodeHeaders(storage.ZCodeJWTToken, storage.CaptchaVerifyParam, storage.CaptchaVerifyRegion)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	current, err := quotaHTTPDo(ctx, nil, "", pluginapi.HTTPRequest{Method: http.MethodGet, URL: zcodeBillingCurrentURL, Headers: headers})
	if err != nil || current.StatusCode < 200 || current.StatusCode >= 300 {
		return zcodeQuotaSnapshot{Refreshed: time.Now(), Error: quotaRefreshError(current, err)}
	}
	balance, err := quotaHTTPDo(ctx, nil, "", pluginapi.HTTPRequest{Method: http.MethodGet, URL: zcodeBillingBalanceURL, Headers: headers})
	if err != nil || balance.StatusCode < 200 || balance.StatusCode >= 300 {
		return zcodeQuotaSnapshot{Refreshed: time.Now(), Error: quotaRefreshError(balance, err)}
	}
	return parseQuotaSnapshot(current.Body, balance.Body)
}

func refreshBigModelQuota(apiKey string) zcodeQuotaSnapshot {
	if strings.TrimSpace(apiKey) == "" {
		return zcodeQuotaSnapshot{Refreshed: time.Now(), Error: "BigModel API Key 缺失"}
	}
	headers := http.Header{"Authorization": []string{"Bearer " + apiKey}, "Accept": []string{"application/json"}}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	quotaResp, err := quotaHTTPDo(ctx, nil, "", pluginapi.HTTPRequest{Method: http.MethodGet, URL: bigModelQuotaURL, Headers: headers})
	if err != nil || quotaResp.StatusCode < 200 || quotaResp.StatusCode >= 300 {
		return zcodeQuotaSnapshot{Refreshed: time.Now(), Error: quotaRefreshError(quotaResp, err)}
	}
	balanceResp, err := quotaHTTPDo(ctx, nil, "", pluginapi.HTTPRequest{Method: http.MethodGet, URL: bigModelBalanceURL, Headers: headers})
	if err != nil || balanceResp.StatusCode < 200 || balanceResp.StatusCode >= 300 {
		return zcodeQuotaSnapshot{Refreshed: time.Now(), Error: quotaRefreshError(balanceResp, err)}
	}
	return parseBigModelQuotaSnapshot(quotaResp.Body, parseBigModelBalanceSnapshot(balanceResp.Body))
}

func parseBigModelBalanceSnapshot(body []byte) *zcodeQuotaSnapshot {
	var envelope struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"msg"`
		Data    struct {
			Balance          *float64 `json:"balance"`
			RechargeAmount   *float64 `json:"rechargeAmount"`
			TotalSpendAmount *float64 `json:"totalSpendAmount"`
			AvailableBalance *float64 `json:"availableBalance"`
		} `json:"data"`
	}
	out := &zcodeQuotaSnapshot{Refreshed: time.Now(), Plan: "BigModel 国内余额", Money: true}
	if json.Unmarshal(body, &envelope) != nil {
		out.Error = "BigModel 余额响应无法解析"
		return out
	}
	if !envelope.Success {
		out.Error = firstString(strings.TrimSpace(envelope.Message), fmt.Sprintf("BigModel 余额接口错误 %d", envelope.Code))
		return out
	}
	out.Remaining = envelope.Data.AvailableBalance
	if out.Remaining == nil {
		out.Remaining = envelope.Data.Balance
	}
	if envelope.Data.RechargeAmount != nil {
		out.Total = envelope.Data.RechargeAmount
	}
	out.Used = envelope.Data.TotalSpendAmount
	if out.Remaining == nil {
		out.Error = "BigModel 余额响应缺少可用余额"
	}
	return out
}

func parseBigModelQuotaSnapshot(body []byte, balance *zcodeQuotaSnapshot) zcodeQuotaSnapshot {
	var envelope struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"msg"`
		Data    struct {
			Level  string `json:"level"`
			Limits []struct {
				Type          string   `json:"type"`
				Unit          int      `json:"unit"`
				Number        int      `json:"number"`
				Usage         *float64 `json:"usage"`
				Current       *float64 `json:"currentValue"`
				Remaining     *float64 `json:"remaining"`
				Percentage    *float64 `json:"percentage"`
				NextResetTime int64    `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	out := zcodeQuotaSnapshot{Refreshed: time.Now()}
	if json.Unmarshal(body, &envelope) != nil {
		out.Error = "BigModel 额度响应无法解析"
		return out
	}
	if !envelope.Success {
		msg := strings.TrimSpace(envelope.Message)
		if strings.Contains(strings.ToLower(msg), "不存在coding plan") || strings.Contains(msg, "不存在 Coding Plan") {
			if balance != nil {
				if balance.Error != "" {
					out.Plan = "BigModel · 未开通 Coding Plan"
					out.Error = balance.Error
					return out
				}
				out.Plan = balance.Plan
				out.Remaining, out.Total, out.Used, out.Money = balance.Remaining, balance.Total, balance.Used, balance.Money
				return out
			}
			out.Plan = "BigModel · 未开通 Coding Plan"
			return out
		}
		out.Error = firstString(msg, fmt.Sprintf("BigModel 额度接口错误 %d", envelope.Code))
		return out
	}
	out.Plan = "BigModel Coding Plan"
	if envelope.Data.Level != "" {
		out.Plan += " · " + strings.ToUpper(envelope.Data.Level)
	}
	var weekly string
	for _, limit := range envelope.Data.Limits {
		isFiveHour := (limit.Type == "CREDIT_LIMIT" || limit.Type == "TOKENS_LIMIT") && limit.Unit == 3 && limit.Number == 5
		isWeekly := (limit.Type == "CREDIT_LIMIT" || limit.Type == "TOKENS_LIMIT") && limit.Unit == 6 && limit.Number == 1
		if isFiveHour {
			out.Total, out.Used, out.Remaining = limit.Usage, limit.Current, limit.Remaining
			if out.Total == nil && limit.Current != nil && limit.Remaining != nil {
				v := *limit.Current + *limit.Remaining
				out.Total = &v
			}
		}
		if isWeekly && limit.Percentage != nil {
			weekly = fmt.Sprintf(" · 周额度已用 %.0f%%", *limit.Percentage)
		}
	}
	out.Plan += weekly
	return out
}

func quotaRefreshError(resp pluginapi.HTTPResponse, err error) string {
	if err != nil {
		return "额度刷新网络失败"
	}
	switch classifyUpstreamStatus(resp.StatusCode, resp.Body) {
	case "credential_invalid":
		return "凭证无效或已过期"
	case "captcha_required":
		return "需要更新官方验证码参数"
	case "insufficient_balance":
		return "余额或资源包不足"
	case "upstream_unavailable":
		return "上游服务暂不可用"
	}
	return fmt.Sprintf("额度接口返回 HTTP %d", resp.StatusCode)
}

func parseQuotaSnapshot(currentBody, balanceBody []byte) zcodeQuotaSnapshot {
	var current, balance struct {
		Data struct {
			Plans []struct {
				Name   string `json:"name"`
				PlanID string `json:"plan_id"`
				Status string `json:"status"`
			} `json:"plans"`
			Balances []struct {
				Total     *float64 `json:"total_units"`
				Used      *float64 `json:"used_units"`
				Remaining *float64 `json:"remaining_units"`
			} `json:"balances"`
		} `json:"data"`
	}
	_ = json.Unmarshal(currentBody, &current)
	_ = json.Unmarshal(balanceBody, &balance)
	out := zcodeQuotaSnapshot{Refreshed: time.Now()}
	for _, plan := range current.Data.Plans {
		if strings.EqualFold(plan.Status, "active") {
			out.Plan = firstString(plan.Name, plan.PlanID)
			break
		}
	}
	for _, item := range balance.Data.Balances {
		if item.Total != nil {
			if out.Total == nil {
				v := 0.0
				out.Total = &v
			}
			*out.Total += *item.Total
		}
		if item.Used != nil {
			if out.Used == nil {
				v := 0.0
				out.Used = &v
			}
			*out.Used += *item.Used
		}
		if item.Remaining != nil {
			if out.Remaining == nil {
				v := 0.0
				out.Remaining = &v
			}
			*out.Remaining += *item.Remaining
		}
	}
	return out
}

func credentialKind(authIndex string) string {
	if strings.TrimSpace(authIndex) == "" {
		return "凭证类型未知"
	}
	raw, err := callHostRPC(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return "凭证类型未知"
	}
	var response hostAuthGetResponse
	if json.Unmarshal(raw, &response) != nil {
		return "凭证类型未知"
	}
	var storage authStorage
	if json.Unmarshal(response.JSON, &storage) != nil {
		return "凭证类型未知"
	}
	if storage.ZCodeJWTToken != "" {
		if storage.CaptchaVerifyParam != "" {
			return "Coding Plan JWT · 已附验证码参数"
		}
		return "Coding Plan JWT · 需验证码参数"
	}
	if storage.APIKey != "" {
		if strings.EqualFold(storage.Provider, "bigmodel") {
			return "BigModel 国内 · 余额 API Key"
		}
		return "Z.AI API Key"
	}
	return "凭证类型未知"
}

func renderZCodeStatusPage(page zcodeStatusPage) []byte {
	var accounts strings.Builder
	if len(page.Accounts) == 0 {
		accounts.WriteString(`<tr><td colspan="5" class="empty">还没有 ZCode 凭证。在 CPA 凭证管理中添加 API Key 或完成 ZCode OAuth 授权。</td></tr>`)
	}
	for _, account := range page.Accounts {
		stateLabel, stateClass := "可路由", "good"
		if account.State == "disabled" {
			stateLabel, stateClass = "已禁用", "muted"
		} else if account.State == "unavailable" {
			stateLabel, stateClass = "不可用", "bad"
		}
		updated := "未记录"
		if !account.UpdatedAt.IsZero() {
			updated = account.UpdatedAt.Local().Format("2006-01-02 15:04")
		}
		evidence := "无请求证据"
		if account.Success > 0 {
			evidence = "有成功请求"
		} else if account.Failed > 0 {
			evidence = "有失败请求"
		}
		accounts.WriteString(fmt.Sprintf(`<tr data-state="%s"><td><strong>%s</strong><small>%s</small></td><td>%s</td><td><span class="state %s">%s</span><small>%s</small></td><td><strong>%d / %d</strong><small>%s</small></td><td>%s</td></tr>`, account.State, html.EscapeString(account.Name), html.EscapeString(account.Credential), html.EscapeString(account.Credential), stateClass, stateLabel, html.EscapeString(account.Detail), account.Success, account.Failed, evidence, updated))
	}
	models := zcodeModelIDs()
	var modelRows strings.Builder
	poolEvidence, evidenceClass := "尚无账号池请求证据", "neutral"
	if page.Success > 0 {
		poolEvidence, evidenceClass = "账号池近期成功", "good"
	} else if page.Failed > 0 {
		poolEvidence, evidenceClass = "账号池近期失败", "warn"
	}
	for _, model := range models {
		if strings.HasPrefix(model, "zcode-offpeak-") {
			status, detail := "unsupported", "Off-Peak 默认关闭；仅在显式启用且账号同时具备 JWT + Coding Plan API Key 时支持。实验限制：无独立 cancel；settle 仅尝试一次且失败即整次失败。"
			if currentRouteConfig().OffPeakEnabled {
				status, detail = "experimental", "Off-Peak 已显式启用；独立有界执行，不会回退到普通或付费路径。上游 SSE 当前先有界缓冲，run 完成后再输出，并非实时流；settle 仅尝试一次。"
			}
			modelRows.WriteString(fmt.Sprintf(`<tr><td><code>%s</code></td><td><span class="state warn">%s</span></td><td><span class="state neutral">%s</span><small>%s</small></td></tr>`, model, status, poolEvidence, detail))
			continue
		}
		modelRows.WriteString(fmt.Sprintf(`<tr><td><code>%s</code></td><td><span class="state neutral">已暴露</span></td><td><span class="state %s">%s</span><small>模型级独立检测尚未执行</small></td></tr>`, model, evidenceClass, poolEvidence))
	}
	balanceState := "等待首次验证"
	balanceClass := "neutral"
	if page.Failed > 0 && page.Success == 0 {
		balanceState, balanceClass = "需要检查上游限制", "warn"
	}
	return []byte(fmt.Sprintf(`<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>ZCode 账号状态</title><style>
:root{color-scheme:dark;--bg:#101418;--panel:#171d23;--line:#2b353f;--text:#edf2f6;--muted:#9ba9b5;--green:#37c58a;--amber:#e5af3d;--red:#ec6d68;--blue:#72b8ff}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}main{max-width:1180px;margin:0 auto;padding:28px 22px 48px}.top{display:flex;justify-content:space-between;gap:20px;align-items:flex-start;border-bottom:1px solid var(--line);padding-bottom:20px}.eyebrow{color:var(--blue);font-size:12px;font-weight:700;text-transform:uppercase}.top h1{font-size:24px;margin:5px 0 6px}.top p{color:var(--muted);margin:0;line-height:1.6}.refresh{background:#222b34;border:1px solid #3a4651;color:var(--text);border-radius:6px;padding:9px 12px;text-decoration:none;white-space:nowrap}.metrics{display:grid;grid-template-columns:repeat(4,1fr);gap:1px;background:var(--line);border:1px solid var(--line);margin:22px 0}.metric{background:var(--panel);padding:15px}.metric b{display:block;font-size:24px}.metric span{color:var(--muted);font-size:12px}.section{margin-top:28px}.section h2{font-size:16px;margin:0 0 10px}.section p{color:var(--muted);margin:0 0 12px;line-height:1.55}.tablewrap{border:1px solid var(--line);overflow:auto;background:var(--panel)}table{width:100%%;border-collapse:collapse;min-width:760px}th,td{text-align:left;padding:13px 14px;border-bottom:1px solid var(--line);vertical-align:top}th{color:var(--muted);font-size:12px;font-weight:600;background:#141a20}tr:last-child td{border-bottom:0}strong{display:block;font-weight:600}small{display:block;color:var(--muted);font-size:12px;margin-top:4px;line-height:1.45}.state{display:inline-block;border-radius:999px;padding:3px 8px;font-size:12px;font-weight:600}.good{background:#173b30;color:#75e1b0}.bad{background:#442424;color:#ffaaa5}.muted{background:#2b333b;color:#bac5cf}.neutral{background:#20384d;color:#9bd2ff}.warn{background:#493916;color:#ffd57a}.notice{border-left:3px solid var(--amber);background:#211e16;color:#e7d5a3;padding:14px 16px;line-height:1.6;overflow-wrap:anywhere;word-break:break-word}.notice strong{margin-bottom:10px}.notice p{margin:8px 0 0;color:#e7d5a3}.empty{text-align:center;color:var(--muted);padding:32px}@media(max-width:680px){main{padding:20px 14px}.top{display:block}.refresh{display:inline-block;margin-top:14px}.metrics{grid-template-columns:repeat(2,1fr)}.top h1{font-size:21px}}
</style></head><body><main><header class="top"><div><div class="eyebrow">Provider health</div><h1>ZCode 账号状态</h1><p>CPA 中已注册的 ZCode 凭证、路由状态与模型可用性。</p></div><a class="refresh" href="?refresh=1">刷新状态</a></header><section class="metrics"><div class="metric"><b>%d</b><span>已注册账号</span></div><div class="metric"><b>%d</b><span>可路由账号</span></div><div class="metric"><b>%d</b><span>不可用 / 已禁用</span></div><div class="metric"><b>%d</b><span>近期成功请求</span></div><div class="metric"><b>%d</b><span>近期失败请求</span></div><div class="metric"><b>%d</b><span>无请求证据</span></div></section><section class="section"><h2>账号池</h2><p>凭证内容不会显示在此页面。状态、更新时间和请求计数由 CPA 宿主提供；更新时间不等同于最近调用时间。</p><div class="filters"><button class="filter active" data-filter="all">全部 <span>%d</span></button><button class="filter" data-filter="active">可路由 <span>%d</span></button><button class="filter" data-filter="unavailable">不可用 <span>%d</span></button><button class="filter" data-filter="disabled">已禁用 <span>%d</span></button></div><div class="tablewrap"><table><thead><tr><th>账号</th><th>凭证路径</th><th>状态</th><th>近期请求</th><th>宿主更新时间</th></tr></thead><tbody>%s</tbody></table></div></section><section class="section"><h2>模型矩阵</h2><p>状态基于账号池的近期请求计数，不将账号池证据误报为单个模型已验证。模型级独立检测尚未执行。</p><div class="tablewrap"><table><thead><tr><th>模型</th><th>CPA 路由</th><th>验证状态</th></tr></thead><tbody>%s</tbody></table></div></section><section class="section"><h2>当前处理建议</h2><div class="notice"><strong class="state %s">%s</strong><p><b>余额或资源包：</b>若上游返回 <code>429 / 1113</code>，表示 Z.AI 账户余额或资源包不足，请在官方账户补充资源。</p><p><b>Coding Plan JWT：</b>该路径还需要官方客户端产生的有效验证码参数。</p><p>页面不会尝试生成、保存或显示验证码参数。自动签到未接入：尚未发现可验证的官方写接口。</p></div></section></main><script>document.querySelectorAll('.filter').forEach(function(button){button.addEventListener('click',function(){document.querySelectorAll('.filter').forEach(function(item){item.classList.remove('active')});button.classList.add('active');var filter=button.dataset.filter;document.querySelectorAll('tbody tr[data-state]').forEach(function(row){row.hidden=filter!=='all'&&row.dataset.state!==filter})})})</script></body></html>`, len(page.Accounts), page.Active, page.Unavailable+page.Disabled, page.Success, page.Failed, page.NoEvidence, len(page.Accounts), page.Active, page.Unavailable, page.Disabled, accounts.String(), modelRows.String(), balanceClass, balanceState))
}

func valueOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func selected(value, want string) string {
	if value == want {
		return " selected"
	}
	return ""
}
func checked(value bool) string {
	if value {
		return " checked"
	}
	return ""
}
func renderRoutePanel() string {
	cfg := currentRouteConfig()
	return fmt.Sprintf(`<section class="section route-panel"><div class="section-head"><div><h2>高风险功能控制面</h2><p class="section-note">更改后立即生效，无需管理员密钥。</p></div><span class="state good" id="route-state">effective generation %d</span></div><form id="route-form"><div class="route-grid"><label>路由模式<select id="route-mode"><option value="auto"%s>自动选择</option><option value="coding-plan"%s>正式套餐</option><option value="start-plan"%s>体验套餐</option></select><small>自动模式优先使用已生效的体验套餐；付费回退默认关闭。</small></label></div><label class="toggle"><input id="dynamic-routing" type="checkbox"%s><span><b>Dynamic routing active</b><small>关闭时仅观察，不改写端点。</small></span></label><label class="toggle"><input id="client-signing" type="checkbox"%s><span><b>Client signing</b></span></label><label class="toggle"><input id="unsigned-chat-replay" type="checkbox"%s><span><b>Unsigned chat replay</b></span></label><label class="toggle"><input id="retain-dual" type="checkbox"%s><span><b>Retain dual credentials</b></span></label><label class="toggle"><input id="allow-paid" type="checkbox"%s><span><b>Paid fallback</b></span></label><label class="toggle"><input id="off-peak" type="checkbox"%s><span><b>OffPeak</b></span></label><label class="toggle"><input id="start-plan-auto-claim" type="checkbox"%s><span><b>Start Plan 自动领取</b><small>轮询周末/体验套餐预览并自动领取；每次消费一个验证码池 token。</small></span></label><div class="route-actions"><button class="action primary" type="submit">保存配置</button><span id="route-result" role="status"></span></div><div class="danger" id="paid-warning" hidden>开启付费回退可能消耗 API Key 付费余额。</div></form></section>`, currentRouteConfigGeneration(), selected(cfg.Mode, "auto"), selected(cfg.Mode, "coding-plan"), selected(cfg.Mode, "start-plan"), checked(cfg.DynamicRoutingActive), checked(cfg.ClientSigningEnabled), checked(cfg.ClientSigningAllowChatReplay), checked(cfg.RetainDual), checked(cfg.AllowPaidFallback), checked(cfg.OffPeakEnabled), checked(cfg.StartPlanAutoClaim))
}

func zcodeModelIDs() []string {
	models := zcodeModels()
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func renderDiagnostics(page zcodeStatusPage) string {
	cfg := currentRouteConfig()
	mode := map[string]string{"auto": "自动选择", "coding-plan": "正式套餐", "start-plan": "体验套餐"}[cfg.Mode]
	if mode == "" {
		mode = "自动选择"
	}
	fallback, fallbackClass := "付费回退已关闭", "good"
	if cfg.AllowPaidFallback {
		fallback, fallbackClass = "允许使用付费余额", "warn"
	}

	credential := "尚无账号"
	if len(page.Accounts) > 0 {
		credential = page.Accounts[0].Credential
	}
	if len(page.Accounts) > 1 {
		credential = fmt.Sprintf("%d 个账号已注册", len(page.Accounts))
	}

	quota := "尚未刷新额度"
	quotaDetail := "点击页面顶部“刷新全部额度”获取最新套餐信息"
	for _, account := range page.Accounts {
		zcodeQuotas.RLock()
		snapshot, ok := zcodeQuotas.items[account.AuthIndex]
		zcodeQuotas.RUnlock()
		if !ok {
			continue
		}
		if snapshot.Error != "" {
			quota, quotaDetail = snapshot.Error, "请检查凭据、验证码参数或上游状态"
		} else if snapshot.Remaining != nil {
			if snapshot.Money {
				quota = fmt.Sprintf("可用余额 ¥%.2f", *snapshot.Remaining)
			} else {
				quota = fmt.Sprintf("剩余 %.0f / %.0f", *snapshot.Remaining, valueOrZero(snapshot.Total))
			}
			quotaDetail = firstString(snapshot.Plan, "Coding Plan") + " · 更新于 " + snapshot.Refreshed.Local().Format("15:04")
		} else if snapshot.Plan != "" {
			quota, quotaDetail = snapshot.Plan, "官方账户状态 · 更新于 "+snapshot.Refreshed.Local().Format("15:04")
		}
		break
	}

	state, stateClass, summary := "等待首次验证", "neutral", "尚无实际请求记录，暂时无法判断账号和模型连通性。"
	if page.Success > 0 {
		state, stateClass, summary = "运行正常", "good", "账号池已有成功请求记录，当前路由具备可用证据。"
	} else if page.Failed > 0 {
		state, stateClass, summary = "需要关注", "warn", "检测到失败请求，请结合凭据、额度和上游响应排查。"
	} else if page.Unavailable+page.Disabled > 0 {
		state, stateClass, summary = "账号不可用", "bad", "账号被 CPA 标记为不可用或已禁用。"
	}

	next := "刷新套餐额度，然后发送一次低成本请求验证连通性。"
	if len(page.Accounts) == 0 {
		next = "先在凭证管理或 OAuth 登录中添加 ZCode 账号。"
	} else if page.Unavailable+page.Disabled > 0 {
		next = "检查账号状态和凭据有效性，恢复后再测试。"
	} else if page.Failed > 0 {
		next = "查看失败响应；仅在明确余额不足时考虑切换路由。"
	} else if page.Success > 0 {
		next = "无需操作；定期刷新额度并关注付费回退设置。"
	}

	return fmt.Sprintf(`<section class="section diagnostics"><div class="diagnostic-head"><div><div class="eyebrow">运行诊断</div><h2>%s</h2><p>%s</p></div><span class="state %s">%s</span></div><div class="diagnostic-grid"><article><span>路由策略</span><b>%s</b><small class="%s-text">%s</small></article><article><span>凭据准备</span><b>%s</b><small>%d 个账号可路由</small></article><article><span>套餐额度</span><b>%s</b><small>%s</small></article><article><span>请求证据</span><b>成功 %d · 失败 %d</b><small>%d 个账号暂无请求证据</small></article></div><div class="next-step"><span>下一步</span><b>%s</b></div><details><summary>安全与切换说明</summary><div class="detail-body"><p>网络错误、上游 5xx、验证码异常和普通限流不会触发自动切换；流式响应开始后不会重放请求。</p><p>页面不会生成、显示或记录 JWT、API Key 与验证码参数。只有显式开启后，OAuth 才会保留双凭据。</p></div></details></section>`, html.EscapeString(state), html.EscapeString(summary), stateClass, html.EscapeString(state), html.EscapeString(mode), fallbackClass, html.EscapeString(fallback), html.EscapeString(credential), page.Active, html.EscapeString(quota), html.EscapeString(quotaDetail), page.Success, page.Failed, page.NoEvidence, html.EscapeString(next))
}

func renderRefreshNotice(page zcodeStatusPage) string {
	if page.QuotaTried == 0 {
		return ""
	}
	class, title := "good", "额度刷新完成"
	if page.QuotaFailed > 0 {
		class, title = "warn", "额度刷新完成，但部分账号失败"
	}
	if page.QuotaOK == 0 {
		class, title = "bad", "额度刷新失败"
	}
	return fmt.Sprintf(`<div class="refresh-notice %s"><b>%s</b><span>已处理 %d 个账号：成功 %d，失败 %d。具体结果显示在账号卡片和运行诊断中。</span></div>`, class, title, page.QuotaTried, page.QuotaOK, page.QuotaFailed)
}

func renderZCodeAccountPage(page zcodeStatusPage) []byte {
	var cards strings.Builder
	if len(page.Accounts) == 0 {
		cards.WriteString(`<div class="empty">还没有 ZCode 凭证。在 CPA 凭证管理中添加 API Key 或完成 ZCode OAuth 授权。</div>`)
	}
	for _, account := range page.Accounts {
		stateLabel, stateClass := "可路由", "good"
		if account.State == "disabled" {
			stateLabel, stateClass = "已禁用", "muted"
		} else if account.State == "unavailable" {
			stateLabel, stateClass = "不可用", "bad"
		}
		evidence, progress, progressClass := "无请求证据", 0, "neutral"
		if account.Success > 0 {
			evidence, progress, progressClass = "近期有成功请求", 100, "good"
		} else if account.Failed > 0 {
			evidence, progress, progressClass = "近期有失败请求", 35, "warn"
		}
		if account.State == "unavailable" {
			progress, progressClass = 15, "bad"
		} else if account.State == "disabled" {
			progress, progressClass = 0, "muted"
		}
		updated := "未记录"
		if !account.UpdatedAt.IsZero() {
			updated = account.UpdatedAt.Local().Format("2006-01-02 15:04")
		}
		quotaText := "尚未刷新额度"
		zcodeQuotas.RLock()
		quota, hasQuota := zcodeQuotas.items[account.AuthIndex]
		zcodeQuotas.RUnlock()
		if hasQuota {
			if quota.Error != "" {
				quotaText = quota.Error
			} else if quota.Remaining != nil {
				if quota.Money {
					quotaText = fmt.Sprintf("可用余额 ¥%.2f", *quota.Remaining)
				} else {
					quotaText = fmt.Sprintf("剩余 %.0f / 总额 %.0f", *quota.Remaining, valueOrZero(quota.Total))
				}
			} else if quota.Plan != "" {
				quotaText = quota.Plan
			} else {
				quotaText = "未返回可汇总额度"
			}
			if !quota.Refreshed.IsZero() {
				quotaText += " · " + quota.Refreshed.Local().Format("15:04")
			}
		}
		platform, costClass := "Z.ai 国际", "neutral"
		if strings.Contains(account.Credential, "BigModel") {
			platform, costClass = "BigModel 国内 · 按量付费", "warn"
		}
		disable := account.State != "disabled"
		action := "启用账号"
		if disable {
			action = "停用账号"
		}
		cards.WriteString(fmt.Sprintf(`<article class="account-card" data-state="%s"><div class="card-head"><div><h3>%s</h3><p>%s</p></div><span class="state %s">%s</span></div><div class="chips"><span class="chip">%s</span><span class="chip %s">%s</span><span class="chip">%s</span></div><div class="card-meta"><span>成功 <b>%d</b></span><span>失败 <b>%d</b></span><span>更新时间 <b>%s</b></span></div><div class="progress-label"><span>%s</span><span>%d%%</span></div><div class="progress"><i class="%s" style="width:%d%%"></i></div><p class="quota">额度 / 余额：%s</p><p class="card-detail">%s</p><div class="account-actions"><button type="button" class="action account-toggle" data-name="%s" data-auth-index="%s" data-disable="%t">%s</button><span class="account-result" role="status"></span></div></article>`, account.State, html.EscapeString(account.Name), html.EscapeString(account.Credential), stateClass, stateLabel, html.EscapeString(account.Credential), costClass, html.EscapeString(platform), evidence, account.Success, account.Failed, updated, evidence, progress, progressClass, progress, html.EscapeString(quotaText), html.EscapeString(account.Detail), html.EscapeString(account.FileName), html.EscapeString(account.AuthIndex), disable, action))
	}
	models := zcodeModelIDs()
	var modelRows strings.Builder
	poolEvidence, evidenceClass := "尚无账号池请求证据", "neutral"
	if page.Success > 0 {
		poolEvidence, evidenceClass = "账号池近期成功", "good"
	} else if page.Failed > 0 {
		poolEvidence, evidenceClass = "账号池近期失败", "warn"
	}
	for _, model := range models {
		if strings.HasPrefix(model, "zcode-offpeak-") {
			status, detail := "unsupported", "Off-Peak 默认关闭；仅在显式启用且账号同时具备 JWT + Coding Plan API Key 时支持。实验限制：无独立 cancel；settle 仅尝试一次且失败即整次失败。"
			if currentRouteConfig().OffPeakEnabled {
				status, detail = "experimental", "Off-Peak 已显式启用；独立有界执行，不会回退到普通或付费路径。上游 SSE 当前先有界缓冲，run 完成后再输出，并非实时流；settle 仅尝试一次。"
			}
			modelRows.WriteString(fmt.Sprintf(`<tr><td><code>%s</code></td><td><span class="state warn">%s</span></td><td><span class="state neutral">%s</span><small>%s</small></td></tr>`, model, status, poolEvidence, detail))
			continue
		}
		trialAvailable, trialKnown := activeTrialModelStatus(model, time.Now())
		routeStatus, routeClass, routeDetail := "已暴露", "neutral", "模型级独立检测尚未执行"
		if trialAvailable {
			routeStatus, routeClass, routeDetail = "体验套餐免费", "good", "近期套餐预览已明确声明该模型，且当前处于生效时间窗口"
		} else if trialKnown {
			routeStatus, routeClass, routeDetail = "体验套餐暂不可用", "warn", "近期套餐预览未发现当前生效且明确包含该模型的权益"
		}
		modelRows.WriteString(fmt.Sprintf(`<tr><td><code>%s</code></td><td><span class="state %s">%s</span></td><td><span class="state %s">%s</span><small>%s</small></td></tr>`, model, routeClass, routeStatus, evidenceClass, poolEvidence, routeDetail))
	}
	return []byte(fmt.Sprintf(`<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>ZCode 账号管理</title><style>
:root{color-scheme:dark;--bg:#11151a;--panel:#1b2026;--panel2:#22282f;--line:#303943;--text:#edf2f6;--muted:#9ba7b2;--green:#36c88a;--amber:#e4ae3a;--red:#ef726b;--blue:#69a9ff}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}main{max-width:1180px;margin:0 auto;padding:28px 22px 52px}.top{display:flex;justify-content:space-between;align-items:flex-start;gap:18px;border-bottom:1px solid var(--line);padding-bottom:20px}.eyebrow{color:var(--blue);font-size:12px;font-weight:700;letter-spacing:.08em;text-transform:uppercase}.top h1{font-size:25px;margin:6px 0}.top p,.section-note{color:var(--muted);line-height:1.55;margin:0}.actions{display:flex;gap:8px;flex-wrap:wrap}.action{background:var(--panel2);border:1px solid var(--line);border-radius:7px;color:var(--text);padding:9px 12px;text-decoration:none;white-space:nowrap}.action.primary{background:#347ee6;border-color:#4c92f4}.metrics{display:grid;grid-template-columns:repeat(6,1fr);gap:1px;background:var(--line);border:1px solid var(--line);margin:22px 0}.metric{background:var(--panel);padding:15px}.metric b{display:block;font-size:23px}.metric span{display:block;color:var(--muted);font-size:12px;margin-top:5px}.section{margin-top:28px}.section-head{display:flex;justify-content:space-between;align-items:end;gap:14px;margin-bottom:12px}.section h2{font-size:17px;margin:0}.filters{display:flex;gap:8px;flex-wrap:wrap;margin:0 0 14px}.filter{background:var(--panel2);border:1px solid var(--line);border-radius:999px;color:var(--muted);padding:8px 12px;cursor:pointer}.filter.active{background:#347ee6;border-color:#4c92f4;color:white}.filter span{margin-left:4px;font-weight:700}.account-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(330px,1fr));gap:14px}.account-card{background:var(--panel);border:1px solid var(--line);border-radius:9px;padding:17px;min-width:0}.card-head{display:flex;justify-content:space-between;gap:12px;align-items:flex-start}.card-head h3{font-size:17px;margin:0 0 5px;overflow-wrap:anywhere}.card-head p,.card-detail{color:var(--muted);font-size:12px;margin:0;overflow-wrap:anywhere}.state{display:inline-block;border-radius:999px;padding:4px 9px;font-size:12px;font-weight:700;white-space:nowrap}.good{background:#173b30;color:#75e1b0}.bad{background:#442424;color:#ffaaa5}.muted{background:#30363d;color:#bec7cf}.neutral{background:#20384d;color:#9bd2ff}.warn{background:#493916;color:#ffd57a}.chips{display:flex;gap:7px;flex-wrap:wrap;margin:16px 0}.chip{background:#2a3037;border:1px solid #3a444e;border-radius:5px;color:#cbd4db;font-size:12px;padding:5px 8px}.card-meta{display:grid;grid-template-columns:repeat(3,1fr);gap:8px;color:var(--muted);font-size:12px;margin-bottom:15px}.card-meta b{display:block;color:var(--text);font-size:15px;margin-top:4px;overflow-wrap:anywhere}.progress-label{display:flex;justify-content:space-between;color:var(--muted);font-size:12px;margin-bottom:6px}.progress{height:8px;background:#303840;border-radius:99px;overflow:hidden}.progress i{display:block;height:100%%;border-radius:99px}.progress i.good{background:var(--green)}.progress i.warn{background:var(--amber)}.progress i.bad{background:var(--red)}.progress i.neutral,.progress i.muted{background:#65717d}.empty{background:var(--panel);border:1px dashed var(--line);border-radius:9px;color:var(--muted);padding:30px;text-align:center}.model-table{border:1px solid var(--line);background:var(--panel);overflow:auto}.model-table table{border-collapse:collapse;width:100%%;min-width:620px}.model-table th,.model-table td{text-align:left;padding:12px 14px;border-bottom:1px solid var(--line)}.model-table th{color:var(--muted);font-size:12px}.model-table tr:last-child td{border-bottom:0}.notice{border-left:3px solid var(--amber);background:#201d16;padding:14px 16px;color:var(--muted);line-height:1.65}.notice p{margin:5px 0}.hidden{display:none!important}@media(max-width:720px){main{padding:20px 14px 40px}.top{display:block}.actions{margin-top:16px}.metrics{grid-template-columns:repeat(2,1fr)}.account-grid{grid-template-columns:1fr}.card-meta{grid-template-columns:repeat(2,1fr)}.section-head{display:block}.section-note{margin-top:8px}}.route-panel{background:var(--panel);border:1px solid var(--line);border-radius:9px;padding:18px}.route-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}label{display:block;color:var(--muted);font-size:12px}select{display:block;width:100%%;margin-top:7px;padding:10px;border:1px solid var(--line);border-radius:7px;background:var(--panel2);color:var(--text)}label small{display:block;margin-top:6px;color:var(--muted);line-height:1.45}.toggle{display:flex;align-items:flex-start;gap:10px;margin-top:14px;padding:12px;border:1px solid var(--line);border-radius:7px;background:var(--panel2)}.toggle input{margin-top:3px}.toggle b{display:block;color:var(--text);font-size:14px}.route-actions{display:flex;align-items:center;gap:12px;margin-top:15px;flex-wrap:wrap}.route-actions input{min-width:260px;flex:1;padding:10px;border:1px solid var(--line);border-radius:7px;background:var(--panel2);color:var(--text)}.route-actions button,.account-actions button{cursor:pointer}.account-actions{display:flex;align-items:center;gap:10px;margin-top:14px}.account-result{font-size:12px}.danger{margin-top:12px;padding:10px 12px;border-left:3px solid var(--red);background:#2b1a1a;color:#ffaaa5}.refresh-notice{display:flex;align-items:center;gap:12px;margin-top:14px;padding:12px 14px;border:1px solid var(--line);border-radius:8px;background:var(--panel)}.refresh-notice b{white-space:nowrap}.refresh-notice span{color:var(--muted);font-size:12px}.refresh-notice.good{border-left:3px solid var(--green)}.refresh-notice.warn{border-left:3px solid var(--amber)}.refresh-notice.bad{border-left:3px solid var(--red)}.diagnostics{background:var(--panel);border:1px solid var(--line);border-radius:9px;padding:20px}.diagnostic-head{display:flex;justify-content:space-between;align-items:flex-start;gap:16px}.diagnostic-head h2{font-size:20px;margin:5px 0 6px}.diagnostic-head p{color:var(--muted);margin:0;line-height:1.55}.diagnostic-grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:1px;margin-top:18px;background:var(--line);border:1px solid var(--line);border-radius:8px;overflow:hidden}.diagnostic-grid article{background:var(--panel2);padding:14px;min-width:0}.diagnostic-grid span,.next-step span{display:block;color:var(--muted);font-size:12px}.diagnostic-grid b{display:block;margin:6px 0;font-size:15px;overflow-wrap:anywhere}.diagnostic-grid small{display:block;color:var(--muted);line-height:1.45}.good-text{color:var(--green)!important}.warn-text{color:var(--amber)!important}.next-step{display:grid;grid-template-columns:80px 1fr;align-items:center;gap:12px;margin-top:14px;padding:13px 14px;border:1px solid var(--line);border-radius:8px}.next-step b{font-size:14px}details{margin-top:10px;color:var(--muted)}summary{cursor:pointer;padding:8px 2px;font-size:12px}.detail-body{padding:0 12px 4px;line-height:1.55;font-size:12px}.detail-body p{margin:5px 0}@media(max-width:900px){.diagnostic-grid{grid-template-columns:repeat(2,1fr)}}@media(max-width:560px){.diagnostic-grid{grid-template-columns:1fr}.diagnostic-head{display:block}.diagnostic-head .state{margin-top:12px}.next-step{grid-template-columns:1fr}}.success-text{color:var(--green)}.error-text{color:var(--red)}@media(prefers-color-scheme:light){:root{color-scheme:light;--bg:#f5f7fa;--panel:#fff;--panel2:#f3f6f9;--line:#dce3ea;--text:#1f2933;--muted:#657382;--green:#16835d;--amber:#9a6810;--red:#c43d37;--blue:#256dcc}body{background:var(--bg)}.action,.filter,.chip,.toggle,select{background:var(--panel2);border-color:var(--line);color:var(--text)}.metric,.account-card,.model-table,.route-panel{background:var(--panel)}.good{background:#dff5eb;color:#116a4b}.bad{background:#fde8e7;color:#a92f2b}.muted{background:#e9eef2;color:#52616e}.neutral{background:#e3f0fc;color:#21649f}.warn{background:#fff1cf;color:#80580e}.notice{background:#fff9e9}.danger{background:#fff0ef;color:#a92f2b}.progress{background:#e6ebef}.model-table th,.model-table td{border-color:var(--line)}}@media(max-width:720px){.route-grid{grid-template-columns:1fr}}</style></head><body><main><header class="top"><div><div class="eyebrow">Provider health</div><h1>ZCode 账号管理</h1><p>账号池、路由状态与近期请求证据。凭证内容不会显示在此页面。</p><p class="section-note">国内 BigModel：在 CPA 凭证管理中导入 <code>bigmodel-coding:你的API_KEY</code>；国际 Z.ai：继续使用 OAuth。两平台凭据严格隔离。</p></div><div class="actions"><a class="action primary" href="?login=zai">登录 Z.AI 国际账户</a><a class="action primary" href="?login=bigmodel">登录 BigModel 国内账户</a><a class="action" href="?refresh=1">刷新状态</a><a class="action" href="?refresh=claim">刷新体验套餐</a><a class="action" href="?refresh=quota">刷新全部额度</a></div></header>%s%s<section class="metrics"><div class="metric"><b>%d</b><span>已注册账号</span></div><div class="metric"><b>%d</b><span>可路由</span></div><div class="metric"><b>%d</b><span>不可用 / 已禁用</span></div><div class="metric"><b>%d</b><span>近期成功</span></div><div class="metric"><b>%d</b><span>近期失败</span></div><div class="metric"><b>%d</b><span>无请求证据</span></div></section><section class="section"><div class="section-head"><h2>账号池</h2><p class="section-note">状态和计数由 CPA 宿主提供；更新时间不等同于最近调用时间。</p></div><div class="filters"><button class="filter active" data-filter="all">全部 <span>%d</span></button><button class="filter" data-filter="active">可路由 <span>%d</span></button><button class="filter" data-filter="unavailable">不可用 <span>%d</span></button><button class="filter" data-filter="disabled">已禁用 <span>%d</span></button></div><div class="account-grid">%s</div></section><section class="section"><div class="section-head"><h2>模型矩阵</h2><p class="section-note">账号池证据不等同于模型级独立检测。</p></div><div class="model-table"><table><thead><tr><th>模型</th><th>CPA 路由</th><th>验证状态</th></tr></thead><tbody>%s</tbody></table></div></section>%s</main><script>document.querySelectorAll('.filter').forEach(function(button){button.addEventListener('click',function(){document.querySelectorAll('.filter').forEach(function(item){item.classList.remove('active')});button.classList.add('active');var filter=button.dataset.filter;document.querySelectorAll('.account-card').forEach(function(card){card.classList.toggle('hidden',filter!=='all'&&card.dataset.state!==filter)})})});const paid=document.getElementById('allow-paid'),warning=document.getElementById('paid-warning');function syncWarning(){warning.hidden=!paid.checked}paid.addEventListener('change',syncWarning);syncWarning();document.querySelectorAll('.account-toggle').forEach(function(button){button.addEventListener('click',async function(){const disabling=button.dataset.disable==='true',result=button.nextElementSibling,active=document.querySelectorAll('.account-card[data-state="active"]').length,key=(window.prompt('请输入 Manager 管理员密钥（仅用于本次账号切换，不会保存）')||'').trim();if(disabling&&active<=1){result.className='account-result error-text';result.textContent='至少保留一个可路由账号';return}if(!key){result.className='account-result error-text';result.textContent='已取消切换';return}button.disabled=true;result.className='account-result';result.textContent='正在切换…';try{const response=await fetch('/v0/management/auth-files/status',{method:'PATCH',headers:{'Content-Type':'application/json','Authorization':'Bearer '+key},credentials:'same-origin',cache:'no-store',body:JSON.stringify({name:button.dataset.name,auth_index:button.dataset.authIndex,disabled:disabling})});if(!response.ok)throw new Error('HTTP '+response.status);result.className='account-result success-text';result.textContent=disabling?'账号已停用':'账号已启用';setTimeout(function(){location.reload()},500)}catch(error){button.disabled=false;result.className='account-result error-text';result.textContent=error.message.includes('401')?'管理员密钥无效':'切换失败：'+error.message}})});document.getElementById('route-form').addEventListener('submit',async function(event){event.preventDefault();const result=document.getElementById('route-result');const form=new URLSearchParams({action:'save_config',route_mode:document.getElementById('route-mode').value,dynamic_routing_active:document.getElementById('dynamic-routing').checked,client_signing_enabled:document.getElementById('client-signing').checked,client_signing_allow_unsigned_chat_replay:document.getElementById('unsigned-chat-replay').checked,retain_dual_credentials:document.getElementById('retain-dual').checked,allow_paid_fallback:paid.checked,off_peak_enabled:document.getElementById('off-peak').checked,start_plan_auto_claim:document.getElementById('start-plan-auto-claim').checked});result.className='';result.textContent='正在保存…';try{const response=await fetch(location.pathname,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},credentials:'same-origin',cache:'no-store',body:form.toString()});const body=await response.json();if(!response.ok||!body.ok)throw new Error(body.message||'HTTP '+response.status);result.className='success-text';result.textContent=body.message||'配置已保存并生效';var state=document.getElementById('route-state');if(state&&body.generation)state.textContent='effective generation '+body.generation}catch(error){result.className='error-text';result.textContent='保存失败：'+error.message}})</script></body></html>`, renderRoutePanel(), renderRefreshNotice(page), len(page.Accounts), page.Active, page.Unavailable+page.Disabled, page.Success, page.Failed, page.NoEvidence, len(page.Accounts), page.Active, page.Unavailable, page.Disabled, cards.String(), modelRows.String(), renderDiagnostics(page)))
}

func claimRefreshRequested(req pluginapi.ManagementRequest) bool {
	return req.Query.Get("refresh") == "claim"
}

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func claimManagementResponse(status int, message string) pluginapi.ManagementResponse {
	body, _ := json.Marshal(map[string]any{"ok": status >= 200 && status < 300, "message": message})
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: body}
}

func handleManualClaim(values url.Values, hostCallbackID string) pluginapi.ManagementResponse {
	authIndex := strings.TrimSpace(values.Get("auth_index"))
	planID := strings.TrimSpace(values.Get("plan_id"))
	verifyParam := strings.TrimSpace(values.Get("verify_param"))
	region := strings.TrimSpace(values.Get("region"))
	finish := func(status int, message string) pluginapi.ManagementResponse {
		return claimManagementResponse(status, message)
	}
	if len(values["action"]) != 1 || len(values["auth_index"]) != 1 || len(values["plan_id"]) != 1 || len(values["confirmation_token"]) != 1 || len(values["confirm"]) != 1 || len(values["verify_param"]) != 1 || len(values["region"]) > 1 || values.Get("action") != "claim" || authIndex == "" || planID == "" || strings.TrimSpace(values.Get("confirmation_token")) == "" || values.Get("confirm") != "true" {
		return finish(http.StatusBadRequest, "领取请求必须来自独立确认页并显式确认")
	}
	if verifyParam == "" || len(verifyParam) > 4096 || len(region) > 64 {
		return finish(http.StatusBadRequest, "必须提交正常浏览器产生的官方一次性 verifyParam，可选填写 region")
	}
	key := normalizeClaimKey(authIndex, planID)
	if !consumeClaimConfirmation(values.Get("confirmation_token"), key, time.Now()) {
		return finish(http.StatusConflict, "确认令牌无效、已过期或已使用，请重新确认")
	}
	if !tryAcquireClaim(key) {
		return finish(http.StatusConflict, "相同账号和套餐已有领取请求正在执行")
	}
	defer releaseClaim(key)
	if !claimPlanInRecentPreview(authIndex, planID) {
		return finish(http.StatusConflict, "套餐预览已过期或 plan_id 不在最近成功预览中，请先刷新体验套餐")
	}
	storage, err := loadClaimStorage(authIndex)
	if err != nil {
		return finish(http.StatusBadRequest, "无法读取账号凭证")
	}
	if err := validateClaimStorage(storage); err != nil {
		return finish(http.StatusBadRequest, "手动领取需要同一账号中的有效 JWT 和固定 UUID v4 DeviceMID")
	}
	storage.CaptchaVerifyParam = verifyParam
	storage.CaptchaVerifyRegion = region
	request, err := buildClaimRequest(storage, planID)
	if err != nil {
		return finish(http.StatusBadRequest, "领取请求材料无效")
	}
	ctx, cancel := context.WithTimeout(context.Background(), claimRequestTimeout)
	defer cancel()
	response, err := claimHTTPDo(ctx, nil, hostCallbackID, request)
	if err != nil {
		return finish(http.StatusBadGateway, "套餐领取网络失败")
	}
	if len(response.Body) > claimMaxResponseBody {
		return finish(http.StatusBadGateway, "套餐领取响应过大")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return finish(http.StatusBadGateway, fmt.Sprintf("套餐领取接口返回 HTTP %d", response.StatusCode))
	}
	result, err := parseClaimResult(response.Body)
	if err != nil {
		return finish(http.StatusBadGateway, "套餐领取响应无法解析")
	}
	if result.Category != "success" {
		if result.Category == "unknown" {
			return finish(http.StatusConflict, fmt.Sprintf("套餐领取失败（业务码 %d）", result.Code))
		}
		return finish(http.StatusConflict, result.Message)
	}
	confirmationRequest := pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     zcodeBillingCurrentURL,
		Headers: claimHeaders(storage),
	}
	confirmationResponse, confirmationErr := claimHTTPDo(ctx, nil, hostCallbackID, confirmationRequest)
	if confirmationErr != nil || confirmationResponse.StatusCode < 200 || confirmationResponse.StatusCode >= 300 || len(confirmationResponse.Body) > claimMaxResponseBody || !claimBillingCurrentConfirmsPlan(confirmationResponse.Body, planID) {
		return finish(http.StatusConflict, "领取接口成功但到账未确认")
	}
	claimPreviews.Lock()
	delete(claimPreviews.items, authIndex)
	claimPreviews.Unlock()
	return finish(http.StatusOK, "已到账")
}

func claimBillingCurrentConfirmsPlan(body []byte, planID string) bool {
	var envelope struct {
		Data struct {
			Plans []struct {
				PlanID       string             `json:"plan_id"`
				Status       string             `json:"status"`
				Entitlements []claimEntitlement `json:"entitlements"`
			} `json:"plans"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	for _, plan := range envelope.Data.Plans {
		if strings.TrimSpace(plan.PlanID) == strings.TrimSpace(planID) && strings.EqualFold(strings.TrimSpace(plan.Status), "active") {
			return true
		}
		for _, entitlement := range plan.Entitlements {
			label := strings.ToLower(strings.Join([]string{entitlement.EntitlementID, entitlement.ShowName, entitlement.Meter, entitlement.UnitType}, " "))
			if strings.EqualFold(strings.TrimSpace(plan.Status), "active") && entitlement.GrantUnits == 300 && strings.Contains(label, "m") {
				return true
			}
		}
	}
	return false
}

func formatClaimTime(unix int64) string {
	if unix <= 0 {
		return "未说明"
	}
	return time.Unix(unix, 0).Local().Format("2006-01-02 15:04")
}

func renderClaimManagement(page zcodeStatusPage) []byte {
	var rows strings.Builder
	for _, item := range sortedClaimSnapshots(page) {
		if item.Preview.Error != "" {
			rows.WriteString(fmt.Sprintf(`<tr><td><strong>%s</strong></td><td colspan="2">%s</td><td><span class="state bad">预览失败</span></td><td>请刷新后重试</td></tr>`, html.EscapeString(item.Account.Name), html.EscapeString(item.Preview.Error)))
			continue
		}
		claimable := item.Preview.Preview.Eligible && item.Preview.Preview.Claimable
		for _, plan := range item.Preview.Preview.Plans {
			var benefits []string
			for _, entitlement := range plan.Entitlements {
				name := firstString(entitlement.ShowName, entitlement.EntitlementID, "未命名权益")
				if entitlement.GrantUnits != 0 {
					name += fmt.Sprintf(" · %.0f %s", entitlement.GrantUnits, entitlement.UnitType)
				}
				benefits = append(benefits, name)
			}
			if len(benefits) == 0 {
				benefits = append(benefits, firstString(plan.Description, "未说明"))
			}
			state := `<span class="state warn">不可领取</span>`
			action := `请刷新后重试`
			if claimable {
				state = `<span class="state good">可领取</span>`
				action = fmt.Sprintf(`<a class="action primary" href="?confirm_claim=1&amp;auth_index=%s&amp;plan_id=%s">官方验证并领取…</a>`, url.QueryEscape(item.Account.AuthIndex), url.QueryEscape(plan.PlanID))
			}
			rows.WriteString(fmt.Sprintf(`<tr><td><strong>%s</strong><small>%s</small></td><td>%s</td><td>%s<br><small>有效期：%s 至 %s</small></td><td>%s</td><td>%s</td></tr>`, html.EscapeString(firstString(plan.Name, plan.PlanID)), html.EscapeString(item.Account.Name), html.EscapeString(strings.Join(benefits, "；")), html.EscapeString(firstString(plan.Description, "体验套餐")), html.EscapeString(formatClaimTime(plan.StartsAt)), html.EscapeString(formatClaimTime(plan.EndsAt)), state, action))
		}
	}
	if rows.Len() == 0 {
		rows.WriteString(`<tr><td colspan="5" class="empty">尚无最近成功的体验套餐预览。刷新体验套餐”；该操作只读，不会自动领取></tr>`)
	}
	return []byte(fmt.Sprintf(`<section class="section claim-panel"><div class="section-head"><div><h2>体验套餐</h2><p class="section-note">展示最近一次只读预览及明确状态。会在你显式确认后执行一次，不会自动领取></div></div><div class="model-table"><table><thead><tr><th>套餐名称 / 账号</th><th>权益</th><th>有效期</th><th>状态</th><th>操作</th></tr></thead><tbody>%s</tbody></table></div></section>`, rows.String()))
}

func renderClaimConfirmation(authIndex, planID, token string) []byte {
	return []byte(fmt.Sprintf(`<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>官方验证并领取</title></head><body><main><h1>官方验证并领取</h1><p>请先在正常浏览器完成官方验证，再粘贴本次官方交互产生的一次性 verifyParam。region 可选。材料仅供本次表单单次使用，不进入验证码池、不持久化，也不写入审计正文。</p><p>账号标识：<code>%s</code><br>套餐：<code>%s</code></p><form id="claim-form" method="post"><input type="hidden" name="action" value="claim"><input type="hidden" name="auth_index" value="%s"><input type="hidden" name="plan_id" value="%s"><input type="hidden" name="confirmation_token" value="%s"><p><label>官方一次性 verifyParam<br><input type="text" name="verify_param" required maxlength="4096" autocomplete="off"></label></p><p><label>region（可选）<br><input type="text" name="region" maxlength="64" autocomplete="off"></label></p><label><input type="checkbox" name="confirm" value="true" required> 我确认材料来自本次正常浏览器官方交互，并立即执行一次领取</label><p><button id="claim-submit" type="submit">官方验证并领取</button> <a href="?">取消</a></p><p id="claim-result" role="status"></p></form></main><script>document.getElementById("claim-form").addEventListener("submit",async function(event){event.preventDefault();var form=event.currentTarget,button=document.getElementById("claim-submit"),result=document.getElementById("claim-result");button.disabled=true;result.textContent="正在提交一次性官方验证材料并检查到账状态…";try{var response=await fetch(location.pathname,{method:"POST",headers:{"Content-Type":"application/x-www-form-urlencoded"},body:new URLSearchParams(new FormData(form)),credentials:"same-origin",cache:"no-store"}),body=await response.json();result.textContent=body.message||("领取失败（HTTP "+response.status+")");if(response.ok&&body.ok){button.textContent="已到账";form.querySelectorAll("input").forEach(function(input){input.disabled=true})}else{button.disabled=false}}catch(error){button.disabled=false;result.textContent="领取请求失败，到账状态未确认"}});</script></body></html>`, html.EscapeString(authIndex), html.EscapeString(planID), html.EscapeString(authIndex), html.EscapeString(planID), html.EscapeString(token)))
}

func renderClaimAudit() []byte {
	events := recentClaimAudit()
	var rows strings.Builder
	start := len(events) - 20
	if start < 0 {
		start = 0
	}
	for i := len(events) - 1; i >= start; i-- {
		event := events[i]
		rows.WriteString(fmt.Sprintf(`<tr><td>%s</td><td><code>%s</code></td><td><code>%s</code></td><td>%s</td></tr>`, html.EscapeString(event.Time.Local().Format("2006-01-02 15:04:05")), html.EscapeString(event.AuthHash), html.EscapeString(event.PlanID), html.EscapeString(event.Result)))
	}
	if rows.Len() == 0 {
		rows.WriteString(`<tr><td colspan="4" class="empty">暂无手动领取审计事件</td></tr>`)
	}
	return []byte(fmt.Sprintf(`<section class="section"><div class="section-head"><div><h2>最近领取审计</h2><p class="section-note">仅显示时间、账号哈希、套餐标识与结果分类；不记录凭证、验证码或请求正文。</p></div></div><div class="model-table"><table><thead><tr><th>时间</th><th>账号哈希</th><th>套餐</th><th>结果</th></tr></thead><tbody>%s</tbody></table></div></section>`, rows.String()))
}

func claimConfirmationRequested(req pluginapi.ManagementRequest) bool {
	return req.Query.Get("confirm_claim") == "1"
}

func quotaRefreshRequested(req pluginapi.ManagementRequest) bool {
	return req.Query.Get("refresh") == "quota"
}

func handleManagementLogin(provider, hostCallbackID string) ([]byte, error) {
	request, err := json.Marshal(rpcAuthLoginStartRequest{
		AuthLoginStartRequest: pluginapi.AuthLoginStartRequest{Provider: provider},
		HostCallbackID:        hostCallbackID,
	})
	if err != nil {
		return nil, err
	}
	started, err := handleAuthLoginStart(request)
	if err != nil {
		return managementResponse(http.StatusBadGateway, "text/plain; charset=utf-8", []byte("无法启动 OAuth 登录："+err.Error()))
	}
	var wrapped envelope
	if err := json.Unmarshal(started, &wrapped); err != nil {
		return nil, err
	}
	if !wrapped.OK {
		message := "无法启动 OAuth 登录"
		if wrapped.Error != nil && strings.TrimSpace(wrapped.Error.Message) != "" {
			message += "：" + wrapped.Error.Message
		}
		return managementResponse(http.StatusBadGateway, "text/plain; charset=utf-8", []byte(message))
	}
	var login pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(wrapped.Result, &login); err != nil {
		return nil, err
	}
	if strings.TrimSpace(login.URL) == "" {
		return managementResponse(http.StatusBadGateway, "text/plain; charset=utf-8", []byte("OAuth 登录地址为空"))
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusFound,
		Headers: http.Header{
			"Cache-Control": []string{"no-store"},
			"Location":      []string{login.URL},
		},
	})
}

func handleManagement(request []byte) ([]byte, error) {
	var rpcReq rpcManagementRequest
	if err := json.Unmarshal(request, &rpcReq); err != nil {
		return nil, err
	}
	req := rpcReq.ManagementRequest
	if req.Path == oauthCallbackPath {
		return handleOAuthCallbackResource(req)
	}
	if req.Method == http.MethodPost {
		if len(req.Body) > claimMaxRequestBody {
			return managementResponse(http.StatusRequestEntityTooLarge, "text/plain; charset=utf-8", []byte("request body too large"))
		}
		values, err := url.ParseQuery(string(req.Body))
		if err != nil {
			return managementResponse(http.StatusBadRequest, "text/plain; charset=utf-8", []byte("invalid form body"))
		}
		var response pluginapi.ManagementResponse
		if values.Get("action") == "prepare_config" {
			response = handlePrepareConfig(values)
		} else if values.Get("action") == "save_config" {
			response = handleSaveConfig(values)
		} else if values.Get("action") == "add_captcha" {
			response = handleAddCaptcha(values)
		} else if values.Get("action") == "clear_captcha" {
			response = handleClearCaptcha(values)
		} else {
			response = handleManualClaim(values, rpcReq.HostCallbackID)
		}
		return okEnvelope(response)
	}
	if req.Method != http.MethodGet {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Body:       []byte("method not allowed"),
		})
	}
	if provider := strings.ToLower(strings.TrimSpace(req.Query.Get("login"))); provider != "" {
		if provider != "zai" && provider != "bigmodel" {
			return managementResponse(http.StatusBadRequest, "text/plain; charset=utf-8", []byte("不支持的 OAuth 平台"))
		}
		return handleManagementLogin(provider, rpcReq.HostCallbackID)
	}
	if req.Query.Get("config_control") == "effective" {
		return okEnvelope(effectiveConfigManagementResponse())
	}
	if req.Query.Get("config_control") == "confirm" {
		return okEnvelope(createConfigConfirmation(req))
	}
	if claimConfirmationRequested(req) {
		key := normalizeClaimKey(req.Query.Get("auth_index"), req.Query.Get("plan_id"))
		if key.AuthIndex == "" || key.PlanID == "" || !claimPlanInRecentPreview(key.AuthIndex, key.PlanID) {
			appendClaimAudit(key.AuthIndex, key.PlanID, "confirmation_rejected", time.Now())
			return managementResponse(http.StatusConflict, "text/plain; charset=utf-8", []byte("套餐预览已过期或确认目标无效"))
		}
		token, err := createClaimConfirmation(key, time.Now())
		if err != nil {
			appendClaimAudit(key.AuthIndex, key.PlanID, "confirmation_unavailable", time.Now())
			return managementResponse(http.StatusServiceUnavailable, "text/plain; charset=utf-8", []byte("暂时无法创建确认，请稍后重试"))
		}
		return managementResponse(http.StatusOK, "text/html; charset=utf-8", renderClaimConfirmation(key.AuthIndex, key.PlanID, token))
	}
	status, err := loadZCodeStatus()
	if err != nil {
		return managementResponse(http.StatusServiceUnavailable, "text/plain; charset=utf-8", []byte("无法读取 ZCode 账号状态："+err.Error()))
	}
	if quotaRefreshRequested(req) {
		status.QuotaTried, status.QuotaOK, status.QuotaFailed = refreshZCodeQuotas(status)
	}
	if claimRefreshRequested(req) {
		refreshClaimPreviews(status, rpcReq.HostCallbackID)
	}
	page := renderZCodeAccountPage(status)
	claimPanel := renderClaimManagement(status)
	auditPanel := renderClaimAudit()
	autoPanel := renderClaimAutoPanel(time.Now())
	page = []byte(strings.Replace(string(page), "</main>", string(claimPanel)+string(autoPanel)+string(renderClaimAutoScript())+string(auditPanel)+"</main>", 1))
	return managementResponse(http.StatusOK, "text/html; charset=utf-8", page)
}

func managementResponse(status int, contentType string, body []byte) ([]byte, error) {
	headers := http.Header{"Cache-Control": []string{"no-store"}}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body})
}

func callbackCORSHeaders(origin string) http.Header {
	headers := http.Header{
		"Access-Control-Allow-Origin":  []string{origin},
		"Access-Control-Allow-Methods": []string{http.MethodPost},
		"Access-Control-Allow-Headers": []string{"Content-Type"},
		"Vary":                         []string{"Origin"},
		"Cache-Control":                []string{"no-store"},
	}
	return headers
}

func callbackJSON(status int, origin string, ok bool, message string) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"ok": ok, "message": message})
	headers := http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}
	if origin == oauthBridgeOrigin {
		headers = callbackCORSHeaders(origin)
		headers.Set("Content-Type", "application/json; charset=utf-8")
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body})
}

func callbackGETPage(success bool) []byte {
	title := "ZCode OAuth 回调"
	message := "请粘贴 ZCode 返回的完整 callback URL。页面不会读取你的 Cookie、登录令牌或网页凭据。"
	form := `<form id="callback-form"><label for="callback">完整 callback URL</label><textarea id="callback" name="callback" rows="5" required maxlength="4096" placeholder="zcode://zai-auth/callback?code=...&amp;state=..."></textarea><button type="submit">提交授权码</button><p id="result"></p></form><script>document.getElementById("callback-form").addEventListener("submit",async function(event){event.preventDefault();const result=document.getElementById("result");try{const raw=document.getElementById("callback").value.trim();const parsed=new URL(raw);const state=parsed.searchParams.get("state")||"";const code=parsed.searchParams.get("code")||"";const response=await fetch(location.pathname,{method:"POST",headers:{"Content-Type":"application/x-www-form-urlencoded","X-ZCode-Callback":raw},body:new URLSearchParams({callback:raw}),credentials:"omit",cache:"no-store",referrerPolicy:"no-referrer"});const body=await response.json();result.textContent=body.message||"提交完成";if(response.ok&&body.ok)document.getElementById("callback").value=""}catch(error){result.textContent="callback URL 无效或提交失败"}});</script>`
	if success {
		title = "授权已接收"
		message = "CPA 已接收一次性授权码，请返回管理面板等待凭证保存。"
		form = ""
	}
	return []byte(fmt.Sprintf(`<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>body{font-family:-apple-system,sans-serif;background:#0f1115;color:#e6e8eb;margin:0;min-height:100vh;display:grid;place-items:center}.card{width:min(560px,calc(100%% - 48px));padding:28px;border:1px solid #2a3242;border-radius:14px;background:#1a1f2a}h1{font-size:24px}p,label{color:#aeb7c4;line-height:1.6}textarea{box-sizing:border-box;width:100%%;margin:8px 0 14px;padding:12px;border-radius:8px;border:1px solid #384257;background:#111722;color:#e6e8eb}button{padding:10px 16px;border:0;border-radius:8px;background:#2684ff;color:white}</style></head><body><main class="card"><h1>%s</h1><p>%s</p>%s</main></body></html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(message), form))
}

func trustedCallbackOrigin(origin string) bool {
	return origin == "" || origin == oauthBridgeOrigin || origin == oauthFirstPartyOrigin
}

func parseManualCallback(raw string) (string, string, error) {
	if len(raw) == 0 || len(raw) > oauthCallbackMaxField {
		return "", "", fmt.Errorf("callback URL 长度无效")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "zcode") || !strings.EqualFold(u.Host, "zai-auth") || u.Path != "/callback" || u.User != nil || u.Fragment != "" {
		return "", "", fmt.Errorf("callback URL 必须为 zcode://zai-auth/callback")
	}
	query := u.Query()
	if len(query["state"]) != 1 || len(query["code"]) != 1 || len(query) != 2 {
		return "", "", fmt.Errorf("callback URL 参数无效")
	}
	state := strings.TrimSpace(query.Get("state"))
	code := strings.TrimSpace(query.Get("code"))
	if state == "" || code == "" {
		return "", "", fmt.Errorf("callback URL 缺少 state 或 code")
	}
	return state, code, nil
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func callbackTunnelBody(req pluginapi.ManagementRequest) []byte {
	if len(req.Body) > 0 {
		return req.Body
	}
	if callback := req.Headers.Get("X-Zcode-Callback"); callback != "" {
		return []byte(url.Values{"callback": []string{callback}}.Encode())
	}
	state := req.Headers.Get("X-Zcode-State")
	code := req.Headers.Get("X-Zcode-Code")
	if state != "" || code != "" {
		return []byte(url.Values{"state": []string{state}, "code": []string{code}}.Encode())
	}
	return nil
}

func handleOAuthCallbackResource(req pluginapi.ManagementRequest) ([]byte, error) {
	tunneled := req.Method == http.MethodGet && req.Headers.Get("X-Zcode-Bridge-Method") == http.MethodPost
	method := req.Method
	if tunneled {
		method = http.MethodPost
	}
	origin := strings.TrimSpace(req.Headers.Get("Origin"))
	switch method {
	case http.MethodOptions:
		if origin != oauthBridgeOrigin {
			return managementResponse(http.StatusForbidden, "text/plain; charset=utf-8", []byte("forbidden origin"))
		}
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusNoContent, Headers: callbackCORSHeaders(origin)})
	case http.MethodGet:
		return managementResponse(http.StatusOK, "text/html; charset=utf-8", callbackGETPage(req.Query.Get("bridge") == "success"))
	case http.MethodPost:
		// Continue below.
	default:
		return managementResponse(http.StatusMethodNotAllowed, "text/plain; charset=utf-8", []byte("method not allowed"))
	}

	if !trustedCallbackOrigin(origin) {
		return callbackJSON(http.StatusForbidden, origin, false, "来源不受信任")
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(req.Headers.Get("Content-Type"), ";")[0]))
	if contentType != "application/x-www-form-urlencoded" {
		return callbackJSON(http.StatusUnsupportedMediaType, origin, false, "只接受表单提交")
	}
	body := req.Body
	if tunneled {
		body = callbackTunnelBody(req)
	}
	if len(body) == 0 || len(body) > oauthCallbackMaxBody {
		return callbackJSON(http.StatusRequestEntityTooLarge, origin, false, "请求体大小无效")
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return callbackJSON(http.StatusBadRequest, origin, false, "表单格式无效")
	}
	for key, values := range form {
		if len(values) != 1 || (key != "state" && key != "code" && key != "callback") {
			return callbackJSON(http.StatusBadRequest, origin, false, "表单字段无效")
		}
	}

	state := strings.TrimSpace(form.Get("state"))
	code := strings.TrimSpace(form.Get("code"))
	callbackURL := strings.TrimSpace(form.Get("callback"))
	if callbackURL != "" {
		if len(form) != 1 || origin == oauthBridgeOrigin || state != "" || code != "" {
			return callbackJSON(http.StatusBadRequest, origin, false, "callback 表单无效")
		}
		state, code, err = parseManualCallback(callbackURL)
		if err != nil {
			return callbackJSON(http.StatusBadRequest, origin, false, err.Error())
		}
	} else if origin != oauthBridgeOrigin {
		return callbackJSON(http.StatusBadRequest, origin, false, "手工提交必须包含完整 callback URL")
	} else if len(form) != 2 || state == "" || code == "" {
		return callbackJSON(http.StatusBadRequest, origin, false, "自动桥表单必须包含 state 和 code")
	}
	if len(state) != 64 || !isLowerHex(state) || len(code) == 0 || len(code) > oauthCallbackMaxField {
		return callbackJSON(http.StatusBadRequest, origin, false, "state 或 code 无效")
	}
	if !saveOAuthCallback(state, code, "") {
		return callbackJSON(http.StatusGone, origin, false, "登录会话不存在、已过期或已消费")
	}
	return callbackJSON(http.StatusOK, origin, true, "授权码已接收")
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
