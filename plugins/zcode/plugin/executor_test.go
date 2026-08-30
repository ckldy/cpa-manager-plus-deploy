package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func useExplicitPaidRouteForTest(t *testing.T) {
	t.Helper()
	routes.Lock()
	old := routes.config
	routes.config = routeConfig{Mode: "strict", StrictRoute: "api-key"}
	routes.Unlock()
	t.Cleanup(func() {
		routes.Lock()
		routes.config = old
		routes.Unlock()
	})
}

func useEndpointRouterForTest(t *testing.T, mode string, mapping map[string]string) {
	t.Helper()
	old := endpointRouter
	r := newEndpointRouting(mode)
	r.snapshot = &routingSnapshot{ExpiresAt: time.Now().Add(time.Hour), Mapping: mapping}
	endpointRouter = r
	t.Cleanup(func() { endpointRouter = old })
}

func mappingForTest(from, to string) map[string]string {
	key, _ := routingKey(from)
	return map[string]string{key: to}
}

func TestExecutorResolveCallCountAndModes(t *testing.T) {
	useExplicitPaidRouteForTest(t)
	storage, _ := json.Marshal(authStorage{APIKey: "test-api-key"})

	t.Run("sync observe", func(t *testing.T) {
		useEndpointRouterForTest(t, routingObserveMode, mappingForTest(apiKeyUpstreamURL, "https://api.z.ai/routed"))
		oldCall := callHostRPC
		defer func() { callHostRPC = oldCall }()
		calls := 0
		callHostRPC = func(_ string, payload any) (json.RawMessage, error) {
			calls++
			raw, _ := json.Marshal(payload)
			var got rpcHostHTTPRequest
			_ = json.Unmarshal(raw, &got)
			if got.Request.URL != apiKeyUpstreamURL {
				t.Fatalf("observe rewrote URL to %q", got.Request.URL)
			}
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK})
		}
		raw, _ := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Payload: []byte(`{"model":"zcode-glm-5.1"}`), StorageJSON: storage}, HostCallbackID: "cb"})
		if _, err := handleExecutorExecute(raw); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || len(endpointRouter.status().Observed) != 1 {
			t.Fatalf("calls=%d observations=%d", calls, len(endpointRouter.status().Observed))
		}
	})

	t.Run("generic active", func(t *testing.T) {
		const from, target = "https://api.z.ai/original", "https://api.z.ai/routed"
		useEndpointRouterForTest(t, routingActiveMode, mappingForTest(from, target))
		oldCall := callHostRPC
		defer func() { callHostRPC = oldCall }()
		calls := 0
		callHostRPC = func(_ string, payload any) (json.RawMessage, error) {
			calls++
			raw, _ := json.Marshal(payload)
			var got rpcHostHTTPRequest
			_ = json.Unmarshal(raw, &got)
			if got.Request.URL != target {
				t.Fatalf("active URL=%q", got.Request.URL)
			}
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK})
		}
		raw, _ := json.Marshal(struct {
			pluginapi.ExecutorHTTPRequest
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{URL: from, StorageJSON: storage}, HostCallbackID: "cb"})
		if _, err := handleExecutorHTTPRequest(raw); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("calls=%d", calls)
		}
	})
}

func TestActiveRewriteRechecksAllowlistAndCredentialBinding(t *testing.T) {
	credential := upstreamCredential{URL: apiKeyUpstreamURL}
	for _, target := range []string{"https://evil.example/steal", bigModelCodingUpstreamURL, codingPlanUpstreamURL} {
		t.Run(target, func(t *testing.T) {
			useEndpointRouterForTest(t, routingActiveMode, mappingForTest(apiKeyUpstreamURL, target))
			if _, err := resolveCredentialURL(context.Background(), nil, "", credential, apiKeyUpstreamURL); err == nil {
				t.Fatalf("active rewrite to %q bypassed trust binding", target)
			}
		})
	}
}

func TestRegistrationDeclaresOnlyNativeAnthropicExecutorFormat(t *testing.T) {
	cap := pluginRegistration().Capabilities
	if got := cap.ExecutorInputFormats; len(got) != 1 || got[0] != "anthropic" {
		t.Fatalf("executor input formats=%v, want native anthropic only", got)
	}
	if got := cap.ExecutorOutputFormats; len(got) != 1 || got[0] != "anthropic" {
		t.Fatalf("executor output formats=%v, want native anthropic only", got)
	}
	if cap.RequestTranslator || cap.ResponseTranslator || cap.RequestNormalizer {
		t.Fatal("plugin must reuse host translation instead of declaring a second translator")
	}
	if cap.UsagePlugin {
		t.Fatal("plugin must not claim usage normalization")
	}
}

