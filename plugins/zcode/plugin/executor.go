package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Z.AI Anthropic endpoints. Coding Plan JWTs and API keys are different
// credentials and must not be sent to the same endpoint/header pair.
const (
	codingPlanUpstreamURL = "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"
	zcodeClientVersion    = "3.10.0"
	executorTimeout       = 120 * time.Second
	streamFrameMax        = 1 << 20  // 1 MiB per frame
	streamTotalMax        = 16 << 20 // 16 MiB total stream
	syncBodyMax           = 16 << 20 // 16 MiB
	requestBodyMax        = 64 << 20 // 64 MiB
)

var apiKeyUpstreamURL = "https://api.z.ai/api/anthropic/v1/messages"

const bigModelCodingUpstreamURL = "https://open.bigmodel.cn/api/anthropic/v1/messages"

func zcodeHeaders(jwt, captchaParam, captchaRegion string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+jwt)
	h.Set("anthropic-version", "2023-06-01")
	h.Set("User-Agent", "ZCode/"+zcodeClientVersion)
	h.Set("X-ZCode-App-Version", zcodeClientVersion)
	h.Set("X-ZCode-Agent", "glm")
	h.Set("X-Title", "Z Code@electron")
	h.Set("HTTP-Referer", "https://zcode.z.ai/")
	if captchaParam != "" {
		h.Set("X-Aliyun-Captcha-Verify-Param", captchaParam)
		if captchaRegion != "" {
			h.Set("X-Aliyun-Captcha-Verify-Region", captchaRegion)
		}
	}
	return h
}

func apiKeyHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("x-api-key", apiKey)
	h.Set("anthropic-version", "2023-06-01")
	h.Set("User-Agent", "ZCode/3.0.1")
	h.Set("HTTP-Referer", "https://zcode.z.ai/")
	return h
}

type upstreamCredential struct {
	URL               string
	Headers           http.Header
	CodingPlan        bool
	CaptchaUsed       bool
	SigningCredential string
}

func resolveCredential(storage []byte) (upstreamCredential, error) {
	candidates, err := resolveCredentialCandidates(storage, "")
	if err != nil {
		return upstreamCredential{}, err
	}
	return candidates[0], nil
}

func isCaptchaRejection(status int, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "captcha") || strings.Contains(text, "aliyun") || strings.Contains(text, "verify")
}

// classifyUpstreamStatus returns a stable, credential-safe category for UI and logs.
func classifyUpstreamStatus(status int, body []byte) string {
	text := strings.ToLower(string(body))
	if status == http.StatusTooManyRequests && (strings.Contains(text, "1113") || strings.Contains(text, "insufficient balance") || strings.Contains(text, "resource package")) {
		return "insufficient_balance"
	}
	if isCaptchaRejection(status, body) {
		return "captcha_required"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "credential_invalid"
	}
	if status >= http.StatusInternalServerError {
		return "upstream_unavailable"
	}
	return "request_failed"
}

func upstreamError(resp pluginapi.HTTPResponse, credential upstreamCredential) error {
	if credential.CodingPlan && !credential.CaptchaUsed && isCaptchaRejection(resp.StatusCode, resp.Body) {
		return fmt.Errorf("Coding Plan captcha verification is required: provide captcha_verify_param from an official ZCode verification session")
	}
	if credential.CodingPlan && credential.CaptchaUsed && isCaptchaRejection(resp.StatusCode, resp.Body) {
		return fmt.Errorf("Coding Plan captcha verification expired or was rejected: refresh captcha_verify_param through the official ZCode client")
	}
	switch classifyUpstreamStatus(resp.StatusCode, resp.Body) {
	case "insufficient_balance":
		return fmt.Errorf("Z.AI API 余额或资源包不足 (429/1113): 请在官方账户补充资源")
	case "credential_invalid":
		return fmt.Errorf("ZCode 凭证无效、过期或权限不足: 请重新授权或更新凭证")
	case "captcha_required":
		return fmt.Errorf("Coding Plan 需要滑块验证: 请通过官方 ZCode 客户端刷新验证信息后重试")
	case "upstream_unavailable":
		return fmt.Errorf("ZCode 上游服务暂不可用: 请稍后重试")
	}
	return fmt.Errorf("zcode upstream request failed with HTTP %d", resp.StatusCode)
}

// isFreeModel reports whether the plugin-native model id maps to a
// provider model that BigModel serves for free (no balance consumption).
func isFreeModel(name string) bool {
	switch name {
	case "glm-4.5-flash":
		return true
	}
	return false
}

