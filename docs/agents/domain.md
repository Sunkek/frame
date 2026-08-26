# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root: the domain glossary.
- **`docs/adr/`**: read the ADRs that touch the area you're about to work in.

Samsara is a flat, single-package library, so there is exactly one `CONTEXT.md` and
one `docs/adr/`. There are no per-context glossaries or ADR directories to look for.

If either file is missing, **proceed silently**. Don't flag its absence; don't
suggest creating it upfront. Glossary entries and ADRs get written when a term or a
decision actually gets resolved, not ahead of time.

## File structure

```
/
├── CONTEXT.md
├── docs/
│   ├── adr/
│   │   ├── 0001-zero-external-dependencies.md
│   │   ├── 0002-clean-shutdown-returns-nil.md
│   │   ├── 0003-health-server-is-a-supervised-component.md
│   │   ├── 0004-fixed-component-set.md
│   │   └── 0005-one-knob-per-option.md
│   └── agents/
└── *.go
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `CONTEXT.md`. Don't drift to synonyms the glossary explicitly avoids.

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing language the project doesn't use (reconsider) or there's a real gap in the glossary worth recording.

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0002 (clean shutdown returns nil), but worth reopening because…_