func TestExecutorCountTokensReturnsUnsupportedEnvelope(t *testing.T) {
	raw, err := handleExecutorCountTokens([]byte(`{"payload":"ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "count_tokens_unsupported" {
		t.Fatalf("unexpected count_tokens envelope: %s", raw)
	}
	if strings.Contains(string(raw), `"total_tokens":0`) {
		t.Fatalf("count_tokens still reports a false zero: %s", raw)
	}
}

func TestForceStreamFieldSetsCorrectValue(t *testing.T) {
	in := []byte(`{"model":"test","messages":[]}`)
	syncPayload := forceStreamField(in, false)
	var body map[string]any
	if err := json.Unmarshal(syncPayload, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != false {
		t.Fatalf("sync stream=%v", body["stream"])
	}
	streamPayload := forceStreamField(in, true)
	if err := json.Unmarshal(streamPayload, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Fatalf("stream stream=%v", body["stream"])
	}
	// Existing stream field should be overridden
	override := forceStreamField([]byte(`{"stream":true}`), false)
	if err := json.Unmarshal(override, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != false {
		t.Fatalf("override stream=%v", body["stream"])
	}
	// Malformed JSON should pass through
	if got := forceStreamField([]byte(`not-json`), true); string(got) != "not-json" {
		t.Fatalf("malformed passthrough=%q", got)
	}
}

func TestUpstreamErrorSanitized(t *testing.T) {
	// The error message must not contain the raw upstream body and must be a
	// fixed category message.
	err := upstreamError(pluginapi.HTTPResponse{StatusCode: http.StatusBadGateway, Body: []byte(`{"error":{"message":"secret-stack-trace"}}`)}, upstreamCredential{})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret-stack-trace") || strings.Contains(msg, "500") {
		t.Fatalf("leaked upstream body in error: %q", msg)
	}
	// Must be one of the fixed category messages (not a passthrough of the body)
	if !strings.Contains(msg, "暂不可用") && !strings.Contains(msg, "请求失败") && !strings.Contains(msg, "request failed") {
		t.Fatalf("unexpected error: %q", msg)
	}
}

func TestHandleExecutorHTTPRequestStripsAuthHeaders(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	var seenHeaders http.Header
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		raw, _ := json.Marshal(payload)
		var req rpcHostHTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		seenHeaders = req.Request.Headers
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)})
	}
	storage, _ := json.Marshal(authStorage{ZCodeJWTToken: "jwt", APIKey: "key", Provider: "zai"})
	raw, _ := json.Marshal(struct {
		pluginapi.ExecutorHTTPRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}{
		ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
			URL:         "https://zcode.z.ai/test",
			StorageJSON: storage,
			Headers:     http.Header{"Authorization": {"Bearer client-key"}, "X-Api-Key": {"client-key"}, "X-Custom": {"keep"}},
		},
		HostCallbackID: "cb",
	})
	if _, err := handleExecutorHTTPRequest(raw); err != nil {
		t.Fatal(err)
	}
	if seenHeaders.Get("Authorization") != "Bearer jwt" {
		t.Fatalf("Authorization should be plugin credential, got %q", seenHeaders.Get("Authorization"))
	}
	// Client's injected key must NOT be present
	if seenHeaders.Get("X-Api-Key") != "" {
		t.Fatalf("X-Api-Key leaked: %q", seenHeaders.Get("X-Api-Key"))
	}
	if seenHeaders.Get("X-Custom") != "keep" {
		t.Fatalf("X-Custom missing: %q", seenHeaders.Get("X-Custom"))
	}
}

func TestExecuteRejectsOversizedPayload(t *testing.T) {
	big := make([]byte, requestBodyMax+1)
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{Payload: big})
	if _, err := handleExecutorExecute(raw); err == nil {
		t.Fatal("expected rejection of oversized payload")
	}
}

func TestSanitizeErrorInExecuteStreamViaHostCallback(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostHTTPDoStream {
			return json.Marshal(rpcHostHTTPStreamResponse{StatusCode: http.StatusBadGateway, StreamID: "err-stream"})
		}
		if method == pluginabi.MethodHostHTTPStreamRead {
			return json.Marshal(rpcHostHTTPStreamReadResponse{Payload: []byte(`{"error":"internal"}`), Done: true})
		}
		if method == pluginabi.MethodHostHTTPStreamClose {
			return json.RawMessage(`{}`), nil
		}
		return nil, nil
	}
	storage, _ := json.Marshal(authStorage{ZCodeJWTToken: "jwt", APIKey: "key", Provider: "zai"})
	raw, _ := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{StorageJSON: storage},
		HostCallbackID:  "cb",
		StreamID:        "plugin-stream",
	})
	_, err := handleExecutorExecuteStream(raw)
	if err == nil {
		t.Fatal("expected error from bad gateway")
	}
	msg := err.Error()
	if strings.Contains(msg, "internal") || strings.Contains(msg, "500") {
		t.Fatalf("leaked upstream body: %q", msg)
	}
}

func TestClassifyUpstreamStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"balance", http.StatusTooManyRequests, `{"code":1113,"msg":"Insufficient balance"}`, "insufficient_balance"},
		{"captcha", http.StatusForbidden, `captcha verification failed`, "captcha_required"},
		{"credential", http.StatusUnauthorized, `invalid token`, "credential_invalid"},
		{"upstream", http.StatusBadGateway, `bad gateway`, "upstream_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUpstreamStatus(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("classifyUpstreamStatus()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestJWTHeadersUseCodingPlanBearerAuth(t *testing.T) {
	h := zcodeHeaders("jwt-token", "", "")
	if got := h.Get("Authorization"); got != "Bearer jwt-token" {
		t.Fatalf("Authorization=%q", got)
	}
	if got := h.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key must not carry a Coding Plan JWT: %q", got)
	}
}

func TestRewritePayloadModelKeepsFreeModelLowercase(t *testing.T) {
	out := rewritePayloadModel([]byte(`{"model":"zcode-glm-4.5-flash","messages":[]}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "glm-4.5-flash" {
		t.Fatalf("model=%q", body["model"])
	}
	if !isFreeModel("glm-4.5-flash") || isFreeModel("GLM-5.2") {
		t.Fatal("isFreeModel classification mismatch")
	}
}

func TestZCodeModelsRegisterFreeModel(t *testing.T) {
	found := false
	for _, model := range zcodeModels() {
		if model.ID == "zcode-glm-4.5-flash" {
			found = true
		}
	}
	if !found {
		t.Fatal("zcode-glm-4.5-flash not registered")
	}
}

func TestRewritePayloadModelMapsNativeNames(t *testing.T) {
	out := rewritePayloadModel([]byte(`{"model":"zcode-glm-5.1","messages":[]}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "GLM-5.1" {
		t.Fatalf("model=%q", body["model"])
	}
}

func TestJWTExplicitlyWinsOverAPIKey(t *testing.T) {
	storage, _ := json.Marshal(authStorage{
		ZCodeJWTToken:       "jwt-token",
		APIKey:              "test-api-key",
		CaptchaVerifyParam:  "verify-param",
		CaptchaVerifyRegion: "sgp",
	})
	credential, err := resolveCredential(storage)
	if err != nil {
		t.Fatal(err)
	}
	if !credential.CodingPlan || credential.URL != codingPlanUpstreamURL {
		t.Fatalf("credential=%+v, want Coding Plan route", credential)
	}
	if credential.Headers.Get("Authorization") != "Bearer jwt-token" {
		t.Fatalf("authorization=%q", credential.Headers.Get("Authorization"))
	}
	if credential.Headers.Get("x-api-key") != "" {
		t.Fatal("API key header leaked into Coding Plan request")
	}
}

func TestFreeFirstRejectsAPIKeyOnlyWithoutExplicitPaidRoute(t *testing.T) {
	routes.Lock()
	old := routes.config
	routes.config = routeConfig{Mode: "free-first", StrictRoute: "coding-plan"}
	routes.Unlock()
	defer func() {
		routes.Lock()
		routes.config = old
		routes.Unlock()
	}()
	storage, _ := json.Marshal(authStorage{APIKey: "test-api-key", Provider: "zai"})
	if _, err := resolveCredential(storage); err == nil || !strings.Contains(err.Error(), "explicitly") {
		t.Fatalf("API-key-only free-first route should be rejected: %v", err)
	}
}

func TestCredentialTargetIsHostBound(t *testing.T) {
	jwt := upstreamCredential{URL: codingPlanUpstreamURL}
	if err := validateCredentialTarget("https://zcode.z.ai/api/v1/test", jwt); err != nil {
		t.Fatalf("bound host rejected: %v", err)
	}
	if err := validateCredentialTarget("https://api.z.ai/api/anthropic/v1/messages", jwt); err == nil {
		t.Fatal("JWT was allowed to cross into api.z.ai")
	}
	bigmodel := upstreamCredential{URL: bigModelCodingUpstreamURL}
	if err := validateCredentialTarget("https://zcode.z.ai/api/v1/test", bigmodel); err == nil {
		t.Fatal("BigModel credential was allowed to cross into zcode.z.ai")
	}
}

func TestCredentialTargetRejectsNonStandardPort(t *testing.T) {
	credential := upstreamCredential{URL: "https://api.z.ai/api/anthropic/v1/messages"}
	if err := validateCredentialTarget("https://api.z.ai:444/test", credential); err == nil {
		t.Fatal("credential target accepted non-standard port")
	}
	if err := validateZCodeURL("https://zcode.z.ai:8443/test"); err == nil {
		t.Fatal("allowlist accepted non-standard port")
	}
}

func TestJWTHeadersIncludeCaptchaWhenProvided(t *testing.T) {
	h := zcodeHeaders("jwt-token", "verify-param", "sgp")
	if got := h.Get("X-Aliyun-Captcha-Verify-Param"); got != "verify-param" {
		t.Fatalf("captcha param=%q", got)
	}
	if got := h.Get("X-Aliyun-Captcha-Verify-Region"); got != "sgp" {
		t.Fatalf("captcha region=%q", got)
	}
}

func TestExecuteAPIKeyWithoutSerializedHTTPClient(t *testing.T) {
	useExplicitPaidRouteForTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-api-key" {
			t.Errorf("x-api-key=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer server.Close()

	oldURL := apiKeyUpstreamURL
	apiKeyUpstreamURL = server.URL
	defer func() { apiKeyUpstreamURL = oldURL }()

	storage, _ := json.Marshal(authStorage{APIKey: "test-api-key"})
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		Payload:     []byte(`{"model":"zcode-glm-5.1","messages":[]}`),
		StorageJSON: storage,
	})
	out, err := handleExecutorExecute(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("unexpected response: %s", out)
	}
}

func TestExecuteUsesHostHTTPCallbackFromRPCRequest(t *testing.T) {
	useExplicitPaidRouteForTest(t)
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()

	called := false
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		called = true
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method=%q", method)
		}
		raw, _ := json.Marshal(payload)
		var req rpcHostHTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if req.HostCallbackID != "callback-123" {
			t.Fatalf("host_callback_id=%q", req.HostCallbackID)
		}
		return json.Marshal(pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"type":"message","content":[]}`),
		})
	}

	storage, _ := json.Marshal(authStorage{APIKey: "test-api-key"})
	raw, _ := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Payload:     []byte(`{"model":"zcode-glm-5.1","messages":[]}`),
			StorageJSON: storage,
		},
		HostCallbackID: "callback-123",
	})
	if _, err := handleExecutorExecute(raw); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("host HTTP callback was not used")
	}
}

