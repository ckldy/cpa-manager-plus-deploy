package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetClaimAutoForTest(t *testing.T) {
	t.Helper()
	claimAuto.stop()
	claimAuto.mu.Lock()
	claimAuto.stopped = false
	claimAuto.holdUntil = make(map[string]time.Time)
	claimAuto.mu.Unlock()
	claimCaptchaPool.clear()
	claimAudit.Lock()
	claimAudit.events = nil
	claimAudit.Unlock()
	claimInFlight.Lock()
	claimInFlight.items = make(map[claimKey]struct{})
	claimInFlight.Unlock()
	confirmations.Lock()
	confirmations.items = make(map[string]confirmationEntry)
	confirmations.Unlock()
	t.Cleanup(func() {
		claimAuto.stop()
		claimAuto.mu.Lock()
		claimAuto.stopped = false
		claimAuto.holdUntil = nil
		claimAuto.mu.Unlock()
		claimCaptchaPool.clear()
		claimCaptchaPool.configure(0, 0)
		claimAudit.Lock()
		claimAudit.events = nil
		claimAudit.Unlock()
		claimInFlight.Lock()
		claimInFlight.items = make(map[claimKey]struct{})
		claimInFlight.Unlock()
	})
}

func setAutoClaimEnabled(t *testing.T, enabled bool) {
	t.Helper()
	routes.Lock()
	routes.config.StartPlanAutoClaim = enabled
	routes.Unlock()
}

func fakeAutoHost(t *testing.T, storages map[string]authStorage) {
	t.Helper()
	callHostRPC = func(method string, request any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthList {
			var files []pluginapi.HostAuthFileEntry
			for index := range storages {
				files = append(files, pluginapi.HostAuthFileEntry{Provider: "zcode", Type: "zcode", AuthIndex: index, Name: index, Label: index, Account: index})
			}
			raw, err := json.Marshal(hostAuthListResponse{Files: files})
			if err != nil {
				return nil, err
			}
			return raw, nil
		}
		if method == pluginabi.MethodHostAuthGet {
			index := request.(pluginapi.HostAuthGetRequest).AuthIndex
			return authGetJSON(t, index, storages[index]), nil
		}
		t.Fatalf("unexpected host method %s", method)
		return nil, nil
	}
}

func autoPreviewBody(plans ...string) string {
	encoded := ""
	for i, plan := range plans {
		if i > 0 {
			encoded += ","
		}
		encoded += `{"plan_id":"` + plan + `","name":"Trial","priority":` + strconv.Itoa(i+1) + `}`
	}
	return `{"code":0,"data":{"eligible":true,"claimable":true,"plans":[` + encoded + `]}}`
}

func autoClaimBody(code int, endsAtUnix int64) string {
	data := `{}`
	if endsAtUnix > 0 {
		data = fmt.Sprintf(`{"plan":{"ends_at":%d}}`, endsAtUnix)
	}
	return fmt.Sprintf(`{"code":%d,"msg":"done","data":%s}`, code, data)
}

func TestPickClaimAutoPlanConfiguredOrHighestPriority(t *testing.T) {
	plans := []claimPlan{{PlanID: "low", Priority: 1}, {PlanID: "high", Priority: 9}, {PlanID: "mid", Priority: 5}}
	if got := pickClaimAutoPlan("", plans); got == nil || got.PlanID != "high" {
		t.Fatalf("highest priority got %+v", got)
	}
	if got := pickClaimAutoPlan("mid", plans); got == nil || got.PlanID != "mid" {
		t.Fatalf("configured plan got %+v", got)
	}
	if got := pickClaimAutoPlan("missing", plans); got != nil {
		t.Fatalf("missing configured plan got %+v", got)
	}
}

func TestClaimAutoTickDisabledDoesNothing(t *testing.T) {
	resetClaimAutoForTest(t)
	setAutoClaimEnabled(t, false)
	if result := claimAuto.tick(time.Now()); result.Action != "disabled" {
		t.Fatalf("action=%s", result.Action)
	}
}

