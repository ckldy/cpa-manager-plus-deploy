package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const DefaultMaxSSEEventBytes = 1 << 20

var (
	ErrSSEEventTooLarge      = errors.New("SSE event exceeds size limit")
	ErrInvalidSSEData        = errors.New("invalid SSE data fields")
	ErrSSETerminationMissing = errors.New("SSE stream terminated without done event")
)

type SOLOEvent struct {
	Event        string
	Response     string
	Reasoning    string
	ToolCalls    json.RawMessage
	Usage        map[string]any
	FinishReason string
	ErrorCode    int64
	ErrorMessage string
}

type SOLOStreamError struct {
	Code int64
	Msg  string
}

func (e *SOLOStreamError) Error() string {
	return fmt.Sprintf("solo error code=%d msg=%s", e.Code, e.Msg)
}

func ParseSOLOEvent(event, data string) (*SOLOEvent, error) {
	ev := &SOLOEvent{Event: strings.TrimSpace(event)}
	if data == "" {
		return ev, nil
	}
	switch ev.Event {
	case "output", "token_usage", "done", "error":
	default:
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		ev.Response = stringValue(raw["response"])
		ev.Reasoning = stringValue(raw["reasoning_content"])
		if calls, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(calls)
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		ev.FinishReason = stringValue(raw["finish_reason"])
	case "error":
		if code, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(code)
		}
		ev.ErrorMessage = stringValue(raw["message"])
	}
	return ev, nil
}

