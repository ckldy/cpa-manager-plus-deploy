package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}
type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}
type registrationCapability struct {
	ModelRegistrar        bool                         `json:"model_registrar"`
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	FrontendAuthProvider  bool                         `json:"frontend_auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
}
type identifierResponse struct {
	Identifier string `json:"identifier"`
}
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcAuthRefreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcHostHTTPRequest struct {
	HostCallbackID string                `json:"host_callback_id,omitempty"`
	Request        pluginapi.HTTPRequest `json:"request"`
}
type rpcHostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}
type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}
type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}
type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}
type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}
type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}
type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}
type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

var loginSessions = newLoginSessionStore(10 * time.Minute)
var csrfState struct {
	sync.RWMutex
	token string
}

type publicLoginClaim struct{ expires time.Time }

var publicLoginClaims struct {
	sync.Mutex
	items map[[32]byte]*publicLoginClaim
}

func reservePublicLogin(parsed loginCallback, now time.Time) (func(bool), bool) {
	fingerprint := sha256.Sum256([]byte(firstNonEmpty(parsed.RefreshToken, parsed.UserJWT.RefreshToken) + "\x00" + parsed.UserJWT.Token))
	publicLoginClaims.Lock()
	if publicLoginClaims.items == nil {
		publicLoginClaims.items = make(map[[32]byte]*publicLoginClaim)
	}
	for key, claim := range publicLoginClaims.items {
		if !now.Before(claim.expires) {
			delete(publicLoginClaims.items, key)
		}
	}
	if _, exists := publicLoginClaims.items[fingerprint]; exists {
		publicLoginClaims.Unlock()
		return nil, false
	}
	claim := &publicLoginClaim{expires: now.Add(10 * time.Minute)}
	publicLoginClaims.items[fingerprint] = claim
	publicLoginClaims.Unlock()
	return func(success bool) {
		publicLoginClaims.Lock()
		defer publicLoginClaims.Unlock()
		if publicLoginClaims.items[fingerprint] != claim {
			return
		}
		if !success {
			delete(publicLoginClaims.items, fingerprint)
			return
		}
		claim.expires = time.Now().Add(10 * time.Minute)
	}, true
}

func initCSRF() string {
	csrfState.Lock()
	defer csrfState.Unlock()
	if csrfState.token == "" {
		b := make([]byte, 32)
		if _, e := rand.Read(b); e == nil {
			csrfState.token = hex.EncodeToString(b)
		}
	}
	return csrfState.token
}
func validCSRF(req pluginapi.ManagementRequest, values url.Values) bool {
	token := initCSRF()
	return token != "" && values.Get("csrf") == token && strings.EqualFold(req.Headers.Get("X-Requested-With"), "traework-panel")
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		startRuntime()
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRegister:
		return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: ProviderTraeWork, Models: traeModels()})
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: ProviderTraeWork, Models: traeModels()})
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{ProviderTraeWork})
	case pluginabi.MethodAuthLoginStart:
		return handleAuthLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return handleAuthLoginPoll(request)
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefreshRPC(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{ProviderTraeWork})
	case pluginabi.MethodExecutorExecute:
		return handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecutorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return handleExecutorCountTokens(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
func pluginRegistration() registration {
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{Name: ProviderTraeWork, Version: "0.1.0", Author: "ckldy", GitHubRepository: "https://github.com/Sliverkiss/traework2api"}, Capabilities: registrationCapability{ModelProvider: true, AuthProvider: true, FrontendAuthProvider: true, Executor: true, ExecutorModelScope: pluginapi.ExecutorModelScopeBoth, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, ManagementAPI: true}}
}
func okEnvelope(v any) ([]byte, error) {
	r, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return json.Marshal(envelope{OK: true, Result: r})
}
func errorEnvelope(c, m string) []byte {
	r, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{c, m}})
	return r
}
func handleExecutorCountTokens([]byte) ([]byte, error) {
	return errorEnvelope("unsupported", "count_tokens is unsupported for traework"), nil
}

func handleAuthLoginStart(raw []byte) ([]byte, error) {
	var req rpcAuthLoginStartRequest
	if len(raw) > 0 {
		if e := json.Unmarshal(raw, &req); e != nil {
			return nil, e
		}
		if p := strings.TrimSpace(req.Provider); p != "" && p != ProviderTraeWork {
			return nil, errors.New("provider mismatch")
		}
	}
	// TRAE 只接受回环回调地址（http://127.0.0.1:18080/authorize），公网 host 会被
	// 授权页直接拒绝，所以无论宿主还是本面板发起，都必须由管理员粘贴回调 URL。
	s, e := loginSessions.New(time.Now())
	if e != nil {
		return nil, e
	}
	u, e := buildLoginURL(s)
	if e != nil {
		return nil, e
	}
	prompt := "完成 TRAE Work 授权后粘贴本机回调 URL。"
	return okEnvelope(pluginapi.AuthLoginStartResponse{Provider: ProviderTraeWork, URL: u, State: s.State, ExpiresAt: s.ExpiresAt.UTC(), Metadata: map[string]any{"prompt": prompt}})
}
func handleAuthLoginPoll(raw []byte) ([]byte, error) {
	var req rpcAuthLoginPollRequest
	if e := json.Unmarshal(raw, &req); e != nil {
		return nil, e
	}
	if req.Provider != ProviderTraeWork {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "provider mismatch"})
	}
	now := time.Now()
	cb, _ := req.Metadata["callback_url"].(string)
	if strings.TrimSpace(cb) == "" {
		sess, alive := loginSessions.Peek(strings.TrimSpace(req.State), now)
		if !alive {
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "登录会话不存在或已过期，请重新发起登录"})
		}
		// 官方面板轮询超过 2 分钟仍无回调：TRAE 浏览器回调固定本机、永远到不了 CPA，
		// 主动失败并指引到插件面板粘贴，避免用户无限等待。
		if now.Add(8 * time.Minute).After(sess.ExpiresAt) {
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "TRAE 的浏览器回调固定指向本机 127" + ".0.0.1:18080，无法自动送达 CPA。请打开 TRAE Work 插件面板，把浏览器地址栏里的完整回调 URL 粘贴进输入框，点「完成登录」"})
		}
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "等待浏览器完成授权…"})
	}
	if len(cb) > 64<<10 {
		return nil, errors.New("callback too large")
	}
	s, e := loginSessions.Consume(req.State, now)
	if e != nil {
		// 粘贴的回调 URL 自带全部凭据（与上游 login.sh 一致），state 缺失/过期
		// （官方面板发起、页面刷新、跨页粘贴）时回退一次性新会话补齐元数据。
		if s, e = loginSessions.New(now); e != nil {
			return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: e.Error()})
		}
	}
	parsed, e := parseLoginCallback(cb)
	if e != nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "invalid callback: " + e.Error()})
	}
	auth, e := completeLoginExchange(hostHTTPDoer{req.HostCallbackID}, s, parsed)
	if e != nil {
		return nil, e
	}
	auth.StorageJSON = refreshTraeModelsInStorage(req.HostCallbackID, auth.StorageJSON)
	return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: auth})
}

func handleAuthRefreshRPC(raw []byte) ([]byte, error) {
	var req rpcAuthRefreshRequest
	if e := json.Unmarshal(raw, &req); e != nil {
		return nil, e
	}
	inner, _ := json.Marshal(req.AuthRefreshRequest)
	return handleAuthRefreshWithClient(context.Background(), hostHTTPDoer{req.HostCallbackID}, inner, time.Now())
}

func managementRegistration() managementRegistrationResponse {
	base := "/v0/management/plugins/" + ProviderTraeWork
	return managementRegistrationResponse{Routes: []pluginapi.ManagementRoute{{Method: http.MethodPost, Path: base + "/login/start"}, {Method: http.MethodPost, Path: base + "/login/poll"}, {Method: http.MethodGet, Path: base + "/accounts"}, {Method: http.MethodPost, Path: base + "/enabled"}, {Method: http.MethodPost, Path: base + "/delete"}, {Method: http.MethodPost, Path: base + "/credits"}, {Method: http.MethodPost, Path: base + "/checkin"}, {Method: http.MethodPost, Path: base + "/models/refresh"}}, Resources: []pluginapi.ResourceRoute{{Path: "/panel", Menu: "TRAE Work", Description: "TRAE Work 登录、积分、签到和模型管理。"}, {Path: "/status", Description: "TRAE Work health status."}}}
}
func managementResponse(status int, contentType string, body []byte) ([]byte, error) {
	h := http.Header{"Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"}}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body})
}
func jsonManagement(status int, v any) ([]byte, error) {
	b, _ := json.Marshal(v)
	return managementResponse(status, "application/json; charset=utf-8", b)
}
func formValues(req pluginapi.ManagementRequest) (url.Values, error) {
	if len(req.Body) > 64<<10 {
		return nil, errors.New("request body too large")
	}
	return url.ParseQuery(string(req.Body))
}

// publicActionFields lists every form field the public resource POST endpoint
// accepts. Actions other than login (start/finish) — credits, checkin, models,
// enabled, delete — are routed through the same unauthenticated public resource
// route so the panel never needs a management admin key.
var publicActionFields = map[string]bool{
	"action":       true,
	"csrf":         true,
	"callback_url": true,
	"auth_index":   true,
	"name":         true,
	"uid":          true,
	"enabled":      true,
	"confirm":      true,
}

func singlePublicLoginForm(values url.Values) bool {
	for key, entries := range values {
		if len(entries) != 1 || !publicActionFields[key] {
			return false
		}
	}
	return len(values["action"]) == 1 && len(values["csrf"]) == 1
}

func publicLoginJSON(status int, body map[string]any) ([]byte, error) {
	return jsonManagement(status, body)
}

func handlePublicLogin(rpc rpcManagementRequest) ([]byte, error) {
	req := rpc.ManagementRequest
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(req.Headers.Get("Content-Type"), ";")[0]))
	if contentType != "application/x-www-form-urlencoded" {
		return publicLoginJSON(http.StatusUnsupportedMediaType, map[string]any{"error": "form content type required"})
	}
	if len(req.Body) > 64<<10 {
		return publicLoginJSON(http.StatusRequestEntityTooLarge, map[string]any{"error": "request too large"})
	}
	values, err := url.ParseQuery(string(req.Body))
	if err != nil || !singlePublicLoginForm(values) {
		return publicLoginJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
	}
	if !validCSRF(req, values) {
		return publicLoginJSON(http.StatusForbidden, map[string]any{"error": "CSRF rejected"})
	}

	switch values.Get("action") {
	case "start":
		if len(values) != 2 {
			return publicLoginJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
		out, err := handleAuthLoginStart(nil)
		if err != nil {
			return publicLoginJSON(http.StatusServiceUnavailable, map[string]any{"error": "login start failed"})
		}
		var result struct {
			Result pluginapi.AuthLoginStartResponse `json:"result"`
		}
		if json.Unmarshal(out, &result) != nil || result.Result.URL == "" || result.Result.State == "" {
			return publicLoginJSON(http.StatusServiceUnavailable, map[string]any{"error": "login start failed"})
		}
		return publicLoginJSON(http.StatusOK, map[string]any{"url": result.Result.URL, "state": result.Result.State, "expires_at": result.Result.ExpiresAt})
	case "finish":
		if len(values) != 3 || len(values["callback_url"]) != 1 {
			return publicLoginJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
		callbackURL := strings.TrimSpace(values.Get("callback_url"))
		if callbackURL == "" || len(callbackURL) > 64<<10 {
			return publicLoginJSON(http.StatusBadRequest, map[string]any{"error": "invalid callback"})
		}
		parsed, err := parseLoginCallback(callbackURL)
		if err != nil {
			return publicLoginJSON(http.StatusBadRequest, map[string]any{"error": "invalid callback"})
		}
		finishClaim, ok := reservePublicLogin(parsed, time.Now())
		if !ok {
			return publicLoginJSON(http.StatusConflict, map[string]any{"error": "callback already submitted"})
		}
		success := false
		defer func() { finishClaim(success) }()

		session, err := loginSessions.New(time.Now())
		if err != nil {
			return publicLoginJSON(http.StatusServiceUnavailable, map[string]any{"error": "login failed"})
		}
		authData, err := completeLoginExchange(hostHTTPDoer{rpc.HostCallbackID}, session, parsed)
		if err != nil {
			return publicLoginJSON(http.StatusBadGateway, map[string]any{"error": "login failed"})
		}
		authData.StorageJSON = refreshTraeModelsInStorage(rpc.HostCallbackID, authData.StorageJSON)
		document, err := json.Marshal(map[string]any{"type": ProviderTraeWork, "provider": ProviderTraeWork, "label": authData.Label, "storage": json.RawMessage(authData.StorageJSON)})
		if err != nil {
			return publicLoginJSON(http.StatusBadGateway, map[string]any{"error": "login failed"})
		}
		if err = (HostAuth{Call: callHostRPC}).Save(context.Background(), authData.FileName, document); err != nil {
			return publicLoginJSON(http.StatusServiceUnavailable, map[string]any{"error": "host auth save failed"})
		}
		success = true
		return publicLoginJSON(http.StatusOK, map[string]any{"status": "success", "message": "登录成功"})
	case "accounts":
		auth := HostAuth{Call: callHostRPC}
		m := NewManagement(auth, nil)
		return jsonManagement(200, m.Accounts(context.Background()))
	case "enabled":
		auth := HostAuth{Call: callHostRPC}
		m := NewManagement(auth, nil)
		return jsonManagement(200, m.SetEnabled(context.Background(), values.Get("auth_index"), values.Get("name"), values.Get("enabled") != "false"))
	case "credits":
		auth := HostAuth{Call: callHostRPC}
		client := traeAccountClient{auth: auth, callback: rpc.HostCallbackID}
		m := NewManagement(auth, client)
		account := ScheduledAccount{AuthIndex: values.Get("auth_index"), Name: values.Get("name"), UID: values.Get("uid")}
		return jsonManagement(200, m.Credits(context.Background(), account))
	case "checkin":
		auth := HostAuth{Call: callHostRPC}
		client := traeAccountClient{auth: auth, callback: rpc.HostCallbackID}
		m := NewManagement(auth, client)
		account := ScheduledAccount{AuthIndex: values.Get("auth_index"), Name: values.Get("name"), UID: values.Get("uid")}
		return jsonManagement(200, m.Checkin(context.Background(), account))
	case "models":
		auth := HostAuth{Call: callHostRPC}
		client := traeAccountClient{auth: auth, callback: rpc.HostCallbackID}
		account := ScheduledAccount{AuthIndex: values.Get("auth_index"), Name: values.Get("name"), UID: values.Get("uid")}
		s, e := client.storage(context.Background(), account)
		if e != nil {
			return jsonManagement(400, map[string]any{"error": "account unavailable"})
		}
		models, e := refreshTraeModels(rpc.HostCallbackID, s, account.Name)
		if e != nil {
			return jsonManagement(502, map[string]any{"error": "model refresh failed"})
		}
		return jsonManagement(200, map[string]any{"models": models, "cached_at": time.Now().UTC()})
	case "delete":
		auth := HostAuth{Call: callHostRPC}
		m := NewManagement(auth, nil)
		return jsonManagement(501, m.Delete(context.Background(), values.Get("auth_index"), values.Get("confirm")))
	default:
		return publicLoginJSON(http.StatusBadRequest, map[string]any{"error": "invalid action"})
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var rpc rpcManagementRequest
	if e := json.Unmarshal(raw, &rpc); e != nil {
		return nil, e
	}
	req := rpc.ManagementRequest
	path := strings.TrimRight(req.Path, "/")
	resourceBase := "/v0/resource/plugins/" + ProviderTraeWork
	resource := resourceBase + "/panel"
	base := "/v0/management/plugins/" + ProviderTraeWork
	if req.Method == http.MethodGet && path == resourceBase+"/status" {
		return jsonManagement(http.StatusOK, map[string]any{"provider": ProviderTraeWork, "status": "ok"})
	}
	if req.Method == http.MethodGet && path == resource {
		return renderPanel(rpc)
	}
	if req.Method == http.MethodPost && path == resource {
		return handlePublicLogin(rpc)
	}
	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		return managementResponse(http.StatusMethodNotAllowed, "text/plain; charset=utf-8", []byte("method not allowed"))
	}
	values := req.Query
	if req.Method == http.MethodPost {
		var e error
		values, e = formValues(req)
		if e != nil {
			return managementResponse(http.StatusBadRequest, "text/plain; charset=utf-8", []byte("invalid request"))
		}
		if !validCSRF(req, values) {
			return managementResponse(http.StatusForbidden, "text/plain; charset=utf-8", []byte("CSRF rejected"))
		}
	}
	auth := HostAuth{Call: callHostRPC}
	client := traeAccountClient{auth: auth, callback: rpc.HostCallbackID}
	m := NewManagement(auth, client)
	account := ScheduledAccount{AuthIndex: values.Get("auth_index"), Name: values.Get("name"), UID: values.Get("uid")}
	switch {
	case req.Method == http.MethodPost && path == base+"/login/start":
		out, e := handleAuthLoginStart(nil)
		if e != nil {
			return nil, e
		}
		var x struct {
			Result pluginapi.AuthLoginStartResponse `json:"result"`
		}
		_ = json.Unmarshal(out, &x)
		return jsonManagement(200, x.Result)
	case req.Method == http.MethodPost && path == base+"/login/poll":
		poll, _ := json.Marshal(rpcAuthLoginPollRequest{AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{Provider: ProviderTraeWork, State: values.Get("state"), Metadata: map[string]any{"callback_url": values.Get("callback_url")}}, HostCallbackID: rpc.HostCallbackID})
		out, e := handleAuthLoginPoll(poll)
		if e != nil {
			return jsonManagement(502, map[string]any{"error": "login failed"})
		}
		var x struct {
			Result pluginapi.AuthLoginPollResponse `json:"result"`
		}
		_ = json.Unmarshal(out, &x)
		if x.Result.Status != pluginapi.AuthLoginStatusSuccess {
			return jsonManagement(400, map[string]any{"error": x.Result.Message})
		}
		doc, _ := json.Marshal(map[string]any{"type": ProviderTraeWork, "provider": ProviderTraeWork, "label": x.Result.Auth.Label, "storage": json.RawMessage(x.Result.Auth.StorageJSON)})
		if e = auth.Save(context.Background(), x.Result.Auth.FileName, doc); e != nil {
			return jsonManagement(503, map[string]any{"error": "host auth save failed"})
		}
		return jsonManagement(200, map[string]any{"status": x.Result.Status, "message": x.Result.Message})
	case req.Method == http.MethodGet && path == base+"/accounts":
		return jsonManagement(200, m.Accounts(context.Background()))
	case req.Method == http.MethodPost && path == base+"/enabled":
		return jsonManagement(200, m.SetEnabled(context.Background(), account.AuthIndex, account.Name, values.Get("enabled") != "false"))
	case req.Method == http.MethodPost && path == base+"/delete":
		return jsonManagement(501, m.Delete(context.Background(), account.AuthIndex, values.Get("confirm")))
	case req.Method == http.MethodPost && path == base+"/credits":
		return jsonManagement(200, m.Credits(context.Background(), account))
	case req.Method == http.MethodPost && path == base+"/checkin":
		return jsonManagement(200, m.Checkin(context.Background(), account))
	case req.Method == http.MethodPost && path == base+"/models/refresh":
		s, e := client.storage(context.Background(), account)
		if e != nil {
			return jsonManagement(400, map[string]any{"error": "account unavailable"})
		}
		models, e := refreshTraeModels(rpc.HostCallbackID, s, account.Name)
		if e != nil {
			return jsonManagement(502, map[string]any{"error": "model refresh failed"})
		}
		return jsonManagement(200, map[string]any{"models": models, "cached_at": time.Now().UTC()})
	default:
		return managementResponse(404, "text/plain; charset=utf-8", []byte("not found"))
	}
}

func renderPanel(rpc rpcManagementRequest) ([]byte, error) {
	auth := HostAuth{Call: callHostRPC}
	files, _ := auth.List(context.Background())
	var rows strings.Builder
	for _, f := range files {
		if !validTraeWorkFile(f) {
			continue
		}
		state := "启用"
		button := "停用"
		enabled := "false"
		if f.Disabled {
			state = "停用"
			button = "启用"
			enabled = "true"
		}
		rows.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%s</td><td>%s</td><td><button data-action="enabled" data-index="%s" data-name="%s" data-enabled="%s">%s</button> <button data-action="credits" data-index="%s">积分</button> <button data-action="checkin" data-index="%s">签到</button> <button data-action="models" data-index="%s">刷新模型</button> <button data-action="delete" data-index="%s">删除（宿主不支持）</button></td></tr>`, html.EscapeString(f.Label), html.EscapeString(f.AuthIndex), state, html.EscapeString(f.AuthIndex), html.EscapeString(f.Name), enabled, button, html.EscapeString(f.AuthIndex), html.EscapeString(f.AuthIndex), html.EscapeString(f.AuthIndex), html.EscapeString(f.AuthIndex)))
	}
	if rows.Len() == 0 {
		rows.WriteString(`<tr><td colspan="4">暂无 TRAE Work 账号</td></tr>`)
	}
	models := traeModels()
	runtime := runtimeSnapshot()
	body := fmt.Sprintf(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>TRAE Work</title><style>body{font:14px system-ui;margin:0;background:#f5f7fa;color:#17202a}main{max-width:1100px;margin:auto;padding:24px}.card{background:white;border:1px solid #dfe5eb;border-radius:12px;padding:18px;margin:16px 0}table{width:100%%;border-collapse:collapse}td,th{padding:10px;border-bottom:1px solid #e6ebef;text-align:left}button,input{padding:8px;margin:3px}code{word-break:break-all}#result{white-space:pre-wrap;word-break:break-all;max-height:14em;overflow:auto}.muted{color:#667}.ok{color:#087f5b}.err{color:#c92a2a}@media(prefers-color-scheme:dark){body{background:#111;color:#eee}.card{background:#1b1b1b;border-color:#333}td,th{border-color:#333}input,button{background:#292929;color:#eee;border:1px solid #555}}</style></head><body><main><h1>TRAE Work 管理</h1><p class="muted">页面不显示 Access Token、Refresh Token 或完整回调 URL。</p><section class="card"><h2>添加账号</h2><button id="start">1. 打开 TRAE 登录页</button><p id="login"></p><p class="muted">2. 在打开的页面完成授权；浏览器会跳到一个打不开的 <code>http://127.0.0.1:18080/authorize?...</code> 地址——这是 TRAE 固定的本机回调，官方不允许公网地址，所以无法自动送达 CPA。</p><p class="muted">3. 复制浏览器地址栏里的完整 URL，粘贴到这里（10 分钟内有效；无需先点第 1 步，粘贴直接完成）：</p><input id="callback" size="70" placeholder="粘贴回调 URL"><button id="finish">完成登录</button></section><section class="card"><h2>账号</h2><table><thead><tr><th>标签</th><th>Auth Index</th><th>状态</th><th>操作</th></tr></thead><tbody>%s</tbody></table><pre id="result" aria-live="polite"></pre></section><section class="card"><h2>自动签到</h2><p>默认开启 · Asia/Hong_Kong · 并发 2 · 稳定账号抖动。运行中：%t；最近运行：%s；下次运行：%s；最近结果：%s</p></section><section class="card"><h2>模型缓存</h2><p>%d 个模型；缓存时间：%s</p></section></main><script>const csrf=%q;async function decode(r){const t=await r.text();let b=null;try{b=t?JSON.parse(t):null}catch(e){}if(!r.ok)throw new Error((b&&(b.error||b.message))||('HTTP '+r.status));return b||{}}async function publicPost(data){data.csrf=csrf;return decode(await fetch(location.pathname,{method:'POST',credentials:'same-origin',cache:'no-store',headers:{'Content-Type':'application/x-www-form-urlencoded','X-Requested-With':'traework-panel'},body:new URLSearchParams(data)}))}document.getElementById('start').onclick=async()=>{try{const b=await publicPost({action:'start'});window.open(b.url,'_blank','noopener');document.getElementById('login').innerHTML='<span class="muted">已打开 TRAE 授权页。完成授权后，把地址栏里的 127.0.0.1 URL 粘贴到下方并点「完成登录」。</span>'}catch(e){show(e)}};document.getElementById('finish').onclick=async()=>{try{let u=document.getElementById('callback').value.trim();if(!u)throw new Error('请先粘贴回调 URL');if(!/^https?:\/\//i.test(u))u='http://'+u;await publicPost({action:'finish',callback_url:u});show('登录成功');location.reload()}catch(e){show(e)}};
document.getElementById('callback').addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();document.getElementById('finish').click()}});function show(v){document.getElementById('result').textContent=typeof v==='string'?v:(v.message||v.error||JSON.stringify(v))}document.querySelectorAll('button[data-action]').forEach(b=>b.onclick=async()=>{try{let d={action:b.dataset.action,auth_index:b.dataset.index,name:b.dataset.name||''};if(b.dataset.action==='enabled')d.enabled=b.dataset.enabled;if(b.dataset.action==='delete')d.confirm='DELETE';const r=await publicPost(d);if(b.dataset.action==='models'){const n=Array.isArray(r&&r.models)?r.models.length:null;show(n==null?(r.message||r.error||'已刷新'):('已刷新 '+n+' 个模型，列表已更新缓存'))}else{show(r)}if(b.dataset.action==='enabled')location.reload()}catch(e){show(e)}})</script></body></html>`, rows.String(), runtime.Running, html.EscapeString(runtime.LastRun), html.EscapeString(runtime.NextRun), html.EscapeString(runtime.LastError), len(models), html.EscapeString(modelCacheTime()), initCSRF())
	return managementResponse(200, "text/html; charset=utf-8", []byte(body))
}
