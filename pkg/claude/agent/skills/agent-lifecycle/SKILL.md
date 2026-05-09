---
name: agent-lifecycle
description: Manage your own context window via `tclaude agent context-info`, `tclaude agent compact [follow-up]`, and `tclaude agent reincarnate [follow-up]`. Lets a long-running agent self-throttle on context pressure without the human babysitting. `compact` is a /compact slash-injection that preserves identity. `reincarnate` is the heavier path: the daemon spawns a fresh CC instance, migrates your identity (groups, permissions, ownerships) onto the new conv-id, and soft-stops the old one. Use periodically — at ~50% on a 1M context window or ~75% on a 200k window — to avoid context rot. Manager pattern: `tclaude agent reincarnate --target <peer> [follow-up]` reincarnates ANOTHER agent (requires the `agent.reincarnate` slug, OR being an owner of a group containing the target).
---

# Self-lifecycle: keep yourself fresh on long tasks

You have three commands for managing your own context window:

| Command                                   | What it does                                                                                       |
|-------------------------------------------|----------------------------------------------------------------------------------------------------|
| `tclaude agent context-info`              | Print the current context_pct + any pending /compact claim                                         |
| `tclaude agent compact [follow-up]`       | Inject `/compact` into your own pane; identity preserved. Optional follow-up prompt is queued.     |
| `tclaude agent reincarnate [follow-up]`   | Replace yourself with a fresh successor that inherits your identity (groups, permissions, ownership) |

`context-info` is read-only and self-targeted, so no permission slug.
`compact` and `reincarnate` are gated on `self.compact` and
`self.reincarnate` respectively. Both are default-granted by
`tclaude setup --install-default-agent-permissions` — see "Permission
setup" below.

The absolute token estimate (`~470k of ~1.0M`) assumes a **1M context
window** — the percentage is the authoritative signal that Claude Code
itself computes. On a 200k-window model the absolute number is off by
5x, but the percentage threshold-based decision is still correct.

## When to compact / reincarnate

Context rot is real: as the window fills you become slower, less
coherent, and more likely to forget early instructions. The thresholds
that work in practice depend on **how big your context window is**:

- **1M context window (Opus 4.7 1M and similar):** start considering
  compaction around **50%**. You can stretch to 60–70% on focused work,
  but past that the rot is noticeable. The window is large enough that
  even after compaction you have plenty of headroom.
- **200k context window (most other models):** push to **75%** or so.
  The window is small enough that compacting too early throws away
  useful context every few turns.

Either way, **don't wait until auto-compact triggers**. By then you're
already deep into the rotted regime. Pre-empt it: poll
`context-info`, and run `compact` once you're near your threshold.

## Compact vs. reincarnate

- `compact` — CC summarises the prior turns and replaces them with a
  short recap. **Default choice** for "I want to keep going on the
  same task without losing my place." Identity, conv-id, name, and
  most state survive; just the message history is abbreviated.
- `reincarnate` — heavier. The daemon spawns a brand new CC instance,
  migrates your identity (groups, per-conv permissions, ownerships)
  onto the new conv-id, and soft-stops the old one. The new agent
  comes up with a **clean context window** but the same identity. Use
  when:
  - You're switching to an unrelated task and prior context would
    actively interfere.
  - Compact has left you stuck — too much summary fluff, can't shake
    a wrong direction.
  - You're at the very tail of the context window and even a compact
    won't buy enough headroom.

## Persisting work *before* reincarnating

The daemon migrates **identity, not task state**. The new agent starts
with no memory of what you were working on. If you don't write down
where you were, the work is lost.

The convention this repo (and others should) adopt:

- Before reincarnating, write a short notes file describing where you
  are: what you decided, what's done, what's next, where the relevant
  files live with paths and line numbers.
- Pass the path of that file as your follow-up prompt: e.g.
  `tclaude agent reincarnate "reload /tmp/task-foo-notes.md and
  continue from the 'Next' section."`
- The project's `CLAUDE.md` (or equivalent) should document the
  expected location of these handoff notes so a freshly-reincarnated
  agent knows where to look without prompting from the human.

The notes file is for *you*, not the human — bullet points with paths
are fine, polish isn't required.

For `compact`, the same advice applies but with lower stakes: a
post-compact summary is lossy but not blank. Reincarnate is harder —
treat it like closing a tab with no recovery.

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
# context: 47% (~470k of ~1.0M tokens, assumes 1M window)
# compact: idle

# Approaching threshold — write down what matters
# (do this in your tools — Read/Edit/Write — not via tclaude)

# Compact in-place, with a follow-up that lands after the summary
tclaude agent compact "now reload /tmp/task-notes.md and continue"