func TestClaimAutoTickClaimsHighestPriorityWithPoolToken(t *testing.T) {
	resetClaimAutoForTest(t)
	setAutoClaimEnabled(t, true)
	fakeAutoHost(t, map[string]authStorage{"acc": {ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}})
	if err := claimCaptchaPool.add("pool-param", "sgp", time.Now()); err != nil {
		t.Fatal(err)
	}
	var claimSeen http.Header
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.Method == http.MethodGet {
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoPreviewBody("plan-a", "plan-b"))}, nil
		}
		claimSeen = req.Headers
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoClaimBody(0, 2000000000))}, nil
	}
	now := time.Now()
	result := claimAuto.tick(now)
	if result.Action != "claimed" || result.PlanID != "plan-b" {
		t.Fatalf("result=%+v", result)
	}
	if claimSeen.Get("X-Aliyun-Captcha-Verify-Param") != "pool-param" || claimSeen.Get("X-Aliyun-Captcha-Verify-Region") != "sgp" {
		t.Fatalf("pool captcha not used: %v", claimSeen)
	}
	if got := claimCaptchaPool.stats(now); got != 0 {
		t.Fatalf("pool token not consumed: ready=%d", got)
	}
	if hold, ok := claimAuto.holdFor("acc"); !ok || !hold.After(now) {
		t.Fatalf("no hold after success: hold=%v ok=%v", hold, ok)
	}
	events := recentClaimAudit()
	if len(events) == 0 || events[len(events)-1].Result != "auto_success" {
		t.Fatalf("audit missing auto_success: %+v", events)
	}
}

func TestClaimAutoTickBacksOffToServerWindowOnAlreadyClaimed(t *testing.T) {
	resetClaimAutoForTest(t)
	setAutoClaimEnabled(t, true)
	fakeAutoHost(t, map[string]authStorage{"acc": {ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}})
	if err := claimCaptchaPool.add("pool-param", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	window := now.Add(45 * time.Minute).Unix()
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.Method == http.MethodGet {
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoPreviewBody("plan-a"))}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoClaimBody(1003, window))}, nil
	}
	result := claimAuto.tick(now)
	if result.Action != "failed" || result.Message == "" {
		t.Fatalf("result=%+v", result)
	}
	// Hold until the server-provided next window rather than the cooldown.
	hold, ok := claimAuto.holdFor("acc")
	if !ok || hold.Unix() != window {
		t.Fatalf("hold=%v ok=%v want %d", hold, ok, window)
	}
}

func TestClaimAutoTickUsesCooldownForOtherFailures(t *testing.T) {
	resetClaimAutoForTest(t)
	setAutoClaimEnabled(t, true)
	fakeAutoHost(t, map[string]authStorage{"acc": {ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}})
	if err := claimCaptchaPool.add("pool-param", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.Method == http.MethodGet {
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoPreviewBody("plan-a"))}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoClaimBody(3007, 0))}, nil
	}
	now := time.Now()
	result := claimAuto.tick(now)
	if result.Action != "failed" {
		t.Fatalf("result=%+v", result)
	}
	hold, ok := claimAuto.holdFor("acc")
	if !ok || !hold.After(now.Add(9*time.Minute)) {
		t.Fatalf("cooldown hold=%v ok=%v", hold, ok)
	}
	events := recentClaimAudit()
	if len(events) == 0 || events[len(events)-1].Result != "auto_captcha_failed" {
		t.Fatalf("audit missing auto_captcha_failed: %+v", events)
	}
}

func TestClaimAutoTickStopsOnLoginRequired(t *testing.T) {
	resetClaimAutoForTest(t)
	setAutoClaimEnabled(t, true)
	fakeAutoHost(t, map[string]authStorage{"acc": {ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}})
	if err := claimCaptchaPool.add("pool-param", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.Method == http.MethodGet {
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoPreviewBody("plan-a"))}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`{"code":401}`)}, nil
	}
	result := claimAuto.tick(time.Now())
	if result.Action != "stopped" {
		t.Fatalf("result=%+v", result)
	}
	if !claimAuto.isStopped() {
		t.Fatal("scheduler not stopped after login_required")
	}
}

func TestClaimAutoTickNoCaptchaHoldsPollInterval(t *testing.T) {
	resetClaimAutoForTest(t)
	setAutoClaimEnabled(t, true)
	fakeAutoHost(t, map[string]authStorage{"acc": {ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}})
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.Method == http.MethodGet {
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoPreviewBody("plan-a"))}, nil
		}
		t.Fatal("claim must not fire without a captcha token")
		return pluginapi.HTTPResponse{}, nil
	}
	now := time.Now()
	result := claimAuto.tick(now)
	if result.Action != "no_captcha" {
		t.Fatalf("result=%+v", result)
	}
	hold, ok := claimAuto.holdFor("acc")
	if !ok || !hold.After(now.Add(claimAutoDefaultPollInterval-time.Minute)) {
		t.Fatalf("no_captcha hold=%v ok=%v", hold, ok)
	}
}

