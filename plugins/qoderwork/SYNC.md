# QoderWork plugin

Production-matched source as of 2026-09-15 (binary `44bffae3…`), rebuild byte-identical.

Fork of upstream `Sliverkiss/cpa-plugin` qoderwork v0.4.1 (`f2f1b77d`) with own patches: passwordless public POST panel actions + CSRF + claim-pro confirmation, client `tools`/`tool_choice` passthrough, `tool_call_id` restoration for multi-turn histories, non-stream pure `tool_calls` responses, and `GET /v0/resource/plugins/qoderwork/status`. `VERSION` intentionally still records the fork baseline (0.4.1); production builds are stamped `dev`.

Runtime OAuth/auth files are excluded. Build: `go build -buildmode=c-shared -o qoderwork.so .`.
