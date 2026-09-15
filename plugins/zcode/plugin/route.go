package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

type routeConfig struct {
	Mode                         string
	StrictRoute                  string
	AllowPaidFallback            bool
	RetainDual                   bool
	OffPeakEnabled               bool
	DynamicRoutingActive         bool
	ClientSigningEnabled         bool
	ClientSigningAllowChatReplay bool
	StartPlanAutoClaim           bool
	ClaimPlanID                  string
	ClaimPollInterval            time.Duration
	ClaimCooldown                time.Duration
	CaptchaPoolMax               int
	CaptchaPoolTTL               time.Duration
	CaptchaSolverEnabled         bool
	CaptchaSolverURL             string
	CaptchaSolverMin             int
	CaptchaSolverScene           string
	CaptchaSolverRegion          string
	CaptchaSolverPrefix          string
}

var routes = struct {
	sync.RWMutex
	config     routeConfig
	generation uint64
}{config: routeConfig{Mode: "auto", StrictRoute: "coding-plan"}}

func applyRouteConfig(request []byte) {
	var lifecycle struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if json.Unmarshal(request, &lifecycle) != nil {
		return
	}
	next := routeConfig{Mode: "auto", StrictRoute: "coding-plan"}
	for _, raw := range strings.Split(string(lifecycle.ConfigYAML), "\n") {
		parts := strings.SplitN(strings.TrimSpace(raw), ":", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := strings.TrimSpace(parts[0]), strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		switch key {
		case "route_mode":
			switch value {
			case "auto", "coding-plan", "start-plan":
				next.Mode = value
			case "free-first":
				next.Mode = "auto"
			case "paid-first":
				next.Mode = "coding-plan"
			case "strict":
				if next.StrictRoute == "api-key" {
					next.Mode = "coding-plan"
				} else {
					next.Mode = "start-plan"
				}
			}
		case "strict_route":
			if value == "coding-plan" || value == "api-key" {
				next.StrictRoute = value
			}
		case "allow_paid_fallback":
			next.AllowPaidFallback = value == "true"
		case "retain_dual_credentials":
			next.RetainDual = value == "true"
		case "off_peak_enabled":
			next.OffPeakEnabled = value == "true"
		case "start_plan_auto_claim":
			next.StartPlanAutoClaim = value == "true"
		case "start_plan_claim_plan_id":
			next.ClaimPlanID = value
		case "start_plan_claim_poll_interval_ms":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				next.ClaimPollInterval = time.Duration(n) * time.Millisecond
			}
		case "start_plan_claim_cooldown_ms":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				next.ClaimCooldown = time.Duration(n) * time.Millisecond
			}
		case "captcha_pool_max":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				next.CaptchaPoolMax = n
			}
		case "captcha_pool_ttl_ms":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				next.CaptchaPoolTTL = time.Duration(n) * time.Millisecond
			}
		case "dynamic_routing_active":
			next.DynamicRoutingActive = value == "true"
		case "client_signing_enabled":
			next.ClientSigningEnabled = value == "true"
		case "client_signing_allow_unsigned_chat_replay":
			next.ClientSigningAllowChatReplay = value == "true"
		case "use_free_plan":
			if value == "false" {
				next.Mode = "auto"
			} // preserve the production trial-first default
		case "captcha_solver_enabled":
			next.CaptchaSolverEnabled = value == "true"
		case "captcha_solver_url":
			next.CaptchaSolverURL = value
		case "captcha_solver_min":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				next.CaptchaSolverMin = n
			}
		case "captcha_solver_scene":
			next.CaptchaSolverScene = value
		case "captcha_solver_region":
			next.CaptchaSolverRegion = value
		case "captcha_solver_prefix":
			next.CaptchaSolverPrefix = value
		}
	}
	routes.Lock()
	routes.config = next
	routes.generation++
	applyRouteConfigSideEffects(next)
	routes.Unlock()
	ensureAutoClaim()
	ensureCaptchaSolver()
}

