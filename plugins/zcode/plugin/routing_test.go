package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	testZAIAnthropic = "https://api.z.ai/api/anthropic/v1/messages"
	testZAIUltra     = "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"
	testBigModel     = "https://open.bigmodel.cn/api/anthropic/v1/messages"
)

func okConfigBody(mappingJSON string) []byte {
	return []byte(`{"code":0,"data":{"proxyEndpoint":{"mapping":` + mappingJSON + `}}}`)
}

func TestRoutingConstants(t *testing.T) {
	if routingSuccessTTL != 5*time.Minute {
		t.Fatalf("routingSuccessTTL=%v", routingSuccessTTL)
	}
	if routingFailureCooldown != 30*time.Second {
		t.Fatalf("routingFailureCooldown=%v", routingFailureCooldown)
	}
	if routingRequestTimeout != 3*time.Second {
		t.Fatalf("routingRequestTimeout=%v", routingRequestTimeout)
	}
	if routingMaxMapping != 256 {
		t.Fatalf("routingMaxMapping=%d", routingMaxMapping)
	}
}

func TestRoutingKeyNormalizesHostAndPort(t *testing.T) {
	cases := []struct {
		raw string
		key string
		ok  bool
	}{
		{"HTTPS://Api.Z.AI:443/api/anthropic/v1/messages/", "https://api.z.ai:443/api/anthropic/v1/messages", true},
		{"https://zcode.z.ai/api/v1/ultra/test", "https://zcode.z.ai:443/api/v1/ultra/test", true},
		{"https://api.z.ai:8443/api/", "https://api.z.ai:8443/api", true},
		{"https://api.z.ai/api/", "https://api.z.ai:443/api", true},
		{"https://api.z.ai/", "https://api.z.ai:443/", true},
		{"https://api.z.ai", "https://api.z.ai:443/", true},
		{"http://api.z.ai/x", "", false},
		{"://bad", "", false},
		{"not-a-url", "", false},
	}
	for _, c := range cases {
		got, ok := routingKey(c.raw)
		if ok != c.ok || got != c.key {
			t.Errorf("routingKey(%q) = (%q, %v), want (%q, %v)", c.raw, got, ok, c.key, c.ok)
		}
	}
}

func TestNormalizeRoutingPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/api/v1/", "/api/v1"},
		{"/api/v1///", "/api/v1"},
		{"/path", "/path"},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := normalizeRoutingPath(c.in); got != c.want {
			t.Errorf("normalizeRoutingPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidPlainHTTPS(t *testing.T) {
	if !validPlainHTTPS("https://api.z.ai/path") {
		t.Fatal("valid https should pass")
	}
	if validPlainHTTPS("http://api.z.ai/path") {
		t.Fatal("http must fail")
	}
	if validPlainHTTPS("https://user:pass@api.z.ai/path") {
		t.Fatal("userinfo must fail")
	}
	if validPlainHTTPS("https://api.z.ai/path?q=1") {
		t.Fatal("query must fail")
	}
	if validPlainHTTPS("https://api.z.ai/path#frag") {
		t.Fatal("fragment must fail")
	}
}

func TestParseRoutingMapping(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		body := okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"` + testZAIUltra + `"}]`)
		m, err := parseRoutingMapping(body)
		if err != nil {
			t.Fatal(err)
		}
		key, _ := routingKey(testZAIAnthropic)
		if got := m[key]; got != testZAIUltra {
			t.Fatalf("mapped=%q", got)
		}
	})
	t.Run("empty mapping", func(t *testing.T) {
		m, err := parseRoutingMapping(okConfigBody(`[]`))
		if err != nil || len(m) != 0 {
			t.Fatalf("m=%v err=%v", m, err)
		}
	})
	t.Run("code != 0", func(t *testing.T) {
		body := []byte(`{"code":5,"data":{"proxyEndpoint":{"mapping":[]}}}`)
		_, err := parseRoutingMapping(body)
		if err == nil {
			t.Fatal("expected error for non-zero code")
		}
	})
	t.Run("code string 0", func(t *testing.T) {
		body := []byte(`{"code":"0","data":{"proxyEndpoint":{"mapping":[]}}}`)
		_, err := parseRoutingMapping(body)
		if err != nil {
			t.Fatalf("string code 0 should be accepted: %v", err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		_, err := parseRoutingMapping([]byte(`not-json`))
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("duplicate from", func(t *testing.T) {
		body := okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"` + testZAIUltra + `"},{"from":"` + testZAIAnthropic + `","to":"` + testBigModel + `"}]`)
		_, err := parseRoutingMapping(body)
		if err == nil {
			t.Fatal("expected error for duplicate from")
		}
	})
	t.Run("non-https from", func(t *testing.T) {
		body := okConfigBody(`[{"from":"http://api.z.ai/x","to":"https://api.z.ai/y"}]`)
		_, err := parseRoutingMapping(body)
		if err == nil {
			t.Fatal("expected error for non-https from")
		}
	})
	t.Run("non-https to", func(t *testing.T) {
		body := okConfigBody(`[{"from":"https://api.z.ai/x","to":"http://api.z.ai/y"}]`)
		_, err := parseRoutingMapping(body)
		if err == nil {
			t.Fatal("expected error for non-https to")
		}
	})
	t.Run("disallowed target host", func(t *testing.T) {
		body := okConfigBody(`[{"from":"https://api.z.ai/x","to":"https://evil.example.com/y"}]`)
		_, err := parseRoutingMapping(body)
		if err == nil {
			t.Fatal("expected error for disallowed host")
		}
	})
	t.Run("max entries enforced", func(t *testing.T) {
		var entries []map[string]string
		for i := 0; i < routingMaxMapping+1; i++ {
			entries = append(entries, map[string]string{
				"from": "https://api.z.ai/x" + string(rune('a'+i%26)),
				"to":   "https://zcode.z.ai/y" + string(rune('a'+i%26)),
			})
		}
		raw, _ := json.Marshal(map[string]any{
			"code": 0,
			"data": map[string]any{
				"proxyEndpoint": map[string]any{"mapping": entries},
			},
		})
		_, err := parseRoutingMapping(raw)
		if err == nil {
			t.Fatal("expected error for >256 entries")
		}
	})
	t.Run("exactly 256 entries ok", func(t *testing.T) {
		var entries []map[string]string
		for i := 0; i < routingMaxMapping; i++ {
			entries = append(entries, map[string]string{
				"from": "https://api.z.ai/x" + string(rune('a'+(i%26))),
				"to":   "https://zcode.z.ai/y" + string(rune('a'+(i%26))),
			})
		}
		// ensure unique keys
		type entry struct{ From, To string }
		es := make([]entry, routingMaxMapping)
		for i := range es {
			es[i].From = "https://api.z.ai/path/" + string(rune('0'+i%10)) + string(rune('a'+i/10))
			es[i].To = "https://zcode.z.ai/target/" + string(rune('0'+i%10))
		}
		raw, _ := json.Marshal(map[string]any{
			"code": 0,
			"data": map[string]any{
				"proxyEndpoint": map[string]any{"mapping": es},
			},
		})
		m, err := parseRoutingMapping(raw)
		if err != nil {
			t.Fatalf("expected 256 entries to be ok: %v", err)
		}
		if len(m) != routingMaxMapping {
			t.Fatalf("got %d entries, want %d", len(m), routingMaxMapping)
		}
	})
}

// ---- endpointRouting tests ----

func TestResolveRewritesMappedURLPreservingQuery(t *testing.T) {
	r := newEndpointRouting(routingActiveMode)
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"` + testZAIUltra + `"}]`)}, nil
	}
	ctx := context.Background()
	got := r.resolve(ctx, nil, "", testZAIAnthropic+"?beta=true")
	if !got.Routed {
		t.Fatal("expected routed")
	}
	if got.URL != testZAIUltra+"?beta=true" {
		t.Fatalf("url=%q, want %q?beta=true", got.URL, testZAIUltra)
	}
}

func TestResolveReturnsOriginalForUnmatched(t *testing.T) {
	r := newEndpointRouting(routingActiveMode)
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"` + testZAIUltra + `"}]`)}, nil
	}
	ctx := context.Background()
	for _, url := range []string{
		"https://api.z.ai/api/coding/paas/v4/chat/completions",
		"https://open.bigmodel.cn/api/anthropic/v1/messages",
		"https://zcode.z.ai/api/v1/zcode-plan/chat/completions",
	} {
		got := r.resolve(ctx, nil, "", url)
		if got.Routed {
			t.Fatalf("unexpected route for %s: %s", url, got.URL)
		}
	}
}

