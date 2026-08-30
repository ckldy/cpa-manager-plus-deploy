package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestClassifyProbeResponse(t *testing.T) {
	tests := []struct {
		name string
		resp pluginapi.HTTPResponse
		want probeClass
	}{
		{"success", pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"choices":[{"message":{"content":"ok"}}]}`)}, probeFree},
		{"deposit", pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"error":{"code":"access_denied","message":"Deposit required to unlock premium models"}}`)}, probePremium},
		{"rate limit", pluginapi.HTTPResponse{StatusCode: 429}, probeUnknown},
		{"server error", pluginapi.HTTPResponse{StatusCode: 503}, probeUnknown},
		{"invalid success", pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"choices":[]}`)}, probeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyProbeResponse(tt.resp); got != tt.want {
				t.Fatalf("got=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestApplyProbeResultKeepsReliableOldValueOnUnknown(t *testing.T) {
	old := probeEntry{Class: probeFree, CheckedAt: time.Unix(10, 0)}
	got := applyProbeResult(old, probeUnknown, time.Unix(20, 0))
	if got.Class != probeFree || !got.CheckedAt.Equal(old.CheckedAt) {
		t.Fatalf("got=%+v", got)
	}
	got = applyProbeResult(old, probePremium, time.Unix(30, 0))
	if got.Class != probePremium || got.CheckedAt.Unix() != 30 {
		t.Fatalf("got=%+v", got)
	}
}

func TestProbeCacheFreshness(t *testing.T) {
	now := time.Unix(100000, 0)
	if probeCacheFresh(now, now.Add(-23*time.Hour)) != true {
		t.Fatal("23h cache should be fresh")
	}
	if probeCacheFresh(now, now.Add(-25*time.Hour)) != false {
		t.Fatal("25h cache should be stale")
	}
	if probeCacheFresh(now, time.Time{}) != false {
		t.Fatal("zero completion time should be stale")
	}
}

func TestFreeModelsHaveFreeSuffix(t *testing.T) {
	resetProbeStateForTest()
	models := baiModels()
	for _, m := range models {
		if m.ID == "bai-glm-5.3-flash" {
			if !strings.HasSuffix(m.DisplayName, " (free)") {
				t.Fatalf("display=%q", m.DisplayName)
			}
			return
		}
	}
	t.Fatal("seed free model missing")
}

func TestPremiumAndUnknownModelsDoNotHaveFreeSuffix(t *testing.T) {
	resetProbeStateForTest()
	setProbeClassForTest("claude-opus-4.8", probePremium)
	for _, m := range baiModels() {
		if m.ID == "bai-claude-opus-4.8" && strings.HasSuffix(m.DisplayName, " (free)") {
			t.Fatalf("premium display=%q", m.DisplayName)
		}
	}
}

func TestProbePayloadIsMinimalNonStreaming(t *testing.T) {
	body := makeProbePayload("glm-5.3-flash")
	text := string(body)
	for _, want := range []string{`"model":"glm-5.3-flash"`, `"stream":false`, `"max_tokens":16`} {
		if !strings.Contains(text, want) {
			t.Fatalf("payload=%s missing %s", text, want)
		}
	}
}

func TestClassifyProbeResponseAcceptsHTTPConstants(t *testing.T) {
	resp := pluginapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(`{"error":{"code":"access_denied"}}`)}
	if got := classifyProbeResponse(resp); got != probePremium {
		t.Fatalf("got=%q", got)
	}
}
