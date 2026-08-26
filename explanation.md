# How `samsara` Works and How to Learn Go Concurrency from It

This guide has two goals:

1. Explain this package in practical detail.
2. Use its code as a real-world introduction to Go concurrency.

If concurrency feels confusing, that is normal. The best way to learn it is by studying a system with a clear lifecycle. `samsara` is exactly that.

---

## 1) What this package does

`samsara` is an in-process lifecycle orchestrator for Go services.

It manages:

- startup ordering (`db` before `api`)
- readiness signaling (component is actually usable)
- health checks
- restart decisions
- graceful shutdown
- process-level signal handling

There are two main types:

- `Application`: top-level process lifecycle.
- `Supervisor`: component lifecycle.

Most services wire both:

```go
sup := samsara.NewSupervisor()
sup.Add(db)
sup.Add(api, samsara.WithDependencies("db"))

app := samsara.NewApplication(
    samsara.WithSupervisor(sup),
    samsara.WithMainFunc(func(ctx context.Context) error {
        <-ctx.Done()
        return nil
    }),
)

if err := app.Run(); err != nil {
    log.Fatal(err)
}
```

---

## 2) Core contract: a `Component` is a long-running unit

```go
type Component interface {
    Name() string
    Start(ctx context.Context, ready func()) error
    Stop(ctx context.Context) error
}
```

How to read this:

- `Start` is not an init function. It represents runtime lifetime.
- `Start` should block until shutdown or failure.
- `ready()` means “I am now serving correctly.”
- `Stop` asks component to terminate and release resources.

`Supervisor` depends on this contract. If `Start` exits before `ready()` is called, startup failed.

### Correct mental model

Think “state machine”, not “function call”:

1. `Start` begins.
2. `ready()` marks transition to Running.
3. `Stop` or `ctx.Done()` triggers transition to Stopping.
4. `Start` returns.

```
        Start(ctx, ready) called
                  │
                  ▼
        ┌──────────────────┐  ready() not called within
        │     Starting     │  startTimeout ──────────────► failed
        └────────┬─────────┘  (or Start returns early w/ err)
            ready()│
                  ▼
        ┌──────────────────┐  Health() fails ──► restart policy
        │     Running      │◄──────────────────┐ decides: restart
        └────────┬─────────┘                   │ (Stop→delay→Start)
   ctx.Done()/Stop│                            └─ exhausted ──► failed
                  ▼
        ┌──────────────────┐
        │     Stopping     │  Stop(ctx) released; Start returns nil
        └──────────────────┘
```

The supervisor encodes exactly this machine. The `ready()` edge is special: it
is the *only* signal that distinguishes "still initialising" from "serving".
Everything downstream — dependency ordering, the `/readyz` gate, restart
accounting — keys off that single transition. That is why the contract demands
`ready()` be called once and only when the component can truly serve: it is the
clock edge the whole orchestrator is sampling.

The ready callback the supervisor injects is idempotent by construction
(`component.go` documents it, `startOne` wraps it):

```go
readySignal := make(chan struct{})
var readyOnce sync.Once
ready := func() {
    readyOnce.Do(func() { close(readySignal) })
}
```

`sync.Once` guarantees the closure body runs at most once across all
goroutines. A naked `close(readySignal)` on a second call would panic
("close of closed channel"); `Once` turns a component bug into a no-op while
still letting `startOne` observe the edge via a *closed-channel receive* (a
receive on a closed channel returns immediately, forever — the canonical
broadcast primitive in Go).

---

## 3) Go concurrency basics you need first

### 3.1 Goroutines

A goroutine is a lightweight concurrent task started with `go`.

```go
go func() {
    doWork()
}()
```

In this package, goroutines run:

- app main function
- supervisor control loops
- each component `Start` function

### 3.2 Channels

Channels synchronize goroutines and pass values.

- Unbuffered channel: send waits for receiver.
- Buffered channel: send waits only if buffer full.

`samsara` examples:

- `readySignal chan struct{}` closed when component calls `ready()`.
- `errCh chan error` collects async failures.
- `criticalErrCh chan error` reports supervisor-fatal errors.

The real four-way wait inside `startOne` (`supervisor.go`):

```go
timer := time.NewTimer(s.startTimeout)
var startErr error
select {
case <-readySignal:        // component called ready() — now running
    timer.Stop()
case err := <-startExit:    // Start returned before ready() — failure
    timer.Stop()
    startErr = err
case <-ctx.Done():          // shutdown fired mid-start
    timer.Stop()
    <-startExit             // drain so the Start goroutine can exit
    return nil
case <-timer.C:             // ready() never came — treat as failure
    startErr = fmt.Errorf("component did not call ready() within %s", s.startTimeout)
}
```

This “wait on many events” pattern is central in Go concurrency. Three theory
points the snippet illustrates:

- **`select` blocks until exactly one case is ready, then commits to it.** If
  several fire simultaneously it picks one uniformly at random — never assume
  ordering between two ready cases. Here that is fine: each case is terminal.
