package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetClaimManagementForTest(t *testing.T) {
	t.Helper()
	oldHTTP := claimHTTPDo
	oldRPC := callHostRPC
	oldPoolMax, oldPoolTTL := claimCaptchaPool.max, claimCaptchaPool.ttl
	claimCaptchaPool.clear()
	claimPreviews.Lock()
	claimPreviews.items = make(map[string]claimPreviewSnapshot)
	claimPreviews.Unlock()
	confirmations.Lock()
	confirmations.items = make(map[string]confirmationEntry)
	confirmations.Unlock()
	claimInFlight.Lock()
	claimInFlight.items = make(map[claimKey]struct{})
	claimInFlight.Unlock()
	claimAudit.Lock()
	claimAudit.events = nil
	claimAudit.Unlock()
	t.Cleanup(func() {
		claimHTTPDo = oldHTTP
		callHostRPC = oldRPC
		claimCaptchaPool.clear()
		claimCaptchaPool.max, claimCaptchaPool.ttl = oldPoolMax, oldPoolTTL
		claimPreviews.Lock()
		claimPreviews.items = make(map[string]claimPreviewSnapshot)
		claimPreviews.Unlock()
		confirmations.Lock()
		confirmations.items = make(map[string]confirmationEntry)
		confirmations.Unlock()
		claimInFlight.Lock()
		claimInFlight.items = make(map[claimKey]struct{})
		claimInFlight.Unlock()
		claimAudit.Lock()
		claimAudit.events = nil
		claimAudit.Unlock()
	})
}

