package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAggregateFiltersEmptyToolCallPlaceholder(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"OK\",\"tool_calls\":[{\"index\":0}]}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	resp, err := AggregateOpenAICompletion(strings.NewReader(input), 4096)
	if err != nil {
		t.Fatal(err)
	}
	message := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, exists := message["tool_calls"]; exists {
		t.Fatalf("empty tool call leaked: %#v", message)
	}
}

func TestParseSOLOEventIgnoresUnknownScalarData(t *testing.T) {
	ev, err := ParseSOLOEvent("ping", `"keepalive"`)
	if err != nil || ev.Event != "ping" {
		t.Fatalf("event=%#v err=%v", ev, err)
	}
}

type slicedReader struct {
	data []byte
	cuts []int
	n    int
}

func (r *slicedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := len(r.data)
	if len(r.cuts) > 0 {
		n = r.cuts[r.n%len(r.cuts)]
		r.n++
	}
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

type failEOFReader struct{ data []byte }

func (r *failEOFReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestDecodeSOLOEventsArbitrarySlicesAndMultilineData(t *testing.T) {
	input := "event: output\r\ndata: {\"response\":\"hel\",\r\ndata: \"reasoning_content\":\"r\"}\r\n\r\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	var events []*SOLOEvent
	err := DecodeSOLOEvents(&slicedReader{data: []byte(input), cuts: []int{1, 2, 5, 3}}, DefaultMaxSSEEventBytes, func(ev *SOLOEvent) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Response != "hel" || events[0].Reasoning != "r" || events[1].FinishReason != "stop" {
		t.Fatalf("events: %#v", events)
	}
}

func TestDecodeSOLOEventsDoubleDataRegression(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"A\"}\ndata: {\"response\":\"B\"}\n\n"
	count := 0
	err := DecodeSOLOEvents(strings.NewReader(input), DefaultMaxSSEEventBytes, func(ev *SOLOEvent) error { count++; return nil })
	if err == nil || !errors.Is(err, ErrInvalidSSEData) || count != 0 {
		t.Fatalf("err=%v count=%d", err, count)
	}
}

func TestDecodeSOLOEventsErrorAndAbnormalEOF(t *testing.T) {
	input := "event: error\ndata: {\"code\":1005,\"message\":\"limit\"}\n\n"
	var got *SOLOEvent
	if err := DecodeSOLOEvents(strings.NewReader(input), 1024, func(ev *SOLOEvent) error { got = ev; return nil }); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Event != "error" || got.ErrorCode != 1005 || got.ErrorMessage != "limit" {
		t.Fatalf("event: %#v", got)
	}
	if err := DecodeSOLOEvents(&failEOFReader{data: []byte("event: output\ndata: {\"response\":\"x\"}")}, 1024, func(*SOLOEvent) error { return nil }); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want unexpected EOF, got %v", err)
	}
}

func TestDecodeSOLOEventsSizeLimit(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"0123456789\"}\n\n"
	if err := DecodeSOLOEvents(strings.NewReader(input), 12, func(*SOLOEvent) error { return nil }); !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("want size error, got %v", err)
	}
}

func TestEmitOpenAIDataValues(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"Hi\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	var values [][]byte
	err := EmitOpenAIDataValues(strings.NewReader(input), 1024, func(value []byte) error {
		values = append(values, append([]byte(nil), value...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || bytes.HasPrefix(values[0], []byte("data:")) {
		t.Fatalf("raw values: %q", values)
	}
	if strings.Contains(string(values[0])+string(values[1]), "[DONE]") {
		t.Fatalf("CPA chunks must not contain DONE: %q", values)
	}
	chunk := decodeChunk(t, string(values[0]))
	choice := chunk["choices"].([]any)[0].(map[string]any)
	if choice["delta"].(map[string]any)["content"] != "Hi" {
		t.Fatalf("chunk: %#v", chunk)
	}
}

func TestEmitOpenAIDataValuesPropagatesCallbackError(t *testing.T) {
	want := errors.New("emit failed")
	input := "event: output\ndata: {\"response\":\"Hi\"}\n\n"
	err := EmitOpenAIDataValues(strings.NewReader(input), 1024, func([]byte) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("want callback error, got %v", err)
	}
}

func TestEmitOpenAIDataValuesRequiresDone(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"partial\"}\n\n"
	var values [][]byte
	err := EmitOpenAIDataValues(strings.NewReader(input), 1024, func(value []byte) error {
		values = append(values, append([]byte(nil), value...))
		return nil
	})
	if !errors.Is(err, ErrSSETerminationMissing) {
		t.Fatalf("want ErrSSETerminationMissing, got %v", err)
	}
	for _, value := range values {
		if string(value) == "[DONE]" {
			t.Fatalf("missing done must not emit [DONE]: %q", values)
		}
	}
}

func TestAggregateOpenAICompletionRequiresDone(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"partial\"}\n\n"
	resp, err := AggregateOpenAICompletion(strings.NewReader(input), 1024)
	if !errors.Is(err, ErrSSETerminationMissing) || resp != nil {
		t.Fatalf("response=%#v error=%v; want nil ErrSSETerminationMissing", resp, err)
	}
}

func TestStreamOpenAIChunks(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"Hi\",\"reasoning_content\":\"R\",\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function_call\":{\"name\":\"f\",\"arguments\":\"{\",\"namespace\":\"x\"}}]}\n\nevent: token_usage\ndata: {\"total_tokens\":3}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
	var out bytes.Buffer
	if err := StreamOpenAIChunks(&out, strings.NewReader(input), 4096); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Count(text, "data: [DONE]") != 1 || !strings.Contains(text, `"content":"Hi"`) || !strings.Contains(text, `"reasoning_content":"R"`) || !strings.Contains(text, `"finish_reason":"tool_calls"`) || !strings.Contains(text, `"usage":{"total_tokens":3}`) || strings.Contains(text, "function_call") || strings.Contains(text, "namespace") {
		t.Fatalf("output: %s", text)
	}
}

func TestStreamOpenAIChunksEventError(t *testing.T) {
	var out bytes.Buffer
	input := "event: error\ndata: {\"code\":1005,\"message\":\"limit\"}\n\n"
	err := StreamOpenAIChunks(&out, strings.NewReader(input), 1024)
	var streamErr *SOLOStreamError
	if !errors.As(err, &streamErr) || streamErr.Code != 1005 {
		t.Fatalf("want SOLOStreamError, got %v", err)
	}
	if got := out.String(); strings.Contains(got, "[DONE]") {
		t.Fatalf("error stream must not emit normal DONE: %q", got)
	}
}

func TestAggregateOpenAICompletionAndEventError(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"Hi\",\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function_call\":{\"name\":\"f\",\"arguments\":\"{\"}}]}\n\nevent: output\ndata: {\"response\":\"!\",\"tool_calls\":[{\"index\":0,\"function_call\":{\"arguments\":\"}\"}}]}\n\nevent: token_usage\ndata: {\"total_tokens\":3}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
	resp, err := AggregateOpenAICompletion(strings.NewReader(input), 4096)
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hi!" || choice["finish_reason"] != "tool_calls" {
		t.Fatalf("response: %#v", resp)
	}
	call := msg["tool_calls"].([]map[string]any)[0]
	fn := call["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != "{}" {
		t.Fatalf("call: %#v", call)
	}

	_, err = AggregateOpenAICompletion(strings.NewReader("event: error\ndata: {\"code\":1005,\"message\":\"limit\"}\n\n"), 1024)
	var se *SOLOStreamError
	if !errors.As(err, &se) || se.Code != 1005 {
		t.Fatalf("want SOLOStreamError, got %v", err)
	}
}

func TestStreamOpenAIChunksMissingDoneNoSyntheticDone(t *testing.T) {
	var out bytes.Buffer
	input := "event: output\ndata: {\"response\":\"partial\"}\n\n"
	err := StreamOpenAIChunks(&out, strings.NewReader(input), 1024)
	if !errors.Is(err, ErrSSETerminationMissing) {
		t.Fatalf("want ErrSSETerminationMissing, got %v", err)
	}
	if strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("missing done must not synthesize [DONE]: %q", out.String())
	}
}

func TestStreamOpenAIChunksAbnormalEOFNoDone(t *testing.T) {
	var out bytes.Buffer
	err := StreamOpenAIChunks(&out, &failEOFReader{data: []byte("event: output\ndata: {\"response\":\"x\"}")}, 1024)
	if !errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("err=%v output=%q", err, out.String())
	}
}

func decodeChunk(t *testing.T, value string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(value, "data: ")), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
