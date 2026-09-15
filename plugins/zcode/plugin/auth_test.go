package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQuotaRefreshUsesSeparatedManagementQuery(t *testing.T) {
	req := pluginapi.ManagementRequest{Path: "/v0/resource/plugins/zcode/status", Query: url.Values{"refresh": []string{"quota"}}}
	if !quotaRefreshRequested(req) {
		t.Fatal("quota refresh query was ignored when host separated Path and Query")
	}
	if quotaRefreshRequested(pluginapi.ManagementRequest{Path: "/v0/resource/plugins/zcode/status?refresh=quota"}) {
		t.Fatal("refresh decision must use the authenticated host Query field")
	}
}

func TestHandleAuthParseAcceptsExplicitBigModelCodingKey(t *testing.T) {
	key := "bm-id.secret-value-1234567890"
	raw, _ := json.Marshal("bigmodel-coding:" + key)
	resp := parseAuthResponseForTest(t, "zcode-bigmodel.json", raw)
	if !resp.Handled {
		t.Fatal("BigModel key was not handled")
	}
	var storage authStorage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage.Provider != "bigmodel" || storage.APIKey != key || storage.Source != "coding-plan-key" {
		t.Fatalf("unexpected storage: provider=%q source=%q key_present=%v", storage.Provider, storage.Source, storage.APIKey != "")
	}
}

func TestHandleAuthParseRejectsBigModelJWT(t *testing.T) {
	raw := []byte(`{"type":"zcode","provider":"bigmodel","zcode_jwt_token":"eyJ.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
	resp := parseAuthResponseForTest(t, "zcode-bigmodel.json", raw)
	if resp.Handled {
		t.Fatal("BigModel must not accept a Z.ai JWT")
	}
}

func parseAuthResponseForTest(t *testing.T, fileName string, raw []byte) pluginapi.AuthParseResponse {
	t.Helper()
	req, err := json.Marshal(pluginapi.AuthParseRequest{FileName: fileName, RawJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	out, err := handleAuthParse(req)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("unexpected error envelope: %s", out)
	}
	var resp pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeLoginStartForTest(t *testing.T, raw []byte) pluginapi.AuthLoginStartResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeLoginPollForTest(t *testing.T, raw []byte) pluginapi.AuthLoginPollResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeManagementForTest(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("unexpected error envelope: %s", raw)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestParseOAuthTokenResponseSupportsCurrentZCodeFields(t *testing.T) {
	tokens, err := parseOAuthTokens(json.RawMessage(`{"token":"plan-jwt","zai":{"access_token":"provider-token"},"user":{"user_id":"u-1","email":"user@example.com"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if tokens.JWTToken != "plan-jwt" || tokens.AccessToken != "provider-token" || tokens.UserID != "u-1" || tokens.UserLabel != "user@example.com" {
		t.Fatalf("tokens=%+v", tokens)
	}
}

func TestZCodeStatusReadsHostAccountStateWithoutLeakingCredentials(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	calls := 0
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		calls++
		switch method {
		case pluginabi.MethodHostAuthList:
			return json.Marshal(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{Provider: ProviderZCode, AuthIndex: "zcode-1", Label: "Primary", Status: "active", Success: 3, Failed: 1}}})
		case pluginabi.MethodHostAuthGet:
			return json.Marshal(hostAuthGetResponse{AuthIndex: "zcode-1", JSON: json.RawMessage(`{"zcode_jwt_token":"secret-jwt","captcha_verify_param":"secret-captcha"}`)})
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	}
	page, err := loadZCodeStatus()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(page.Accounts) != 1 || page.Accounts[0].Credential != "Coding Plan JWT · 已附验证码参数" {
		t.Fatalf("page=%+v calls=%d", page, calls)
	}
	body := string(renderZCodeStatusPage(page))
	if strings.Contains(body, "secret-jwt") || strings.Contains(body, "secret-captcha") {
		t.Fatal("status page leaked a credential")
	}
	if !strings.Contains(body, "账号池近期成功") {
		t.Fatal("status page did not distinguish account-pool evidence from model verification")
	}
	cardBody := string(renderZCodeAccountPage(page))
	for _, want := range []string{"ZCode 账号管理", "account-card", "data-filter=\"active\"", "近期有成功请求", "模型级独立检测尚未执行"} {
		if !strings.Contains(cardBody, want) {
			t.Fatalf("card page missing %q", want)
		}
	}
	if strings.Contains(cardBody, "secret-jwt") || strings.Contains(cardBody, "secret-captcha") {
		t.Fatal("card page leaked a credential")
	}
}

func TestQuotaRefreshOnlyUsesCodingPlanJWTAndDoesNotLeakSecrets(t *testing.T) {
	oldCall, oldHTTP := callHostRPC, quotaHTTPDo
	defer func() { callHostRPC, quotaHTTPDo = oldCall, oldHTTP }()
	zcodeQuotas.Lock()
	zcodeQuotas.items = make(map[string]zcodeQuotaSnapshot)
	zcodeQuotas.Unlock()
	calls := 0
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthGet {
			t.Fatalf("unexpected host method %q", method)
		}
		return json.Marshal(hostAuthGetResponse{AuthIndex: "jwt", JSON: json.RawMessage(`{"zcode_jwt_token":"secret-jwt","captcha_verify_param":"secret-captcha"}`)})
	}
	quotaHTTPDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if req.Headers.Get("Authorization") != "Bearer secret-jwt" || req.Headers.Get("X-Aliyun-Captcha-Verify-Param") != "secret-captcha" {
			t.Fatal("billing request did not use Coding Plan credentials")
		}
		body := []byte(`{"data":{"plans":[{"name":"Coding Plan","status":"active"}],"balances":[{"total_units":100,"used_units":20,"remaining_units":80}]}}`)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
	}
	quota := refreshZCodeQuota("jwt")
	if calls != 2 || quota.Remaining == nil || *quota.Remaining != 80 || quota.Plan != "Coding Plan" {
		t.Fatalf("quota=%+v calls=%d", quota, calls)
	}
	zcodeQuotas.Lock()
	zcodeQuotas.items["jwt"] = quota
	zcodeQuotas.Unlock()
	page := zcodeStatusPage{Accounts: []zcodeStatusAccount{{AuthIndex: "jwt", Name: "Primary", Credential: "Coding Plan JWT"}}}
	body := string(renderZCodeAccountPage(page))
	if !strings.Contains(body, "剩余 80 / 总额 100") || strings.Contains(body, "secret-jwt") || strings.Contains(body, "secret-captcha") {
		t.Fatal("quota card did not render safely")
	}

	calls = 0
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		return json.Marshal(hostAuthGetResponse{AuthIndex: "key", JSON: json.RawMessage(`{"api_key":"secret-key"}`)})
	}
	keyQuota := refreshZCodeQuota("key")
	if calls != 0 || keyQuota.Error != "仅 Coding Plan JWT 支持额度刷新" {
		t.Fatalf("API Key should not call billing endpoint: %+v", keyQuota)
	}
}

