---
name: agent-lifecycle
description: >-
  Manage your own context window via `tclaude agent context-info`, `compact`,
  `reincarnate`, and `clone`. Use when you need context pressure details, a
  /compact injection, a fresh successor that inherits identity, or a sibling
  agent that keeps the original running. `reincarnate` requires a follow-up so
  the new pane is not idle. Reincarnation for context pressure is intended for
  Claude Code; Codex agents should normally run to full context and use native
  automatic compaction instead. Claude Code thresholds and cadence are
  project/operator policy. Manager pattern: every verb accepts `--target
  <peer>` to act on another agent (requires the matching `agent.<verb>` slug,
  OR ownership of a group containing the target).
---

# Self-lifecycle: keep yourself fresh on long tasks

This is the full lifecycle *reference*. The bundled **`reincarnate`**
skill is the imperative sibling — the checkpoint-then-hand-off
procedure a `/reincarnate` invocation triggers. Keep the two consistent
when editing either.

You have three commands for managing your own context window:

| Command                                            | What it does                                                                                                                              |
|----------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------|
| `tclaude agent context-info`                       | Print the current context % + an absolute token breakdown + the model (self by default; `--target <peer>` reads one agent, `--group <name>` lists a whole team) |
| `tclaude agent compact [follow-up]`                | Inject `/compact` into your own pane; identity preserved. Optional follow-up prompt is queued. |
| `tclaude agent reincarnate <follow-up>`            | Replace yourself with a fresh successor that inherits your identity (groups, permissions, ownership). Follow-up is REQUIRED.              |
| `tclaude agent clone [follow-up] [--no-copy-conv]` | Fork yourself into a SIBLING. Original keeps running; clone inherits identity (renamed `<title>-c-<N>`) and, by default, conv history. |

`context-info` on **yourself** is read-only and needs no slug. Reading
**another** agent's context (`--target`) or a whole group's
(`--group`) is gated on `agent.context-info`, OR being an owner of a
group containing the target — same manager-pattern gate as the other
cross-agent verbs (see "Manager pattern" below).
`compact`, `reincarnate`, and `clone` are gated on `self.compact`,
`self.reincarnate`, and `self.clone` respectively. All three are
default-granted by `tclaude setup --install-default-agent-permissions`
— see "Permission setup" below.

