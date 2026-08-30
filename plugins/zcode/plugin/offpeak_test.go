package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestOffPeakDisabledAndStrictEligibility(t *testing.T) {
	cfg := defaultOffPeakConfig()
	if cfg.Enabled {
		t.Fatal("off-peak must default disabled")
	}
	_, err := newOffPeakExecution(cfg, authStorage{Provider: "zai", ZCodeJWTToken: "jwt", APIKey: "key"}, "glm-4.5-flash", []byte(`{}`))
	if !errors.Is(err, errOffPeakDisabled) {
		t.Fatalf("err=%v", err)
	}
	cfg.Enabled = true
	cfg.AllowedModels = map[string]bool{"glm-4.5-flash": true}
	if _, err = newOffPeakExecution(cfg, authStorage{Provider: "zai", ZCodeJWTToken: "jwt"}, "glm-4.5-flash", []byte(`{}`)); !errors.Is(err, errOffPeakCredentialUnsupported) {
		t.Fatalf("err=%v", err)
	}
	if _, err = newOffPeakExecution(cfg, authStorage{Provider: "bigmodel", ZCodeJWTToken: "jwt", APIKey: "key"}, "glm-4.5-flash", []byte(`{}`)); !errors.Is(err, errOffPeakPlatformUnsupported) {
		t.Fatalf("err=%v", err)
	}
	if _, err = newOffPeakExecution(cfg, authStorage{Provider: "zai", ZCodeJWTToken: "jwt", APIKey: "key"}, "other", []byte(`{}`)); !errors.Is(err, errOffPeakModelUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestOffPeakRequestBuildersUseConfirmedContract(t *testing.T) {
	e := mustOffPeakExecution(t)
	checks := []struct {
		req            pluginapi.HTTPRequest
		method, suffix string
	}{
		{e.buildPreflightRequest(), http.MethodGet, "/ticket/availability"},
		{e.buildTakeRequest("task-1"), http.MethodPost, "/ticket"},
		{e.buildStatusRequest("ticket-1"), http.MethodPost, "/ticket/status"},
		{e.buildRunRequest("ticket-1"), http.MethodPost, "/anthropic/v1/messages"},
		{e.buildSettleRequest("ticket/1"), http.MethodPost, "/ticket/ticket%2F1/settle"},
	}
	for _, c := range checks {
		if c.req.Method != c.method || !strings.HasSuffix(c.req.URL, c.suffix) {
			t.Fatalf("bad request: %#v", c.req)
		}
		if c.req.Headers.Get("Authorization") != "Bearer jwt" || c.req.Headers.Get("X-Coding-Plan-Api-Key") != "key" {
			t.Fatal("missing isolated credentials")
		}
	}
}

func TestOffPeakExecuteStateMachineAndResultRead(t *testing.T) {
	e := mustOffPeakExecution(t)
	var calls []string
	do := func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls = append(calls, req.URL)
		switch len(calls) {
		case 1:
			return jsonHTTP(200, `{"code":0,"data":{"can_take_number":true}}`), nil
		case 2:
			return jsonHTTP(200, `{"code":0,"data":{"ticket_id":"t1","state":"queued"}}`), nil
		case 3:
			return jsonHTTP(200, `{"code":0,"data":{"tickets":[{"ticket_id":"t1","state":"active"}]}}`), nil
		case 4:
			return jsonHTTP(200, `{"id":"msg","content":[{"type":"text","text":"ok"}]}`), nil
		case 5:
			return jsonHTTP(200, ``), nil
		default:
			t.Fatalf("unexpected call %d", len(calls))
			return pluginapi.HTTPResponse{}, nil
		}
	}
	result, transitions, err := e.Execute(context.Background(), nil, "", do, func(time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if string(result) == "" || strings.Join(stateStrings(transitions), ",") != "pending,running,succeeded" {
		t.Fatalf("transitions=%v result=%s", transitions, result)
	}
	if len(calls) != 5 || !strings.HasSuffix(calls[4], "/ticket/t1/settle") {
		t.Fatalf("ticket was not settled exactly once: %v", calls)
	}
}

func TestOffPeakCapsPollingBodyAndSanitizesErrors(t *testing.T) {
	// Poll limit: keep the body cap large enough that normal protocol envelopes
	// reach the polling branch.
	e := mustOffPeakExecution(t)
	e.Config.MaxPolls = 1
	e.Config.MaxBodyBytes = 256
	responses := []pluginapi.HTTPResponse{
		jsonHTTP(200, `{"can_take_number":true}`),
		jsonHTTP(200, `{"ticket_id":"t1","state":"queued"}`),
		jsonHTTP(200, `{"tickets":[{"ticket_id":"t1","state":"queued"}]}`),
		jsonHTTP(200, ``),
	}
	i := 0
	do := func(context.Context, pluginapi.HostHTTPClient, string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		r := responses[i]
		i++
		return r, nil
	}
	_, transitions, err := e.Execute(context.Background(), nil, "", do, func(time.Duration) error { return nil })
	if !errors.Is(err, errOffPeakPollLimit) || transitions[len(transitions)-1].State != offPeakFailed {
		t.Fatalf("err=%v transitions=%v", err, transitions)
	}

	// Body cap: test independently so it cannot mask the poll-limit assertion.
	bodyLimited := mustOffPeakExecution(t)
	bodyLimited.Config.MaxBodyBytes = 8
	_, transitions, err = bodyLimited.Execute(context.Background(), nil, "", func(context.Context, pluginapi.HostHTTPClient, string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonHTTP(200, `{"can_take_number":true}`), nil
	}, func(time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "body limit") || transitions[len(transitions)-1].State != offPeakFailed {
		t.Fatalf("body-limit err=%v transitions=%v", err, transitions)
	}
	if strings.Contains(sanitizeOffPeakError(errors.New("Bearer secret.jwt api-key=paid")), "secret") {
		t.Fatal("error leaked credentials")
	}
}

func TestOffPeakModelSelectionIsExplicit(t *testing.T) {
	model, ok := offPeakNativeModel([]byte(`{"model":"zcode-offpeak-glm-4.5-flash"}`))
	if !ok || model != "glm-4.5-flash" {
		t.Fatalf("model=%q ok=%t", model, ok)
	}
	if _, ok := offPeakNativeModel([]byte(`{"model":"zcode-glm-4.5-flash"}`)); ok {
		t.Fatal("ordinary free model must not enter off-peak")
	}
}

func TestOffPeakKeepaliveFrameIsSSEComment(t *testing.T) {
	if got := string(offPeakKeepaliveFrame()); got != ": keepalive\n\n" {
		t.Fatalf("keepalive=%q", got)
	}
}
func TestOffPeakSettleStatusPolicy(t *testing.T) {
	for _, status := range []int{200, 204, 299, 404, 409} {
		if !offPeakSettleSuccessful(status) {
			t.Fatalf("status %d must succeed", status)
		}
	}
	for _, status := range []int{400, 401, 403, 429, 500} {
		if offPeakSettleSuccessful(status) {
			t.Fatalf("status %d must fail", status)
		}
	}
}

func TestOffPeakSettleFailureDoesNotClaimSuccess(t *testing.T) {
	e := mustOffPeakExecution(t)
	responses := []pluginapi.HTTPResponse{
		jsonHTTP(200, `{"can_take_number":true}`),
		jsonHTTP(200, `{"ticket_id":"t1","state":"active"}`),
		jsonHTTP(200, `{"id":"msg"}`),
		jsonHTTP(400, ``),
	}
	i := 0
	_, transitions, err := e.Execute(context.Background(), nil, "", func(context.Context, pluginapi.HostHTTPClient, string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		r := responses[i]
		i++
		return r, nil
	}, func(time.Duration) error { return nil })
	if err == nil || i != 4 || transitions[len(transitions)-1].State != offPeakFailed {
		t.Fatalf("err=%v calls=%d transitions=%v", err, i, transitions)
	}
	for _, transition := range transitions {
		if transition.State == offPeakSucceeded {
			t.Fatal("settle failure claimed success")
		}
	}
}

func TestOffPeakExpiredTicketRetriedOnceButRunNeverReplayed(t *testing.T) {
	e := mustOffPeakExecution(t)
	var takes, runs, settles int
	do := func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/ticket/availability"):
			return jsonHTTP(200, `{"can_take_number":true}`), nil
		case strings.HasSuffix(req.URL, "/ticket"):
			takes++
			if takes == 1 {
				return jsonHTTP(200, `{"ticket_id":"old","state":"expired"}`), nil
			}
			return jsonHTTP(200, `{"ticket_id":"new","state":"active"}`), nil
		case strings.HasSuffix(req.URL, "/settle"):
			settles++
			return jsonHTTP(404, ``), nil
		case strings.HasSuffix(req.URL, "/anthropic/v1/messages"):
			runs++
			return jsonHTTP(503, ``), nil
		default:
			return pluginapi.HTTPResponse{}, errors.New("unexpected request")
		}
	}
	_, _, err := e.Execute(context.Background(), nil, "", do, func(time.Duration) error { return nil })
	if err == nil || takes != 2 || runs != 1 || settles != 2 {
		t.Fatalf("err=%v takes=%d runs=%d settles=%d", err, takes, runs, settles)
	}
}

func TestOffPeakStatusSelectsMatchingTicket(t *testing.T) {
	e := mustOffPeakExecution(t)
	var runs int
	do := func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/ticket/availability"):
			return jsonHTTP(200, `{"can_take_number":true}`), nil
		case strings.HasSuffix(req.URL, "/ticket"):
			return jsonHTTP(200, `{"ticket_id":"t1","state":"queued"}`), nil
		case strings.HasSuffix(req.URL, "/ticket/status"):
			return jsonHTTP(200, `{"tickets":[{"ticket_id":"other","state":"expired"},{"ticket_id":"t1","state":"active"}]}`), nil
		case strings.HasSuffix(req.URL, "/anthropic/v1/messages"):
			runs++
			if req.Headers.Get("X-Off-Peak-Ticket-Id") != "t1" {
				t.Fatalf("run used wrong ticket: %q", req.Headers.Get("X-Off-Peak-Ticket-Id"))
			}
			return jsonHTTP(200, `{"id":"msg"}`), nil
		case strings.HasSuffix(req.URL, "/ticket/t1/settle"):
			return jsonHTTP(200, ``), nil
		default:
			return pluginapi.HTTPResponse{}, errors.New("unexpected request")
		}
	}
	_, transitions, err := e.Execute(context.Background(), nil, "", do, func(time.Duration) error { return nil })
	if err != nil || runs != 1 || strings.Join(stateStrings(transitions), ",") != "pending,running,succeeded" {
		t.Fatalf("err=%v runs=%d transitions=%v", err, runs, transitions)
	}
}

