package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestZaiCLIUnsupportedFallbackIsExplicitOnly(t *testing.T) {
	for _, err := range []error{
		errors.New("status=404 not found"),
		errors.New("feature not supported"),
		errors.New("unsupported endpoint"),
	} {
		if !isZaiCLIExplicitlyUnsupported(err) {
			t.Fatalf("expected explicit unsupported error to permit fallback: %v", err)
		}
	}
	for _, err := range []error{
		errors.New("network timeout"),
		errors.New("TLS certificate validation failed"),
		errors.New("invalid response envelope"),
		errors.New("status=500 internal error"),
	} {
		if isZaiCLIExplicitlyUnsupported(err) {
			t.Fatalf("unsafe fallback allowed for error: %v", err)
		}
	}
}

func TestPollZaiCLILoginRejectsHostBindingMismatchWithoutNetwork(t *testing.T) {
	secret := strings.Repeat("b", 64)
	_, status, err := pollZaiCLILogin("host-callback", oauthCallback{
		Provider:       "zai",
		FlowID:         "flow_abc",
		PollBearer:     secret,
		BoundHost:      "attacker.example",
		ExpiresAt:      time.Now().Add(time.Minute),
		ServerMediated: true,
	})
	if status != "error" || err == nil || !strings.Contains(err.Error(), "安全校验失败") {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("poll bearer leaked in error")
	}
}

func TestPollZaiCLILoginRejectsExpiredSessionWithoutNetwork(t *testing.T) {
	_, status, err := pollZaiCLILogin("host-callback", oauthCallback{
		Provider:       "zai",
		FlowID:         "flow_abc",
		PollBearer:     strings.Repeat("c", 64),
		BoundHost:      zcodeOAuthHost,
		ExpiresAt:      time.Now().Add(-time.Second),
		ServerMediated: true,
	})
	if status != "expired" || err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestPollZaiCLILoginRejectsUnsafeFlowIDWithoutNetwork(t *testing.T) {
	_, status, err := pollZaiCLILogin("host-callback", oauthCallback{
		Provider:       "zai",
		FlowID:         "../token",
		PollBearer:     strings.Repeat("d", 64),
		BoundHost:      zcodeOAuthHost,
		ExpiresAt:      time.Now().Add(time.Minute),
		ServerMediated: true,
	})
	if status != "error" || err == nil || !strings.Contains(err.Error(), "安全校验失败") {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestServerMediatedOAuthSessionIsConsumedOnce(t *testing.T) {
	state := "one-time-state"
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{
		Provider:       "zai",
		FlowID:         "flow_abc",
		PollBearer:     strings.Repeat("e", 64),
		BoundHost:      zcodeOAuthHost,
		ExpiresAt:      time.Now().Add(time.Minute),
		ServerMediated: true,
	}
	oauthCallbacks.Unlock()

	removeOAuthCallback(state)
	removeOAuthCallback(state)

	oauthCallbacks.Lock()
	_, exists := oauthCallbacks.items[state]
	oauthCallbacks.Unlock()
	if exists {
		t.Fatal("terminal OAuth session remained consumable")
	}
}

func TestFinishOAuthLoginPersistsJWTWithoutTransientSecrets(t *testing.T) {
	accessToken := "access-token-must-not-persist"
	jwt := "header.payload.signature"
	response, err := finishOAuthLogin("host-callback", "zai", oauthTokens{
		AccessToken: accessToken,
		JWTToken:    jwt,
		UserID:      "user_9",
		UserLabel:   "User Nine",
	})
	if err != nil {
		t.Fatalf("finishOAuthLogin: %v", err)
	}
	resp := decodeLoginPollForTest(t, response)
	var storage authStorage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage.ZCodeJWTToken != jwt || storage.Source != "jwt" {
		t.Fatalf("JWT credential was not persisted: %#v", storage)
	}
	if storage.AccessToken != "" || strings.Contains(string(resp.Auth.StorageJSON), accessToken) {
		t.Fatal("access token leaked into persisted storage")
	}
}

func TestSaveOAuthCallbackCannotBindServerMediatedSession(t *testing.T) {
	state := "server-mediated-state"
	oauthCallbacks.Lock()
	oauthCallbacks.items[state] = oauthCallback{
		Provider:       "zai",
		FlowID:         "flow_abc",
		PollBearer:     strings.Repeat("f", 64),
		BoundHost:      zcodeOAuthHost,
		ExpiresAt:      time.Now().Add(time.Minute),
		ServerMediated: true,
	}
	oauthCallbacks.Unlock()
	defer removeOAuthCallback(state)

	if saveOAuthCallback(state, "legacy-auth-code", "") {
		t.Fatal("legacy callback was accepted for a server-mediated session")
	}
}