// forceStreamField sets the "stream" field of the JSON payload to the given
// value so the payload always matches the ABI method being called (sync vs
// stream). Malformed payloads are passed through unchanged.
func forceStreamField(payload []byte, want bool) []byte {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	body["stream"] = want
	if out, err := json.Marshal(body); err == nil {
		return out
	}
	return payload
}

// rewritePayloadModel strips the zcode- prefix from the model field so the
// upstream Z.AI endpoint receives the provider-native model name (e.g.
// zcode-glm-5.1 -> glm-5.1).
func rewritePayloadModel(payload []byte) []byte {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	if m, ok := body["model"].(string); ok {
		if len(m) > 6 && m[:6] == "zcode-" {
			m = m[6:]
		}
		nativeNames := map[string]string{
			"glm-5.2":     "GLM-5.2",
			"glm-5.1":     "GLM-5.1",
			"glm-5-turbo": "GLM-5-Turbo",
			"glm-turbo":   "GLM-5-Turbo",
			"glm-4.7":     "GLM-4.7",
		}
		if native := nativeNames[m]; native != "" {
			m = native
		}
		body["model"] = m
		if out, err := json.Marshal(body); err == nil {
			return out
		}
	}
	return payload
}

func normalizeAnthropicNonStreamResponse(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("data:")) {
		return raw, nil
	}
	var message struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, err
	}
	if message.Type != "message" {
		return raw, nil
	}
	marshalFrame := func(value any) ([]byte, error) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return append(append([]byte("data: "), encoded...), []byte("\n\n")...), nil
	}
	var out []byte
	start, err := marshalFrame(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": message.ID, "type": "message", "role": message.Role,
			"model": message.Model, "content": []any{}, "usage": json.RawMessage(message.Usage),
		},
	})
	if err != nil {
		return nil, err
	}
	out = append(out, start...)
	for index, block := range message.Content {
		contentBlock := map[string]any{"type": block.Type}
		delta := map[string]any{}
		switch block.Type {
		case "thinking":
			contentBlock["thinking"] = ""
			delta["type"] = "thinking_delta"
			delta["thinking"] = block.Thinking
		case "text":
			contentBlock["text"] = ""
			delta["type"] = "text_delta"
			delta["text"] = block.Text
		case "tool_use":
			contentBlock["id"] = block.ID
			contentBlock["name"] = block.Name
			contentBlock["input"] = map[string]any{}
			delta["type"] = "input_json_delta"
			if len(block.Input) == 0 {
				delta["partial_json"] = "{}"
			} else {
				delta["partial_json"] = string(block.Input)
			}
		default:
			continue
		}
		for _, event := range []map[string]any{
			{"type": "content_block_start", "index": index, "content_block": contentBlock},
			{"type": "content_block_delta", "index": index, "delta": delta},
			{"type": "content_block_stop", "index": index},
		} {
			frame, frameErr := marshalFrame(event)
			if frameErr != nil {
				return nil, frameErr
			}
			out = append(out, frame...)
		}
	}
	delta, err := marshalFrame(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": message.StopReason},
		"usage": json.RawMessage(message.Usage),
	})
	if err != nil {
		return nil, err
	}
	out = append(out, delta...)
	stop, err := marshalFrame(map[string]any{"type": "message_stop"})
	if err != nil {
		return nil, err
	}
	return append(out, stop...), nil
}

func executeHTTP(ctx context.Context, client pluginapi.HostHTTPClient, hostCallbackID string, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if client != nil {
		return client.Do(ctx, request)
	}
	if hostCallbackID != "" {
		raw, err := callHostRPC(pluginabi.MethodHostHTTPDo, rpcHostHTTPRequest{
			HostCallbackID: hostCallbackID,
			Request:        request,
		})
		if err != nil {
			return pluginapi.HTTPResponse{}, err
		}
		var resp pluginapi.HTTPResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return pluginapi.HTTPResponse{}, err
		}
		return resp, nil
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	req.Header = request.Headers.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, syncBodyMax))
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
}

func resolveCredentialURL(ctx context.Context, client pluginapi.HostHTTPClient, hostCallbackID string, credential upstreamCredential, rawURL string) (string, error) {
	routed := endpointRouter.resolve(ctx, client, hostCallbackID, rawURL)
	if !routed.Routed {
		return rawURL, nil
	}
	// Active rewrites cross a fresh trust boundary: enforce both the global
	// target allowlist and the credential's original platform/host binding.
	if err := validateZCodeURL(routed.URL); err != nil {
		return "", err
	}
	if err := validateCredentialTarget(routed.URL, credential); err != nil {
		return "", err
	}
	return routed.URL, nil
}

