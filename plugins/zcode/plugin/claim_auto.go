package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Auto-claim scheduler for the start-plan ("weekend/trial plan") billing
// campaign. Protocol evidence from the zcode-api (zcode-proxy) repository
// (2026-08-29):
//   - GET  {origin}/api/v1/zcode-plan/billing/preview  (identity headers + X-Device-Mid)
//   - POST {origin}/api/v1/zcode-plan/billing/claim     body {plan_id}, JWT + captcha
//   - Biz codes: 1001 not_found, 1002 unavailable, 1003 already_claimed,
//     1004 ineligible, 1005 quota_exhausted, 3001 invalid_request,
//     3007 captcha, 401 login_required.
//
// The scheduler polls previews for JWT-capable accounts, picks the configured
// plan or the highest-priority one, consumes one token from the user-supplied
// captcha pool, and claims. It never generates, forges, or auto-solves
// captchas. Backoff semantics mirror the repository scheduler: success holds
// until the plan's ends_at, already_claimed/quota_exhausted hold until the
// server-provided next window, other failures use the cooldown, and
// login_required stops the scheduler (a config toggle re-enables it).

const (
	claimAutoDefaultPollInterval = 5 * time.Minute
	claimAutoDefaultCooldown     = 10 * time.Minute
	claimAutoMaxHold             = 24 * time.Hour
)

type claimAutoConfig struct {
	Enabled      bool
	PlanID       string
	PollInterval time.Duration
	Cooldown     time.Duration
}

func claimAutoConfigFromRoute(cfg routeConfig) claimAutoConfig {
	out := claimAutoConfig{Enabled: cfg.StartPlanAutoClaim, PlanID: cfg.ClaimPlanID, PollInterval: cfg.ClaimPollInterval, Cooldown: cfg.ClaimCooldown}
	if out.PollInterval <= 0 {
		out.PollInterval = claimAutoDefaultPollInterval
	}
	if out.Cooldown <= 0 {
		out.Cooldown = claimAutoDefaultCooldown
	}
	return out
}

type claimAutoTickResult struct {
	Action    string // disabled | stopped | error | idle | claimed | failed | skip | no_captcha
	AuthIndex string
	PlanID    string
	Message   string
}

type claimAutoScheduler struct {
	mu        sync.Mutex
	running   bool
	stopped   bool
	stopCh    chan struct{}
	doneCh    chan struct{}
	wakeCh    chan struct{}
	holdUntil map[string]time.Time
}

var claimAuto = &claimAutoScheduler{}

// ensureAutoClaim reconciles the scheduler with the effective route config.
// Called from applyRouteConfig on every register/reconfigure.
func ensureAutoClaim() {
	if currentRouteConfig().StartPlanAutoClaim {
		claimAuto.start()
	} else {
		claimAuto.stop()
	}
}

func (s *claimAutoScheduler) start() {
	s.mu.Lock()
	if s.running {
		if s.wakeCh != nil {
			select {
			case s.wakeCh <- struct{}{}:
			default:
			}
		}
		s.mu.Unlock()
		return
	}
	if s.doneCh != nil {
		done := s.doneCh
		s.mu.Unlock()
		<-done
		s.mu.Lock()
	}
	s.running = true
	s.stopped = false
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.wakeCh = make(chan struct{}, 1)
	s.holdUntil = make(map[string]time.Time)
	stopCh, doneCh, wakeCh := s.stopCh, s.doneCh, s.wakeCh
	s.mu.Unlock()
	go s.run(stopCh, doneCh, wakeCh)
}

func (s *claimAutoScheduler) stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	stopCh, doneCh := s.stopCh, s.doneCh
	close(stopCh)
	s.mu.Unlock()
	<-doneCh
}

func (s *claimAutoScheduler) run(stopCh, doneCh, wakeCh chan struct{}) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		close(doneCh)
	}()
	for {
		if s.isStopped() {
			return
		}
		result := s.tick(time.Now())
		if result.Action == "stopped" {
			return
		}
		cfg := claimAutoConfigFromRoute(currentRouteConfig())
		if !cfg.Enabled {
			return
		}
		delay := s.nextDelay(cfg, time.Now())
		select {
		case <-stopCh:
			return
		case <-wakeCh:
		case <-time.After(delay):
		}
	}
}

func (s *claimAutoScheduler) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *claimAutoScheduler) setStopped() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}

func (s *claimAutoScheduler) setHold(authIndex string, until time.Time) {
	s.mu.Lock()
	if s.holdUntil != nil {
		s.holdUntil[authIndex] = until
	}
	s.mu.Unlock()
}