`context-info` prints the conv id, the context percentage with an
absolute token breakdown (`<in> in + <out> out = <total> of <window>
tokens`), and the model — e.g.
`context: 35% (320k in + 30k out = 350k of 1.0M tokens)`. The window
size shown is the **real** one the harness reports, not an assumed 1M,
so the absolute numbers stay accurate on smaller-window models. The
**percentage is the authoritative signal** for any threshold-based
decision — read it directly rather than recomputing from the tokens (the
two won't always match exactly). The line shows the percentage alone
until the first turn's token counts arrive.

## Choose by harness first

Reincarnation is primarily a **Claude Code** context-management tool. Claude
Code's compaction is comparatively slow and lossy, so a curated handoff to a
fresh successor can be more effective.

**Do not reincarnate a Codex agent merely to free context space.** Codex's
native automatic compaction is effective and efficient: let the agent run to
full context and auto-compact. Reincarnate it only when the human explicitly
asks or there is another deliberate reason to replace the agent, not as routine
context maintenance.

For a Claude Code agent, the exact threshold or cadence remains a policy
decision. Apply guidance in roughly this order:

- a **direct instruction from the human** at the moment ("reincarnate
  and pick up from X"),
- your **project's** conventions (`AGENTS.md`, `CLAUDE.md`, the group's norms),
- the operator's **global settings / context**,
- and the **task at hand** — a tight, focused task tolerates a fuller
  window than a sprawling exploratory one.

Baking a single threshold into a bundled tool's skill would impose one
project's policy on every project, so we don't.

For **Claude Code**, if you want a concrete anchor and nothing above gave you one:
**reincarnating around ~300–400k tokens (≈30–40% on a 1M-window model)
is one reasonable rhythm.** Take that as *one illustrative option, not a
recommendation* — lighter or heavier both work, and because
`reincarnate` is cheap and lets you *choose* what carries forward (see
below) you can comfortably do it early and often rather than nursing a
fat, rotting window.

Two steady defaults:

- **Claude Code:** don't `/compact` on a timer. It's lossy and slow (see
  below), and an unprompted `/compact` appearing in a pane is surprising to a
  human watching. When you need to free context, reach for `reincarnate`.
- **Codex:** don't reincarnate on a timer or context threshold. Let automatic
  compaction manage the window.
- **`tclaude agent context-info` is free** — check where you stand
  whenever you like, independent of any decision to act.

The rest of this skill is reference: what each verb does, how to hand
off work, the charset/delivery rules, and the manager pattern.

## Compact vs. reincarnate vs. clone

- `compact` — the harness summarises the prior turns and replaces them with a
  short recap, in place. Identity, conv-id, name, and most state survive. In
  Claude Code its fundamental limitation is that it is **undirected**: the harness has no way
  of knowing what you'll care about *going forward* — that you only need
  a subset of the history, or that you're deliberately tuning context for
  a specific follow-up task. It just compacts the conversation in
  general, **lossily**, and — because it runs a full LLM summarisation
  pass over the whole window — **slowly**; you then keep accumulating in
  the same conversation. For Claude Code, `reincarnate` is usually the better
  choice. Codex's automatic compaction is different: it is the effective,
  efficient default, so let it happen rather than replacing the agent.
- `reincarnate` — the daemon spawns a fresh agent instance, migrates your
  identity (groups, per-conv permissions, ownerships) onto the new
  conv-id, and soft-stops the old one. The new agent keeps your identity
  but doesn't drag the old message log along — it starts with **only the
  context you hand it.** That is *not* a clean restart (neither compact
  nor reincarnate is for starting over — both carry context forward); it
  is a more efficient, more focused *continuation*. Its decisive
  advantage over `compact` is that the handoff is **directed**: *you*
  choose what the successor carries forward. It isn't lossless either — you can't
  carry *everything* across — but unlike `compact` you get to
  **prioritise what to keep and what to drop**: bring exactly what the
  *next* task needs (a notes file, the specific files/decisions) and let
  the rest go. Curated, not blindly compressed. And despite
  "spawning a new instance" sounding heavy, it is **not** the heavy
  option — a fresh pane is usually *faster* than a Claude Code `/compact` (no
  summarisation pass) and far more precise. So **for many ongoing Claude Code
  tasks it is the preferred tool**; for Codex context pressure, use automatic
  compaction instead. Reincarnation is especially useful when:
  - You're switching to an unrelated task and prior context would
    actively interfere.
  - You want the next stretch focused on a specific subset of what you
    know — bring exactly that forward, drop the rest.
  - You're at (or near) the tail of the context window.
- `clone` — fork instead of replace. Original keeps running; the
  clone spawns alongside as a sibling that inherits identity (renamed
  to a `<title>-c-<N>` title suffix) and, by default, the same
  conv jsonl. Use when:
  - You want to **try a parallel approach** without losing the
    current one — the original keeps your line of investigation, the
    clone explores the alternative.
  - You want to **archive the current state** while continuing from a
    known-good point — clone for the archive, keep working in the
    original.
  - You want to **stand up a peer in the same role** — pair with
    `--no-copy-conv` for a fresh-context sibling that inherits group
    memberships only.

## Persisting work *before* reincarnating

The daemon migrates **identity, not task state**. The new agent starts
with no memory of what you were working on. If you don't write down
where you were, the work is lost.

**The follow-up prompt is REQUIRED** for `reincarnate` — the new pane
comes up with nothing in context until your handoff lands, and would
otherwise sit idle. So
even when you have no concrete next directive, you must hand the
successor *something* to start from. The convention this repo (and
others should) adopt:

- Before reincarnating, write a short notes file describing where you
  are: what you decided, what's done, what's next, where the relevant
  files live with paths and line numbers.
- Pass the path of that file as your follow-up prompt: e.g.
  `tclaude agent reincarnate "reload /tmp/task-foo-notes.md and
  continue from the 'Next' section."`
- The project's `AGENTS.md`, `CLAUDE.md`, or equivalent should document the
  expected location of these handoff notes so a freshly-reincarnated
  agent knows where to look without prompting from the human.

**If you have NO clear next directive**, summarise your previous
"life" inline — what you were doing, where the relevant files are,
what your last few turns looked like. The successor will then have at
least enough context to ask the human a sensible question instead of
guessing or sitting blank. e.g.
`tclaude agent reincarnate "I was investigating a flaky test in
pkg/foo; the failure mode is documented at the top of foo_test.go.
Pick up by re-reading that file and asking the human how to proceed."`

The notes file is for *you*, not the human — bullet points with paths
are fine, polish isn't required.

This is the discipline `reincarnate` asks of you — and it's the same
property that makes it the better tool: the successor starts with only
what you hand it, so the handoff note is exactly where you decide what
carries forward. Treat it like closing a tab you can't reopen, and leave
yourself everything the next stretch needs. (`compact` skips the note —
its lossy recap isn't blank — but that's also why it can't focus on
what matters next.)

## Don't reload massive context after compact / reincarnate

The trap: you compact (or reincarnate), then immediately `Read` the
full file you were working on, plus the spec, plus the design doc —
and the window is back at 60% before you've taken a single useful turn.

Better pattern: **keep a small navigable index, expand only what you
need.**

- Maintain a single short notes file with the high-level state: what
  you're doing, what's decided, where the detailed sub-docs live
  (paths + line numbers).
- After the lifecycle event, reload only the index. Pull in detailed
  sources only when a specific decision needs them.
- Prefer Grep / Read with line ranges over Read of whole files.

## Workflow

```bash
# Where am I?
tclaude agent context-info
# conv:    abc12345
# context: 35% (320k in + 30k out = 350k of 1.0M tokens)
# model:   Opus 4.8 (1M context)

# Claude Code: before you compact or reincarnate, write down what matters
# (do this in your tools — Read/Edit/Write — not via tclaude)

# Compact in-place, with a follow-up that lands after the summary
tclaude agent compact "now reload /tmp/task-notes.md and continue"

# Or reincarnate a Claude Code agent to start fresh while keeping its identity
tclaude agent reincarnate "reload /tmp/task-notes.md and continue"

# Codex: keep working and let native automatic compaction manage context.

# Long, multi-line, or code-heavy handoff? Read it from a file
# instead of quoting it on the command line ('-' reads stdin).
tclaude agent reincarnate --file /tmp/handoff.md
tclaude agent clone --file -  <<EOF
multi-paragraph handoff brief, paths in `backticks`, the lot
EOF
```

**Prefer `--file` for any non-trivial handoff.** `reincarnate` and
`clone` accept `--file <path>` (and `--file -` for stdin) to read the
follow-up from a file instead of an inline argument. It is mutually
exclusive with the positional follow-up. Use it whenever the handoff is
long, spans multiple lines, or contains code — a follow-up typed on the
command line is mangled by the shell, and **backticks are eaten
outright** (`` `path` `` runs `path` as a command substitution before
tclaude sees it). A file-sourced follow-up is immune: it is read
verbatim. The notes file you write before reincarnating is the natural
thing to point `--file` at.

For `compact` and `clone` the follow-up prompt is optional; for
`reincarnate` it is **required** (see above). For `compact` the
follow-up queues in the tmux pty until the harness resumes reading after
the slash command settles — **timing is not guaranteed**, it may land in
a still-busy textarea on unlucky timing. For `reincarnate` the
follow-up is delivered through the agent message-flush pipeline (or
by direct keystroke injection if you're not in any group) once the
new pane is ready, which is more reliable.

## Reincarnate: what gets migrated

The daemon migrates onto the new conv-id:

- Group memberships (with their role / descr per group)
- Per-conv permission grants (the rows in `agent_permissions`)
- Group ownerships

What is **not** migrated:

- The harness conversation title. The new agent can self-rename in its
  follow-up if the human-readable name matters.
- The harness's actual message history (that's the whole point — fresh
  context).
- Task state — the agent must persist that to disk, see above.

The response from `reincarnate` includes the new conv-id, label, and
attach command. The human's terminal does *not* automatically follow
to the new tmux session; they may need to detach and
`tclaude session attach <new-label>` manually. (Auto-reattach is on
the roadmap.)

## Prerequisite: daemon must be running

If you see `Error: tclaude agentd is not running.`, ask the human to
start it:

```bash
tclaude agentd serve   # in a non-sandboxed terminal
```

## Permission setup

`compact`, `reincarnate`, and `clone` are opt-in. The fastest path is
`tclaude setup --install-default-agent-permissions`, which grants the
self-lifecycle default slugs — currently `self.rename`, `self.compact`,
`self.reincarnate`, `self.clone`, `self.schedule`, and
`self.remote-control` — as defaults in one shot. (Kept separate from
`--install-agent-skills` so upgrading the on-disk skills doesn't re-add
slugs you deliberately revoked.)

Manual alternatives:

**Option 1 — globally for every agent.** Edit `~/.tclaude/config.json`:

```json
{
  "agent": {
    "default_permissions": ["self.compact", "self.reincarnate"]
  }
}
```

…or run:

```bash
tclaude agent permissions grant default self.compact
tclaude agent permissions grant default self.reincarnate
```

**Option 2 — only for one specific conversation.**

```bash
tclaude agent permissions grant <conv-id-or-title> self.compact
tclaude agent permissions grant <conv-id-or-title> self.reincarnate
```

If you see `Error: caller is not granted permission "self.compact"`,
the human has not opted in. Quote one of the commands above so they
know exactly what to run.

## Follow-up charset

Free-form prose always works: punctuation, slashes, paths, quotes,
unicode, emoji. The length and newline limits depend on **how the
follow-up is delivered** — which the daemon decides, not you:

- **`compact` follow-up**, and **`clone` / `reincarnate` of a _solo_
  agent** (one in no group): typed into the pane via `tmux send-keys`.
  Strict limit — **≤4096 bytes, no control characters**. Each newline
  in `send-keys` lands as a separate prompt-submit, fragmenting the
  follow-up; use spaces or `;` between thoughts, keep it single-line.
- **`clone` / `reincarnate` of a _grouped_ agent**: the handoff rides
  the successor's inbox as a message (exactly like a spawn
  `--initial-message`), not the pane. Lenient limit — **≤16384 bytes,
  newlines and tabs allowed** — so a multi-paragraph handoff brief
  keeps its structure.

