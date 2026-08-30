package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func resetCaptchaSolverForTest(t *testing.T) {
	t.Helper()
	captchaSolverWarmSvc.stop()
	captchaSolverWarmSvc.mu.Lock()
	captchaSolverWarmSvc.stats = captchaSolverStats{}
	captchaSolverWarmSvc.mu.Unlock()
	claimCaptchaPool.clear()
	claimCaptchaPool.configure(0, 0)
	routes.Lock()
	routes.config.CaptchaSolverEnabled = false
	routes.config.CaptchaSolverURL = ""
	routes.config.CaptchaSolverMin = 0
	routes.config.CaptchaSolverScene = ""
	routes.config.CaptchaSolverRegion = ""
	routes.config.CaptchaSolverPrefix = ""
	routes.Unlock()
	t.Cleanup(func() {
		captchaSolverWarmSvc.stop()
		captchaSolverWarmSvc.mu.Lock()
		captchaSolverWarmSvc.stats = captchaSolverStats{}
		captchaSolverWarmSvc.mu.Unlock()
		claimCaptchaPool.clear()
		claimCaptchaPool.configure(0, 0)
	})
}

func TestCaptchaSolverConfigFromRouteDefaults(t *testing.T) {
	resetCaptchaSolverForTest(t)
	cfg := captchaSolverConfigFromRoute(routeConfig{})
	if cfg.Enabled || cfg.URL != captchaSolverDefaultURL || cfg.Min != captchaSolverDefaultMin ||
		cfg.Scene != captchaSolverDefaultScene || cfg.Region != captchaSolverDefaultReg || cfg.Prefix != captchaSolverDefaultPref {
		t.Fatalf("defaults cfg=%+v", cfg)
	}
}

func TestCaptchaSolverWarmFetchesAndRefillsPool(t *testing.T) {
	resetCaptchaSolverForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"ok":true,"verifyParam":"eyJjZXJ0aWZ5SWQiOiJ4eHh4Iiwic2NlbmVJZCI6IjExeHlndHZkIiwiaXNTaWduIjp0cnVlLCJzZWN1cml0eVRva2VuIjoiNjZvbzdlNzJuQTYxdVZMaVpWS2lMWXFGMW05ck9ubzN2RUlQSkthTDdLTHhDSnFiMVVCd1JwbDRwN0VjRlRnZFEyVVV5eW5TL2l4eFVWQzlkVVlZbWlvWGsxYWdoSGJQdzVMaU9IeU1uYnBVU01Ya3BlQmFWZWlaRVI1N2VpVmEifQ==","region":"sgp","elapsedMs":10}`)
		w.Header().Set("content-type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()
	routes.Lock()
	routes.config.CaptchaSolverEnabled = true
	routes.config.CaptchaSolverURL = srv.URL
	routes.config.CaptchaSolverMin = 2
	routes.Unlock()

	captchaSolverWarmSvc.refill()
	if got := claimCaptchaPool.stats(time.Now()); got != 2 {
		t.Fatalf("pool ready=%d want 2", got)
	}
	param, region, ok := claimCaptchaPool.take(time.Now())
	if !ok || len(param) < 50 || !strings.HasPrefix(param, "eyJ") || region != "sgp" {
		t.Fatalf("take=%q/%q ok=%v", param, region, ok)
	}
	captchaSolverWarmSvc.mu.Lock()
	stats := captchaSolverWarmSvc.stats
	captchaSolverWarmSvc.mu.Unlock()
	if stats.Solves != 2 || stats.Failures != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestCaptchaSolverWarmStopsOnError(t *testing.T) {
	resetCaptchaSolverForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"ok":false,"error":"solve failed: timeout"}`, http.StatusBadGateway)
	}))
	defer srv.Close()
	routes.Lock()
	routes.config.CaptchaSolverEnabled = true
	routes.config.CaptchaSolverURL = srv.URL
	routes.config.CaptchaSolverMin = 5
	routes.Unlock()

	captchaSolverWarmSvc.refill()
	if got := claimCaptchaPool.stats(time.Now()); got != 0 {
		t.Fatalf("pool ready=%d want 0 (solve failed)", got)
	}
	captchaSolverWarmSvc.mu.Lock()
	stats := captchaSolverWarmSvc.stats
	captchaSolverWarmSvc.mu.Unlock()
	if stats.Failures == 0 || !strings.Contains(stats.LastError, "solver") {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestCaptchaSolverLifecycleViaApplyRouteConfig(t *testing.T) {
	resetCaptchaSolverForTest(t)
	// Disabled by default: no warm goroutine.
	applyRouteConfig(lifecycleConfig(""))
	captchaSolverWarmSvc.mu.Lock()
	running := captchaSolverWarmSvc.running
	captchaSolverWarmSvc.mu.Unlock()
	if running {
		t.Fatal("solver warm running despite not enabled")
	}
	// Enable via YAML (URL points at a dead port; running flag is what we assert).
	applyRouteConfig(lifecycleConfig("captcha_solver_enabled: true\ncaptcha_solver_url: http://127.0.0.1:1/solve\ncaptcha_solver_min: 3\n"))
	captchaSolverWarmSvc.mu.Lock()
	running = captchaSolverWarmSvc.running
	captchaSolverWarmSvc.mu.Unlock()
	if !running {
		t.Fatal("solver warm did not start when enabled")
	}
	applyRouteConfig(lifecycleConfig(""))
	captchaSolverWarmSvc.mu.Lock()
	running = captchaSolverWarmSvc.running
	captchaSolverWarmSvc.mu.Unlock()
	if running {
		t.Fatal("solver warm still running after disable")
	}
}

func TestCaptchaSolverNeverLogsToken(t *testing.T) {
	resetCaptchaSolverForTest(t)
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("x-test")
		w.Write([]byte(`{"ok":true,"verifyParam":"eyJjZXJ0aWZ5SWQiOiJ4eHh4Iiwic2NlbmVJZCI6IjExeHlndHZkIiwiaXNTaWduIjp0cnVlLCJzZWN1cml0eVRva2VuIjoiNjZvbzdlNzJuQTYxdVZMaVpWS2lMWXFGMW05ck9ubzN2RUlQSkthTDdLTHhDSnFiMVVCd1JwbDRwN0VjRlRnZFEyVVV5eW5TL2l4eFVWQzlkVVlZbWlvWGsxYWdoSGJQdzVMaU9IeU1uYnBVU01Ya3BlQmFWZWlaRVI1N2VpVmEifQ==","region":"sgp"}`))
	}))
	defer srv.Close()
	routes.Lock()
	routes.config.CaptchaSolverEnabled = true
	routes.config.CaptchaSolverURL = srv.URL
	routes.config.CaptchaSolverMin = 1
	routes.Unlock()
	captchaSolverWarmSvc.refill()
	_ = captured
	// The verify param must never appear in stats / last error / panel text.
	captchaSolverWarmSvc.mu.Lock()
	stats := captchaSolverWarmSvc.stats
	captchaSolverWarmSvc.mu.Unlock()
	if strings.Contains(stats.LastError, "eyJ") {
		t.Fatal("stats leaked token-like material")
	}
	status := captchaSolverStatus()
	for _, v := range status {
		s, _ := v.(string)
		if strings.Contains(s, "eyJ") {
			t.Fatal("status leaked token-like material")
		}
	}
}