func TestExecuteStreamUsesHostBridges(t *testing.T) {
	useExplicitPaidRouteForTest(t)
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()

	readCount := 0
	emitted := make(chan string, 1)
	closed := make(chan string, 1)
	httpClosed := make(chan struct{}, 1)
	callHostRPC = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			raw, _ := json.Marshal(payload)
			var got rpcHostHTTPRequest
			_ = json.Unmarshal(raw, &got)
			if got.Request.URL != "https://api.z.ai/routed" {
				t.Fatalf("active stream URL=%q", got.Request.URL)
			}
			return json.Marshal(rpcHostHTTPStreamResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   "http-stream-1",
			})
		case pluginabi.MethodHostHTTPStreamRead:
			readCount++
			return json.Marshal(rpcHostHTTPStreamReadResponse{Payload: []byte("data: ok\n\n"), Done: true})
		case pluginabi.MethodHostStreamEmit:
			raw, _ := json.Marshal(payload)
			var req rpcStreamEmitRequest
			_ = json.Unmarshal(raw, &req)
			emitted <- string(req.Payload)
			return json.RawMessage(`{}`), nil
		case pluginabi.MethodHostStreamClose:
			raw, _ := json.Marshal(payload)
			var req rpcStreamCloseRequest
			_ = json.Unmarshal(raw, &req)
			closed <- req.Error
			return json.RawMessage(`{}`), nil
		case pluginabi.MethodHostHTTPStreamClose:
			httpClosed <- struct{}{}
			return json.RawMessage(`{}`), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	}

	useEndpointRouterForTest(t, routingActiveMode, mappingForTest(apiKeyUpstreamURL, "https://api.z.ai/routed"))
	storage, _ := json.Marshal(authStorage{APIKey: "test-api-key"})
	raw, _ := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Payload:     []byte(`{"model":"zcode-glm-5.1","messages":[],"stream":true}`),
			StorageJSON: storage,
		},
		HostCallbackID: "callback-123",
		StreamID:       "plugin-stream-1",
	})
	out, err := handleExecutorExecuteStream(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	var resp streamResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Chunks) != 0 {
		t.Fatalf("buffered chunks=%d, want async host stream", len(resp.Chunks))
	}
	if got := <-emitted; got != "data: ok\n\n" {
		t.Fatalf("emitted=%q", got)
	}
	if got := <-closed; got != "" {
		t.Fatalf("close error=%q", got)
	}
	// Wait for forwardHostHTTPStream's deferred host-stream close before the
	// test restores the global callback; otherwise the goroutine can race with
	// the deferred assignment below.
	<-httpClosed
	if readCount != 1 {
		t.Fatalf("read count=%d", readCount)
	}
}

