package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
)

func TestEnsureDeviceMIDPersistsAndReusesValue(t *testing.T) {
	storage := &authStorage{}
	if err := ensureDeviceMID(storage); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(storage.DeviceMID) {
		t.Fatalf("DeviceMID=%q", storage.DeviceMID)
	}
	first := storage.DeviceMID
	if err := ensureDeviceMID(storage); err != nil {
		t.Fatal(err)
	}
	if storage.DeviceMID != first {
		t.Fatalf("MID changed from %q to %q", first, storage.DeviceMID)
	}
	raw, err := json.Marshal(storage)
	if err != nil {
		t.Fatal(err)
	}
	var restored authStorage
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.DeviceMID != first {
		t.Fatalf("persisted MID=%q, want %q", restored.DeviceMID, first)
	}
}

func TestBuildClaimRequestsArePureAndUseStoredCredentials(t *testing.T) {
	storage := authStorage{
		ZCodeJWTToken:       "jwt-secret",
		DeviceMID:           "11111111-2222-4333-8444-555555555555",
		CaptchaVerifyParam:  "captcha-param",
		CaptchaVerifyRegion: "cn-shanghai",
	}
	preview, err := buildClaimPreviewRequest(storage)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Method != http.MethodGet || preview.URL != zcodeClaimPreviewURL {
		t.Fatalf("preview=%+v", preview)
	}
	if len(preview.Body) != 0 || preview.Headers.Get("Authorization") != "Bearer jwt-secret" || preview.Headers.Get("X-Device-Mid") != storage.DeviceMID {
		t.Fatalf("unexpected preview request: %+v", preview)
	}

	claim, err := buildClaimRequest(storage, " weekend-plan ")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Method != http.MethodPost || claim.URL != zcodeClaimURL || claim.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("claim=%+v", claim)
	}
	if claim.Headers.Get("X-Aliyun-Captcha-Verify-Param") != "captcha-param" || claim.Headers.Get("X-Aliyun-Captcha-Verify-Region") != "cn-shanghai" {
		t.Fatalf("claim captcha headers=%v", claim.Headers)
	}
	var body map[string]string
	if err := json.Unmarshal(claim.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["plan_id"] != "weekend-plan" {
		t.Fatalf("claim body=%s", claim.Body)
	}
}

func TestBuildClaimRequestRejectsMissingMaterial(t *testing.T) {
	for _, tc := range []struct {
		name    string
		storage authStorage
		planID  string
	}{
		{name: "jwt", storage: authStorage{DeviceMID: "mid"}, planID: "plan"},
		{name: "mid", storage: authStorage{ZCodeJWTToken: "jwt"}, planID: "plan"},
		{name: "plan", storage: authStorage{ZCodeJWTToken: "jwt", DeviceMID: "mid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildClaimRequest(tc.storage, tc.planID); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestParseClaimPreview(t *testing.T) {
	preview, err := parseClaimPreview([]byte(`{"code":0,"msg":"ok","data":{"eligible":true,"claimable":true,"plans":[{"plan_id":"weekend","name":"Weekend Trial"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Eligible || !preview.Claimable || len(preview.Plans) != 1 || preview.Plans[0].PlanID != "weekend" {
		t.Fatalf("preview=%+v", preview)
	}
	if _, err := parseClaimPreview([]byte(`not-json`)); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	if _, err := parseClaimPreview([]byte(`{"data":{}}`)); err == nil {
		t.Fatal("missing business code was accepted")
	}
}

func TestParseClaimResultBusinessCodes(t *testing.T) {
	cases := []struct {
		code     int
		category string
	}{
		{0, "success"},
		{1002, "activity_unavailable"},
		{1003, "already_claimed"},
		{1004, "ineligible"},
		{1005, "quota_exhausted"},
		{3001, "invalid_device_or_parameter"},
		{3007, "captcha_failed"},
		{9999, "unknown"},
	}
	for _, tc := range cases {
		raw, _ := json.Marshal(map[string]any{"code": tc.code, "msg": "upstream message", "data": map[string]any{"plan_id": "weekend"}})
		result, err := parseClaimResult(raw)
		if err != nil {
			t.Fatalf("code %d: %v", tc.code, err)
		}
		if result.Code != tc.code || result.Category != tc.category || result.Message == "" {
			t.Fatalf("code %d result=%+v", tc.code, result)
		}
	}
	if _, err := parseClaimResult([]byte(`{`)); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	stringCode, err := parseClaimResult([]byte(`{"code":"1003"}`))
	if err != nil || stringCode.Category != "already_claimed" {
		t.Fatalf("string code result=%+v err=%v", stringCode, err)
	}
	if _, err := parseClaimResult([]byte(`{"data":{}}`)); err == nil {
		t.Fatal("missing business code was accepted")
	}
}
