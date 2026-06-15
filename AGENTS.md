# Repository Guidelines

## Project Structure & Module Organization
This repository is a single-package Go module (`github.com/sunkek/samsara`) with source files at the repository root (for example, `application.go`, `supervisor.go`, `health_server.go`).  
Tests currently live in `samsara_test.go`; add new tests as `*_test.go` files in the same package.  
Project metadata and contributor docs are also at root: `README.md`, `CONTRIBUTING.md`, `CHANGELOG.md`, and `SECURITY.md`. CI config is in `.github/workflows/ci.yml`.

## Build, Test, and Development Commands
Use the `Makefile` targets:

- `make test`: run fast unit tests (`go test ./...`).
- `make test-race`: run race-enabled tests 3 times (CI-equivalent).
- `make vet`: run `go vet ./...`.
- `make lint`: run `staticcheck ./...` (install separately).
- `make fmt`: format code with `gofmt -w -s .`.
- `make check`: full local gate (`fmt`, `vet`, `test-race`).
- `make tidy`: run `go mod tidy`.

For direct parity with CI, prefer `make check` before opening a PR.

## Coding Style & Naming Conventions
Follow standard Go formatting and idioms; `gofmt` is mandatory.  
Keep the package dependency-free unless a change is explicitly discussed first.  
Use clear, descriptive exported names (`NewSupervisor`, `WithHealthInterval`) and keep commit-sized changes focused.  
Commit subjects should be short, imperative, and under 72 characters (example: `fix shutdown race in supervisor`).

## Testing Guidelines
Concurrency safety is core to this project. Every change should include tests for behavior and shutdown/restart edge cases where relevant.  
Race detection is required: run `go test -race -count=3 -timeout=120s ./...` (or `make test-race`).  
Use Go test naming conventions (`TestXxx`) and prefer table-driven tests for policy/state-matrix scenarios.

## Commit & Pull Request Guidelines
For non-trivial changes, open an issue first to confirm direction.  
PRs should include:

- clear problem statement and rationale,
- tests that fail before and pass after,
- docs updates when public behavior/API changes.

Keep PR scope tight; avoid unrelated refactors. Reference issue IDs when relevant (for example, `(#42)`).

## Security & Reporting
Do not disclose vulnerabilities in public issues. Use GitHub Security Advisories (`Security -> Report a vulnerability`) as described in `SECURITY.md`.
