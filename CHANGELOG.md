# Changelog

All notable changes to samsara are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
samsara uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

---

## [0.5.1] — 2026-07-09

### Fixed
- `Application.Run` no longer closes the internal error channel during shutdown.
  On the shutdown-timeout path the main and supervisor goroutines can still be
  alive and send an error after `Run` proceeds; closing the channel turned that
  late send into a send-on-closed-channel panic. Errors are now drained
  non-blockingly (the channel is buffered to its sender count).
- `ExponentialBackoff` now caps its pre-jitter delay at 1 hour. Previously
  `baseDelay << attempt` overflowed the int64 nanosecond duration at high
  attempt counts, wrapping to a negative/zero delay and spinning a hot restart
  loop. The delay stays positive and bounded regardless of attempt count.
- `Supervisor` restart-wait branches use an explicit stopped timer instead of
  `time.After`, so a context cancellation during the restart delay no longer
  leaks a pending timer until it fires.

---

## [0.5.0] — 2026-06-15

### Added
- `WithParallelStartStop` supervisor option. When enabled, components start and
  stop concurrently, constrained only by the dependency graph declared via
  `WithDependencies`: a component starts once all its dependencies have signalled
  `ready()`, and stops only after all its dependents have stopped. Components
  with no edge between them run concurrently. The default remains strictly
  sequential startup in registration order with exact reverse-order shutdown.

---

## [0.4.0] — 2026-04-04

### Changed
- **Breaking:** Package renamed from `frame` to `samsara`. Update your import
  path to `github.com/sunkek/samsara`.

### Added
- `RestartCount` field on `ComponentStatus` — counts how many times the
  supervisor has restarted a component, visible in `HealthReport()`,
  `HealthReportOrdered()`, and the `/healthz` JSON response.
- `restart_count` field in `/healthz` JSON (omitted when zero).
- Lifecycle contract formally documented on the `Component` interface —
  specifies `ready()` semantics, `Stop()` idempotency, background goroutine
  ownership, and cancellation return values.

### Fixed
- OS signal shutdown (Ctrl+C) incorrectly returned a non-nil error and logged
  "interrupt signal received". Clean signal shutdown now returns `nil` and
  logs "application stopped cleanly".

---

## [0.3.3] — 2026-04-03

### Fixed
- Startup loop race: when `ctx.Done()` and `readyCh` fired simultaneously
  during component startup, the in-flight component could be omitted from the
  `started` slice, causing `stopAll` to skip it. The component would start but
  never receive a `Stop` call on shutdown.
- `startOne` no longer calls `doStop` in its `ctx.Done()` branch — that
  responsibility belongs exclusively to `stopAll` in `Run`, which now always
  includes the in-flight component.

### Added
- `RestartCount` tracked in `manage()` loop (internal, surfaced in 0.4.0).
- `WithHealthName` option on `HealthServer` for registering multiple health
  server instances.
- `Tier` stored directly on `healthStatus` to remove the `s.components` map
  lookup from the health reporting path.

---

## [0.3.0] — 2026-04-02

### Changed
- **Breaking:** `Component.Start` signature changed from `Start(ctx) error`
  to `Start(ctx, ready func()) error`. The `ready` function replaces the
  `Starter` interface, eliminating a fundamental race condition where
  `Started()` returned a channel that `Start()` could overwrite concurrently.
- **Breaking:** `Starter` interface removed.
- `startProbeWindow` option removed (was only a workaround for the `Starter`
  race).

### Added
- `WithHealthName` on `HealthServer`.
- `HealthServer.Name()` is now configurable (defaults to `"health-server"`).

### Fixed
- Race between `Started()` and `Start()` that caused first-attempt startup
  timeouts in practice.

---

## [0.2.0] — 2026-04-01

### Added
- `HealthServer` component with `/livez`, `/readyz`, `/healthz` endpoints.
- `HealthReporter` interface — `*Supervisor` satisfies it.
- `HealthReportOrdered()` returning a name-sorted `[]NamedComponentStatus`.
- `Tier` stored on `ComponentStatus` for downstream consumers.
- `EventHooks`: `OnUnhealthy`, `OnRecovered`, `OnFailed`, `OnRestart`.
- `MetricsObserver` interface with nop default.
- `ExponentialBackoff` restart policy with ±25% jitter.
- `context.WithCancelCause` throughout for precise failure attribution.
- Topological dependency sort preserving insertion order for determinism.
- `stopSig()` called immediately on shutdown so a second Ctrl+C kills the
  process rather than being swallowed.

---

## [0.1.0] — 2026-03-30

### Added
- Initial release.
- `Component` interface: `Name()`, `Start(ctx)`, `Stop(ctx)`.
- `Supervisor` with sequential dependency-ordered startup and reverse-order
  shutdown.
- `WithDependencies` for declaring startup ordering constraints.
- `TierCritical`, `TierSignificant`, `TierAuxiliary`.
- `NeverRestart`, `AlwaysRestart`, `MaxRetries` restart policies.
- `WithRestartResetWindow` for resetting the restart counter after stable
  operation.
- `Application` wiring OS signal handling, supervisor, and optional main
  function into a single `Run()` call.
- `Logger` interface compatible with `*slog.Logger`.
- Zero external dependencies.

[Unreleased]: https://github.com/sunkek/samsara/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/sunkek/samsara/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/sunkek/samsara/compare/v0.3.3...v0.4.0
[0.3.3]: https://github.com/sunkek/samsara/compare/v0.3.0...v0.3.3
[0.3.0]: https://github.com/sunkek/samsara/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/sunkek/samsara/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/sunkek/samsara/releases/tag/v0.1.0
