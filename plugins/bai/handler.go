package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// envelope wraps every plugin response.
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

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
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

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRegister:
		return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: ProviderBAI, Models: baiModels()})
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: ProviderBAI, Models: baiModels()})
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: ProviderBAI})
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: ProviderBAI})
	case pluginabi.MethodExecutorExecute:
		return handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecutorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return handleExecutorCountTokens(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistrationResponse{
			Resources: []pluginapi.ResourceRoute{
				{
					Path:        "/status",
					Menu:        "B.AI 账号",
					Description: "B.AI API Key 状态、余额与可用模型。",
				},
			},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ProviderBAI is the plugin provider key.
const ProviderBAI = "bai"

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "bai",
			Version:          "0.1.0",
			Author:           "ckldy",
			GitHubRepository: "https://github.com/ckldy/cpa-plugin-bai",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "display_models", Type: pluginapi.ConfigFieldTypeString, Description: "Deprecated; models are fetched from /v1/models at runtime."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"openai"},
			ExecutorOutputFormats: []string{"openai"},
			ManagementAPI:         true,
		},
	}
}

func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func handleExecutorCountTokens(request []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	// Lightweight: report a placeholder count; CPA handles most token accounting.
	return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"total_tokens":0}`)})
}

// baiModels returns the static catalog. Names must match the upstream
// /v1/models list exactly (B.AI uses dots: claude-opus-4.8); the status page
// additionally shows the live list fetched with the stored key.
func baiModels() []pluginapi.ModelInfo {
	now := time.Now().Unix()
	base := pluginapi.ModelInfo{
		Object:                     "model",
		OwnedBy:                    ProviderBAI,
		Created:                    now,
		SupportedGenerationMethods: []string{"chat"},
		Type:                       "chat",
		UserDefined:                true,
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
	}
	mk := func(id string, ctxLen, maxOut int64, display string) pluginapi.ModelInfo {
		m := base
		m.ID = "bai-" + id
		m.Name = id
		display = strings.TrimSuffix(display, "（免押）")
		display = strings.TrimSuffix(display, "（需充值）")
		if probeClassForModel(id) == probeFree {
			display += " (free)"
		}
		m.DisplayName = display
		m.ContextLength = ctxLen
		m.MaxCompletionTokens = maxOut
		m.InputTokenLimit = ctxLen
		m.OutputTokenLimit = maxOut
		return m
	}
	vision := func(id string, ctxLen, maxOut int64, display string) pluginapi.ModelInfo {
		m := mk(id, ctxLen, maxOut, display)
		m.SupportedInputModalities = []string{"text", "image"}
		return m
	}
	return []pluginapi.ModelInfo{
		// Free tier (no deposit required; verified 2026-08-28):
		mk("glm-5.3-flash", 200000, 128000, "B.AI GLM-5.3 Flash（免押）"),
		mk("deepseek-v4-flash", 128000, 64000, "B.AI DeepSeek V4 Flash（免押）"),
		mk("qwen3.8-flash", 256000, 32000, "B.AI Qwen3.8 Flash（免押）"),
		mk("hy3", 128000, 32000, "B.AI Hunyuan T1（免押）"),
		// Premium tier (403 access_denied until a deposit unlocks the account):
		mk("claude-opus-4.8", 200000, 64000, "B.AI Claude Opus 4.8（需充值）"),
		mk("claude-opus-5", 200000, 64000, "B.AI Claude Opus 5（需充值）"),
		mk("claude-sonnet-4.6", 200000, 64000, "B.AI Claude Sonnet 4.6（需充值）"),
		mk("claude-sonnet-5", 200000, 64000, "B.AI Claude Sonnet 5（需充值）"),
		mk("claude-haiku-4.5", 200000, 32000, "B.AI Claude Haiku 4.5（需充值）"),
		mk("gpt-5.6-terra", 400000, 128000, "B.AI GPT-5.6 Terra（需充值）"),
		mk("gpt-5.6-sol", 400000, 128000, "B.AI GPT-5.6 Sol（需充值）"),
		mk("gpt-5.6-luna", 400000, 128000, "B.AI GPT-5.6 Luna（需充值）"),
		mk("gpt-5.5", 400000, 128000, "B.AI GPT-5.5（需充值）"),
		mk("gpt-5.4", 400000, 128000, "B.AI GPT-5.4（需充值）"),
		vision("gemini-3.1-pro", 1000000, 65536, "B.AI Gemini 3.1 Pro（需充值）"),
		vision("gemini-3.5-flash", 1000000, 65536, "B.AI Gemini 3.5 Flash（需充值）"),
		vision("gemini-3.6-flash", 1000000, 65536, "B.AI Gemini 3.6 Flash（需充值）"),
		mk("deepseek-v4-pro", 128000, 64000, "B.AI DeepSeek V4 Pro（需充值）"),
		mk("glm-5.3", 200000, 128000, "B.AI GLM-5.3（需充值）"),
		mk("glm-5.2", 200000, 128000, "B.AI GLM-5.2（需充值）"),
		mk("kimi-k3", 256000, 64000, "B.AI Kimi K3（需充值）"),
		mk("minimax-m3", 200000, 64000, "B.AI MiniMax M3（需充值）"),
		mk("qwen3.8-max", 256000, 64000, "B.AI Qwen3.8 Max（需充值）"),
	}
}
