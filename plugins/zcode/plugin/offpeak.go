package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const offPeakOrigin = "https://zcode.z.ai/api/v1/off-peak"

var offPeakLimiter = newOffPeakConcurrencyLimiter(4)

type offPeakAccountSlot struct {
	ch   chan struct{}
	refs int
}

type offPeakConcurrencyLimiter struct {
	global   chan struct{}
	mu       sync.Mutex
	accounts map[string]*offPeakAccountSlot
}

func newOffPeakConcurrencyLimiter(global int) *offPeakConcurrencyLimiter {
	return &offPeakConcurrencyLimiter{global: make(chan struct{}, global), accounts: make(map[string]*offPeakAccountSlot)}
}

func (l *offPeakConcurrencyLimiter) acquire(ctx context.Context, identity string) (func(), error) {
	select {
	case l.global <- struct{}{}:
	case <-ctx.Done():
		return nil, errors.New("off-peak global concurrency limit reached")
	}
	l.mu.Lock()
	slot := l.accounts[identity]
	if slot == nil {
		slot = &offPeakAccountSlot{ch: make(chan struct{}, 1)}
		l.accounts[identity] = slot
	}
	slot.refs++
	l.mu.Unlock()
	select {
	case slot.ch <- struct{}{}:
	case <-ctx.Done():
		l.dropReference(identity, slot)
		<-l.global
		return nil, errors.New("off-peak account concurrency limit reached")
	}
	return func() {
		<-slot.ch
		l.dropReference(identity, slot)
		<-l.global
	}, nil
}

func (l *offPeakConcurrencyLimiter) dropReference(identity string, slot *offPeakAccountSlot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot.refs--
	if slot.refs == 0 {
		delete(l.accounts, identity)
	}
}

func offPeakIdentity(authID string, storage authStorage) string {
	if id := strings.TrimSpace(authID); id != "" {
		return "auth:" + id
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(storage.ZCodeJWTToken) + "\x00" + strings.TrimSpace(storage.APIKey)))
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func configuredOffPeakExecution(storageJSON, payload []byte, authID string) (*offPeakExecution, bool, error) {
	model, selected := offPeakNativeModel(payload)
	if !selected {
		return nil, false, nil
	}
	storage, err := parseAuthStorage(storageJSON)
	if err != nil {
		return nil, true, err
	}
	cfg := defaultOffPeakConfig()
	cfg.Enabled = currentRouteConfig().OffPeakEnabled
	cfg.AllowedModels = map[string]bool{"glm-4.5-flash": true}
	var body map[string]any
	if json.Unmarshal(payload, &body) != nil {
		return nil, true, errors.New("invalid off-peak payload")
	}
	body["model"] = model
	body["stream"] = true
	nativePayload, err := json.Marshal(body)
	if err != nil {
		return nil, true, errors.New("invalid off-peak payload")
	}
	execution, err := newOffPeakExecution(cfg, *storage, model, nativePayload, authID)
	return execution, true, err
}

func executeConfiguredOffPeak(ctx context.Context, execution *offPeakExecution, client pluginapi.HostHTTPClient, callback string, sleep func(time.Duration) error) ([]byte, error) {
	release, err := offPeakLimiter.acquire(ctx, execution.Identity)
	if err != nil {
		return nil, err
	}
	defer release()
	result, _, err := execution.Execute(ctx, client, callback, executeHTTP, sleep)
	return result, err
}

var (
	errOffPeakDisabled              = errors.New("off-peak is disabled")
	errOffPeakPlatformUnsupported   = errors.New("off-peak platform unsupported")
	errOffPeakCredentialUnsupported = errors.New("off-peak credential unsupported")
	errOffPeakModelUnsupported      = errors.New("off-peak model unsupported")
	errOffPeakPollLimit             = errors.New("off-peak polling limit reached")
	errOffPeakExpired               = errors.New("off-peak ticket expired")
)

type offPeakState string

const (
	offPeakPending   offPeakState = "pending"
	offPeakRunning   offPeakState = "running"
	offPeakSucceeded offPeakState = "succeeded"
	offPeakFailed    offPeakState = "failed"
	offPeakExpired   offPeakState = "expired"
)

type offPeakConfig struct {
	Enabled        bool
	AllowedModels  map[string]bool
	Timeout        time.Duration
	ControlTimeout time.Duration
	PollInterval   time.Duration
	MaxPolls       int
	MaxBodyBytes   int
}

func defaultOffPeakConfig() offPeakConfig {
	return offPeakConfig{Timeout: 2 * time.Minute, ControlTimeout: 15 * time.Second, PollInterval: 2 * time.Second, MaxPolls: 60, MaxBodyBytes: 16 << 20}
}

type offPeakExecution struct {
	Config   offPeakConfig
	Storage  authStorage
	Identity string
	Model    string
	Payload  []byte
}
type offPeakTransition struct {
	State    offPeakState
	TicketID string
}
type offPeakHTTPDo func(context.Context, pluginapi.HostHTTPClient, string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)

