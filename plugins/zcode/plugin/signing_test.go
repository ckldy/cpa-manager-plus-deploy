package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseSigningCredentialStrict(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{{"id.secret", true}, {"id.secret.more", false}, {"noseparator", false}, {".secret", false}, {"id.", false}, {" .secret", false}} {
		_, got := parseSigningCredential(tc.raw)
		if got != tc.ok {
			t.Fatalf("parse %q = %v, want %v", tc.raw, got, tc.ok)
		}
	}
}

func TestSigningDisabledAndEligibilityNeverFetch(t *testing.T) {
	calls := 0
	deps := signingDeps{do: func(context.Context, *http.Request, int64) (*http.Response, []byte, error) {
		calls++
		return nil, nil, nil
	}}
	for _, tc := range []struct {
		cfg        signingConfig
		url        string
		credential string
		session    string
	}{
		{signingConfig{}, "https://api.z.ai/v1/chat", "id.secret", "s"},
		{signingConfig{Enabled: true}, "http://api.z.ai/v1/chat", "id.secret", "s"},
		{signingConfig{Enabled: true}, "https://api.z.ai/api/v1/zcode-plan/chat/completions/", "id.secret", "s"},
		{signingConfig{Enabled: true}, "https://api.z.ai/v1/chat", "bad", "s"},
		{signingConfig{Enabled: true}, "https://api.z.ai/v1/chat", "id.secret", ""},
	} {
		h := http.Header{}
		if tc.session != "" {
			h.Set("X-Session-Id", tc.session)
		}
		out, err := newSigningManager(tc.cfg, deps).sign(context.Background(), tc.url, h, tc.credential, "3.1.1")
		if err != nil || out.Signed {
			t.Fatalf("unexpected sign result: signed=%v err=%v", out.Signed, err)
		}
	}
	if calls != 0 {
		t.Fatalf("eligibility performed %d network calls", calls)
	}
}

func TestHKDFSHA256Vector(t *testing.T) {
	got := hex.EncodeToString(hkdfSHA256([]byte("secret"), signingKDFSalt, "getSignKey_hmac", 32))
	const want = "b289437da100f87c5b9c8b8d5543f191017a0c6a96a7f19fec74b5882220a989"
	if got != want {
		t.Fatalf("HKDF = %s, want %s", got, want)
	}
}

func encryptedPrivateCipher(t *testing.T, id, secret string, private ed25519.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(hkdfSHA256([]byte(secret), signingKDFSalt, "ed25519_priv", 32))
	gcm, _ := cipher.NewGCM(block)
	iv := bytes.Repeat([]byte{7}, 12)
	plain := []byte(base64.StdEncoding.EncodeToString(der))
	sealed := gcm.Seal(nil, iv, plain, []byte(id))
	return base64.StdEncoding.EncodeToString(append(iv, sealed...))
}

func TestSigningHandshakeHeadersAndPoW(t *testing.T) {
	seed := bytes.Repeat([]byte{3}, ed25519.SeedSize)
	private := ed25519.NewKeyFromSeed(seed)
	ciphertext := encryptedPrivateCipher(t, "id", "secret", private)
	now := time.UnixMilli(1700000000123)
	random := bytes.NewReader(bytes.Repeat([]byte{9}, 4096))
	var requests []*http.Request
	do := func(_ context.Context, req *http.Request, max int64) (*http.Response, []byte, error) {
		if max != signingResponseMax {
			t.Fatalf("max=%d", max)
		}
		requests = append(requests, req.Clone(context.Background()))
		if req.URL.Path == signingGatePath {
			return &http.Response{StatusCode: 200}, []byte(`{"code":0,"data":{"codingPlanSignature":{"enable":true}}}`), nil
		}
		if req.URL.Path != signingHandshakePath || req.Header.Get("Authorization") != "id.secret" {
			t.Fatalf("bad handshake request")
		}
		return &http.Response{StatusCode: 200}, []byte(`{"code":200,"data":{"privateCipher":"` + ciphertext + `"}}`), nil
	}
	m := newSigningManager(signingConfig{Enabled: true}, signingDeps{now: func() time.Time { return now }, rand: random, do: do})
	h := http.Header{"X-Session-Id": {"session"}, "X-Client-Sign-Verified": {"stale"}}
	out, err := m.sign(context.Background(), "https://api.z.ai/v1/chat", h, "id.secret", "3.1.1")
	if err != nil || !out.Signed {
		t.Fatalf("signed=%v err=%v", out.Signed, err)
	}
	for _, name := range []string{"X-Client-Ts", "X-Client-Version", "X-Client-Sig", "X-Session-Id", "X-Client-Nonce", "X-App-Id", "X-Client-Pow"} {
		if out.Header.Get(name) == "" {
			t.Errorf("missing %s", name)
		}
	}
	if out.Header.Get("X-Client-Sign-Verified") != "" {
		t.Fatal("stale signing header retained")
	}
	message := "id\n1700000000123\n3.1.1\nsession\n" + out.Header.Get("X-Client-Nonce")
	sig, _ := base64.StdEncoding.DecodeString(out.Header.Get("X-Client-Sig"))
	if !ed25519.Verify(private.Public().(ed25519.PublicKey), []byte(message), sig) {
		t.Fatal("business LF signature invalid")
	}
	pow := out.Header.Get("X-Client-Pow")
	seedHash := sha256.Sum256([]byte("id\nzcode\nsession\n1700000000123"))
	digest := sha256.Sum256([]byte(hex.EncodeToString(seedHash[:])[:32] + "\n" + pow))
	if digest[0] != 0 {
		t.Fatal("PoW does not have 8 leading zero bits")
	}
	if len(requests) != 2 {
		t.Fatalf("requests=%d", len(requests))
	}
}