func authGetJSON(t *testing.T, authIndex string, storage authStorage) []byte {
	t.Helper()
	stored, err := json.Marshal(storage)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(hostAuthGetResponse{AuthIndex: authIndex, JSON: stored})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func confirmedClaimValues(t *testing.T, authIndex, planID string) url.Values {
	t.Helper()
	token, err := createClaimConfirmation(normalizeClaimKey(authIndex, planID), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return url.Values{"action": {"claim"}, "auth_index": {authIndex}, "plan_id": {planID}, "confirmation_token": {token}, "confirm": {"true"}}
}

func TestClaimRefreshUsesHostAuthGetJWTOnlyAndHostCallback(t *testing.T) {
	resetClaimManagementForTest(t)
	callHostRPC = func(method string, request any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthGet {
			t.Fatalf("method=%q", method)
		}
		authIndex := request.(pluginapi.HostAuthGetRequest).AuthIndex
		if authIndex == "jwt" {
			return authGetJSON(t, authIndex, authStorage{ZCodeJWTToken: "secret-jwt", DeviceMID: "mid"}), nil
		}
		return authGetJSON(t, authIndex, authStorage{APIKey: "secret-key"}), nil
	}
	calls := 0
	claimHTTPDo = func(ctx context.Context, client pluginapi.HostHTTPClient, callback string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if callback != "host-callback" {
			t.Fatalf("callback=%q", callback)
		}
		if req.Method != http.MethodGet || req.URL != zcodeClaimPreviewURL {
			t.Fatalf("request=%+v", req)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"plans":[{"plan_id":"trial","name":"周末体验","description":"体验权益","starts_at":1700000000,"ends_at":1700086400,"entitlements":[{"entitlement_id":"tokens","show_name":"Token 权益","grant_units":1000,"unit_type":"tokens","period":"once"}]}]}}`)}, nil
	}
	page := zcodeStatusPage{Accounts: []zcodeStatusAccount{{AuthIndex: "jwt", Name: "JWT"}, {AuthIndex: "key", Name: "Key"}}}
	tried, ok, failed := refreshClaimPreviews(page, "host-callback")
	if tried != 1 || ok != 1 || failed != 0 || calls != 1 {
		t.Fatalf("counts=%d/%d/%d calls=%d", tried, ok, failed, calls)
	}
	snapshot, exists := getClaimPreviewSnapshot("jwt")
	if !exists || len(snapshot.Preview.Plans) != 1 || snapshot.Preview.Plans[0].Entitlements[0].ShowName != "Token 权益" {
		t.Fatalf("snapshot=%+v exists=%v", snapshot, exists)
	}
	if _, exists := getClaimPreviewSnapshot("key"); exists {
		t.Fatal("API key account received a claim preview")
	}
}

func TestClaimRefreshBoundsTimeoutBodyAndSanitizesErrors(t *testing.T) {
	resetClaimManagementForTest(t)
	callHostRPC = func(string, any) (json.RawMessage, error) {
		return authGetJSON(t, "jwt", authStorage{ZCodeJWTToken: "jwt-secret", DeviceMID: "mid-secret"}), nil
	}
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("claim request has no deadline")
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: make([]byte, claimMaxResponseBody+1)}, nil
	}
	result := refreshClaimPreview("jwt", "callback")
	if result.Error == "" || strings.Contains(result.Error, "jwt-secret") || strings.Contains(result.Error, "mid-secret") {
		t.Fatalf("unsafe error=%q", result.Error)
	}
	if _, exists := getClaimPreviewSnapshot("jwt"); exists {
		t.Fatal("oversized response was cached")
	}

	claimHTTPDo = func(context.Context, pluginapi.HostHTTPClient, string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{}, errors.New("Authorization: Bearer jwt-secret")
	}
	result = refreshClaimPreview("jwt", "callback")
	if result.Error == "" || strings.Contains(result.Error, "jwt-secret") {
		t.Fatalf("credential leaked: %q", result.Error)
	}
}

func TestManualClaimRequiresExplicitFieldsRecentPreviewAndMaterials(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{Plans: []claimPlan{{PlanID: "preview-plan", Name: "Trial"}}})
	base := confirmedClaimValues(t, "idx", "preview-plan")
	for _, field := range []string{"action", "auth_index", "plan_id", "confirmation_token", "confirm"} {
		values := url.Values{}
		for key, items := range base {
			values[key] = append([]string(nil), items...)
		}
		values.Del(field)
		if response := handleManualClaim(values, "callback"); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("missing %s status=%d", field, response.StatusCode)
		}
	}
	badPlan := confirmedClaimValues(t, "idx", "invented")
	if response := handleManualClaim(badPlan, "callback"); response.StatusCode != http.StatusConflict {
		t.Fatalf("invented plan status=%d", response.StatusCode)
	}

	for _, tc := range []struct {
		name    string
		storage authStorage
	}{
		{"jwt", authStorage{DeviceMID: "mid", CaptchaVerifyParam: "captcha"}},
		{"mid", authStorage{ZCodeJWTToken: "jwt", CaptchaVerifyParam: "captcha"}},
		{"captcha", authStorage{ZCodeJWTToken: "jwt", DeviceMID: "mid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callHostRPC = func(string, any) (json.RawMessage, error) { return authGetJSON(t, "idx", tc.storage), nil }
			if response := handleManualClaim(confirmedClaimValues(t, "idx", "preview-plan"), "callback"); response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
			}
		})
	}
}

func TestManualClaimExecutesOnceWithOptionalRegion(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{Plans: []claimPlan{{PlanID: "trial", Name: "Trial"}}})
	callHostRPC = func(method string, request any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthGet || request.(pluginapi.HostAuthGetRequest).AuthIndex != "idx" {
			t.Fatalf("unexpected auth get: %s %+v", method, request)
		}
		return authGetJSON(t, "idx", authStorage{ZCodeJWTToken: "jwt", DeviceMID: "mid", CaptchaVerifyParam: "captcha"}), nil
	}
	calls := 0
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, callback string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if callback != "host-callback" || req.Headers.Get("X-Aliyun-Captcha-Verify-Param") != "captcha" || req.Headers.Get("X-Aliyun-Captcha-Verify-Region") != "" {
			t.Fatalf("callback=%q headers=%v", callback, req.Headers)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"msg":"ok","data":{}}`)}, nil
	}
	response := handleManualClaim(confirmedClaimValues(t, "idx", "trial"), "host-callback")
	if response.StatusCode != http.StatusOK || calls != 1 || !strings.Contains(string(response.Body), "领取成功") {
		t.Fatalf("response=%+v calls=%d", response, calls)
	}
}