func TestResolveMatchesTrailingSlashFrom(t *testing.T) {
	r := newEndpointRouting(routingActiveMode)
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: okConfigBody(`[{"from":"` + testZAIAnthropic + `/","to":"` + testZAIUltra + `"}]`)}, nil
	}
	ctx := context.Background()
	got := r.resolve(ctx, nil, "", testZAIAnthropic)
	if !got.Routed || got.URL != testZAIUltra {
		t.Fatalf("trailing-slash from match failed: %+v", got)
	}
}

func TestResolveFailsOpenOnNetworkError(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	calls := 0
	r := newEndpointRouting(routingActiveMode)
	r.now = func() time.Time { return clock }
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		return pluginapi.HTTPResponse{}, errors.New("network down")
	}
	ctx := context.Background()
	got := r.resolve(ctx, nil, "", testZAIAnthropic)
	if got.Routed || got.URL != testZAIAnthropic {
		t.Fatalf("fail-open violated: %+v", got)
	}
	_ = r.resolve(ctx, nil, "", testZAIAnthropic)
	if calls != 1 {
		t.Fatalf("cooldown should prevent refetch, calls=%d", calls)
	}
	clock = clock.Add(routingFailureCooldown + time.Second)
	_ = r.resolve(ctx, nil, "", testZAIAnthropic)
	if calls != 2 {
		t.Fatalf("expected refetch after cooldown, calls=%d", calls)
	}
}

func TestResolveFailsOpenOnMalformedEnvelope(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{`{"code":5,"data":{}}`, `{"code":0}`, `not-json-at-all`} {
		r := newEndpointRouting(routingActiveMode)
		r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(bad)}, nil
		}
		got := r.resolve(ctx, nil, "", testZAIAnthropic)
		if got.Routed {
			t.Fatalf("bad body %q should fail-open: %+v", bad, got)
		}
	}
}

func TestResolveCachesSuccessfulSnapshot(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	calls := 0
	r := newEndpointRouting(routingActiveMode)
	r.now = func() time.Time { return clock }
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		body := okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"` + testZAIUltra + `"}]`)
		if calls > 1 {
			body = okConfigBody(`[]`)
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
	}
	ctx := context.Background()
	got := r.resolve(ctx, nil, "", testZAIAnthropic)
	if !got.Routed {
		t.Fatal("first call should route")
	}
	clock = clock.Add(routingSuccessTTL - time.Second)
	got = r.resolve(ctx, nil, "", testZAIAnthropic)
	if !got.Routed {
		t.Fatal("cached snapshot should still be fresh")
	}
	clock = clock.Add(2 * time.Second)
	got = r.resolve(ctx, nil, "", testZAIAnthropic)
	if got.Routed {
		t.Fatal("stale snapshot should refresh to empty mapping")
	}
	if calls != 2 {
		t.Fatalf("expected 2 refresh calls, got %d", calls)
	}
}

func TestResolveDedupesConcurrentRefreshes(t *testing.T) {
	calls := 0
	r := newEndpointRouting(routingActiveMode)
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		time.Sleep(20 * time.Millisecond)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"` + testZAIUltra + `"}]`)}, nil
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]routedURL, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = r.resolve(ctx, nil, "", testZAIAnthropic)
		}(i)
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("expected single refresh, calls=%d", calls)
	}
	for i := range results {
		if !results[i].Routed {
			t.Fatalf("result %d not routed", i)
		}
	}
}

