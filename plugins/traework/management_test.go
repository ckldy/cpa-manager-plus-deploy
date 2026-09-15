package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestSetAuthDisabledPreservesPhysicalJSON(t *testing.T) {
	var saved map[string]any
	host := HostAuth{
		Call: func(method string, payload any) (json.RawMessage, error) {
			switch method {
			case pluginabi.MethodHostAuthList:
				return json.RawMessage(`{"files":[{"auth_index":"a1","name":"traework-u1.json","provider":"traework"}]}`), nil
			case pluginabi.MethodHostAuthGet:
				return json.RawMessage(`{"auth_index":"a1","json":{"type":"traework","disabled":false,"storage":{"access_token":"secret"},"unknown":{"keep":true}}}`), nil
			case pluginabi.MethodHostAuthSave:
				raw, _ := json.Marshal(payload)
				var req struct {
					Name string          `json:"name"`
					JSON json.RawMessage `json:"json"`
				}
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Fatal(err)
				}
				if req.Name != "traework-u1.json" {
					t.Fatalf("name=%q", req.Name)
				}
				if err := json.Unmarshal(req.JSON, &saved); err != nil {
					t.Fatal(err)
				}
				return json.RawMessage(`{"name":"traework-u1.json"}`), nil
			default:
				return nil, errors.New("unexpected host method")
			}
		},
	}
	if err := host.SetDisabled(context.Background(), "a1", "traework-u1.json", true); err != nil {
		t.Fatal(err)
	}
	if saved["disabled"] != true {
		t.Fatalf("disabled=%v", saved["disabled"])
	}
	if saved["unknown"].(map[string]any)["keep"] != true {
		t.Fatal("unknown top-level data was not preserved")
	}
	if saved["storage"].(map[string]any)["access_token"] != "secret" {
		t.Fatal("storage was not preserved")
	}
}

func TestHostAuthRejectsForeignProviderBeforeGetOrSave(t *testing.T) {
	getOrSaveCalled := false
	host := HostAuth{Call: func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthList {
			return json.RawMessage(`{"files":[{"auth_index":"foreign","name":"other.json","provider":"other"}]}`), nil
		}
		getOrSaveCalled = true
		return nil, errors.New("must not be called")
	}}
	if _, err := host.Get(context.Background(), "foreign"); !errors.Is(err, ErrForeignHostAuth) {
		t.Fatalf("err=%v", err)
	}
	if getOrSaveCalled {
		t.Fatal("foreign auth reached get/save")
	}
}

func TestHostMarkerStorePreservesPhysicalJSON(t *testing.T) {
	var saved map[string]any
	host := HostAuth{Call: func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return json.RawMessage(`{"files":[{"auth_index":"a1","name":"traework-u1.json","provider":"traework"}]}`), nil
		case pluginabi.MethodHostAuthGet:
			return json.RawMessage(`{"auth_index":"a1","json":{"type":"traework","storage":{"refresh_token":"opaque-test-value"},"unknown":7}}`), nil
		case pluginabi.MethodHostAuthSave:
			raw, _ := json.Marshal(payload)
			var req struct {
				JSON json.RawMessage `json:"json"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(req.JSON, &saved); err != nil {
				t.Fatal(err)
			}
			return json.RawMessage(`{"name":"traework-u1.json"}`), nil
		default:
			return nil, errors.New("unexpected method")
		}
	}}
	store := HostAuthMarkerStore{Auth: host}
	marker := CheckinMarker{LastDay: "2026-08-31", LastStatus: "already_checked", LastAt: 123}
	if err := store.Save(context.Background(), ScheduledAccount{AuthIndex: "a1", Name: "traework-u1.json"}, marker); err != nil {
		t.Fatal(err)
	}
	if saved["unknown"] != float64(7) {
		t.Fatal("unknown field lost")
	}
	storage := saved["storage"].(map[string]any)
	if storage["refresh_token"] != "opaque-test-value" {
		t.Fatal("credential field lost")
	}
	got := storage["traework_auto_checkin"].(map[string]any)
	if _, exists := saved["traework_auto_checkin"]; exists {
		t.Fatal("marker must live in canonical storage, not at physical top level")
	}
	if got["last_day"] != marker.LastDay || got["last_status"] != marker.LastStatus {
		t.Fatalf("marker=%v", got)
	}
}

func TestDeleteAuthIsExplicitlyUnsupported(t *testing.T) {
	called := false
	host := HostAuth{Call: func(string, any) (json.RawMessage, error) { called = true; return nil, nil }}
	err := host.Delete(context.Background(), "a1", "DELETE")
	if !errors.Is(err, ErrHostAuthDeleteUnsupported) {
		t.Fatalf("err=%v", err)
	}
	if called {
		t.Fatal("delete must not call the host or remove a file directly")
	}
}

func TestManagementRejectsDeleteWithoutConfirmation(t *testing.T) {
	m := NewManagement(HostAuth{}, nil)
	result := m.Delete(context.Background(), "a1", "no")
	if result.Status != 400 || result.Code != "confirmation_required" {
		t.Fatalf("result=%+v", result)
	}
}
