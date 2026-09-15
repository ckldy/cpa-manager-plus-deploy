package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func executeHTTP(ctx context.Context, client pluginapi.HostHTTPClient, callback string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if client != nil {
		return client.Do(ctx, req)
	}
	raw, e := callHostRPC(pluginabi.MethodHostHTTPDo, rpcHostHTTPRequest{HostCallbackID: callback, Request: req})
	if e != nil {
		return pluginapi.HTTPResponse{}, e
	}
	var r pluginapi.HTTPResponse
	e = json.Unmarshal(raw, &r)
	return r, e
}

var executorClientHook func(pluginapi.ExecutorRequest) pluginapi.HostHTTPClient

func clientFor(r pluginapi.ExecutorRequest) pluginapi.HostHTTPClient {
	if executorClientHook != nil {
		return executorClientHook(r)
	}
	return r.HTTPClient
}
func credentialFor(raw []byte) (*authStorage, error) {
	s, e := parseAuthStorage(raw)
	if e != nil {
		return nil, e
	}
	if s.AccessToken == "" {
		return nil, errors.New("missing access token")
	}
	return s, nil
}
func chatRequest(s *authStorage, payload []byte) (pluginapi.HTTPRequest, error) {
	body, e := BuildSOLOPayload(payload)
	if e != nil {
		return pluginapi.HTTPRequest{}, e
	}
	if len(body) > 4<<20 {
		return pluginapi.HTTPRequest{}, errors.New("request too large")
	}
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: traeAgentHost + traeChatPath, Headers: traeHeaders(s, true), Body: body}, nil
}
func handleExecutorExecute(raw []byte) ([]byte, error) {
	var rpc rpcExecutorRequest
	if e := json.Unmarshal(raw, &rpc); e != nil {
		return nil, e
	}
	s, e := credentialFor(rpc.StorageJSON)
	if e != nil {
		return nil, e
	}
	req, e := chatRequest(s, rpc.Payload)
	if e != nil {
		return nil, e
	}
	stream, e := openStream(context.Background(), clientFor(rpc.ExecutorRequest), rpc.HostCallbackID, req)
	if e != nil {
		return nil, e
	}
	defer stream.Close()
	result, e := AggregateOpenAICompletion(io.LimitReader(stream, 16<<20+1), DefaultMaxSSEEventBytes)
	if e != nil {
		return nil, e
	}
	payload, e := json.Marshal(result)
	if e != nil {
		return nil, e
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload, Headers: stream.headers, Metadata: map[string]any{"status_code": stream.status}})
}
func handleExecutorExecuteStream(raw []byte) ([]byte, error) {
	var rpc rpcExecutorRequest
	if e := json.Unmarshal(raw, &rpc); e != nil {
		return nil, e
	}
	s, e := credentialFor(rpc.StorageJSON)
	if e != nil {
		return nil, e
	}
	req, e := chatRequest(s, rpc.Payload)
	if e != nil {
		return nil, e
	}
	stream, e := openStream(context.Background(), clientFor(rpc.ExecutorRequest), rpc.HostCallbackID, req)
	if e != nil {
		return nil, e
	}
	if rpc.StreamID != "" {
		go func() {
			defer stream.Close()
			err := EmitOpenAIDataValues(stream, DefaultMaxSSEEventBytes, func(v []byte) error {
				_, e := callHostRPC(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: rpc.StreamID, Payload: v})
				return e
			})
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			_, _ = callHostRPC(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{StreamID: rpc.StreamID, Error: msg})
		}()
		return okEnvelope(streamResponse{Headers: stream.headers})
	}
	defer stream.Close()
	chunks := []pluginapi.ExecutorStreamChunk{}
	e = EmitOpenAIDataValues(stream, DefaultMaxSSEEventBytes, func(v []byte) error {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: append([]byte(nil), v...)})
		return nil
	})
	if e != nil {
		return nil, e
	}
	return okEnvelope(streamResponse{Headers: stream.headers, Chunks: chunks})
}

type upstreamStream struct {
	io.Reader
	close   func()
	status  int
	headers http.Header
}

func (s *upstreamStream) Close() {
	if s.close != nil {
		s.close()
		s.close = nil
	}
}
func openStream(ctx context.Context, client pluginapi.HostHTTPClient, callback string, req pluginapi.HTTPRequest) (*upstreamStream, error) {
	if client != nil {
		r, e := client.DoStream(ctx, req)
		if e != nil {
			return nil, e
		}
		if r.StatusCode >= 400 {
			b := readChunks(r.Chunks, 64<<10)
			return nil, upstreamStatus(pluginapi.HTTPResponse{StatusCode: r.StatusCode, Body: b})
		}
		return &upstreamStream{Reader: &chunkReader{chunks: r.Chunks}, status: r.StatusCode, headers: r.Headers}, nil
	}
	if callback == "" {
		return nil, errors.New("host HTTP callback required")
	}
	raw, e := callHostRPC(pluginabi.MethodHostHTTPDoStream, rpcHostHTTPRequest{HostCallbackID: callback, Request: req})
	if e != nil {
		return nil, e
	}
	var r rpcHostHTTPStreamResponse
	if e = json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	if r.StreamID == "" {
		return nil, errors.New("empty host HTTP stream")
	}
	hs := &hostStreamReader{id: r.StreamID}
	if r.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(hs, 64<<10))
		hs.Close()
		return nil, upstreamStatus(pluginapi.HTTPResponse{StatusCode: r.StatusCode, Body: b})
	}
	return &upstreamStream{Reader: hs, close: hs.Close, status: r.StatusCode, headers: r.Headers}, nil
}

type chunkReader struct {
	chunks <-chan pluginapi.HTTPStreamChunk
	buf    []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		c, ok := <-r.chunks
		if !ok {
			return 0, io.EOF
		}
		if c.Err != nil {
			return 0, c.Err
		}
		r.buf = c.Payload
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
func readChunks(ch <-chan pluginapi.HTTPStreamChunk, n int) []byte {
	var b bytes.Buffer
	for c := range ch {
		if c.Err != nil {
			break
		}
		left := n - b.Len()
		if left <= 0 {
			break
		}
		if len(c.Payload) > left {
			c.Payload = c.Payload[:left]
		}
		b.Write(c.Payload)
	}
	return b.Bytes()
}

type hostStreamReader struct {
	id   string
	buf  []byte
	done bool
}

func (r *hostStreamReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 && !r.done {
		raw, e := callHostRPC(pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{r.id})
		if e != nil {
			return 0, e
		}
		var c rpcHostHTTPStreamReadResponse
		if e = json.Unmarshal(raw, &c); e != nil {
			return 0, e
		}
		if c.Error != "" {
			return 0, fmt.Errorf("%s", c.Error)
		}
		r.buf = c.Payload
		r.done = c.Done
	}
	if len(r.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
func (r *hostStreamReader) Close() {
	if r.id != "" {
		_, _ = callHostRPC(pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{r.id})
		r.id = ""
	}
}