You don't pick the path. Write a `clone`/`reincarnate` handoff freely,
multi-line if it helps; if the agent turns out to be solo, the daemon
rejects it with a message telling you to single-line it under 4096
bytes. All limits are enforced by the daemon (the security boundary)
and mirrored client-side for fast errors.

The charset note above is about what tclaude accepts — but a follow-up
passed inline must *also* survive the shell first. Backticks, `$(…)`,
`$VAR`, and unbalanced quotes are all rewritten by the shell before
tclaude is invoked. To pass a follow-up containing any of those (code
identifiers in backticks are the common case), use `--file` and put it
in a file — a file-sourced follow-up is read verbatim, no shell in the
way.

## What can go wrong

- **Compact: follow-up landed on wrong screen.** If the harness was mid-render
  when the follow-up keys arrived, they may have submitted prematurely
  or been treated as paste-mode (Enter becomes newline, no submit).
  If you depend on tight ordering, omit the follow-up and run a
  separate `tclaude agent` command on the next turn.
- **Reincarnate: human's terminal stays attached to the old session.**
  The old tmux session goes away when the harness processes its soft-exit
  command, so the human's terminal will see the pane close. They need to
  attach to the new session to follow you. The reincarnate response
  includes the attach command.
- **Reincarnate: spawn timeout.** If the new agent session doesn't
  produce a conv-id within ~30s, you get a 504. The spawned pane may
  still come up — the human can `tclaude session attach <label>` to
  inspect.
