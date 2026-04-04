# samsara

A small, explicit lifecycle runtime for Go services — zero external dependencies.

The name comes from the concept of cyclical existence: components fail, are restarted, and return to service. `samsara` makes that cycle explicit, controlled, and observable — rather than something that happens ad hoc in `main.go`.

`samsara` coordinates the startup, health monitoring, graceful shutdown, and orchestrator integration of your service's infrastructure components. It handles the questions every production service eventually faces: what order do dependencies start? what happens when Redis dies at 3am? when should the pod stop receiving traffic?

```sh
go get github.com/sunkek/samsara
```

---

## Core concept

Everything your service depends on is a **Component**:

```go
type Component interface {
    Name() string
    Start(ctx context.Context, ready func()) error
    Stop(ctx context.Context) error
}
```

`Start` blocks for the component's entire lifetime. Call `ready()` exactly once when the component can serve traffic. `Stop` unblocks `Start` and cleans up.

---

## Lifecycle contract

This section is the authoritative specification. Read it before implementing a component.

### `Start(ctx, ready)`

- **Must block** until the component exits — do not return early while the component is still serving.
- Call `ready()` **exactly once**, only when the component is actually able to serve its intended function — not before a connection is verified, not before a port is bound.
- `ready()` is safe to call multiple times (idempotent internally), but should only be called once semantically.
- If `ctx` is cancelled (clean shutdown), return `nil`.
- Return a **non-nil error only on abnormal failure** — not on clean context cancellation.
- If `ready()` is never called, the supervisor waits up to `startTimeout` (default 15s) and treats the attempt as a failure.

### `Stop(ctx)`

- Must unblock `Start` and release resources within the context deadline (`stopTimeout`, default 10s).
- Must be **idempotent** — the supervisor may call `Stop` more than once in some shutdown paths.
- Must be **concurrency-safe** — `Stop` may be called concurrently with a still-initialising `Start` (e.g. before a port is bound). Guard shared state accordingly.

### Background goroutines

If `Start` spawns background goroutines, they must exit when `ctx` is cancelled or `Stop` is called. The supervisor has no way to detect or reap leaked goroutines. A leaked goroutine from a restarted component will accumulate across restart cycles.

### Cancellation semantics

| Event | `Start` should return | `Stop` is called |
|---|---|---|
| Clean shutdown (signal / `Shutdown()`) | `nil` | Yes |
| Component failure | non-nil error | No (already exited) |
| Restart due to health failure | `nil` (Stop unblocks it) | Yes, before restart |

---

## Minimal example

```go
func main() {
    app := samsara.NewApplication(
        samsara.WithMainFunc(func(ctx context.Context) error {
            log.Println("running — press Ctrl+C to stop")
            <-ctx.Done()
            return nil
        }),
    )
    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

`ctx` is cancelled automatically on `SIGINT`, `SIGTERM`, `SIGHUP`, or `SIGQUIT`. A clean signal shutdown returns `nil` — exit code 0.

---

## Real-world example

```go
func main() {
    logger := slog.Default()

    sup := samsara.NewSupervisor(
        samsara.WithSupervisorLogger(logger),
        samsara.WithHealthInterval(10 * time.Second),
        samsara.WithEventHooks(&samsara.EventHooks{
            OnUnhealthy: func(component string, err error) {
                logger.Error("component unhealthy", "component", component, "error", err)
            },
            OnRecovered: func(component string) {
                logger.Info("component recovered", "component", component)
            },
            OnFailed: func(component string, err error) {
                logger.Error("component permanently failed", "component", component, "error", err)
            },
        }),
    )

    // Register HealthServer FIRST — it starts before all other components
    // and stops LAST, keeping orchestrators informed throughout.
    hs := samsara.NewHealthServer(sup, samsara.WithHealthAddr(":8080"))
    sup.Add(hs)

    sup.Add(postgres.New(cfg.Postgres),
        samsara.WithTier(samsara.TierCritical),
        samsara.WithRestartPolicy(samsara.ExponentialBackoff(5, time.Second)),
    )
    sup.Add(redis.New(cfg.Redis),
        samsara.WithTier(samsara.TierCritical),
        samsara.WithRestartPolicy(samsara.ExponentialBackoff(5, time.Second)),
    )
    sup.Add(s3.New(cfg.S3),
        samsara.WithTier(samsara.TierSignificant),
        samsara.WithRestartPolicy(samsara.AlwaysRestart(5*time.Second)),
    )
    // HTTP server starts only after postgres and redis are ready.
    sup.Add(httpserver.New(cfg.HTTP),
        samsara.WithTier(samsara.TierCritical),
        samsara.WithRestartPolicy(samsara.MaxRetries(3, 2*time.Second)),
        samsara.WithDependencies("postgres", "redis"),
    )

    app := samsara.NewApplication(
        samsara.WithSupervisor(sup),
        samsara.WithLogger(logger),
        samsara.WithShutdownTimeout(30*time.Second),
    )
    if err := app.Run(); err != nil {
        logger.Error("application exited with error", "error", err)
        os.Exit(1)
    }
}
```

---

## Component tiers

Tiers define how a component's health affects the rest of the application.

| Tier | Transient unhealthy | Permanent failure |
|---|---|---|
| `TierCritical` (default) | App shuts down | App shuts down |
| `TierSignificant` | `/readyz` → 503, app stays up | App shuts down |
| `TierAuxiliary` | Logged only, no effect | Component removed, app continues |

**Use `TierSignificant`** for components that degrade — but don't break — your service (e.g. a cache, a metrics sink). The app keeps running and the load balancer is informed via `/readyz`.

**Use `TierAuxiliary`** for components whose failure is entirely non-blocking (e.g. an audit log sink, a tracing exporter).

### Failure model

Each component goes through these states:

```
          start failure
               │
