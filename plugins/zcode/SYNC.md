# ZCode plugin

`plugin/` contains the production-matched Go source (official-verify-claim build of 2026-09-12, binary `b91025f6…`; off-peak ticket-id status match included; rebuild is production-matched, not bit-for-bit due to embedded build path). `solver/` is the isolated Node captcha solver (multi-stage Dockerfile builds the CJS bundle; sources synced to the 2026-09-05 production solver revision).

Publication sanitization: `oauthFirstPartyOrigin` in `plugin/util.go` uses the placeholder `https://cpa.example.com` — set it to your own public panel origin before building.

All high-risk switches are disabled in the public template (`allow_paid_fallback`, `client_signing_enabled`, `dynamic_routing_active`, `off_peak_enabled`, `start_plan_auto_claim`, `captcha_solver_enabled`, `retain_dual_credentials`); enable deliberately after reviewing each feature's behaviour. Production currently runs the same feature set with signing/off-peak/dual-credential enabled.

Build: `cd plugin && go build -buildmode=c-shared -o zcode.so .` (go ≥ 1.26, CGO enabled). No credentials included.