func executeSignedHTTP(ctx context.Context, manager *signingManager, credential upstreamCredential, request pluginapi.HTTPRequest, send func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)) (pluginapi.HTTPResponse, error) {
	if manager == nil || credential.SigningCredential == "" {
		return send(request)
	}
	baseHeaders := request.Headers.Clone()
	first, err := manager.sign(ctx, request.URL, baseHeaders, credential.SigningCredential, zcodeClientVersion)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	request.Headers = first.Header
	resp, err := send(request)
	if err != nil || !first.Signed || !isSigningVerifyFailure(resp.StatusCode, resp.Body) {
		return resp, err
	}

	// A VERIFY response proves that the upstream received and processed the
	// request. Invalidate the cached signing key, but never replay a generation
	// request whose body may already have triggered computation or billing.
	manager.invalidate(request.URL, credential.SigningCredential, false)
	if !isSafeUnsignedReplay(request.Method, request.URL) {
		return resp, nil
	}

	second, err := manager.sign(ctx, request.URL, baseHeaders, credential.SigningCredential, zcodeClientVersion)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	request.Headers = second.Header
	resp, err = send(request)
	if err != nil || !second.Signed || !isSigningVerifyFailure(resp.StatusCode, resp.Body) {
		return resp, err
	}

	// A second VERIFY failure invalidates only the cached key. Safe methods may
	// make one final unsigned attempt; generation POST requests never reach here.
	manager.invalidate(request.URL, credential.SigningCredential, false)
	request.Headers = baseHeaders
	return send(request)
}

func handleExecutorExecute(request []byte) ([]byte, error) {
	var rpcReq rpcExecutorRequest
	if err := json.Unmarshal(request, &rpcReq); err != nil {
		return nil, err
	}
	req := rpcReq.ExecutorRequest
	if len(req.Payload) > requestBodyMax {
		return nil, fmt.Errorf("request payload exceeds %d bytes", requestBodyMax)
	}
	if offPeak, selected, offPeakErr := configuredOffPeakExecution(req.StorageJSON, req.Payload, req.AuthID); selected {
		if offPeakErr != nil {
			return nil, offPeakErr
		}
		execCtx, cancel := context.WithTimeout(context.Background(), offPeak.Config.Timeout)
		defer cancel()
		body, runErr := executeConfiguredOffPeak(execCtx, offPeak, req.HTTPClient, rpcReq.HostCallbackID, func(delay time.Duration) error {
			select {
			case <-time.After(delay):
				return nil
			case <-execCtx.Done():
				return errors.New("off-peak wait interrupted")
			}
		})
		if runErr != nil {
			return nil, errors.New(sanitizeOffPeakError(runErr))
		}
		return okEnvelope(pluginapi.ExecutorResponse{Payload: body, Headers: http.Header{"Content-Type": []string{"application/json"}}, Metadata: map[string]any{"status_code": http.StatusOK, "route": "off-peak"}})
	}
	candidates, err := resolveCredentialCandidates(req.StorageJSON, req.AuthID)
	if err != nil {
		return nil, err
	}
	execCtx, cancel := context.WithTimeout(context.Background(), executorTimeout)
	defer cancel()
	var resp pluginapi.HTTPResponse
	for index, credential := range candidates {
		targetURL, routeErr := resolveCredentialURL(execCtx, req.HTTPClient, rpcReq.HostCallbackID, credential, credential.URL)
		if routeErr != nil {
			return nil, routeErr
		}
		httpReq := pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     targetURL,
			Headers: credential.Headers,
			Body:    forceStreamField(rewritePayloadModel(req.Payload), false),
		}
		resp, err = executeSignedHTTP(execCtx, defaultSigningManager, credential, httpReq, func(out pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return executeHTTP(execCtx, req.HTTPClient, rpcReq.HostCallbackID, out)
		})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < http.StatusBadRequest {
			payload, normalizeErr := normalizeAnthropicNonStreamResponse(resp.Body)
			if normalizeErr != nil {
				return nil, fmt.Errorf("invalid Anthropic non-stream response: %w", normalizeErr)
			}
			return okEnvelope(pluginapi.ExecutorResponse{
				Payload:  payload,
				Headers:  resp.Headers,
				Metadata: map[string]any{"status_code": resp.StatusCode, "route": map[bool]string{true: "coding-plan", false: "api-key"}[credential.CodingPlan]},
			})
		}
		if index+1 >= len(candidates) || !mayFallback(resp.StatusCode, resp.Body) {
			return nil, upstreamError(resp, credential)
		}
	}
	return nil, upstreamError(resp, candidates[len(candidates)-1])
}