func TestAuthLoginStartUsesServerMediatedCallback(t *testing.T) {
	raw, _ := json.Marshal(rpcAuthLoginStartRequest{AuthLoginStartRequest: pluginapi.AuthLoginStartRequest{Provider: ProviderZCode}})
	out, err := handleAuthLoginStart(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeLoginStartForTest(t, out)
	u, err := url.Parse(resp.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("redirect_uri"); got != "https://zcode.z.ai/api/v1/oauth/cli/callback/zai" {
		t.Fatalf("redirect_uri=%q", got)
	}
	if got := u.Query().Get("state"); got == "" {
		t.Fatal("server authorize URL is missing its OAuth state")
	}
	if u.Query().Get("state") == resp.State {
		t.Fatal("server OAuth state must remain separate from the plugin polling state")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(resp.State) {
		t.Fatalf("state must be 256-bit lowercase hex, got %q", resp.State)
	}
	removeOAuthCallback(resp.State)
}

func TestOAuthCallbackPOSTAcceptsOnce(t *testing.T) {
	state := strings.Repeat("a", 64)
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	form := url.Values{"state": []string{state}, "code": []string{"code-1"}}
	req := pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    "/v0/resource/plugins/zcode/callback",
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}, "Origin": []string{"https://chat.z.ai"}},
		Body:    []byte(form.Encode()),
	}
	raw, _ := json.Marshal(req)
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeManagementForTest(t, out)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"ok":true`) {
		t.Fatalf("first callback status=%d body=%s", resp.StatusCode, resp.Body)
	}

	out, err = handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp = decodeManagementForTest(t, out)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("replayed callback status=%d", resp.StatusCode)
	}
}

func TestOAuthCallbackPOSTTunnelAcceptsUnknownState(t *testing.T) {
	state := strings.Repeat("8", 64)
	raw, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   oauthCallbackPath,
		Headers: http.Header{
			"Content-Type":          []string{"application/x-www-form-urlencoded"},
			"Origin":                []string{oauthBridgeOrigin},
			"X-Zcode-Bridge-Method": []string{http.MethodPost},
			"X-Zcode-State":         []string{state},
			"X-Zcode-Code":          []string{"code-1"},
		},
	})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeManagementForTest(t, out).StatusCode; got != http.StatusGone {
		t.Fatalf("status=%d", got)
	}
}

func TestOAuthCallbackRejectsUntrustedOrigin(t *testing.T) {
	state := strings.Repeat("b", 64)
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	form := url.Values{"state": []string{state}, "code": []string{"code-1"}}
	raw, _ := json.Marshal(pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    "/v0/resource/plugins/zcode/callback",
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}, "Origin": []string{"https://evil.example"}},
		Body:    []byte(form.Encode()),
	})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeManagementForTest(t, out).StatusCode; got != http.StatusForbidden {
		t.Fatalf("status=%d", got)
	}
}

func TestOAuthCallbackManualFallbackValidatesCustomScheme(t *testing.T) {
	state := strings.Repeat("c", 64)
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	callback := "https://evil.example/callback?state=" + state + "&code=code-1"
	form := url.Values{"callback": []string{callback}}
	raw, _ := json.Marshal(pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    "/v0/resource/plugins/zcode/callback",
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}, "Origin": []string{"https://cpa.example.com"}},
		Body:    []byte(form.Encode()),
	})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeManagementForTest(t, out).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("status=%d", got)
	}
}

func TestOAuthCallbackGETDoesNotConsumeQueryCode(t *testing.T) {
	state := strings.Repeat("d", 64)
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	raw, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/zcode/callback",
		Query:  url.Values{"state": []string{state}, "code": []string{"leaked-in-url"}},
	})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeManagementForTest(t, out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	oauthCallbacks.Lock()
	stored := oauthCallbacks.items[state]
	oauthCallbacks.Unlock()
	if stored.Code != "" {
		t.Fatalf("GET query code was consumed: %q", stored.Code)
	}
}

func TestOAuthCallbackRejectsReplayAfterPollClaim(t *testing.T) {
	state := strings.Repeat("9", 64)
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{Code: "auth-code", ExpiresAt: time.Now().Add(time.Minute), Processing: true}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	form := url.Values{"state": []string{state}, "code": []string{"replacement"}}
	raw, _ := json.Marshal(pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    oauthCallbackPath,
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}, "Origin": []string{oauthBridgeOrigin}},
		Body:    []byte(form.Encode()),
	})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeManagementForTest(t, out).StatusCode; got != http.StatusGone {
		t.Fatalf("status=%d", got)
	}
}

func TestAuthLoginPollUnknownStateTerminates(t *testing.T) {
	raw, _ := json.Marshal(rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{Provider: ProviderZCode, State: strings.Repeat("a", 64)}})
	out, err := handleAuthLoginPoll(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeLoginPollForTest(t, out)
	if resp.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status=%q", resp.Status)
	}
}

func TestAuthLoginPollExchangesCodeAndFallsBackToJWT(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	state := "oauth-state-1234567890"
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{Code: "auth-code", ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	callCount := 0
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		callCount++
		requestRaw, _ := json.Marshal(payload)
		var req rpcHostHTTPRequest
		_ = json.Unmarshal(requestRaw, &req)
		if req.HostCallbackID != "host-callback" {
			t.Fatalf("host_callback_id=%q", req.HostCallbackID)
		}
		if callCount == 1 {
			if req.Request.URL != ZCodeTokenURL {
				t.Fatalf("token URL=%q", req.Request.URL)
			}
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"token":"eyJheader.eyJ1c2VyX2lkIjoidTEifQ.signature","zai":{"access_token":"oauth-access"},"user":{"user_id":"u1","name":"Alice"}}}`)})
		}
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusBadGateway, Body: []byte(`{"code":500,"msg":"business unavailable"}`)})
	}

	raw, _ := json.Marshal(rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{Provider: ProviderZCode, State: state}, HostCallbackID: "host-callback"})
	out, err := handleAuthLoginPoll(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeLoginPollForTest(t, out)
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status=%q message=%q", resp.Status, resp.Message)
	}
	var storage authStorage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage.ZCodeJWTToken == "" || storage.AccessToken != "" || storage.APIKey != "" || storage.Source != "jwt" {
		t.Fatalf("unexpected fallback storage: %#v", storage)
	}
}

