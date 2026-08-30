package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	signingGatePath      = "/api/v1/agent/configs"
	signingHandshakePath = "/api/paas/c1f3a7e2/v2/client"
	signingAppID         = "zcode"
	signingKDFSalt       = "WD_CLIENT_SIGN_KDF_SALT"
	signingGateTTL       = time.Hour
	signingGateTimeout   = 15 * time.Second
	signingHandshakeTTL  = 10 * time.Second
	signingResponseMax   = 1 << 20
)

var unsignedSigningPaths = map[string]struct{}{
	"/api/v1/zcode-plan/anthropic/v1/messages": {},
	"/api/v1/zcode-plan/chat/completions":      {},
	"/api/v1/off-peak/anthropic/v1/messages":   {},
}

var signingHeaderNames = map[string]struct{}{
	"x-client-ts": {}, "x-client-version": {}, "x-client-sig": {},
	"x-client-nonce": {}, "x-app-id": {}, "x-client-pow": {},
	"x-client-sign-verified": {},
}

type signingConfig struct {
	Enabled                 bool
	AllowUnsignedChatReplay bool
	GateTimeout             time.Duration
	HandshakeTimeout        time.Duration
	ResponseMax             int64
}

type signingHTTPDo func(context.Context, *http.Request, int64) (*http.Response, []byte, error)

type signingDeps struct {
	now  func() time.Time
	rand io.Reader
	do   signingHTTPDo
}

type signingState struct {
	gateEnabled bool
	gateExpires time.Time
	privateKey  ed25519.PrivateKey
	bypass      bool
}

type signingManager struct {
	cfg    signingConfig
	deps   signingDeps
	mu     sync.Mutex
	states map[string]*signingState
}

type signingOutcome struct {
	Header http.Header
	Signed bool
}

type signingCredential struct{ id, secret string }

func newSigningManager(cfg signingConfig, deps signingDeps) *signingManager {
	if cfg.GateTimeout <= 0 {
		cfg.GateTimeout = signingGateTimeout
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = signingHandshakeTTL
	}
	if cfg.ResponseMax <= 0 {
		cfg.ResponseMax = signingResponseMax
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.rand == nil {
		deps.rand = rand.Reader
	}
	if deps.do == nil {
		deps.do = strictSigningHTTPDo
	}
	return &signingManager{cfg: cfg, deps: deps, states: make(map[string]*signingState)}
}

var defaultSigningManager = newSigningManager(signingConfig{}, signingDeps{})

func strictSigningHTTPDo(ctx context.Context, req *http.Request, max int64) (*http.Response, []byte, error) {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, nil, errors.New("client-signing request failed")
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, max+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return resp, nil, errors.New("client-signing response read failed")
	}
	if int64(len(body)) > max {
		return resp, nil, errors.New("client-signing response exceeds limit")
	}
	return resp, body, nil
}

func parseSigningCredential(raw string) (signingCredential, bool) {
	if strings.Count(raw, ".") != 1 {
		return signingCredential{}, false
	}
	parts := strings.SplitN(raw, ".", 2)
	if strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return signingCredential{}, false
	}
	return signingCredential{id: parts[0], secret: parts[1]}, true
}

func signingStateKey(origin, credential string) string { return origin + "\n" + credential }

func normalizedSigningPath(path string) string {
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	return strings.TrimRight(path, "/")
}