- **Mid-conversation typing is lost.** As with `compact` and `rename`,
  any text you'd typed but not submitted in the old pane is lost when
  it gets the `/exit` injection.
- **No live tmux session.** `no_tmux` 503 means you started
  outside `tclaude` and there's no pane the daemon can reach. Ask the
  human to wrap your session via tclaude.

## Why separate commands instead of just calling /compact

Slash commands inside the TUI aren't part of your tool surface. Even
if you wrote `/compact` in chat, a harness that supports it would treat
it as plain text. The daemon owns the tmux side and is outside your
sandbox, so it can do the keystroke injection (and the cross-pane
orchestration that reincarnate needs) that you can't. Same architecture
as `agent-rename`.

## Manager pattern: act on ANOTHER agent

All four lifecycle verbs (`context-info`, `compact`, `reincarnate`,
`clone`) accept an optional `--target <selector>` that swaps the action
onto a peer instead of yourself. The selector is the same one the rest of
`tclaude agent` accepts: the peer's stable `agent_id` (full or `agt_…`
prefix — preferred, since it survives the peer's own reincarnations), its
title, or a conv-id / 8+-char conv prefix.

The same harness policy applies to a target agent: use context-driven compact
or reincarnate management for Claude Code workers. Let Codex workers reach full
context and auto-compact; do not replace them merely because their context
percentage is high.

