# Plugin source provenance

Synced: 2026-10-29 (production-matched snapshot, CPA kernel v8.0.46-patched)

| Plugin | Source origin | Source revision | Production binary SHA-256 (prefix) |
|---|---|---|---|
| ZCode | production-matched source tree (official-verify-claim build, 2026-09-12) | `b91025f6` deploy tree + solver `captcha-happy.ts` 2026-09-05 | `f9951d6cf3330b7e1c1fc9958c2d7e3bc459829c6b430ba47e19050bdb6e1a33` |
| B.AI | production-matched source tree (zero-balance 402 + `/status`, 2026-09-15) | rebuild verified byte-identical with go1.27 linux/amd64 | `35cc97b1369c4c2cc47acc3ede280d910a1d314da23fc9c912a4e50c8b846cdf` |
| QoderWork | production-matched source tree (`/status` + tools passthrough era, 2026-09-15) | fork of upstream `qoderwork` v0.4.1 (`f2f1b77d`) + own patches; rebuild verified byte-identical | `44bffae3b813cb109e8841f205a43d2a796d18f03fb4a8d8a9093d2d95ae6a26` |
| TRAE Work | first-party plugin source (no public upstream), 2026-09-15 | includes 9074 check-in retry, panel overflow fixes, `/status`; rebuild verified byte-identical | `03ed39875205604803b9b903f271b494a139efce5691632a040677d0820ce152` |
| WorkBuddy | production-matched source tree (0.12.35-lock-repaint-20261014) | version stamp `0.12.35-lock-repaint-20261014`, includes lock repaint, dynamic task routing, pool guarding and full panel test suite (51 tests) | `892e47b11b5ebd671f6d36d7495141ca3b9d88e3e85cdd7377c2cb215e3edd22` |
| PrivacyFilter | https://github.com/rheodev/cpa-plugin-privacyfilter | tag `v0.2.0`, commit `a3db9d1f951d6d34cf7f00029cb71ff46a600d1f` | `94f4798ae1eb76a75681bb8dd04ed81971243fea8ba1b9f7d66139518d53e95b` |

## Verification performed on the production build host (2026-10-29)

- `gofmt -l`, `go vet`, `go test -count=1`: all plugin trees green.
- WorkBuddy Node panel test suite: 51 tests pass.
- `CGO_ENABLED=1 go build -buildmode=c-shared`: all plugins verified compatible with CPA kernel v8.0.46 ABI (SchemaVersion 7 backward-compatible negotiation).

## Publication sanitization (source intentionally differs from production literals)

- `zcode/plugin/util.go` / `zcode/plugin/auth_test.go` / `traework/login_test.go`: private deployment origin replaced with the placeholder `https://cpa.example.com`. Before building for your own deployment, set `oauthFirstPartyOrigin` to your public panel origin (ZCode OAuth callback Origin allow-list).
- Test fixtures elsewhere use invalid/example values.

## Interpretation

- "Production-matched" means the source snapshot selected for the deployed plugin. It does **not** claim bit-for-bit reproducibility unless the toolchain, SDK revision and build directory are also pinned (see verification notes above for which plugins are byte-identical).
- Runtime auth files, logs, `.so` files, generated bundles and dependencies are intentionally excluded.
- Upstream licenses remain in the corresponding vendored plugin directories.
- The 2026-09-15 snapshot's digests were superseded by this sync; digests above refer to the deployed production binaries at sync time.