func TestOffPeakRejectsStatusWithoutMatchingTicket(t *testing.T) {
	e := mustOffPeakExecution(t)
	var runs, settles int
	do := func(_ context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/ticket/availability"):
			return jsonHTTP(200, `{"can_take_number":true}`), nil
		case strings.HasSuffix(req.URL, "/ticket"):
			return jsonHTTP(200, `{"ticket_id":"t1","state":"queued"}`), nil
		case strings.HasSuffix(req.URL, "/ticket/status"):
			return jsonHTTP(200, `{"tickets":[{"ticket_id":"other","state":"active"}]}`), nil
		case strings.HasSuffix(req.URL, "/anthropic/v1/messages"):
			runs++
			return jsonHTTP(200, `{"id":"msg"}`), nil
		case strings.HasSuffix(req.URL, "/ticket/t1/settle"):
			settles++
			return jsonHTTP(200, ``), nil
		default:
			return pluginapi.HTTPResponse{}, errors.New("unexpected request")
		}
	}
	_, transitions, err := e.Execute(context.Background(), nil, "", do, func(time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "malformed status") || runs != 0 || settles != 1 || transitions[len(transitions)-1].State != offPeakFailed {
		t.Fatalf("err=%v runs=%d settles=%d transitions=%v", err, runs, settles, transitions)
	}
}