func (s *claimAutoScheduler) holdFor(authIndex string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hold, ok := s.holdUntil[authIndex]
	return hold, ok
}

func (s *claimAutoScheduler) nextDelay(cfg claimAutoConfig, now time.Time) time.Duration {
	s.mu.Lock()
	var earliest time.Time
	has := false
	for _, hold := range s.holdUntil {
		if !has || hold.Before(earliest) {
			earliest = hold
			has = true
		}
	}
	s.mu.Unlock()
	if has && earliest.After(now) {
		return earliest.Sub(now)
	}
	return cfg.PollInterval
}

func (s *claimAutoScheduler) tick(now time.Time) claimAutoTickResult {
	cfg := claimAutoConfigFromRoute(currentRouteConfig())
	if !cfg.Enabled {
		return claimAutoTickResult{Action: "disabled"}
	}
	if s.isStopped() {
		return claimAutoTickResult{Action: "stopped"}
	}
	page, err := loadZCodeStatus()
	if err != nil {
		s.setHold("", now.Add(cfg.Cooldown))
		return claimAutoTickResult{Action: "error", Message: "账号状态不可用"}
	}
	var last claimAutoTickResult
	var claimedResult claimAutoTickResult
	claimed := false
	for _, account := range page.Accounts {
		if account.AuthIndex == "" || account.State == "disabled" || account.State == "unavailable" {
			continue
		}
		authIndex := strings.TrimSpace(account.AuthIndex)
		if hold, ok := s.holdFor(authIndex); ok && now.Before(hold) {
			continue
		}
		last = s.claimAccount(cfg, authIndex, now)
		if last.Action == "stopped" {
			return last
		}
		if last.Action == "claimed" {
			claimed = true
			claimedResult = last
		}
	}
	if claimed {
		return claimedResult
	}
	if last.Action != "" && last.Action != "skip" {
		return last
	}
	return claimAutoTickResult{Action: "idle"}
}

