package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ProviderBAI upstream endpoint (OpenAI-compatible aggregate).
const baiUpstreamURL = "https://api.b.ai/v1/chat/completions"

// authStorage is the persisted credential shape for a B.AI API key.
type authStorage struct {
	APIKey    string `json:"api_key,omitempty"`
	UserLabel string `json:"user_label,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Source    string `json:"source,omitempty"`
}

func parseAuthStorage(raw []byte) (*authStorage, error) {
	var s authStorage
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.APIKey) == "" {
		return nil, errMissingAPIKey
	}
	return &s, nil
}

type missingAPIKeyError struct{}

func (missingAPIKeyError) Error() string { return "missing bai api_key" }

var errMissingAPIKey error = missingAPIKeyError{}

// handleAuthParse recognizes a B.AI API key from panel-provided material.
//
// Accepted shapes:
//  1. JSON with an api_key field (provider bai or bai- prefixed file name).
//  2. A bare API key string (JSON string payload or plain text).
//  3. A "bai:<key>" or "bai-<key>" prefixed string.
func handleAuthParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	raw := req.RawJSON
	if len(raw) == 0 {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	var declared struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
	}
	_ = json.Unmarshal(raw, &declared)
	typ := strings.ToLower(strings.TrimSpace(declared.Type))
	if typ != "" && typ != ProviderBAI {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if typ == "" {
		routed := strings.EqualFold(strings.TrimSpace(req.Provider), ProviderBAI)
		prefixed := strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.FileName)), ProviderBAI+"-")
		if !routed && !prefixed {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
		}
	}

	var s authStorage
	trimmedInput := strings.TrimSpace(string(raw))
	var decodedInput string
	if json.Unmarshal(raw, &decodedInput) == nil {
		trimmedInput = strings.TrimSpace(decodedInput)
	}

	// Prefix form: bai:<key> / bai-<key>
	if key, ok := strings.CutPrefix(trimmedInput, "bai:"); ok {
		s = authStorage{APIKey: strings.TrimSpace(key)}
	} else if !strings.HasPrefix(trimmedInput, "{") && len(trimmedInput) > 4 {
		// Only strip the bai- prefix for plain text bodies, so JSON payloads
		// are untouched. Accepts bai-<key> verbatim.
		if rest, ok := strings.CutPrefix(trimmedInput, "bai-"); ok && looksLikeAPIKey(rest) {
			s = authStorage{APIKey: strings.TrimSpace(rest)}
		}
	}

	if s.APIKey == "" {
		var probe map[string]string
		if err := json.Unmarshal(raw, &probe); err == nil {
			for _, k := range []string{"api_key", "apikey", "api-key", "key", "token"} {
				if v := strings.TrimSpace(probe[k]); looksLikeAPIKey(v) {
					s = authStorage{APIKey: v}
					break
				}
			}
		}
	}
	if s.APIKey == "" && looksLikeAPIKey(trimmedInput) {
		s = authStorage{APIKey: trimmedInput}
	}
	if strings.TrimSpace(s.APIKey) == "" {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	s.Provider = ProviderBAI
	s.Source = "api-key"
	if s.UserLabel == "" {
		s.UserLabel = "B.AI API Key · " + maskKey(s.APIKey)
	}
	storage, err := json.Marshal(&s)
	if err != nil {
		return nil, err
	}
	auth := pluginapi.AuthData{
		Provider:    ProviderBAI,
		FileName:    req.FileName,
		Label:       s.UserLabel,
		StorageJSON: storage,
		Metadata: map[string]any{
			"type":            ProviderBAI,
			"source":          s.Source,
			"credential_type": s.Source,
			"upstream":        "api.b.ai",
		},
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

// handleAuthRefresh keeps the existing credential as-is; B.AI keys do not expire.
func handleAuthRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	s, err := parseAuthStorage(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    ProviderBAI,
			ID:          req.AuthID,
			Label:       s.UserLabel,
			StorageJSON: req.StorageJSON,
			Metadata: map[string]any{
				"type":            ProviderBAI,
				"source":          s.Source,
				"credential_type": s.Source,
				"upstream":        "api.b.ai",
			},
		},
		NextRefreshAfter: time.Now().Add(24 * time.Hour).UTC(),
	})
}

func looksLikeAPIKey(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 20 || len(v) > 200 {
		return false
	}
	if strings.ContainsAny(v, " \t\r\n{}[]\"") {
		return false
	}
	return true
}

// maskKey shows only the first 4 and last 4 characters.
func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}
