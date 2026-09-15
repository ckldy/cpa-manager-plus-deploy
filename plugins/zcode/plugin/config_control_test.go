package main

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func explicitConfigValues(value string) url.Values {
	v := url.Values{"action": {"prepare_config"}, "confirm": {"true"}, "confirmation_token": {"token"}}
	for _, field := range highRiskConfigFields {
		v[field] = []string{value}
	}
	return v
}

func TestHighRiskDefaultsAndInvalidReloadAreSafe(t *testing.T) {
	applyRouteConfig(lifecycleConfig("dynamic_routing_active: true\nclient_signing_enabled: true\nclient_signing_allow_unsigned_chat_replay: true\nretain_dual_credentials: true\nallow_paid_fallback: true\noff_peak_enabled: true\n"))
	applyRouteConfig(lifecycleConfig("dynamic_routing_active: yes\nclient_signing_enabled: 1\nclient_signing_allow_unsigned_chat_replay: TRUE\nretain_dual_credentials: invalid\nallow_paid_fallback:\noff_peak_enabled: nope\n"))
	if got := currentHighRiskConfig(); got != (highRiskConfigPayload{}) {
		t.Fatalf("invalid reload inherited unsafe state: %+v", got)
	}
	if endpointRoutingMode() != routingObserveMode {
		t.Fatal("dynamic routing did not return to observe mode")
	}
	if claimAuto.running {
		t.Fatal("auto-claim scheduler running despite start_plan_auto_claim unset")
	}
}

func TestHighRiskConfigRestartReloadsFromHostConfig(t *testing.T) {
	applyRouteConfig(lifecycleConfig("allow_paid_fallback: true\noff_peak_enabled: true\n"))
	before := currentRouteConfigGeneration()
	applyRouteConfig(lifecycleConfig(""))
	if currentRouteConfigGeneration() <= before || currentHighRiskConfig() != (highRiskConfigPayload{}) {
		t.Fatalf("restart/reconfigure did not reset from host config: generation=%d config=%+v", currentRouteConfigGeneration(), currentHighRiskConfig())
	}
}

func TestPrepareConfigRequiresAllSevenExplicitBooleans(t *testing.T) {
	values := explicitConfigValues("false")
	delete(values, "start_plan_auto_claim")
	if response := handlePrepareConfig(values); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing field status=%d", response.StatusCode)
	}
	values = explicitConfigValues("false")
	values.Set("start_plan_auto_claim", "yes")
	if response := handlePrepareConfig(values); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid field status=%d", response.StatusCode)
	}
}

func TestPrepareConfigConsumesBoundTokenOnceAndDoesNotPersist(t *testing.T) {
	confirmations = newConfirmationStore()
	values := explicitConfigValues("false")
	payload, err := parseExplicitHighRiskConfig(values)
	if err != nil {
		t.Fatal(err)
	}
	token, err := confirmations.create(configConfirmationPurpose, configBinding(payload), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	values.Set("confirmation_token", token)
	generation := currentRouteConfigGeneration()
	if response := handlePrepareConfig(values); response.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d body=%s", response.StatusCode, response.Body)
	}
	if currentRouteConfigGeneration() != generation {
		t.Fatal("prepare falsely mutated effective config")
	}
	if response := handlePrepareConfig(values); response.StatusCode != http.StatusConflict {
		t.Fatalf("replay status=%d", response.StatusCode)
	}
}

func TestConfigConfirmationRejectsPayloadMismatch(t *testing.T) {
	confirmations = newConfirmationStore()
	values := explicitConfigValues("false")
	payload, _ := parseExplicitHighRiskConfig(values)
	token, _ := confirmations.create(configConfirmationPurpose, configBinding(payload), time.Now())
	values.Set("confirmation_token", token)
	values.Set("allow_paid_fallback", "true")
	if response := handlePrepareConfig(values); response.StatusCode != http.StatusConflict {
		t.Fatalf("mismatch status=%d", response.StatusCode)
	}
}

func saveConfigValues(risk map[string]string) url.Values {
	v := url.Values{"action": {"save_config"}}
	v.Set("route_mode", "free-first")
	v.Set("strict_route", "coding-plan")
	for _, field := range highRiskConfigFields {
		v.Set(field, risk[field])
	}
	return v
}

func TestSaveConfigAppliesImmediatelyNoKeyNoToken(t *testing.T) {
	applyRouteConfig(lifecycleConfig(""))
	before := currentRouteConfigGeneration()
	risk := map[string]string{}
	for _, field := range highRiskConfigFields {
		risk[field] = "false"
	}
	risk["allow_paid_fallback"] = "true"
	risk["off_peak_enabled"] = "true"
	response := handleSaveConfig(saveConfigValues(risk))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("save_config status=%d body=%s", response.StatusCode, response.Body)
	}
	if currentRouteConfigGeneration() <= before {
		t.Fatal("save_config did not increment generation")
	}
	cfg := currentRouteConfig()
	if !cfg.AllowPaidFallback || !cfg.OffPeakEnabled {
		t.Fatalf("save_config did not apply fields: %+v", cfg)
	}
}

func TestSaveConfigRejectsInvalidBoolean(t *testing.T) {
	risk := map[string]string{}
	for _, field := range highRiskConfigFields {
		risk[field] = "false"
	}
	risk["allow_paid_fallback"] = "yes"
	response := handleSaveConfig(saveConfigValues(risk))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid boolean, got %d", response.StatusCode)
	}
}