[Starting] ───────────────► [Failed] ─── if restart policy allows ──► [Starting]
               │                                                            ↑
           ready() called                                                   │
               │                                                      stop + delay
           [Running]                                                        │
               │                                                            │
        health check fails ──────────────────────────────────────────────────
               │
        restart policy exhausted
               │
           [Permanently Failed]
               │
        TierCritical/Significant ──► shutdown
        TierAuxiliary            ──► removed from monitoring, app continues
```

Recovery is automatic: if a restarted component calls `ready()` and health checks pass, `OnRecovered` fires and `/readyz` flips back to 200.

---

## Restart policies

```go
samsara.NeverRestart()                         // fail once → permanent (default)
samsara.AlwaysRestart(2*time.Second)           // retry forever, fixed delay
samsara.MaxRetries(5, time.Second)             // up to 5 retries, fixed delay
samsara.ExponentialBackoff(5, time.Second)     // 1s, 2s, 4s, 8s, 16s (±25% jitter)
```

The attempt counter resets to zero if the component runs without fault for longer than `WithRestartResetWindow` (default 5 minutes).

### When to use restarts vs when to crash

Use internal restarts for components whose failure is transient and independent — a cache client that loses its connection, a queue consumer that gets disconnected, a metrics exporter. These are safe to restart because their failure doesn't affect application correctness.

Be cautious restarting core request-path components (the primary HTTP server, the main DB pool). A flapping critical component can create a misleading "alive but broken" state. If a component fails repeatedly, consider whether the correct response is to restart it or to crash the pod and let the orchestrator restart the whole process from a clean state.

---

## Health checking

Implement `HealthChecker` to participate in health polling:

```go
type HealthChecker interface {
    Health(ctx context.Context) error
}
```

The supervisor calls `Health` every `WithHealthInterval` (default 10s). A non-nil result triggers the tier logic above. Health checks are bounded by `WithHealthTimeout` (default 5s).

---

## Health endpoints

`HealthServer` exposes three HTTP endpoints for orchestrators:

| Endpoint | 200 when | Use for |
|---|---|---|
| `GET /livez` | Process is alive | Kubernetes `livenessProbe` |
| `GET /readyz` | All Critical + Significant components healthy | Kubernetes `readinessProbe` |
| `GET /healthz` | Same as `/readyz` | Docker `HEALTHCHECK` |

**`/readyz` flips during startup** — it returns 503 until every Critical and Significant component has called `ready()` and passed its first health check.

**`/readyz` flips during shutdown** — the HealthServer stops last, so it returns 503 as soon as shutdown begins, before any other component stops. This drains load balancer connections cleanly.

**Recovery** — if a degraded component recovers, `/readyz` returns 200 again automatically.

`/readyz` response body:
```json
{
  "status": "degraded",
  "components": [
    { "name": "postgres",    "status": "ok",       "restart_count": 0 },
    { "name": "redis",       "status": "degraded", "error": "connection refused", "restart_count": 2 },
    { "name": "http-server", "status": "ok",       "restart_count": 0 }
  ]
}
```

`restart_count` is omitted from JSON when zero.

Docker example:
```dockerfile
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD wget -qO- http://localhost:8080/healthz || exit 1
```

Register `HealthServer` first:
```go
hs := samsara.NewHealthServer(sup,
    samsara.WithHealthAddr(":8080"),
    samsara.WithHealthLogger(logger),
)
sup.Add(hs) // always first
```

---

## Dependency ordering

Components start sequentially in registration order. Use `WithDependencies` when a component must not start until another is ready:

```go
sup.Add(postgres.New(cfg))
sup.Add(redis.New(cfg))
sup.Add(httpServer,
    samsara.WithDependencies("postgres", "redis"),
)
```

`httpServer` starts only after both `postgres` and `redis` have called `ready()`. On shutdown, components stop in reverse start order — `httpServer` stops before `postgres` and `redis`, so no in-flight requests touch a closed pool.

---

## Pitfalls and best practices

### Call `ready()` too early

```go
// ❌ Wrong — ready() before the connection is verified
func (c *Cache) Start(ctx context.Context, ready func()) error {
    c.client = redis.NewClient(opts) // lazy — no connection yet
    ready()                          // supervisor proceeds, but cache may be broken
    <-ctx.Done()
    return nil
}