- **A `time.Timer` is a channel-bearing object, not a callback.** `timer.C`
  delivers one value after the duration. Calling `timer.Stop()` on every
  non-timeout path prevents the timer's goroutine/heap entry from lingering
  until it fires — a small but real resource discipline in long-running loops.
- **The `ctx.Done()` case drains `startExit` before returning.** The `Start`
  goroutine *will* eventually send its return value; if nobody receives it and
  the channel were unbuffered, that goroutine leaks. The drain (`<-startExit`)
  plus the buffer (below) are two independent safeguards against the same leak.

### 3.3 Mutexes and atomics

Use `sync.Mutex` / `sync.RWMutex` for shared complex state.  
Use `sync/atomic` for small primitive values when lock-free is enough.

In this code:

- `Application.mu` protects `cancelRoot`.
- `healthStatus.mu` protects health/restart data.
- `Supervisor.running` is an atomic flag.

### 3.4 `WaitGroup`

`sync.WaitGroup` lets you wait until a set of goroutines exits:

```go
var wg sync.WaitGroup
wg.Add(1)
go func() { defer wg.Done(); run() }()
wg.Wait()
```

`Application` uses this during shutdown to wait for `main` and `supervisor`.

### 3.5 `context.Context`

Context is cancellation + deadlines + metadata.

Here it is the shutdown backbone:

- OS signal cancels root context.
- cancellation propagates to all components.
- health checks run with timeout contexts.
- `Stop` runs with timeout contexts.

This package uses `context.WithCancelCause`, so shutdown reason can be inspected via `context.Cause(ctx)`.

---

## 4) Go memory model intuition (practical version)

A data race means two goroutines access same variable concurrently, at least one is write, and no synchronization exists.

Synchronization edges you rely on:

- channel send/receive
- channel close/receive
- mutex unlock/lock
- atomic operations
- `WaitGroup` completion sequencing

Example from tests:

- `startCalled` and `stopCalled` are atomic counters.
- without atomic, reads in test goroutine could race with writes in component goroutine.

Rule: if you are unsure whether access is synchronized, assume it is racy until proven otherwise.

### 4.1 Happens-before, concretely

The Go memory model is defined in terms of a *happens-before* partial order. A
read is guaranteed to observe a write only if the write happens-before the read.
The synchronization edges above are exactly the operations that create
happens-before relationships. Two that this package leans on heavily:

- **`go` statement.** Everything a goroutine did *before* `go f()` happens-before
  the start of `f`. This is why `Run` can build `s.statuses` and then launch
  `manage` goroutines that read it with no lock — the map write happens-before
  every `go`, which happens-before every read inside the child.
- **Channel close → receive.** A `close(ch)` happens-before a receive that
  observes the channel is closed. `startOne`'s `close(readySignal)` therefore
  publishes everything the component did before calling `ready()` to whoever
  observes `<-readySignal`. The closed channel carries no data, but it carries a
  *memory ordering guarantee* — that is the whole point.

The practical heuristic: every time data crosses a goroutine boundary, find the
specific edge (a channel op, a lock, an atomic, a `go`, or a `WaitGroup`) that
orders the write before the read. If you cannot name the edge, the access is
racy.

---

## 5) `Application.Run()` lifecycle, step by step

Inside `application.go`, `Run` does this:

1. Validates there is something to run.
2. Builds signal-aware context (`SIGINT`, `SIGTERM`, `SIGHUP`, `SIGQUIT`).
3. Wraps it with `WithCancelCause`.
4. Starts supervisor goroutine.
5. Starts main goroutine.
6. Waits for root cancellation.
7. Stops signal interception so second Ctrl+C can kill process.
8. Waits for goroutines up to `shutdownTimeout`.
9. Aggregates errors with `errors.Join`.

The signal-aware context and the two-layer cancellation:

```go
sigCtx, stopSig := signal.NotifyContext(
    context.Background(),
    os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT,
)
defer stopSig()

// WithCancelCause layers a programmatic cancel (carrying a reason) on top of
// the signal-driven cancel.
rootCtx, cancelRoot := context.WithCancelCause(sigCtx)
defer cancelRoot(nil)
```

`signal.NotifyContext` returns a context that is cancelled when any of the named
signals arrives — it bridges the Unix signal world into the context world.
Wrapping it again with `WithCancelCause` gives a *second* way to cancel
(`Shutdown(cause)`, a critical component failure, or `main` returning) while
also attaching an inspectable reason via `context.Cause(rootCtx)`. Cancellation
propagates down the tree: cancel `sigCtx` and `rootCtx` cancels too; cancel
`rootCtx` directly and `sigCtx` is untouched.

The shutdown wait uses a `WaitGroup` funneled through a channel so it can race a
timeout:

```go
done := make(chan struct{})
go func() { wg.Wait(); close(done) }()

select {
case <-done:                              // both goroutines exited in time
case <-time.After(a.shutdownTimeout):     // gave up waiting
    timeoutErr = ErrShutdownTimeout
}
```