func TestOffPeakRunUsesRemainingTotalContext(t *testing.T) {
	e := mustOffPeakExecution(t)
	e.Config.Timeout = time.Minute
	e.Config.ControlTimeout = 10 * time.Millisecond
	step := 0
	do := func(ctx context.Context, _ pluginapi.HostHTTPClient, _ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		step++
		if strings.HasSuffix(req.URL, "/anthropic/v1/messages") {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < 30*time.Second {
				t.Fatal("run used control timeout instead of total remaining context")
			}
		}
		switch step {
		case 1:
			return jsonHTTP(200, `{"can_take_number":true}`), nil
		case 2:
			return jsonHTTP(200, `{"ticket_id":"t1","state":"active"}`), nil
		case 3:
			return jsonHTTP(200, `{"id":"msg"}`), nil
		case 4:
			return jsonHTTP(200, ``), nil
		default:
			return pluginapi.HTTPResponse{}, errors.New("unexpected call")
		}
	}
	if _, _, err := e.Execute(context.Background(), nil, "", do, func(time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestOffPeakIdentityAndPerAccountConcurrency(t *testing.T) {
	storage := authStorage{ZCodeJWTToken: "raw-jwt", APIKey: "raw-key"}
	if got := offPeakIdentity("", storage); strings.Contains(got, "raw-") || !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("unsafe identity %q", got)
	}
	if got := offPeakIdentity("account-1", storage); got != "auth:account-1" {
		t.Fatalf("identity=%q", got)
	}
	limiter := newOffPeakConcurrencyLimiter(4)
	release, err := limiter.acquire(context.Background(), "same")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err = limiter.acquire(ctx, "same"); err == nil || !strings.Contains(err.Error(), "account") {
		t.Fatalf("err=%v", err)
	}
	otherRelease, err := limiter.acquire(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	otherRelease()
}

func mustOffPeakExecution(t *testing.T) *offPeakExecution {
	t.Helper()
	cfg := defaultOffPeakConfig()
	cfg.Enabled = true
	cfg.AllowedModels = map[string]bool{"glm-4.5-flash": true}
	cfg.PollInterval = 0
	e, err := newOffPeakExecution(cfg, authStorage{Provider: "zai", ZCodeJWTToken: "jwt", APIKey: "key"}, "glm-4.5-flash", []byte(`{"model":"glm-4.5-flash","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func jsonHTTP(status int, body string) pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)}
}
func stateStrings(v []offPeakTransition) []string {
	out := make([]string, len(v))
	for i, x := range v {
		out[i] = string(x.State)
	}
	return out
}

var _ = json.Valid
