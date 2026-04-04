# Contributing to samsara

Thanks for your interest. samsara is a small, focused package and contributions
should stay in that spirit — precise, well-tested, and zero new dependencies.

## Before you open a PR

1. **Open an issue first** for anything beyond a trivial fix. A brief discussion
   saves everyone time if the direction isn't right.

2. **Check the existing tests** — the test suite is the specification. If a
   scenario isn't tested, that's a gap worth filling before changing behaviour.

## Development setup

```sh
git clone https://github.com/sunkek/samsara
cd samsara
make check        # fmt + vet + race tests
```

`staticcheck` is optional but appreciated:

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
make lint
```

## What good contributions look like

**Fixes** — a failing test that demonstrates the bug, then the minimal fix.
Avoid changing unrelated code in the same PR.

**Features** — a clear rationale (what production problem does this solve?),
an addition to the test suite that would fail without the change, and updated
documentation if the public API changes.

**Documentation** — plain language, concrete examples. The README's lifecycle
contract section is the most important thing to keep accurate.

## The race detector is non-negotiable

Every PR must pass `go test -race -count=3 ./...`. This package manages
concurrent goroutine lifecycles — race conditions are the most likely class
of bug and the hardest to catch in review.

## API stability

samsara is pre-v1. Breaking changes are possible between minor versions, but
should be minimised and clearly documented in the CHANGELOG.

Once v1.0.0 is tagged, the public API is stable and breaking changes require
a major version bump per semver.

## Commit messages

Short imperative subject line, 72 characters max. Reference an issue number
when relevant: `fix supervisor shutdown race (#42)`.