func TestAuthLoginPollDefaultsToJWTWithoutKeyExchange(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	state := "oauth-state-jwt-default-1234"
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{Provider: "zai", Code: "auth-code", ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	calls := 0
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		calls++
		if calls != 1 {
			t.Fatal("Z.AI default OAuth must not exchange or create an API Key")
		}
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"token":"eyJheader.eyJ1c2VyX2lkIjoidTEifQ.signature","zai":{"access_token":"oauth-access"},"user":{"user_id":"u1"}}}`)})
	}

	raw, _ := json.Marshal(rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{Provider: ProviderZCode, State: state}, HostCallbackID: "host-callback"})
	out, err := handleAuthLoginPoll(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeLoginPollForTest(t, out)
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status=%q message=%q", resp.Status, resp.Message)
	}
	var storage authStorage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage.ZCodeJWTToken == "" || storage.APIKey != "" || storage.Source != "jwt" || storage.AccessToken != "" {
		t.Fatalf("unexpected default OAuth storage: %#v", storage)
	}
}

func TestOAuthCallbackOPTIONSAllowsOnlyBridgeOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin string
		status int
	}{
		{origin: "https://chat.z.ai", status: http.StatusNoContent},
		{origin: "https://evil.example", status: http.StatusForbidden},
		{origin: "", status: http.StatusForbidden},
	} {
		raw, _ := json.Marshal(pluginapi.ManagementRequest{
			Method:  http.MethodOptions,
			Path:    oauthCallbackPath,
			Headers: http.Header{"Origin": []string{tc.origin}},
		})
		out, err := handleManagement(raw)
		if err != nil {
			t.Fatal(err)
		}
		resp := decodeManagementForTest(t, out)
		if resp.StatusCode != tc.status {
			t.Fatalf("origin=%q status=%d", tc.origin, resp.StatusCode)
		}
		if tc.status == http.StatusNoContent {
			if got := resp.Headers.Get("Access-Control-Allow-Origin"); got != oauthBridgeOrigin {
				t.Fatalf("allow-origin=%q", got)
			}
			if got := resp.Headers.Get("Access-Control-Allow-Credentials"); got != "" {
				t.Fatalf("allow-credentials=%q", got)
			}
		}
	}
}

func TestOAuthCallbackRejectsOversizedBody(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    oauthCallbackPath,
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}, "Origin": []string{oauthBridgeOrigin}},
		Body:    []byte(strings.Repeat("x", oauthCallbackMaxBody+1)),
	})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeManagementForTest(t, out).StatusCode; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", got)
	}
}

func TestOAuthCallbackManualFallbackAcceptsOfficialCustomScheme(t *testing.T) {
	state := strings.Repeat("e", 64)
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	callback := "zcode://zai-auth/callback?state=" + state + "&code=code-1"
	form := url.Values{"callback": []string{callback}}
	raw, _ := json.Marshal(pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    oauthCallbackPath,
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}, "Origin": []string{oauthFirstPartyOrigin}},
		Body:    []byte(form.Encode()),
	})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeManagementForTest(t, out).StatusCode; got != http.StatusOK {
		t.Fatalf("status=%d", got)
	}
}

func TestOAuthCallbackRejectsInvalidStateAndExtraFields(t *testing.T) {
	for _, form := range []url.Values{
		{"state": []string{strings.Repeat("G", 64)}, "code": []string{"code-1"}},
		{"state": []string{strings.Repeat("f", 64)}, "code": []string{"code-1"}, "extra": []string{"1"}},
		{"state": []string{strings.Repeat("f", 64)}, "code": []string{"one", "two"}},
	} {
		raw, _ := json.Marshal(pluginapi.ManagementRequest{
			Method:  http.MethodPost,
			Path:    oauthCallbackPath,
			Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}, "Origin": []string{oauthBridgeOrigin}},
			Body:    []byte(form.Encode()),
		})
		out, err := handleManagement(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeManagementForTest(t, out).StatusCode; got != http.StatusBadRequest {
			t.Fatalf("form=%v status=%d", form, got)
		}
	}
}

func TestExchangeOAuthCodeUsesOfficialRedirectURI(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		raw, _ := json.Marshal(payload)
		var req rpcHostHTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		var body map[string]string
		if err := json.Unmarshal(req.Request.Body, &body); err != nil {
			t.Fatal(err)
		}
		if got := body["redirect_uri"]; got != "zcode://zai-auth/callback" {
			t.Fatalf("redirect_uri=%q", got)
		}
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"token":"jwt","zai":{"access_token":"access"}}}`)})
	}
	if _, err := exchangeOAuthCode("host-callback", "auth-code", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestFindAPIKeyIDHandlesCreateResponse(t *testing.T) {
	if got := findAPIKeyID(json.RawMessage(`{"name":"zcode-api-key","apiKey":"key-id"}`), zcodeAPIKeyName); got != "key-id" {
		t.Fatalf("api key id=%q", got)
	}
}

func TestHandleAuthParseAcceptsZCodeAPIKey(t *testing.T) {
	raw := []byte(`{"type":"zcode","api_key":"key-id.secret-value-1234567890"}`)
	resp := parseAuthResponseForTest(t, "zcode-api-key.json", raw)
	if !resp.Handled {
		t.Fatal("ZCode API key was not handled")
	}
	if resp.Auth.Provider != ProviderZCode {
		t.Fatalf("provider=%q", resp.Auth.Provider)
	}
	if resp.Auth.ID != "" {
		t.Fatalf("ID=%q, want empty so host derives stable file identity", resp.Auth.ID)
	}
	if resp.Auth.FileName != "zcode-api-key.json" {
		t.Fatalf("filename=%q", resp.Auth.FileName)
	}
	var storage map[string]any
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage["api_key"] != "key-id.secret-value-1234567890" {
		t.Fatalf("storage=%v", storage)
	}
}

func TestHandleAuthParseUnquotesJSONStringJWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEyMzQ1Njc4OTAifQ.signature-value-1234567890"
	raw, err := json.Marshal(jwt)
	if err != nil {
		t.Fatal(err)
	}
	resp := parseAuthResponseForTest(t, "zcode-jwt.json", raw)
	if !resp.Handled {
		t.Fatal("JSON string JWT was not handled")
	}
	var storage map[string]any
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage["zcode_jwt_token"] != jwt {
		t.Fatalf("stored token=%q, want unquoted JWT", storage["zcode_jwt_token"])
	}
}

