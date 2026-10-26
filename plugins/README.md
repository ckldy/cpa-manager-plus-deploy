# CPA plugins

Each production plugin has an independent directory. Source trees contain no runtime auth files or compiled `.so` artifacts.

| Directory | Production version / hash prefix | Source |
|---|---|---|
| `zcode/` | official-verify-claim build / `f9951d6c` | production-matched source + isolated solver |
| `bai/` | 0.1.0 / `35cc97b1` | production-matched source (zero-balance 402, `/status`) |
| `qoderwork/` | dev @ 0.4.1 base / `44bffae3` | production-matched source (panel public POST, tools passthrough, `/status`) |
| `traework/` | dev / `03ed3987` | first-party source (check-in retry, `/status`, panel fixes) |
| `workbuddy/` | 0.12.35 / `892e47b1` | production-matched source (0.12.35-lock-repaint-20261014, 51 panel tests) |
| `privacyfilter/` | 0.2.0 / `94f4798a` | official upstream tag `v0.2.0` |

Hashes identify audited production binaries; binaries are not committed. Rebuilds may differ unless the toolchain and build directory are identical. All plugins are fully compatible with CPA kernel v8.0.44 ABI (SchemaVersion 7).

See `SOURCE-MANIFEST.md` for provenance and full digests.