func (s *claimAutoScheduler) claimAccount(cfg claimAutoConfig, authIndex string, now time.Time) claimAutoTickResult {
	fail := func(result, message string, planID string) claimAutoTickResult {
		return claimAutoTickResult{Action: "failed", AuthIndex: authIndex, PlanID: planID, Message: message}
	}
	storage, err := loadClaimStorage(authIndex)
	if err != nil || strings.TrimSpace(storage.ZCodeJWTToken) == "" {
		return claimAutoTickResult{Action: "skip", AuthIndex: authIndex, Message: "无 JWT"}
	}
	snapshot := refreshClaimPreviewWithStorage(authIndex, "", storage)
	if snapshot.Error != "" {
		s.setHold(authIndex, now.Add(cfg.Cooldown))
		return fail("preview_failed", snapshot.Error, "")
	}
	target := pickClaimAutoPlan(cfg.PlanID, snapshot.Preview.Plans)
	if target == nil {
		return claimAutoTickResult{Action: "skip", AuthIndex: authIndex, Message: "无可领取套餐"}
	}
	param, region, ok := claimCaptchaPool.take(now)
	if !ok {
		s.setHold(authIndex, now.Add(cfg.PollInterval))
		return claimAutoTickResult{Action: "no_captcha", AuthIndex: authIndex, PlanID: target.PlanID, Message: "验证码池为空"}
	}
	key := normalizeClaimKey(authIndex, target.PlanID)
	if !tryAcquireClaim(key) {
		return claimAutoTickResult{Action: "skip", AuthIndex: authIndex, PlanID: target.PlanID, Message: "领取进行中"}
	}
	defer releaseClaim(key)

	claimStorage := storage
	claimStorage.CaptchaVerifyParam = param
	claimStorage.CaptchaVerifyRegion = region
	request, err := buildClaimRequest(claimStorage, target.PlanID)
	if err != nil {
		s.setHold(authIndex, now.Add(cfg.Cooldown))
		appendClaimAudit(authIndex, target.PlanID, "auto_request_invalid", now)
		return fail("request_invalid", "领取请求材料无效", target.PlanID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), claimRequestTimeout)
	defer cancel()
	response, err := claimHTTPDo(ctx, nil, "", request)
	if err != nil {
		s.setHold(authIndex, now.Add(cfg.Cooldown))
		appendClaimAudit(authIndex, target.PlanID, "auto_network_error", now)
		return fail("network_error", "套餐领取网络失败", target.PlanID)
	}
	if len(response.Body) > claimMaxResponseBody {
		s.setHold(authIndex, now.Add(cfg.Cooldown))
		appendClaimAudit(authIndex, target.PlanID, "auto_response_too_large", now)
		return fail("response_too_large", "套餐领取响应过大", target.PlanID)
	}
	if response.StatusCode == http.StatusUnauthorized {
		s.setStopped()
		appendClaimAudit(authIndex, target.PlanID, "auto_login_required", now)
		return claimAutoTickResult{Action: "stopped", AuthIndex: authIndex, PlanID: target.PlanID, Message: "登录已失效，需要重新登录"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.setHold(authIndex, now.Add(cfg.Cooldown))
		appendClaimAudit(authIndex, target.PlanID, "auto_http_error", now)
		return fail("http_error", "套餐领取接口返回非 2xx", target.PlanID)
	}
	result, err := parseClaimResult(response.Body)
	if err != nil {
		s.setHold(authIndex, now.Add(cfg.Cooldown))
		appendClaimAudit(authIndex, target.PlanID, "auto_parse_error", now)
		return fail("parse_error", "套餐领取响应无法解析", target.PlanID)
	}
	if result.Category == "success" {
		hold := now.Add(cfg.PollInterval)
		if endsAt := claimFailureEndsAt(result); endsAt > 0 {
			if until := time.Unix(endsAt, 0); until.After(now) && until.Sub(now) < claimAutoMaxHold {
				hold = until
			}
		}
		s.setHold(authIndex, hold)
		appendClaimAudit(authIndex, target.PlanID, "auto_success", now)
		return claimAutoTickResult{Action: "claimed", AuthIndex: authIndex, PlanID: target.PlanID}
	}
	appendClaimAudit(authIndex, target.PlanID, "auto_"+result.Category, now)
	if result.Category == "login_required" || result.Category == "unusual_activity" {
		s.setStopped()
		return claimAutoTickResult{Action: "stopped", AuthIndex: authIndex, PlanID: target.PlanID, Message: result.Message}
	}
	hold := now.Add(cfg.Cooldown)
	if result.Category == "already_claimed" || result.Category == "quota_exhausted" {
		if endsAt := claimFailureEndsAt(result); endsAt > 0 {
			if until := time.Unix(endsAt, 0); until.After(now) && until.Sub(now) < claimAutoMaxHold {
				hold = until
			}
		}
	}
	s.setHold(authIndex, hold)
	return fail(result.Category, result.Message, target.PlanID)
}

// pickClaimAutoPlan returns the configured plan, or the highest-priority one
// from the preview list (server order breaks ties via stable max scan).
func pickClaimAutoPlan(planID string, plans []claimPlan) *claimPlan {
	planID = strings.TrimSpace(planID)
	if planID != "" {
		for i := range plans {
			if strings.TrimSpace(plans[i].PlanID) == planID {
				return &plans[i]
			}
		}
		return nil
	}
	if len(plans) == 0 {
		return nil
	}
	best := 0
	for i := 1; i < len(plans); i++ {
		if plans[i].Priority > plans[best].Priority {
			best = i
		}
	}
	return &plans[best]
}

// claimFailureEndsAt extracts the server-provided next claimable window from a
// claim response's data.plan.ends_at (unix seconds), used to back off past
// already_claimed / quota_exhausted failures.
func claimFailureEndsAt(result claimResult) int64 {
	var envelope struct {
		Plan struct {
			EndsAt int64 `json:"ends_at"`
		} `json:"plan"`
	}
	if json.Unmarshal(result.Data, &envelope) != nil {
		return 0
	}
	return envelope.Plan.EndsAt
}

// claimAutoStatus exposes scheduler + pool state to the management page.
// Returns counts only; never returns captcha tokens.
func claimAutoStatus(now time.Time) map[string]any {
	cfg := claimAutoConfigFromRoute(currentRouteConfig())
	claimAuto.mu.Lock()
	held := 0
	for _, h := range claimAuto.holdUntil {
		if h.After(now) {
			held++
		}
	}
	running, stopped := claimAuto.running, claimAuto.stopped
	claimAuto.mu.Unlock()
	return map[string]any{
		"enabled":          cfg.Enabled,
		"running":          running,
		"stopped":          stopped,
		"held_accounts":    held,
		"plan_id":          cfg.PlanID,
		"poll_interval_ms": int(cfg.PollInterval / time.Millisecond),
		"cooldown_ms":      int(cfg.Cooldown / time.Millisecond),
		"captcha_ready":    claimCaptchaPool.stats(now),
	}
}
