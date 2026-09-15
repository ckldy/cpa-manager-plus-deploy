package main

import (
	"context"
	"errors"
	"strings"
)

type AccountClient interface {
	CheckinStatus(context.Context, ScheduledAccount) (checked bool, credits int64, enabled bool, err error)
	CheckinClaim(context.Context, ScheduledAccount) error
	Credits(context.Context, ScheduledAccount) (int64, error)
}

type Management struct {
	Auth   HostAuth
	Client AccountClient
}

type ManagementResult struct {
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
	Result any    `json:"result,omitempty"`
}

func NewManagement(auth HostAuth, client AccountClient) *Management {
	return &Management{Auth: auth, Client: client}
}

func (m *Management) Accounts(ctx context.Context) ManagementResult {
	files, err := m.Auth.List(ctx)
	if err != nil {
		return managementFailure(503, "host_auth_unavailable", err)
	}
	out := make([]HostAuthFile, 0, len(files))
	for _, file := range files {
		if strings.EqualFold(strings.TrimSpace(file.Provider), ProviderTraeWork) {
			out = append(out, file)
		}
	}
	return ManagementResult{Status: 200, Result: out}
}

func (m *Management) SetEnabled(ctx context.Context, authIndex, name string, enabled bool) ManagementResult {
	if err := m.Auth.SetDisabled(ctx, authIndex, name, !enabled); err != nil {
		return managementFailure(503, "host_auth_save_failed", err)
	}
	return ManagementResult{Status: 200, Result: map[string]bool{"enabled": enabled}}
}

func (m *Management) Delete(ctx context.Context, authIndex, confirm string) ManagementResult {
	if confirm != "DELETE" {
		return ManagementResult{Status: 400, Code: "confirmation_required", Error: `confirm must equal "DELETE"`}
	}
	err := m.Auth.Delete(ctx, authIndex, confirm)
	if errors.Is(err, ErrHostAuthDeleteUnsupported) {
		return managementFailure(501, "unsupported", err)
	}
	if err != nil {
		return managementFailure(503, "delete_failed", err)
	}
	return ManagementResult{Status: 200}
}

func (m *Management) Credits(ctx context.Context, account ScheduledAccount) ManagementResult {
	if m.Client == nil {
		return ManagementResult{Status: 501, Code: "unsupported", Error: "credits client unavailable"}
	}
	credits, err := m.Client.Credits(ctx, account)
	if err != nil {
		return managementFailure(502, "credits_failed", err)
	}
	return ManagementResult{Status: 200, Result: map[string]int64{"credits": credits}}
}

func (m *Management) Checkin(ctx context.Context, account ScheduledAccount) ManagementResult {
	if m.Client == nil {
		return ManagementResult{Status: 501, Code: "unsupported", Error: "checkin client unavailable"}
	}
	checked, credits, enabled, err := m.Client.CheckinStatus(ctx, account)
	if err != nil {
		return managementFailure(502, "checkin_status_failed", err)
	}
	if enabled && !checked {
		if err := m.Client.CheckinClaim(ctx, account); err != nil {
			return managementFailure(502, "checkin_claim_failed", err)
		}
		checked, credits, enabled, err = m.Client.CheckinStatus(ctx, account)
		if err != nil {
			return managementFailure(502, "checkin_status_failed", err)
		}
		if !checked {
			return ManagementResult{Status: 502, Code: "checkin_not_confirmed", Error: "checkin claim was accepted but upstream status did not confirm success"}
		}
		credits, err = m.Client.Credits(ctx, account)
		if err != nil {
			return managementFailure(502, "credits_failed", err)
		}
	}
	return ManagementResult{Status: 200, Result: map[string]any{"checked_in": checked, "enabled": enabled, "credits": credits}}
}

func managementFailure(status int, code string, err error) ManagementResult {
	message := "operation failed"
	if err != nil {
		message = err.Error()
	}
	return ManagementResult{Status: status, Code: code, Error: message}
}
