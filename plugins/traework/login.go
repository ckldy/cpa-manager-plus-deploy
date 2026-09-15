package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
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
	traeAPIHost       = "https://api.trae.com.cn"
	traeClientID      = "en1oxy7wnw8j9n"
	traeIDEVersion    = "0.1.43"
	traeCallbackURL   = "http://127.0.0.1:18080/authorize"
	traeTraceIDLength = 16
	traeExchangePath  = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	traeUserInfoPath  = "/cloudide/api/v3/trae/GetUserInfo"
	maxAuthResponse   = 1 << 20
)

// loginSession 是一次登录会话。TRAE 将 auth_callback_url 限制为回环地址
// （只接受 http://127.0.0.1:18080/authorize，已实测拒绝任何公网 host），因此
// 浏览器无法自动送达远端 CPA，回调 URL 必须由管理员手动粘贴完成登录。
type loginSession struct {
	State     string
	MachineID string
	DeviceID  string
	ExpiresAt time.Time
}

type loginSessionStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	sessions map[string]loginSession
}

func newLoginSessionStore(ttl time.Duration) *loginSessionStore {
	return &loginSessionStore{ttl: ttl, sessions: make(map[string]loginSession)}
}

func (s *loginSessionStore) New(now time.Time) (loginSession, error) {
	state, err := randomHex(32)
	if err != nil {
		return loginSession{}, err
	}
	machineID, err := randomHex(16)
	if err != nil {
		return loginSession{}, err
	}
	deviceID, err := randomHex(16)
	if err != nil {
		return loginSession{}, err
	}
	session := loginSession{State: state, MachineID: machineID, DeviceID: deviceID, ExpiresAt: now.Add(s.ttl)}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, item := range s.sessions {
		if !now.Before(item.ExpiresAt) {
			delete(s.sessions, key)
		}
	}
	s.sessions[state] = session
	return session, nil
}

// Peek 非破坏性读取会话（自动回调用 trace 前缀定位会话后再原子 Consume）。
func (s *loginSessionStore) Peek(state string, now time.Time) (loginSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[state]
	if !ok || !now.Before(session.ExpiresAt) {
		return loginSession{}, false
	}
	return session, true
}

