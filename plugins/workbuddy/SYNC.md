# WorkBuddy plugin

Upstream https://github.com/Sliverkiss/cpa-plugin `main` @ `3a039f9` (**v0.9.3**) plus a small fork patch, matching production binary `40bab45d…` (version stamp `0.9.3-wbprefix-20260915`).

Fork patch (public model-id prefix; internal snapshots/panel/cache keep raw ids):
- `models.go`: `modelPublicPrefix = "workbuddy-"` + `prefixModelIDs()`; applied in `handleModelStatic`/`handleModelForAuth` after `filterExcludedModels`; `resolveUpstreamModel` strips the prefix first.
- Tests updated accordingly (`models_test.go`, `configured_models_test.go`) + `workbuddy_model_prefix_test.go` (prefix idempotence, no input mutation, strip+alias).

Effect: every served model id is namespaced `workbuddy-*` (avoids collisions with other providers); clients must use prefixed names.

0.9.3 vs 0.8.5: dynamic per-auth model catalog (entitlements + models.dev metadata + last-good cache), `models` plugin config, readiness gate before execution, desensitization layer (off by default), Global expert trial.

Build: `go build -trimpath -buildmode=c-shared -ldflags "-X main.version=…" -o workbuddy.so .`. Runtime credentials excluded.
