package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	zcodeClaimPreviewBaseURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/preview"
	zcodeClaimURL            = "https://zcode.z.ai/api/v1/zcode-plan/billing/claim"
	claimMaxResponseBody     = 256 * 1024
	claimMaxRequestBody      = 16 * 1024
	claimRequestTimeout      = 12 * time.Second
	claimPreviewTTL          = 30 * time.Minute
	claimAuditLimit          = 100
)

var zcodeClaimPreviewURL = func() string {
	query := url.Values{"app_version": []string{zcodeClientVersion}, "platform": []string{"linux-x64"}}
	return zcodeClaimPreviewBaseURL + "?" + query.Encode()
}()

type claimEntitlement struct {
	EntitlementID string   `json:"entitlement_id"`
	ShowName      string   `json:"show_name,omitempty"`
	Meter         string   `json:"meter,omitempty"`
	UnitType      string   `json:"unit_type,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	GrantUnits    float64  `json:"grant_units,omitempty"`
	Period        string   `json:"period,omitempty"`
	Priority      int      `json:"priority,omitempty"`
	EffectiveAt   int64    `json:"effective_at,omitempty"`
}

type claimPlan struct {
	PlanID       string             `json:"plan_id"`
	Name         string             `json:"name,omitempty"`
	Description  string             `json:"description,omitempty"`
	Priority     int                `json:"priority,omitempty"`
	Entitlements []claimEntitlement `json:"entitlements,omitempty"`
	StartsAt     int64              `json:"starts_at,omitempty"`
	EndsAt       int64              `json:"ends_at,omitempty"`
}

type claimPreview struct {
	Eligible  bool        `json:"eligible"`
	Claimable bool        `json:"claimable"`
	Plans     []claimPlan `json:"plans,omitempty"`
}

type claimResult struct {
	Code     int             `json:"code"`
	Category string          `json:"category"`
	Message  string          `json:"message"`
	Data     json.RawMessage `json:"data,omitempty"`
}

type claimPreviewSnapshot struct {
	Preview   claimPreview
	Refreshed time.Time
	Error     string
}

var claimPreviews = struct {
	sync.RWMutex
	items map[string]claimPreviewSnapshot
}{items: make(map[string]claimPreviewSnapshot)}

var claimHTTPDo = executeHTTP

type claimKey struct {
	AuthIndex string
	PlanID    string
}

type claimAuditEvent struct {
	Time     time.Time
	AuthHash string
	PlanID   string
	Result   string
}

var claimInFlight = struct {
	sync.Mutex
	items map[claimKey]struct{}
}{items: make(map[claimKey]struct{})}

var claimAudit = struct {
	sync.RWMutex
	events []claimAuditEvent
}{}

func normalizeClaimKey(authIndex, planID string) claimKey {
	return claimKey{AuthIndex: strings.TrimSpace(authIndex), PlanID: strings.TrimSpace(planID)}
}

func claimConfirmationBinding(key claimKey) string {
	return key.AuthIndex + "\x00" + key.PlanID
}

func createClaimConfirmation(key claimKey, now time.Time) (string, error) {
	if key.AuthIndex == "" || key.PlanID == "" {
		return "", fmt.Errorf("invalid confirmation binding")
	}
	return confirmations.create("claim", claimConfirmationBinding(key), now)
}

func consumeClaimConfirmation(token string, key claimKey, now time.Time) bool {
	return confirmations.consume(strings.TrimSpace(token), "claim", claimConfirmationBinding(key), now)
}

func tryAcquireClaim(key claimKey) bool {
	claimInFlight.Lock()
	defer claimInFlight.Unlock()
	if _, exists := claimInFlight.items[key]; exists {
		return false
	}
	claimInFlight.items[key] = struct{}{}
	return true
}

func releaseClaim(key claimKey) {
	claimInFlight.Lock()
	delete(claimInFlight.items, key)
	claimInFlight.Unlock()
}

func appendClaimAudit(authIndex, planID, result string, now time.Time) {
	digest := sha256.Sum256([]byte(strings.TrimSpace(authIndex)))
	planID = strings.TrimSpace(planID)
	if len(planID) > 80 {
		planID = planID[:80]
	}
	event := claimAuditEvent{Time: now, AuthHash: hex.EncodeToString(digest[:])[:12], PlanID: planID, Result: result}
	claimAudit.Lock()
	if len(claimAudit.events) >= claimAuditLimit {
		copy(claimAudit.events, claimAudit.events[len(claimAudit.events)-claimAuditLimit+1:])
		claimAudit.events = claimAudit.events[:claimAuditLimit-1]
	}
	claimAudit.events = append(claimAudit.events, event)
	claimAudit.Unlock()
}

func recentClaimAudit() []claimAuditEvent {
	claimAudit.RLock()
	defer claimAudit.RUnlock()
	out := make([]claimAuditEvent, len(claimAudit.events))
	copy(out, claimAudit.events)
	return out
}

func buildClaimPreviewRequest(storage authStorage) (pluginapi.HTTPRequest, error) {
	if err := validateClaimStorage(storage); err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	return pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     zcodeClaimPreviewURL,
		Headers: claimHeaders(storage),
	}, nil
}

func buildClaimRequest(storage authStorage, planID string) (pluginapi.HTTPRequest, error) {
	if err := validateClaimStorage(storage); err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return pluginapi.HTTPRequest{}, errors.New("missing plan_id")
	}
	body, err := json.Marshal(map[string]string{"plan_id": planID})
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	headers := claimHeaders(storage)
	headers.Set("Content-Type", "application/json")
	return pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     zcodeClaimURL,
		Headers: headers,
		Body:    body,
	}, nil
}

func validateClaimStorage(storage authStorage) error {
	if strings.TrimSpace(storage.ZCodeJWTToken) == "" {
		return errors.New("missing zcode_jwt_token")
	}
	if strings.TrimSpace(storage.DeviceMID) == "" {
		return errors.New("missing device_mid")
	}
	return nil
}

func validateManualClaimStorage(storage authStorage) error {
	if err := validateClaimStorage(storage); err != nil {
		return err
	}
	if strings.TrimSpace(storage.CaptchaVerifyParam) == "" {
		return errors.New("missing captcha_verify_param")
	}
	return nil
}

func claimHeaders(storage authStorage) http.Header {
	headers := zcodeHeaders(storage.ZCodeJWTToken, storage.CaptchaVerifyParam, storage.CaptchaVerifyRegion)
	headers.Set("X-Device-Mid", strings.TrimSpace(storage.DeviceMID))
	return headers
}

func parseClaimPreview(body []byte) (claimPreview, error) {
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data claimPreview    `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return claimPreview{}, fmt.Errorf("decode claim preview: %w", err)
	}
	code, err := parseBusinessCode(envelope.Code)
	if err != nil {
		return claimPreview{}, err
	}
	if code != 0 && code != 200 {
		category, message := classifyClaimBusinessCode(code, envelope.Msg)
		return claimPreview{}, fmt.Errorf("claim preview %s: %s", category, message)
	}
	return envelope.Data, nil
}

