# B.AI plugin

Production-matched source as of 2026-09-15 (binary `0081a398…`), rebuild byte-identical.

Includes free-model `(free)` labelling, upstream health probing, model fallback with automatic downgrade on insufficient balance, zero-balance fast 402 (no 500 bubbling) on all three execution paths (non-stream / stream / host-callback stream), and `GET /v0/resource/plugins/bai/status`.

No credentials are included. Build: `go build -buildmode=c-shared -o bai.so .` (go ≥ 1.26, CGO enabled).