// DecodeSOLOEvents incrementally parses SSE regardless of reader slicing. Each
// event is bounded by maxEventBytes; multiple data lines follow SSE newline
// joining, while a second complete JSON data value is rejected.
func DecodeSOLOEvents(r io.Reader, maxEventBytes int, emit func(*SOLOEvent) error) error {
	if maxEventBytes <= 0 {
		maxEventBytes = DefaultMaxSSEEventBytes
	}
	br := bufio.NewReaderSize(r, maxEventBytes+1)
	var event string
	var data []string
	size := 0
	dispatch := func() error {
		defer func() {
			event = ""
			data = nil
			size = 0
		}()
		if event == "" && len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		if len(data) > 1 {
			for _, line := range data[:len(data)-1] {
				if json.Valid([]byte(strings.TrimSpace(line))) {
					return ErrInvalidSSEData
				}
			}
		}
		ev, err := ParseSOLOEvent(event, joined)
		if err != nil {
			return err
		}
		return emit(ev)
	}
	for {
		lineBytes, readErr := br.ReadSlice('\n')
		if readErr == bufio.ErrBufferFull {
			return ErrSSEEventTooLarge
		}
		size += len(lineBytes)
		if size > maxEventBytes {
			return ErrSSEEventTooLarge
		}
		if readErr != nil {
			if readErr == io.EOF && len(lineBytes) == 0 && event == "" && len(data) == 0 {
				return nil
			}
			if readErr == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return readErr
		}
		line := strings.TrimSuffix(string(lineBytes), "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
}

// EmitOpenAIDataValues converts SOLO events and emits bare OpenAI JSON data
// values. The callback receives JSON bytes without a "data:" prefix or framing
// newlines. The CPA OpenAI handler emits the final [DONE] marker on close.
func EmitOpenAIDataValues(r io.Reader, maxEventBytes int, emit func([]byte) error) error {
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	var usage map[string]any
	done := false
	emitChunk := func(delta map[string]any, finish string) error {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "",
			"choices": []any{choice},
		}
		if usage != nil {
			chunk["usage"] = usage
			usage = nil
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return emit(raw)
	}
	err := DecodeSOLOEvents(r, maxEventBytes, func(ev *SOLOEvent) error {
		switch ev.Event {
		case "output":
			delta := map[string]any{}
			if ev.Response != "" {
				delta["content"] = ev.Response
			}
			if ev.Reasoning != "" {
				delta["reasoning_content"] = ev.Reasoning
			}
			if calls := openAIToolCallDeltas(ev.ToolCalls); len(calls) > 0 {
				delta["tool_calls"] = calls
			}
			if len(delta) > 0 {
				return emitChunk(delta, "")
			}
		case "token_usage":
			usage = ev.Usage
		case "done":
			if err := emitChunk(map[string]any{}, ev.FinishReason); err != nil {
				return err
			}
			done = true
		case "error":
			return &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !done {
		return ErrSSETerminationMissing
	}
	return nil
}

// StreamOpenAIChunks is the framed SSE compatibility wrapper.
func StreamOpenAIChunks(w io.Writer, r io.Reader, maxEventBytes int) error {
	err := EmitOpenAIDataValues(r, maxEventBytes, func(value []byte) error {
		_, err := fmt.Fprintf(w, "data: %s\n\n", value)
		return err
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(w, "data: [DONE]\n\n")
	return err
}

func AggregateOpenAICompletion(r io.Reader, maxEventBytes int) (map[string]any, error) {
	var content, reasoning strings.Builder
	finish := "stop"
	var usage map[string]any
	calls := map[int]map[string]any{}
	order := []int{}
	done := false
	err := DecodeSOLOEvents(r, maxEventBytes, func(ev *SOLOEvent) error {
		switch ev.Event {
		case "output":
			content.WriteString(ev.Response)
			reasoning.WriteString(ev.Reasoning)
			mergeToolCalls(calls, &order, ev.ToolCalls)
		case "token_usage":
			usage = ev.Usage
		case "done":
			done = true
			if ev.FinishReason != "" {
				finish = ev.FinishReason
			}
		case "error":
			return &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !done {
		return nil, ErrSSETerminationMissing
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(order) > 0 {
		sortInts(order)
		out := make([]map[string]any, 0, len(order))
		for _, i := range order {
			out = append(out, calls[i])
		}
		message["tool_calls"] = out
	}
	resp := map[string]any{"id": fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), "object": "chat.completion", "created": time.Now().Unix(), "model": "", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

func openAIToolCallDeltas(raw json.RawMessage) []map[string]any {
	var calls []map[string]any
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &calls) != nil {
		return nil
	}
	for _, call := range calls {
		normalizeCall(call)
	}
	out := calls[:0]
	for _, call := range calls {
		if meaningfulToolCall(call) {
			out = append(out, call)
		}
	}
	return out
}
func meaningfulToolCall(call map[string]any) bool {
	if strings.TrimSpace(stringValue(call["id"])) != "" || strings.TrimSpace(stringValue(call["type"])) != "" {
		return true
	}
	fn, _ := call["function"].(map[string]any)
	return fn != nil && (strings.TrimSpace(stringValue(fn["name"])) != "" || stringValue(fn["arguments"]) != "")
}
func normalizeCall(call map[string]any) {
	if fc, ok := call["function_call"].(map[string]any); ok {
		call["function"] = fc
		delete(call, "function_call")
	}
	if fn, ok := call["function"].(map[string]any); ok {
		delete(fn, "namespace")
		delete(fn, "partial_arguments")
	}
}
func mergeToolCalls(dst map[int]map[string]any, order *[]int, raw json.RawMessage) {
	var calls []map[string]any
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &calls) != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return
		}
		calls = []map[string]any{one}
	}
	for _, call := range calls {
		normalizeCall(call)
		if !meaningfulToolCall(call) {
			continue
		}
		idx := 0
		if n, ok := call["index"].(float64); ok {
			idx = int(n)
		}
		merged, ok := dst[idx]
		if !ok {
			merged = map[string]any{"index": idx}
			dst[idx] = merged
			*order = append(*order, idx)
		}
		normalizeCall(call)
		for _, k := range []string{"id", "type"} {
			if v := stringValue(call[k]); v != "" {
				merged[k] = v
			}
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		mf, _ := merged["function"].(map[string]any)
		if mf == nil {
			mf = map[string]any{}
			merged["function"] = mf
		}
		if v := stringValue(fn["name"]); v != "" {
			mf["name"] = v
		}
		if v := stringValue(fn["arguments"]); v != "" {
			mf["arguments"] = stringValue(mf["arguments"]) + v
		}
	}
}
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}
