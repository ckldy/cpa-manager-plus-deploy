// management.go implements the QoderWork management API and web panel:
// account dashboard (nickname, credits, plan, check-in streak), manual/auto
// check-in (daily at 09:00 and 21:00 local time), and quota refresh.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// billingBase hosts the Buddy-gas-station check-in and resource-package APIs.
// It is a var (not const) so tests can override it with an httptest server.
var billingBase = "https://openapi.qoder.com.cn"

// If the panel later wants to surface "usage export ready", re-add it and wire
// it into buildDashboardEx's response.

// -----------------------------------------------------------------------------
// Account listing via host auth callbacks
// -----------------------------------------------------------------------------

type creditsSummary struct {
	// TotalRemain is currently usable credits across all active packages.
	TotalRemain int64 `json:"total_remain"`
	// TotalUsed is consumed credits in the current cycle (sum of packages).
	TotalUsed int64 `json:"total_used"`
	// TotalSize is the credit capacity/pool (sum of package sizes). remain+used ≈ size.
	TotalSize int64 `json:"total_size"`
	// PackCount is number of resource packages included in the aggregate.
	PackCount int `json:"pack_count"`
	// FetchedAt is when this snapshot was taken (RFC3339). Upstream billing lag
	// can make remain/used look "stuck" for minutes after chat; compare this
	// timestamp — not only the numbers — when diagnosing frozen credits.
	FetchedAt string           `json:"fetched_at,omitempty"`
	Packages  []packageSummary `json:"packages"`
}

type packageSummary struct {
	Name       string `json:"name"`
	Remain     int64  `json:"remain"`
	Used       int64  `json:"used"`
	Size       int64  `json:"size"`
	CycleStart string `json:"cycle_start"`
	CycleEnd   string `json:"cycle_end"`
}

type checkinSummary struct {
	Active          bool     `json:"active"`
	TodayCheckedIn  bool     `json:"today_checked_in"`
	StreakDays      int64    `json:"streak_days"`
	DailyCredit     int64    `json:"daily_credit"`
	TodayCredit     int64    `json:"today_credit"`
	TotalCredits    int64    `json:"total_credits"`
	WeekCheckinDays int64    `json:"week_checkin_days"`
	ActivityName    string   `json:"activity_name"`
	Season          int64    `json:"season"`
	CheckinDates    []string `json:"checkin_dates,omitempty"`
}

// -----------------------------------------------------------------------------
// Auto check-in scheduler (09:00 / 21:00 local)
// -----------------------------------------------------------------------------

// Management API routes + handler
// -----------------------------------------------------------------------------

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementBasePathCache holds the host-injected BasePath so handleManagement
// doesn't hardcode /v0/management. Falls back to the historical default if the
// host doesn't provide one (older CPA builds).
var (
	managementBasePathCache   = "/v0/management"
	managementBasePathCacheMu sync.RWMutex

	panelCSRFMu    sync.RWMutex
	panelCSRFToken string
)

func initPanelCSRF() string {
	panelCSRFMu.RLock()
	token := panelCSRFToken
	panelCSRFMu.RUnlock()
	if token != "" {
		return token
	}

	panelCSRFMu.Lock()
	defer panelCSRFMu.Unlock()
	if panelCSRFToken == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err == nil {
			panelCSRFToken = hex.EncodeToString(raw)
		}
	}
	return panelCSRFToken
}

func panelCSRFValid(req pluginapi.ManagementRequest, values url.Values) bool {
	token := initPanelCSRF()
	return token != "" && values.Get("csrf") == token && strings.EqualFold(req.Headers.Get("X-Requested-With"), "qoderwork-panel")
}

var publicPanelFields = map[string]bool{
	"action":     true,
	"csrf":       true,
	"auth_index": true,
	"enabled":    true,
	"pat":        true,
	"confirm":    true,
}

func validPublicPanelForm(values url.Values) bool {
	for key, entries := range values {
		if !publicPanelFields[key] || len(entries) != 1 {
			return false
		}
	}
	return len(values["action"]) == 1
}

