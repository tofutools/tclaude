# Agent coordination — TODO / DONE

The active backlog lives under sibling directories rather than in this
file, so an agent picking up a task only needs to load the single
relevant feature page instead of the full backlog.

## Layout

```
docs/plans/
├── TODO/
│   ├── high-prio/   ← active work; pick from here first
│   ├── med-prio/    ← worth doing, not blocking
│   └── future/      ← deferred / "if shows up in practice" /
│                      cross-machine and similar far-out items
└── DONE/
    └── *.md         ← one shipped feature per file (no index)
```

One file per coherent feature (kebab-case slug). Each TODO file is
self-contained: states what's open, briefly notes what's already
shipped for context, lists relevant files, and any open questions.

## Conventions

**Picking up work**

1. `ls docs/plans/TODO/high-prio/` first. Drop to `med-prio/` if
   nothing fits.
2. Open the relevant file. Sanity-check against current code before
   assuming the TODO is still accurate — plan docs decay.
3. After shipping, move the TODO file into `DONE/` — keep its
   filename, rewrite the body to describe what shipped (CLI surface,
   schema migrations, slugs, file paths, test scenarios, commit refs
   if known). One file per feature; there is **no index** —
   `ls -lt docs/plans/DONE/` orders by ship date and `grep -rli`
   finds features by keyword.
   - Sub-item shipped → mark it inline in the TODO file ("shipped
     2026-MM-DD"); move the whole file into `DONE/` once everything
     substantive is done.

**New TODOs**

Create a new file in the appropriate tier rather than appending to an
existing one if it's a distinct surface. Better many small files than
one giant one — the whole point of this layout is keeping the
per-feature context loadable in isolation.

**Reprioritising**

`mv docs/plans/TODO/<from>/<file>.md docs/plans/TODO/<to>/`. The dir
*is* the priority — there's no metadata header to update.

## Related design docs

- `agent-coord.md` — original v1 design for `tclaude agent`
  (cross-session messaging, groups, inbox).
- `agentd.md` — daemon design (HTTP-over-Unix-socket, peer-cred
  identity).
- `testharness-v2.md` — simulator / flow-test architecture.
