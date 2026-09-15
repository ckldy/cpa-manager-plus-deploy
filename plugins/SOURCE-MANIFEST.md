# Plugin source provenance

Synced: 2026-09-15 (production-matched snapshot)

| Plugin | Source origin | Source revision | Production binary SHA-256 (prefix) |
|---|---|---|---|
| ZCode | production-matched source tree (official-verify-claim build, 2026-09-12) | `b91025f6` deploy tree + solver `captcha-happy.ts` 2026-09-05 | `b91025f60ae9a0b359fcf1aaa637a468c6097d0dacb3d071989c1fa574acb1b9` |
| B.AI | production-matched source tree (zero-balance 402 + `/status`, 2026-09-15) | rebuild verified byte-identical with go1.27 linux/amd64 | `0081a3980a575c6f1ea2e361664f0839fc6d26ee22f304f18877933136dc58cf` |
| QoderWork | production-matched source tree (`/status` + tools passthrough era, 2026-09-15) | fork of upstream `qoderwork` v0.4.1 (`f2f1b77d`) + own patches; rebuild verified byte-identical | `44bffae3b813cb109e8841f205a43d2a796d18f03fb4a8d8a9093d2d95ae6a26` |
| TRAE Work | first-party plugin source (no public upstream), 2026-09-15 | includes 9074 check-in retry, panel overflow fixes, `/status`; rebuild verified byte-identical | `03ed39875205604803b9b903f271b494a139efce5691632a040677d0820ce152` |
| WorkBuddy | https://github.com/Sliverkiss/cpa-plugin `main` + fork prefix patch | main `3a039f9` (v0.9.3) + 4 patched files (`models.go`, `models_test.go`, `configured_models_test.go`, `workbuddy_model_prefix_test.go`); version stamp `0.9.3-wbprefix-20260915` | `40bab45d22bc468f745fa60c225799f13fbdb8b6c89f56b349901f2aa12937ff` |
| PrivacyFilter | https://github.com/rheodev/cpa-plugin-privacyfilter | tag `v0.2.0`, commit `a3db9d1f951d6d34cf7f00029cb71ff46a600d1f` | `94f4798ae1eb76a75681bb8dd04ed81971243fea8ba1b9f7d66139518d53e95b` |

## Verification performed on the production build host (2026-09-15)

- `gofmt -l`, `go vet`, `go test -count=1`: all plugin trees green.
- `CGO_ENABLED=1 go build -buildmode=c-shared`: **bai, qoderwork, traework rebuilt byte-for-byte identical to the deployed `.so`**. WorkBuddy's deployed binary was built in the same tree. ZCode rebuild differs only in embedded build path (no `.go` file in the tree is newer than the deployed artifact), so it is production-matched rather than bit-for-bit reproducible.

## Publication sanitization (source intentionally differs from production literals)

- `zcode/plugin/util.go` / `zcode/plugin/auth_test.go` / `traework/login_test.go`: private deployment origin replaced with the placeholder `https://cpa.example.com`. Before building for your own deployment, set `oauthFirstPartyOrigin` to your public panel origin (ZCode OAuth callback Origin allow-list).
- Test fixtures elsewhere use invalid/example values.

## Interpretation

- "Production-matched" means the source snapshot selected for the deployed plugin. It does **not** claim bit-for-bit reproducibility unless the toolchain, SDK revision and build directory are also pinned (see verification notes above for which plugins are byte-identical).
- Runtime auth files, logs, `.so` files, generated bundles and dependencies are intentionally excluded.
- Upstream licenses remain in the corresponding vendored plugin directories.
- The 2026-08-30 snapshot's tree digests were superseded by this sync; digests above refer to the deployed production binaries at sync time.