func publicPanelJSON(status int, value any) ([]byte, error) {
	return okEnvelope(mgmtJSONResponse(status, value))
}

func parsePanelBool(value string) (*bool, bool) {
	switch value {
	case "true":
		result := true
		return &result, true
	case "false":
		result := false
		return &result, true
	default:
		return nil, false
	}
}

func publicPanelRequest(req pluginapi.ManagementRequest) (pluginapi.ManagementRequest, url.Values, int, string) {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(req.Headers.Get("Content-Type"), ";")[0]))
	if contentType != "application/x-www-form-urlencoded" {
		return req, nil, http.StatusUnsupportedMediaType, "form content type required"
	}
	if len(req.Body) > 64<<10 {
		return req, nil, http.StatusRequestEntityTooLarge, "request too large"
	}
	values, err := url.ParseQuery(string(req.Body))
	if err != nil || !validPublicPanelForm(values) {
		return req, nil, http.StatusBadRequest, "invalid request"
	}
	if !panelCSRFValid(req, values) {
		return req, nil, http.StatusForbidden, "CSRF rejected"
	}
	body := map[string]any{}
	for key, entries := range values {
		if key == "action" || key == "csrf" {
			continue
		}
		body[key] = entries[0]
	}
	if raw, err := json.Marshal(body); err == nil {
		req.Body = raw
	}
	if authIndex := strings.TrimSpace(values.Get("auth_index")); authIndex != "" {
		if req.Query == nil {
			req.Query = url.Values{}
		}
		req.Query.Set("auth_index", authIndex)
	}
	return req, values, 0, ""
}

