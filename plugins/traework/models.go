package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func staticTraeWorkModels(now time.Time) []pluginapi.ModelInfo {
	// Safe fallback: IDs are verified SOLO configuration names; unknown limits stay zero.
	ids := []string{"glm-5.2"}
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, newTraeModel(id, id, now))
	}
	return out
}

// newTraeModel uses the upstream display_name for the model ID so users see
// readable names like "traework-GLM-5.2" instead of "traework-glm-5.2". The
// config_name is kept in the Name field for request mapping.
func newTraeModel(configName, displayName string, now time.Time) pluginapi.ModelInfo {
	id := modelID(configName, displayName)
	return pluginapi.ModelInfo{
		ID:                         ProviderTraeWork + "-" + id,
		Name:                       configName,
		DisplayName:                firstNonEmpty(displayName, configName),
		Object:                     "model",
		OwnedBy:                    ProviderTraeWork,
		Created:                    now.Unix(),
		Type:                       "chat",
		UserDefined:                true,
		SupportedGenerationMethods: []string{"chat"},
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
	}
}

// modelID picks the display_name as the user-facing ID suffix when it's
// different from config_name and meaningful. Otherwise keeps config_name.
func modelID(configName, displayName string) string {
	if displayName != "" && displayName != "-" && displayName != configName {
		s := sanitizeModelID(displayName)
		if s != "" {
			return s
		}
	}
	return configName
}

// sanitizeModelID replaces spaces with hyphens, keeps safe characters.
func sanitizeModelID(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == ' ' {
			b.WriteRune('-')
		} else if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// persistedTraeModels is the model cache stored inside the traework auth file.
// Persisting serves two purposes: (1) the cache survives plugin/container
// restarts, so the host registers the real models even before any manual
// refresh; (2) the auth-file write wakes the host watcher, which re-runs model
// registration so refreshed models become visible in /v1/models immediately
// instead of only after an unrelated auth/config event.
type persistedTraeModels struct {
	RefreshedAt int64                `json:"refreshed_at"`
	Models      []persistedTraeModel `json:"models"`
}
type persistedTraeModel struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

func buildPersistedTraeModels(models []pluginapi.ModelInfo, at time.Time) persistedTraeModels {
	out := persistedTraeModels{RefreshedAt: at.Unix()}
	for _, m := range models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		out.Models = append(out.Models, persistedTraeModel{ID: m.ID, Name: m.Name, DisplayName: m.DisplayName})
	}
	return out
}

func restoreTraeModels(p persistedTraeModels) []pluginapi.ModelInfo {
	now := time.Unix(p.RefreshedAt, 0)
	out := make([]pluginapi.ModelInfo, 0, len(p.Models))
	for _, m := range p.Models {
		sid := strings.TrimSpace(m.ID)
		cn := strings.TrimSpace(m.Name)
		if cn == "" {
			// Old cache without Name field: derive config_name from legacy ID.
			cn = strings.TrimPrefix(sid, ProviderTraeWork+"-")
		}
		if cn == "" {
			continue
		}
		out = append(out, newTraeModel(cn, m.DisplayName, now))
	}
	return out
}

// persistTraeModels writes the model cache into the traework auth file. The
// write triggers the host watcher, which re-runs model registration. name is
// the auth file name; when empty the first enabled traework account is used.
func persistTraeModels(auth HostAuth, name string, models []pluginapi.ModelInfo, at time.Time) error {
	if len(models) == 0 {
		return errors.New("no models to persist")
	}
	ctx := context.Background()
	files, err := auth.List(ctx)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	var authIndex string
	for _, f := range files {
		if !validTraeWorkFile(f) || f.Disabled {
			continue
		}
		if name == "" || strings.EqualFold(strings.TrimSpace(f.Name), name) {
			name, authIndex = strings.TrimSpace(f.Name), f.AuthIndex
			break
		}
	}
	if authIndex == "" {
		return errors.New("traework auth not found for model cache persist")
	}
	doc, err := auth.Get(ctx, authIndex)
	if err != nil {
		return err
	}
	var physical map[string]json.RawMessage
	if err := json.Unmarshal(doc.JSON, &physical); err != nil {
		return fmt.Errorf("decode physical auth json: %w", err)
	}
	raw, err := json.Marshal(buildPersistedTraeModels(models, at))
	if err != nil {
		return err
	}
	if err := setCanonicalStorageField(physical, "traework_models_cache", raw); err != nil {
		return err
	}
	updated, err := json.Marshal(physical)
	if err != nil {
		return err
	}
	return auth.Save(ctx, name, updated)
}

