package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// captchaSolverClient calls the standalone Node happy-dom captcha solver
// service (solver-server on the HK VPS) over loopback HTTP. The service runs
// the upstream zcode-api happy-dom solver and returns a freshly-solved Aliyun
// verify param. This Go side never runs the solver itself; it only fetches
// tokens and warms the in-memory pool consumed by the start-plan auto-claimer.
//
// Security: the solver listens on 127.0.0.1 only (loopback trust). The verify
// param is never logged, persisted, or echoed by this client.

const (
	captchaSolverDefaultURL   = "http://127.0.0.1:8777/solve"
	captchaSolverDefaultMin   = 2 // keep at least this many ready tokens in the pool
	captchaSolverRefillEvery  = 3 * time.Second
	captchaSolverHTTPTimeout  = 60 * time.Second
	captchaSolverMaxBody      = 64 * 1024
	captchaSolverDefaultScene = "11xygtvd"
	captchaSolverDefaultReg   = "sgp"
	captchaSolverDefaultPref  = "no8xfe"
)

type captchaSolverConfig struct {
	Enabled bool
	URL     string
	Min     int
	Scene   string
	Region  string
	Prefix  string
}

func captchaSolverConfigFromRoute(cfg routeConfig) captchaSolverConfig {
	out := captchaSolverConfig{
		Enabled: cfg.CaptchaSolverEnabled,
		URL:     cfg.CaptchaSolverURL,
		Min:     cfg.CaptchaSolverMin,
		Scene:   cfg.CaptchaSolverScene,
		Region:  cfg.CaptchaSolverRegion,
		Prefix:  cfg.CaptchaSolverPrefix,
	}
	if strings.TrimSpace(out.URL) == "" {
		out.URL = captchaSolverDefaultURL
	}
	if out.Min <= 0 {
		out.Min = captchaSolverDefaultMin
	}
	if strings.TrimSpace(out.Scene) == "" {
		out.Scene = captchaSolverDefaultScene
	}
	if strings.TrimSpace(out.Region) == "" {
		out.Region = captchaSolverDefaultReg
	}
	if strings.TrimSpace(out.Prefix) == "" {
		out.Prefix = captchaSolverDefaultPref
	}
	return out
}

type captchaSolverWarm struct {
	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	stats   captchaSolverStats
}

type captchaSolverStats struct {
	Solves     int
	Failures   int
	LastError  string
	LastOKAt   time.Time
	LastFailAt time.Time
}

var captchaSolverWarmSvc = &captchaSolverWarm{}

func ensureCaptchaSolver() {
	cfg := captchaSolverConfigFromRoute(currentRouteConfig())
	if cfg.Enabled {
		captchaSolverWarmSvc.start()
	} else {
		captchaSolverWarmSvc.stop()
	}
}

func (w *captchaSolverWarm) start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	if w.doneCh != nil {
		done := w.doneCh
		w.mu.Unlock()
		<-done
		w.mu.Lock()
	}
	w.running = true
	w.stopCh = make(chan struct{})
	w.doneCh = make(chan struct{})
	stopCh, doneCh := w.stopCh, w.doneCh
	w.mu.Unlock()
	go w.run(stopCh, doneCh)
}

func (w *captchaSolverWarm) stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	stopCh, doneCh := w.stopCh, w.doneCh
	close(stopCh)
	w.mu.Unlock()
	<-doneCh
}

func (w *captchaSolverWarm) run(stopCh, doneCh chan struct{}) {
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(doneCh)
	}()
	ticker := time.NewTicker(captchaSolverRefillEvery)
	defer ticker.Stop()
	for {
		w.refill()
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}
	}
}

// refill tops the pool up to the configured minimum by solving on demand.
func (w *captchaSolverWarm) refill() {
	cfg := captchaSolverConfigFromRoute(currentRouteConfig())
	ready := claimCaptchaPool.stats(time.Now())
	for ready < cfg.Min {
		param, region, err := w.solve(cfg)
		if err != nil {
			w.mu.Lock()
			w.stats.Failures++
			w.stats.LastError = err.Error()
			w.stats.LastFailAt = time.Now()
			w.mu.Unlock()
			return
		}
		if err := claimCaptchaPool.add(param, region, time.Now()); err != nil {
			w.mu.Lock()
			w.stats.Failures++
			w.stats.LastError = err.Error()
			w.stats.LastFailAt = time.Now()
			w.mu.Unlock()
			return
		}
		w.mu.Lock()
		w.stats.Solves++
		w.stats.LastOKAt = time.Now()
		w.mu.Unlock()
		ready++
	}
}

func (w *captchaSolverWarm) solve(cfg captchaSolverConfig) (param, region string, err error) {
	payload := map[string]any{
		"scene":     cfg.Scene,
		"region":    cfg.Region,
		"prefix":    cfg.Prefix,
		"timeoutMs": int(captchaSolverHTTPTimeout / time.Millisecond),
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), captchaSolverHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("captcha solver request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: captchaSolverHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("captcha solver unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, captchaSolverMaxBody))
	if err != nil {
		return "", "", fmt.Errorf("captcha solver read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("captcha solver http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Ok          bool   `json:"ok"`
		VerifyParam string `json:"verifyParam"`
		Region      string `json:"region"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("captcha solver decode: %w", err)
	}
	if !out.Ok || strings.TrimSpace(out.VerifyParam) == "" {
		return "", "", fmt.Errorf("captcha solver returned no token: %s", firstString(out.Error, "empty"))
	}
	// Security: only the length + success are observable; never the token.
	region = out.Region
	if strings.TrimSpace(region) == "" {
		region = cfg.Region
	}
	return out.VerifyParam, region, nil
}

func captchaSolverStatus() map[string]any {
	cfg := captchaSolverConfigFromRoute(currentRouteConfig())
	captchaSolverWarmSvc.mu.Lock()
	stats := captchaSolverWarmSvc.stats
	running := captchaSolverWarmSvc.running
	captchaSolverWarmSvc.mu.Unlock()
	return map[string]any{
		"enabled":    cfg.Enabled,
		"running":    running,
		"url":        cfg.URL,
		"min":        cfg.Min,
		"solves":     stats.Solves,
		"failures":   stats.Failures,
		"last_error": stats.LastError,
		"last_ok":    stats.LastOKAt,
		"last_fail":  stats.LastFailAt,
		"pool_ready": claimCaptchaPool.stats(time.Now()),
	}
}
