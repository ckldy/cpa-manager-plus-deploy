package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	ErrHostAuthDeleteUnsupported = errors.New("host auth delete unsupported")
	ErrForeignHostAuth           = errors.New("host auth target is not traework")
)

type HostAuthCall func(method string, payload any) (json.RawMessage, error)

type HostAuth struct{ Call HostAuthCall }

type HostAuthFile struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Label     string `json:"label"`
	Disabled  bool   `json:"disabled"`
}

type HostAuthDocument struct {
	AuthIndex string
	JSON      json.RawMessage
}

func (h HostAuth) call(method string, payload any) (json.RawMessage, error) {
	if h.Call == nil {
		return nil, errors.New("host auth callback unavailable")
	}
	return h.Call(method, payload)
}

func (h HostAuth) List(ctx context.Context) ([]HostAuthFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := h.call(pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return nil, fmt.Errorf("host.auth.list: %w", err)
	}
	var response struct {
		Files []HostAuthFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	return response.Files, nil
}

func validTraeWorkFile(file HostAuthFile) bool {
	return strings.EqualFold(strings.TrimSpace(file.Provider), ProviderTraeWork) && strings.HasPrefix(strings.ToLower(strings.TrimSpace(file.Name)), ProviderTraeWork+"-")
}

func (h HostAuth) target(ctx context.Context, authIndex string) (HostAuthFile, error) {
	files, err := h.List(ctx)
	if err != nil {
		return HostAuthFile{}, err
	}
	for _, file := range files {
		if file.AuthIndex == authIndex {
			if !validTraeWorkFile(file) {
				return HostAuthFile{}, ErrForeignHostAuth
			}
			return file, nil
		}
	}
	return HostAuthFile{}, errors.New("host auth target not found")
}

func (h HostAuth) Get(ctx context.Context, authIndex string) (HostAuthDocument, error) {
	if err := ctx.Err(); err != nil {
		return HostAuthDocument{}, err
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return HostAuthDocument{}, errors.New("auth_index is required")
	}
	if _, err := h.target(ctx, authIndex); err != nil {
		return HostAuthDocument{}, err
	}
	raw, err := h.call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return HostAuthDocument{}, fmt.Errorf("host.auth.get: %w", err)
	}
	var response struct {
		AuthIndex string          `json:"auth_index"`
		JSON      json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return HostAuthDocument{}, fmt.Errorf("decode host.auth.get: %w", err)
	}
	if len(response.JSON) == 0 || !json.Valid(response.JSON) {
		return HostAuthDocument{}, errors.New("host.auth.get returned invalid json")
	}
	return HostAuthDocument{AuthIndex: response.AuthIndex, JSON: append(json.RawMessage(nil), response.JSON...)}, nil
}

func (h HostAuth) Save(ctx context.Context, name string, document json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("auth file name is required")
	}
	if !strings.HasPrefix(strings.ToLower(name), ProviderTraeWork+"-") {
		return ErrForeignHostAuth
	}
	if len(document) == 0 || !json.Valid(document) {
		return errors.New("auth document must be valid json")
	}
	var identity struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(document, &identity); err != nil {
		return err
	}
	declared := strings.TrimSpace(identity.Provider)
	if declared == "" {
		declared = strings.TrimSpace(identity.Type)
	}
	if !strings.EqualFold(declared, ProviderTraeWork) {
		return ErrForeignHostAuth
	}
	_, err := h.call(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: document})
	if err != nil {
		return fmt.Errorf("host.auth.save: %w", err)
	}
	return nil
}

// SetDisabled performs a read-modify-save of the host's physical JSON. It only
// changes the top-level disabled field, preserving storage and unknown fields.
func (h HostAuth) SetDisabled(ctx context.Context, authIndex, name string, disabled bool) error {
	file, err := h.target(ctx, strings.TrimSpace(authIndex))
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		name = file.Name
	}
	if !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(file.Name)) {
		return ErrForeignHostAuth
	}
	doc, err := h.Get(ctx, authIndex)
	if err != nil {
		return err
	}
	var physical map[string]json.RawMessage
	if err := json.Unmarshal(doc.JSON, &physical); err != nil {
		return fmt.Errorf("decode physical auth json: %w", err)
	}
	physical["disabled"] = json.RawMessage("false")
	if disabled {
		physical["disabled"] = json.RawMessage("true")
	}
	updated, err := json.Marshal(physical)
	if err != nil {
		return err
	}
	return h.Save(ctx, name, updated)
}

type CheckinMarker struct {
	LastDay    string `json:"last_day"`
	LastStatus string `json:"last_status"`
	LastAt     int64  `json:"last_at"`
}

type HostAuthMarkerStore struct{ Auth HostAuth }

func (s HostAuthMarkerStore) Load(ctx context.Context, account ScheduledAccount) (CheckinMarker, error) {
	doc, err := s.Auth.Get(ctx, account.AuthIndex)
	if err != nil {
		return CheckinMarker{}, err
	}
	var physical struct {
		Storage struct {
			Marker CheckinMarker `json:"traework_auto_checkin"`
		} `json:"storage"`
		Marker CheckinMarker `json:"traework_auto_checkin"`
	}
	if err := json.Unmarshal(doc.JSON, &physical); err != nil {
		return CheckinMarker{}, err
	}
	if physical.Storage.Marker.LastDay != "" || physical.Storage.Marker.LastStatus != "" || physical.Storage.Marker.LastAt != 0 {
		return physical.Storage.Marker, nil
	}
	return physical.Marker, nil
}

func (s HostAuthMarkerStore) Save(ctx context.Context, account ScheduledAccount, marker CheckinMarker) error {
	file, err := s.Auth.target(ctx, account.AuthIndex)
	if err != nil {
		return err
	}
	if account.Name != "" && !strings.EqualFold(strings.TrimSpace(account.Name), file.Name) {
		return ErrForeignHostAuth
	}
	doc, err := s.Auth.Get(ctx, account.AuthIndex)
	if err != nil {
		return err
	}
	var physical map[string]json.RawMessage
	if err := json.Unmarshal(doc.JSON, &physical); err != nil {
		return fmt.Errorf("decode physical auth json: %w", err)
	}
	rawMarker, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := setCanonicalStorageField(physical, "traework_auto_checkin", rawMarker); err != nil {
		return err
	}
	updated, err := json.Marshal(physical)
	if err != nil {
		return err
	}
	return s.Auth.Save(ctx, file.Name, updated)
}

// Delete is deliberately unsupported: the verified host ABI has no delete
// callback. In particular, this method never removes an auth file directly.
func (h HostAuth) Delete(context.Context, string, string) error { return ErrHostAuthDeleteUnsupported }