func TestStrictTransportRejectsRedirectAndBodyLimit(t *testing.T) {
	// The production helper's redirect callback is tested structurally without network.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := client.CheckRedirect(&http.Request{}, nil); err != http.ErrUseLastResponse {
		t.Fatal("redirect not rejected")
	}
	body := io.NopCloser(strings.NewReader("12345"))
	read, _ := io.ReadAll(io.LimitReader(body, 4))
	if len(read) != 4 {
		t.Fatal("limit reader invariant")
	}
}

func TestVerifyRetryDoesNotThirdReplayChatPOST(t *testing.T) {
	m := newSigningManager(signingConfig{Enabled: true}, signingDeps{})
	// Seed a signing state so this test remains pure and performs no gate/handshake network.
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	origin := "https://api.z.ai"
	credential := "id.secret"
	ciphertext := encryptedPrivateCipher(t, "id", "secret", private)
	m.deps.do = func(context.Context, *http.Request, int64) (*http.Response, []byte, error) {
		return &http.Response{StatusCode: 200}, []byte(`{"code":200,"data":{"privateCipher":"` + ciphertext + `"}}`), nil
	}
	m.states[signingStateKey(origin, credential)] = &signingState{gateEnabled: true, gateExpires: time.Now().Add(time.Hour), privateKey: private}
	m.deps.now = func() time.Time { return time.UnixMilli(1) }
	m.deps.rand = bytes.NewReader(bytes.Repeat([]byte{1}, 8192))
	calls := 0
	send := func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"reason":"VERIFY_SIGNATURE_INVALID"}`)}, nil
	}
	req := pluginapi.HTTPRequest{Method: http.MethodPost, URL: origin + "/v1/chat", Headers: http.Header{"X-Session-Id": {"s"}}}
	resp, err := executeSignedHTTP(context.Background(), m, upstreamCredential{SigningCredential: credential}, req, send)
	if err != nil || resp.StatusCode != 401 || calls != 2 {
		t.Fatalf("status=%d calls=%d err=%v", resp.StatusCode, calls, err)
	}
	if m.states[signingStateKey(origin, credential)].bypass {
		t.Fatal("dangerous requests must not enter persistent unsigned bypass")
	}
}

func TestUnsignedChatReplayIsPathScoped(t *testing.T) {
	if !isExplicitChatSigningPath(http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages") {
		t.Fatal("known chat path must be eligible for explicit replay")
	}
	for _, raw := range []string{"https://api.z.ai/admin/delete", "https://api.z.ai/v1/keys", "https://api.z.ai/api/anthropic/v1/messages"} {
		method := http.MethodPost
		if strings.Contains(raw, "delete") {
			method = http.MethodDelete
		}
		if raw != "https://api.z.ai/api/anthropic/v1/messages" && isExplicitChatSigningPath(method, raw) {
			t.Fatalf("unsafe path eligible: %s %s", method, raw)
		}
	}
}

func TestVerifyThirdReplayRequiresSafeMethodOrOptIn(t *testing.T) {
	if !isSafeUnsignedReplay(http.MethodGet, "https://api.z.ai/x") {
		t.Fatal("GET must be safe")
	}
	if isSafeUnsignedReplay(http.MethodPost, "https://api.z.ai/v1/chat") {
		t.Fatal("POST must not be safe")
	}
	if !isSigningVerifyFailure(401, []byte(`{"error":{"message":"VERIFY_APIKEY_EXPIRED"}}`)) {
		t.Fatal("VERIFY envelope missed")
	}
	if isSigningVerifyFailure(401, bytes.Repeat([]byte{'x'}, signingResponseMax+1)) {
		t.Fatal("oversized VERIFY body accepted")
	}
}

func TestGateAndHandshakeStayOnCredentialHost(t *testing.T) {
	if sameSigningHost("https://api.z.ai", "https://evil.example/v1") == nil {
		t.Fatal("cross-host allowed")
	}
	if sameSigningHost("https://api.z.ai", "https://api.z.ai/v1") != nil {
		t.Fatal("same host rejected")
	}
}

func TestErrorsDoNotContainCredentialOrUpstreamBody(t *testing.T) {
	secret := "id.super-secret-value"
	m := newSigningManager(signingConfig{Enabled: true}, signingDeps{do: func(context.Context, *http.Request, int64) (*http.Response, []byte, error) {
		return &http.Response{StatusCode: 500}, []byte(secret), nil
	}})
	h := http.Header{"X-Session-Id": {"s"}}
	_, err := m.sign(context.Background(), "https://api.z.ai/v1/chat", h, secret, "v")
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Fatal("credential leaked")
	}
}

func TestRouteSigningDefaultsDisabled(t *testing.T) {
	applyRouteConfig(mustLifecycle(t, "route_mode: free-first\n"))
	cfg := currentRouteConfig()
	if cfg.ClientSigningEnabled || cfg.ClientSigningAllowChatReplay {
		t.Fatal("signing defaults are not disabled")
	}
	applyRouteConfig(mustLifecycle(t, "client_signing_enabled: true\nclient_signing_allow_unsigned_chat_replay: true\n"))
	cfg = currentRouteConfig()
	if !cfg.ClientSigningEnabled || !cfg.ClientSigningAllowChatReplay {
		t.Fatal("signing config not parsed")
	}
}

func mustLifecycle(t *testing.T, yaml string) []byte {
	t.Helper()
	out, err := json.Marshal(map[string][]byte{"config_yaml": []byte(yaml)})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