# Or reincarnate to start fresh while keeping your identity
tclaude agent reincarnate "reload /tmp/task-notes.md and continue"
```

The follow-up prompt is optional. For `compact` it queues in the tmux
pty until CC resumes reading after the slash command settles —
**timing is not guaranteed**, the follow-up may land in a still-busy
textarea on unlucky timing. For `reincarnate` the follow-up is
delivered through the agent message-flush pipeline (or by direct
keystroke injection if you're not in any group) once the new pane is
ready, which is more reliable.

## Reincarnate: what gets migrated

The daemon migrates onto the new conv-id:

- Group memberships (with their alias / role / descr per group)
- Per-conv permission grants (the rows in `agent_permissions`)
- Group ownerships

What is **not** migrated:

- CC's conversation title (set via `/rename` inside CC). The new
  agent can self-rename in its follow-up if the human-readable name
  matters.
- CC's actual message history (that's the whole point — fresh
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

`compact` and `reincarnate` are opt-in. The fastest path is
`tclaude setup --install-default-agent-permissions`, which grants
`self.rename`, `self.compact`, and `self.reincarnate` as defaults in
one shot. (Kept separate from `--install-agent-skills` so upgrading
the on-disk skills doesn't re-add slugs you deliberately revoked.)

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
tclaude agent permissions grant <conv-id-or-alias> self.compact
tclaude agent permissions grant <conv-id-or-alias> self.reincarnate
```

If you see `Error: caller is not granted permission "self.compact"`,
the human has not opted in. Quote one of the commands above so they
know exactly what to run.

## Follow-up charset

Free-form prose works: punctuation, slashes, paths, quotes, unicode,
emoji. Two limits:

- **No control characters** (newlines, tabs, etc.). Each newline in
  `tmux send-keys` would land as a separate prompt-submit, fragmenting
  the follow-up. Use spaces or `;` between thoughts; reformat to a
  single line.
- **Max 4096 bytes** — keeps the tmux invocation reasonable.

Both limits are enforced by the daemon (the security boundary) and
mirrored client-side for fast errors.

## What can go wrong

- **Compact: follow-up landed on wrong screen.** If CC was mid-render
  when the follow-up keys arrived, they may have submitted prematurely
  or been treated as paste-mode (Enter becomes newline, no submit).
  If you depend on tight ordering, omit the follow-up and run a
  separate `tclaude agent` command on the next turn.
- **Reincarnate: human's terminal stays attached to the old session.**
  The old tmux session goes away when CC processes the soft `/exit`,
  so the human's terminal will see the pane close. They need to
  attach to the new session to follow you. The reincarnate response
  includes the attach command.
- **Reincarnate: spawn timeout.** If the new CC session doesn't
  produce a conv-id within ~30s, you get a 504. The spawned pane may
  still come up — the human can `tclaude session attach <label>` to
  inspect.
- **Mid-conversation typing is lost.** As with `compact` and `rename`,
  any text you'd typed but not submitted in the old pane is lost when
  it gets the `/exit` injection.
- **No live tmux session.** `no_tmux` 503 means you started CC
  outside `tclaude` and there's no pane the daemon can reach. Ask the
  human to wrap your session via tclaude.

## Why separate commands instead of just calling /compact

Slash commands inside the TUI aren't part of your tool surface. Even
if you wrote `/compact` in chat, CC would treat it as plain text. The
daemon owns the tmux side and is outside your sandbox, so it can do
the keystroke injection (and the cross-pane orchestration that
reincarnate needs) that you can't. Same architecture as
`agent-rename`.

## Manager pattern: reincarnate ANOTHER agent

`tclaude agent reincarnate` accepts an optional `--target <selector>`
that swaps the action onto a peer instead of yourself. The selector is
the same alias / conv-id / 8+-char prefix the rest of `tclaude agent`
accepts. Concrete shape:

```bash
tclaude agent reincarnate --target worker-1 \
  "rotted on the auth refactor; reload /tmp/auth-notes.md and pick up where you left off"
```

Auth model: the caller passes if EITHER

- they hold the `agent.reincarnate` slug (default human-only — granted
  via `tclaude agent permissions grant <caller> agent.reincarnate`), OR
- they own at least one group that contains the target (mirrors how
  `tclaude agent message` already special-cases group owners).

When the call succeeds the response includes `caller_conv` so the
target's audit trail records who reincarnated it. The handoff message
uses **your conv-id** as the sender, not the target's old conv —
that way the new agent sees `In-Reply-To` pointing at you and can
reply directly.

Notes vs. self-reincarnate:

- The target must have an alive tmux session — if it's offline,
  `tclaude agent groups resume` it first.
- `--ask-human` is **not** honored on cross-agent calls; the manager
  pattern is opt-in via explicit grants, not a popup escape hatch.
- Disk-handoff convention (persist task state before reincarnating)
  applies to the TARGET, not you. As manager you should make sure the
  worker has somewhere to dump notes, ideally before sending it the
  reincarnate request — the simplest pattern is to message the worker
  `"checkpoint your state to /tmp/<task>-notes.md"` first, wait, then
  reincarnate with a follow-up that says `"reload /tmp/<task>-notes.md
  and continue"`.
