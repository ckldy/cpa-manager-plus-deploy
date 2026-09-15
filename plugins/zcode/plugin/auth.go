package main

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ZCode OAuth + API endpoints (confirmed by zcode-reverse-engineer).
const (
	ZCodeAuthorizeURL    = "https://chat.z.ai/api/oauth/authorize"
	BigModelAuthorizeURL = "https://bigmodel.cn/login"
	ZCodeTokenURL        = "https://zcode.z.ai/api/v1/oauth/token"
	ZCodeCLIInitURL      = "https://zcode.z.ai/api/v1/oauth/cli/init"
	ZCodeCLIPollBaseURL  = "https://zcode.z.ai/api/v1/oauth/cli/poll/"
	zcodeOAuthHost       = "zcode.z.ai"
	ZCodeBusinessLogin   = "https://api.z.ai/api/auth/z/login"
	ZCodeAppID           = "client_P8X5CMWmlaRO9gyO-KSqtg"
	BigModelAppID        = "zcode"
	ZCodeRedirectURI     = "zcode://zai-auth/callback"
	zcodeCustomerURL     = "https://api.z.ai/api/biz/customer/getCustomerInfo"
	bigModelCustomerURL  = "https://bigmodel.cn/api/biz/customer/getCustomerInfo"
	zcodeAPIHost         = "https://api.z.ai"
	bigModelAPIHost      = "https://bigmodel.cn"
	zcodeAPIKeyName      = "zcode-api-key"
)

type oauthCallback struct {
	Provider        string
	Code            string
	Error           string
	FlowID          string
	PollBearer      string
	PollIntervalSec int
	BoundHost       string
	ExpiresAt       time.Time
	Processing      bool
	ServerMediated  bool
}

var oauthCallbacks = struct {
	sync.Mutex
	items map[string]oauthCallback
}{items: make(map[string]oauthCallback)}