func (m *signingManager) sign(ctx context.Context, rawURL string, header http.Header, credential, appVersion string) (signingOutcome, error) {
	m.mu.Lock()
	enabled := m.cfg.Enabled
	m.mu.Unlock()
	if !enabled {
		return signingOutcome{Header: header}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return signingOutcome{Header: header}, nil
	}
	if _, ok := unsignedSigningPaths[normalizedSigningPath(u.EscapedPath())]; ok {
		return signingOutcome{Header: header}, nil
	}
	if header.Get("X-Client-Sig") != "" {
		return signingOutcome{Header: header}, nil
	}
	cred, ok := parseSigningCredential(credential)
	if !ok || strings.TrimSpace(header.Get("X-Session-Id")) == "" {
		return signingOutcome{Header: header}, nil
	}
	origin := "https://" + u.Host
	if err := sameSigningHost(origin, rawURL); err != nil {
		return signingOutcome{Header: header}, err
	}
	key := signingStateKey(origin, credential)
	m.mu.Lock()
	state := m.states[key]
	if state == nil {
		state = &signingState{}
		m.states[key] = state
	}
	gateValid, gateEnabled, privateKey := state.gateExpires.After(m.deps.now()), state.gateEnabled, append(ed25519.PrivateKey(nil), state.privateKey...)
	m.mu.Unlock()
	if !gateValid {
		gateEnabled, err = m.fetchGate(ctx, origin, credential)
		if err != nil {
			return signingOutcome{}, err
		}
		m.mu.Lock()
		state.gateEnabled, state.gateExpires = gateEnabled, m.deps.now().Add(signingGateTTL)
		m.mu.Unlock()
	}
	if !gateEnabled {
		return signingOutcome{Header: header}, nil
	}
	if len(privateKey) == 0 {
		privateKey, err = m.handshake(ctx, origin, credential, cred)
		if err != nil {
			return signingOutcome{}, err
		}
		m.mu.Lock()
		state.privateKey = append(ed25519.PrivateKey(nil), privateKey...)
		m.mu.Unlock()
	}
	return m.buildHeaders(header, cred.id, appVersion, privateKey)
}

func sameSigningHost(origin, target string) error {
	a, errA := url.Parse(origin)
	b, errB := url.Parse(target)
	if errA != nil || errB != nil || a.Scheme != "https" || b.Scheme != "https" || a.User != nil || b.User != nil || !strings.EqualFold(a.Hostname(), b.Hostname()) || a.Port() != b.Port() {
		return errors.New("client-signing target is outside credential host binding")
	}
	return nil
}

func (m *signingManager) fetchGate(parent context.Context, origin, credential string) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, m.cfg.GateTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin+signingGatePath, nil)
	req.Header.Set("x-api-key", credential)
	resp, body, err := m.deps.do(ctx, req, m.cfg.ResponseMax)
	if err != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, errors.New("client-signing gate unavailable")
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			CodingPlanSignature *struct {
				Enable bool `json:"enable"`
			} `json:"codingPlanSignature"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code != 0 {
		return false, errors.New("client-signing gate unavailable")
	}
	return envelope.Data.CodingPlanSignature != nil && envelope.Data.CodingPlanSignature.Enable, nil
}

func (m *signingManager) handshake(parent context.Context, origin, rawCredential string, cred signingCredential) (ed25519.PrivateKey, error) {
	ts := fmt.Sprint(m.deps.now().UnixMilli())
	nonceBytes := make([]byte, 16)
	if _, err := io.ReadFull(m.deps.rand, nonceBytes); err != nil {
		return nil, errors.New("client-signing random failed")
	}
	nonce := hex.EncodeToString(nonceBytes)
	mac := hmac.New(sha256.New, hkdfSHA256([]byte(cred.secret), signingKDFSalt, "getSignKey_hmac", 32))
	_, _ = mac.Write([]byte("get_sign_key\n" + cred.id + "\n" + ts + "\n" + nonce))
	payload, _ := json.Marshal(map[string]string{"apiKey": rawCredential, "nonce": nonce, "sig": base64.StdEncoding.EncodeToString(mac.Sum(nil)), "ts": ts})
	ctx, cancel := context.WithTimeout(parent, m.cfg.HandshakeTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, origin+signingHandshakePath, bytes.NewReader(payload))
	req.Header.Set("Authorization", rawCredential)
	req.Header.Set("Content-Type", "application/json")
	resp, body, err := m.deps.do(ctx, req, m.cfg.ResponseMax)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		return nil, errors.New("client-signing handshake unavailable")
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			PrivateCipher string `json:"privateCipher"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code != 200 || envelope.Data.PrivateCipher == "" {
		return nil, errors.New("client-signing handshake rejected")
	}
	return decryptSigningPrivateKey(cred, envelope.Data.PrivateCipher)
}

func hkdfSHA256(secret []byte, salt, info string, size int) []byte {
	extract := hmac.New(sha256.New, []byte(salt))
	_, _ = extract.Write(secret)
	prk := extract.Sum(nil)
	out, previous := make([]byte, 0, size), []byte(nil)
	for counter := byte(1); len(out) < size; counter++ {
		expand := hmac.New(sha256.New, prk)
		_, _ = expand.Write(previous)
		_, _ = expand.Write([]byte(info))
		_, _ = expand.Write([]byte{counter})
		previous = expand.Sum(nil)
		out = append(out, previous...)
	}
	return out[:size]
}