func TestClaimPageShowsPlanDetailsAndExplicitManualForm(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{Plans: []claimPlan{{
		PlanID: "trial", Name: "周末体验", Description: "体验权益", StartsAt: 1700000000, EndsAt: 1700086400,
		Entitlements: []claimEntitlement{{EntitlementID: "tokens", ShowName: "Token 权益", GrantUnits: 1000, UnitType: "tokens"}},
	}}})
	body := string(renderClaimManagement(zcodeStatusPage{Accounts: []zcodeStatusAccount{{AuthIndex: "idx", Name: "Account"}}}))
	for _, want := range []string{"周末体验", "Token 权益", "有效期", "可领取", `confirm_claim=1`, `auth_index=idx`, `plan_id=trial`} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	if strings.Contains(body, `name="confirm"`) {
		t.Fatal("listing page must not contain a hidden confirmation")
	}
}

func TestClaimConfirmationPageRequiresGETStage(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{Plans: []claimPlan{{PlanID: "trial"}}})
	request, err := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodGet, Query: url.Values{"confirm_claim": {"1"}, "auth_index": {"idx"}, "plan_id": {"trial"}}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleManagement(request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result pluginapi.ManagementResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	body := string(envelope.Result.Body)
	if envelope.Result.StatusCode != http.StatusOK || !strings.Contains(body, `name="confirmation_token"`) || !strings.Contains(body, `type="checkbox" name="confirm"`) {
		t.Fatalf("confirmation response=%+v", envelope.Result)
	}
	if strings.Contains(body, `type="hidden" name="confirm"`) {
		t.Fatal("confirmation was hidden instead of explicit")
	}
	for _, want := range []string{`id="claim-form"`, `Content-Type":"application/x-www-form-urlencoded"`, `正在领取，请稍候`, `claim-result`} {
		if !strings.Contains(body, want) {
			t.Fatalf("confirmation page missing %q", want)
		}
	}
}