`wg.Wait()` itself cannot be `select`ed on — it is a blocking call, not a
channel. The idiom is to run it in a throwaway goroutine that closes a channel
on completion, then `select` between that channel and a timeout. This is *the*
standard way to put a deadline on "wait for N goroutines" in Go.

Concurrency details worth noticing:

- `cancelRoot` is stored under mutex because `Shutdown()` may be called from another goroutine.
- main-function exit always cancels root context, so everything gets shutdown signal.
- context cancellation due to OS signal is treated as clean exit (not failure).
- `errCh` is buffered to size 2 (supervisor + main) so neither error-reporting
  goroutine blocks on send even if `Run` is busy; errors are drained and merged
  with `errors.Join` after the wait.

---

## 6) `Supervisor.Run()` lifecycle, step by step

`Supervisor` handles component orchestration.

### 6.1 Preparation

- Marks itself running (`atomic.StoreInt32`).
- Topologically sorts components by dependencies.
- Initializes health status map.

```go
func (s *Supervisor) Run(ctx context.Context) error {
    atomic.StoreInt32(&s.running, 1)

    ordered, err := s.topoSort()        // dependency order, or cycle error
    if err != nil {
        return err
    }
    s.order = ordered

    s.statusMu.Lock()
    s.statuses = make(map[string]*healthStatus, len(ordered))
    for _, mc := range ordered {
        s.statuses[mc.component.Name()] = &healthStatus{tier: mc.tier}
    }
    s.statusMu.Unlock()

    ctx, cancel := context.WithCancelCause(ctx)
    defer cancel(nil)

    if s.parallel {
        return s.runParallel(ctx, cancel, ordered)
    }
    return s.runSequential(ctx, cancel, ordered)
}
```

#### The topological sort algorithm

`topoSort` (`supervisor.go`) is a **depth-first-search topological sort** with
cycle detection — the classic algorithm. It produces an order where every
component appears *after* all of its dependencies.

```go
var visit func(name string) error
visit = func(name string) error {
    if inStack[name] {                         // back-edge → cycle
        return fmt.Errorf("%w: %s", ErrCircularDependency, name)
    }
    if visited[name] {                          // already emitted
        return nil
    }
    mc, ok := s.components[name]
    if !ok {                                    // dep names a missing component
        return fmt.Errorf("%w: %s", ErrUnknownDependency, name)
    }
    inStack[name] = true
    for _, dep := range mc.deps {
        if err := visit(dep); err != nil {      // recurse into deps first
            return err
        }
    }
    inStack[name] = false
    visited[name] = true
    result = append(result, mc)                 // post-order append = topo order
    return nil
}
```

Theory worth internalising:

