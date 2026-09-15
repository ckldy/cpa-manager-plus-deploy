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

func TestTraeWorkRegistrationAndUnsupportedTokens(t *testing.T) {
	reg := pluginRegistration()
	if reg.Metadata.Name != ProviderTraeWork || !reg.Capabilities.ModelProvider || !reg.Capabilities.AuthProvider || !reg.Capabilities.FrontendAuthProvider || !reg.Capabilities.Executor || !reg.Capabilities.ManagementAPI {
		t.Fatalf("registration=%+v", reg)
	}
	raw, err := handleExecutorCountTokens(nil)
	if err != nil || !json.Valid(raw) || !strings.Contains(string(raw), "unsupported") {
		t.Fatalf("count_tokens=%s err=%v", raw, err)
	}
}

func TestLoginStartPollReturnsAuthData(t *testing.T) {
	old := hostHTTPCall
	defer func() { hostHTTPCall = old }()
	hostHTTPCall = func(_ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch req.URL {
		case traeAPIHost + traeExchangePath:
			return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{}, Body: []byte(`{"Result":{"Token":"access","RefreshToken":"rotated","TokenExpireAt":1893456000000}}`)}, nil
		case traeAPIHost + traeUserInfoPath:
			return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{}, Body: []byte(`{"Result":{"UserID":"u1","ScreenName":"Ada","EnterpriseID":"e1"}}`)}, nil
		case traeAgentHost + traeModelsPath:
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM"}}]}`)}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return pluginapi.HTTPResponse{}, nil
		}
	}
	startedRaw, err := handleAuthLoginStart(nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result pluginapi.AuthLoginStartResponse `json:"result"`
	}
	if err = json.Unmarshal(startedRaw, &env); err != nil {
		t.Fatal(err)
	}
	poll, _ := json.Marshal(rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{Provider: ProviderTraeWork, State: env.Result.State, Metadata: map[string]any{"callback_url": `http://127.0.0.1:18080/authorize?refreshToken=refresh`}}, HostCallbackID: "cb"})
	resultRaw, err := handleAuthLoginPoll(poll)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Result pluginapi.AuthLoginPollResponse `json:"result"`
	}
	_ = json.Unmarshal(resultRaw, &got)
	if got.Result.Status != pluginapi.AuthLoginStatusSuccess || got.Result.Auth.Provider != ProviderTraeWork || len(got.Result.Auth.StorageJSON) == 0 {
		t.Fatalf("poll=%+v", got.Result)
	}
	var loginStorage struct {
		Cache *persistedTraeModels `json:"traework_models_cache"`
	}
	if err := json.Unmarshal(got.Result.Auth.StorageJSON, &loginStorage); err != nil {
		t.Fatal(err)
	}
	if loginStorage.Cache == nil || len(loginStorage.Cache.Models) != 1 || loginStorage.Cache.Models[0].ID != "traework-GLM" {
		t.Fatalf("login storage did not carry model cache: %s", got.Result.Auth.StorageJSON)
	}
}

func TestModelsUsePrefixAndFallback(t *testing.T) {
	models := staticTraeWorkModels(time.Unix(1, 0))
	if len(models) == 0 {
		t.Fatal("empty fallback")
	}
	for _, m := range models {
		if !strings.HasPrefix(m.ID, "traework-") {
			t.Fatalf("model=%q", m.ID)
		}
	}
}

