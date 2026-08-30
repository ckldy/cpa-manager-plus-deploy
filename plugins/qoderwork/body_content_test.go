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