// Consume atomically removes the session before returning it, preventing replay.
func (s *loginSessionStore) Consume(state string, now time.Time) (loginSession, error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return loginSession{}, errors.New("missing login state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[state]
	if ok {
		delete(s.sessions, state)
	}
	if !ok {
		return loginSession{}, errors.New("login session not found or already consumed")
	}
	if !now.Before(session.ExpiresAt) {
		return loginSession{}, errors.New("login session expired")
	}
	return session, nil
}

func randomHex(n int) (string, error) {
	if n <= 0 {
		return "", errors.New("random byte count must be positive")
	}
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func buildLoginURL(session loginSession) (string, error) {
	if session.State == "" || session.MachineID == "" || session.DeviceID == "" {
		return "", errors.New("incomplete login session")
	}
	u := url.URL{Scheme: "https", Host: "www.trae.cn", Path: "/authorization"}
	q := u.Query()
	for key, value := range map[string]string{
		"login_version": "1", "auth_from": "solo", "login_channel": "native_ide", "plugin_version": "2.3.62834", "auth_type": "local", "client_id": traeClientID, "redirect": "0", "login_trace_id": session.State[:traeTraceIDLength], "auth_callback_url": traeCallbackURL, "machine_id": session.MachineID, "device_id": session.DeviceID, "x_device_id": session.DeviceID, "x_machine_id": session.MachineID, "x_device_brand": "PC", "x_device_type": "PC", "x_os_version": "1.0", "x_app_version": traeIDEVersion, "x_app_type": "stable",
	} {
		q.Set(key, value)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type loginCallback struct {
	RefreshToken string
	UserInfo     struct {
		UserID, ScreenName, TenantID string
	}
	UserJWT struct {
		Token, RefreshToken string
		TokenExpireAt       int64
	}
}

func parseLoginCallback(raw string) (loginCallback, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return loginCallback{}, fmt.Errorf("invalid callback URL: %w", err)
	}
	if u.Scheme != "http" || u.Host != "127.0.0.1:18080" || u.Path != "/authorize" || u.Fragment != "" || u.User != nil {
		return loginCallback{}, errors.New("callback URL origin or path mismatch")
	}
	return parseLoginCallbackQuery(u.Query())
}

// parseLoginCallbackQuery 校验回调 query 严格白名单并解析凭据。
func parseLoginCallbackQuery(q url.Values) (loginCallback, error) {
	// TRAE 真实回调会随版本携带白名单之外的参数（如 sessionId/state/auth_callback_url），
	// 未知参数名一律忽略：只提取 refreshToken/userJwt/userInfo/host，多余参数不参与解析。
	// 任何参数出现多次仍拒绝（歧义/污染）；host 若存在必须为官方 API（下方校验）。
	for key, values := range q {
		if len(values) != 1 {
			return loginCallback{}, fmt.Errorf("invalid callback parameter %q", key)
		}
	}
	if host := strings.TrimSpace(q.Get("host")); host != "" && host != traeAPIHost {
		return loginCallback{}, errors.New("callback API host mismatch")
	}
	var out loginCallback
	out.RefreshToken = strings.TrimSpace(q.Get("refreshToken"))
	if raw := q.Get("userInfo"); raw != "" {
		var v struct {
			UserID     string `json:"UserID"`
			ScreenName string `json:"ScreenName"`
			TenantID   string `json:"TenantID"`
		}
		if err := decodeJSONObject(raw, &v); err != nil {
			return loginCallback{}, fmt.Errorf("invalid userInfo: %w", err)
		}
		out.UserInfo.UserID, out.UserInfo.ScreenName, out.UserInfo.TenantID = v.UserID, v.ScreenName, v.TenantID
	}
	if raw := q.Get("userJwt"); raw != "" {
		var v struct {
			Token         string `json:"Token"`
			RefreshToken  string `json:"RefreshToken"`
			TokenExpireAt int64  `json:"TokenExpireAt"`
		}
		if err := decodeJSONObject(raw, &v); err != nil {
			return loginCallback{}, fmt.Errorf("invalid userJwt: %w", err)
		}
		out.UserJWT.Token, out.UserJWT.RefreshToken, out.UserJWT.TokenExpireAt = v.Token, v.RefreshToken, v.TokenExpireAt
	}
	if out.RefreshToken == "" {
		out.RefreshToken = strings.TrimSpace(out.UserJWT.RefreshToken)
	}
	if out.RefreshToken == "" && strings.TrimSpace(out.UserJWT.Token) == "" {
		return loginCallback{}, errors.New("callback contains no usable token")
	}
	return out, nil
}

func decodeJSONObject(raw string, dst any) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type exchangeTokenResult struct {
	AccessToken, RefreshToken string
	ExpiresAt                 int64
}

func ExchangeToken(ctx context.Context, client HTTPDoer, apiHost, clientID, refreshToken string) (exchangeTokenResult, error) {
	return exchangeTokenAt(ctx, client, apiHost, clientID, refreshToken, time.Now())
}

func exchangeTokenAt(ctx context.Context, client HTTPDoer, apiHost, clientID, refreshToken string, now time.Time) (exchangeTokenResult, error) {
	if client == nil {
		return exchangeTokenResult{}, errors.New("nil HTTP client")
	}
	endpoint, err := authEndpoint(apiHost, traeExchangePath)
	if err != nil {
		return exchangeTokenResult{}, err
	}
	refreshToken, clientID = strings.TrimSpace(refreshToken), strings.TrimSpace(clientID)
	if refreshToken == "" || clientID == "" {
		return exchangeTokenResult{}, errors.New("missing client ID or refresh token")
	}
	body, _ := json.Marshal(map[string]string{"ClientID": clientID, "RefreshToken": refreshToken, "ClientSecret": "-", "UserID": ""})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return exchangeTokenResult{}, err
	}
	setTraeOAuthHeaders(req)
	var payload struct {
		Result struct {
			Token                              string `json:"Token"`
			RefreshToken                       string `json:"RefreshToken"`
			TokenExpireAt, TokenExpireDuration int64
		} `json:"Result"`
	}
	if err := doAuthJSON(client, req, &payload); err != nil {
		return exchangeTokenResult{}, err
	}
	if strings.TrimSpace(payload.Result.Token) == "" {
		return exchangeTokenResult{}, errors.New("exchange response missing token")
	}
	expires := payload.Result.TokenExpireAt
	if expires > 1_000_000_000_000 {
		expires /= 1000
	}
	if expires <= 0 && payload.Result.TokenExpireDuration > 0 {
		expires = now.Unix() + payload.Result.TokenExpireDuration
	}
	return exchangeTokenResult{AccessToken: payload.Result.Token, RefreshToken: firstNonEmpty(payload.Result.RefreshToken, refreshToken), ExpiresAt: expires}, nil
}

type userInfoResult struct{ UserID, Nickname, EnterpriseID string }

func GetUserInfo(ctx context.Context, client HTTPDoer, apiHost, accessToken string) (userInfoResult, error) {
	if client == nil {
		return userInfoResult{}, errors.New("nil HTTP client")
	}
	endpoint, err := authEndpoint(apiHost, traeUserInfoPath)
	if err != nil {
		return userInfoResult{}, err
	}
	if accessToken = strings.TrimSpace(accessToken); accessToken == "" {
		return userInfoResult{}, errors.New("missing access token")
	}
	body, _ := json.Marshal(map[string]string{"ReqSource": "IDE", "IDEVersion": traeIDEVersion})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return userInfoResult{}, err
	}
	setTraeOAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", accessToken)
	var payload struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := doAuthJSON(client, req, &payload); err != nil {
		return userInfoResult{}, err
	}
	if strings.TrimSpace(payload.Result.UserID) == "" {
		return userInfoResult{}, errors.New("userinfo response missing user ID")
	}
	return userInfoResult{UserID: payload.Result.UserID, Nickname: payload.Result.ScreenName, EnterpriseID: payload.Result.EnterpriseID}, nil
}

func authEndpoint(host, path string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(host))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("invalid API host")
	}
	if !strings.EqualFold(u.Hostname(), "api.trae.com.cn") || u.Port() != "" {
		return "", errors.New("untrusted API host")
	}
	u.Path, u.RawPath = path, ""
	return u.String(), nil
}

func setTraeOAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Trae/"+traeIDEVersion)
}

func doAuthJSON(client HTTPDoer, req *http.Request, dst any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp == nil || resp.Body == nil {
		return errors.New("empty HTTP response")
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxAuthResponse+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(raw) > maxAuthResponse {
		return errors.New("authentication response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("authentication upstream HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errors.New("authentication upstream returned invalid JSON")
	}
	return nil
}