func newOffPeakExecution(cfg offPeakConfig, storage authStorage, model string, payload []byte, authID ...string) (*offPeakExecution, error) {
	if !cfg.Enabled {
		return nil, errOffPeakDisabled
	}
	if !strings.EqualFold(strings.TrimSpace(storage.Provider), "zai") {
		return nil, errOffPeakPlatformUnsupported
	}
	if strings.TrimSpace(storage.ZCodeJWTToken) == "" || strings.TrimSpace(storage.APIKey) == "" {
		return nil, errOffPeakCredentialUnsupported
	}
	if !cfg.AllowedModels[strings.TrimSpace(model)] {
		return nil, errOffPeakModelUnsupported
	}
	if cfg.Timeout <= 0 || cfg.ControlTimeout <= 0 || cfg.MaxPolls <= 0 || cfg.MaxBodyBytes <= 0 {
		return nil, errors.New("invalid off-peak limits")
	}
	id := ""
	if len(authID) != 0 {
		id = authID[0]
	}
	return &offPeakExecution{Config: cfg, Storage: storage, Identity: offPeakIdentity(id, storage), Model: model, Payload: append([]byte(nil), payload...)}, nil
}

func (e *offPeakExecution) headers(body bool) http.Header {
	h := http.Header{"Authorization": []string{"Bearer " + strings.TrimSpace(e.Storage.ZCodeJWTToken)}, "X-Coding-Plan-Api-Key": []string{strings.TrimSpace(e.Storage.APIKey)}}
	if body {
		h.Set("Content-Type", "application/json")
	}
	return h
}
func (e *offPeakExecution) buildPreflightRequest() pluginapi.HTTPRequest {
	return pluginapi.HTTPRequest{Method: http.MethodGet, URL: offPeakOrigin + "/ticket/availability", Headers: e.headers(false)}
}
func (e *offPeakExecution) buildTakeRequest(task string) pluginapi.HTTPRequest {
	b, _ := json.Marshal(map[string]string{"task_id": task})
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: offPeakOrigin + "/ticket", Headers: e.headers(true), Body: b}
}
func (e *offPeakExecution) buildStatusRequest(ticket string) pluginapi.HTTPRequest {
	b, _ := json.Marshal(map[string][]string{"ticket_ids": {ticket}})
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: offPeakOrigin + "/ticket/status", Headers: e.headers(true), Body: b}
}
func (e *offPeakExecution) buildRunRequest(ticket string) pluginapi.HTTPRequest {
	h := e.headers(true)
	h.Set("X-Off-Peak-Ticket-Id", ticket)
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: offPeakOrigin + "/anthropic/v1/messages", Headers: h, Body: e.Payload}
}
func (e *offPeakExecution) buildSettleRequest(ticket string) pluginapi.HTTPRequest {
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: offPeakOrigin + "/ticket/" + url.PathEscape(ticket) + "/settle", Headers: e.headers(false)}
}

func offPeakNativeModel(payload []byte) (string, bool) {
	var body struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &body) != nil {
		return "", false
	}
	const prefix = "zcode-offpeak-"
	if !strings.HasPrefix(body.Model, prefix) || len(body.Model) == len(prefix) {
		return "", false
	}
	return strings.TrimPrefix(body.Model, prefix), true
}
func offPeakKeepaliveFrame() []byte { return []byte(": keepalive\n\n") }
func offPeakSettleSuccessful(status int) bool {
	return status >= 200 && status < 300 || status == http.StatusNotFound || status == http.StatusConflict
}

