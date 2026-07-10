# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`samsara` is a zero-dependency Go library (single package, `github.com/sunkek/samsara`) for managing service component lifecycles — startup ordering, health monitoring, restart policies, and graceful shutdown. All source files live at the repo root alongside tests.

## Commands

```sh
make test        # go test ./...  (fast)
make test-race   # go test -race -count=3 -timeout=120s ./...  (CI-equivalent)
make vet         # go vet ./...
make lint        # staticcheck ./...  (install: go install honnef.co/go/tools/cmd/staticcheck@latest)
make fmt         # gofmt -w -s .
make check       # fmt + vet + test-race  (run this before any PR)
make tidy        # go mod tidy
```

Run a single test: `go test -run TestName ./...`

## Architecture

Three main types compose a running service:

- **`Application`** (`application.go`) — top-level entry point. Handles OS signals (`SIGINT`, `SIGTERM`, `SIGHUP`, `SIGQUIT`), wires an optional `Supervisor` and an optional `mainFunc` into a single `Run()` call, and enforces a `shutdownTimeout`.

- **`Supervisor`** (`supervisor.go`) — manages a set of `Component`s. Starts them sequentially (respecting `WithDependencies` ordering), polls `Health()` on an interval, applies restart policies on failure, and fires `EventHooks`. Exposes `HealthReportOrdered()` for status inspection.

- **`HealthServer`** (`health_server.go`) — a `Component` itself. Exposes `/livez`, `/readyz`, `/healthz` HTTP endpoints. Must be registered first (`sup.Add(hs)` before any other component) so it starts first and stops last, keeping orchestrators informed throughout the lifecycle. `/livez` is supervision-aware: it consults `Supervisor.Alive()` via the `LivenessReporter` interface and returns 503 once the supervisor enters a failure-driven shutdown.

Supporting types:

- **`Component`** interface (`component.go`) — `Name() string`, `Start(ctx, ready)`, `Stop(ctx)`. The contract: `Start` blocks for the component's entire lifetime; call `ready()` exactly once when truly able to serve; return `nil` on clean shutdown, non-nil on failure.
- **`RestartPolicy`** (`restart_policy.go`) — `NeverRestart`, `AlwaysRestart`, `MaxRetries`, `ExponentialBackoff` (with ±25% jitter).
- **`Tier`** (`component.go`) — `TierCritical` (default), `TierSignificant`, `TierAuxiliary`. Controls how a component's failure propagates to `/readyz` and whether it triggers a full shutdown.
- **`EventHooks`** (`hooks.go`) — `OnUnhealthy`, `OnRecovered`, `OnFailed`, `OnRestart`, plus lifecycle hooks `OnReady`, `BeforeStop`, `OnStopped`. All fire synchronously and must not block.
- **`MetricsObserver`** (`metrics.go`) — telemetry interface; implement to receive start/stop/health events without adding package dependencies. A concrete Prometheus/OTel implementation is intentionally **not** shipped here (would break zero-dep) — it lives in the separate `samsara-components` library.
- **`Logger`** (`logger.go`) — satisfied directly by `*slog.Logger`.
- **`testutil`** (`testutil/`) — separate package (stdlib-only) exporting `FakeComponent`, a configurable `Component`+`HealthChecker` for tests.

## Key invariants

- **Zero external dependencies** — do not introduce any; this is a hard constraint.
- **Race detector is non-negotiable** — every change must pass `go test -race -count=3 ./...`. The package manages concurrent goroutine lifecycles; races are the most likely bug class.
- **`Start` must return `nil` on clean shutdown** — returning `ctx.Err()` is treated as a crash by the supervisor.
- **`Stop` must be idempotent and concurrency-safe** — the supervisor may call it concurrently with an initialising `Start`.
- **`ready()` must be called only after the component can actually serve** — not after a lazy client construction that hasn't verified connectivity.
- Restart attempt counter resets after `WithRestartResetWindow` (default 5 min) of fault-free operation.
- **`statuses` map is built once in `Run` before any goroutine starts and is never structurally mutated** — `manage` reads `s.statuses[name]` without `statusMu`. Any feature that mutates the component set at runtime (e.g. dynamic add/remove) must first put every status access behind `statusMu`. This is why dynamic add/remove is deferred.
- **A confirmed fault (`onFault` in `supervisor.go`) covers both a failed health probe and an unexpected post-`ready()` `Start` exit** — both funnel through the same restart-policy + tier handling. With `WithHealthThreshold`, only the *confirmed* state (after K consecutive probes) drives `/readyz` and restarts; raw probes are still reported to the `MetricsObserver`.

## Testing

Tests live in `samsara_test.go` and topic files (`tier_a_test.go`, `tier_b_test.go`, `tier_c_test.go`, `parallel_test.go`); `testutil` has its own tests. Add new test files as `*_test.go` at the repo root. Prefer table-driven tests for policy/state-matrix scenarios. Every behavioural change needs a test that fails before the fix and passes after. Use `samsara/testutil.FakeComponent` for new tests instead of hand-rolled stubs where it fits.