func TestJWTHeadersIncludeZCodeClientContract(t *testing.T) {
	h := zcodeHeaders("jwt-token", "", "")
	for key, want := range map[string]string{
		"X-ZCode-Agent":       "glm",
		"X-Title":             "Z Code@electron",
		"X-ZCode-App-Version": zcodeClientVersion,
	} {
		if got := h.Get(key); got != want {
			t.Fatalf("%s=%q, want %q", key, got, want)
		}
	}
}

func TestCaptchaRejectionExplainsRefresh(t *testing.T) {
	err := upstreamError(pluginapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(`{"error":"captcha verify failed"}`)}, upstreamCredential{CodingPlan: true, CaptchaUsed: true})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecuteReturnsErrorForUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Authentication Failed"}}`))
	}))
	defer server.Close()

	oldURL := apiKeyUpstreamURL
	apiKeyUpstreamURL = server.URL
	defer func() { apiKeyUpstreamURL = oldURL }()
	storage, _ := json.Marshal(authStorage{APIKey: "test-api-key"})
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{Payload: []byte(`{"model":"zcode-glm-5.1","messages":[]}`), StorageJSON: storage})
	if _, err := handleExecutorExecute(raw); err == nil {
		t.Fatal("upstream 401 was returned as a successful executor payload")
	}
}

