# Design exercise: reincarnate and restart on main

This is the first design exercise proposed in [operations](operations.md): trace
real operations through harnesses whose lifecycles differ, then find the shared
rules, services and the smallest useful strategy boundary. It describes main as
of this writing and does not change code. Line references will drift; the
function names are the stable anchors.

Two operations were traced:

- **Reincarnate:** replace an agent's native conversation with a fresh one
  and keep the agent's identity. Claude Code and Codex use genuinely different
  sequences here.
- **Restart:** stop the process and resume the same native conversation. This
  is a control case: the harness differences are small.

## Reincarnate: side by side

Entry: `runReincarnationOrchestration` in
[reincarnate.go](../../pkg/claude/agentd/reincarnate.go). One function serves
every harness and branches on `launchEnroll :=
successorHarness.SupportsLaunchEnrollment()` plus a few `harness.CodexName`
checks.

### Normal sequence

| Step | Claude Code (launch enrollment) | Codex (ID discovered after first turn) |
|---|---|---|
| 1. Admission | Launch lock per agent, stale-generation check, live-pane requirement, durable relaunch config | Same |
| 2. Sandbox and launch plan | Re-resolve sandbox policy, validate posture, capability, Copilot loopback and access plan; persist the snapshot with a rollback closure | Same, plus Codex SSH-workaround finalisation and fast-mode-at-launch |
| 3. Titles | Successor keeps the base name; predecessor gets `-x` / `-x-N`. Title read from `conv_index` | Same rule; title read from the Codex thread store (`harnessNativeTitle`) |
| 4. Native ID | **Preset** a UUID (`--session-id`) before the fork | **Unknown** at launch. Codex mints it and exposes it only after a turn runs |
| 5. First turn | Handoff inserted into the inbox **before** the fork; title and handoff ride in as launch args (`--name`, positional prompt) | Launch gets the inert default seed (`codexSpawnSeedPrompt`: "acknowledge and wait") so that an ID materialises |
| 6. Readiness proof | Pane alive **and** no wrapper-failure signal. A preset ID proves nothing, and the session row exists before tmux | ID appears on the session row (hook-reported), which itself proves the harness booted. Then, if selected, wait for app-server readiness |
| 7. Identity | `db.RotateAgentConv(old, new, "reincarnate")` | Same |
| 8. Terminal | Move attached tmux clients to the successor | Same |
| 9. Handoff settle | Mark the pre-inserted inbox row delivered, re-derive actor refs; background: settle, then drain other queued mail | Insert the handoff row now; background: wait alive → `/rename` (Codex: out-of-band through its title store) → settle → nudge delivery |
| 10. Retire predecessor | `/rename <prev>-x`, stamp `conv_index.archived_at`, soft exit, schedule directory cleanup | Rename via thread store; `archived_at` is a no-op (Codex keeps archive state in its own store); soft exit; cleanup |

### Failure handling

| Failure | Claude Code | Codex |
|---|---|---|
| Wrapper dies before tmux | Wrapper-failure signal → roll back handoff row and sandbox snapshot | Same signal, but there is no pre-inserted handoff row to roll back |
| Harness exits at startup | Pane closes → timeout → kill and roll back | No ID appears → timeout → kill and roll back |
| Harness dies after the readiness check | **Not detected** (acknowledged residual) | Less likely: the ID only exists after a real turn ran |
| App-server not ready | n/a | Stop the successor, roll back, keep the predecessor |
| Rotation fails | Abort; predecessor remains live with its identity; successor left orphaned for manual cleanup | Same |
| Handoff delivery fails | The launch arg itself cannot be dropped; if the pre-fork inbox insert fails, the handoff goes inline only and the response warns | Rename and nudge are post-connect send-keys; the handoff stays in the inbox if they fail |

## Restart: the control case

Entry: `dashboardRestartAgent` in
[agent_restart.go](../../pkg/claude/agentd/agent_restart.go).

1. Resolve agent, launch lock, current-generation check, idle preflight.
2. Durable relaunch config must resolve before anything is stopped.
3. Park attached tmux clients on a bridge session.
4. Stop: the shared soft→grace→hard ladder (`escalateShutdownUnderLaunchLock`).
5. Re-check the generation, then `resumeOneConvUnderLaunchLock`: the same
   sandbox re-resolution and validation as reincarnate, then
   `SpawnDetachedTclaudeResume`.
6. Return clients to the new pane and remove the bridge.

The harness differences are all *mechanism*, not *sequence*:

- Soft exit is typed text or signal keys (`harness.Lifecycle`).
- The resume command shape differs: `claude --resume <id>` vs
  `codex resume <id>` in the spawners.
- A Codex app-server drive adds a readiness wait.

Restart needs **no strategy**. It is a common operation over services whose
implementations differ per harness. This is the "same responsibility,
different mechanism → service" row of the proposal, confirmed.