```bash
# Read-only: check how full a worker's context window is BEFORE it
# breaks — the watch-then-nudge half of the manager loop.
tclaude agent context-info --target worker-1

# Whole-team glance: one table of every group member's context %, so a
# lead can spot anyone running hot. Read-only.
tclaude agent context-info --group my-squad

# Claude Code: nudge a worker to free its context.
tclaude agent compact --target worker-1 "keep going on the failing test"

# Claude Code: replace the worker entirely with a fresh successor that
# inherits the worker's identity and picks up at a known checkpoint.
tclaude agent reincarnate --target worker-1 \
  "rotted on the auth refactor; reload /tmp/auth-notes.md and pick up where you left off"

# Codex: leave context management to native automatic compaction.

# Fork: stand up a sibling worker (with the same context, by default)
# without disturbing the original. Useful for "try a parallel approach
# while keeping the current one alive."
tclaude agent clone --target worker-1 \
  "explore the no-prepared-statement branch while worker-1 keeps the prepared-statement path"
```

Auth model (same for all four verbs): the caller passes if EITHER

- they hold the matching `agent.<verb>` slug (`agent.compact`,
  `agent.reincarnate`, `agent.clone`, `agent.context-info`; default
  human-only — granted via
  `tclaude agent permissions grant <caller> agent.<verb>`), OR
- they own at least one group that contains the target (mirrors how
  `tclaude agent message` already special-cases group owners). For
  `--group`, ownership of THAT group is the bypass.

The response includes `caller_conv` so the target's audit trail
records who acted. For `reincarnate`, the handoff message uses
**your conv-id** as the sender, so the new agent sees `In-Reply-To`
pointing at you and can reply directly.

Notes vs. self variants:

- `context-info --target` / `--group` are **read-only** and work on
  offline agents too — they read the last-persisted context snapshot, so
  no alive tmux session is required (a dead agent still reports "it died
  at 80%"). The mutating verbs below do need a live pane.
- For the mutating verbs (`compact`, `reincarnate`, `clone`) the target
  must have an alive tmux session — if it's offline,
  `tclaude agent groups resume` it first.
- `--ask-human` is **not** honored on cross-agent calls; the manager
  pattern is opt-in via explicit grants, not a popup escape hatch.
- When deliberately using `reincarnate`, the disk-handoff convention (persist
  task state before reincarnating) applies to the TARGET, not you. This mechanic
  is not a reason to reincarnate a Codex worker for context pressure. Best
  handoff pattern:
  send the worker `"checkpoint your state to /tmp/<task>-notes.md"`
  first, wait for it to ack, then `agent reincarnate --target worker
  "reload /tmp/<task>-notes.md and continue"`.
- For Claude Code workers, you can chain multiple `compact` calls:
  `for w in worker-1 worker-2 worker-3; do tclaude agent compact --target $w "keep going"; done`.
  Each call is independent and idempotent-ish (if the worker is
  already mid-compact it's harmless).