func TestValidateZCodeURLRejectsCredentialExfiltration(t *testing.T) {
	if err := validateZCodeURL("https://example.com/steal"); err == nil {
		t.Fatal("accepted an untrusted credentialed request URL")
	}
	if err := validateZCodeURL("https://api.z.ai/api/anthropic/v1/messages"); err != nil {
		t.Fatalf("rejected trusted Z.AI URL: %v", err)
	}
}

func TestZCodeModelsIncludeCurrentBigModelCatalog(t *testing.T) {
	models := zcodeModels()
	byID := map[string]pluginapi.ModelInfo{}
	for _, model := range models {
		byID[model.ID] = model
	}
	for _, id := range []string{"zcode-glm-5.3", "zcode-glm-5.3-flash"} {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if m.ContextLength != 1000000 || m.MaxCompletionTokens != 128000 {
			t.Fatalf("wrong limits for %s: %#v", id, m)
		}
	}
	if got := byID["zcode-glm-5.3-flash"].SupportedInputModalities; len(got) != 2 || got[1] != "image" {
		t.Fatalf("flash modalities=%v", got)
	}
}

func TestModelMatrixUsesRegisteredCatalog(t *testing.T) {
	body := string(renderZCodeAccountPage(zcodeStatusPage{}))
	for _, model := range zcodeModels() {
		if !strings.Contains(body, model.ID) {
			t.Fatalf("matrix missing registered model %s", model.ID)
		}
	}
}

func TestFrameSSEPayloadSeparatesEventsAndPreservesThinking(t *testing.T) {
	in := []byte("event: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"ok\"}}\n\n")
	got := frameSSEPayload(in)
	if len(got) != 2 {
		t.Fatalf("frames=%d: %q", len(got), got)
	}
	if strings.Contains(string(got[0]), "event:") || !strings.HasPrefix(string(got[0]), "data: ") {
		t.Fatalf("bad first frame: %q", got[0])
	}
	if !strings.Contains(string(got[1]), "thinking_delta") {
		t.Fatalf("thinking lost: %q", got[1])
	}
}

func TestTakeSSEFramesBuffersSplitEvent(t *testing.T) {
	frames, rest := takeSSEFrames([]byte("event: content_block_delta\ndata: {\"type\":\"thinking_de"), false)
	if len(frames) != 0 || len(rest) == 0 {
		t.Fatalf("frames=%d rest=%q", len(frames), rest)
	}
	frames, rest = takeSSEFrames(append(rest, []byte("lta\"}}\n\n")...), false)
	if len(frames) != 1 || len(rest) != 0 || !strings.Contains(string(frames[0]), "thinking_delta") {
		t.Fatalf("frames=%q rest=%q", frames, rest)
	}
}