func (e *offPeakExecution) Execute(parent context.Context, client pluginapi.HostHTTPClient, callback string, do offPeakHTTPDo, sleep func(time.Duration) error) (result []byte, transitions []offPeakTransition, err error) {
	ctx, cancel := context.WithTimeout(parent, e.Config.Timeout)
	defer cancel()
	fail := func(cause error) ([]byte, []offPeakTransition, error) {
		state := offPeakFailed
		if errors.Is(cause, errOffPeakExpired) {
			state = offPeakExpired
		}
		transitions = append(transitions, offPeakTransition{State: state})
		return nil, transitions, cause
	}
	validate := func(resp pluginapi.HTTPResponse, callErr error) (pluginapi.HTTPResponse, error) {
		if callErr != nil {
			return resp, errors.New("off-peak network failure")
		}
		if len(resp.Body) > e.Config.MaxBodyBytes {
			return resp, errors.New("off-peak response exceeds body limit")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return resp, fmt.Errorf("off-peak upstream HTTP %d", resp.StatusCode)
		}
		return resp, nil
	}
	control := func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		callCtx, stop := context.WithTimeout(ctx, e.Config.ControlTimeout)
		defer stop()
		return validate(do(callCtx, client, callback, req))
	}
	run := func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		// Run gets the total remaining execution context. It is never retried.
		return validate(do(ctx, client, callback, req))
	}
	settle := func(ticket string) error {
		// Experimental limitation: exactly one detached settle attempt, with no retry.
		callCtx, stop := context.WithTimeout(context.Background(), e.Config.ControlTimeout)
		defer stop()
		resp, settleErr := do(callCtx, client, callback, e.buildSettleRequest(ticket))
		if settleErr != nil {
			return errors.New("off-peak settle failure (single experimental attempt)")
		}
		if len(resp.Body) > e.Config.MaxBodyBytes {
			return errors.New("off-peak settle response exceeds body limit")
		}
		if !offPeakSettleSuccessful(resp.StatusCode) {
			return fmt.Errorf("off-peak settle HTTP %d (single experimental attempt)", resp.StatusCode)
		}
		return nil
	}

	pre, callErr := control(e.buildPreflightRequest())
	if callErr != nil {
		return fail(callErr)
	}
	var avail struct {
		Can bool `json:"can_take_number"`
	}
	if callErr = parseOffPeakData(pre.Body, &avail); callErr != nil || !avail.Can {
		if callErr == nil {
			callErr = errors.New("off-peak unavailable")
		}
		return fail(callErr)
	}

	for ticketAttempt := 0; ticketAttempt < 2; ticketAttempt++ {
		taken, takeErr := control(e.buildTakeRequest(fmt.Sprintf("cpa-%d", time.Now().UnixNano())))
		if takeErr != nil {
			return fail(takeErr)
		}
		var ticket struct {
			ID    string `json:"ticket_id"`
			State string `json:"state"`
		}
		if takeErr = parseOffPeakData(taken.Body, &ticket); takeErr != nil || ticket.ID == "" {
			if takeErr == nil {
				takeErr = errors.New("off-peak malformed ticket")
			}
			return fail(takeErr)
		}
		transitions = append(transitions, offPeakTransition{State: offPeakPending, TicketID: ticket.ID})
		state := ticket.State
		expired := false
		for polls := 0; state != "ready" && state != "active"; polls++ {
			if state == "expired" || state == "not_found" || state == "settled" {
				expired = true
				break
			}
			if polls >= e.Config.MaxPolls {
				if settleErr := settle(ticket.ID); settleErr != nil {
					return fail(settleErr)
				}
				return fail(errOffPeakPollLimit)
			}
			if sleepErr := sleep(e.Config.PollInterval); sleepErr != nil {
				if settleErr := settle(ticket.ID); settleErr != nil {
					return fail(settleErr)
				}
				return fail(errors.New("off-peak wait interrupted"))
			}
			status, statusErr := control(e.buildStatusRequest(ticket.ID))
			if statusErr != nil {
				if settleErr := settle(ticket.ID); settleErr != nil {
					return fail(settleErr)
				}
				return fail(statusErr)
			}
			var batch struct {
				Tickets []struct {
					ID    string `json:"ticket_id"`
					State string `json:"state"`
				} `json:"tickets"`
			}
			if statusErr = parseOffPeakData(status.Body, &batch); statusErr != nil || len(batch.Tickets) == 0 {
				if statusErr == nil {
					statusErr = errors.New("off-peak malformed status")
				}
				if settleErr := settle(ticket.ID); settleErr != nil {
					return fail(settleErr)
				}
				return fail(statusErr)
			}
			state = batch.Tickets[0].State
		}
		if expired {
			if settleErr := settle(ticket.ID); settleErr != nil {
				return fail(settleErr)
			}
			if ticketAttempt == 0 {
				continue
			}
			return fail(errOffPeakExpired)
		}

		transitions = append(transitions, offPeakTransition{State: offPeakRunning, TicketID: ticket.ID})
		runResp, runErr := run(e.buildRunRequest(ticket.ID))
		// Run is never replayed, regardless of its outcome.
		settleErr := settle(ticket.ID)
		if runErr != nil {
			if settleErr != nil {
				return fail(fmt.Errorf("%v; %w", runErr, settleErr))
			}
			return fail(runErr)
		}
		if settleErr != nil {
			return fail(settleErr)
		}
		transitions = append(transitions, offPeakTransition{State: offPeakSucceeded, TicketID: ticket.ID})
		return append([]byte(nil), runResp.Body...), transitions, nil
	}
	return fail(errOffPeakExpired)
}

func parseOffPeakData(body []byte, out any) error {
	var env struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &env) == nil && env.Code != nil {
		if *env.Code != 0 {
			return errors.New("off-peak business failure")
		}
		body = env.Data
	}
	if len(body) == 0 || json.Unmarshal(body, out) != nil {
		return errors.New("off-peak malformed response")
	}
	return nil
}

var secretPattern = regexp.MustCompile(`(?i)(bearer\s+|api[-_ ]?key\s*[=:]?\s*)[^\s,;]+`)

func sanitizeOffPeakError(err error) string {
	if err == nil {
		return ""
	}
	s := secretPattern.ReplaceAllString(err.Error(), "$1[redacted]")
	if len(s) > 240 {
		s = s[:240]
	}
	return s
}
