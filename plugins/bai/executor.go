package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// baiHeaders builds the upstream request headers for a B.AI API key.
func baiHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", "bai-cpa-plugin/0.1.0")
	return h
}

type upstreamCredential struct {
	URL     string
	Headers http.Header
}

// resolveCredential turns the persisted storage into a ready upstream call.
func resolveCredential(storage []byte) (upstreamCredential, error) {
	s, err := parseAuthStorage(storage)
	if err != nil {
		return upstreamCredential{}, err
	}
	return upstreamCredential{URL: baiUpstreamURL, Headers: baiHeaders(s.APIKey)}, nil
}

// rewritePayloadModel strips the bai- prefix from the model field and
// normalizes anything the host pipeline may have added so the upstream B.AI
// endpoint receives a clean OpenAI chat payload:
//   - content blocks [{"type":"text","text":...}] collapse to plain strings
//   - the host-injected metadata object is dropped
//   - system prompts sent as a top-level "system" array flatten to a message
func rewritePayloadModel(payload []byte) []byte {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	changed := false
	if m, ok := body["model"].(string); ok {
		if native, found := strings.CutPrefix(m, "bai-"); found {
			body["model"] = strings.TrimSuffix(native, " (free)")
			changed = true
		}
	}
	if _, ok := body["metadata"]; ok {
		delete(body, "metadata")
		changed = true
	}
	if msgs, ok := body["messages"].([]any); ok {
		fixed := normalizeMessages(msgs)
		if fixed != nil {
			body["messages"] = fixed
			changed = true
		}
	}
	if !changed {
		return payload
	}
	if out, err := json.Marshal(body); err == nil {
		return out
	}
	return payload
}

// normalizeMessages collapses Anthropic-style content blocks to plain strings.
// It returns nil when nothing needed changing.
func normalizeMessages(msgs []any) []any {
	changed := false
	for i, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		parts := make([]string, 0, len(blocks))
		allText := true
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok || block["type"] != "text" {
				allText = false
				break
			}
			text, _ := block["text"].(string)
			parts = append(parts, text)
		}
		if allText && len(parts) > 0 {
			msg["content"] = strings.Join(parts, "\n")
			msgs[i] = msg
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return msgs
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
}

// executorClientHook lets tests inject a fake host HTTP client; production
// always uses the client carried by the request (or the host callback path).
var executorClientHook func(req pluginapi.ExecutorRequest) pluginapi.HostHTTPClient

func clientFor(req pluginapi.ExecutorRequest) pluginapi.HostHTTPClient {
	if executorClientHook != nil {
		return executorClientHook(req)
	}
	return req.HTTPClient
}

func handleExecutorExecute(request []byte) ([]byte, error) {
	var rpcReq rpcExecutorRequest
	if err := json.Unmarshal(request, &rpcReq); err != nil {
		return nil, err
	}
	req := rpcReq.ExecutorRequest
	credential, err := resolveCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	var lastErr error
	payloads := attemptPayloads(rewritePayloadModel(req.Payload))
	for i, payload := range payloads {
		resp, err := executeHTTP(context.Background(), clientFor(req), rpcReq.HostCallbackID, pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     credential.URL,
			Headers: credential.Headers,
			Body:    payload,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= http.StatusBadRequest {
			class := classifyUpstreamStatus(resp.StatusCode, resp.Body)
			lastErr = upstreamError(resp)
			if i == len(payloads)-1 || !fallbackEligible(class) {
				if class == "insufficient_balance" {
					return errorEnvelopeStatus("insufficient_balance", "B.AI 余额不足，付费模型当前不可用", http.StatusPaymentRequired), nil
				}
				return nil, lastErr
			}
			continue
		}
		return okEnvelope(pluginapi.ExecutorResponse{
			Payload:  resp.Body,
			Headers:  resp.Headers,
			Metadata: map[string]any{"status_code": resp.StatusCode, "route": "api-key"},
		})
	}
	return nil, lastErr
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
	credential, err := resolveCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	buildRequest := func(payload []byte) pluginapi.HTTPRequest {
		return pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     credential.URL,
			Headers: credential.Headers,
			Body:    payload,
		}
	}
	payloads := attemptPayloads(rewritePayloadModel(req.Payload))
	if rpcReq.HostCallbackID != "" && rpcReq.StreamID != "" {
		return executeStreamViaHostCallback(rpcReq, buildRequest, payloads)
	}
	var lastErr error
	for i, payload := range payloads {
		streamResp, err := executeHTTPStream(context.Background(), clientFor(req), buildRequest(payload))
		if err != nil {
			lastErr = err
			continue
		}
		if streamResp.StatusCode >= http.StatusBadRequest {
			var body []byte
			for c := range streamResp.Chunks {
				if c.Err != nil {
					lastErr = c.Err
					break
				}
				body = append(body, c.Payload...)
				if len(body) >= 500 {
					break
				}
			}
			if lastErr == nil {
				lastErr = upstreamError(pluginapi.HTTPResponse{StatusCode: streamResp.StatusCode, Body: body})
			}
			class := classifyUpstreamStatus(streamResp.StatusCode, body)
			if i == len(payloads)-1 || !fallbackEligible(class) {
				if class == "insufficient_balance" {
					return errorEnvelopeStatus("insufficient_balance", "B.AI 余额不足，付费模型当前不可用", http.StatusPaymentRequired), nil
				}
				return nil, lastErr
			}
			continue
		}
		var frames [][]byte
		var buffer []byte
		for chunk := range streamResp.Chunks {
			if chunk.Err != nil {
				return nil, chunk.Err
			}
			buffer = append(buffer, chunk.Payload...)
			var rest []byte
			frames, rest = takeSSEFrames(buffer, false)
			buffer = rest
		}
		if len(buffer) > 0 {
			tail, _ := takeSSEFrames(buffer, true)
			frames = append(frames, tail...)
		}
		return okEnvelope(streamResponse{Headers: streamResp.Headers, Chunks: toExecutorChunks(frames)})
	}
	return nil, lastErr
}