func TestObserveModeRecordsWithoutSwitching(t *testing.T) {
	r := newEndpointRouting(routingObserveMode) // default
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"` + testZAIUltra + `"}]`)}, nil
	}
	ctx := context.Background()
	got := r.resolve(ctx, nil, "", testZAIAnthropic)
	if got.Routed || got.URL != testZAIAnthropic {
		t.Fatalf("observe mode must not switch: %+v", got)
	}
	st := r.status()
	if len(st.Observed) != 1 || st.Observed[0].From != testZAIAnthropic || st.Observed[0].To != testZAIUltra {
		t.Fatalf("observe mode did not record: %+v", st.Observed)
	}
	// switching to active mode enables routing with the same snapshot
	if err := r.setMode(routingActiveMode); err != nil {
		t.Fatal(err)
	}
	got = r.resolve(ctx, nil, "", testZAIAnthropic)
	if !got.Routed || got.URL != testZAIUltra {
		t.Fatalf("active mode should switch: %+v", got)
	}
}

func TestResolveRejectsDisallowedTargetHost(t *testing.T) {
	r := newEndpointRouting(routingActiveMode)
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"https://evil.example.com/y"}]`)}, nil
	}
	ctx := context.Background()
	got := r.resolve(ctx, nil, "", testZAIAnthropic)
	if got.Routed {
		t.Fatal("disallowed target host should fail-open (no route)")
	}
	st := r.status()
	if st.Snapshot {
		t.Fatal("snapshot must be nil after target rejection")
	}
}

func TestConfigFetchHeaders(t *testing.T) {
	var seen http.Header
	r := newEndpointRouting(routingObserveMode)
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		seen = req.Headers
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{}}`)}, nil
	}
	ctx := context.Background()
	_ = r.resolve(ctx, nil, "", testZAIAnthropic)
	if seen.Get("User-Agent") != "ZCode/"+zcodeClientVersion {
		t.Fatalf("User-Agent=%q", seen.Get("User-Agent"))
	}
	if seen.Get("X-Platform") != "linux-x64" {
		t.Fatalf("X-Platform=%q", seen.Get("X-Platform"))
	}
	if seen.Get("Accept") != "application/json" {
		t.Fatalf("Accept=%q", seen.Get("Accept"))
	}
	if seen.Get("X-ZCode-Agent") != "" {
		t.Fatalf("X-ZCode-Agent must be stripped on config fetch, got %q", seen.Get("X-ZCode-Agent"))
	}
	if seen.Get("Authorization") != "" {
		t.Fatalf("config fetch must not carry credentials: %q", seen.Get("Authorization"))
	}
}

func TestLookupReadOnly(t *testing.T) {
	r := newEndpointRouting(routingActiveMode)
	r.httpDo = func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: okConfigBody(`[{"from":"` + testZAIAnthropic + `","to":"` + testZAIUltra + `"}]`)}, nil
	}
	ctx := context.Background()
	_ = r.resolve(ctx, nil, "", testZAIAnthropic)
	if to, ok := r.lookup(testBigModel); ok {
		t.Fatalf("unmapped endpoint should not be found: %q", to)
	}
	if to, ok := r.lookup(testZAIAnthropic); !ok || to != testZAIUltra {
		t.Fatalf("mapped endpoint lookup failed: %q %v", to, ok)
	}
}

func TestSetModeRejectsInvalid(t *testing.T) {
	if err := setEndpointRoutingMode("invalid"); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestEndpointRouterDefaultsToObserve(t *testing.T) {
	if endpointRoutingMode() != routingObserveMode {
		t.Fatal("default endpointRouter must be in observe mode")
	}
}

func TestApplyRoutingQuery(t *testing.T) {
	cases := []struct {
		target, raw, want string
	}{
		{testZAIUltra, testZAIAnthropic + "?beta=true", testZAIUltra + "?beta=true"},
		{testZAIUltra, testZAIAnthropic, testZAIUltra},
		{testZAIUltra + "?existing=1", testZAIAnthropic + "?beta=true", testZAIUltra + "?beta=true"},
	}
	for _, c := range cases {
		got := applyRoutingQuery(c.target, c.raw)
		if got != c.want {
			t.Errorf("applyRoutingQuery(%q, %q) = %q, want %q", c.target, c.raw, got, c.want)
		}
	}
}