func parseClaimResult(body []byte) (claimResult, error) {
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return claimResult{}, fmt.Errorf("decode claim result: %w", err)
	}
	code, err := parseBusinessCode(envelope.Code)
	if err != nil {
		return claimResult{}, err
	}
	category, message := classifyClaimBusinessCode(code, envelope.Msg)
	return claimResult{Code: code, Category: category, Message: message, Data: envelope.Data}, nil
}

func parseBusinessCode(raw json.RawMessage) (int, error) {
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return 0, errors.New("missing claim business code")
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		code, err := strconv.Atoi(strings.TrimSpace(text))
		if err == nil {
			return code, nil
		}
	}
	return 0, errors.New("invalid claim business code")
}

func classifyClaimBusinessCode(code int, upstreamMessage string) (category, message string) {
	switch code {
	case 0, 200:
		category, message = "success", "领取成功"
	case 1002:
		category, message = "activity_unavailable", "活动当前不可用"
	case 1003:
		category, message = "already_claimed", "该套餐已经领取"
	case 1004:
		category, message = "ineligible", "当前账号不符合领取条件"
	case 1005:
		category, message = "quota_exhausted", "活动名额已用完"
	case 3001:
		category, message = "invalid_device_or_parameter", "请求参数或设备 MID 无效"
	case 3007:
		category, message = "captcha_failed", "验证码校验失败"
	case 401:
		category, message = "login_required", "登录已失效，需要重新登录"
	default:
		category, message = "unknown", strings.TrimSpace(upstreamMessage)
		if message == "" {
			message = fmt.Sprintf("未知业务码 %d", code)
		}
	}
	return category, message
}

