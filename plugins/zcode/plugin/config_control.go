package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const configConfirmationPurpose = "high-risk-config"

var highRiskConfigFields = []string{
	"dynamic_routing_active",
	"client_signing_enabled",
	"client_signing_allow_unsigned_chat_replay",
	"retain_dual_credentials",
	"allow_paid_fallback",
	"off_peak_enabled",
	"start_plan_auto_claim",
}

type highRiskConfigPayload struct {
	DynamicRoutingActive                 bool `json:"dynamic_routing_active"`
	ClientSigningEnabled                 bool `json:"client_signing_enabled"`
	ClientSigningAllowUnsignedChatReplay bool `json:"client_signing_allow_unsigned_chat_replay"`
	RetainDualCredentials                bool `json:"retain_dual_credentials"`
	AllowPaidFallback                    bool `json:"allow_paid_fallback"`
	OffPeakEnabled                       bool `json:"off_peak_enabled"`
	StartPlanAutoClaim                   bool `json:"start_plan_auto_claim"`
}

type effectiveConfigResponse struct {
	Effective  highRiskConfigPayload `json:"effective"`
	Generation uint64                `json:"generation"`
	Source     string                `json:"source"`
}

func currentHighRiskConfig() highRiskConfigPayload {
	cfg := currentRouteConfig()
	return highRiskConfigPayload{
		DynamicRoutingActive:                 cfg.DynamicRoutingActive,
		ClientSigningEnabled:                 cfg.ClientSigningEnabled,
		ClientSigningAllowUnsignedChatReplay: cfg.ClientSigningAllowChatReplay,
		RetainDualCredentials:                cfg.RetainDual,
		AllowPaidFallback:                    cfg.AllowPaidFallback,
		OffPeakEnabled:                       cfg.OffPeakEnabled,
		StartPlanAutoClaim:                   cfg.StartPlanAutoClaim,
	}
}

func configBinding(payload highRiskConfigPayload) string {
	raw, _ := json.Marshal(payload)
	return string(raw)
}

func parseExplicitHighRiskConfig(values url.Values) (highRiskConfigPayload, error) {
	if len(values) != len(highRiskConfigFields)+3 || values.Get("action") != "prepare_config" || values.Get("confirm") != "true" || strings.TrimSpace(values.Get("confirmation_token")) == "" {
		return highRiskConfigPayload{}, strconv.ErrSyntax
	}
	for _, field := range append([]string{"action", "confirm", "confirmation_token"}, highRiskConfigFields...) {
		if len(values[field]) != 1 {
			return highRiskConfigPayload{}, strconv.ErrSyntax
		}
	}
	read := func(name string) (bool, error) {
		value := values.Get(name)
		if value != "true" && value != "false" {
			return false, strconv.ErrSyntax
		}
		return value == "true", nil
	}
	var out highRiskConfigPayload
	var err error
	if out.DynamicRoutingActive, err = read("dynamic_routing_active"); err != nil {
		return highRiskConfigPayload{}, err
	}
	if out.ClientSigningEnabled, err = read("client_signing_enabled"); err != nil {
		return highRiskConfigPayload{}, err
	}
	if out.ClientSigningAllowUnsignedChatReplay, err = read("client_signing_allow_unsigned_chat_replay"); err != nil {
		return highRiskConfigPayload{}, err
	}
	if out.RetainDualCredentials, err = read("retain_dual_credentials"); err != nil {
		return highRiskConfigPayload{}, err
	}
	if out.AllowPaidFallback, err = read("allow_paid_fallback"); err != nil {
		return highRiskConfigPayload{}, err
	}
	if out.OffPeakEnabled, err = read("off_peak_enabled"); err != nil {
		return highRiskConfigPayload{}, err
	}
	if out.StartPlanAutoClaim, err = read("start_plan_auto_claim"); err != nil {
		return highRiskConfigPayload{}, err
	}
	return out, nil
}

func handlePrepareConfig(values url.Values) pluginapi.ManagementResponse {
	payload, err := parseExplicitHighRiskConfig(values)
	if err != nil {
		return configJSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "message": "all seven fields must be present exactly once as true or false"})
	}
	if !confirmations.consume(strings.TrimSpace(values.Get("confirmation_token")), configConfirmationPurpose, configBinding(payload), time.Now()) {
		return configJSONResponse(http.StatusConflict, map[string]any{"ok": false, "message": "confirmation token invalid, expired, mismatched, or already used"})
	}
	return configJSONResponse(http.StatusOK, map[string]any{"ok": true, "payload": payload, "persisted": false, "generation": currentRouteConfigGeneration()})
}

func configJSONResponse(status int, value any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: body}
}

func createConfigConfirmation(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	values := req.Query
	if len(values) != len(highRiskConfigFields)+1 || values.Get("config_control") != "confirm" {
		return configJSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "message": "exactly seven fields are required"})
	}
	payloadValues := url.Values{}
	for _, field := range highRiskConfigFields {
		all, ok := values[field]
		if !ok || len(all) != 1 || (all[0] != "true" && all[0] != "false") {
			return configJSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "message": "all seven fields must be explicit true or false"})
		}
		payloadValues.Set(field, all[0])
	}
	payloadValues.Set("action", "prepare_config")
	payloadValues.Set("confirm", "true")
	payloadValues.Set("confirmation_token", "placeholder")
	payload, err := parseExplicitHighRiskConfig(payloadValues)
	if err != nil {
		return configJSONResponse(http.StatusBadRequest, map[string]any{"ok": false})
	}
	token, err := confirmations.create(configConfirmationPurpose, configBinding(payload), time.Now())
	if err != nil {
		return configJSONResponse(http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "confirmation unavailable"})
	}
	return configJSONResponse(http.StatusOK, map[string]any{"ok": true, "confirmation_token": token, "expires_in_seconds": int(confirmationTTL.Seconds())})
}

func effectiveConfigManagementResponse() pluginapi.ManagementResponse {
	return configJSONResponse(http.StatusOK, effectiveConfigResponse{Effective: currentHighRiskConfig(), Generation: currentRouteConfigGeneration(), Source: "host PluginReconfigure"})
}
