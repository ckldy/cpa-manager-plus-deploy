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
	raw, _ := json.Marshal(authStorage{ZCodeJWTToken: "jwt", APIKey: "api-key-12345678901234567890"})
	return raw
}

func TestRouteConfigDefaultsDoNotAllowPaidFallback(t *testing.T) {
	applyRouteConfig(lifecycleConfig("enabled: true\n"))
	got := currentRouteConfig()
	if got.Mode != "auto" || got.AllowPaidFallback {
		t.Fatalf("unsafe defaults: %+v", got)
	}
}

func TestPlanRouteModesAreParsed(t *testing.T) {
	for _, mode := range []string{"auto", "coding-plan", "start-plan"} {
		applyRouteConfig(lifecycleConfig("route_mode: " + mode + "\n"))
		if got := currentRouteConfig().Mode; got != mode {
			t.Fatalf("route_mode=%s parsed as %s", mode, got)
		}
	}
}

func TestStartPlanRequiresJWTAndNeverFallsBackWithoutConsent(t *testing.T) {
	applyRouteConfig(lifecycleConfig("route_mode: start-plan\nallow_paid_fallback: false\n"))
	got, err := resolveCredentialCandidates(dualStorage(), "trial")
	if err != nil || len(got) != 1 || !got[0].CodingPlan {
		t.Fatalf("unexpected start-plan route: got=%+v err=%v", got, err)
	}

	onlyKey, _ := json.Marshal(authStorage{APIKey: "api-key-12345678901234567890"})
	if _, err := resolveCredentialCandidates(onlyKey, "trial"); err == nil {
		t.Fatal("start-plan silently fell back to a paid API key")
	}
}

func TestCodingPlanSelectsKeyCredential(t *testing.T) {
	applyRouteConfig(lifecycleConfig("route_mode: coding-plan\n"))
	got, err := resolveCredentialCandidates(dualStorage(), "coding")
	if err != nil || len(got) != 1 || got[0].CodingPlan {
		t.Fatalf("unexpected coding-plan route: got=%+v err=%v", got, err)
	}
}
func TestLegacyStrictRoutesMapToPlanModes(t *testing.T) {
	cases := []struct {
		strictRoute string
		wantMode    string
	}{
		{strictRoute: "coding-plan", wantMode: "start-plan"},
		{strictRoute: "api-key", wantMode: "coding-plan"},
	}
	for _, tc := range cases {
		applyRouteConfig(lifecycleConfig("strict_route: " + tc.strictRoute + "\nroute_mode: strict\n"))
		if got := currentRouteConfig().Mode; got != tc.wantMode {
			t.Fatalf("legacy strict_route=%s mapped to %s, want %s", tc.strictRoute, got, tc.wantMode)
		}
	}
}
func TestAutoRequiresExplicitPaidFallbackAfterTrialExhaustion(t *testing.T) {
	zero := 0.0
	zcodeQuotas.Lock()
	zcodeQuotas.items["a"] = zcodeQuotaSnapshot{Remaining: &zero}
	zcodeQuotas.Unlock()
	applyRouteConfig(lifecycleConfig("route_mode: auto\nallow_paid_fallback: false\n"))
	if _, err := resolveCredentialCandidates(dualStorage(), "a"); err == nil {
		t.Fatal("paid fallback occurred without consent")
	}
	applyRouteConfig(lifecycleConfig("route_mode: auto\nallow_paid_fallback: true\n"))
	got, err := resolveCredentialCandidates(dualStorage(), "a")
	if err != nil || len(got) != 1 || got[0].CodingPlan {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
func TestLegacyPaidFirstMapsToCodingPlan(t *testing.T) {
	applyRouteConfig(lifecycleConfig("route_mode: paid-first\n"))
	if got := currentRouteConfig().Mode; got != "coding-plan" {
		t.Fatalf("legacy paid-first mapped to %s", got)
	}
	candidates, err := resolveCredentialCandidates(dualStorage(), "a")
	if err != nil || len(candidates) != 1 || candidates[0].CodingPlan {
		t.Fatalf("got=%+v err=%v", candidates, err)
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
	raw, _ := json.Marshal(authStorage{Provider: "bigmodel", APIKey: "bm-id.secret-value-1234567890", Source: "coding-plan-key"})
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
	applyRouteConfig(lifecycleConfig("route_mode: start-plan\n"))
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
	applyRouteConfig(lifecycleConfig("route_mode: auto\n"))
	raw, _ := json.Marshal(authStorage{APIKey: "api-key-12345678901234567890"})
	got, err := resolveCredentialCandidates(raw, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != apiKeyUpstreamURL {
		t.Fatalf("legacy key changed platform: %#v", got)
	}
}