func TestPublicModelRefreshBranchAndCSRF(t *testing.T) {
	oldCall, oldHTTP := callHostRPC, hostHTTPCall
	defer func() { callHostRPC = oldCall; hostHTTPCall = oldHTTP }()
	callHostRPC = fakeHostAuthCall
	hostHTTPCall = func(_ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.URL != traeAgentHost+traeModelsPath {
			t.Fatalf("url=%s", req.URL)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM"}}]}`)}, nil
	}
	body := url.Values{"csrf": {initCSRF()}, "action": {"models"}, "auth_index": {"a1"}}.Encode()
	raw, _ := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/resource/plugins/traework/panel", Headers: http.Header{"X-Requested-With": {"traework-panel"}, "Content-Type": {"application/x-www-form-urlencoded"}}, Body: []byte(body)}, HostCallbackID: "cb"})
	out, err := handleManagement(raw)
	resp := decodeManagement(t, out)
	if err != nil || !strings.Contains(string(resp.Body), "traework-GLM") {
		t.Fatalf("out=%s err=%v", resp.Body, err)
	}
	raw, _ = json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/resource/plugins/traework/panel", Headers: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, Body: []byte("action=models&auth_index=a1")}})
	out, _ = handleManagement(raw)
	resp = decodeManagement(t, out)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no csrf status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestPanelUsesPublicPasteLoginAndHasCompleteActions(t *testing.T) {
	old := callHostRPC
	defer func() { callHostRPC = old }()
	callHostRPC = fakeHostAuthCall
	raw, _ := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/traework/panel"}})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeManagement(t, out)
	page := string(resp.Body)
	for _, want := range []string{"fetch(location.pathname", "action:'start'", "action:'finish'", "data-action=\"enabled", "data-action=\"credits", "data-action=\"checkin", "data-action=\"models", "删除（宿主不支持）", "自动签到", "csrf=", "await r.text()", "JSON.parse(t)"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, forbidden := range []string{"/v0/management/plugins/traework/login/start", "/v0/management/plugins/traework/login/poll", "Authorization", "management key", "管理员密钥", "managementPost", "base=", "b={error:await r.text()}", "await r.json()"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("public login page contains forbidden management auth dependency %q", forbidden)
		}
	}
	for _, secret := range []string{"secret-access", "secret-refresh"} {
		if strings.Contains(page, secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
}

func TestPublicLoginStartNeedsNoAuthorization(t *testing.T) {
	resp := publicLoginPost(t, url.Values{"action": {"start"}}, true, "public-callback")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var got struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.URL, "https://www.trae.cn/authorization?") || got.State == "" {
		t.Fatalf("start=%+v", got)
	}
	if strings.Contains(string(resp.Body), "Authorization") || strings.Contains(string(resp.Body), "management") {
		t.Fatalf("unexpected auth dependency in response: %s", resp.Body)
	}
}

func TestPublicLoginFinishWithoutStateSavesOnceAndReturnsNoSecrets(t *testing.T) {
	oldCall, oldHTTP := callHostRPC, hostHTTPCall
	defer func() { callHostRPC = oldCall; hostHTTPCall = oldHTTP }()

	saves := 0
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthSave:
			saves++
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var req pluginapi.HostAuthSaveRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatal(err)
			}
			if req.Name != "traework-u-public.json" || !strings.Contains(string(req.JSON), `"provider":"traework"`) || !strings.Contains(string(req.JSON), `"access_token":"secret-access"`) {
				t.Fatalf("save request=%s", raw)
			}
			var physical struct {
				Storage struct {
					Cache *persistedTraeModels `json:"traework_models_cache"`
				} `json:"storage"`
			}
			if err := json.Unmarshal(req.JSON, &physical); err != nil {
				t.Fatal(err)
			}
			if physical.Storage.Cache == nil || len(physical.Storage.Cache.Models) != 1 {
				t.Fatalf("initial save did not include model cache: %s", req.JSON)
			}
			return json.RawMessage(`{"name":"traework-u-public.json"}`), nil
		case pluginabi.MethodHostAuthList:
			return json.RawMessage(`{"files":[{"auth_index":"a-public","name":"traework-u-public.json","provider":"traework"}]}`), nil
		case pluginabi.MethodHostAuthGet:
			return json.RawMessage(`{"auth_index":"a-public","json":{"type":"traework","provider":"traework","storage":{"provider":"traework","access_token":"secret-access","refresh_token":"secret-refresh"}}}`), nil
		default:
			return nil, nil
		}
	}
	hostHTTPCall = func(callback string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if callback != "public-callback" {
			t.Fatalf("host callback=%q", callback)
		}
		switch req.URL {
		case traeAPIHost + traeExchangePath:
			return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{}, Body: []byte(`{"Result":{"Token":"secret-access","RefreshToken":"secret-rotated","TokenExpireAt":1893456000000}}`)}, nil
		case traeAPIHost + traeUserInfoPath:
			return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{}, Body: []byte(`{"Result":{"UserID":"u-public","ScreenName":"Public User","EnterpriseID":"e-public"}}`)}, nil
		case traeAgentHost + traeModelsPath:
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM"}}]}`)}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return pluginapi.HTTPResponse{}, nil
		}
	}

	callback := `http://127.0.0.1:18080/authorize?refreshToken=secret-refresh&sessionId=s1&host=https%3A%2F%2Fapi.trae.com.cn`
	respa := publicLoginPost(t, url.Values{"action": {"finish"}, "callback_url": {callback}}, true, "public-callback")
	if respa.StatusCode != http.StatusOK || saves != 1 {
		t.Fatalf("first submit status=%d saves=%d body=%s", respa.StatusCode, saves, respa.Body)
	}
	for _, secret := range []string{"secret-refresh", "secret-access", "secret-rotated", callback} {
		if strings.Contains(string(respa.Body), secret) {
			t.Fatalf("response leaked secret %q: %s", secret, respa.Body)
		}
	}

	resp := publicLoginPost(t, url.Values{"action": {"finish"}, "callback_url": {callback}}, true, "public-callback")
	if resp.StatusCode != http.StatusConflict || saves != 1 {
		t.Fatalf("duplicate status=%d saves=%d body=%s", resp.StatusCode, saves, resp.Body)
	}
}

