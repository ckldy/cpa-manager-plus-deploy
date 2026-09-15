package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestFallbackCandidatesExcludesRequestedAndOrdersReliableFirst(t *testing.T) {
	resetProbeStateForTest()
	// Seed (2026-09-14): qwen3.8-flash/hy3/mimo-v2.5 free, glm-5.3-flash
	// premium, deepseek-v4-flash unknown. Candidates exclude the requested
	// model and put reliably-free models first regardless of catalogue order.
	got := fallbackCandidates("glm-5.3-flash")
	want := []string{"qwen3.8-flash", "hy3", "mimo-v2.5", "deepseek-v4-flash"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestAttemptPayloadsOnlyForFreeModels(t *testing.T) {
	resetProbeStateForTest()
	// glm-5.3-flash is seeded premium (credit-gated 2026-09-14) but stays in
	// the free catalogue, so its requests must still get the full switch chain.
	base := rewritePayloadModel([]byte(`{"model":"bai-glm-5.3-flash (free)","messages":[]}`))
	if got := attemptPayloads(base); len(got) != 5 {
		t.Fatalf("catalogue model attempts=%d want=5", len(got))
	}
	premium := rewritePayloadModel([]byte(`{"model":"bai-glm-5.3","messages":[]}`))
	if got := attemptPayloads(premium); len(got) != 1 {
		t.Fatalf("premium attempts=%d want=1", len(got))
	}
	unknown := rewritePayloadModel([]byte(`{"model":"bai-hy4-preview","messages":[]}`))
	if got := attemptPayloads(unknown); len(got) != 1 {
		t.Fatalf("unknown attempts=%d want=1", len(got))
	}
	// A model probed free after startup (dynamic discovery) gets the chain too.
	setProbeClassForTest("brand-new-free", probeFree)
	dynamic := rewritePayloadModel([]byte(`{"model":"bai-brand-new-free","messages":[]}`))
	if got := attemptPayloads(dynamic); len(got) != 6 {
		t.Fatalf("probe-free dynamic attempts=%d want=6", len(got))
	}
}

func TestPayloadWithNativeModelRewritesModel(t *testing.T) {
	base := rewritePayloadModel([]byte(`{"model":"bai-glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`))
	out := payloadWithNativeModel(base, "qwen3.8-flash")
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "qwen3.8-flash" {
		t.Fatalf("model=%q", body["model"])
	}
}

func TestFallbackEligible(t *testing.T) {
	for _, class := range []string{"rate_limited", "upstream_unavailable", "insufficient_balance"} {
		if !fallbackEligible(class) {
			t.Fatalf("class %q should be eligible", class)
		}
	}
	for _, class := range []string{"credential_invalid", "bad_request", ""} {
		if fallbackEligible(class) {
			t.Fatalf("class %q must not be eligible", class)
		}
	}
}

func fallbackExecuteRequest(t *testing.T, client *fakeHostClient, model string) []byte {
	t.Helper()
	storage, _ := json.Marshal(authStorage{APIKey: "sk-bai-test-key", Provider: ProviderBAI})
	payload, _ := json.Marshal(map[string]any{"model": model, "messages": []any{}})
	reqBytes, _ := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "bai-test",
		AuthProvider: ProviderBAI,
		Model:        model,
		Format:       "openai",
		StorageJSON:  storage,
		Payload:      payload,
	}})
	_ = client
	return reqBytes
}

func TestHandleExecutorExecuteFallsBackOnRateLimit(t *testing.T) {
	resetProbeStateForTest()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "glm-5.3-flash" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback-ok"}}],"model":"qwen3.8-flash"}`))
	}))
	defer server.Close()

	client := &fakeHostClient{server: server}
	prev := executorClientHook
	executorClientHook = func(req pluginapi.ExecutorRequest) pluginapi.HostHTTPClient { return client }
	defer func() { executorClientHook = prev }()

	out, err := handleExecutorExecute(fallbackExecuteRequest(t, client, "bai-glm-5.3-flash (free)"))
	if err != nil {
		t.Fatalf("expected fallback success, got error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls=%d want=2", got)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("envelope=%s err=%v", out, err)
	}
	var execResp pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &execResp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(execResp.Payload), "fallback-ok") {
		t.Fatalf("payload=%s", execResp.Payload)
	}
}

