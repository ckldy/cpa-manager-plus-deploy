package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestPrefixModelIDsMarksPublicListOnly guards the fork patch: ModelProvider
// responses carry the "workbuddy-" marker, internal slices are never mutated,
// and prefixing is idempotent.
func TestPrefixModelIDsMarksPublicListOnly(t *testing.T) {
	in := []pluginapi.ModelInfo{defaultModelInfo("glm-5.2", "GLM-5.2")}
	out := prefixModelIDs(in)
	if out[0].ID != "workbuddy-glm-5.2" {
		t.Fatalf("prefixed ID = %q, want workbuddy-glm-5.2", out[0].ID)
	}
	if in[0].ID != "glm-5.2" {
		t.Fatalf("input slice was mutated: %#v", in)
	}
	again := prefixModelIDs(out)
	if again[0].ID != "workbuddy-glm-5.2" {
		t.Fatalf("prefixing not idempotent: %q", again[0].ID)
	}
}

// TestResolveUpstreamModelStripsPublicPrefix guards the request path: clients
// request the prefixed public ID, the executor must forward the bare upstream
// ID; aliases configured with bare names still resolve after prefix stripping.
func TestResolveUpstreamModelStripsPublicPrefix(t *testing.T) {
	if got := resolveUpstreamModel("workbuddy-glm-5.2", nil); got != "glm-5.2" {
		t.Fatalf("prefixed model resolved to %q", got)
	}
	if got := resolveUpstreamModel("glm-5.2", nil); got != "glm-5.2" {
		t.Fatalf("bare model resolved to %q", got)
	}

	modelAliasCache.Lock()
	old := modelAliasCache.byAlias
	modelAliasCache.byAlias = map[string]string{"wb-ds": "deepseek-v4-flash"}
	modelAliasCache.Unlock()
	t.Cleanup(func() {
		modelAliasCache.Lock()
		modelAliasCache.byAlias = old
		modelAliasCache.Unlock()
	})

	if got := resolveUpstreamModel("wb-ds", nil); got != "deepseek-v4-flash" {
		t.Fatalf("alias resolved to %q", got)
	}
	if got := resolveUpstreamModel("workbuddy-deepseek-v4-flash", nil); got != "deepseek-v4-flash" {
		t.Fatalf("prefixed model alias-resolved to %q", got)
	}
}
