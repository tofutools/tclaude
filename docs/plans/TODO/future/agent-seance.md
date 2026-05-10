# `tclaude agent seance` — consult a predecessor

Inspired by `gt seance` from Steve Yegge's [Welcome to Gas
Town](https://steve-yegge.medium.com/welcome-to-gas-town-4f25ee16dd04).
Lets a successor agent (post-reincarnate, post-clone, post-revive)
read or even resume its predecessor's session — useful when the
normal handoff failed or was incomplete.

## What gas town does

> Allows workers to communicate with their predecessors in the
> same role by using Claude Code's `/resume` feature to revive
> previous sessions. This helps agents discover and retrieve work
> handed off by their predecessor when the normal handoff
> process fails.

## Why we'd want it

tclaude's reincarnate flow expects the agent to persist work-in-
progress to disk *before* calling reincarnate (the "continuity
contract" in
[`DONE/agent-self-lifecycle.md`](../../DONE/agent-self-lifecycle.md)).
That's enough for the common case — the follow-up message via
`agent_messages` plus the agent's own notes on disk get the
successor going.

But sometimes:

- The agent forgot to persist (or persisted incompletely).
- The reincarnate was triggered externally (a manager / cron /
  context-nudge auto-trigger) and the agent didn't get to wrap up.
- The agent crashed without a clean `/exit`.
- The successor needs context the predecessor didn't think to
  write down ("what was the last file you edited?", "what tool
  call were you about to make?").

In all those cases, the predecessor's `.jsonl` survives unchanged
(reincarnate doesn't touch it — it just stamps `archived_at` and
optionally renames to `<title>-x`). So the data is there;
seance is the verb that exposes it.

## Already-shipped infrastructure

- Schema v15 `agent_conv_succession` (see
  [`DONE/conv-succession-chain.md`](../../DONE/conv-succession-chain.md))
  records every `(old, new, reason)` transition.
- `db.ResolveLatestConv(id)` walks forward; the inverse walk
  ("who was my predecessor?") is trivially the SELECT
  `WHERE new_conv_id = ?`.
- Conv `.jsonl` files are not deleted on reincarnate — only
  archived. Read paths still work.

So the daemon-side data is all in place. This file is the verb +
UX on top.

## CLI

```
tclaude agent seance              # consult my own predecessor
tclaude agent seance --target <conv>   # consult someone else's predecessor
tclaude agent seance --depth 2    # walk back two reincarnations
```

Subcommands:

```
tclaude agent seance read [--last N]   # show last N turns from predecessor's jsonl
tclaude agent seance summary           # one-paragraph summary of predecessor's last state
tclaude agent seance resume            # spawn a read-only tmux pane resuming predecessor
tclaude agent seance ls                # list predecessors in chain (with depth)
```

`read --last N` is the bread-and-butter — fast scan of the
last N conversation turns from the predecessor's jsonl, so the
successor can see what the predecessor was working on without
opening another pane.

`summary` runs an LLM pass over the last K turns (or just the
final user/assistant pair) to extract "what task were you on,
what files did you touch, what was your next intended step".
Gives the successor a fast briefing.

`resume` is the heaviest verb: spawns a fresh tmux session via
`tclaude session new -r <predecessor-conv> -d --global`, attaches
read-only via `tmux attach -r` semantics. The successor inspects
visually, then detaches. The predecessor's identity stays
archived — the resume here is for inspection, not revival.

`ls` walks the succession chain backward (`SELECT WHERE
new_conv_id = ?` recursively, cycle-protected) and shows depth
+ titles. Useful when an agent has reincarnated several times
and the seancer wants to peek further back than depth-1.

## Permissions

Slug: `self.seance` — read-only consult of own predecessor.
**Default-granted** (alongside `self.compact` / `self.reincarnate`
/ `self.clone`). Cheap, recoverable, the agent already implicitly
"owns" its own historical jsonls.

Slug: `agent.seance` — consult another conv's predecessor.
**Default human-only.** Same gating as other `agent.<verb>`
manager-pattern slugs; group-owner implicit power applies.

The `resume` subcommand is the most expensive (spawns a new tmux
session + CC process). Could gate behind a separate slug
`self.seance.resume` if the resource cost matters; lean toward
folding into `self.seance` for v1 and revisiting if abuse shows
up.

## Read-only enforcement

`seance resume` should make it hard for the successor to
accidentally type into the predecessor's pane and start a new
turn — that would mutate the predecessor's archived `.jsonl`
and confuse anyone walking the chain later. Options:

1. **tmux attach `-r`** — read-only mode at the tmux level.
   Successor sees the pane but keystrokes are dropped.
2. **Sentinel turn** — inject a `[system: this is a seance — do
   not respond]` message at the top so even if the successor
   does send a turn, the predecessor's CC instance refuses.
3. **Both belt and suspenders.**

Lean (3): tmux `-r` is the load-bearing protection; the sentinel
turn is documentation of intent for any human inspecting the
file later.

## Audit

Every seance call records to a small `agent_seance_log` table:
`(seancer_conv, predecessor_conv, verb, at)`. Forensics: "who
peeked at whose history when". Tiny; cheap; matches the
forensic-trail pattern of other recent additions.

## Test coverage

Flow tests under `pkg/claude/agentd/*_flow_test.go`:

- `TestSeance_Read_LastN` — reincarnate, successor seances
  `read --last 5` against predecessor; assert the returned turns
  match the predecessor's jsonl tail.
- `TestSeance_Resume_OpensReadOnlyPane` — assert tmux session is
  spawned with `-r` flag.
- `TestSeance_Ls_WalksChain` — three-level chain
  `worker → worker-r-1 → worker-r-2`; `seance ls` from
  `worker-r-2` returns both predecessors with correct depth.
- `TestSeance_Permission_AgentSeance_HumanOnly` — peer agent
  without the slug + not group-owner gets 403 on cross-agent
  `seance read`.
- `TestSeance_AuditRowRecorded` — every seance call writes a row
  to `agent_seance_log`.

## Out of scope (deferred even within future/)

- **Bidirectional revival.** Letting the successor RESUME the
  predecessor as a live sibling (rather than read-only inspection).
  Would mean two agents on what used to be one conv; identity-
  collision can of worms. Skip.
- **Cross-machine seance.** Predecessors that lived on a
  different host. Out of scope while tclaude is single-host.
- **Predecessor edit / repair.** Letting the seancer modify the
  predecessor's jsonl to "fix" handoff data. Mutating archived
  history is a bad idea — recover by writing fresh notes
  forward, not editing the past.

## Cross-references

- [`DONE/conv-succession-chain.md`](../../DONE/conv-succession-chain.md)
  — chain table this leans on.
- [`DONE/agent-self-lifecycle.md`](../../DONE/agent-self-lifecycle.md)
  — reincarnate is the producer of succession rows.
- [Gas Town article](https://steve-yegge.medium.com/welcome-to-gas-town-4f25ee16dd04)
  — original concept.
