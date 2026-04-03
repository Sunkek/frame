# frame

A production-grade application lifecycle framework for Go services — zero external dependencies.

`frame` manages the startup, health monitoring, graceful shutdown, and orchestrator integration of your service's infrastructure components (HTTP servers, database pools, message brokers, caches, etc.).

```sh
go get github.com/sunkek/frame
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

`Start` must **block** for the component's entire lifetime. Call `ready()` exactly once when the component is prepared to serve traffic — the supervisor will not start the next component until `ready()` is called. Return `nil` on clean shutdown and a non-nil error on unexpected failure.

```go
type PostgresPool struct {
    pool *pgxpool.Pool
    stop chan struct{}
}

func (p *PostgresPool) Name() string { return "postgres" }

func (p *PostgresPool) Start(ctx context.Context, ready func()) error {
    p.stop = make(chan struct{})
    pool, err := pgxpool.New(ctx, dsn)
    if err != nil {
        return err
    }
    if err := pool.Ping(ctx); err != nil {
        return err
    }
    p.pool = pool
    ready() // pool is up — supervisor proceeds to the next component
    select {
    case <-p.stop:
    case <-ctx.Done():
    }
    return nil
}

func (p *PostgresPool) Stop(ctx context.Context) error {
    close(p.stop)
    p.pool.Close()
    return nil
}
```

---

## Minimal example

```go
func main() {
    app := frame.NewApplication(
        frame.WithMainFunc(func(ctx context.Context) error {
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

`ctx` is cancelled automatically on `SIGINT`, `SIGTERM`, `SIGHUP`, or `SIGQUIT`.

---

## Real-world example

```go
func main() {
    logger := slog.Default()

    sup := frame.NewSupervisor(
        frame.WithSupervisorLogger(logger),
        frame.WithHealthInterval(10 * time.Second),
        frame.WithEventHooks(&frame.EventHooks{
            OnUnhealthy: func(component string, err error) {
                logger.Error("component unhealthy", "component", component, "error", err)
                // fire a PagerDuty/Slack alert here
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
    hs := frame.NewHealthServer(sup, frame.WithHealthAddr(":8080"))
    sup.Add(hs)

    // Infrastructure — critical dependencies.
    sup.Add(postgres.New(cfg.Postgres),
        frame.WithTier(frame.TierCritical),
        frame.WithRestartPolicy(frame.ExponentialBackoff(5, time.Second)),
    )
    sup.Add(redis.New(cfg.Redis),
        frame.WithTier(frame.TierCritical),
        frame.WithRestartPolicy(frame.ExponentialBackoff(5, time.Second)),
    )

    // S3 is useful but not required for core functionality.
    sup.Add(s3.New(cfg.S3),
        frame.WithTier(frame.TierSignificant),
        frame.WithRestartPolicy(frame.AlwaysRestart(5*time.Second)),
    )

    // HTTP server depends on postgres and redis — starts only after both
    // are healthy, ensuring no requests arrive before dependencies are ready.
    sup.Add(httpserver.New(cfg.HTTP),
        frame.WithTier(frame.TierCritical),
        frame.WithRestartPolicy(frame.MaxRetries(3, 2*time.Second)),
        frame.WithDependencies("postgres", "redis"),
    )

    app := frame.NewApplication(
        frame.WithSupervisor(sup),
        frame.WithLogger(logger),
        frame.WithShutdownTimeout(30*time.Second),
    )

    if err := app.Run(); err != nil {
        logger.Error("application exited with error", "error", err)
        os.Exit(1)
    }
}
```

---

## Component tiers

Tiers control how a component's health affects the rest of the application.

| Tier | Transient unhealthy | Permanent failure |
|---|---|---|
| `TierCritical` (default) | App shuts down | App shuts down |
| `TierSignificant` | `/readyz` → 503, app stays up | App shuts down |
| `TierAuxiliary` | Logged only, no effect | Component removed, app continues |

**Use `TierSignificant`** for components that degrade — but don't break — your service (e.g. a cache, a metrics sink, a secondary read replica). The app keeps running and the load balancer is informed via `/readyz`.

**Use `TierAuxiliary`** for components whose failure is entirely non-blocking (e.g. an audit log sink, a tracing exporter).

---

## Restart policies

```go
frame.NeverRestart()                             // fail once → done (default)
frame.AlwaysRestart(2*time.Second)               // retry forever with fixed delay
frame.MaxRetries(5, time.Second)                 // up to 5 retries, fixed delay
frame.ExponentialBackoff(5, time.Second)         // 1s, 2s, 4s, 8s, 16s (±25% jitter)
```

The restart attempt counter resets to zero if the component has been running without fault for longer than `WithRestartResetWindow` (default 5 minutes).

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

`HealthServer` exposes three endpoints for orchestrators:

| Endpoint | 200 when | Purpose |
|---|---|---|
| `GET /livez` | Process is alive | Kubernetes `livenessProbe` |
| `GET /readyz` | All Critical + Significant components healthy | Kubernetes `readinessProbe` |
| `GET /healthz` | Same as `/readyz` | Docker `HEALTHCHECK` |

`/readyz` response body:
```json
{
  "status": "degraded",
  "components": [
    { "name": "postgres",    "status": "ok" },
    { "name": "redis",       "status": "degraded", "error": "connection refused" },
    { "name": "http-server", "status": "ok" }
  ]
}
```

Docker example:
```dockerfile
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD wget -qO- http://localhost:8080/healthz || exit 1
```

Register `HealthServer` first so it binds its port before any other component starts:

```go
hs := frame.NewHealthServer(sup,
    frame.WithHealthAddr(":8080"),
    frame.WithHealthLogger(logger),
)
sup.Add(hs) // always first
```

---

## Dependency ordering

Components start sequentially in registration order. Use `WithDependencies` to declare explicit ordering when insertion order isn't sufficient:

```go
sup.Add(postgres.New(cfg))   // starts first
sup.Add(redis.New(cfg))      // starts second
sup.Add(httpServer,          // starts only after postgres AND redis are ready
    frame.WithDependencies("postgres", "redis"),
)
```

On shutdown, components stop in **reverse** start order — so `httpServer` stops before `postgres` and `redis`, ensuring no in-flight requests try to use a closed pool.

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

```go
sup := frame.NewSupervisor(
    frame.WithMetricsObserver(&prometheusAdapter{}),
)
```

---

## Programmatic shutdown

```go
app := frame.NewApplication(...)

// From any goroutine — attaches a cause visible via context.Cause(ctx):
app.Shutdown(errors.New("config reload required"))
```

---

## Logger interface

`frame.Logger` is satisfied directly by `*slog.Logger`, `*zap.SugaredLogger`, and most structured loggers:

```go
frame.WithLogger(slog.Default())
frame.WithSupervisorLogger(slog.Default())
frame.WithHealthLogger(slog.Default())
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