func TestHandleAuthParseDoesNotClaimOtherProvider(t *testing.T) {
	raw := []byte(`{"type":"workbuddy","api_key":"key-id.secret-value-1234567890"}`)
	resp := parseAuthResponseForTest(t, "workbuddy.json", raw)
	if resp.Handled {
		t.Fatal("claimed another provider's credential")
	}
}

func TestHandleAuthParseDoesNotClaimUnroutedTypelessSecret(t *testing.T) {
	resp := parseAuthResponseForTest(t, "other.json", []byte(`"key-id.secret-value-1234567890"`))
	if resp.Handled {
		t.Fatal("claimed an untyped credential not routed or named for zcode")
	}
}

func TestRoutePanelSaveNoAdminKeyNoConfirmTokenDirectSave(t *testing.T) {
	body := string(renderZCodeAccountPage(zcodeStatusPage{}))
	for _, want := range []string{"save_config", "保存配置"} {
		if !strings.Contains(body, want) {
			t.Fatalf("route panel missing %q", want)
		}
	}
	for _, mustNot := range []string{"management-key", "config-confirm", "confirmation_token", "确认、持久化并等待生效", "PluginReconfigure", "config_control=confirm"} {
		if strings.Contains(body, mustNot) {
			t.Fatalf("route panel must not contain %q", mustNot)
		}
	}
	if strings.Contains(body, "localStorage.setItem") || strings.Contains(body, "sessionStorage.setItem") {
		t.Fatal("management key must not be persisted")
	}
}