func executeHTTPStream(ctx context.Context, client pluginapi.HostHTTPClient, request pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	if client != nil {
		return client.DoStream(ctx, request)
	}
	resp, err := executeHTTP(ctx, nil, "", request)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, 1)
	chunks <- pluginapi.HTTPStreamChunk{Payload: resp.Body}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Chunks: chunks}, nil
}

func handleExecutorExecuteStream(request []byte) ([]byte, error) {
	var rpcReq rpcExecutorRequest
	if err := json.Unmarshal(request, &rpcReq); err != nil {
		return nil, err
	}
	req := rpcReq.ExecutorRequest
	if len(req.Payload) > requestBodyMax {
		return nil, fmt.Errorf("request payload exceeds %d bytes", requestBodyMax)
	}
	if offPeak, selected, offPeakErr := configuredOffPeakExecution(req.StorageJSON, req.Payload, req.AuthID); selected {
		if offPeakErr != nil {
			return nil, offPeakErr
		}
		if rpcReq.HostCallbackID != "" && rpcReq.StreamID != "" {
			go forwardOffPeakStream(rpcReq, offPeak)
			return okEnvelope(streamResponse{Headers: http.Header{"Content-Type": []string{"text/event-stream"}}})
		}
		streamCtx, cancel := context.WithTimeout(context.Background(), offPeak.Config.Timeout)
		defer cancel()
		var keepalives [][]byte
		body, runErr := executeConfiguredOffPeak(streamCtx, offPeak, req.HTTPClient, rpcReq.HostCallbackID, func(delay time.Duration) error {
			keepalives = append(keepalives, offPeakKeepaliveFrame())
			select {
			case <-time.After(delay):
				return nil
			case <-streamCtx.Done():
				return errors.New("off-peak wait interrupted")
			}
		})
		chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(keepalives)+1)
		for _, frame := range keepalives {
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frame})
		}
		for _, frame := range frameSSEPayload(body) {
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frame})
		}
		if runErr != nil {
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Err: errors.New(sanitizeOffPeakError(runErr))})
		}
		return okEnvelope(streamResponse{Headers: http.Header{"Content-Type": []string{"text/event-stream"}}, Chunks: chunks})
	}
	credential, err := resolveCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithTimeout(context.Background(), executorTimeout)
	defer cancel()
	targetURL, err := resolveCredentialURL(streamCtx, req.HTTPClient, rpcReq.HostCallbackID, credential, credential.URL)
	if err != nil {
		return nil, err
	}
	httpReq := pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     targetURL,
		Headers: credential.Headers,
		Body:    forceStreamField(rewritePayloadModel(req.Payload), true),
	}
	// Streaming requests are signed before any bytes are sent. We deliberately
	// do not perform VERIFY replay here: once a stream starts, automatic replay
	// can duplicate computation or charges.
	if credential.SigningCredential != "" {
		signed, signErr := defaultSigningManager.sign(streamCtx, httpReq.URL, httpReq.Headers.Clone(), credential.SigningCredential, zcodeClientVersion)
		if signErr != nil {
			return nil, signErr
		}
		httpReq.Headers = signed.Header
	}
	if rpcReq.HostCallbackID != "" && rpcReq.StreamID != "" {
		return executeStreamViaHostCallback(rpcReq, httpReq)
	}
	streamResp, err := executeHTTPStream(streamCtx, req.HTTPClient, httpReq)
	if err != nil {
		return nil, err
	}
	if streamResp.StatusCode >= http.StatusBadRequest {
		var body []byte
		for c := range streamResp.Chunks {
			if c.Err != nil {
				return nil, c.Err
			}
			body = append(body, c.Payload...)
			if len(body) >= 500 {
				break
			}
		}
		return nil, upstreamError(pluginapi.HTTPResponse{StatusCode: streamResp.StatusCode, Headers: streamResp.Headers, Body: body}, credential)
	}

	// Drain the host stream into a bounded slice for the ABI envelope.
	// The host turns these into an internal stream afterwards.
	var rawStream []byte
	var streamErr error
	for c := range streamResp.Chunks {
		if c.Err != nil {
			streamErr = c.Err
			continue
		}
		if len(rawStream)+len(c.Payload) > streamTotalMax {
			streamErr = fmt.Errorf("stream response exceeded %d bytes", streamTotalMax)
			break
		}
		rawStream = append(rawStream, c.Payload...)
	}
	frames := frameSSEPayload(rawStream)
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(frames)+1)
	for _, frame := range frames {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frame})
	}
	if streamErr != nil {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Err: streamErr})
	}
	return okEnvelope(streamResponse{
		Headers: streamResp.Headers,
		Chunks:  chunks,
	})
}

