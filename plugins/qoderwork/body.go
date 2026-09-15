// body.go constructs the QoderWork agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// cpaToUpstreamKey maps CPA-facing model names to upstream keys.
// Unknown names pass through unchanged (server silently routes to auto).
func cpaToUpstreamKey(cpaModel string) string {
	switch cpaModel {
	case "qoder-auto", "auto":
		return "auto"
	case "qwen3.8-max-preview", "qwen3.8-max", "qmodel_preview":
		return "qmodel_preview"
	case "qwen3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qmodel":
		return "qmodel"
	case "qwen3.6-flash", "q36fmodel":
		return "q36fmodel"
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-v4-flash", "dfmodel":
		return "dfmodel"
	case "glm-5.2", "gm51model":
		return "gm51model"
	case "kimi-k2.7-code", "kmodel":
		return "kmodel"
	case "minimax-m2.7", "mmodel":
		return "mmodel"
	}
	return cpaModel
}

// openAIMessage is one message in the OpenAI chat completion format.
// Tool-history fields (tool_calls / tool_call_id) are preserved verbatim so
// the Qoder upstream can deserialize multi-turn agent conversations: without
// tool_call_id on a `tool` message the gateway returns
// "messages[i]: missing field `tool_call_id`" → provider_error → empty_stream.
type openAIMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

// UnmarshalJSON accepts legacy string content and OpenAI content-part arrays.
// QoderWork is text-only, therefore text/input_text/output_text parts are joined.
func (m *openAIMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ToolCallID = raw.ToolCallID
	if len(raw.ToolCalls) > 0 && string(raw.ToolCalls) != "null" {
		m.ToolCalls = append(json.RawMessage(nil), raw.ToolCalls...)
	}
	// Messages without a content field (e.g. assistant tool_calls-only turns)
	// unmarshal to a nil RawMessage; treat absent/null content as empty text.
	if len(raw.Content) == 0 || string(raw.Content) == "null" {
		m.Content = ""
		return nil
	}
	if err := json.Unmarshal(raw.Content, &m.Content); err == nil {
		return nil
	}
	var parts []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		InputText string `json:"input_text"`
	}
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return fmt.Errorf("unsupported message content: %w", err)
	}
	m.Content = ""
	for _, part := range parts {
		if part.Type == "text" || part.Type == "input_text" || part.Type == "output_text" {
			if part.Text != "" {
				m.Content += part.Text
			} else {
				m.Content += part.InputText
			}
		}
	}
	return nil
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model      string          `json:"model"`
	Messages   []openAIMessage `json:"messages"`
	Stream     bool            `json:"stream"`
	Tools      json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

// extractLatestUserPrompt returns the content of the last user message.
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey).
func buildQoderBody(req *openAIRequest, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"

	// model_config
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
	}

	// chat_context.text.text + chat_context.extra.originalContent.text
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
			}
		}
	}

	// messages: keep system prompt from baseprompt (template has it), replace user/assistant
	var systemMsgs []any
	if msgs, ok := base["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if role, _ := mm["role"].(string); role == "system" {
					systemMsgs = append(systemMsgs, m)
				}
			}
		}
	}
	// Append the actual conversation. Tool-history fields (tool_calls on
	// assistant turns, tool_call_id on tool turns) are forwarded verbatim so
	// the gateway can deserialize agent multi-turn conversations (otherwise it
	// rejects with "missing field `tool_call_id`" → provider_error).
	for _, m := range req.Messages {
		out := map[string]any{
			"role":    m.Role,
			"content": m.Content,
		}
		if m.ToolCallID != "" {
			out["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			out["tool_calls"] = m.ToolCalls
		}
		systemMsgs = append(systemMsgs, out)
	}
	base["messages"] = systemMsgs

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	// Client tools: when the caller declares its own OpenAI tool list, replace
	// the template's baked-in Qoder CLI tools with the client tools. Verified
	// against the live gateway: the model then emits tool_calls for the client
	// tool names (instead of ignoring the client and using Qoder's built-ins),
	// enabling the host to execute tools and round-trip results. When the
	// client sends no tools, the template's built-in tool list is left as-is.
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		var tools []any
		if err := json.Unmarshal(req.Tools, &tools); err == nil && len(tools) > 0 {
			base["tools"] = tools
		}
	}
	// tool_choice passthrough (auto / none / {"type":"function",...}) for
	// strict clients. Omitempty-style: only set when present and non-null.
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var choice any
		if err := json.Unmarshal(req.ToolChoice, &choice); err == nil {
			base["tool_choice"] = choice
		}
	}

	return json.Marshal(base)
}