func TestPublicLoginFinishRejectsUnsafeRequests(t *testing.T) {
	oldHTTP := hostHTTPCall
	defer func() { hostHTTPCall = oldHTTP }()
	hostHTTPCall = func(_ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Fatal("unsafe callback reached upstream")
		return pluginapi.HTTPResponse{}, nil
	}
	good := `http://127.0.0.1:18080/authorize?refreshToken=r`
	tests := []struct {
		name        string
		values      url.Values
		requested   bool
		contentType string
		want        int
	}{
		{name: "missing csrf", values: url.Values{"action": {"finish"}, "csrf": {""}, "callback_url": {good}}, requested: true, contentType: "application/x-www-form-urlencoded", want: http.StatusForbidden},
		{name: "missing requested with", values: url.Values{"action": {"finish"}, "callback_url": {good}}, contentType: "application/x-www-form-urlencoded", want: http.StatusForbidden},
		{name: "wrong content type", values: url.Values{"action": {"finish"}, "callback_url": {good}}, requested: true, contentType: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "duplicate form callback", values: url.Values{"action": {"finish"}, "callback_url": {good, good}}, requested: true, contentType: "application/x-www-form-urlencoded", want: http.StatusBadRequest},
		{name: "foreign origin", values: url.Values{"action": {"finish"}, "callback_url": {`https://evil.example/authorize?refreshToken=r`}}, requested: true, contentType: "application/x-www-form-urlencoded", want: http.StatusBadRequest},
		{name: "wrong path", values: url.Values{"action": {"finish"}, "callback_url": {`http://127.0.0.1:18080/other?refreshToken=r`}}, requested: true, contentType: "application/x-www-form-urlencoded", want: http.StatusBadRequest},
		{name: "duplicate callback parameter", values: url.Values{"action": {"finish"}, "callback_url": {`http://127.0.0.1:18080/authorize?refreshToken=r&refreshToken=s`}}, requested: true, contentType: "application/x-www-form-urlencoded", want: http.StatusBadRequest},
		{name: "forged api host", values: url.Values{"action": {"finish"}, "callback_url": {`http://127.0.0.1:18080/authorize?refreshToken=r&host=https%3A%2F%2Fevil.example`}}, requested: true, contentType: "application/x-www-form-urlencoded", want: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name != "missing csrf" {
				tc.values.Set("csrf", initCSRF())
			}
			resp := publicLoginPostWithContentType(t, tc.values, tc.requested, "public-callback", tc.contentType)
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.want, resp.Body)
			}
		})
	}

	oversized := url.Values{"action": {"finish"}, "csrf": {initCSRF()}, "callback_url": {strings.Repeat("x", (64<<10)+1)}}
	resp := publicLoginPost(t, oversized, true, "public-callback")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestPublicLoginFailuresAreRedacted(t *testing.T) {
	oldCall, oldHTTP := callHostRPC, hostHTTPCall
	defer func() { callHostRPC = oldCall; hostHTTPCall = oldHTTP }()
	callback := `http://127.0.0.1:18080/authorize?refreshToken=never-echo-this`
	hostHTTPCall = func(_ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"secret":"upstream-secret"}`)}, nil
	}
	resp := publicLoginPost(t, url.Values{"action": {"finish"}, "callback_url": {callback}}, true, "public-callback")
	if resp.StatusCode != http.StatusBadGateway || string(resp.Body) != `{"error":"login failed"}` {
		t.Fatalf("upstream failure status=%d body=%s", resp.StatusCode, resp.Body)
	}

	hostHTTPCall = func(_ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch req.URL {
		case traeAPIHost + traeExchangePath:
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"Result":{"Token":"save-secret","RefreshToken":"save-refresh"}}`)}, nil
		case traeAPIHost + traeUserInfoPath:
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"Result":{"UserID":"u-save","ScreenName":"Save"}}`)}, nil
		default:
			return pluginapi.HTTPResponse{}, nil
		}
	}
	callHostRPC = func(method string, _ any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthSave {
			return nil, errors.New("storage backend secret detail")
		}
		return nil, nil
	}
	callback = `http://127.0.0.1:18080/authorize?refreshToken=save-callback-secret`
	resp = publicLoginPost(t, url.Values{"action": {"finish"}, "callback_url": {callback}}, true, "public-callback")
	if resp.StatusCode != http.StatusServiceUnavailable || string(resp.Body) != `{"error":"host auth save failed"}` {
		t.Fatalf("save failure status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestPublicActionsNeedNoManagementKey(t *testing.T) {
	oldCall, oldHTTP := callHostRPC, hostHTTPCall
	defer func() { callHostRPC = oldCall; hostHTTPCall = oldHTTP }()
	callHostRPC = fakeHostAuthCall
	hostHTTPCall = func(_ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch req.URL {
		case traeUGHost + traeCheckinStatusPath:
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"checked_in":true,"credits":4700,"enable":true}`)}, nil
		case traeUGHost + traeCreditsPath:
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":4700}}}]}`)}, nil
		case traeAgentHost + traeModelsPath:
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM"}}]}`)}, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return pluginapi.HTTPResponse{}, nil
		}
	}

	// accounts — no auth_index needed
	resp := publicLoginPost(t, url.Values{"action": {"accounts"}}, true, "action-cb")
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "traework-u1.json") {
		t.Fatalf("accounts status=%d body=%s", resp.StatusCode, resp.Body)
	}

	// credits
	resp = publicLoginPost(t, url.Values{"action": {"credits"}, "auth_index": {"a1"}}, true, "action-cb")
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "4700") {
		t.Fatalf("credits status=%d body=%s", resp.StatusCode, resp.Body)
	}

	// checkin
	resp = publicLoginPost(t, url.Values{"action": {"checkin"}, "auth_index": {"a1"}}, true, "action-cb")
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "checked_in") {
		t.Fatalf("checkin status=%d body=%s", resp.StatusCode, resp.Body)
	}

	// models refresh
	resp = publicLoginPost(t, url.Values{"action": {"models"}, "auth_index": {"a1"}}, true, "action-cb")
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "traework-GLM") {
		t.Fatalf("models status=%d body=%s", resp.StatusCode, resp.Body)
	}

	// enabled (toggle)
	resp = publicLoginPost(t, url.Values{"action": {"enabled"}, "auth_index": {"a1"}, "name": {"traework-u1.json"}, "enabled": {"false"}}, true, "action-cb")
	if resp.StatusCode != 200 {
		t.Fatalf("enabled status=%d body=%s", resp.StatusCode, resp.Body)
	}

	// delete (always 501 unsupported from host)
	resp = publicLoginPost(t, url.Values{"action": {"delete"}, "auth_index": {"a1"}, "confirm": {"DELETE"}}, true, "action-cb")
	if resp.StatusCode != 501 {
		t.Fatalf("delete status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestPublicActionsRejectMissingCSRF(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	callHostRPC = fakeHostAuthCall

	// missing CSRF field → form validation rejects with 400
	raw, _ := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/resource/plugins/traework/panel", Headers: http.Header{"X-Requested-With": {"traework-panel"}, "Content-Type": {"application/x-www-form-urlencoded"}}, Body: []byte("action=checkin&auth_index=a1")}, HostCallbackID: "cb"})
	out, _ := handleManagement(raw)
	resp := decodeManagement(t, out)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing csrf field status=%d body=%s", resp.StatusCode, resp.Body)
	}

	// wrong CSRF value → 403 CSRF rejected
	body := url.Values{"action": {"checkin"}, "auth_index": {"a1"}, "csrf": {"wrong-csrf-value"}}.Encode()
	raw, _ = json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/resource/plugins/traework/panel", Headers: http.Header{"X-Requested-With": {"traework-panel"}, "Content-Type": {"application/x-www-form-urlencoded"}}, Body: []byte(body)}, HostCallbackID: "cb"})
	out, _ = handleManagement(raw)
	resp = decodeManagement(t, out)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong csrf status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func publicLoginPost(t *testing.T, values url.Values, requested bool, callbackID string) pluginapi.ManagementResponse {
	t.Helper()
	return publicLoginPostWithContentType(t, values, requested, callbackID, "application/x-www-form-urlencoded")
}

func publicLoginPostWithContentType(t *testing.T, values url.Values, requested bool, callbackID, contentType string) pluginapi.ManagementResponse {
	t.Helper()
	if values.Get("csrf") == "" {
		if _, exists := values["csrf"]; !exists {
			values.Set("csrf", initCSRF())
		}
	}
	headers := http.Header{}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	if requested {
		headers.Set("X-Requested-With", "traework-panel")
	}
	raw, _ := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/resource/plugins/traework/panel", Headers: headers, Body: []byte(values.Encode())}, HostCallbackID: callbackID})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	return decodeManagement(t, out)
}

func TestRuntimeDiscoveryAndStableJitter(t *testing.T) {
	old := callHostRPC
	defer func() { callHostRPC = old }()
	callHostRPC = fakeHostAuthCall
	accounts, err := discoverScheduledAccounts(context.Background(), HostAuth{Call: callHostRPC})
	if err != nil || len(accounts) != 1 || accounts[0].AuthIndex != "a1" {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	a := stableAccountJitter("a1")
	if a != stableAccountJitter("a1") || a < 0 || a >= 30*time.Minute {
		t.Fatalf("jitter=%v", a)
	}
	if !nextDailyRun(time.Date(2026, 8, 31, 10, 0, 0, 0, hongKongLocation())).After(time.Date(2026, 8, 31, 10, 0, 0, 0, hongKongLocation())) {
		t.Fatal("next run not future")
	}
}

func decodeManagement(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env struct {
		Result pluginapi.ManagementResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env.Result
}

func TestParseAuthStorageAcceptsHostPhysicalEnvelope(t *testing.T) {
	raw := []byte(`{"provider":"traework","storage":{"provider":"traework","access_token":"access","refresh_token":"refresh"}}`)
	s, err := parseAuthStorage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessToken != "access" || s.RefreshToken != "refresh" {
		t.Fatalf("unexpected parsed storage: %#v", s)
	}
}

func fakeHostAuthCall(method string, payload any) (json.RawMessage, error) {
	switch method {
	case pluginabi.MethodHostAuthList:
		return json.RawMessage(`{"files":[{"auth_index":"a1","name":"traework-u1.json","provider":"traework","label":"Ada"},{"auth_index":"x","name":"other.json","provider":"other"}]}`), nil
	case pluginabi.MethodHostAuthGet:
		return json.RawMessage(`{"auth_index":"a1","json":{"type":"traework","provider":"traework","storage":{"provider":"traework","access_token":"secret-access","refresh_token":"secret-refresh","uid":"u1"}}}`), nil
	case pluginabi.MethodHostAuthSave:
		return json.RawMessage(`{"name":"traework-u1.json"}`), nil
	default:
		return nil, nil
	}
}

func TestPublicStatusReturnsJSON(t *testing.T) {
	raw, _ := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/traework/status"}})
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeManagement(t, out)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Headers.Get("Content-Type"), "application/json") {
		t.Fatalf("status=%d content-type=%q body=%s", resp.StatusCode, resp.Headers.Get("Content-Type"), resp.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["provider"] != ProviderTraeWork || body["status"] != "ok" {
		t.Fatalf("body=%s", resp.Body)
	}
}
