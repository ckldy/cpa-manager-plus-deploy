# CPA plugins

Each production plugin has an independent directory. Source trees contain no runtime auth files or compiled `.so` artifacts.

| Directory | Production version/hash prefix | Source |
|---|---|---|
| `zcode/` | 0.6.11 / `94a709a8` | production-matched source + isolated solver |
| `bai/` | 0.1.0 / `866babe6` | production-matched source |
| `qoderwork/` | dev / `00498524` | production-matched source |
| `workbuddy/` | 0.8.5 / `2a232d33` | official upstream source |
| `privacyfilter/` | 0.2.0 / `94f4798a` | official upstream tag |

Hashes identify audited production binaries; binaries are not committed. Rebuilds may differ unless the toolchain and linker metadata are identical.
