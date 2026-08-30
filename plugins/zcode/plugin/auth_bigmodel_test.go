package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBigModelAuthLoginStartUsesCustomAuthorizeParameters(t *testing.T) {
	raw, _ := json.Marshal(rpcAuthLoginStartRequest{AuthLoginStartRequest: pluginapi.AuthLoginStartRequest{Provider: "bigmodel"}})
	out, err := handleAuthLoginStart(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeLoginStartForTest(t, out)
	defer removeOAuthCallback(resp.State)
	u, err := url.Parse(resp.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "bigmodel.cn" || u.Path != "/login" {
		t.Fatalf("authorize URL=%q", resp.URL)
	}
	if got := u.Query().Get("appId"); got != "zcode" {
		t.Fatalf("appId=%q", got)
	}
	if got := u.Query().Get("redirect"); got != ZCodeRedirectURI {
		t.Fatalf("redirect=%q", got)
	}
	if got := u.Query().Get("state"); got == "" || got != resp.State {
		t.Fatalf("authorize state=%q response state=%q", got, resp.State)
	}
	if u.Query().Get("client_id") != "" || u.Query().Get("redirect_uri") != "" {
		t.Fatalf("BigModel URL used Z.AI OAuth parameters: %q", u.RawQuery)
	}
}

func TestParseOAuthTokenResponseSupportsBigModelFields(t *testing.T) {
	tokens, err := parseOAuthTokensForProvider(json.RawMessage(`{"token":"zcode-jwt","bigmodel":{"access_token":"bm-access"},"user":{"user_id":"bm-user"}}`), "bigmodel")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.JWTToken != "zcode-jwt" || tokens.AccessToken != "bm-access" || tokens.UserID != "bm-user" {
		t.Fatalf("tokens=%+v", tokens)
	}
}

func TestResolveBigModelAPIKeyReadsExistingKeyWithoutCreating(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	methods := make([]string, 0, 3)
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		raw, _ := json.Marshal(payload)
		var req rpcHostHTTPRequest
		_ = json.Unmarshal(raw, &req)
		methods = append(methods, req.Request.Method)
		if req.Request.Headers.Get("Authorization") != "bm-access" {
			t.Fatalf("authorization=%q", req.Request.Headers.Get("Authorization"))
		}
		switch len(methods) {
		case 1:
			if req.Request.URL != "https://bigmodel.cn/api/biz/customer/getCustomerInfo" {
				t.Fatalf("customer URL=%q", req.Request.URL)
			}
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"organizations":[{"organizationId":"org","projects":[{"projectId":"project"}]}]}}`)})
		case 2:
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":[{"name":"zcode-api-key","apiKey":"bm-key"}]}`)})
		case 3:
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"secretKey":"bm-secret"}}`)})
		default:
			t.Fatal("unexpected extra request")
			return nil, nil
		}
	}
	key, err := resolveBigModelAPIKey("host-callback", "bm-access")
	if err != nil {
		t.Fatal(err)
	}
	if key != "bm-key.bm-secret" {
		t.Fatalf("key=%q", key)
	}
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("BigModel key lookup mutated upstream with %s", method)
		}
	}
}

func TestResolveZaiAPIKeyDoesNotCreateMissingKey(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	methods := make([]string, 0, 3)
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		raw, _ := json.Marshal(payload)
		var req rpcHostHTTPRequest
		_ = json.Unmarshal(raw, &req)
		methods = append(methods, req.Request.Method)
		switch len(methods) {
		case 1: // business login
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"access_token":"biz-token"}}`)})
		case 2: // customer info
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"organizations":[{"organizationId":"org","projects":[{"projectId":"project"}]}]}}`)})
		case 3: // existing keys: none
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":[]}`)})
		default:
			t.Fatal("unexpected mutation or extra request")
			return nil, nil
		}
	}
	if _, err := resolveZaiAPIKey("host-callback", "oauth-access"); err == nil {
		t.Fatal("missing existing key should fail")
	}
	for _, method := range methods[1:] { // business login is protocol-required POST
		if method != http.MethodGet {
			t.Fatalf("Z.AI key lookup mutated upstream with %s", method)
		}
	}
}

func TestFindAPIKeyIDRequiresExactName(t *testing.T) {
	data := json.RawMessage(`[{"name":"other-key","apiKey":"wrong"},{"name":"zcode-api-key","apiKey":"right"}]`)
	if got := findAPIKeyID(data, zcodeAPIKeyName); got != "right" {
		t.Fatalf("exact key=%q", got)
	}
	if got := findAPIKeyID(json.RawMessage(`[{"name":"other-key","apiKey":"wrong"}]`), zcodeAPIKeyName); got != "" {
		t.Fatalf("arbitrary fallback key selected: %q", got)
	}
}

func TestAuthLoginPollRejectsProviderMismatch(t *testing.T) {
	state := "oauth-provider-bound-state"
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{Provider: "zai", Code: "auth-code", ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)
	raw, _ := json.Marshal(rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{Provider: "bigmodel", State: state}})
	out, err := handleAuthLoginPoll(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeLoginPollForTest(t, out)
	if resp.Status != pluginapi.AuthLoginStatusError || !strings.Contains(resp.Message, "平台") {
		t.Fatalf("provider mismatch was not rejected: %#v", resp)
	}
}

func TestBigModelAuthLoginPollPersistsIsolatedCredentialAndMID(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	state := strings.Repeat("7", 64)
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{Provider: "bigmodel", Code: "auth-code", ExpiresAt: time.Now().Add(time.Minute)}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	responses := []pluginapi.HTTPResponse{
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"token":"zcode-jwt","bigmodel":{"access_token":"bm-access"},"user":{"user_id":"bm-user"}}}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"organizations":[{"organizationId":"org","projects":[{"projectId":"project"}]}]}}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":[{"name":"zcode-api-key","apiKey":"bm-key"}]}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"secretKey":"bm-secret"}}`)},
	}
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		if len(responses) == 0 {
			t.Fatal("unexpected extra request")
		}
		raw, _ := json.Marshal(payload)
		var req rpcHostHTTPRequest
		_ = json.Unmarshal(raw, &req)
		if len(responses) == 4 {
			var body map[string]string
			_ = json.Unmarshal(req.Request.Body, &body)
			if body["provider"] != "bigmodel" || body["redirect_uri"] != ZCodeRedirectURI || body["state"] != state {
				t.Fatalf("token exchange body=%v", body)
			}
		}
		response := responses[0]
		responses = responses[1:]
		return json.Marshal(response)
	}

	raw, _ := json.Marshal(rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{Provider: "bigmodel", State: state}, HostCallbackID: "host-callback"})
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
	if storage.Provider != "bigmodel" || storage.APIKey != "bm-key.bm-secret" || storage.ZCodeJWTToken != "" || storage.Source != "coding-plan-key" {
		t.Fatalf("platform credentials were not isolated: %#v", storage)
	}
	if storage.DeviceMID == "" {
		t.Fatal("DeviceMID was not persisted")
	}
}
