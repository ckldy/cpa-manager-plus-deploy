package main

import (
	"encoding/json"
	"testing"
)

func TestOpenAIMessageContentShapes(t *testing.T) {
	var msgs []openAIMessage
	in := []byte(`[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"assistant","content":null},
		{"role":"user","content":[{"type":"text","text":"part"}]}
	]`)
	if err := json.Unmarshal(in, &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"hi", "", "", "part"}
	for i, w := range want {
		if msgs[i].Content != w {
			t.Fatalf("msg %d content=%q want %q", i, msgs[i].Content, w)
		}
	}
}

func TestOpenAIMessagePreservesToolHistory(t *testing.T) {
	// Tool history must survive unmarshal + buildQoderBody so the upstream
	// gateway can deserialize the conversation. Dropping tool_call_id made
	// every multi-turn agent request fail with provider_error
	// "missing field `tool_call_id`" → empty_stream.
	var msgs []openAIMessage
	in := []byte(`[
		{"role":"user","content":"do it"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"/tmp/x\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"result ok"},
		{"role":"user","content":"thanks"}
	]`)
	if err := json.Unmarshal(in, &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msgs[1].Content != "" {
		t.Fatalf("assistant tool_calls turn content=%q want empty", msgs[1].Content)
	}
	if len(msgs[1].ToolCalls) == 0 {
		t.Fatal("assistant tool_calls turn lost tool_calls")
	}
	if msgs[2].ToolCallID != "call_1" {
		t.Fatalf("tool msg tool_call_id=%q want call_1", msgs[2].ToolCallID)
	}

	body, err := buildQoderBody(&openAIRequest{Model: "auto", Messages: msgs}, "auto", "personal_standard")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	var decoded struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode built body: %v", err)
	}
	// Find the tool role message appended after template system messages and
	// assert it carries tool_call_id.
	var toolMsg map[string]json.RawMessage
	for _, m := range decoded.Messages {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role == "tool" {
			toolMsg = m
			break
		}
	}
	if toolMsg == nil {
		t.Fatal("built body has no tool message")
	}
	var tid string
	if err := json.Unmarshal(toolMsg["tool_call_id"], &tid); err != nil || tid != "call_1" {
		t.Fatalf("built tool message tool_call_id missing or wrong (err=%v, val=%q)", err, tid)
	}
	// And the assistant tool_calls turn must retain tool_calls.
	var asstMsg map[string]json.RawMessage
	for _, m := range decoded.Messages {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role == "assistant" && len(m["tool_calls"]) > 0 && string(m["tool_calls"]) != "null" {
			asstMsg = m
			break
		}
	}
	if asstMsg == nil {
		t.Fatal("built body lost assistant tool_calls")
	}
}

func TestBuildQoderBodyForwardsClientTools(t *testing.T) {
	// When a client declares its own tools, the built upstream body must carry
	// those tools (replacing the template's baked-in Qoder CLI tool list) so the
	// gateway returns tool_calls for the client's names — enabling the host to
	// execute them. Verified live: replacing tools with a client get_weather
	// schema makes the model emit tool_calls for get_weather (200).
	clientTools := []byte(`[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`)
	clientChoice := []byte(`{"type":"function","function":{"name":"get_weather"}}`)
	msgs := []openAIMessage{{Role: "user", Content: "weather in Shanghai"}}
	body, err := buildQoderBody(&openAIRequest{
		Model:      "dfmodel",
		Messages:   msgs,
		Tools:      clientTools,
		ToolChoice: clientChoice,
	}, "dfmodel", "personal_standard")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	var decoded struct {
		Tools      []map[string]json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage              `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode built body: %v", err)
	}
	if len(decoded.Tools) != 1 {
		t.Fatalf("built body tools count=%d want 1", len(decoded.Tools))
	}
	var fn struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(decoded.Tools[0]["function"], &fn.Function); err != nil {
		t.Fatalf("decode tools[0].function: %v", err)
	}
	if fn.Function.Name != "get_weather" {
		t.Fatalf("built tools[0].function.name=%q want get_weather", fn.Function.Name)
	}
	if len(decoded.ToolChoice) == 0 {
		t.Fatal("built body missing tool_choice")
	}
}

func TestBuildQoderBodyLeavesBuiltinToolsWhenNoClientTools(t *testing.T) {
	// Without client tools, the template's built-in tool list must remain
	// (baseline behaviour — the model uses Qoder's own CLI tools).
	msgs := []openAIMessage{{Role: "user", Content: "hi"}}
	body, err := buildQoderBody(&openAIRequest{Model: "auto", Messages: msgs}, "auto", "personal_standard")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	var decoded struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode built body: %v", err)
	}
	if len(decoded.Tools) == 0 {
		t.Fatal("expected built-in template tools when no client tools sent")
	}
}
