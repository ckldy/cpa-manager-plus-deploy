# CPA plugins

Each production plugin has an independent directory. Source trees contain no runtime auth files or compiled `.so` artifacts.

| Directory | Production version / hash prefix | Source |
|---|---|---|
| `zcode/` | official-verify-claim build / `b91025f6` | production-matched source + isolated solver |
| `bai/` | 0.1.0 / `0081a398` | production-matched source (zero-balance 402, `/status`) |
| `qoderwork/` | dev @ 0.4.1 base / `44bffae3` | production-matched source (panel public POST, tools passthrough, `/status`) |
| `traework/` | dev / `03ed3987` | first-party source (check-in retry, `/status`, panel fixes) |
| `workbuddy/` | 0.9.3 + `workbuddy-` prefix patch / `40bab45d` | upstream `Sliverkiss/cpa-plugin` main `3a039f9` + fork patch (4 files) |
| `privacyfilter/` | 0.2.0 / `94f4798a` | official upstream tag `v0.2.0` |

Hashes identify audited production binaries; binaries are not committed. Rebuilds may differ unless the toolchain and build directory are identical.

See `SOURCE-MANIFEST.md` for provenance and full digests.
