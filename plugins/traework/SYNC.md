# TRAE Work plugin

First-party CLIProxyAPI plugin (no public upstream repository). Production-matched source as of 2026-09-15, binary `03ed39875205604803b9b903f271b494a139efce5691632a040677d0820ce152`.

Capabilities: TRAE Work OAuth login + local callback, model catalog (35+ models, dynamic refresh with bounded cache), credits (SOLO pool), automatic check-in with upstream rate-limit (code 9074) retry/backoff, chat/stream execution with tool-call passthrough, panel with public POST actions (no admin key), `GET /v0/resource/plugins/traework/status` health endpoint.

Sanitized for publication: origin literals in `login_test.go` use `cpa.example.com` fixtures.

Build: `go build -buildmode=c-shared -o traework.so .` (go ≥ 1.26, CGO enabled). Runtime auth files are not included.
