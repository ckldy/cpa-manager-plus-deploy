package main

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ProviderZCode is the plugin provider key.
const ProviderZCode = "zcode"

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "zcode",
			Version:          "0.6.11",
			Author:           "ckldy",
			GitHubRepository: "https://github.com/ckldy/cpa-plugin-zcode",
			Logo:             "https://raw.githubusercontent.com/ckldy/cpa-plugin-zcode/main/logo.png",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "route_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"free-first", "paid-first", "strict"}, Description: "Credential route policy; defaults to free-first without paid fallback."},
				{Name: "strict_route", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"coding-plan", "api-key"}, Description: "Route used in strict mode."},
				{Name: "allow_paid_fallback", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow free-first mode to consume API Key balance after confirmed Coding Plan exhaustion."},
				{Name: "retain_dual_credentials", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow OAuth to retain both JWT and API Key for route switching."},
				{Name: "dynamic_routing_active", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Actively apply validated dynamic endpoint mappings; safe default is observe-only."},
				{Name: "client_signing_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable V4 client signing; disabled by default."},
				{Name: "client_signing_allow_unsigned_chat_replay", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow unsigned chat replay after signing verification failures; disabled by default."},
				{Name: "off_peak_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable isolated experimental Off-Peak execution for the explicit zcode-offpeak-glm-4.5-flash model; requires JWT + Coding Plan API Key; disabled by default."},
				{Name: "start_plan_auto_claim", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Auto-claim the weekend/trial start-plan campaign for JWT-capable accounts, consuming one user-supplied captcha-pool token per attempt; disabled by default."},
				{Name: "captcha_solver_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Warm the captcha pool from the local Node happy-dom solver service (captcha_solver_url); disabled by default. Never runs the solver in-process."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"anthropic"},
			ExecutorOutputFormats: []string{"anthropic"},
			ManagementAPI:         true,
		},
	}
}

// zcodeModels returns the model catalog contributed by the plugin.
// Model IDs are prefixed with zcode- so they are unambiguous and never collide
// with built-in providers (e.g. glm-5.1 exists in the CPA built-in catalog).
func zcodeModels() []pluginapi.ModelInfo {
	now := time.Now().Unix()
	base := pluginapi.ModelInfo{
		Object:                     "model",
		OwnedBy:                    ProviderZCode,
		Created:                    now,
		SupportedGenerationMethods: []string{"chat"},
		Type:                       "chat",
		UserDefined:                true,
	}
	mk := func(id string, ctxLen, maxOut int64, display string) pluginapi.ModelInfo {
		m := base
		m.ID = "zcode-" + id
		m.Name = id
		m.DisplayName = display
		m.ContextLength = ctxLen
		m.MaxCompletionTokens = maxOut
		m.InputTokenLimit = ctxLen
		m.OutputTokenLimit = maxOut
		m.SupportedInputModalities = []string{"text"}
		m.SupportedOutputModalities = []string{"text"}
		return m
	}
	flash := mk("glm-5.3-flash", 1000000, 128000, "ZCode GLM-5.3 Flash")
	flash.SupportedInputModalities = []string{"text", "image"}
	offPeak := mk("offpeak-glm-4.5-flash", 131072, 98304, "ZCode Off-Peak GLM-4.5 Flash (实验性·默认关闭)")
	return []pluginapi.ModelInfo{
		mk("glm-5.3", 1000000, 128000, "ZCode GLM-5.3"),
		mk("glm-4.5-flash", 131072, 98304, "ZCode GLM-4.5 Flash (免费直通)"),
		offPeak,
		flash,
		mk("glm-5.2", 1000000, 128000, "ZCode GLM-5.2 (兼容)"),
		mk("glm-5.1", 1000000, 128000, "ZCode GLM-5.1 (兼容)"),
		mk("glm-5-turbo", 1000000, 128000, "ZCode GLM-5 Turbo (兼容)"),
		mk("glm-4.7", 1000000, 128000, "ZCode GLM-4.7 (兼容)"),
	}
}