// ✅ Right — ready() only after a successful ping
func (c *Cache) Start(ctx context.Context, ready func()) error {
    c.client = redis.NewClient(opts)
    if err := c.client.Ping(ctx).Err(); err != nil {
        return err
    }
    ready()
    <-ctx.Done()
    return nil
}
```

### Forget to unblock Start on Stop

```go
// ❌ Wrong — Stop closes the client but Start blocks forever
func (s *Server) Start(ctx context.Context, ready func()) error {
    ready()
    for job := range s.jobs { process(job) } // blocks; Stop never unblocks this
    return nil
}

// ✅ Right — Stop signals Start to exit
func (s *Server) Start(ctx context.Context, ready func()) error {
    s.stop = make(chan struct{})
    ready()
    select {
    case <-s.stop:
    case <-ctx.Done():
    }
    return nil
}
func (s *Server) Stop(_ context.Context) error {
    close(s.stop)
    return nil
}
```

### Leak goroutines after Stop

```go
// ❌ Wrong — background goroutine outlives the component
func (w *Worker) Start(ctx context.Context, ready func()) error {
    go w.runLoop() // no way to stop this
    ready()
    <-ctx.Done()
    return nil
}

// ✅ Right — goroutine exits when ctx is cancelled
func (w *Worker) Start(ctx context.Context, ready func()) error {
    go w.runLoop(ctx) // ctx cancellation stops the loop
    ready()
    <-ctx.Done()
    return nil
}
```

### Return non-nil error on clean shutdown

```go
// ❌ Wrong — ctx.Err() looks like a failure to the supervisor
func (s *Server) Start(ctx context.Context, ready func()) error {
    ready()
    <-ctx.Done()
    return ctx.Err() // returns context.Canceled — treated as a crash
}

// ✅ Right
func (s *Server) Start(ctx context.Context, ready func()) error {
    ready()
    <-ctx.Done()
    return nil
}
```

---

## Runtime status

Inspect all component states at any time via the supervisor:

```go
for _, status := range sup.HealthReportOrdered() {
    fmt.Printf("%-20s healthy=%-5v restarts=%d\n",
        status.Name,
        status.Err == nil,
        status.RestartCount,
    )
}
```

`ComponentStatus` fields:

| Field | Type | Description |
|---|---|---|
| `Err` | `error` | `nil` = healthy; non-nil = last health check error |
| `Known` | `bool` | `false` until first health check completes |
| `Tier` | `Tier` | `TierCritical`, `TierSignificant`, or `TierAuxiliary` |
| `RestartCount` | `int` | How many times the supervisor has restarted this component |

---

## Metrics integration

Implement `MetricsObserver` to receive telemetry without adding dependencies to the package:

```go
type MetricsObserver interface {
    ComponentStarted(component string, attempt int)
    ComponentStopped(component string, err error)
    ComponentRestarting(component string, err error, attempt int, delay time.Duration)
    HealthCheckCompleted(component string, duration time.Duration, err error)
}
```

---

## Programmatic shutdown

```go
app.Shutdown(errors.New("config reload required"))
// cause is available inside Start/main via context.Cause(ctx)
```

`app.Shutdown(nil)` is a clean shutdown — same semantics as Ctrl+C, returns nil from `app.Run()`.

---

## Logger interface

`samsara.Logger` is satisfied directly by `*slog.Logger` and most structured loggers:

```go
samsara.WithLogger(slog.Default())
samsara.WithSupervisorLogger(slog.Default())
samsara.WithHealthLogger(slog.Default())
```

---

## Configuration reference

### Supervisor

| Option | Default | Description |
|---|---|---|
| `WithHealthInterval` | 10s | How often to poll `Health()` |
| `WithStartTimeout` | 15s | How long to wait for `ready()` to be called |
| `WithHealthTimeout` | 5s | Deadline for each `Health()` call |
| `WithStopTimeout` | 10s | Deadline for each `Stop()` call |
| `WithRestartResetWindow` | 5m | Stable runtime before restart counter resets |
| `WithSupervisorLogger` | nop | Structured logger |
| `WithEventHooks` | nil | Lifecycle event callbacks |
| `WithMetricsObserver` | nop | Telemetry receiver |

### Application

| Option | Default | Description |
|---|---|---|
| `WithShutdownTimeout` | 15s | How long to wait for clean exit after signal |
| `WithLogger` | nop | Structured logger |
| `WithMainFunc` | nil | Optional blocking main function |
| `WithSupervisor` | nil | Optional supervisor to run alongside main |

### HealthServer

| Option | Default | Description |
|---|---|---|
| `WithHealthAddr` | `:9090` | Listen address |
| `WithHealthName` | `health-server` | Component name (for multi-instance setups) |
| `WithHealthLogger` | nop | Structured logger |
| `WithHealthReadTimeout` | 5s | HTTP read timeout |
| `WithHealthWriteTimeout` | 5s | HTTP write timeout |

---

## Acknowledgements

Initially inspired by [this article](https://habr.com/ru/companies/timeweb/articles/589167) and [appctl](https://github.com/iv-menshenin/appctl).
