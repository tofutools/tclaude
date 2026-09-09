# Iteration strategy and selection

**Proposal, not an implementation commitment or schedule.** Actual selected work,
statuses and dependencies live in AWB. This document explains how to choose it.

## First proving increments

| Candidate | Value | Risk | Bounded starting point | Evidence of benefit |
|---|---|---|---|---|
| Shared dialog/optional input | High, operator-visible | Low–medium | Two existing forms using the same primitive | Duplicate helpers/styles removed; save/reopen behavior unchanged |
| Presence-aware configuration resolver | Very high | Medium | Existing shared helpers plus their direct/team callers | Clearer owner and reduced dependencies; remove only demonstrated duplication |
| Internal action below HTTP | High | Medium | One trigger guardrail/action plus its HTTP caller | Synthetic request/recorder removed for that path; same authority/refusal outcomes |
| Instance-owned spawner dependency | High for testability | Medium | The action above and its flow tests | No shared spawner mutation in migrated tests; isolated instances |

The first three are alternatives to evaluate, not a demand for three concurrent
projects. Start with one; keep implementation work in progress small. A baseline
check is part of the slice, not a weeks-long discovery prerequisite.

## Later, only when supported by the first results

- Broaden resolution to additional option families and profile access rules.
- Move remaining native interpretation into the existing harness adapters.
- Extract meaningful DB transactions with one writer per operation.
- Move dashboard-triggered observation writes to explicit ownership.
- Clarify lifecycle identities and transitions where current bugs justify it.
- Apply the agreed sandbox inheritance simplification as an explicit policy change.

Do not mechanically transplant the replacement branch's model or storage schema.
Salvage focused regression cases and small proven algorithms after checking them
against current main. The prior branch is evidence, not a migration destination.

## Each selected work item must answer

- Which ordinary maintenance problem or operator workflow improves?
- Where is its behavior today, including less obvious team/automation callers?
- Which decisions move, and which existing implementation will be removed?
- What is deliberately unchanged? Is any behavior change separately approved?
- Which focused tests and public-path check demonstrate compatibility?
- How do we revert, and what temporary adapter remains after the slice?
- What would cause us to stop or split the work instead of expanding its scope?

## Progress without misleading percentages

For each fixed, selected wave report:

- accepted slices / committed slices in that wave;
- migrated callers / identified callers for each extraction;
- obsolete policy paths and temporary adapters removed;
- change fan-out for a representative option or fix;
- focused test time and actual effort spent;
- newly discovered scope separately, with a re-estimation decision.

A wave's percentage is not a percentage of the whole product's architecture.
Do not weight experimental plugin/remote prototypes equally with production
launch, groups, terminals and logs merely because each has a tab. Net line count
is supporting evidence; adding good tests can increase it while simplifying code.

## Keep cost bounded

Agree a small work budget when selecting a slice. At its limit, present the
actual result and remaining work; do not autonomously turn it into a platform
rewrite. Run focused checks during implementation and required broader checks
at the review/integration boundary. Repeat only for changed code or concrete
failures. Preserve independent review without reopening settled components.

After the first completed slices, compare measured benefit against effort before
committing to a broader sequence. Main should be better even if the project stops
at that checkpoint.