// applyRouteConfigFields applies a routeConfig directly in-memory and returns the new generation.
// Used by the save_config management handler to bypass the host PluginReconfigure round-trip.
func applyRouteConfigFields(next routeConfig) uint64 {
	routes.Lock()
	routes.config = next
	routes.generation++
	applyRouteConfigSideEffects(next)
	gen := routes.generation
	routes.Unlock()
	ensureAutoClaim()
	ensureCaptchaSolver()
	return gen
}

func applyRouteConfigSideEffects(next routeConfig) {
	if next.DynamicRoutingActive {
		_ = setEndpointRoutingMode(routingActiveMode)
	} else {
		_ = setEndpointRoutingMode(routingObserveMode)
	}
	defaultSigningManager.mu.Lock()
	defaultSigningManager.cfg.Enabled = next.ClientSigningEnabled
	defaultSigningManager.cfg.AllowUnsignedChatReplay = next.ClientSigningAllowChatReplay
	defaultSigningManager.mu.Unlock()
	claimCaptchaPool.configure(next.CaptchaPoolMax, next.CaptchaPoolTTL)
}

func currentRouteConfig() routeConfig { routes.RLock(); defer routes.RUnlock(); return routes.config }
func currentRouteConfigGeneration() uint64 {
	routes.RLock()
	defer routes.RUnlock()
	return routes.generation
}

func credentialForJWT(s *authStorage) upstreamCredential {
	headers, _ := buildZCodeIdentityHeaders(*s)
	return upstreamCredential{URL: codingPlanUpstreamURL, Headers: headers, CodingPlan: true, CaptchaUsed: s.CaptchaVerifyParam != ""}
}
func credentialForAPIKey(s *authStorage) upstreamCredential {
	return upstreamCredential{URL: apiKeyUpstreamURL, Headers: apiKeyHeaders(s.APIKey), SigningCredential: s.APIKey}
}

func resolveCredentialCandidates(storage []byte, authID string) ([]upstreamCredential, error) {
	s, err := parseAuthStorage(storage)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(s.Provider, "bigmodel") {
		if s.ZCodeJWTToken != "" || s.APIKey == "" {
			return nil, errors.New("invalid BigModel credential")
		}
		return []upstreamCredential{{URL: bigModelCodingUpstreamURL, Headers: apiKeyHeaders(s.APIKey), CodingPlan: true, SigningCredential: s.APIKey}}, nil
	}
	cfg := currentRouteConfig()
	hasJWT, hasKey := s.ZCodeJWTToken != "", s.APIKey != ""
	jwt := func() upstreamCredential { return credentialForJWT(s) }
	key := func() upstreamCredential { return credentialForAPIKey(s) }
	switch cfg.Mode {
	case "start-plan":
		if !hasJWT {
			return nil, errors.New("start-plan route selected but zcode_jwt_token is missing")
		}
		if hasKey && cfg.AllowPaidFallback {
			return []upstreamCredential{jwt(), key()}, nil
		}
		return []upstreamCredential{jwt()}, nil
	case "coding-plan":
		if !hasKey {
			return nil, errors.New("coding-plan route selected but api_key is missing")
		}
		return []upstreamCredential{key()}, nil
	default:
		if hasJWT {
			exhausted := false
			zcodeQuotas.RLock()
			q, ok := zcodeQuotas.items[authID]
			zcodeQuotas.RUnlock()
			if ok && q.Error == "" && q.Remaining != nil && *q.Remaining <= 0 {
				exhausted = true
			}
			if exhausted {
				if hasKey && cfg.AllowPaidFallback {
					return []upstreamCredential{key()}, nil
				}
				return nil, errors.New("start-plan quota exhausted; paid fallback is disabled")
			}
			if hasKey && cfg.AllowPaidFallback {
				return []upstreamCredential{jwt(), key()}, nil
			}
			return []upstreamCredential{jwt()}, nil
		}
		if hasKey {
			return []upstreamCredential{key()}, nil
		}
	}
	return nil, errors.New("no usable ZCode credential for selected route")
}

func mayFallback(status int, body []byte) bool {
	kind := classifyUpstreamStatus(status, body)
	return kind == "insufficient_balance" || kind == "credential_invalid"
}