func cacheClaimPreview(authIndex string, preview claimPreview) {
	claimPreviews.Lock()
	claimPreviews.items[authIndex] = claimPreviewSnapshot{Preview: preview, Refreshed: time.Now()}
	claimPreviews.Unlock()
}

func getClaimPreviewSnapshot(authIndex string) (claimPreviewSnapshot, bool) {
	claimPreviews.RLock()
	snapshot, ok := claimPreviews.items[authIndex]
	claimPreviews.RUnlock()
	if !ok || time.Since(snapshot.Refreshed) > claimPreviewTTL {
		return claimPreviewSnapshot{}, false
	}
	return snapshot, true
}

func loadClaimStorage(authIndex string) (authStorage, error) {
	raw, err := callHostRPC(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return authStorage{}, errors.New("无法读取账号凭证")
	}
	var response hostAuthGetResponse
	var storage authStorage
	if json.Unmarshal(raw, &response) != nil || json.Unmarshal(response.JSON, &storage) != nil {
		return authStorage{}, errors.New("无法读取账号凭证")
	}
	return storage, nil
}

func refreshClaimPreviews(page zcodeStatusPage, hostCallbackID string) (tried, succeeded, failed int) {
	for _, account := range page.Accounts {
		storage, err := loadClaimStorage(account.AuthIndex)
		if err != nil || strings.TrimSpace(storage.ZCodeJWTToken) == "" {
			continue
		}
		tried++
		result := refreshClaimPreviewWithStorage(account.AuthIndex, hostCallbackID, storage)
		if result.Error == "" {
			succeeded++
		} else {
			failed++
		}
	}
	return
}

func refreshClaimPreview(authIndex, hostCallbackID string) claimPreviewSnapshot {
	storage, err := loadClaimStorage(authIndex)
	if err != nil {
		return claimPreviewSnapshot{Refreshed: time.Now(), Error: "无法读取账号凭证"}
	}
	return refreshClaimPreviewWithStorage(authIndex, hostCallbackID, storage)
}

func refreshClaimPreviewWithStorage(authIndex, hostCallbackID string, storage authStorage) claimPreviewSnapshot {
	out := claimPreviewSnapshot{Refreshed: time.Now()}
	request, err := buildClaimPreviewRequest(storage)
	if err != nil {
		out.Error = "账号缺少 JWT 或设备 MID"
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), claimRequestTimeout)
	defer cancel()
	response, err := claimHTTPDo(ctx, nil, hostCallbackID, request)
	if err != nil {
		out.Error = "套餐预览网络失败"
		return out
	}
	if len(response.Body) > claimMaxResponseBody {
		out.Error = "套餐预览响应过大"
		return out
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		out.Error = fmt.Sprintf("套餐预览接口返回 HTTP %d", response.StatusCode)
		return out
	}
	preview, err := parseClaimPreview(response.Body)
	if err != nil {
		out.Error = "套餐预览响应无法解析"
		return out
	}
	valid := preview.Plans[:0]
	for _, plan := range preview.Plans {
		plan.PlanID = strings.TrimSpace(plan.PlanID)
		if plan.PlanID != "" {
			valid = append(valid, plan)
		}
	}
	preview.Plans = valid
	out.Preview = preview
	cacheClaimPreview(authIndex, preview)
	return out
}

func claimPlanInRecentPreview(authIndex, planID string) bool {
	snapshot, ok := getClaimPreviewSnapshot(authIndex)
	if !ok {
		return false
	}
	for _, plan := range snapshot.Preview.Plans {
		if plan.PlanID == planID {
			return true
		}
	}
	return false
}

func sortedClaimSnapshots(page zcodeStatusPage) []struct {
	Account zcodeStatusAccount
	Preview claimPreviewSnapshot
} {
	items := make([]struct {
		Account zcodeStatusAccount
		Preview claimPreviewSnapshot
	}, 0)
	for _, account := range page.Accounts {
		if snapshot, ok := getClaimPreviewSnapshot(account.AuthIndex); ok {
			items = append(items, struct {
				Account zcodeStatusAccount
				Preview claimPreviewSnapshot
			}{account, snapshot})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Account.Name < items[j].Account.Name })
	return items
}
