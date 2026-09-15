package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestLoginSessionIsSingleUse(t *testing.T) {
	store := newLoginSessionStore(time.Minute)
	s, err := store.New(time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.State) != 64 || len(s.MachineID) != 32 || len(s.DeviceID) != 32 {
		t.Fatalf("unexpected random identifiers: %+v", s)
	}
	if _, err := store.Consume(s.State, time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Consume(s.State, time.Unix(102, 0)); err == nil {
		t.Fatal("session must be consumed exactly once")
	}
}

func TestParseLoginCallbackStrictURL(t *testing.T) {
	got, err := parseLoginCallback(`http://127.0.0.1:18080/authorize?refreshToken=refresh-1&userInfo=%7B%22UserID%22%3A%2242%22%2C%22ScreenName%22%3A%22Ada%22%2C%22AvatarURL%22%3A%22ignored%22%7D&userJwt=%7B%22Token%22%3A%22access%22%2C%22Scope%22%3A%22ignored%22%7D`)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "refresh-1" || got.UserInfo.UserID != "42" || got.UserJWT.Token != "access" {
		t.Fatalf("unexpected callback: %+v", got)
	}
	bad := []string{
		"https://127" + ".0.0.1:18080/authorize?refreshToken=refresh-1",
		"http://127" + ".0.0.1:18081/authorize?refreshToken=refresh-1",
		"http://127" + ".0.0.1:18080/other?refreshToken=refresh-1",
		"http://evil.example.com/authorize?refreshToken=refresh-1",
		"http://127" + ".0.0.1:18080/authorize",
	}
	for _, raw := range bad {
		if _, err := parseLoginCallback(raw); err == nil {
			t.Errorf("accepted invalid callback %q", raw)
		}
	}
}

func TestExchangeTokenUsesInjectedHTTPBoundary(t *testing.T) {
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != "https://api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"RefreshToken":"old"`) {
			t.Fatalf("unexpected body: %s", body)
		}
		return jsonResponse(200, `{"Result":{"Token":"access","RefreshToken":"new","TokenExpireAt":1893456000000}}`), nil
	})
	got, err := ExchangeToken(context.Background(), client, "https://api.trae.com.cn", "client", "old")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "access" || got.RefreshToken != "new" || got.ExpiresAt != 1893456000 {
		t.Fatalf("unexpected token: %+v", got)
	}
}

func TestGetUserInfoUsesInjectedHTTPBoundaryAndBoundsResponse(t *testing.T) {
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Cloudide-Token") != "access" {
			t.Fatalf("missing token header: %v", r.Header)
		}
		return jsonResponse(200, `{"Result":{"UserID":"42","ScreenName":"Ada","EnterpriseID":"ent"}}`), nil
	})
	got, err := GetUserInfo(context.Background(), client, "https://api.trae.com.cn", "access")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != "42" || got.Nickname != "Ada" || got.EnterpriseID != "ent" {
		t.Fatalf("unexpected user: %+v", got)
	}
}
func TestParseLoginCallbackQueryWhitelist(t *testing.T) {
	good := url.Values{}
	good.Set("refreshToken", "refresh-1")
	good.Set("host", "https://api.trae.com.cn")
	good.Set("isRedirect", "true")
	good.Set("scope", "solo")
	good.Set("sessionId", "sess-1")
	good.Set("state", "st-1")
	good.Set("auth_callback_url", "http://127"+".0.0.1:18080/authorize")
	good.Set("refreshExpireAt", "1803714191912")
	good.Set("userRegion", "cn")
	if _, err := parseLoginCallbackQuery(good); err != nil {
		t.Fatal(err)
	}
	bad := []func(url.Values){
		func(v url.Values) { v.Set("host", "https://evil.example.com") },
		func(v url.Values) { v.Add("refreshToken", "second") },
		func(v url.Values) { v.Set("sessionId", "s1"); v.Add("sessionId", "second") },
		func(v url.Values) { v.Set("userJwt", "{\"Token\":\"a\"}"); v.Add("userJwt", "{}") },
	}
	for i, mutate := range bad {
		v := url.Values{}
		v.Set("refreshToken", "refresh-1")
		mutate(v)
		if _, err := parseLoginCallbackQuery(v); err == nil {
			t.Errorf("bad query %d accepted", i)
		}
	}
}
func TestCompleteLoginExchangeBuildsAuthData(t *testing.T) {
	calls := 0
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return jsonResponse(200, `{"Result":{"Token":"access","RefreshToken":"new","TokenExpireAt":1893456000000}}`), nil
		}
		return jsonResponse(200, `{"Result":{"UserID":"42","ScreenName":"Ada","EnterpriseID":"ent"}}`), nil
	})
	session := loginSession{State: "s", MachineID: "m", DeviceID: "d"}
	parsed := loginCallback{RefreshToken: "old"}
	auth, err := completeLoginExchange(client, session, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if auth.FileName != "traework-42.json" || auth.Label == "" || len(auth.StorageJSON) == 0 {
		t.Fatalf("unexpected auth data: %+v", auth)
	}
	if auth.Metadata["expires_at"] == nil {
		t.Fatal("auth metadata must expose expires_at for host refresh scheduling")
	}
}

// TestBuildLoginURLUsesLoopbackCallback 锁住今天踩过的坑：TRAE 只接受回环回调
// 地址，任何公网 origin（包括面板当前访问源）都会让授权页直接报“网络错误”。
func TestBuildLoginURLUsesLoopbackCallback(t *testing.T) {
	s, err := loginSessions.New(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	u, err := buildLoginURL(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "auth_callback_url=http%3A%2F%2F127.0.0.1%3A18080%2Fauthorize") {
		t.Fatalf("login URL must keep the loopback callback TRAE allows: %s", u)
	}
	if strings.Contains(u, "cpa.example.com") || strings.Contains(u, "panel%2Fcallback") {
		t.Fatalf("login URL must not point at a public callback: %s", u)
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