func TestConfirmationTokenSingleUseExpiryAndBinding(t *testing.T) {
	resetClaimManagementForTest(t)
	key := normalizeClaimKey(" idx ", " trial ")
	token, err := createClaimConfirmation(key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if consumeClaimConfirmation(token, normalizeClaimKey("other", "trial"), time.Now()) {
		t.Fatal("wrong binding accepted")
	}
	if consumeClaimConfirmation(token, key, time.Now()) {
		t.Fatal("binding failure did not consume token")
	}
	expired, _ := createClaimConfirmation(key, time.Now().Add(-confirmationTTL-time.Second))
	if consumeClaimConfirmation(expired, key, time.Now()) {
		t.Fatal("expired token accepted")
	}
	valid, _ := createClaimConfirmation(key, time.Now())
	if !consumeClaimConfirmation(valid, key, time.Now()) || consumeClaimConfirmation(valid, key, time.Now()) {
		t.Fatal("token was not exactly-once")
	}
}

func TestManualClaimRejectsConcurrentSameIdentityPlanAndReleases(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{Plans: []claimPlan{{PlanID: "trial"}}})
	callHostRPC = func(string, any) (json.RawMessage, error) {
		return authGetJSON(t, "idx", authStorage{ZCodeJWTToken: "jwt", DeviceMID: "mid", CaptchaVerifyParam: "captcha"}), nil
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	claimHTTPDo = func(context.Context, pluginapi.HostHTTPClient, string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		close(entered)
		<-release
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0}`)}, nil
	}
	firstValues := confirmedClaimValues(t, "idx", "trial")
	firstDone := make(chan pluginapi.ManagementResponse, 1)
	go func() { firstDone <- handleManualClaim(firstValues, "callback") }()
	<-entered
	second := handleManualClaim(confirmedClaimValues(t, "idx", "trial"), "callback")
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent status=%d", second.StatusCode)
	}
	close(release)
	if first := <-firstDone; first.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d", first.StatusCode)
	}
	cacheClaimPreview("idx", claimPreview{Plans: []claimPlan{{PlanID: "trial"}}})
	claimHTTPDo = func(context.Context, pluginapi.HostHTTPClient, string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{}, errors.New("failed")
	}
	if got := handleManualClaim(confirmedClaimValues(t, "idx", "trial"), "callback"); got.StatusCode != http.StatusBadGateway {
		t.Fatalf("failure status=%d", got.StatusCode)
	}
	if !tryAcquireClaim(normalizeClaimKey("idx", "trial")) {
		t.Fatal("failure path leaked in-flight lock")
	}
	releaseClaim(normalizeClaimKey("idx", "trial"))
}

func TestClaimAuditIsBoundedAndRedacted(t *testing.T) {
	resetClaimManagementForTest(t)
	secretAuth := "auth-jwt-secret"
	secretBody := "captcha-and-request-body-secret"
	for i := 0; i < claimAuditLimit+10; i++ {
		appendClaimAudit(secretAuth, "plan-"+strings.Repeat("x", 200), "network_error", time.Now())
	}
	events := recentClaimAudit()
	if len(events) != claimAuditLimit {
		t.Fatalf("events=%d", len(events))
	}
	rendered := string(renderClaimAudit())
	if strings.Contains(rendered, secretAuth) || strings.Contains(rendered, secretBody) || strings.Contains(rendered, "jwt-secret") || strings.Contains(rendered, "captcha") {
		t.Fatalf("audit leaked sensitive material: %s", rendered)
	}
	if len(events[0].AuthHash) != 12 || len(events[0].PlanID) > 80 {
		t.Fatalf("unsafe event=%+v", events[0])
	}
}

func TestClaimRefreshRequestedIsExact(t *testing.T) {
	if !claimRefreshRequested(pluginapi.ManagementRequest{Query: url.Values{"refresh": {"claim"}}}) {
		t.Fatal("refresh=claim not recognized")
	}
	for _, value := range []string{"", "1", "quota", "claim "} {
		if claimRefreshRequested(pluginapi.ManagementRequest{Query: url.Values{"refresh": {value}}}) {
			t.Fatalf("refresh=%q recognized", value)
		}
	}
}

func TestManualClaimSnapshotCanExpire(t *testing.T) {
	resetClaimManagementForTest(t)
	claimPreviews.Lock()
	claimPreviews.items["idx"] = claimPreviewSnapshot{Preview: claimPreview{Plans: []claimPlan{{PlanID: "trial"}}}, Refreshed: time.Now().Add(-claimPreviewTTL - time.Second)}
	claimPreviews.Unlock()
	if _, ok := getClaimPreviewSnapshot("idx"); ok {
		t.Fatal("expired preview accepted")
	}
}

func TestManagementCaptchaPoolDispatch(t *testing.T) {
	resetClaimManagementForTest(t)
	post := func(values url.Values) pluginapi.ManagementResponse {
		t.Helper()
		request, err := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Body: []byte(values.Encode())}})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := handleManagement(request)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Result pluginapi.ManagementResponse `json:"result"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Result
	}
	if response := post(url.Values{"action": {"add_captcha"}, "param": {"verify-token-1"}}); response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), "验证码已加入池") {
		t.Fatalf("add status=%d body=%s", response.StatusCode, response.Body)
	}
	if response := post(url.Values{"action": {"add_captcha"}, "param": {"verify-token-2"}, "region": {"sgp"}}); response.StatusCode != http.StatusOK {
		t.Fatalf("add2 status=%d", response.StatusCode)
	}
	if got := claimCaptchaPool.stats(time.Now()); got != 2 {
		t.Fatalf("pool ready=%d", got)
	}
	if response := post(url.Values{"action": {"clear_captcha"}}); response.StatusCode != http.StatusOK || claimCaptchaPool.stats(time.Now()) != 0 {
		t.Fatalf("clear status=%d pool=%d", response.StatusCode, claimCaptchaPool.stats(time.Now()))
	}
	if response := post(url.Values{"action": {"add_captcha"}, "param": {"short"}}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid add status=%d", response.StatusCode)
	}
}
