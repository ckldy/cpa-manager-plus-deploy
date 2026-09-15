package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const ProviderTraeWork = "traework"

// authStorage is the canonical TraeWork StorageJSON schema. Upstream hosts are
// deliberately absent: all network destinations are compile-time bound.
type authStorage struct {
	AccessToken  string               `json:"access_token,omitempty"`
	RefreshToken string               `json:"refresh_token,omitempty"`
	ExpiresAt    int64                `json:"expires_at,omitempty"`
	MachineID    string               `json:"machine_id,omitempty"`
	DeviceID     string               `json:"device_id,omitempty"`
	UID          string               `json:"uid,omitempty"`
	EnterpriseID string               `json:"enterprise_id,omitempty"`
	Nickname     string               `json:"nickname,omitempty"`
	Provider     string               `json:"provider,omitempty"`
	Source       string               `json:"source,omitempty"`
	ModelsCache  *persistedTraeModels `json:"traework_models_cache,omitempty"`
	AutoCheckin  *CheckinMarker       `json:"traework_auto_checkin,omitempty"`
}

func parseAuthStorage(raw []byte) (*authStorage, error) {
	var envelope struct {
		Storage json.RawMessage `json:"storage"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Storage) != 0 && string(envelope.Storage) != "null" {
		raw = envelope.Storage
	}
	var forbidden struct {
		APIHost json.RawMessage `json:"api_host"`
	}
	if err := json.Unmarshal(raw, &forbidden); err != nil {
		return nil, err
	}
	if len(forbidden.APIHost) != 0 {
		return nil, errors.New("canonical storage must not contain api_host")
	}
	var s authStorage
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	normalizeAuthStorage(&s)
	if s.AccessToken == "" && s.RefreshToken == "" {
		return nil, errors.New("missing traework access_token or refresh_token")
	}
	if s.Provider != ProviderTraeWork {
		return nil, errors.New("invalid traework provider")
	}
	return &s, nil
}

func normalizeAuthStorage(s *authStorage) {
	s.AccessToken = strings.TrimSpace(s.AccessToken)
	s.RefreshToken = strings.TrimSpace(s.RefreshToken)
	s.MachineID = strings.TrimSpace(s.MachineID)
	s.DeviceID = strings.TrimSpace(s.DeviceID)
	s.UID = strings.TrimSpace(s.UID)
	s.EnterpriseID = strings.TrimSpace(s.EnterpriseID)
	s.Nickname = strings.TrimSpace(s.Nickname)
	s.Provider = strings.ToLower(strings.TrimSpace(s.Provider))
	s.Source = strings.TrimSpace(s.Source)
	if s.ExpiresAt > 1_000_000_000_000 {
		s.ExpiresAt /= 1000
	}
	if s.Provider == "" {
		s.Provider = ProviderTraeWork
	}
	if s.Source == "" {
		s.Source = "oauth"
	}
}

func setCanonicalStorageField(physical map[string]json.RawMessage, key string, value json.RawMessage) error {
	if storageRaw, ok := physical["storage"]; ok && len(storageRaw) > 0 && string(storageRaw) != "null" {
		var storage map[string]json.RawMessage
		if err := json.Unmarshal(storageRaw, &storage); err != nil {
			return fmt.Errorf("decode physical auth storage: %w", err)
		}
		storage[key] = value
		updated, err := json.Marshal(storage)
		if err != nil {
			return err
		}
		physical["storage"] = updated
		return nil
	}
	physical[key] = value
	return nil
}

type nestedTraeAuth struct {
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		APIHost      string `json:"apiHost"`
		MachineID    string `json:"machineId"`
		DeviceID     string `json:"deviceId"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
}

func parseTraeMaterial(raw []byte) (*authStorage, error) {
	if s, err := parseAuthStorage(raw); err == nil {
		return s, nil
	}
	var n nestedTraeAuth
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, err
	}
	if err := validateImportedAPIHost(n.Auth.APIHost); err != nil {
		return nil, err
	}
	s := &authStorage{AccessToken: n.Auth.AccessToken, RefreshToken: n.Auth.RefreshToken, ExpiresAt: n.Auth.ExpiresAt, MachineID: n.Auth.MachineID, DeviceID: n.Auth.DeviceID, UID: n.Account.UID, EnterpriseID: n.Account.EnterpriseID, Nickname: n.Account.Nickname, Provider: ProviderTraeWork, Source: "oauth"}
	normalizeAuthStorage(s)
	if s.AccessToken == "" && s.RefreshToken == "" {
		return nil, errors.New("missing traework token material")
	}
	return s, nil
}

func validateImportedAPIHost(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "api.trae.com.cn") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("untrusted imported apiHost")
	}
	return nil
}

func handleAuthParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if len(req.RawJSON) == 0 {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	var declared struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
	}
	_ = json.Unmarshal(req.RawJSON, &declared)
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	fileName := strings.ToLower(strings.TrimSpace(req.FileName))
	declaredType := strings.ToLower(strings.TrimSpace(declared.Type))
	declaredProvider := strings.ToLower(strings.TrimSpace(declared.Provider))
	if declaredType != "" && declaredType != ProviderTraeWork {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if declaredProvider != "" && declaredProvider != ProviderTraeWork {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if provider != ProviderTraeWork && !strings.HasPrefix(fileName, ProviderTraeWork+"-") && declaredType != ProviderTraeWork && declaredProvider != ProviderTraeWork {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	s, err := parseTraeMaterial(req.RawJSON)
	if err != nil {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	storage, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{Provider: ProviderTraeWork, FileName: req.FileName, Label: authLabel(s), StorageJSON: storage, Metadata: authMetadata(s)}})
}

// refreshAuthStorage exchanges the rotating refresh token and commits only
// after the complete response has been validated. Failure leaves s unchanged.
func refreshAuthStorage(ctx context.Context, client HTTPDoer, s *authStorage, now time.Time) error {
	if s == nil {
		return errors.New("nil auth storage")
	}
	refreshToken := strings.TrimSpace(s.RefreshToken)
	if refreshToken == "" {
		return errors.New("missing refresh token")
	}
	result, err := exchangeTokenAt(ctx, client, traeAPIHost, traeClientID, refreshToken, now)
	if err != nil {
		return err
	}
	updated := *s
	updated.AccessToken = result.AccessToken
	updated.RefreshToken = result.RefreshToken
	updated.ExpiresAt = result.ExpiresAt
	normalizeAuthStorage(&updated)
	*s = updated
	return nil
}

func handleAuthRefreshWithClient(ctx context.Context, client HTTPDoer, request []byte, now time.Time) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	s, err := parseAuthStorage(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	if err := refreshAuthStorage(ctx, client, s, now); err != nil {
		return nil, err
	}
	storage, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: ProviderTraeWork, ID: req.AuthID, Label: authLabel(s), StorageJSON: storage, Metadata: authMetadata(s)}, NextRefreshAfter: now.Add(24 * time.Hour).UTC()})
}

func handleAuthRefresh(request []byte) ([]byte, error) {
	return nil, fmt.Errorf("traework auth refresh HTTP boundary is not integrated; use handleAuthRefreshWithClient")
}

func authLabel(s *authStorage) string { return firstNonEmpty(s.Nickname, s.UID, "TraeWork account") }
func authMetadata(s *authStorage) map[string]any {
	return map[string]any{
		"type": ProviderTraeWork, "source": s.Source, "credential_type": "oauth", "uid": s.UID,
		"expires_at": s.ExpiresAt, "refresh_interval_seconds": int64((24 * time.Hour) / time.Second),
	}
}
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
