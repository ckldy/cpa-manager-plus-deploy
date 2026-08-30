package main

import (
	"encoding/json"
	"testing"
)

func lifecycleConfig(yaml string) []byte {
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
	return raw
}
func dualStorage() []byte {
	raw, _ := json.Marshal(authStorage{ZCodeJWTToken: "jwt", APIKey: "test-api-key"})
	return raw
}

func TestRouteConfigDefaultsDoNotAllowPaidFallback(t *testing.T) {
	applyRouteConfig(lifecycleConfig("enabled: true\n"))
	got := currentRouteConfig()
	if got.Mode != "free-first" || got.AllowPaidFallback {
		t.Fatalf("unsafe defaults: %+v", got)
	}
}
func TestStrictRoutesNeverFallback(t *testing.T) {
	for _, route := range []string{"coding-plan", "api-key"} {
		applyRouteConfig(lifecycleConfig("route_mode: strict\nstrict_route: " + route + "\n"))
		got, err := resolveCredentialCandidates(dualStorage(), "a")
		if err != nil || len(got) != 1 {
			t.Fatalf("route=%s got=%+v err=%v", route, got, err)
		}
		if (route == "coding-plan") != got[0].CodingPlan {
			t.Fatalf("wrong strict route: %s", route)
		}
	}
}
func TestFreeFirstRequiresExplicitPaidFallback(t *testing.T) {
	zero := 0.0
	zcodeQuotas.Lock()
	zcodeQuotas.items["a"] = zcodeQuotaSnapshot{Remaining: &zero}
	zcodeQuotas.Unlock()
	applyRouteConfig(lifecycleConfig("route_mode: free-first\nallow_paid_fallback: false\n"))
	if _, err := resolveCredentialCandidates(dualStorage(), "a"); err == nil {
		t.Fatal("paid fallback occurred without consent")
	}
	applyRouteConfig(lifecycleConfig("route_mode: free-first\nallow_paid_fallback: true\n"))
	got, err := resolveCredentialCandidates(dualStorage(), "a")
	if err != nil || len(got) != 1 || got[0].CodingPlan {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
func TestPaidFirstFallsBackOnlyToCodingPlan(t *testing.T) {
	applyRouteConfig(lifecycleConfig("route_mode: paid-first\n"))
	got, err := resolveCredentialCandidates(dualStorage(), "a")
	if err != nil || len(got) != 2 || got[0].CodingPlan || !got[1].CodingPlan {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
func TestFallbackClassificationIsNarrow(t *testing.T) {
	if mayFallback(500, []byte("server")) || mayFallback(403, []byte("captcha verify")) || mayFallback(429, []byte("rate limit")) {
		t.Fatal("unsafe fallback classification")
	}
	if !mayFallback(429, []byte(`{"code":1113,"message":"insufficient balance"}`)) || !mayFallback(401, []byte("unauthorized")) {
		t.Fatal("expected explicit fallback class")
	}
}

func TestResolveBigModelCodingKeyIsPlatformIsolated(t *testing.T) {
	raw, _ := json.Marshal(authStorage{Provider: "bigmodel", APIKey: "test-bigmodel-key", Source: "coding-plan-key"})
	got, err := resolveCredentialCandidates(raw, "bm")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != bigModelCodingUpstreamURL || !got[0].CodingPlan {
		t.Fatalf("unexpected BigModel route: %#v", got)
	}
	if got[0].Headers.Get("x-api-key") == "" {
		t.Fatal("BigModel key header missing")
	}
}

func TestJWTIdentityHeadersAreUsedByCredentialMainPath(t *testing.T) {
	raw, _ := json.Marshal(authStorage{
		ZCodeJWTToken:       "jwt",
		CaptchaVerifyParam:  "captcha",
		CaptchaVerifyRegion: "sgp",
		DeviceMID:           "persisted-mid",
	})
	got, err := resolveCredentialCandidates(raw, "identity")
	if err != nil {
		t.Fatal(err)
	}
	h := got[0].Headers
	if h.Get("Authorization") != "Bearer jwt" || h.Get("X-Aliyun-Captcha-Verify-Param") != "captcha" || h.Get("X-Aliyun-Captcha-Verify-Region") != "sgp" {
		t.Fatalf("authorization/captcha headers not preserved: %#v", h)
	}
	if h.Get("X-Device-Mid") != "persisted-mid" || h.Get("X-Platform") != "linux-x64" {
		t.Fatalf("identity headers not connected to credential path: %#v", h)
	}
}

func TestLegacyAPIKeyRemainsZai(t *testing.T) {
	raw, _ := json.Marshal(authStorage{APIKey: "test-api-key"})
	got, err := resolveCredentialCandidates(raw, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != apiKeyUpstreamURL {
		t.Fatalf("legacy key changed platform: %#v", got)
	}
}
