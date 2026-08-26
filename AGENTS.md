# AGENTS.md

`samsara` is a Go library (single package, `github.com/sunkek/samsara`) that supervises
service component lifecycles. Sources and tests sit at the repo root; `testutil/` is a
stdlib-only subpackage of test fakes.

Read [CONTEXT.md](./CONTEXT.md) for the domain vocabulary — component, fault, tier,
readiness — and use those words in code, comments, and commit messages. The decisions
behind the surprising parts live in [docs/adr/](./docs/adr/): stdlib-only dependencies,
the `nil`-on-clean-shutdown contract, the health server as a supervised component, and
the fixed component set.

## Working here

Run `make check` (fmt, vet, race tests) before opening a PR; `make lint` needs
`go install honnef.co/go/tools/cmd/staticcheck@latest` first. One test:
`go test -run TestName ./...`.

The package manages concurrent goroutine lifecycles, so races are the likeliest bug
class and `go test -race -count=3` is the gate that catches them. Every behavioural
change needs a test that fails before it and passes after. Reach for
`samsara/testutil.FakeComponent` rather than a fresh stub.

Keep the module stdlib-only. Concrete Prometheus and OpenTelemetry adapters belong in
the separate `samsara-components` module.

## Invariants

These hold across the package; a change that breaks one needs the reasoning written
down before the code.

- The `statuses` map is built once in `Run`, before any goroutine starts, and is never
  structurally mutated. `manage` therefore reads `s.statuses[name]` without `statusMu`.
  Anything that changes the component set at runtime must move every status access
  behind the mutex first.
- A confirmed fault (`onFault`, `supervisor.go`) covers both a threshold-breaching
  health probe and an unexpected post-`ready()` `Start` exit; both take the same
  restart-policy and tier path. Only the confirmed state drives readiness and restarts —
  raw probes still reach the `MetricsObserver`.
- The restart attempt counter resets after `WithRestartResetWindow` (default 5 min) of
  fault-free running.
- A `Component`'s `Stop` is idempotent and safe to call concurrently with a still-
  initialising `Start`; its `ready()` fires only once the component can genuinely serve.

## Commits and PRs

Open an issue first for non-trivial direction changes. Commit subjects: imperative,
under 72 characters. PRs carry a problem statement, the tests, and doc updates when
public behaviour changes. Report vulnerabilities through GitHub Security Advisories,
per `SECURITY.md`.

## Agent skills

### Issue tracker

Issues live in GitHub Issues for `sunkek/samsara`, driven through the `gh` CLI.
See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles, each label string equal to its role name.
See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` and `docs/adr/` at the repo root.
See `docs/agents/domain.md`.