func TestClaimAutoTickSkipsHeldAndDisabledAccounts(t *testing.T) {
	resetClaimAutoForTest(t)
	setAutoClaimEnabled(t, true)
	callHostRPC = func(method string, request any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthList {
			off := pluginapi.HostAuthFileEntry{Provider: "zcode", Type: "zcode", AuthIndex: "off", Name: "off", Disabled: true}
			held := pluginapi.HostAuthFileEntry{Provider: "zcode", Type: "zcode", AuthIndex: "held", Name: "held"}
			raw, _ := json.Marshal(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{off, held}})
			return raw, nil
		}
		if method == pluginabi.MethodHostAuthGet {
			index := request.(pluginapi.HostAuthGetRequest).AuthIndex
			return authGetJSON(t, index, authStorage{ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}), nil
		}
		t.Fatalf("unexpected method %s", method)
		return nil, nil
	}
	claimAuto.setHold("held", time.Now().Add(time.Hour))
	previews := 0
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.Method == http.MethodGet {
			previews++
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoPreviewBody("plan-a"))}, nil
		}
		t.Fatal("claim fired for a held/disabled account")
		return pluginapi.HTTPResponse{}, nil
	}
	result := claimAuto.tick(time.Now())
	if result.Action == "claimed" {
		t.Fatal("unexpected claim")
	}
	if previews != 0 {
		t.Fatalf("previews=%d want 0 (held and disabled accounts skipped before preview)", previews)
	}
}

func TestClaimAutoLifecycleStartsAndStopsOnConfig(t *testing.T) {
	resetClaimAutoForTest(t)
	fakeAutoHost(t, map[string]authStorage{})
	applyRouteConfig(lifecycleConfig("start_plan_auto_claim: true\n"))
	if !claimAuto.running {
		t.Fatal("scheduler did not start when enabled")
	}
	applyRouteConfig(lifecycleConfig("start_plan_auto_claim: false\n"))
	if claimAuto.running {
		t.Fatal("scheduler still running after disable")
	}
}

func TestClaimAutoConfigParsedFromHostYAML(t *testing.T) {
	resetClaimAutoForTest(t)
	fakeAutoHost(t, map[string]authStorage{})
	applyRouteConfig(lifecycleConfig("start_plan_auto_claim: true\nstart_plan_claim_plan_id: weekend-1\nstart_plan_claim_poll_interval_ms: 60000\nstart_plan_claim_cooldown_ms: 120000\ncaptcha_pool_max: 12\ncaptcha_pool_ttl_ms: 30000\n"))
	cfg := claimAutoConfigFromRoute(currentRouteConfig())
	if !cfg.Enabled || cfg.PlanID != "weekend-1" || cfg.PollInterval != time.Minute || cfg.Cooldown != 2*time.Minute {
		t.Fatalf("cfg=%+v", cfg)
	}
	if claimCaptchaPool.max != 12 || claimCaptchaPool.ttl != 30*time.Second {
		t.Fatalf("pool max=%d ttl=%s", claimCaptchaPool.max, claimCaptchaPool.ttl)
	}
	applyRouteConfig(lifecycleConfig(""))
	if claimAuto.running {
		t.Fatal("scheduler not stopped on reload without the flag")
	}
}

func TestClaimAuto3012StopsWithoutRetry(t *testing.T) {
	resetClaimAutoForTest(t)
	setAutoClaimEnabled(t, true)
	fakeAutoHost(t, map[string]authStorage{"acc": {ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}})
	if err := claimCaptchaPool.add("pool-param", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	claims := 0
	claimHTTPDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.Method == http.MethodGet {
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoPreviewBody("plan-a"))}, nil
		}
		claims++
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(autoClaimBody(3012, 0))}, nil
	}
	result := claimAuto.tick(time.Now())
	if result.Action != "stopped" || !strings.Contains(result.Message, "异常活动") {
		t.Fatalf("result=%+v", result)
	}
	if !claimAuto.isStopped() || claims != 1 {
		t.Fatalf("stopped=%v claims=%d", claimAuto.isStopped(), claims)
	}
	again := claimAuto.tick(time.Now().Add(time.Hour))
	if again.Action != "stopped" || claims != 1 {
		t.Fatalf("retry occurred: result=%+v claims=%d", again, claims)
	}
}
