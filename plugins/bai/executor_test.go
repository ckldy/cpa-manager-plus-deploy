package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRewritePayloadModelStripsBAIPrefix(t *testing.T) {
	out := rewritePayloadModel([]byte(`{"model":"bai-claude-opus-4.8","messages":[]}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "claude-opus-4.8" {
		t.Fatalf("model=%q", body["model"])
	}
}

func TestRewritePayloadModelStripsFreeSuffix(t *testing.T) {
	out := rewritePayloadModel([]byte(`{"model":"bai-glm-5.3-flash (free)","messages":[]}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "glm-5.3-flash" {
		t.Fatalf("model=%q", body["model"])
	}
}

func TestRewritePayloadModelNormalizesContentBlocksAndMetadata(t *testing.T) {
	in := []byte(`{"model":"bai-glm-5.3-flash","metadata":{"user_id":"abc"},"messages":[{"role":"user","content":[{"type":"text","text":"你好"}]}]}`)
	out := rewritePayloadModel(in)
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["metadata"]; ok {
		t.Fatal("metadata must be dropped")
	}
	msgs := body["messages"].([]any)
	msg := msgs[0].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("content=%v (%T)", msg["content"], msg["content"])
	}
}

func TestRewritePayloadModelLeavesNativeName(t *testing.T) {
	out := rewritePayloadModel([]byte(`{"model":"claude-opus-4-8","messages":[]}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "claude-opus-4-8" {
		t.Fatalf("model=%q", body["model"])
	}
}

func TestAuthParseBareKey(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: ProviderBAI, FileName: "bai-test.json", RawJSON: []byte(`"sk-bai-test-key-1234567890abcd"`)})
	resp, err := decodeAuthParse(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Handled {
		t.Fatal("bare key should be handled")
	}
	var storage authStorage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage.APIKey != "sk-bai-test-key-1234567890abcd" || storage.Provider != ProviderBAI {
		t.Fatalf("storage=%+v", storage)
	}
}

func TestAuthParsePrefixedKey(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: ProviderBAI, FileName: "bai-test.json", RawJSON: []byte(`"bai:sk-bai-test-key-1234567890abcd"`)})
	resp, err := decodeAuthParse(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Handled {
		t.Fatal("prefixed key should be handled")
	}
	var storage authStorage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage.APIKey != "sk-bai-test-key-1234567890abcd" {
		t.Fatalf("storage=%+v", storage)
	}
}

func TestAuthParseJSONBody(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: ProviderBAI, FileName: "bai-test.json", RawJSON: []byte(`{"api_key":"sk-bai-test-key-1234567890abcd","provider":"bai"}`)})
	resp, err := decodeAuthParse(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Handled {
		t.Fatal("json body should be handled")
	}
	var storage authStorage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage.APIKey != "sk-bai-test-key-1234567890abcd" {
		t.Fatalf("storage=%+v", storage)
	}
	if !strings.HasPrefix(resp.Auth.Label, "B.AI API Key · sk-b") || strings.Contains(resp.Auth.Label, "1234567890abcd") {
		t.Fatalf("label must mask the key: %q", resp.Auth.Label)
	}
}

func TestAuthParseRejectsOtherProviders(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: "zcode", FileName: "zcode-test.json", RawJSON: []byte(`{"type":"zcode","zcode_jwt_token":"eyJhbGciOi.xxxxx.yyyyy"}`)})
	resp, err := decodeAuthParse(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Handled {
		t.Fatal("zcode material must not be handled by bai plugin")
	}
}

func decodeAuthParse(t *testing.T, raw []byte) (*pluginapi.AuthParseResponse, error) {
	t.Helper()
	out, err := handleAuthParse(raw)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, err
	}
	if !env.OK {
		t.Fatalf("envelope error: %+v", env.Error)
	}
	var resp pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// fakeHostClient adapts an httptest server into pluginapi.HostHTTPClient.
type fakeHostClient struct {
	server *httptest.Server
}

func (c *fakeHostClient) Do(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, request.Method, c.server.URL+mustPath(request.URL), bytes.NewReader(request.Body))
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	req.Header = request.Headers.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body.Bytes()}, nil
}

func (c *fakeHostClient) DoStream(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := c.Do(ctx, request)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, 1)
	chunks <- pluginapi.HTTPStreamChunk{Payload: resp.Body}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Chunks: chunks}, nil
}

// mustPath extracts the path+query portion of an absolute URL.
func mustPath(raw string) string {
	idx := strings.Index(raw, "/v1/")
	if idx < 0 {
		return raw
	}
	return raw[idx:]
}

func TestExecuteNonStreamStripsPrefixAndSendsBearer(t *testing.T) {
	var gotAuth, gotModel, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	client := &fakeHostClient{server: server}
	storage, _ := json.Marshal(authStorage{APIKey: "sk-bai-test-key-1234567890abcd", Provider: ProviderBAI})
	payload, _ := json.Marshal(map[string]any{"model": "bai-claude-opus-4.8", "messages": []any{}})
	reqBytes, _ := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "bai-test",
			AuthProvider: ProviderBAI,
			Model:        "bai-claude-opus-4.8",
			Format:       "anthropic",
			StorageJSON:  storage,
			Payload:      payload,
			HTTPClient:   client,
		},
	})

	// Route through handleMethod so the envelope logic is covered too.
	// MethodExecutorExecute unmarshals into rpcExecutorRequest; the
	// pluginapi.ExecutorRequest embed is json-tag-less, so field names
	// serialize as-is ("HTTPClient" is an interface and ignored on marshal,
	// which is why the client is injected through a package-level hook).
	prev := executorClientHook
	executorClientHook = func(req pluginapi.ExecutorRequest) pluginapi.HostHTTPClient { return client }
	defer func() { executorClientHook = prev }()

	out, err := handleMethod("executor.execute", reqBytes)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("envelope=%+v err=%v", env, err)
	}
	var resp pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Payload), `"content":"ok"`) {
		t.Fatalf("payload=%s", resp.Payload)
	}
	if gotAuth != "Bearer sk-bai-test-key-1234567890abcd" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotModel != "claude-opus-4.8" {
		t.Fatalf("model=%q", gotModel)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path=%q", gotPath)
	}
}

func TestTakeSSEFramesReframesMixedChunks(t *testing.T) {
	// Chunk boundaries cut an event in half; frames must stay intact.
	frames, rest := takeSSEFrames([]byte("event: message_start\ndata: {\"a\":1}\n\nevent: content_block\ndata: {\"b\":2"), false)
	if len(frames) != 1 || !strings.Contains(string(frames[0]), `"a":1`) {
		t.Fatalf("frames=%q", frames)
	}
	if !strings.Contains(string(rest), `"b":2`) {
		t.Fatalf("rest=%q", rest)
	}
	frames, _ = takeSSEFrames(append([]byte("data: "), append(rest, []byte("}\n\n")...)...), true)
	if len(frames) != 1 || !strings.Contains(string(frames[0]), `"b":2`) {
		t.Fatalf("frames=%q", frames)
	}
}

func TestTakeSSEFramesDoesNotDoublePrefixCompleteDataLines(t *testing.T) {
	// Chunks carry the raw data VALUE; the host wraps them in its own
	// "data: " line when relaying to the client.
	frames, rest := takeSSEFrames([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), false)
	if len(frames) != 1 {
		t.Fatalf("frames=%q", frames)
	}
	if rest != nil && len(rest) != 0 {
		t.Fatalf("rest=%q", rest)
	}
	want := "{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	if string(frames[0]) != want {
		t.Fatalf("frame=%q want=%q", frames[0], want)
	}
}

func TestBAIModelsRegistered(t *testing.T) {
	models := baiModels()
	found := false
	for _, m := range models {
		if m.ID == "bai-claude-opus-4.8" {
			found = true
		}
		if !strings.HasPrefix(m.ID, "bai-") {
			t.Fatalf("model id missing bai- prefix: %q", m.ID)
		}
	}
	if !found {
		t.Fatal("bai-claude-opus-4.8 not registered")
	}
}