func TestParseBigModelQuotaNoPlanIsAccountStateNotRefreshFailure(t *testing.T) {
	got := parseBigModelQuotaSnapshot([]byte(`{"success":false,"code":500,"msg":"当前用户不存在coding plan"}`), nil)
	if got.Error != "" || got.Plan != "BigModel · 未开通 Coding Plan" {
		t.Fatalf("unexpected snapshot: %#v", got)
	}
}

func TestParseBigModelBalanceSnapshotReadsAvailableBalance(t *testing.T) {
	got := parseBigModelBalanceSnapshot([]byte(`{"code":200,"msg":"操作成功","data":{"balance":9.699698360,"rechargeAmount":10.000000,"giveAmount":0.000000,"totalSpendAmount":0.300301640,"availableBalance":9.699698360,"creditStatus":"NOT_OPEN"},"success":true}`))
	if got.Error != "" {
		t.Fatalf("unexpected error: %q", got.Error)
	}
	if got.Remaining == nil || *got.Remaining < 9.6996 || *got.Remaining > 9.6997 {
		t.Fatalf("unexpected remaining: %#v", got.Remaining)
	}
	if !got.Money || got.Plan != "BigModel 国内余额" {
		t.Fatalf("snapshot=%+v", got)
	}
	if got.Total == nil || *got.Total != 10 {
		t.Fatalf("unexpected total: %#v", got.Total)
	}
}

