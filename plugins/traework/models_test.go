package main

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var errTestAuthNotFound = errors.New("test auth not found")

// statefulHostAuth is an in-memory HostAuth fake that persists documents across
// List/Get/Save calls, mirroring how the CPA host stores traework auth files.
type statefulHostAuth struct {
	mu    sync.Mutex
	docs  map[string]map[string]json.RawMessage
	saves int
}

func (s *statefulHostAuth) call(method string, payload any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case pluginabi.MethodHostAuthList:
		files := make([]map[string]any, 0, len(s.docs))
		for name := range s.docs {
			files = append(files, map[string]any{"auth_index": name, "name": name, "provider": ProviderTraeWork})
		}
		b, _ := json.Marshal(map[string]any{"files": files})
		return b, nil
	case pluginabi.MethodHostAuthGet:
		var req pluginapi.HostAuthGetRequest
		raw, _ := json.Marshal(payload)
		_ = json.Unmarshal(raw, &req)
		doc, ok := s.docs[req.AuthIndex]
		if !ok {
			return nil, errTestAuthNotFound
		}
		b, _ := json.Marshal(map[string]any{"auth_index": req.AuthIndex, "json": doc})
		return b, nil
	case pluginabi.MethodHostAuthSave:
		var req pluginapi.HostAuthSaveRequest
		raw, _ := json.Marshal(payload)
		_ = json.Unmarshal(raw, &req)
		var doc map[string]json.RawMessage
		_ = json.Unmarshal(req.JSON, &doc)
		s.docs[req.Name] = doc
		s.saves++
		b, _ := json.Marshal(map[string]any{"name": req.Name})
		return b, nil
	}
	return nil, nil
}

func TestBuildRestorePersistedTraeModels(t *testing.T) {
	models := []pluginapi.ModelInfo{newTraeModel("glm-5.2", "GLM 5.2", time.Unix(100, 0))}
	p := buildPersistedTraeModels(models, time.Unix(200, 0))
	if p.RefreshedAt != 200 || len(p.Models) != 1 || p.Models[0].ID != "traework-GLM-5.2" {
		t.Fatalf("built=%+v", p)
	}
	got := restoreTraeModels(p)
	if len(got) != 1 || got[0].ID != "traework-GLM-5.2" || got[0].DisplayName != "GLM 5.2" {
		t.Fatalf("restored=%+v", got)
	}
}

func TestPersistTraeModelsRoundTrip(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	fake := &statefulHostAuth{docs: map[string]map[string]json.RawMessage{
		"traework-u1.json": {
			"type":     json.RawMessage(`"traework"`),
			"provider": json.RawMessage(`"traework"`),
			"storage":  json.RawMessage(`{"provider":"traework","access_token":"a","refresh_token":"r"}`),
		},
	}}
	callHostRPC = fake.call
	auth := HostAuth{Call: callHostRPC}
	at := time.Unix(200, 0)
	if err := persistTraeModels(auth, "traework-u1.json", []pluginapi.ModelInfo{
		newTraeModel("glm-5.2", "GLM", time.Unix(100, 0)),
		newTraeModel("kimi-k3", "Kimi", time.Unix(100, 0)),
	}, at); err != nil {
		t.Fatal(err)
	}
	if fake.saves != 1 {
		t.Fatalf("saves=%d", fake.saves)
	}
	doc := fake.docs["traework-u1.json"]
	var storage map[string]json.RawMessage
	if err := json.Unmarshal(doc["storage"], &storage); err != nil {
		t.Fatal(err)
	}
	if len(storage["traework_models_cache"]) == 0 {
		t.Fatalf("cache was not stored inside canonical storage: %s", doc["storage"])
	}
	if len(doc["traework_models_cache"]) != 0 {
		t.Fatal("cache must not be stored at physical top level")
	}
	// Persisted cache is stored inside the auth file and survives a process
	// restart: load with a fresh HostAuth (fresh docs map in a new fake).
	fake2 := &statefulHostAuth{docs: fake.docs}
	callHostRPC = fake2.call
	got, gotAt, err := loadPersistedTraeModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "traework-GLM" || got[1].ID != "traework-Kimi" {
		t.Fatalf("got=%+v", got)
	}
	if gotAt.Unix() != 200 {
		t.Fatalf("at=%v", gotAt)
	}
}

func TestTraeModelsFallsBackToPersistedCache(t *testing.T) {
	oldCall := callHostRPC
	defer func() { callHostRPC = oldCall }()
	fake := &statefulHostAuth{docs: map[string]map[string]json.RawMessage{
		"traework-u1.json": {
			"type":     json.RawMessage(`"traework"`),
			"provider": json.RawMessage(`"traework"`),
			"storage":  json.RawMessage(`{"provider":"traework","access_token":"a","refresh_token":"r"}`),
		},
	}}
	callHostRPC = fake.call
	if err := persistTraeModels(HostAuth{Call: callHostRPC}, "traework-u1.json",
		[]pluginapi.ModelInfo{newTraeModel("glm-5.2", "GLM", time.Unix(1, 0))}, time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	// Simulate a fresh process: empty in-memory cache.
	modelCache.Lock()
	modelCache.models = nil
	modelCache.at = time.Time{}
	modelCache.Unlock()
	models := traeModels()
	if len(models) != 1 || models[0].ID != "traework-GLM" {
		t.Fatalf("got=%+v", models)
	}
	if got := modelCacheTime(); got == "静态安全 fallback" {
		t.Fatalf("expected persisted cache time, got %q", got)
	}
}

func TestRefreshTraeModelsInStorageAttachesCacheWithoutSave(t *testing.T) {
	oldCall, oldHTTP := callHostRPC, hostHTTPCall
	defer func() { callHostRPC = oldCall; hostHTTPCall = oldHTTP }()
	fake := &statefulHostAuth{docs: map[string]map[string]json.RawMessage{}}
	callHostRPC = fake.call
	hostHTTPCall = func(_ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.URL != traeAgentHost+traeModelsPath {
			t.Fatalf("url=%s", req.URL)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM"}},{"config_name":"kimi-k3","display_config":{"display_name":"Kimi"}}]}`)}, nil
	}
	storage, _ := json.Marshal(&authStorage{AccessToken: "a", RefreshToken: "r", Provider: ProviderTraeWork, Source: "oauth"})
	updated := refreshTraeModelsInStorage("", storage)
	if fake.saves != 0 {
		t.Fatalf("unexpected persist saves=%d", fake.saves)
	}
	var got authStorage
	if err := json.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	if got.ModelsCache == nil || len(got.ModelsCache.Models) != 2 {
		t.Fatalf("storage cache=%s", updated)
	}
	modelCache.Lock()
	memoryCount := len(modelCache.models)
	modelCache.Unlock()
	if memoryCount < 2 {
		t.Fatalf("memory cache count=%d", memoryCount)
	}
}