func decryptSigningPrivateKey(cred signingCredential, encoded string) (ed25519.PrivateKey, error) {
	ciphertext, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(ciphertext) <= 28 {
		return nil, errors.New("client-signing private key invalid")
	}
	block, err := aes.NewCipher(hkdfSHA256([]byte(cred.secret), signingKDFSalt, "ed25519_priv", 32))
	if err != nil {
		return nil, errors.New("client-signing private key invalid")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("client-signing private key invalid")
	}
	plain, err := gcm.Open(nil, ciphertext[:12], ciphertext[12:], []byte(cred.id))
	if err != nil {
		return nil, errors.New("client-signing private key invalid")
	}
	der, err := base64.StdEncoding.Strict().DecodeString(string(plain))
	if err != nil {
		return nil, errors.New("client-signing private key invalid")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("client-signing private key invalid")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("client-signing private key invalid")
	}
	return key, nil
}

func (m *signingManager) buildHeaders(header http.Header, id, appVersion string, privateKey ed25519.PrivateKey) (signingOutcome, error) {
	ts := fmt.Sprint(m.deps.now().UnixMilli())
	session := strings.TrimSpace(header.Get("X-Session-Id"))
	nonceBytes := make([]byte, 16)
	if _, err := io.ReadFull(m.deps.rand, nonceBytes); err != nil {
		return signingOutcome{Header: header}, errors.New("client-signing random failed")
	}
	nonce := hex.EncodeToString(nonceBytes)
	pow, err := createSigningPoW(id, session, ts, m.deps.rand)
	if err != nil {
		return signingOutcome{Header: header}, errors.New("client-signing proof failed")
	}
	sig := ed25519.Sign(privateKey, []byte(id+"\n"+ts+"\n"+appVersion+"\n"+session+"\n"+nonce))
	out := header.Clone()
	for name := range signingHeaderNames {
		out.Del(name)
	}
	out.Del("X-Session-Id")
	out.Set("X-Client-Ts", ts)
	out.Set("X-Client-Version", appVersion)
	out.Set("X-Client-Sig", base64.StdEncoding.EncodeToString(sig))
	out.Set("X-Session-Id", session)
	out.Set("X-Client-Nonce", nonce)
	out.Set("X-App-Id", signingAppID)
	out.Set("X-Client-Pow", pow)
	return signingOutcome{Header: out, Signed: true}, nil
}

func createSigningPoW(id, session, ts string, random io.Reader) (string, error) {
	seedDigest := sha256.Sum256([]byte(id + "\n" + signingAppID + "\n" + session + "\n" + ts))
	seed := hex.EncodeToString(seedDigest[:])[:32]
	raw := make([]byte, 12)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", err
	}
	prefix := hex.EncodeToString(raw)
	for counter := uint64(0); counter <= uint64(^uint32(0)); counter++ {
		candidate := fmt.Sprintf("%s%08x", prefix, counter)
		digest := sha256.Sum256([]byte(seed + "\n" + candidate))
		if digest[0] == 0 {
			return candidate, nil
		}
	}
	return "", errors.New("proof not found")
}

func isSigningVerifyFailure(status int, body []byte) bool {
	if status != http.StatusUnauthorized || len(body) > signingResponseMax {
		return false
	}
	var envelope struct {
		Msg, Reason string
		Data, Error struct{ Reason, Message string }
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	for _, value := range []string{envelope.Msg, envelope.Reason, envelope.Data.Reason, envelope.Error.Reason, envelope.Error.Message} {
		if value == "VERIFY_SIGNATURE_INVALID" || value == "VERIFY_APIKEY_EXPIRED" {
			return true
		}
	}
	return false
}

func (m *signingManager) invalidate(rawURL, credential string, bypass bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	key := signingStateKey("https://"+u.Host, credential)
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.states[key]; state != nil {
		state.privateKey = nil
		state.bypass = false
	}
}

func isExplicitChatSigningPath(method, rawURL string) bool {
	if strings.ToUpper(method) != http.MethodPost {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := normalizedSigningPath(u.EscapedPath())
	return path == "/api/anthropic/v1/messages" || path == "/api/coding/paas/v4/chat/completions" || path == "/api/coding/paas/v4/responses"
}

func isSafeUnsignedReplay(method, rawURL string) bool {
	if _, err := url.ParseRequestURI(rawURL); err != nil {
		return false
	}
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}
