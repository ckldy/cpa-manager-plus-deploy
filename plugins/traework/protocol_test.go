package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBuildSOLOPayloadStripsProviderNamespace(t *testing.T) {
	out, err := BuildSOLOPayload([]byte(`{"model":"traework-glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "glm-5.2" || got["config_name"] != "glm-5.2" {
		t.Fatalf("upstream model namespace not stripped: %#v", got)
	}
}

func TestBuildSOLOPayloadResolvesDisplayNameIDToConfigName(t *testing.T) {
	// With a registered model whose ID is the friendly display name, the payload
	// must resolve back to the upstream config_name, not the display name.
	old := modelCache.models
	modelCache.Lock()
	modelCache.models = []pluginapi.ModelInfo{newTraeModel("glm-5.2", "GLM-5.2", time.Unix(1, 0))}
	modelCache.Unlock()
	defer func() {
		modelCache.Lock()
		modelCache.models = old
		modelCache.Unlock()
	}()
	out, err := BuildSOLOPayload([]byte(`{"model":"traework-GLM-5.2","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "glm-5.2" || got["config_name"] != "glm-5.2" {
		t.Fatalf("display-name ID not resolved to config_name: %#v", got)
	}
}

func TestPrepareBodyTextAndTools(t *testing.T) {
	src := []byte(`{"model":" solo-model ","stream":false,"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(src), &got); err != nil {
		t.Fatal(err)
	}
	if got["stream"] != true || got["function"] != SOLOFunction || got["model"] != "solo-model" || got["config_name"] != "solo-model" {
		t.Fatalf("unexpected envelope: %#v", got)
	}
	msgs := got["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || content["text"] != "hello" {
		t.Fatalf("unexpected content: %#v", content)
	}
	calls := msgs[1].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["function_call"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("unexpected calls: %#v", calls)
	}
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["parameters"] != `{"type":"object"}` || got["tool_choice"] != "lookup" {
		t.Fatalf("unexpected tools: %#v choice=%#v", fn, got["tool_choice"])
	}
}

func TestBuildSOLOPayloadRejectsInvalidEnvelope(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"model":`),
		[]byte(`{"messages":[]}`),
		[]byte(`{"model":"m"}`),
		[]byte(`{"model":" ","messages":[]}`),
		[]byte(`{"model":"m","messages":{}}`),
	}
	for _, src := range cases {
		if out, err := BuildSOLOPayload(src); err == nil || out != nil {
			t.Fatalf("BuildSOLOPayload(%q) = %q, %v; want nil,error", src, out, err)
		}
		if got := PrepareBody(src); got != nil {
			t.Fatalf("PrepareBody(%q) = %q; invalid input must not pass through", src, got)
		}
	}
}

func TestBuildSOLOPayloadRejectsNamelessTools(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"{}"}}]}]}`),
		[]byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"parameters":{"type":"object"}}}]}`),
		[]byte(`{"model":"m","messages":[],"tools":["bad"]}`),
	}
	for _, src := range cases {
		if _, err := BuildSOLOPayload(src); err == nil {
			t.Fatalf("BuildSOLOPayload(%q) unexpectedly succeeded", src)
		}
	}
}

func TestPrepareBodyToolChoiceNoneAndInvalidJSON(t *testing.T) {
	src := []byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"x"}}],"functions":[{}],"tool_choice":"none"}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(src), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["tools"]; ok {
		t.Fatal("tools must be suppressed")
	}
	if _, ok := got["functions"]; ok {
		t.Fatal("functions must be suppressed")
	}
	if _, ok := got["tool_choice"]; ok {
		t.Fatal("tool_choice must be suppressed")
	}
	bad := []byte(`{"model":`)
	if PrepareBody(bad) != nil {
		t.Fatal("invalid JSON must not pass through")
	}
}

func TestPrepareBodyPreservesArrayContent(t *testing.T) {
	src := []byte(`{"model":"model-x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(src), &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "model-x" || got["config_name"] != "model-x" {
		t.Fatalf("model missing: %#v", got)
	}
	blocks := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("array content changed: %#v", blocks)
	}
}