- **Two marker sets, not one.** `visited` means "fully processed, already in
  `result`". `inStack` means "currently on the recursion stack (an ancestor of
  the node we're exploring)". Encountering an `inStack` node again is a
  *back edge* — proof of a cycle. A single boolean cannot distinguish "seen
  before on a different branch" (legal: diamond dependency) from "seen on the
  current path" (illegal: cycle). This is the standard white/grey/black DFS
  coloring, compressed to two maps.
- **Post-order append yields topological order.** A node is appended only after
  all its dependencies have been recursively appended, so dependencies always
  precede dependents in `result`. No separate reverse step is needed.
- **Stable across runs.** The outer loop drives `visit` in `insertionOrder`
  (registration order), so among components with no ordering constraint the
  output is deterministic — which is what makes the sequential start/stop order
  predictable and testable.
- **Complexity** is O(V + E): every component and every dependency edge is
  visited once.

### 6.2 Sequential startup (default)

For each component in dependency order:

- launches component management goroutine
- waits until `ready()` or startup failure
- if failure is permanent, shutdown already-started components and return error

This guarantees dependent components start only after dependencies are ready.

The core of `runSequential` (`supervisor.go`):

```go
started := make([]*managedComponent, 0, len(ordered))
criticalErrCh := make(chan error, len(ordered))
var wg sync.WaitGroup

for _, mc := range ordered {
    readyCh, startErrCh := s.launch(ctx, cancel, mc, criticalErrCh, &wg)

    select {
    case err := <-readyCh:
        if err != nil {
            s.stopAll(started)   // unwind everything already up
            <-startErrCh         // wait for this component's goroutine to exit
            return err
        }
        started = append(started, mc)   // record for reverse-order shutdown

    case <-ctx.Done():
        // shutdown fired mid-startup: this component's goroutine is live, so
        // include it in the stop set even if it never reached ready()
        started = append(started, mc)
        s.stopAll(started)
        // ... drain, wg.Wait(), collect criticalErrCh ...
        return /* cause or nil */
    }
}

<-ctx.Done()       // all up — block until shutdown
s.stopAll(started)
wg.Wait()
```

Key design points:

- **`started` is the unwind stack.** It records components in the order they
  came up; `stopAll` walks it backwards (`for i := len-1; i >= 0; i--`) so
  dependents stop before the dependencies they rely on. This is the LIFO
  discipline that makes graceful shutdown safe — a component is never torn down
  while something still needs it.
- **The loop is the synchronisation barrier.** Because `runSequential` blocks on
  `readyCh` before launching the next component, the dependency invariant is
  enforced *temporally* — component N+1's goroutine does not even exist until
  component N has signalled ready. No explicit dependency-wait logic is needed in
  this mode; the loop structure *is* the ordering.
- **Mid-startup shutdown is handled explicitly.** If the context cancels while
  waiting for component K, K's `Start` goroutine is already running. It is added
  to `started` regardless of readiness so `stopAll` calls its (idempotent)
  `Stop`, which unblocks the in-flight `Start`.

### 6.2b Parallel startup (`WithParallelStartStop`)

When `WithParallelStartStop()` is set, `runParallel` relaxes the ordering: the
*only* constraint is the dependency graph. Every component gets its own
lifetime goroutine immediately; each waits on a per-dependency channel before
starting:

```go
readyChs := make(map[string]chan struct{}, len(ordered))
for _, mc := range ordered {
    readyChs[mc.component.Name()] = make(chan struct{})
}

for _, mc := range ordered {
    name := mc.component.Name()
    lifeWg.Add(1); startWg.Add(1)
    go func() {
        defer lifeWg.Done()
        for _, dep := range mc.deps {           // gate on each dependency
            select {
            case <-readyChs[dep]:               // dep signalled ready
            case <-ctx.Done():
                startWg.Done(); return
            }
        }
        if err := s.startOne(ctx, mc); err != nil {
            criticalErrCh <- err; cancel(err); startWg.Done(); return
        }
        startedMu.Lock(); started = append(started, mc); startedMu.Unlock()
        close(readyChs[name])                   // unblock dependents
        startWg.Done()
        if err := s.manage(ctx, mc, cancel); err != nil {
            criticalErrCh <- err; cancel(err)
        }
    }()
}
```

The mechanism is a **distributed barrier built from closed channels**:
`close(readyChs[name])` broadcasts "I'm ready" to every goroutine blocked on
`<-readyChs[name]`. A closed channel is the idiomatic one-to-many edge in Go —
all waiters wake, no value is sent, and late waiters also return immediately.
Components with no edge between them race freely, so their relative start order
is non-deterministic *by design* (the docstring warns of this).

Note the **two `WaitGroup`s with different scopes**: `startWg` tracks only the
startup phase (so shutdown can wait for the `started` set to stabilise before
tearing down), while `lifeWg` tracks each component's full lifetime goroutine
(so `Run` does not return until every `manage` loop has exited). `startedMu`
guards the shared `started` slice because, unlike the sequential path, multiple
goroutines append to it concurrently.

Shutdown mirrors startup in reverse: `stopAllParallel` builds a
`dependents` map and each component waits for all of *its* dependents to finish
stopping (`<-stoppedChs[dep]`) before calling its own `Stop` — the precise dual
of the start-time dependency gate.

### 6.3 Ongoing management (`manage`)

For each running component:

- if no `HealthChecker`, status becomes known healthy and waits for context done
- else periodically call `Health` with timeout
- on unhealthy:
  - fire hooks/metrics
  - check restart policy
  - stop component
  - wait delay
  - restart component

The interface assertion that decides whether a component is even monitored:

```go
hc, hasHealth := mc.component.(HealthChecker)
if !hasHealth {
    s.statuses[name].set(nil)   // mark "known healthy", then idle
    <-ctx.Done()
    return nil
}
```

This is a **type assertion against an optional interface** — `HealthChecker` is
a separate single-method interface (`Health(ctx) error`) that a `Component` may
or may not satisfy. The comma-ok form (`hc, hasHealth := ...`) never panics; it
just reports whether the concrete type also implements `Health`. This is Go's
idiom for "capability detection": the supervisor adds health polling *only* to
components that opt in, with zero configuration, purely by method-set membership.

The monitoring loop itself is a **ticker + cancellation** select:

```go
ticker := time.NewTicker(s.healthInterval)
defer ticker.Stop()

for {
    select {
    case <-ctx.Done():
        return nil
    case <-ticker.C:
        hCtx, hCancel := context.WithTimeout(ctx, s.healthTimeout)
        hErr := hc.Health(hCtx)
        hCancel()                       // release timer immediately, every tick
        s.statuses[name].set(hErr)
        s.metrics.HealthCheckCompleted(name, duration, hErr)
        if hErr == nil { /* maybe fire recovered */ continue }
        // ... unhealthy: policy → Stop → delay → startOne ...
    }
}
```

Theory points:

- **A per-check timeout context derived from the parent.** `context.WithTimeout`
  builds a child context that fires if *either* its own deadline passes *or* the
  parent (`ctx`) is cancelled — so a shutdown mid-health-check still aborts the
  check promptly. `hCancel()` is called every iteration (not deferred to loop
  end) to free the timer right away; deferring inside a `for` would accumulate
  one live timer per tick until the function returns.
- **`time.Ticker` vs `time.Timer`.** A ticker re-arms automatically and delivers
  on a fixed cadence; that is what a periodic health probe wants. `defer
  ticker.Stop()` is mandatory — an un-stopped ticker keeps a runtime timer alive
  even after the loop exits.
- **The restart happens inline, in the same goroutine.** On unhealthy + restart,
  `manage` itself calls `doStop` → waits `delay` → `startOne`. The component's
  monitoring goroutine *is* its restart driver; there is no separate restarter.
  This keeps all per-component state (`attempt`, `startedAt`, `wasUnhealthy`)
  as plain local variables — no shared mutable state, hence no locking.

### 6.4 Permanent failure handling by tier

- `TierCritical`: cancel run with error.
- `TierSignificant`: also cancels run on permanent failure.
- `TierAuxiliary`: logs failure, app continues.

### 6.5 Shutdown

When context is done:

- waits for per-component goroutines
- stops components in reverse order
- returns critical error cause if there was one

---

## 7) Understanding `startOne`: one of the most important functions

`startOne` is a good concurrency lesson.

It starts component and then waits on four possible events:

1. component called `ready()`
2. component `Start` returned early with error
3. context canceled
4. startup timeout hit

Why this matters:

- Startup timeout catches components that forget to call `ready()`.
- `ready` is guarded by `sync.Once`, so accidental double-call does not panic.
- `startExit` channel is buffered so sending return value cannot deadlock if caller already moved on.

This is robust defensive concurrency design.

---

## 8) Health server behavior and concurrency implications

`HealthServer` is itself a component:

- `Start` binds TCP listener, sets `alive=true`, calls `ready()`, serves HTTP.
- `Stop` sets `alive=false`, calls `server.Shutdown(ctx)`.

Endpoints:

- `/livez`: based on internal `alive` flag.
- `/readyz` and `/healthz`: based on supervisor health report.

Concurrency detail: `alive` is behind `RWMutex`, because handlers and lifecycle methods run in different goroutines.

```go
func (h *HealthServer) handleLivez(w http.ResponseWriter, _ *http.Request) {
    h.mu.RLock()
    alive := h.alive          // copy out under read-lock, release fast
    h.mu.RUnlock()
    // ... write 200 or 503 based on the local copy ...
}
```

Why `RWMutex` and not a plain `Mutex`? Liveness can be scraped by many probes
concurrently; `RLock` lets all readers proceed in parallel and only blocks them
during the rare `Lock` in `Start`/`Stop` that flips `alive`. The handler copies
the flag into a local and releases the lock *before* writing the HTTP response,
so it never holds the lock across I/O.

### 8.1 How `/readyz` aggregates health by tier

`handleReadyz` pulls a snapshot from the supervisor and folds it into a single
overall status, where the **tier decides whether a degraded component counts**:

```go
report := h.reporter.HealthReport()
allHealthy := true
for _, status := range report {
    detail := componentHealthDetail{Name: status.Name, Status: statusOK, ...}
    if status.Known && status.Err != nil {
        detail.Status = statusDegraded
        detail.Error = status.Err.Error()
        if status.Tier != TierAuxiliary {   // aux failures don't fail readiness
            allHealthy = false
        }
    }
    details = append(details, detail)
}
code := http.StatusOK
if !allHealthy {
    code = http.StatusServiceUnavailable    // 503 → orchestrator stops routing
}
```

The tier check `status.Tier != TierAuxiliary` is the load-bearing line: a
degraded *auxiliary* component is reported in the JSON body (observability) but
does **not** flip the response to 503, so Kubernetes/Envoy keep routing traffic.
Critical and significant degradations *do* fail readiness. This is the HTTP
projection of the tier semantics described in `component.go` — the same concept
that governs shutdown, viewed from the load balancer's side. `status.Known`
guards against reporting a component as healthy *or* unhealthy before its first
health check has completed (the zero value of the error is `nil`, which would
otherwise read as "healthy" prematurely).

---

## 9) Writing components safely (with examples)

### 9.1 Common safe template

```go
type Worker struct {
    mu      sync.Mutex
    stopCh  chan struct{}
    running bool
}

func (w *Worker) Name() string { return "worker" }

func (w *Worker) Start(ctx context.Context, ready func()) error {
    w.mu.Lock()
    if w.running {
        w.mu.Unlock()
        return errors.New("already running")
    }
    w.stopCh = make(chan struct{})
    w.running = true
    w.mu.Unlock()

    // Initialize dependency, bind port, establish DB connection, etc.
    ready()

    select {
    case <-ctx.Done():
    case <-w.stopCh:
    }

    w.mu.Lock()
    w.running = false
    w.mu.Unlock()
    return nil
}

func (w *Worker) Stop(ctx context.Context) error {
    _ = ctx // often used if shutdown operation can block
    w.mu.Lock()
    ch := w.stopCh
    w.mu.Unlock()
    if ch != nil {
        select {
        case <-ch:
        default:
            close(ch)
        }
    }
    return nil
}
```

### 9.2 Frequent mistakes

- Calling `ready()` too early (before port bind/connection check).
- Never calling `ready()`.
- Returning error on normal context cancellation.
- `Stop` not idempotent (double close panic).
- background goroutine keeps running after `Stop` (goroutine leak).

---

## 10) Advanced `select` patterns worth learning

### Cancellation-aware wait

```go
select {
case item := <-workCh:
    _ = item
case <-ctx.Done():
    return ctx.Err()
}
```

### Timeout around operation

```go
timer := time.NewTimer(2 * time.Second)
defer timer.Stop()

select {
case <-doneCh:
    return nil
case <-timer.C:
    return errors.New("timed out")
}
```

### Ticker loop

```go
ticker := time.NewTicker(10 * time.Second)
defer ticker.Stop()

for {
    select {
    case <-ticker.C:
        checkHealth()
    case <-ctx.Done():
        return nil
    }
}
```

This package uses all three patterns.

---

## 11) Testing concurrency: techniques used in this repo

`samsara_test.go` is a useful study reference.

### 11.1 Deterministic synchronization in tests

Tests do not “sleep and hope”. They use:

- channels for precise milestones (`started` channel)
- helper with timeout (`waitStarted`)
- polling with deadline where needed

### 11.2 Race-safe test state

`mockComponent` stores mutable flags/counters in atomics, not plain fields.

Why: tests and runtime goroutines run concurrently. Plain fields would race.

### 11.3 Black-box package testing

Tests use `package samsara_test` and import public API only.  
This catches API regressions and validates behavior like a real user.

### 11.4 What scenarios are covered

- dependency order and reverse stop order
- circular/unknown dependency failures
- startup retries and timeout when `ready()` not called
- tier behavior under unhealthy states
- context cause propagation
- metrics/hook callbacks
- health endpoint semantics

Run with race detector:

```bash
make test-race
```

That runs:

```bash
go test -race -count=3 -timeout=120s ./...
```

`-count=3` helps expose flaky timing bugs.

---

## 12) Race detector: what it does and how to use it

`go test -race` instruments memory accesses and reports conflicting read/write pairs.

When it reports a race:

1. read both stack traces
2. identify shared variable
3. choose synchronization strategy (mutex, atomic, channel handoff, or ownership)
4. re-run race tests

Do not ignore race warnings in lifecycle code. Many production “random crashes” are races.

---

## 13) Practical debugging of concurrency issues

When behavior is odd (hang, intermittent fail, never-ready startup):

1. Add temporary structured logs around state transitions.
2. Add deadlines to every blocking wait in tests.
3. Verify every goroutine has a cancellation path.
4. Check every channel close is guarded against double-close.
5. Check every shared mutable variable has synchronization.

For this package specifically:

- if startup hangs, inspect whether component called `ready()`
- if shutdown hangs, inspect `Stop` and background goroutines
- if app exits unexpectedly, inspect tier + restart policy combination

---

## 14) Why tiers + restart policy are separate concepts

This is a design lesson:

- Restart policy answers: “Should this component restart after failure?”
- Tier answers: “How does its health affect application-level availability?”

Separating these concerns keeps behavior explicit and composable.

Example:

- cache might be `TierSignificant` + `AlwaysRestart(5s)`
- tracing exporter might be `TierAuxiliary` + `MaxRetries(...)`

### 14.1 The policy interface and the strategy pattern

`RestartPolicy` is a one-method interface — a textbook **strategy pattern**.
The supervisor never branches on "what kind of policy is this"; it just asks:

```go
type RestartPolicy interface {
    ShouldRestart(err error, attempt int) (restart bool, delay time.Duration)
}
```

Each policy is a tiny value type implementing that method. `NeverRestart`
returns `(false, 0)` always; `AlwaysRestart` returns `(true, fixedDelay)`;
`MaxRetries` compares `attempt` to a cap. The interesting one is exponential
backoff:

```go
func (p exponentialBackoff) ShouldRestart(_ error, attempt int) (bool, time.Duration) {
    if attempt >= p.max {
        return false, 0
    }
    base := p.base * (1 << attempt)                          // base × 2^attempt
    jitter := time.Duration(float64(base) * (0.75 + rand.Float64()*0.5))
    return true, jitter
}
```

Theory and Go internals here:

- **`1 << attempt` is the doubling.** Left-shifting 1 by `attempt` bits computes
  2^attempt as an integer with no `math.Pow` / float round-trip: attempt 0 → ×1,
  attempt 1 → ×2, attempt 2 → ×4. `time.Duration` is just an `int64` nanosecond
  count, so the multiply is plain integer arithmetic.
- **Jitter spreads the thundering herd.** `0.75 + rand.Float64()*0.5` is a
  uniform factor in `[0.75, 1.25)`, i.e. ±25%. Without jitter, N replicas that
  fail together would also *retry* together, hammering a recovering dependency
  in synchronised waves. Randomising each delay de-correlates them. The package
  uses `math/rand/v2`, whose top-level functions are safe for concurrent use, so
  multiple `manage` goroutines can call `ShouldRestart` without a shared lock.
- **`attempt` is supplied by the caller, not stored in the policy.** Policies
  are stateless value types — the same `exponentialBackoff{}` can be shared by
  many components. All per-component history (`attempt`, `startedAt`) lives as
  locals in `manage`, which is why no synchronisation is needed around it.

---

## 15) Lifecycle timeline example (end-to-end)

Imagine components `db -> api` plus health server:

1. `health-server` starts, binds port, calls `ready()`.
2. `db` starts, connects, calls `ready()`.
3. `api` starts, binds port, calls `ready()`.
4. health checks run periodically.
5. `db` becomes unhealthy.
6. supervisor stops `db`, waits retry delay, restarts `db`.
7. if recovery succeeds, app continues.
8. on Ctrl+C, root context canceled.
9. stop order: `api`, `db`, then `health-server`.

This deterministic timeline is the value `samsara` provides.

---

## 16) Study plan to learn Go concurrency using this repo

If your goal is learning, follow this order:

1. Read `component.go` comments first (contract).
2. Read `application.go` `Run` once, top-to-bottom.
3. Read `supervisor.go` functions in this order:
   - `topoSort` (the dependency-ordering algorithm)
   - `Run` (dispatches to sequential or parallel)
   - `runSequential` → `launch` → `startOne` → `manage` → `stopAll`
   - then `runParallel` → `stopAllParallel` and compare: same guarantees,
     channel-barrier implementation instead of a blocking loop
4. Run `make test-race`.
5. Open tests and map each test to one runtime behavior.
6. Modify a test slightly (for example, longer ready delay) and re-run.

You will learn faster by altering one behavior and observing consequences.

---

## 17) Final mental model

`samsara` is a concurrency control plane inside one Go process.

- `context` carries cancellation.
- goroutines do concurrent work.
- channels synchronize state transitions.
- mutex/atomic protect shared state.
- supervisor logic turns failures into explicit policy outcomes.
- tests verify lifecycle guarantees under concurrency pressure.

If you can explain `ready()`, restart policy, tier behavior, and shutdown sequencing, you already understand most of practical Go concurrency.

---

## 18) Advanced nuances specific to this code

These are the subtle decisions a careful reader should be able to defend. They are the difference between "I read it" and "I own it".

### 18.1 The map is shared without a lock — on purpose

In `manage` (`supervisor.go`) the code reads `s.statuses[name]` directly, *not* under `statusMu`:

```go
s.statuses[name].set(nil)
```

But `Supervisor.HealthReport` (`supervisor.go`) takes `statusMu.RLock()`. Why the asymmetry? Because there are **two different shared things**, with two different synchronization stories:

1. The **map structure** (`s.statuses` itself — the `map` header, its buckets). Written exactly once in `Run` (the `s.statuses = make(map[string]*healthStatus, len(ordered))` block, `supervisor.go`) *before any goroutine that reads it is launched*. The goroutine launch (`go func()` in `launch`/`runParallel`) is itself a happens-before edge: everything the parent did before `go` is visible to the child (this is a guarantee of the Go memory model, not an accident of the scheduler). So `manage` reading the map needs no lock — the write already "happened before" the read.
2. The **`*healthStatus` values** the map points to. These *are* mutated concurrently (each tick calls `.set()`). They carry their own `mu` (`healthStatus.mu`, an `RWMutex`). That is what protects the actual field writes.

A Go map detail that makes this safe: **concurrent reads of a map are fine; a
concurrent read with a write is a fatal `fatal error: concurrent map read and
map write`** (the runtime actively detects map races and crashes, separate from
the `-race` detector). The package never *writes* the map after startup — it
only mutates the pointed-to values — so the map itself is read-only-concurrent
and the runtime check is never tripped.

`HealthReport` still takes `statusMu` because it reads the *map variable* `s.statuses` from a goroutine (the HTTP handler) that has *no* happens-before edge to `Run`'s initialisation — it was not launched by `Run` after the map write. The `RLock` pairs with the `s.statusMu.Lock()` that guards `Run`'s map build to supply that missing edge. This is the single most advanced idea in the package: **synchronization scope follows the data, not the syntax.** Read the doc comment on `manage` ("It accesses s.statuses[name] directly (without statusMu) because the map …") and the one on the `healthStatus` type ("tier is set once at Run() time before any goroutine can read it …"), both in `supervisor.go` — they state exactly this invariant. If someone later adds a code path that mutates the map after startup, this whole argument breaks and both the race detector and the runtime map-race check will (rightly) scream.

### 18.2 Two buffered channels, two different reasons

- `readyCh := make(chan error, 1)` (in `launch`, `supervisor.go`) — buffered size 1 so the launch goroutine can send its ready/fail result and move on even if `runSequential`'s `select` already left via `ctx.Done()`. Without the buffer the goroutine would block forever on send → leak.
- `startExit := make(chan error, 1)` (in `startOne`, `supervisor.go`) — buffered so the goroutine running `Start` can deliver `Start`'s return value even after `startOne` stopped listening (e.g. it returned on timeout). Same leak-avoidance reasoning, one level deeper.

Rule you can carry to your own code: **if a goroutine sends on a channel and the receiver might already be gone, buffer it (or the goroutine leaks).** Buffer size = max number of unreceived sends.

### 18.3 `criticalErrCh` capacity = number of components

`criticalErrCh := make(chan error, len(ordered))` (first statement of both `runSequential` and `runParallel`, `supervisor.go`). Every component's `manage` may send exactly one critical error. Buffer sized so *all* of them can report without blocking, even if `Run` hasn't started draining yet. Then each runner `close()`s it and `range`s to collect (the `close(criticalErrCh)` / `for err := range criticalErrCh` pairs in `runSequential` and `runParallel`). Closing-then-ranging is the canonical "collect from N producers" pattern — `range` over a channel yields every buffered value and then exits cleanly when the channel is closed and drained. But it is only safe because the `wg.Wait()`/`lifeWg.Wait()` immediately preceding each close guarantees every producer goroutine has exited *before* the close. Sending on a closed channel panics (`send on closed channel`); the `WaitGroup` is the barrier that makes the close provably safe — no producer can still be in flight.

### 18.4 Why `cancel(err)` AND `criticalErrCh <- err`

In `launch` (`supervisor.go`):

```go
if err := s.manage(ctx, mc, cancel, startExit); err != nil {
    criticalErrCh <- err
    cancel(err)
}
```

Two distinct jobs: `criticalErrCh <- err` makes the error *retrievable* by `Run` so it becomes the return value. `cancel(err)` *triggers the shutdown* of everything else and records the cause. One reports, one acts. They are not redundant.

### 18.5 The clean-shutdown-vs-failure filter, in three places

`isContextError` (`application.go`) appears at every boundary where "the context was cancelled" must be distinguished from "something crashed":

- `Supervisor.Run`'s runners (three guards: two in `runSequential`, one in `runParallel`, `supervisor.go`): only return `context.Cause` if the cause is a *real* failure, not the OS signal that cancelled the parent. The guard is `if cause := context.Cause(ctx); cause != nil && !isContextError(ctx.Err())`.
- `Application.Run`'s supervisor goroutine (the `if isContextError(rootCtx.Err())` guard, `application.go`): swallow the supervisor's error entirely if the root context was signal-cancelled.

This is the practical answer to a question that trips up everyone: *"my context-cancelled error keeps surfacing as a fatal error on Ctrl+C."* The fix is exactly this — treat `context.Canceled`/`DeadlineExceeded` as success at the lifecycle boundary. Note the CLAUDE.md invariant "`Start` must return `nil` on clean shutdown" is the component-side half of the same rule.

### 18.6 The restart-reset window has order-dependent subtlety

In `manage`'s `onFault` (`supervisor.go`):

```go
if time.Since(startedAt) > s.restartResetWindow {
    attempt = 0
}
```

`startedAt` is reset to `time.Now()` after every successful restart (the `startedAt = time.Now()` a few lines further down in the same closure). So the attempt counter only resets if the component ran *cleanly for longer than the window* since its last (re)start. A component that flaps every minute under a 5-minute window will keep climbing `attempt` until the policy gives up — which is what you want (flapping ≠ healthy). Trace this with `ExponentialBackoff` to see why the window exists: without it, a component that recovers briefly then dies would restart forever.

### 18.7 `signal.NotifyContext` must be released mid-shutdown

`stopSig()` is called twice in `Application.Run` (`defer stopSig()` right after `signal.NotifyContext`, and an explicit call just before the "application shutting down" log; the deferred call is harmless since `stopSig` is idempotent). The explicit early call is the important one: while `NotifyContext` is active it *intercepts* SIGINT, so a second Ctrl+C during a slow shutdown does nothing. Releasing it restores default behavior → second Ctrl+C hard-kills. This is a real operational nicety; users expect "Ctrl+C again to force quit."

### 18.8 Things to try to prove you own it

1. Make a component never call `ready()`. Predict: `startOne` times out at `startTimeout`, treated as failure → restart policy decides. Run a test to confirm.
2. Give a `TierAuxiliary` component `NeverRestart()` and make `Health` fail. Predict: logged + hook fired, `/readyz` *stays* 200 (aux excluded by the `if status.Tier != TierAuxiliary` guard in `handleReadyz`, `health_server.go`; `handlePermanentFailure`'s `default` branch returns `nil` for aux, `supervisor.go`), app keeps running.
3. Add a `fmt.Println` in `manage` before `s.statuses[name].set` and run `go test -race`. Confirm no race is reported — then convince yourself why, using §18.1.
4. Remove the buffer from `readyCh` (size 0) and run the shutdown-during-startup test. Watch it hang/leak. Restore it.