func TestRefreshBigModelQuotaCombinesQuotaAndBalance(t *testing.T) {
	oldCall, oldHTTP := callHostRPC, quotaHTTPDo
	defer func() { callHostRPC, quotaHTTPDo = oldCall, oldHTTP }()
	zcodeQuotas.Lock()
	zcodeQuotas.items = make(map[string]zcodeQuotaSnapshot)
	zcodeQuotas.Unlock()
	calls := 0
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		return json.Marshal(hostAuthGetResponse{AuthIndex: "bigmodel", JSON: json.RawMessage(`{"api_key":"secret-bm","provider":"bigmodel"}`)})
	}
	quotaHTTPDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if req.Headers.Get("Authorization") != "Bearer secret-bm" {
			t.Fatal("balance request did not use BigModel API key")
		}
		switch req.URL {
		case bigModelQuotaURL:
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"success":false,"code":500,"msg":"当前用户不存在coding plan"}`)}, nil
		case bigModelBalanceURL:
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":200,"msg":"操作成功","data":{"balance":9.699698360,"rechargeAmount":10.000000,"totalSpendAmount":0.300301640,"availableBalance":9.699698360},"success":true}`)}, nil
		}
		t.Fatalf("unexpected URL %q", req.URL)
		return pluginapi.HTTPResponse{}, nil
	}
	quota := refreshZCodeQuota("bigmodel")
	if calls != 2 || quota.Error != "" {
		t.Fatalf("quota=%+v calls=%d", quota, calls)
	}
	if !quota.Money || quota.Remaining == nil || *quota.Remaining < 9.6996 || *quota.Remaining > 9.6997 {
		t.Fatalf("quota=%+v", quota)
	}
	if quota.Plan != "BigModel 国内余额" {
		t.Fatalf("plan=%q", quota.Plan)
	}
	zcodeQuotas.Lock()
	zcodeQuotas.items["bigmodel"] = quota
	zcodeQuotas.Unlock()
	page := zcodeStatusPage{Accounts: []zcodeStatusAccount{{AuthIndex: "bigmodel", Name: "BigModel", Credential: "BigModel 国内 · 余额 API Key"}}}
	body := string(renderZCodeAccountPage(page))
	if !strings.Contains(body, "可用余额 ¥9.70") {
		t.Fatal("balance card did not render money format")
	}
}

func TestParseBigModelQuotaUsesFiveHourAndReportsWeekly(t *testing.T) {
	got := parseBigModelQuotaSnapshot([]byte(`{"success":true,"data":{"level":"pro","limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":12000,"currentValue":3000,"remaining":9000,"percentage":25},{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":60000,"currentValue":12000,"remaining":48000,"percentage":20}]}}`), nil)
	if got.Error != "" || got.Total == nil || *got.Total != 12000 || got.Used == nil || *got.Used != 3000 || got.Remaining == nil || *got.Remaining != 9000 {
		t.Fatalf("unexpected quota: %#v", got)
	}
	if !strings.Contains(got.Plan, "PRO") || !strings.Contains(got.Plan, "周额度已用 20%") {
		t.Fatalf("plan=%q", got.Plan)
	}
}

func TestAccountCardsOfferAuthenticatedNonPersistentStatusToggle(t *testing.T) {
	body := string(renderZCodeAccountPage(zcodeStatusPage{Active: 1, Accounts: []zcodeStatusAccount{{AuthIndex: "idx-1", FileName: "one.json", Name: "One", State: "active"}}}))
	for _, want := range []string{"account-toggle", "one.json", "idx-1", "/v0/management/auth-files/status", "Authorization", "至少保留一个可路由账号"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(body, "localStorage.setItem") || strings.Contains(body, "sessionStorage.setItem") {
		t.Fatal("management key must not be persisted")
	}
}
