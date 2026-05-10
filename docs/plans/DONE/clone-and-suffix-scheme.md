# Clone fixes + reincarnate/clone suffix scheme (2026-05)

Clone-specific polish and the unified `-r-N` / `-c-N` suffix
scheme that cleaned up the previous `-reincarnate-<N>` /
`-clone-<N>` verbosity.

## `-r-N` / `-c-N` short suffix scheme (commit a5dafb3)

Uniform monotonic N for reincarnate (`<base>-r-<N>`) and clone
(`<base>-c-<N>`). The strip regex on each side recognises BOTH
the new short form AND the legacy `-reincarnate-<N>` /
`-clone-<N>` so existing titles transition cleanly without
nesting. Legacy-form titles do NOT reserve numbers in the new
namespace (no surprising holes after the changeover). Tests
rewritten + new coverage for both legacy-form behaviours.

Naming chain:

- `worker` → renames to `worker-x`, new is `worker-r-1`
- `worker-r-1` → renames to `worker-r-1-x`, new is `worker-r-2`

## Numeric-suffix collision fix (commit 0d19f2b)

Numeric suffix in base names (e.g. `worker-3`) doesn't collide
with `-r-N` / `-c-N` enumeration. Strip regex distinguishes
"author-chosen `-3`" from "system `-r-3`".

## Clone alias fallback + post-spawn `/rename` (commit d0cb0e1)

Two clone fixes:

- Alias fallback when the source agent had no per-group alias —
  derive from the source conv's CustomTitle so the clone gets a
  sensible alias instead of an empty one.
- Post-spawn `/rename` — once the new clone's conv-id
  materialises, inject `/rename <derived-title>` so CC's in-
  process title agrees with the daemon's view.

## Clone alias scheme: always `-clone-<N>`

Every clone gets `<base>-clone-<N>` (or `clone-<N>` when the
original had no alias), where N is the smallest integer free
across all `agent_group_members` rows system-wide. Clone-of-a-
clone strips the existing `-clone-<digits>` suffix before
recomputing, so `worker-clone-3` clones to `worker-clone-N`
(bumped) rather than `worker-clone-3-clone-1` (nested). Counter
is global, not per-group, so the same clone gets the same alias
across every group it inherits.

## Clone rate limiting (commit fc2f9cc)

Schema v19 `agent_clone_history` table. `db.ClaimCloneSlot` does
atomic INSERT-WHERE-NOT-EXISTS. `runCloneOrchestration` returns
429 `rate_limited` if the same source was cloned within
`agentd.CloneCooldown` (default 1m). Per-source-conv. Failed
attempts don't extend the timer.

Open: tunable cooldown via config; per-target-group rate as well
(today only per-source).
