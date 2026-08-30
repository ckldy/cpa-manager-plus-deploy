# Plugin source provenance

Synced: 2026-08-30

| Plugin | Source origin | Source revision | Sanitized tree SHA-256 | Production binary SHA-256 |
|---|---|---|---|---|
| ZCode | production-matched local source snapshot | plugin 0.6.11 | `d237997d6c3fd65befaa5d4efe281c28b756ba968c47805570bd3fa144b40f38` | `94a709a8c78a7a5fd03dde8569d5c54b11a32ee4a6fc0489e9ed33a528a07410` |
| B.AI | production-matched local source snapshot | plugin 0.1.0 | `40e32fa4dc2414eefc7089ceed79fcc450918c7cf75ba5ac6913e7f3c12eaba7` | `866babe628ca0ffd7260d8bcba61747ef0d48b2ce5625710f37e2a35157f756d` |
| QoderWork | production-matched local source snapshot | production `dev` build | `33f7ed4797f14a26fb044b6295398640f40d783c12892a8e36193ad63fc4c92b` | `00498524edf07e7a0fa79a18d4de2f0159f913d04064d14a2a09260f86de5a9d` |
| WorkBuddy | https://github.com/Sliverkiss/cpa-plugin/tree/main/workbuddy | `0c4cb3bff09b62af124d1452341861a63c4c5549` (0.8.5) | `f61c39b8650c8e92cebcea503b5c89c9752c99090d54ebf69d88364a27cb9945` | `2a232d33c6323fc65883f9cd738e80d2fb492f30be3c1eb9c68774042c183616` |
| PrivacyFilter | https://github.com/rheodev/cpa-plugin-privacyfilter | tag `v0.2.0`, commit `a3db9d1f951d6d34cf7f00029cb71ff46a600d1f` | `34887ddd184b1567a318d20d37ebee09471c3ea77036cf816534dec2afaa77e2` | `94f4798ae1eb76a75681bb8dd04ed81971243fea8ba1b9f7d66139518d53e95b` |

## Interpretation

- “Production-matched” means the source snapshot selected for the deployed plugin. It does **not** claim bit-for-bit reproducibility unless the toolchain, SDK revision and linker metadata are also pinned.
- Runtime auth files, logs, `.so` files, generated bundles and dependencies are intentionally excluded.
- Upstream licenses remain in the corresponding vendored plugin directories.
- Test credentials were replaced with unmistakably invalid fixtures before publication; this can change the sanitized tree digest without changing production behavior.