func handlePublicPanel(req pluginapi.ManagementRequest) ([]byte, error) {
	req, values, status, message := publicPanelRequest(req)
	if status != 0 {
		return publicPanelJSON(status, map[string]any{"error": message})
	}

	switch values.Get("action") {
	case "accounts":
		if len(values) != 2 {
			return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
		return publicPanelJSON(http.StatusOK, buildDashboardEx(false, false))
	case "refresh":
		if len(values) != 2 {
			return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
		return publicPanelJSON(http.StatusOK, buildDashboardEx(true, true))
	case "checkin":
		return publicPanelJSON(http.StatusOK, handleManualCheckin(req))
	case "checkin-config":
		if len(values["enabled"]) != 1 {
			return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
		enabled, ok := parsePanelBool(values.Get("enabled"))
		if !ok {
			return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "invalid enabled"})
		}
		raw, _ := json.Marshal(map[string]any{"enabled": enabled})
		req.Body = raw
		return publicPanelJSON(http.StatusOK, handleCheckinConfig(req))
	case "credits":
		if len(values["auth_index"]) != 1 {
			return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
		return publicPanelJSON(http.StatusOK, handleCreditsQuery(req))
	case "import":
		if len(values["pat"]) != 1 {
			return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
		return publicPanelJSON(http.StatusOK, handleImportPAT(req))
	case "select":
		if len(values["auth_index"]) != 1 {
			return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
		return publicPanelJSON(http.StatusOK, handleSelectAuth(req))
	case "claim-pro":
		if len(values["auth_index"]) != 1 || values.Get("confirm") != "claim-pro" {
			return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "confirmation required"})
		}
		return publicPanelJSON(http.StatusOK, handleClaimPro(req))
	default:
		return publicPanelJSON(http.StatusBadRequest, map[string]any{"error": "invalid action"})
	}
}

func loadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List QoderWork accounts with credits, plan and check-in status."},
			{Method: http.MethodPost, Path: base + "/refresh", Description: "Force refresh quota/cache for all accounts."},
			{Method: http.MethodPost, Path: base + "/checkin", Description: "Manually check in one account (auth_index) or all."},
			{Method: http.MethodPost, Path: base + "/checkin/config", Description: "Toggle auto check-in (enabled: true/false)."},
			{Method: http.MethodGet, Path: base + "/credits", Description: "Get real-time credits for one (auth_index query) or all accounts."},
			{Method: http.MethodPost, Path: base + "/import", Description: "Import a QoderWork PAT (pt-...) by exchanging it for a jobToken pair and persisting."},
			{Method: http.MethodPost, Path: base + "/select", Description: "Select the active account card used for chat routing (body: {auth_index})."},
			{Method: http.MethodPost, Path: base + "/keepalive", Description: "Manually refresh access tokens for all accounts (or one with auth_index)."},
			{Method: http.MethodPost, Path: base + "/claim-pro", Description: "Claim one-time Pro upgrade pack for one account (auth_index)."},
			{Method: http.MethodGet, Path: base + "/keepalive/status", Description: "Last keepalive run summary + config."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "QoderWork", Description: "QoderWork dashboard: credits, check-in, plan, import."},
			{Path: "/status", Description: "QoderWork health status."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser UI resource route: GET renders the panel, POST handles all panel
	// operations without a CPA management key. The explicit CSRF/header checks
	// above keep cross-origin forms from invoking account actions.
	resourceBase := "/v0/resource/plugins/" + providerName
	resource := resourceBase + "/panel"
	if req.Method == http.MethodGet && path == resourceBase+"/status" {
		return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{"provider": providerName, "status": "ok"}))
	}
	if path == resource {
		switch req.Method {
		case http.MethodGet:
			return okEnvelope(mgmtHTMLResponse(servePanel("/panel", initPanelCSRF())))
		case http.MethodPost:
			return handlePublicPanel(req)
		default:
			return publicPanelJSON(http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}

	// Retain historical resource aliases as read-only pages.
	resPrefix := "/v0/resource/plugins/" + providerName
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		return okEnvelope(mgmtHTMLResponse(servePanel(sub, initPanelCSRF())))
	}

	// Plugin-layer auth + rate limit for mutating endpoints.
	// The rate limiter ONLY guards against brute-force on the management key
	// (repeated auth failures from one IP). Authenticated requests must never
	// be throttled — the panel's normal operation (checkin + reload + claim)
	// can easily exceed 5 POSTs in quick succession.
	if req.Method == http.MethodPost || mutatingManagementPath(path) {
		ip := managementClientIP(req)
		if status, msg := checkManagementAuth(req); status != 0 {
			// Auth failed — consume a rate-limit token for this IP.
			if !allowManagementRequest(ip) {
				return okEnvelope(mgmtJSONResponse(http.StatusTooManyRequests, map[string]any{
					"error": "rate limit exceeded, try again later",
				}))
			}
			return okEnvelope(mgmtJSONResponse(status, map[string]any{"error": msg}))
		}
	}

	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(false, false)))
	case req.Method == http.MethodPost && path == base+"/refresh":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(true, true)))
	case req.Method == http.MethodPost && path == base+"/checkin":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleManualCheckin(req)))
	case req.Method == http.MethodPost && path == base+"/checkin/config":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCheckinConfig(req)))
	case req.Method == http.MethodGet && path == base+"/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCreditsQuery(req)))
	case req.Method == http.MethodPost && path == base+"/import":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImportPAT(req)))
	case req.Method == http.MethodPost && path == base+"/select":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleSelectAuth(req)))
	case req.Method == http.MethodPost && path == base+"/keepalive":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveNow(req)))
	case req.Method == http.MethodPost && path == base+"/claim-pro":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleClaimPro(req)))
	case req.Method == http.MethodGet && path == base+"/keepalive/status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveStatus()))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

// -----------------------------------------------------------------------------
// Plugin-layer management auth + rate limit (v0.6.31)
// -----------------------------------------------------------------------------
//
// When management_key is configured (config_yaml or WB_MANAGEMENT_KEY env), all
// mutating endpoints under /v0/management/plugins/qoderwork/* require a matching
// Bearer token. Read-only GET endpoints (accounts/credits/panel) pass through so
// the panel can render before the user has pasted a key — the panel itself
// supplies the key on every call via Authorization header.
//
// A per-IP token-bucket rate limiter guards against brute-force when the key
// check fails repeatedly.