func forwardOffPeakStream(rpcReq rpcExecutorRequest, execution *offPeakExecution) {
	ctx, cancel := context.WithTimeout(context.Background(), execution.Config.Timeout)
	defer cancel()
	emit := func(payload []byte) error {
		_, err := callHostRPC(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: rpcReq.StreamID, Payload: payload})
		return err
	}
	body, err := executeConfiguredOffPeak(ctx, execution, rpcReq.ExecutorRequest.HTTPClient, rpcReq.HostCallbackID, func(delay time.Duration) error {
		if err := emit(offPeakKeepaliveFrame()); err != nil {
			return errors.New("off-peak stream closed")
		}
		select {
		case <-time.After(delay):
			return nil
		case <-ctx.Done():
			return errors.New("off-peak wait interrupted")
		}
	})
	if err == nil {
		for _, frame := range frameSSEPayload(body) {
			if emit(frame) != nil {
				err = errors.New("off-peak stream closed")
				break
			}
		}
	}
	if err != nil {
		closePluginStream(rpcReq.StreamID, sanitizeOffPeakError(err))
		return
	}
	closePluginStream(rpcReq.StreamID, "")
}

func executeStreamViaHostCallback(rpcReq rpcExecutorRequest, request pluginapi.HTTPRequest) ([]byte, error) {
	raw, err := callHostRPC(pluginabi.MethodHostHTTPDoStream, rpcHostHTTPRequest{
		HostCallbackID: rpcReq.HostCallbackID,
		Request:        request,
	})
	if err != nil {
		return nil, err
	}
	var resp rpcHostHTTPStreamResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		// Read a bounded error body for classification, then return a
		// sanitized error without upstream raw text.
		errBody, _ := readHostHTTPError(resp.StreamID, 500)
		return nil, upstreamError(pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Body: errBody}, upstreamCredential{})
	}
	if resp.StreamID == "" {
		return nil, fmt.Errorf("zcode upstream stream id is empty")
	}

	go forwardHostHTTPStream(resp.StreamID, rpcReq.StreamID)
	return okEnvelope(streamResponse{Headers: resp.Headers})
}

func readHostHTTPError(streamID string, limit int) ([]byte, error) {
	defer func() {
		_, _ = callHostRPC(pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: streamID})
	}()
	var body []byte
	for len(body) < limit {
		raw, err := callHostRPC(pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: streamID})
		if err != nil {
			return body, err
		}
		var chunk rpcHostHTTPStreamReadResponse
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return body, err
		}
		if chunk.Error != "" {
			return body, fmt.Errorf("%s", chunk.Error)
		}
		body = append(body, chunk.Payload...)
		if chunk.Done {
			break
		}
	}
	return body, nil
}

func forwardHostHTTPStream(httpStreamID, pluginStreamID string) {
	defer func() {
		_, _ = callHostRPC(pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: httpStreamID})
	}()
	var buffer []byte
	for {
		raw, err := callHostRPC(pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: httpStreamID})
		if err != nil {
			closePluginStream(pluginStreamID, err.Error())
			return
		}
		var chunk rpcHostHTTPStreamReadResponse
		if err := json.Unmarshal(raw, &chunk); err != nil {
			closePluginStream(pluginStreamID, err.Error())
			return
		}
		if chunk.Error != "" {
			closePluginStream(pluginStreamID, "zcode upstream stream error")
			return
		}
		if len(buffer)+len(chunk.Payload) > streamTotalMax {
			closePluginStream(pluginStreamID, "zcode upstream stream exceeded size limit")
			return
		}
		buffer = append(buffer, chunk.Payload...)
		frames, rest := takeSSEFrames(buffer, chunk.Done)
		buffer = rest
		for _, payload := range frames {
			if _, err := callHostRPC(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: pluginStreamID, Payload: payload}); err != nil {
				closePluginStream(pluginStreamID, err.Error())
				return
			}
		}
		if chunk.Done {
			closePluginStream(pluginStreamID, "")
			return
		}
	}
}

