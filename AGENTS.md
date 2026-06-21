# Repository Guidelines

## Project Structure & Module Organization

This Go module is `github.com/t4db/t4`. Core database code lives in the repository root and `internal/` packages such as `store`, `wal`, `checkpoint`, `election`, `peer`, and `cli`. The standalone server entry point is `cmd/t4`; reusable public helpers are under `pkg/`, especially `pkg/object`. etcd-compatible APIs live in `etcd/`. Tests are split across root `*_test.go` files, package tests, and broader suites in `tests/integration`, `tests/e2e`, `tests/compat`, and `tests/apiserver`. Documentation is in `docs/` and mirrored into the Astro site under `website/`; deployment and operational assets are in `charts/`, `bench/`, and `jepsen/`.

## Build, Test, and Development Commands

- `go build ./cmd/t4` builds the CLI/server binary.
- `go test -short -timeout 360s -count=1 . ./cmd/t4 ./etcd ./etcd/auth ./internal/... ./pkg/object ./tests/compat` matches the main CI unit test set.
- `go test -short -timeout 360s -count=1 ./tests/integration` runs the short integration suite.
- `go test -race -timeout 360s -count=1 ./...` runs the full race-enabled Go suite.
- `go vet ./...` and `golangci-lint run` run static checks; golangci is configured by `.golangci.yml`.
- `make docs-generate` regenerates generated docs after CLI flag or config changes.
- `make test-apiserver` runs Kubernetes apiserver storage tests.
- `cd website && npm ci && npm run build` verifies documentation site changes.

## Coding Style & Naming Conventions

Use standard Go formatting (`gofmt`/`go fmt`) and tabs for indentation. Keep package names short, lowercase, and domain-specific. Use exported identifiers only for public API surface and document them when they are intended for users. Prefer context-aware APIs, explicit error returns, and existing package boundaries over new cross-package shortcuts.

## Testing Guidelines

Place unit tests beside the code as `*_test.go`; name tests `TestXxx`, benchmarks `BenchmarkXxx`, and examples `ExampleXxx`. Use `testing.Short()` for expensive scenarios so CI short runs stay predictable. E2E tests that require MinIO or external services belong under `tests/e2e` and should be guarded by environment flags such as `T4_E2E_MINIO`.

## Commit & Pull Request Guidelines

Recent history uses concise, imperative commit subjects such as `Fix the Pebble shutdown panic` or `Return ErrNoLeader instead of raw dial error`; keep subjects focused on behavior. Pull requests should describe the change, link related issues when applicable, list the commands run, and call out docs, config, storage-format, or compatibility impacts.

## Security & Configuration Tips

Never commit credentials, TLS keys, or object-store secrets. Prefer `T4_*` environment variables for local configuration, and update generated docs when adding or changing CLI flags or config fields.
