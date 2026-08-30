package main

import (
	"strings"
	"testing"
	"time"
)

func resetCaptchaPoolForTest(t *testing.T) {
	t.Helper()
	oldMax, oldTTL := claimCaptchaPool.max, claimCaptchaPool.ttl
	claimCaptchaPool.clear()
	t.Cleanup(func() {
		claimCaptchaPool.clear()
		claimCaptchaPool.max, claimCaptchaPool.ttl = oldMax, oldTTL
	})
}

func TestCaptchaPoolAddTakeSingleUse(t *testing.T) {
	resetCaptchaPoolForTest(t)
	now := time.Now()
	if err := claimCaptchaPool.add("verify-param-1", "sgp", now); err != nil {
		t.Fatal(err)
	}
	if err := claimCaptchaPool.add("verify-param-2", "", now); err != nil {
		t.Fatal(err)
	}
	if got := claimCaptchaPool.stats(now); got != 2 {
		t.Fatalf("ready=%d", got)
	}
	param, region, ok := claimCaptchaPool.take(now)
	if !ok || param != "verify-param-1" || region != "sgp" {
		t.Fatalf("take=%q/%q ok=%v", param, region, ok)
	}
	// Single-use: the token is gone.
	if got := claimCaptchaPool.stats(now); got != 1 {
		t.Fatalf("ready after take=%d", got)
	}
}

func TestCaptchaPoolRejectsInvalidAndBounds(t *testing.T) {
	resetCaptchaPoolForTest(t)
	now := time.Now()
	if err := claimCaptchaPool.add("short", "", now); err == nil {
		t.Fatal("short param accepted")
	}
	if err := claimCaptchaPool.add(strings.Repeat("a", 5000), "", now); err == nil {
		t.Fatal("oversized param accepted")
	}
	if err := claimCaptchaPool.add("verify-param", strings.Repeat("r", 65), now); err == nil {
		t.Fatal("oversized region accepted")
	}
	claimCaptchaPool.max = 2
	if err := claimCaptchaPool.add("verify-param-1", "", now); err != nil {
		t.Fatal(err)
	}
	if err := claimCaptchaPool.add("verify-param-2", "", now); err != nil {
		t.Fatal(err)
	}
	if err := claimCaptchaPool.add("verify-param-3", "", now); err == nil {
		t.Fatal("pool over its configured max accepted a token")
	}
}

func TestCaptchaPoolExpiresTokens(t *testing.T) {
	resetCaptchaPoolForTest(t)
	now := time.Now()
	claimCaptchaPool.ttl = time.Minute
	if err := claimCaptchaPool.add("verify-param-old", "", now); err != nil {
		t.Fatal(err)
	}
	if err := claimCaptchaPool.add("verify-param-fresh", "", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// At t+90s the first token is expired; only the fresh one survives.
	ready := claimCaptchaPool.stats(now.Add(90 * time.Second))
	if ready != 1 {
		t.Fatalf("ready=%d want 1", ready)
	}
	param, _, ok := claimCaptchaPool.take(now.Add(90 * time.Second))
	if !ok || param != "verify-param-fresh" {
		t.Fatalf("took expired or wrong token: %q ok=%v", param, ok)
	}
}

func TestCaptchaPoolConfigureFallsBackToDefaults(t *testing.T) {
	resetCaptchaPoolForTest(t)
	claimCaptchaPool.configure(0, 0)
	if claimCaptchaPool.max != captchaPoolDefaultMax || claimCaptchaPool.ttl != captchaPoolDefaultTTL {
		t.Fatalf("configure(0,0) -> max=%d ttl=%s", claimCaptchaPool.max, claimCaptchaPool.ttl)
	}
	claimCaptchaPool.configure(7, 90*time.Second)
	if claimCaptchaPool.max != 7 || claimCaptchaPool.ttl != 90*time.Second {
		t.Fatalf("configure(7,90s) -> max=%d ttl=%s", claimCaptchaPool.max, claimCaptchaPool.ttl)
	}
}

func TestCaptchaPoolErrorsNeverLeakTokens(t *testing.T) {
	resetCaptchaPoolForTest(t)
	now := time.Now()
	if err := claimCaptchaPool.add("super-secret-token", "", now); err != nil {
		t.Fatal(err)
	}
	// A full-pool rejection must not echo the stored token.
	claimCaptchaPool.max = 1
	err := claimCaptchaPool.add("another-secret-token", "", now)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("pool error leaked a token: %q", err)
	}
}
