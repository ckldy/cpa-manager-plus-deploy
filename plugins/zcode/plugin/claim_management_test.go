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
			return authGetJSON(t, authIndex, authStorage{ZCodeJWTToken: "secret-jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}), nil
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

func TestClaimPreviewRequestUsesCurrentIdentityHeadersAndUUIDDeviceMID(t *testing.T) {
	storage := authStorage{
		ZCodeJWTToken: "jwt-secret",
		DeviceMID:     "11111111-2222-4333-8444-555555555555",
	}
	request, err := buildClaimPreviewRequest(storage)
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != http.MethodGet {
		t.Fatalf("method=%q", request.Method)
	}
	parsed, err := url.Parse(request.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/api/v1/zcode-plan/billing/preview" {
		t.Fatalf("path=%q", parsed.Path)
	}
	if parsed.Query().Get("app_version") != zcodeClientVersion {
		t.Fatalf("app_version=%q", parsed.Query().Get("app_version"))
	}
	if parsed.Query().Get("platform") != "linux-x64" {
		t.Fatalf("platform query=%q", parsed.Query().Get("platform"))
	}
	for name, want := range map[string]string{
		"Authorization":       "Bearer jwt-secret",
		"X-ZCode-App-Version": zcodeClientVersion,
		"X-Platform":          "linux-x64",
		"X-Device-Mid":        storage.DeviceMID,
	} {
		if got := request.Headers.Get(name); got != want {
			t.Fatalf("%s=%q want %q", name, got, want)
		}
	}
	if request.Headers.Get("X-ZCode-Agent") != "" {
		t.Fatal("billing control-plane request must omit X-ZCode-Agent")
	}
}

func TestClaimStorageRequiresUUIDDeviceMID(t *testing.T) {
	for _, mid := range []string{"", "mid", "mid-0001", "11111111-2222-3333-4444-555555555555"} {
		if err := validateClaimStorage(authStorage{ZCodeJWTToken: "jwt", DeviceMID: mid}); err == nil {
			t.Fatalf("invalid device MID accepted: %q", mid)
		}
	}
	if err := validateClaimStorage(authStorage{ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}); err != nil {
		t.Fatalf("valid UUID rejected: %v", err)
	}
}

func TestClaimPageUsesDynamicPreviewPlanIDAndClaimability(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{
		Eligible:  true,
		Claimable: true,
		Plans: []claimPlan{{
			PlanID:       "campaign-2026-09-13",
			Name:         "2026-09-13 体验活动",
			StartsAt:     1789232400,
			EndsAt:       1789318800,
			Entitlements: []claimEntitlement{{ShowName: "Token 权益", GrantUnits: 2000, UnitType: "tokens"}},
		}},
	})
	body := string(renderClaimManagement(zcodeStatusPage{Accounts: []zcodeStatusAccount{{AuthIndex: "idx", Name: "Account"}}}))
	for _, want := range []string{"2026-09-13 体验活动", "2000 tokens", "可领取", "plan_id=campaign-2026-09-13"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	if strings.Contains(body, "weekend-1") {
		t.Fatal("management page used a stale hard-coded plan_id")
	}
}

func TestClaimPageDisablesClaimWhenPreviewNotClaimableAndShowsError(t *testing.T) {
	resetClaimManagementForTest(t)
	claimPreviews.Lock()
	claimPreviews.items["idx"] = claimPreviewSnapshot{
		Preview: claimPreview{
			Eligible:  false,
			Claimable: false,
			Plans:     []claimPlan{{PlanID: "campaign-2026-09-13", Name: "2026-09-13 体验活动"}},
		},
		Refreshed: time.Now(),
		Error:     "请求参数或设备 MID 无效",
	}
	claimPreviews.Unlock()
	body := string(renderClaimManagement(zcodeStatusPage{Accounts: []zcodeStatusAccount{{AuthIndex: "idx", Name: "Account"}}}))
	if !strings.Contains(body, "请求参数或设备 MID 无效") {
		t.Fatal("preview error not shown")
	}
	if strings.Contains(body, "confirm_claim=1") {
		t.Fatal("claim action shown for a non-claimable preview")
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
	cacheClaimPreview("idx", claimPreview{Eligible: true, Claimable: true, Plans: []claimPlan{{PlanID: "preview-plan", Name: "Trial"}}})
	base := officialClaimValues(t, "idx", "preview-plan", "official-one-time-param", "")
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
	badPlan := officialClaimValues(t, "idx", "invented", "official-one-time-param", "")
	if response := handleManualClaim(badPlan, "callback"); response.StatusCode != http.StatusConflict {
		t.Fatalf("invented plan status=%d", response.StatusCode)
	}

	for _, tc := range []struct {
		name    string
		storage authStorage
	}{
		{"jwt", authStorage{DeviceMID: "11111111-2222-4333-8444-555555555555", CaptchaVerifyParam: "persistent-must-not-be-used"}},
		{"mid", authStorage{ZCodeJWTToken: "jwt", CaptchaVerifyParam: "persistent-must-not-be-used"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callHostRPC = func(string, any) (json.RawMessage, error) { return authGetJSON(t, "idx", tc.storage), nil }
			if response := handleManualClaim(officialClaimValues(t, "idx", "preview-plan", "official-one-time-param", ""), "callback"); response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
			}
		})
	}
}

func TestManualClaimExecutesOnceWithOptionalRegion(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{Eligible: true, Claimable: true, Plans: []claimPlan{{PlanID: "trial", Name: "Trial"}}})
	callHostRPC = func(method string, request any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthGet || request.(pluginapi.HostAuthGetRequest).AuthIndex != "idx" {
			t.Fatalf("unexpected auth get: %s %+v", method, request)
		}
		return authGetJSON(t, "idx", authStorage{ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555", CaptchaVerifyParam: "captcha"}), nil
	}
	calls := 0
	claimHTTPDo = func(ctx context.Context, _ pluginapi.HostHTTPClient, callback string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if callback != "host-callback" {
			t.Fatalf("callback=%q", callback)
		}
		if req.Method == http.MethodPost {
			if req.Headers.Get("X-Aliyun-Captcha-Verify-Param") != "official-one-time-param" || req.Headers.Get("X-Aliyun-Captcha-Verify-Region") != "sgp" {
				t.Fatalf("claim headers=%v", req.Headers)
			}
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0}`)}, nil
		}
		if req.Method != http.MethodGet || req.URL != zcodeBillingCurrentURL {
			t.Fatalf("confirmation request=%+v", req)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}]}}`)}, nil
	}
	response := handleManualClaim(officialClaimValues(t, "idx", "trial", "official-one-time-param", "sgp"), "host-callback")
	if response.StatusCode != http.StatusOK || calls != 2 || !strings.Contains(string(response.Body), "已到账") {
		t.Fatalf("response=%+v calls=%d", response, calls)
	}
}

func TestActiveTrialEntitlementRequiresEffectiveWindow(t *testing.T) {
	now := time.Unix(1700000100, 0)
	active := claimPlan{PlanID: "trial", StartsAt: now.Add(-time.Hour).Unix(), EndsAt: now.Add(time.Hour).Unix()}
	future := claimPlan{PlanID: "future", StartsAt: now.Add(time.Hour).Unix(), EndsAt: now.Add(2 * time.Hour).Unix()}
	expired := claimPlan{PlanID: "expired", StartsAt: now.Add(-2 * time.Hour).Unix(), EndsAt: now.Add(-time.Hour).Unix()}
	if !claimPlanActiveAt(active, now) || claimPlanActiveAt(future, now) || claimPlanActiveAt(expired, now) {
		t.Fatal("体验套餐生效窗口判断错误")
	}
}

func TestTrialModelCapabilityMustBeExplicit(t *testing.T) {
	plan := claimPlan{PlanID: "trial", Entitlements: []claimEntitlement{{Capabilities: []string{"glm-4.6", "GLM-4.7"}}}}
	if !claimPlanAllowsModel(plan, "zcode-glm-4.6") || !claimPlanAllowsModel(plan, "zcode-glm-4.7") {
		t.Fatal("明确声明的体验套餐模型未被识别")
	}
	if claimPlanAllowsModel(plan, "zcode-glm-5.1") {
		t.Fatal("未明确声明的模型被错误标记为体验套餐免费")
	}
}

func TestClaimPageShowsPlanDetailsAndExplicitManualForm(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{Eligible: true, Claimable: true, Plans: []claimPlan{{
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
	cacheClaimPreview("idx", claimPreview{Eligible: true, Claimable: true, Plans: []claimPlan{{PlanID: "trial"}}})
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
	for _, want := range []string{`id="claim-form"`, `Content-Type":"application/x-www-form-urlencoded"`, `正在提交一次性官方验证材料并检查到账状态`, `claim-result`} {
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
	cacheClaimPreview("idx", claimPreview{Eligible: true, Claimable: true, Plans: []claimPlan{{PlanID: "trial"}}})
	callHostRPC = func(string, any) (json.RawMessage, error) {
		return authGetJSON(t, "idx", authStorage{ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555", CaptchaVerifyParam: "captcha"}), nil
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	claimHTTPDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.Method == http.MethodPost {
			close(entered)
			<-release
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0}`)}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}]}}`)}, nil
	}
	firstValues := officialClaimValues(t, "idx", "trial", "official-one-time-param", "")
	firstDone := make(chan pluginapi.ManagementResponse, 1)
	go func() { firstDone <- handleManualClaim(firstValues, "callback") }()
	<-entered
	second := handleManualClaim(officialClaimValues(t, "idx", "trial", "official-one-time-param", ""), "callback")
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent status=%d", second.StatusCode)
	}
	close(release)
	if first := <-firstDone; first.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d", first.StatusCode)
	}
	cacheClaimPreview("idx", claimPreview{Eligible: true, Claimable: true, Plans: []claimPlan{{PlanID: "trial"}}})
	claimHTTPDo = func(context.Context, pluginapi.HostHTTPClient, string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{}, errors.New("failed")
	}
	if got := handleManualClaim(officialClaimValues(t, "idx", "trial", "official-one-time-param", ""), "callback"); got.StatusCode != http.StatusBadGateway {
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

func officialClaimValues(t *testing.T, authIndex, planID, verifyParam, region string) url.Values {
	t.Helper()
	values := confirmedClaimValues(t, authIndex, planID)
	values.Set("verify_param", verifyParam)
	if region != "" {
		values.Set("region", region)
	}
	return values
}

func TestManualClaimRequiresOneTimeOfficialVerifyParamAndIgnoresPersistentCaptcha(t *testing.T) {
	resetClaimManagementForTest(t)
	cacheClaimPreview("idx", claimPreview{Eligible: true, Claimable: true, Plans: []claimPlan{{PlanID: "trial"}}})
	callHostRPC = func(method string, request any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthGet || request.(pluginapi.HostAuthGetRequest).AuthIndex != "idx" {
			t.Fatalf("unexpected auth lookup: %s %+v", method, request)
		}
		return authGetJSON(t, "idx", authStorage{ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555", CaptchaVerifyParam: "persistent-must-not-be-used"}), nil
	}
	if got := handleManualClaim(confirmedClaimValues(t, "idx", "trial"), "callback"); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing one-time verifyParam status=%d body=%s", got.StatusCode, got.Body)
	}
	values := officialClaimValues(t, "idx", "trial", "official-one-time-param", "sgp")
	calls := 0
	claimHTTPDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if req.Method == http.MethodPost {
			if got := req.Headers.Get("X-Aliyun-Captcha-Verify-Param"); got != "official-one-time-param" {
				t.Fatalf("claim verifyParam=%q", got)
			}
			if got := req.Headers.Get("X-Aliyun-Captcha-Verify-Region"); got != "sgp" {
				t.Fatalf("claim region=%q", got)
			}
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0}`)}, nil
		}
		if req.Method != http.MethodGet || req.URL != zcodeBillingCurrentURL {
			t.Fatalf("confirmation request=%+v", req)
		}
		if req.Headers.Get("Authorization") != "Bearer jwt" || req.Headers.Get("X-Device-Mid") != "11111111-2222-4333-8444-555555555555" {
			t.Fatalf("billing identity mismatch: %v", req.Headers)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}]}}`)}, nil
	}
	beforePool := claimCaptchaPool.stats(time.Now())
	beforeAudit := len(recentClaimAudit())
	got := handleManualClaim(values, "callback")
	if got.StatusCode != http.StatusOK || !strings.Contains(string(got.Body), "已到账") {
		t.Fatalf("response=%+v", got)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want claim plus billing/current", calls)
	}
	if claimCaptchaPool.stats(time.Now()) != beforePool || len(recentClaimAudit()) != beforeAudit {
		t.Fatal("manual one-time material entered pool or audit")
	}
	if replay := handleManualClaim(values, "callback"); replay.StatusCode != http.StatusConflict {
		t.Fatalf("one-time material replay status=%d", replay.StatusCode)
	}
}

func TestManualClaimCodeZeroRequiresIndependentBillingConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		want    string
		ok      bool
	}{
		{"active target plan", `{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}]}}`, "已到账", true},
		{"not confirmed", `{"code":0,"data":{"plans":[{"plan_id":"other","status":"active"}]}}`, "领取接口成功但到账未确认", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaimManagementForTest(t)
			cacheClaimPreview("idx", claimPreview{Eligible: true, Claimable: true, Plans: []claimPlan{{PlanID: "trial"}}})
			callHostRPC = func(string, any) (json.RawMessage, error) {
				return authGetJSON(t, "idx", authStorage{ZCodeJWTToken: "jwt", DeviceMID: "11111111-2222-4333-8444-555555555555"}), nil
			}
			step := 0
			claimHTTPDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				step++
				if step == 1 {
					return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0}`)}, nil
				}
				if req.Method != http.MethodGet || req.URL != zcodeBillingCurrentURL {
					t.Fatalf("confirmation request=%+v", req)
				}
				return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(tc.current)}, nil
			}
			got := handleManualClaim(officialClaimValues(t, "idx", "trial", "official-one-time-param", ""), "callback")
			if !strings.Contains(string(got.Body), tc.want) {
				t.Fatalf("body=%s", got.Body)
			}
			if tc.ok && got.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", got.StatusCode)
			}
			if !tc.ok && got.StatusCode >= 200 && got.StatusCode < 300 {
				t.Fatalf("unconfirmed claim reported success: %d %s", got.StatusCode, got.Body)
			}
		})
	}
}

func TestClaimBusinessCode3012IsUnusualActivity(t *testing.T) {
	category, message := classifyClaimBusinessCode(3012, "")
	if category != "unusual_activity" || !strings.Contains(message, "异常活动") {
		t.Fatalf("category=%q message=%q", category, message)
	}
}

func TestOfficialClaimPageCopyAndFields(t *testing.T) {
	body := string(renderClaimConfirmation("idx", "trial", "token"))
	for _, want := range []string{"官方验证并领取", "正常浏览器", "官方一次性 verifyParam", `name="verify_param"`, `name="region"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("confirmation page missing %q", want)
		}
	}
}