func TestHandleExecutorExecuteDoesNotFallBackOnCredentialError(t *testing.T) {
	resetProbeStateForTest()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer server.Close()

	client := &fakeHostClient{server: server}
	prev := executorClientHook
	executorClientHook = func(req pluginapi.ExecutorRequest) pluginapi.HostHTTPClient { return client }
	defer func() { executorClientHook = prev }()

	_, err := handleExecutorExecute(fallbackExecuteRequest(t, client, "bai-glm-5.3-flash"))
	if err == nil || !strings.Contains(err.Error(), "无效或已过期") {
		t.Fatalf("err=%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls=%d want=1", got)
	}
}

func TestHandleExecutorExecuteDoesNotFallBackForPremiumModel(t *testing.T) {
	resetProbeStateForTest()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer server.Close()

	client := &fakeHostClient{server: server}
	prev := executorClientHook
	executorClientHook = func(req pluginapi.ExecutorRequest) pluginapi.HostHTTPClient { return client }
	defer func() { executorClientHook = prev }()

	_, err := handleExecutorExecute(fallbackExecuteRequest(t, client, "bai-glm-5.3"))
	if err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("err=%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls=%d want=1", got)
	}
}

func TestHandleExecutorExecuteStreamFallsBackBeforeChunks(t *testing.T) {
	resetProbeStateForTest()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "glm-5.3-flash" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"qwen3.8-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := &fakeHostClient{server: server}
	prev := executorClientHook
	executorClientHook = func(req pluginapi.ExecutorRequest) pluginapi.HostHTTPClient { return client }
	defer func() { executorClientHook = prev }()

	out, err := handleExecutorExecuteStream(fallbackExecuteRequest(t, client, "bai-glm-5.3-flash"))
	if err != nil {
		t.Fatalf("expected stream fallback success, got error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls=%d want=2", got)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("envelope=%s err=%v", out, err)
	}
	var sr streamResponse
	if err := json.Unmarshal(env.Result, &sr); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, c := range sr.Chunks {
		sb.Write(c.Payload)
	}
	if !strings.Contains(sb.String(), "qwen3.8-flash") {
		t.Fatalf("chunks=%s", sb.String())
	}
}

// TestHandleExecutorExecuteFallsBackOnCreditQuota guards the 2026-09-14
// regression: upstream switched glm-5.3-flash to a credit gate and reports it
// as HTTP 400 code=insufficient_user_quota. That must be classified as
// insufficient_balance (fallback-eligible), not bad_request (hard fail), and a
// demoted catalogue model must still switch even though its probe class is
// premium.
func TestHandleExecutorExecuteFallsBackOnCreditQuota(t *testing.T) {
	resetProbeStateForTest()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "glm-5.3-flash" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"credit insufficient balance: balance=0 required=102 (request id: 2026091412115057306692c955d568vABW1sIZ)","type":"api_error","param":"","code":"insufficient_user_quota"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback-ok"}}],"model":"qwen3.8-flash"}`))
	}))
	defer server.Close()

	client := &fakeHostClient{server: server}
	prev := executorClientHook
	executorClientHook = func(req pluginapi.ExecutorRequest) pluginapi.HostHTTPClient { return client }
	defer func() { executorClientHook = prev }()

	out, err := handleExecutorExecute(fallbackExecuteRequest(t, client, "bai-glm-5.3-flash (free)"))
	if err != nil {
		t.Fatalf("expected quota fallback success, got error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls=%d want=2", got)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("envelope=%s err=%v", out, err)
	}
	var execResp pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &execResp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(execResp.Payload), "fallback-ok") {
		t.Fatalf("payload=%s", execResp.Payload)
	}
}

func TestClassifyUpstreamStatusCreditQuota(t *testing.T) {
	quotaBody := []byte(`{"error":{"message":"credit insufficient balance: balance=0 required=102","type":"api_error","param":"","code":"insufficient_user_quota"}}`)
	if got := classifyUpstreamStatus(http.StatusBadRequest, quotaBody); got != "insufficient_balance" {
		t.Fatalf("400 quota got=%q", got)
	}
	if got := classifyUpstreamStatus(http.StatusBadRequest, []byte(`{"error":{"message":"unknown model"}}`)); got != "bad_request" {
		t.Fatalf("400 generic must stay bad_request, got=%q", got)
	}
	if got := classifyUpstreamStatus(http.StatusUnauthorized, []byte(`{"error":{"message":"invalid key"}}`)); got != "credential_invalid" {
		t.Fatalf("401 got=%q", got)
	}
	if !fallbackEligible(classifyUpstreamStatus(http.StatusBadRequest, quotaBody)) {
		t.Fatal("400 quota must be fallback-eligible")
	}
}