One gap against the proposed honest-outcome rule: restart reports `resumed` as
soon as the detached resume launch returns. The power-on path confirms the pane
actually came online (`confirmConvOnline`, `powerOnOnlineGrace`); restart does
not, so a harness that exits at startup is reported as restarted.

## Native ID changes arrive from three directions

Every path ends in the same primitive, `db.RotateAgentConv`:

| Source | Example | Where correlation lives today |
|---|---|---|
| Operation-initiated | Reincarnate (preset or discovered ID) | `reincarnate.go`, via launch-enrollment branch |
| Native, announced | Claude `/clear`, `/resume`, compaction: `SessionStart` with `source=clear/resume/compact` | Shared hook code (`hook_callback.go`: pending_conv, `needsIdentityMigration`, `migrateClearedIdentity`) |
| Native, unannounced | Claude `/remote-control` bridge handoff: `source=startup` with a new ID | Shared hook code, proven by a bounded **transcript head-scan** (`isVerifiedConvContinuation`) |

The last two rows are Claude-specific knowledge: the meaning of hook sources
and the transcript lineage fields. They currently sit in generic session hook
code. This is the clearest concrete example of what the proposal means by
"the integration owns correlation." A Claude observation handler would emit
"continuation advanced (agent, reason)" and the core would apply the rotation,
without the core knowing that transcripts were read to decide it.

## What this says about the proposal

### Confirmed

1. **Most of reincarnate is common.** Admission, launch-plan resolution,
   titles, rotation, terminal carry-over, predecessor retirement and the
   response are the same for every harness. The harness-specific part is one
   coherent slice: *obtain a live successor with a known native ID and give it
   its title and first turn* (steps 4–6 and 9).
2. **Strategies should be selected by native mechanism, not harness name.**
   Claude Code, Copilot and OpenCode all take the launch-enrollment branch
   (OpenCode through its server API). Only Codex takes the discover-after-seed
   branch. Two strategies cover four harnesses. Today the branch is an
   untyped `launchEnroll` boolean, with Codex-name checks scattered through
   the common function.
3. **Restart needs no strategy.** Not every operation needs one, which
   supports the "add a variation point only when a real harness demonstrates
   the need" rule.
4. **The stop ladder is already the right shape.** The comment on
   `escalateShutdownUnderLaunchLock` records that a second stop ladder was
   merged into one shared definition of "stopped." That is the proposed
   service extraction, already done once.

### Challenged

1. **Users see generations.** Reincarnate's user-visible contract includes
   the predecessor: it stays reachable by séance and the succession edge, and
   the response names `old_conv` and `new_conv`. This is now answered by
   Agent generations (see the [overview](README.md#agents-generations-and-native-references)),
   not by a Conversation entity. The core knows generations and the current
   pointer; native contents stay private to the integration.
2. **Main conflates generations with native ID changes.** `db.RotateAgentConv`
   runs for intentional reincarnation and for Claude Code `/clear` alike. In
   the proposed boundary only the former may be generational; `/clear` would
   update the current generation's association inside the integration.
3. **Archive state is split across stores.** Claude keeps it in
   `conv_index.archived_at`, Codex in its own thread store, and reincarnate
   treats `sql.ErrNoRows` as the expected Codex no-op. In the proposed model,
   recognising prior associations takes over archiving's role in identity
   tracking, and archiving is not part of the agent-accessible model.

### First extraction candidate

This is a **service**, not a strategy: a "relaunch plan" that resolves the
durable config, re-resolves and validates the sandbox policy, persists the
snapshot and returns spawn arguments plus a rollback handle. Today that
preamble is repeated:

- `resumeOneConvUnderLaunchLock` and reincarnate both run the sandbox
  resolve/validate/persist/rollback sequence (about 100 lines each).
- The validators also recur in clone, séance and sandbox assignment.
- Both hand-copy the same ~25 fields from `durableRelaunchConfig` into
  `SpawnArgs`.

This is where a missed field silently drops a posture on one path. The
existing comments on `composeAgentRelaunchProfile` already name that risk.

Only then extract the successor strategy, with two implementations:

```go
// Sketch only: obtain a live successor whose native ID is known,
// with title and first turn delivered.
type successorLauncher interface {
    Launch(plan relaunchPlan, intro successorIntro) (successor, error)
}
```

- **Preset-ID launcher:** preset ID, launch args, pane-alive plus
  wrapper-failure proof.
- **Seed-and-discover launcher:** seed turn, ID poll, post-connect rename
  and delivery.

Each owns its own rollback of what it created. The common operation keeps
admission, rotation, retirement and the response.

## Open questions this exercise surfaced

- Should restart confirm the pane came online, as power-on does?
- Does reincarnation create a new generation, and does séance target an
  existing one? The generation boundary leaves this open.
- Should the unannounced-rotation transcript scan move behind the Claude
  integration first, since it is the clearest leak of native mechanics into
  shared code?