const (
	mgmtRateLimitCapacity = 5                // burst
	mgmtRateLimitRefill   = time.Minute / 10 // 1 token per 6s
	mgmtRateLimitTTL      = 10 * time.Minute // idle entry eviction
)

type mgmtRateEntry struct {
	tokens   float64
	lastSeen time.Time
}

var (
	mgmtRateLimit   = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu sync.Mutex
)

func loadedManagementKey() string {
	managementAPIKeyMu.RLock()
	defer managementAPIKeyMu.RUnlock()
	return managementAPIKey
}

// checkManagementAuth returns an HTTP status + error message when the request
// should be rejected. status=0 means allow.
// checkManagementAuth mirrors the community-plugin convention (grok-panel):
// trust the host middleware. The host already authenticated the request with
// the CPA management key before forwarding to the plugin — re-checking here
// would force operators to configure a second key just for the plugin.
//
// If a management_key is explicitly configured (config_yaml management_key: or
// WB_MANAGEMENT_KEY env), we enforce it as defence-in-depth on top of the
// host check. Otherwise (the default) we return 0 and let the request through.
func checkManagementAuth(req pluginapi.ManagementRequest) (int, string) {
	want := loadedManagementKey()
	if want == "" {
		return 0, "" // trust host middleware (community convention)
	}
	got := strings.TrimSpace(req.Headers.Get("Authorization"))
	if !strings.HasPrefix(got, "Bearer ") {
		return http.StatusUnauthorized, "missing Bearer token"
	}
	token := strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return http.StatusForbidden, "invalid management key"
	}
	return 0, ""
}

// allowManagementRequest applies a per-IP token bucket. ip may be empty when the
// host doesn't forward X-Forwarded-For / RemoteAddr — in that case use a single
// global bucket.
func allowManagementRequest(ip string) bool {
	if ip == "" {
		ip = "_global"
	}
	mgmtRateLimitMu.Lock()
	defer mgmtRateLimitMu.Unlock()
	now := time.Now()
	e, ok := mgmtRateLimit[ip]
	if !ok {
		e = &mgmtRateEntry{tokens: mgmtRateLimitCapacity, lastSeen: now}
		mgmtRateLimit[ip] = e
	}
	// Refill.
	elapsed := now.Sub(e.lastSeen)
	e.tokens += float64(elapsed) / float64(mgmtRateLimitRefill)
	if e.tokens > mgmtRateLimitCapacity {
		e.tokens = mgmtRateLimitCapacity
	}
	e.lastSeen = now
	if e.tokens < 1 {
		return false
	}
	e.tokens--
	// Lazy eviction of idle entries (don't grow the map forever).
	if len(mgmtRateLimit) > 1024 {
		for k, v := range mgmtRateLimit {
			if now.Sub(v.lastSeen) > mgmtRateLimitTTL {
				delete(mgmtRateLimit, k)
			}
		}
	}
	return true
}

// managementClientIP extracts a best-effort client identifier for rate limiting.
// CPA host doesn't currently forward RemoteAddr, so fall back to X-Forwarded-For
// / X-Real-IP headers if the deployment adds them via a reverse proxy.
func managementClientIP(req pluginapi.ManagementRequest) string {
	if xff := strings.TrimSpace(req.Headers.Get("X-Forwarded-For")); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if xr := strings.TrimSpace(req.Headers.Get("X-Real-Ip")); xr != "" {
		return xr
	}
	return ""
}

// mutatingManagementPath reports whether the path performs a write (checkin,
// import, trial claim, select, refresh, config toggle). Read endpoints pass.
func mutatingManagementPath(path string) bool {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch path {
	case base + "/refresh",
		base + "/checkin",
		base + "/checkin/config",
		base + "/import",
		base + "/select",
		base + "/keepalive",
		base + "/claim-pro":
		return true
	}
	return false
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

// checkinLocks serializes per-account manual check-in (B4).
// Entries are pruned during dashboard prune to avoid unbounded growth
// when auth accounts are deleted/rotated.
var (
	checkinLocks sync.Map // auth_index -> *sync.Mutex
)
