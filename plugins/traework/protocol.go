package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	SOLOFunction      = "solo_work_lite"
	DefaultConfigName = "glm-5.2"
)

var ErrInvalidOpenAIPayload = errors.New("invalid OpenAI payload")

// BuildSOLOPayload strictly validates and converts an OpenAI chat request.
func BuildSOLOPayload(src []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, fmt.Errorf("%w: JSON: %v", ErrInvalidOpenAIPayload, err)
	}
	model, ok := obj["model"].(string)
	model = strings.TrimSpace(model)
	if !ok || model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidOpenAIPayload)
	}
	upstreamModel := strings.TrimSpace(strings.TrimPrefix(model, ProviderTraeWork+"-"))
	if upstreamModel == "" {
		return nil, fmt.Errorf("%w: upstream model is required", ErrInvalidOpenAIPayload)
	}
	if cfg := modelConfigName(model); cfg != "" {
		upstreamModel = cfg
	}
	messages, ok := obj["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("%w: messages must be an array", ErrInvalidOpenAIPayload)
	}

	for messageIndex, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: messages[%d] must be an object", ErrInvalidOpenAIPayload, messageIndex)
		}
		if message["role"] == "assistant" {
			if rawCalls, present := message["tool_calls"]; present {
				calls, ok := rawCalls.([]any)
				if !ok {
					return nil, fmt.Errorf("%w: messages[%d].tool_calls must be an array", ErrInvalidOpenAIPayload, messageIndex)
				}
				for callIndex, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("%w: messages[%d].tool_calls[%d] must be an object", ErrInvalidOpenAIPayload, messageIndex, callIndex)
					}
					fn, ok := call["function"].(map[string]any)
					if !ok {
						fn, ok = call["function_call"].(map[string]any)
					}
					if !ok || strings.TrimSpace(stringValue(fn["name"])) == "" {
						return nil, fmt.Errorf("%w: messages[%d].tool_calls[%d].function.name is required", ErrInvalidOpenAIPayload, messageIndex, callIndex)
					}
					call["function_call"] = fn
					delete(call, "function")
				}
			}
		}
		if content, ok := message["content"].(string); ok {
			message["content"] = []any{map[string]any{"type": "text", "text": content}}
		}
	}

	if err := normalizeToolChoice(obj); err != nil {
		return nil, err
	}
	if err := normalizeTools(obj); err != nil {
		return nil, err
	}
	obj["stream"] = true
	obj["function"] = SOLOFunction
	obj["model"] = upstreamModel
	obj["config_name"] = upstreamModel
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal SOLO payload: %w", err)
	}
	return out, nil
}

// PrepareBody is a compatibility wrapper. Invalid input returns nil rather than
// being forwarded to the upstream unchanged. New executor code should call
// BuildSOLOPayload and handle its error.
func PrepareBody(src []byte) []byte {
	out, err := BuildSOLOPayload(src)
	if err != nil {
		return nil
	}
	return out
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// modelConfigName resolves a user-facing model ID (traework-<display_name>) back
// to the upstream config_name used in SOLO requests. Falls back to the
// prefix-stripped id when the registry has no match (e.g. legacy IDs).
func modelConfigName(fullID string) string {
	id := strings.TrimSpace(strings.TrimPrefix(fullID, ProviderTraeWork+"-"))
	if id == "" {
		return ""
	}
	for _, m := range traeModels() {
		if strings.TrimPrefix(strings.TrimSpace(m.ID), ProviderTraeWork+"-") == id && strings.TrimSpace(m.Name) != "" {
			return strings.TrimSpace(m.Name)
		}
	}
	return id
}

func normalizeToolChoice(obj map[string]any) error {
	choice, ok := obj["tool_choice"]
	if !ok {
		return nil
	}
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	switch v := choice.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ := strings.ToLower(strings.TrimSpace(stringValue(v["type"])))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name = stringValue(fn["name"])
			}
			if name == "" {
				name = stringValue(v["name"])
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("%w: tool_choice function name is required", ErrInvalidOpenAIPayload)
			}
			obj["tool_choice"] = name
		default:
			return fmt.Errorf("%w: unsupported tool_choice", ErrInvalidOpenAIPayload)
		}
	default:
		return fmt.Errorf("%w: invalid tool_choice", ErrInvalidOpenAIPayload)
	}
	return nil
}

func normalizeTools(obj map[string]any) error {
	rawTools, present := obj["tools"]
	if !present {
		return nil
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return fmt.Errorf("%w: tools must be an array", ErrInvalidOpenAIPayload)
	}
	for index, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: tools[%d] must be an object", ErrInvalidOpenAIPayload, index)
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok || strings.TrimSpace(stringValue(fn["name"])) == "" {
			return fmt.Errorf("%w: tools[%d].function.name is required", ErrInvalidOpenAIPayload, index)
		}
		if params, ok := fn["parameters"].(map[string]any); ok {
			encoded, err := json.Marshal(params)
			if err != nil {
				return fmt.Errorf("%w: tools[%d].function.parameters: %v", ErrInvalidOpenAIPayload, index, err)
			}
			fn["parameters"] = string(encoded)
		}
	}
	return nil
}