func frameSSEPayload(payload []byte) [][]byte {
	frames, _ := takeSSEFrames(payload, true)
	return frames
}

func takeSSEFrames(payload []byte, flush bool) ([][]byte, []byte) {
	normalized := bytes.ReplaceAll(payload, []byte("\r\n"), []byte("\n"))
	parts := bytes.Split(normalized, []byte("\n\n"))
	limit := len(parts) - 1
	if flush {
		limit = len(parts)
	}
	frames := make([][]byte, 0, limit)
	for _, block := range parts[:limit] {
		var dataLines [][]byte
		for _, line := range bytes.Split(block, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				dataLines = append(dataLines, bytes.TrimSpace(line[5:]))
			}
		}
		if len(dataLines) == 0 {
			continue
		}
		data := bytes.Join(dataLines, []byte("\n"))
		frame := append([]byte("data: "), data...)
		frame = append(frame, '\n', '\n')
		frames = append(frames, frame)
	}
	if flush || len(parts) == 0 {
		return frames, nil
	}
	return frames, append([]byte(nil), parts[len(parts)-1]...)
}

func closePluginStream(streamID, errMsg string) {
	_, _ = callHostRPC(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{StreamID: streamID, Error: errMsg})
}

func handleExecutorCountTokens(_ []byte) ([]byte, error) {
	return errorEnvelope("count_tokens_unsupported", "zcode executor does not provide a tokenizer; token counting is unsupported"), nil
}

func truncateBody(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}

func effectiveHTTPSPort(u *url.URL) string {
	if u.Port() == "" {
		return "443"
	}
	return u.Port()
}

func validateZCodeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.User != nil || effectiveHTTPSPort(u) != "443" || (u.Hostname() != "api.z.ai" && u.Hostname() != "zcode.z.ai" && u.Hostname() != "open.bigmodel.cn") {
		return fmt.Errorf("refusing credentialed request to untrusted URL")
	}
	return nil
}

func validateCredentialTarget(raw string, credential upstreamCredential) error {
	target, err := url.Parse(raw)
	if err != nil {
		return err
	}
	bound, err := url.Parse(credential.URL)
	if err != nil || bound.Hostname() == "" {
		return fmt.Errorf("credential has no trusted upstream binding")
	}
	// Credentials are host-bound: a Z.AI Coding Plan JWT may only reach its
	// zcode.z.ai host, a Z.AI API key only api.z.ai, and a BigModel key only
	// open.bigmodel.cn. This is intentionally stricter than a shared allowlist.
	if target.Scheme != "https" || !strings.EqualFold(target.Hostname(), bound.Hostname()) || effectiveHTTPSPort(target) != effectiveHTTPSPort(bound) || effectiveHTTPSPort(target) != "443" || target.User != nil {
		return fmt.Errorf("refusing credentialed request outside its bound upstream host")
	}
	return nil
}

func handleExecutorHTTPRequest(request []byte) ([]byte, error) {
	var rpcReq struct {
		pluginapi.ExecutorHTTPRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &rpcReq); err != nil {
		return nil, err
	}
	req := rpcReq.ExecutorHTTPRequest
	credential, err := resolveCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	if err := validateCredentialTarget(req.URL, credential); err != nil {
		return nil, err
	}
	if len(req.Body) > requestBodyMax {
		return nil, fmt.Errorf("request body exceeds %d bytes", requestBodyMax)
	}
	headers := req.Headers
	if headers == nil {
		headers = http.Header{}
	}
	// Unconditionally strip client-supplied auth headers so a client can never
	// inject its own credential and bypass routing/accounting, then overlay the
	// plugin-resolved credential headers.
	for _, k := range []string{"Authorization", "X-Api-Key", "Proxy-Authorization"} {
		headers.Del(k)
	}
	for k, values := range credential.Headers {
		for _, value := range values {
			headers.Add(k, value)
		}
	}
	httpCtx, cancel := context.WithTimeout(context.Background(), executorTimeout)
	defer cancel()
	targetURL, err := resolveCredentialURL(httpCtx, req.HTTPClient, rpcReq.HostCallbackID, credential, req.URL)
	if err != nil {
		return nil, err
	}
	httpReq := pluginapi.HTTPRequest{Method: req.Method, URL: targetURL, Headers: headers, Body: req.Body}
	resp, err := executeSignedHTTP(httpCtx, defaultSigningManager, credential, httpReq, func(out pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return executeHTTP(httpCtx, req.HTTPClient, rpcReq.HostCallbackID, out)
	})
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	})
}
