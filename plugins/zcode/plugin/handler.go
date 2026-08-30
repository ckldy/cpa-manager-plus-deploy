package main

import (
	"encoding/json"
	"net/http"

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
	ModelRegistrar           bool                         `json:"model_registrar"`
	ModelProvider            bool                         `json:"model_provider"`
	AuthProvider             bool                         `json:"auth_provider"`
	FrontendAuthProvider     bool                         `json:"frontend_auth_provider"`
	Executor                 bool                         `json:"executor"`
	ExecutorModelScope       pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats     []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats    []string                     `json:"executor_output_formats,omitempty"`
	RequestTranslator        bool                         `json:"request_translator"`
	RequestNormalizer        bool                         `json:"request_normalizer"`
	ResponseTranslator       bool                         `json:"response_translator"`
	ResponseBeforeTranslator bool                         `json:"response_before_translator"`
	ResponseAfterTranslator  bool                         `json:"response_after_translator"`
	ThinkingApplier          bool                         `json:"thinking_applier"`
	UsagePlugin              bool                         `json:"usage_plugin"`
	CommandLinePlugin        bool                         `json:"command_line_plugin"`
	ManagementAPI            bool                         `json:"management_api"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
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
		applyRouteConfig(request)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRegister:
		return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: ProviderZCode, Models: zcodeModels()})
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: ProviderZCode, Models: zcodeModels()})
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: ProviderZCode})
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart:
		return handleAuthLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return handleAuthLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: ProviderZCode})
	case pluginabi.MethodExecutorExecute:
		return handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecutorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return handleExecutorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return handleExecutorHTTPRequest(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistrationResponse{
			Routes: []pluginapi.ManagementRoute{
				{Method: http.MethodPost, Path: "/status"},
			},
			Resources: []pluginapi.ResourceRoute{
				{
					Path:        "/status",
					Menu:        "ZCode 账号",
					Description: "ZCode (Z.ai) 账号池与调用状态。",
				},
				{Path: "/callback"},
			},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