func toExecutorChunks(frames [][]byte) []pluginapi.ExecutorStreamChunk {
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(frames))
	for _, frame := range frames {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: frame})
	}
	return chunks
}

func executeStreamViaHostCallback(rpcReq rpcExecutorRequest, buildRequest func([]byte) pluginapi.HTTPRequest, payloads [][]byte) ([]byte, error) {
	var lastErr error
	for i, payload := range payloads {
		raw, err := callHostRPC(pluginabi.MethodHostHTTPDoStream, rpcHostHTTPRequest{
			HostCallbackID: rpcReq.HostCallbackID,
			Request:        buildRequest(payload),
		})
		if err != nil {
			lastErr = err
			continue
		}
		var resp rpcHostHTTPStreamResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}
		if resp.StatusCode >= http.StatusBadRequest {
			body, readErr := readHostHTTPError(resp.StreamID, 500)
			if readErr != nil {
				lastErr = fmt.Errorf("bai upstream %d: %v", resp.StatusCode, readErr)
			} else {
				lastErr = fmt.Errorf("bai upstream %d: %s", resp.StatusCode, truncateBody(body, 500))
			}
			class := classifyUpstreamStatus(resp.StatusCode, body)
			if i == len(payloads)-1 || !fallbackEligible(class) {
				if class == "insufficient_balance" {
					return errorEnvelopeStatus("insufficient_balance", "B.AI 余额不足，付费模型当前不可用", http.StatusPaymentRequired), nil
				}
				return nil, lastErr
			}
			continue
		}
		if resp.StreamID == "" {
			return nil, fmt.Errorf("bai upstream stream id is empty")
		}

		go forwardHostHTTPStream(resp.StreamID, rpcReq.StreamID)
		return okEnvelope(streamResponse{Headers: resp.Headers})
	}
	return nil, lastErr
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
			closePluginStream(pluginStreamID, chunk.Error)
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

// takeSSEFrames splits arbitrary network chunks into standalone `data:` SSE
// frames. The CPA response converter expects each chunk to be exactly one
// data frame; upstream chunks may start mid-event, contain several events or
// truncate one, so re-framing here is required (lesson from zcode v0.6.11).
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
				value := bytes.TrimSpace(line[5:])
				dataLines = append(dataLines, value)
			}
		}
		if len(dataLines) == 0 {
			continue
		}
		// Emit the raw data VALUE only. When the host relays chunks from the
		// non-callback path it wraps each payload in its own "data: " line, so
		// a full frame here would be double-prefixed downstream.
		data := bytes.Join(dataLines, []byte("\n"))
		frame := append([]byte(data), '\n', '\n')
		frames = append(frames, frame)
	}
	var rest []byte
	if !flush && limit < len(parts) {
		rest = parts[limit]
	}
	return frames, rest
}

func closePluginStream(streamID, errMsg string) {
	_, _ = callHostRPC(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{StreamID: streamID, Error: errMsg})
}

func truncateBody(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}

// validateBAIURL refuses credentialed requests to untrusted hosts.
func validateBAIURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.Hostname() != "api.b.ai" {
		return fmt.Errorf("refusing credentialed request to untrusted URL")
	}
	return nil
}

func upstreamError(resp pluginapi.HTTPResponse) error {
	switch classifyUpstreamStatus(resp.StatusCode, resp.Body) {
	case "credential_invalid":
		return fmt.Errorf("B.AI API Key 无效或已过期")
	case "insufficient_balance":
		return fmt.Errorf("B.AI 需要充值解锁该模型或积分不足: 请在平台上充值后重试")
	case "rate_limited":
		return fmt.Errorf("B.AI 上游限流: 请稍后重试")
	case "upstream_unavailable":
		return fmt.Errorf("B.AI 上游服务暂不可用: 请稍后重试")
	}
	return fmt.Errorf("bai upstream %d: %s", resp.StatusCode, truncateBody(resp.Body, 500))
}

// classifyUpstreamStatus returns a stable, credential-safe category for UI and logs.
func classifyUpstreamStatus(status int, body []byte) string {
	text := strings.ToLower(string(body))
	if status == http.StatusTooManyRequests {
		if strings.Contains(text, "insufficient") || strings.Contains(text, "balance") || strings.Contains(text, "credit") {
			return "insufficient_balance"
		}
		return "rate_limited"
	}
	if status == http.StatusPaymentRequired {
		return "insufficient_balance"
	}
	if status == http.StatusBadRequest && upstreamQuotaExceeded(text) {
		return "insufficient_balance"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "credential_invalid"
	}
	if status == http.StatusServiceUnavailable || status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
		return "upstream_unavailable"
	}
	if status >= http.StatusInternalServerError {
		return "upstream_unavailable"
	}
	return "bad_request"
}

// upstreamQuotaExceeded detects B.AI's credit-quota gate on non-standard
// statuses: HTTP 400 with code insufficient_user_quota / message "credit
// insufficient balance: balance=0 required=102 ..." (seen on glm-5.3-flash
// since 2026-09-14). text must already be lower-cased.
func upstreamQuotaExceeded(text string) bool {
	return strings.Contains(text, "insufficient_user_quota") ||
		strings.Contains(text, "credit insufficient balance")
}
