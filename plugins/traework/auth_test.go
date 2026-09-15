package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCanonicalStorageOmitsAPIHost(t *testing.T) {
	s := authStorage{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: 1893456000, MachineID: "machine", DeviceID: "device", UID: "42", EnterpriseID: "ent", Nickname: "Ada", Provider: ProviderTraeWork, Source: "oauth"}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["api_host"]; ok {
		t.Fatalf("canonical storage persisted api_host: %s", raw)
	}
	if _, ok := fields["api_key"]; ok {
		t.Fatalf("canonical storage persisted legacy api_key: %s", raw)
	}
}

func TestCanonicalStoragePreservesPluginState(t *testing.T) {
	cache := persistedTraeModels{RefreshedAt: 200, Models: []persistedTraeModel{{ID: "traework-glm-5.2"}}}
	marker := CheckinMarker{LastDay: "2026-09-01", LastStatus: "checked", LastAt: 123}
	s := authStorage{AccessToken: "access", RefreshToken: "refresh", Provider: ProviderTraeWork, ModelsCache: &cache, AutoCheckin: &marker}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseAuthStorage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ModelsCache == nil || len(parsed.ModelsCache.Models) != 1 || parsed.AutoCheckin == nil || parsed.AutoCheckin.LastDay != marker.LastDay {
		t.Fatalf("plugin state lost: %s", raw)
	}
}

func TestAuthParseImportsOfficialNestedHostAndDropsIt(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"access","refreshToken":"refresh","expiresAt":1893456000,"apiHost":"https://api.trae.com.cn","machineId":"machine","deviceId":"device"},"account":{"uid":"42","enterpriseId":"ent","nickname":"Ada"}}`)
	parsed := parseAuthResponse(t, ProviderTraeWork, "traework-42.json", nested)
	if !parsed.Handled || parsed.Auth.Provider != ProviderTraeWork || parsed.Auth.Label != "Ada" {
		t.Fatalf("unexpected response: %+v", parsed)
	}
	var fields map[string]any
	if err := json.Unmarshal(parsed.Auth.StorageJSON, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["api_host"]; ok {
		t.Fatalf("import retained apiHost: %s", parsed.Auth.StorageJSON)
	}
}

func TestAuthParsePreservesCanonicalModelsCache(t *testing.T) {
	material := []byte(`{"provider":"traework","access_token":"access","refresh_token":"refresh","traework_models_cache":{"refreshed_at":200,"models":[{"id":"traework-glm-5.2","display_name":"GLM"}]}}`)
	parsed := parseAuthResponse(t, ProviderTraeWork, "traework-42.json", material)
	if !parsed.Handled {
		t.Fatal("canonical auth was not handled")
	}
	var storage authStorage
	if err := json.Unmarshal(parsed.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage.ModelsCache == nil || storage.ModelsCache.RefreshedAt != 200 || len(storage.ModelsCache.Models) != 1 {
		t.Fatalf("cache was stripped during AuthParse: %s", parsed.Auth.StorageJSON)
	}
}

func TestAuthParseRejectsNestedMaliciousHost(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"access","refreshToken":"refresh","apiHost":"https://evil.example"},"account":{"uid":"42"}}`)
	parsed := parseAuthResponse(t, ProviderTraeWork, "traework-42.json", nested)
	if parsed.Handled {
		t.Fatal("malicious apiHost must be rejected")
	}
}

func TestAuthParseRejectsWrongRoute(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"access","refreshToken":"refresh"},"account":{"uid":"42"}}`)
	parsed := parseAuthResponse(t, "other", "other.json", nested)
	if parsed.Handled {
		t.Fatal("wrong provider must not be handled")
	}
}

func TestRefreshAuthStorageRotatesTokens(t *testing.T) {
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"Result":{"Token":"new-access","RefreshToken":"new-refresh","TokenExpireAt":1893456000000}}`), nil
	})
	s := &authStorage{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 1, UID: "42", Provider: ProviderTraeWork, Source: "oauth"}
	if err := refreshAuthStorage(context.Background(), client, s, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if s.AccessToken != "new-access" || s.RefreshToken != "new-refresh" || s.ExpiresAt != 1893456000 {
		t.Fatalf("unexpected refreshed storage: %+v", s)
	}
}

func TestRefreshAuthStorageFailureIsAtomic(t *testing.T) {
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New("transport failed") })
	s := &authStorage{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 123, UID: "42", Provider: ProviderTraeWork, Source: "oauth"}
	before := *s
	if err := refreshAuthStorage(context.Background(), client, s, time.Unix(100, 0)); err == nil {
		t.Fatal("expected refresh failure")
	}
	if !reflect.DeepEqual(*s, before) {
		t.Fatalf("failed refresh mutated storage: before=%+v after=%+v", before, *s)
	}
}

func TestHandleAuthRefreshWithClientRefreshesAndMarshals(t *testing.T) {
	client := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"Result":{"Token":"new-access","RefreshToken":"new-refresh","TokenExpireDuration":60}}`), nil
	})
	storage, _ := json.Marshal(authStorage{AccessToken: "old", RefreshToken: "refresh", UID: "42", Provider: ProviderTraeWork, Source: "oauth"})
	req, _ := json.Marshal(pluginapi.AuthRefreshRequest{AuthID: "id", StorageJSON: storage})
	out, err := handleAuthRefreshWithClient(context.Background(), client, req, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(out, &env)
	var refreshed pluginapi.AuthRefreshResponse
	_ = json.Unmarshal(env.Result, &refreshed)
	var got authStorage
	_ = json.Unmarshal(refreshed.Auth.StorageJSON, &got)
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.ExpiresAt != 160 || refreshed.Auth.ID != "id" {
		t.Fatalf("unexpected refresh: %+v %+v", got, refreshed.Auth)
	}
	if refreshed.Auth.Metadata["expires_at"] != float64(160) {
		t.Fatalf("missing expiry metadata: %#v", refreshed.Auth.Metadata)
	}
}

func parseAuthResponse(t *testing.T, provider, fileName string, material []byte) pluginapi.AuthParseResponse {
	t.Helper()
	req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: provider, FileName: fileName, RawJSON: material})
	out, err := handleAuthParse(req)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	var parsed pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}
