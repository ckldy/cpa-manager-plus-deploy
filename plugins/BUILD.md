# Plugin build and release workflow

## Go plugins

Run inside the plugin source directory:

```bash
gofmt -w *.go
go test ./...
go vet ./...
go build -trimpath -buildmode=c-shared -o <plugin>.so .
sha256sum <plugin>.so
```

ZCode uses `plugins/zcode/plugin` as its Go module. The other Go plugins use their plugin directory directly.

The build host used for the audited production-matched snapshots was Linux/amd64 with Go 1.27. Build compatibility still depends on the CLIProxyAPI v7 plugin SDK revision selected by each `go.mod`/`go.sum`.

## ZCode solver

The repository does not commit `node_modules` or the generated CJS bundle. Compose builds it from `plugins/zcode/solver/Dockerfile`:

```bash
docker compose build --no-cache zcode-solver
docker compose up -d zcode-solver
```

The solver has no published host port. Keep `captcha_solver_enabled: false` unless the full claim path and rollback have been reviewed.

## Release rules

1. Work on a branch; never synchronize plugin trees directly to `main`.
2. Pin upstream plugin revisions in `SOURCE-MANIFEST.md`.
3. Replace only synthetic test secrets; never copy runtime auth files.
4. Run focused tests and secret scanning on the staged tree.
5. Commit each independently releasable plugin separately.
6. Push the branch and merge through a pull request.
7. Do not publish `.so` artifacts in the source commit. Attach audited binaries to a versioned release only when needed, with full SHA-256 checksums.