// loadPersistedTraeModels returns the most recently cached model list from the
// first enabled traework account that carries a cache. It is the restart-safe
// fallback between the in-memory cache and the static safety fallback.
func loadPersistedTraeModels() ([]pluginapi.ModelInfo, time.Time, error) {
	auth := HostAuth{Call: callHostRPC}
	ctx := context.Background()
	files, err := auth.List(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	for _, f := range files {
		if !validTraeWorkFile(f) || f.Disabled {
			continue
		}
		doc, err := auth.Get(ctx, f.AuthIndex)
		if err != nil {
			continue
		}
		var physical struct {
			Storage struct {
				Cache persistedTraeModels `json:"traework_models_cache"`
			} `json:"storage"`
			Cache persistedTraeModels `json:"traework_models_cache"`
		}
		if err := json.Unmarshal(doc.JSON, &physical); err != nil {
			continue
		}
		cache := physical.Storage.Cache
		if len(cache.Models) == 0 {
			cache = physical.Cache // legacy top-level cache
		}
		if len(cache.Models) == 0 {
			continue
		}
		models := restoreTraeModels(cache)
		if len(models) > 0 {
			return models, time.Unix(cache.RefreshedAt, 0), nil
		}
	}
	return nil, time.Time{}, nil
}

func traeModels() []pluginapi.ModelInfo {
	modelCache.Lock()
	if len(modelCache.models) > 0 {
		defer modelCache.Unlock()
		return append([]pluginapi.ModelInfo(nil), modelCache.models...)
	}
	modelCache.Unlock()
	if persisted, at, err := loadPersistedTraeModels(); err == nil && len(persisted) > 0 {
		modelCache.Lock()
		if len(modelCache.models) == 0 {
			modelCache.models = append([]pluginapi.ModelInfo(nil), persisted...)
			modelCache.at = at
		}
		out := append([]pluginapi.ModelInfo(nil), modelCache.models...)
		modelCache.Unlock()
		return out
	}
	return staticTraeWorkModels(time.Now())
}
func modelCacheTime() string {
	modelCache.Lock()
	defer modelCache.Unlock()
	if modelCache.at.IsZero() {
		return "静态安全 fallback"
	}
	return modelCache.at.UTC().Format(time.RFC3339)
}

// fetchTraeModels pulls the live model list from the TRAE agent endpoint and
// populates the in-memory cache. Persisting is left to the caller so login and
// refresh flows can decide when to wake the host watcher.
func fetchTraeModels(callback string, s *authStorage) ([]pluginapi.ModelInfo, error) {
	body, _ := json.Marshal(map[string]any{"function": SOLOFunction, "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil})
	resp, err := boundedDo(contextBackground(), callback, pluginapi.HTTPRequest{Method: "POST", URL: traeAgentHost + traeModelsPath, Headers: traeHeaders(s, false), Body: body}, 1<<20)
	if err != nil {
		return nil, err
	}
	if err = upstreamStatus(resp); err != nil {
		return nil, err
	}
	var v struct {
		List []struct {
			Name    string `json:"config_name"`
			Display struct {
				Name string `json:"display_name"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err = json.Unmarshal(resp.Body, &v); err != nil {
		return nil, err
	}
	out := []pluginapi.ModelInfo{}
	for _, x := range v.List {
		if strings.TrimSpace(x.Name) != "" {
			out = append(out, newTraeModel(x.Name, x.Display.Name, time.Now()))
		}
	}
	if len(out) == 0 {
		return nil, ErrInvalidOpenAIPayload
	}
	modelCache.Lock()
	modelCache.models = append([]pluginapi.ModelInfo(nil), out...)
	modelCache.at = time.Now()
	modelCache.Unlock()
	return out, nil
}

// refreshTraeModels fetches live models and persists them into the auth file.
// The persist is best-effort: even when it fails the in-memory cache is fresh
// and the next host registration event will pick the models up.
func refreshTraeModels(callback string, s *authStorage, name string) ([]pluginapi.ModelInfo, error) {
	models, err := fetchTraeModels(callback, s)
	if err != nil {
		return nil, err
	}
	if err := persistTraeModels(HostAuth{Call: callHostRPC}, name, models, time.Now()); err != nil {
		_ = err // non-fatal
	}
	return models, nil
}

func attachTraeModelsCache(storageJSON json.RawMessage, models []pluginapi.ModelInfo, at time.Time) (json.RawMessage, error) {
	var s authStorage
	if err := json.Unmarshal(storageJSON, &s); err != nil {
		return nil, err
	}
	cache := buildPersistedTraeModels(models, at)
	s.ModelsCache = &cache
	return json.Marshal(s)
}

// refreshTraeModelsInStorage is used by login flows before the host saves the
// new auth. Keeping the cache inside canonical StorageJSON makes it survive the
// host's AuthParse normalization; one host save is enough.
func refreshTraeModelsInStorage(callback string, storageJSON json.RawMessage) json.RawMessage {
	s, err := parseAuthStorage(storageJSON)
	if err != nil || strings.TrimSpace(s.AccessToken) == "" {
		return storageJSON
	}
	models, err := fetchTraeModels(callback, s)
	if err != nil {
		return storageJSON
	}
	updated, err := attachTraeModelsCache(storageJSON, models, time.Now())
	if err != nil {
		return storageJSON
	}
	return updated
}

// Kept tiny to avoid leaking cancellation semantics into model registration callbacks.
func contextBackground() context.Context { return context.Background() }