// authStorage is the persisted auth JSON stored in StorageJSON.
type authStorage struct {
	ZCodeJWTToken       string `json:"zcode_jwt_token,omitempty"`
	APIKey              string `json:"api_key,omitempty"`
	CaptchaVerifyParam  string `json:"captcha_verify_param,omitempty"`
	CaptchaVerifyRegion string `json:"captcha_verify_region,omitempty"`
	DeviceMID           string `json:"device_mid,omitempty"`
	AccessToken         string `json:"-"`
	RefreshToken        string `json:"-"`
	UserLabel           string `json:"user_label,omitempty"`
	UserID              string `json:"user_id,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Source              string `json:"source,omitempty"` // "jwt" | "api-key"
}

func parseAuthStorage(raw []byte) (*authStorage, error) {
	var s authStorage
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.ZCodeJWTToken) == "" && strings.TrimSpace(s.APIKey) == "" {
		return nil, errors.New("missing zcode_jwt_token or api_key")
	}
	return &s, nil
}

// decodeJWTPayload decodes a JWT payload without verification (used only for labeling).
func decodeJWTPayload(jwt string) map[string]any {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil
	}
	p := parts[1]
	if m := len(p) % 4; m != 0 {
		p += strings.Repeat("=", 4-m)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(p, "="))
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(p)
		if err != nil {
			return nil
		}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// buildLabel derives a human-readable label from auth material.
func buildLabel(s *authStorage) string {
	if s.UserLabel != "" {
		return s.UserLabel
	}
	if s.UserID != "" {
		return "zcode-" + s.UserID
	}
	if s.ZCodeJWTToken == "" {
		return "zcode-api-key"
	}
	if p := decodeJWTPayload(s.ZCodeJWTToken); p != nil {
		if uid, ok := p["user_id"].(string); ok && uid != "" {
			return "zcode-" + uid
		}
		if uid, ok := p["sub"].(string); ok && uid != "" {
			return "zcode-" + uid
		}
	}
	return "zcode-account"
}

func handleAuthParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}

	// Accepted material shapes:
	//   1. JSON with zcode_jwt_token or api_key.
	//   2. A bare JWT / API key, including a JSON string submitted by the panel.
	//   3. JSON with a common token field.
	raw := req.RawJSON
	if len(raw) == 0 {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	var declared struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &declared)
	if typ := strings.ToLower(strings.TrimSpace(declared.Type)); typ != "" && typ != ProviderZCode {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	} else if typ == "" {
		routed := strings.EqualFold(strings.TrimSpace(req.Provider), ProviderZCode)
		prefixed := strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.FileName)), ProviderZCode+"-")
		if !routed && !prefixed {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
		}
	}

	var s authStorage
	trimmedInput := strings.TrimSpace(string(raw))
	var decodedInput string
	if json.Unmarshal(raw, &decodedInput) == nil {
		trimmedInput = strings.TrimSpace(decodedInput)
	}
	if strings.HasPrefix(strings.ToLower(trimmedInput), "bigmodel-coding:") {
		key := strings.TrimSpace(trimmedInput[len("bigmodel-coding:"):])
		if !looksLikeAPIKey(key) {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
		}
		s = authStorage{Provider: "bigmodel", APIKey: key, Source: "coding-plan-key"}
	}
	unmarshalOK := s.APIKey != "" || json.Unmarshal(raw, &s) == nil
	if strings.EqualFold(s.Provider, "bigmodel") && (s.ZCodeJWTToken != "" || s.APIKey == "") {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if !unmarshalOK || (s.ZCodeJWTToken == "" && s.APIKey == "") {
		trimmed := strings.TrimSpace(string(raw))
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			trimmed = strings.TrimSpace(text)
		}
		if looksLikeJWT(trimmed) {
			s = authStorage{ZCodeJWTToken: trimmed}
		} else if looksLikeAPIKey(trimmed) {
			s = authStorage{APIKey: trimmed}
		} else {
			var probe map[string]string
			if err := json.Unmarshal(raw, &probe); err == nil {
				for _, k := range []string{"zcode_jwt_token", "jwt", "access_token", "token"} {
					if v := strings.TrimSpace(probe[k]); looksLikeJWT(v) {
						s = authStorage{ZCodeJWTToken: v}
						break
					}
				}
				if s.ZCodeJWTToken == "" {
					if v := strings.TrimSpace(probe["api_key"]); looksLikeAPIKey(v) {
						s = authStorage{APIKey: v}
					}
				}
			}
			if s.ZCodeJWTToken == "" && s.APIKey == "" {
				return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
			}
		}
	}

	kind := "api-key"
	if strings.EqualFold(s.Provider, "bigmodel") {
		s.Provider, kind = "bigmodel", "coding-plan-key"
	} else {
		s.Provider = "zai"
		if s.ZCodeJWTToken != "" {
			kind = "jwt"
		}
	}
	s.Source = kind
	if err := ensureDeviceMID(&s); err != nil {
		return nil, fmt.Errorf("generate device MID: %w", err)
	}
	label := buildLabel(&s)
	storage, err := json.Marshal(&s)
	if err != nil {
		return nil, err
	}
	auth := pluginapi.AuthData{
		Provider:    ProviderZCode,
		ID:          "",
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: storage,
		Metadata:    map[string]any{"type": ProviderZCode, "source": kind, "credential_type": kind, "upstream_platform": s.Provider},
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

func looksLikeJWT(v string) bool {
	return len(v) > 40 && strings.Count(v, ".") == 2 && strings.HasPrefix(v, "eyJ")
}

func looksLikeAPIKey(v string) bool {
	v = strings.TrimSpace(v)
	return len(v) >= 20 && !strings.ContainsAny(v, " \t\r\n{}[]\"")
}

type zaiCLIInitData struct {
	FlowID          string `json:"flow_id"`
	PollToken       string `json:"poll_token"`
	AuthorizeURL    string `json:"authorize_url"`
	ExpiresAt       int64  `json:"expires_at"`
	PollIntervalSec int    `json:"poll_interval_sec"`
}

func startZaiCLILogin(hostCallbackID string) ([]byte, bool, error) {
	bearer, err := randomState(32)
	if err != nil {
		return nil, false, fmt.Errorf("generate Z.AI OAuth bearer: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"provider": "zai"})
	headers := http.Header{"Authorization": []string{"Bearer " + bearer}}
	data, err := requestRemoteData(hostCallbackID, http.MethodPost, ZCodeCLIInitURL, headers, body)
	if err != nil {
		if isZaiCLIExplicitlyUnsupported(err) {
			return nil, true, err
		}
		return nil, false, errors.New("Z.AI OAuth 初始化失败，请稍后重试")
	}

	var init zaiCLIInitData
	if err := json.Unmarshal(data, &init); err != nil {
		return nil, false, errors.New("Z.AI OAuth 初始化响应协议错误")
	}
	init.FlowID = strings.TrimSpace(init.FlowID)
	init.AuthorizeURL = strings.TrimSpace(init.AuthorizeURL)
	if init.FlowID == "" || init.AuthorizeURL == "" || init.ExpiresAt <= 0 || init.PollIntervalSec <= 0 {
		return nil, false, errors.New("Z.AI OAuth 初始化响应协议错误")
	}
	if strings.ContainsAny(init.FlowID, "/?#\\") {
		return nil, false, errors.New("Z.AI OAuth 初始化响应安全校验失败")
	}
	authorizeURL, err := url.Parse(init.AuthorizeURL)
	if err != nil || authorizeURL.Scheme != "https" || authorizeURL.Hostname() != "chat.z.ai" {
		return nil, false, errors.New("Z.AI OAuth 授权地址安全校验失败")
	}
	expiresAt := time.Unix(init.ExpiresAt, 0).UTC()
	if !expiresAt.After(time.Now()) {
		return nil, false, errors.New("Z.AI OAuth 初始化响应已过期")
	}

	state, err := randomState(32)
	if err != nil {
		return nil, false, fmt.Errorf("generate Z.AI OAuth state: %w", err)
	}
	oauthCallbacks.Lock()
	for key, callback := range oauthCallbacks.items {
		if time.Now().After(callback.ExpiresAt) {
			delete(oauthCallbacks.items, key)
		}
	}
	oauthCallbacks.items[state] = oauthCallback{
		Provider:        "zai",
		FlowID:          init.FlowID,
		PollBearer:      bearer,
		PollIntervalSec: init.PollIntervalSec,
		BoundHost:       zcodeOAuthHost,
		ExpiresAt:       expiresAt,
		ServerMediated:  true,
	}
	oauthCallbacks.Unlock()

	out, err := okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  ProviderZCode,
		URL:       init.AuthorizeURL,
		State:     state,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			"provider":          "zai",
			"flow":              "server-mediated",
			"poll_interval_sec": init.PollIntervalSec,
		},
	})
	return out, false, err
}

func isZaiCLIExplicitlyUnsupported(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "status=404") ||
		strings.Contains(text, "status 404") ||
		strings.Contains(text, "not found") ||
		strings.Contains(text, "not supported") ||
		strings.Contains(text, "unsupported")
}

func handleAuthLoginStart(request []byte) ([]byte, error) {
	var rpcReq rpcAuthLoginStartRequest
	if err := json.Unmarshal(request, &rpcReq); err != nil {
		return nil, err
	}
	provider := strings.ToLower(strings.TrimSpace(rpcReq.Provider))
	if provider == "" || strings.EqualFold(provider, ProviderZCode) {
		provider = "zai"
	}
	if provider != "zai" && provider != "bigmodel" {
		return nil, fmt.Errorf("unsupported OAuth provider %q", provider)
	}
	if provider == "zai" {
		response, unsupported, err := startZaiCLILogin(rpcReq.HostCallbackID)
		if err == nil {
			return response, nil
		}
		if !unsupported {
			return nil, err
		}
	}
	state, err := randomState(32)
	if err != nil {
		return nil, fmt.Errorf("generate Z.AI OAuth state: %w", err)
	}
	expiresAt := time.Now().Add(10 * time.Minute).UTC()
	oauthCallbacks.Lock()
	for key, callback := range oauthCallbacks.items {
		if time.Now().After(callback.ExpiresAt) {
			delete(oauthCallbacks.items, key)
		}
	}
	oauthCallbacks.items[state] = oauthCallback{Provider: provider, ExpiresAt: expiresAt}
	oauthCallbacks.Unlock()

	var u url.URL
	if provider == "bigmodel" {
		u = url.URL{Scheme: "https", Host: "bigmodel.cn", Path: "/login"}
		q := u.Query()
		q.Set("appId", BigModelAppID)
		q.Set("redirect", ZCodeRedirectURI)
		q.Set("state", state)
		u.RawQuery = q.Encode()
	} else {
		u = url.URL{Scheme: "https", Host: "chat.z.ai", Path: "/api/oauth/authorize"}
		q := u.Query()
		q.Set("response_type", "code")
		q.Set("client_id", ZCodeAppID)
		q.Set("redirect_uri", ZCodeRedirectURI)
		q.Set("state", state)
		u.RawQuery = q.Encode()
	}

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  ProviderZCode,
		URL:       u.String(),
		State:     state,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			"provider":     provider,
			"state":        state,
			"redirect_uri": ZCodeRedirectURI,
		},
	})
}

func handleAuthLoginPoll(request []byte) ([]byte, error) {
	var rpcReq rpcAuthLoginPollRequest
	if err := json.Unmarshal(request, &rpcReq); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(rpcReq.State)
	oauthCallbacks.Lock()
	callback, exists := oauthCallbacks.items[state]
	if !exists {
		oauthCallbacks.Unlock()
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Z.AI 登录会话不存在或已消费，请重新登录。"})
	}
	claimed := callback.Code != "" && callback.Error == "" && !callback.Processing
	if claimed {
		stored := callback
		stored.Processing = true
		oauthCallbacks.items[state] = stored
	}
	oauthCallbacks.Unlock()
	if time.Now().After(callback.ExpiresAt) {
		removeOAuthCallback(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Z.AI 授权已过期，请重新登录。"})
	}
	if callback.ServerMediated {
		if callback.Provider != "zai" || callback.BoundHost != zcodeOAuthHost || callback.FlowID == "" || callback.PollBearer == "" {
			removeOAuthCallback(state)
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Z.AI OAuth 会话安全校验失败，请重新登录。"})
		}
		requestedProvider := strings.ToLower(strings.TrimSpace(rpcReq.Provider))
		if requestedProvider == ProviderZCode {
			requestedProvider = "zai"
		}
		if requestedProvider != "" && requestedProvider != "zai" {
			removeOAuthCallback(state)
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "OAuth 登录平台与会话不一致，请重新登录。"})
		}
		oauthCallbacks.Lock()
		stored, ok := oauthCallbacks.items[state]
		if !ok || stored.Processing {
			oauthCallbacks.Unlock()
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending})
		}
		stored.Processing = true
		oauthCallbacks.items[state] = stored
		oauthCallbacks.Unlock()

		tokens, status, err := pollZaiCLILogin(rpcReq.HostCallbackID, callback)
		if status == "pending" && err == nil {
			oauthCallbacks.Lock()
			if current, ok := oauthCallbacks.items[state]; ok {
				current.Processing = false
				oauthCallbacks.items[state] = current
			}
			oauthCallbacks.Unlock()
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending})
		}
		removeOAuthCallback(state)
		if err != nil {
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: err.Error()})
		}
		return finishOAuthLogin(rpcReq.HostCallbackID, "zai", tokens)
	}
	if callback.Error != "" {
		removeOAuthCallback(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Z.AI 授权失败：" + callback.Error})
	}
	if callback.Code == "" || !claimed {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending})
	}

	provider := callback.Provider
	if provider == "" || strings.EqualFold(provider, ProviderZCode) {
		provider = "zai"
	}
	// The provider recorded when the OAuth session was created is authoritative.
	// A poll request must not be able to switch a Z.AI state into BigModel (or
	// vice versa) and send the authorization code to a different token endpoint.
	requestedProvider := strings.ToLower(strings.TrimSpace(rpcReq.Provider))
	if requestedProvider == ProviderZCode {
		requestedProvider = "zai"
	}
	if requestedProvider != "" && requestedProvider != provider {
		removeOAuthCallback(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "OAuth 登录平台与会话不一致，请重新登录。"})
	}
	tokens, err := exchangeOAuthCodeForProvider(rpcReq.HostCallbackID, callback.Code, state, provider)
	if err != nil {
		removeOAuthCallback(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Z.AI 授权码兑换失败，请重新登录。"})
	}
	removeOAuthCallback(state)

	storage := authStorage{
		UserID:    tokens.UserID,
		UserLabel: tokens.UserLabel,
		Provider:  provider,
	}
	if provider == "bigmodel" {
		apiKey, errAPIKey := resolveBigModelAPIKey(rpcReq.HostCallbackID, tokens.AccessToken)
		if errAPIKey != nil {
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "BigModel 未找到现有 API Key，请先在平台创建。"})
		}
		storage.APIKey = apiKey
		storage.Source = "coding-plan-key"
	} else {
		// Z.AI OAuth is a Coding Plan login: persist the JWT by default and do
		// not silently turn it into a paid API-key credential. An explicitly
		// configured dual-credential mode may only read an existing key.
		storage.ZCodeJWTToken = tokens.JWTToken
		storage.Source = "jwt"
		if currentRouteConfig().RetainDual {
			if apiKey, errAPIKey := resolveZaiAPIKey(rpcReq.HostCallbackID, tokens.AccessToken); errAPIKey == nil {
				storage.APIKey = apiKey
				storage.Source = "dual"
			}
		}
	}
	if err := ensureDeviceMID(&storage); err != nil {
		return nil, fmt.Errorf("generate device MID: %w", err)
	}
	storageJSON, err := json.Marshal(storage)
	if err != nil {
		return nil, err
	}
	label := buildLabel(&storage)
	fileLabel := safeFileLabel(tokens.UserID)
	filePrefix := "zcode-"
	if provider == "bigmodel" {
		filePrefix = "zcode-bigmodel-"
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth: pluginapi.AuthData{
			Provider:    ProviderZCode,
			FileName:    filePrefix + fileLabel + ".json",
			Label:       label,
			StorageJSON: storageJSON,
			Metadata: map[string]any{
				"type":              ProviderZCode,
				"source":            storage.Source,
				"credential_type":   storage.Source,
				"api_key_exchange":  map[bool]string{true: "success", false: "fallback-jwt"}[storage.APIKey != ""],
				"upstream_platform": provider,
			},
		},
	})
}

type zaiCLIPollData struct {
	Status string `json:"status"`
	Token  string `json:"token"`
	User   struct {
		UserID string `json:"user_id"`
		Name   string `json:"name"`
	} `json:"user"`
	ZAI struct {
		AccessToken string `json:"access_token"`
	} `json:"zai"`
}

func pollZaiCLILogin(hostCallbackID string, callback oauthCallback) (oauthTokens, string, error) {
	if callback.BoundHost != zcodeOAuthHost || callback.FlowID == "" || callback.PollBearer == "" {
		return oauthTokens{}, "error", errors.New("Z.AI OAuth 会话安全校验失败，请重新登录。")
	}
	if time.Now().After(callback.ExpiresAt) {
		return oauthTokens{}, "expired", errors.New("Z.AI 授权已过期，请重新登录。")
	}
	if strings.ContainsAny(callback.FlowID, "/?#\\") {
		return oauthTokens{}, "error", errors.New("Z.AI OAuth 会话安全校验失败，请重新登录。")
	}

	pollURL := ZCodeCLIPollBaseURL + url.PathEscape(callback.FlowID)
	parsed, err := url.Parse(pollURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != callback.BoundHost || parsed.Path != "/api/v1/oauth/cli/poll/"+url.PathEscape(callback.FlowID) {
		return oauthTokens{}, "error", errors.New("Z.AI OAuth 轮询地址安全校验失败，请重新登录。")
	}
	headers := http.Header{"Authorization": []string{"Bearer " + callback.PollBearer}}
	data, err := requestRemoteData(hostCallbackID, http.MethodGet, pollURL, headers, nil)
	if err != nil {
		return oauthTokens{}, "error", errors.New("Z.AI OAuth 轮询失败，请稍后重新登录。")
	}

	var poll zaiCLIPollData
	if err := json.Unmarshal(data, &poll); err != nil {
		return oauthTokens{}, "error", errors.New("Z.AI OAuth 轮询响应协议错误，请重新登录。")
	}
	switch strings.ToLower(strings.TrimSpace(poll.Status)) {
	case "pending":
		return oauthTokens{}, "pending", nil
	case "failed", "denied", "rejected":
		return oauthTokens{}, "rejected", errors.New("Z.AI 授权被拒绝，请重新登录。")
	case "expired":
		return oauthTokens{}, "expired", errors.New("Z.AI 授权已过期，请重新登录。")
	case "ready":
		accessToken := strings.TrimSpace(poll.ZAI.AccessToken)
		jwtToken := strings.TrimSpace(poll.Token)
		if accessToken == "" || jwtToken == "" {
			return oauthTokens{}, "error", errors.New("Z.AI OAuth 轮询响应协议错误，请重新登录。")
		}
		return oauthTokens{
			AccessToken: accessToken,
			JWTToken:    jwtToken,
			UserID:      strings.TrimSpace(poll.User.UserID),
			UserLabel:   strings.TrimSpace(poll.User.Name),
		}, "ready", nil
	default:
		return oauthTokens{}, "error", errors.New("Z.AI OAuth 轮询响应协议错误，请重新登录。")
	}
}

func finishOAuthLogin(hostCallbackID, provider string, tokens oauthTokens) ([]byte, error) {
	storage := authStorage{
		UserID:    tokens.UserID,
		UserLabel: tokens.UserLabel,
		Provider:  provider,
	}
	if provider == "bigmodel" {
		apiKey, err := resolveBigModelAPIKey(hostCallbackID, tokens.AccessToken)
		if err != nil {
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "BigModel 未找到现有 API Key，请先在平台创建。"})
		}
		storage.APIKey = apiKey
		storage.Source = "coding-plan-key"
	} else {
		storage.ZCodeJWTToken = tokens.JWTToken
		storage.Source = "jwt"
		if currentRouteConfig().RetainDual {
			if apiKey, err := resolveZaiAPIKey(hostCallbackID, tokens.AccessToken); err == nil {
				storage.APIKey = apiKey
				storage.Source = "dual"
			}
		}
	}
	if err := ensureDeviceMID(&storage); err != nil {
		return nil, fmt.Errorf("generate device MID: %w", err)
	}
	storageJSON, err := json.Marshal(storage)
	if err != nil {
		return nil, err
	}
	filePrefix := "zcode-"
	if provider == "bigmodel" {
		filePrefix = "zcode-bigmodel-"
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth: pluginapi.AuthData{
			Provider:    ProviderZCode,
			FileName:    filePrefix + safeFileLabel(tokens.UserID) + ".json",
			Label:       buildLabel(&storage),
			StorageJSON: storageJSON,
			Metadata: map[string]any{
				"type":              ProviderZCode,
				"source":            storage.Source,
				"credential_type":   storage.Source,
				"api_key_exchange":  map[bool]string{true: "success", false: "fallback-jwt"}[storage.APIKey != ""],
				"upstream_platform": provider,
			},
		},
	})
}

func removeOAuthCallback(state string) {
	oauthCallbacks.Lock()
	delete(oauthCallbacks.items, state)
	oauthCallbacks.Unlock()
}

func saveOAuthCallback(state, code, errorMessage string) bool {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	errorMessage = strings.TrimSpace(errorMessage)
	if (code == "") == (errorMessage == "") {
		return false
	}

	oauthCallbacks.Lock()
	defer oauthCallbacks.Unlock()
	callback, ok := oauthCallbacks.items[state]
	if !ok || time.Now().After(callback.ExpiresAt) {
		delete(oauthCallbacks.items, state)
		return false
	}
	if callback.ServerMediated || callback.Code != "" || callback.Error != "" || callback.Processing {
		return false
	}
	callback.Code = code
	callback.Error = errorMessage
	oauthCallbacks.items[state] = callback
	return true
}

type oauthTokens struct {
	AccessToken string
	JWTToken    string
	UserID      string
	UserLabel   string
}

type remoteEnvelope struct {
	Code any             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func exchangeOAuthCode(hostCallbackID, code, state string) (oauthTokens, error) {
	return exchangeOAuthCodeForProvider(hostCallbackID, code, state, "zai")
}

func exchangeOAuthCodeForProvider(hostCallbackID, code, state, provider string) (oauthTokens, error) {
	body, _ := json.Marshal(map[string]string{
		"provider":     provider,
		"code":         code,
		"redirect_uri": ZCodeRedirectURI,
		"state":        state,
	})
	data, err := requestRemoteData(hostCallbackID, http.MethodPost, ZCodeTokenURL, nil, body)
	if err != nil {
		return oauthTokens{}, fmt.Errorf("Z.AI token 兑换失败：%w", err)
	}
	return parseOAuthTokensForProvider(data, provider)
}

// parseOAuthTokens accepts the current ZCode token-exchange fields without
// retaining OAuth access or refresh tokens in persisted credential storage.
func parseOAuthTokens(data json.RawMessage) (oauthTokens, error) {
	return parseOAuthTokensForProvider(data, "zai")
}

func parseOAuthTokensForProvider(data json.RawMessage, provider string) (oauthTokens, error) {
	var payload struct {
		Token string `json:"token"`
		ZAI   struct {
			AccessToken string `json:"access_token"`
		} `json:"zai"`
		BigModel struct {
			AccessToken string `json:"access_token"`
		} `json:"bigmodel"`
		User struct {
			UserID   string `json:"user_id"`
			ID       string `json:"id"`
			Name     string `json:"name"`
			Username string `json:"username"`
			Email    string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return oauthTokens{}, errors.New("Z.AI token 响应格式无效")
	}
	jwt := strings.TrimSpace(payload.Token)
	accessToken := strings.TrimSpace(payload.ZAI.AccessToken)
	if provider == "bigmodel" {
		accessToken = strings.TrimSpace(payload.BigModel.AccessToken)
	}
	if jwt == "" || accessToken == "" {
		return oauthTokens{}, errors.New("Z.AI token 响应缺少凭证")
	}
	return oauthTokens{
		AccessToken: accessToken,
		JWTToken:    jwt,
		UserID:      firstString(payload.User.UserID, payload.User.ID),
		UserLabel:   firstString(payload.User.Name, payload.User.Username, payload.User.Email),
	}, nil
}

func resolveBigModelAPIKey(hostCallbackID, accessToken string) (string, error) {
	headers := http.Header{"Authorization": []string{accessToken}}
	customerData, err := requestRemoteData(hostCallbackID, http.MethodGet, bigModelCustomerURL, headers, nil)
	if err != nil {
		return "", err
	}
	organizationID, projectID := pickOrganizationProject(customerData)
	if organizationID == "" || projectID == "" {
		return "", errors.New("无法确定 BigModel organization/project")
	}
	keysURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", bigModelAPIHost, url.PathEscape(organizationID), url.PathEscape(projectID))
	keysData, err := requestRemoteData(hostCallbackID, http.MethodGet, keysURL, headers, nil)
	if err != nil {
		return "", err
	}
	apiKeyID := findNamedAPIKeyID(keysData, zcodeAPIKeyName)
	if apiKeyID == "" {
		return "", errors.New("BigModel 未找到现有 zcode-api-key")
	}
	copied, err := requestRemoteData(hostCallbackID, http.MethodGet, keysURL+"/copy/"+url.PathEscape(apiKeyID), headers, nil)
	if err != nil {
		return "", err
	}
	var secret struct {
		SecretKey string `json:"secretKey"`
		APIKey    string `json:"apiKey"`
	}
	if err := json.Unmarshal(copied, &secret); err != nil {
		return "", err
	}
	secretKey := firstString(secret.SecretKey, secret.APIKey)
	if secretKey == "" {
		return apiKeyID, nil
	}
	if strings.Contains(secretKey, ".") {
		return secretKey, nil
	}
	return apiKeyID + "." + secretKey, nil
}

func resolveZaiAPIKey(hostCallbackID, accessToken string) (string, error) {
	loginBody, _ := json.Marshal(map[string]string{"token": accessToken})
	loginData, err := requestRemoteData(hostCallbackID, http.MethodPost, ZCodeBusinessLogin, nil, loginBody)
	if err != nil {
		return "", err
	}
	var login struct {
		AccessToken  string `json:"access_token"`
		AccessToken2 string `json:"accessToken"`
	}
	if err := json.Unmarshal(loginData, &login); err != nil {
		return "", err
	}
	bizToken := firstString(login.AccessToken, login.AccessToken2)
	if bizToken == "" {
		return "", errors.New("Z.AI business token 响应缺少 access_token")
	}
	headers := http.Header{"Authorization": []string{"Bearer " + bizToken}}
	customerData, err := requestRemoteData(hostCallbackID, http.MethodGet, zcodeCustomerURL, headers, nil)
	if err != nil {
		return "", err
	}
	organizationID, projectID := pickOrganizationProject(customerData)
	if organizationID == "" || projectID == "" {
		return "", errors.New("无法确定 Z.AI organization/project")
	}
	keysURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", zcodeAPIHost, url.PathEscape(organizationID), url.PathEscape(projectID))
	keysData, err := requestRemoteData(hostCallbackID, http.MethodGet, keysURL, headers, nil)
	if err != nil {
		return "", err
	}
	apiKeyID := findAPIKeyID(keysData, zcodeAPIKeyName)
	if apiKeyID == "" {
		return "", errors.New("Z.AI 未找到现有 API Key，请先在官方平台创建")
	}
	copied, err := requestRemoteData(hostCallbackID, http.MethodGet, keysURL+"/copy/"+url.PathEscape(apiKeyID), headers, nil)
	if err != nil {
		return "", err
	}
	var secret struct {
		SecretKey string `json:"secretKey"`
		APIKey    string `json:"apiKey"`
	}
	if err := json.Unmarshal(copied, &secret); err != nil {
		return "", err
	}
	secretKey := firstString(secret.SecretKey, secret.APIKey)
	if secretKey == "" {
		return "", errors.New("Z.AI API Key copy 响应缺少 secretKey")
	}
	if strings.Contains(secretKey, ".") {
		return secretKey, nil
	}
	return apiKeyID + "." + secretKey, nil
}

func requestRemoteData(hostCallbackID, method, requestURL string, headers http.Header, body []byte) (json.RawMessage, error) {
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	headers.Set("Content-Type", "application/json")
	resp, err := executeHTTP(context.Background(), nil, hostCallbackID, pluginapi.HTTPRequest{
		Method: method, URL: requestURL, Headers: headers, Body: body,
	})
	if err != nil {
		return nil, err
	}
	var envelope remoteEnvelope
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return nil, fmt.Errorf("upstream HTTP %d returned invalid JSON", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !remoteCodeOK(envelope.Code) {
		message := strings.TrimSpace(envelope.Msg)
		if message == "" {
			message = fmt.Sprintf("upstream HTTP %d", resp.StatusCode)
		}
		return nil, errors.New(message)
	}
	return envelope.Data, nil
}

func remoteCodeOK(code any) bool {
	switch value := code.(type) {
	case nil:
		return true
	case float64:
		return value == 0 || value == 200
	case string:
		return value == "0" || value == "200"
	default:
		return false
	}
}

func pickOrganizationProject(data json.RawMessage) (string, string) {
	var customer struct {
		Organizations []struct {
			OrganizationID   string `json:"organizationId"`
			OrganizationName string `json:"organizationName"`
			Projects         []struct {
				ProjectID   string `json:"projectId"`
				ProjectName string `json:"projectName"`
			} `json:"projects"`
		} `json:"organizations"`
	}
	if json.Unmarshal(data, &customer) != nil {
		return "", ""
	}
	for _, organization := range customer.Organizations {
		for _, project := range organization.Projects {
			if organization.OrganizationID != "" && project.ProjectID != "" {
				return organization.OrganizationID, project.ProjectID
			}
		}
	}
	return "", ""
}

func findNamedAPIKeyID(data json.RawMessage, preferredName string) string {
	var list []struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
		ID     string `json:"id"`
	}
	if json.Unmarshal(data, &list) != nil {
		return ""
	}
	for _, item := range list {
		if item.Name == preferredName {
			return firstString(item.APIKey, item.ID)
		}
	}
	return ""
}

func findAPIKeyID(data json.RawMessage, preferredName string) string {
	var list []struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
		ID     string `json:"id"`
	}
	if json.Unmarshal(data, &list) == nil {
		for _, item := range list {
			if item.Name == preferredName {
				return firstString(item.APIKey, item.ID)
			}
		}
	}
	var single struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
		ID     string `json:"id"`
	}
	if json.Unmarshal(data, &single) == nil && single.Name == preferredName {
		return firstString(single.APIKey, single.ID)
	}
	return ""
}

func firstString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func fallbackFileLabel() string {
	if value, err := randomState(6); err == nil {
		return value
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func safeFileLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallbackFileLabel()
	}
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return fallbackFileLabel()
	}
	return b.String()
}

func handleAuthRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	s, err := parseAuthStorage(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	if err := ensureDeviceMID(s); err != nil {
		return nil, fmt.Errorf("generate device MID: %w", err)
	}
	storageJSON, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	kind := "api-key"
	if s.ZCodeJWTToken != "" {
		kind = "jwt"
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    ProviderZCode,
			ID:          req.AuthID,
			Label:       buildLabel(s),
			StorageJSON: storageJSON,
			Metadata:    map[string]any{"type": ProviderZCode, "source": kind, "credential_type": kind, "upstream_platform": s.Provider},
		},
		NextRefreshAfter: time.Now().Add(24 * time.Hour).UTC(),
	})
}

// ensureDeviceMID creates a UUIDv4 once and reuses the value persisted in authStorage.
func ensureDeviceMID(storage *authStorage) error {
	if strings.TrimSpace(storage.DeviceMID) != "" {
		return nil
	}
	bytes := make([]byte, 16)
	if _, err := crand.Read(bytes); err != nil {
		return err
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	hexMID := hex.EncodeToString(bytes)
	storage.DeviceMID = fmt.Sprintf("%s-%s-%s-%s-%s",
		hexMID[0:8], hexMID[8:12], hexMID[12:16], hexMID[16:20], hexMID[20:32])
	return nil
}

// randomState returns n cryptographically random bytes encoded as lowercase hex.
func randomState(n int) (string, error) {
	rb := make([]byte, n)
	if _, err := crand.Read(rb); err != nil {
		return "", err
	}
	return hex.EncodeToString(rb), nil
}
