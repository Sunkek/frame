# Samsara

Samsara supervises the lifecycle of the long-running parts of a single Go
service: it starts them in dependency order, watches their health, restarts them
under a policy, reports readiness to an orchestrator, and shuts them all down
gracefully. This file is the glossary for that domain.

## Language

### The managed things

**Component**:
A long-running part of a service that has a start, a lifetime, and a stop — an
HTTP server, a consumer, a connection pool. The unit Samsara manages.
_Avoid_: service, worker, module, subsystem

**Supervisor**:
The owner of a set of Components. It decides when each one starts, whether it is
still healthy, and when it dies.
_Avoid_: manager, runner, orchestrator (an orchestrator is external — see below)

**Application**:
The process-level wrapper: one Supervisor, an optional main function, OS signal
handling, and a shutdown deadline over the whole thing.
_Avoid_: service, server, app

**Orchestrator**:
The external system running the process — Kubernetes, Nomad, systemd. It is a
consumer of Samsara's health endpoints, never a thing Samsara controls.

### Lifecycle

**Ready**:
The point at which a Component can actually serve — a port bound, a pool
verified. Signalled exactly once, by the Component itself. Constructing a client
is not ready.
_Avoid_: started, initialised, up

**Running**:
Ready and not yet stopped. A Component is running while its start call blocks.

**Startup order**:
The sequence in which Components are started, derived from declared
dependencies. A Component starts only after everything it depends on is ready.
_Avoid_: boot order, init order

**Dependency**:
A named Component that must be ready before this one starts. It expresses
ordering only — Samsara never injects or restarts along dependency edges.

**Graceful shutdown**:
Stopping every running Component in reverse startup order, each within its own
budget and all within the overall grace period.
_Avoid_: teardown, drain, halt

### Health and failure

**Health probe**:
One call asking a Component whether it is currently well. Probes are raw
observations; they do not by themselves change a Component's state.
_Avoid_: health check, ping, heartbeat

**Fault**:
A confirmed problem with a running Component: either enough consecutive failed
probes to breach the fail threshold, or an unexpected exit after ready. Both kinds of
fault are handled identically. A fault is what a restart policy responds to.
_Avoid_: crash, error, outage

**Fail threshold**:
How many consecutive failed probes it takes to turn a run of blips into a fault.
Set per Component with `WithHealthFailThreshold`; below 1 means the first failed
probe is a fault.
_Avoid_: max failures, retry count

**Recover threshold**:
How many consecutive successful probes it takes to leave the unhealthy state.
Set per Component with `WithHealthRecoverThreshold`.
_Avoid_: healthy count

**Debounce**:
The pairing of the two thresholds — the reason a single failed probe is not a
fault and a single successful one is not a recovery. Its purpose is to keep
transient blips out of restart decisions and readiness.
_Avoid_: hysteresis, smoothing, flap protection

**Unhealthy**:
The state a Component is in between a fault and its recovery. Distinct from a
single failed probe, which is a blip and has no state.

**Recovered**:
The transition out of unhealthy after enough consecutive successful probes.
_Avoid_: healed, back up

**Permanent failure**:
A fault the restart policy declines to restart from. This is terminal for the
Component; what it means for the Application is decided by tier.
_Avoid_: fatal error, dead

**Restart policy**:
The rule deciding, per fault, whether to restart the Component and how long to
wait first.

**Restart attempt**:
One counted retry after a fault. The count resets after a long enough stretch of
fault-free running, so an occasional fault never exhausts a budget meant for a
crash loop.

**Tier**:
How much a Component's failure matters to the Application: critical (its
permanent failure ends the Application), significant (its unhealthiness makes
the Application not-ready, its permanent failure ends the Application), or
auxiliary (neither — only logs and hooks).
_Avoid_: priority, severity, importance level

### Reporting

**Liveness**:
Whether the process is worth keeping alive at all. Turns false once the
Supervisor has entered a failure-driven shutdown — a signal to the orchestrator
to replace the process, not to wait.
_Avoid_: alive check, /livez as a concept name

**Readiness**:
Whether the Application should currently receive traffic. Turns false while a
critical or significant Component is unhealthy, and true again on recovery.
_Avoid_: availability, serving

**Health report**:
A snapshot of every Component's current state — last fault, restart count —
taken for inspection. It is a view, never a source of truth.

**Hook**:
A synchronous, non-blocking callback fired on a lifecycle or health transition,
for the embedding application's own logging and alerting.
_Avoid_: callback, listener, handler

**Metrics observer**:
The telemetry sink. It receives raw events, including probes below the fault
threshold — the fuller, noisier view that hooks deliberately do not get.
