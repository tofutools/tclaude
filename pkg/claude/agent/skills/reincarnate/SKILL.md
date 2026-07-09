---
name: reincarnate
description: >-
  Reincarnate NOW — replace yourself with a fresh successor that inherits
  your identity (groups, permissions, ownerships) via `tclaude agent
  reincarnate`. This is the do-it-now procedure: checkpoint your task
  state to a handoff file, then hand off. Use when the human types
  `/reincarnate` (optionally followed by a focus for the successor), asks
  you to reincarnate / hand off to a fresh instance, or you have already
  decided to act on a full context window. For the full lifecycle
  reference — compact vs. clone trade-offs, when-to-act policy, and the
  `--target` manager pattern — see the `agent-lifecycle` skill.
---

# /reincarnate — hand off to a fresh successor, now

You are about to replace yourself. The daemon spawns a fresh agent
instance, migrates your identity onto it — group memberships, per-conv
permission grants, group ownerships — and soft-stops this one. The
successor does **not** get your message history: it starts with only
what your follow-up hands it. That makes the handoff note the entire
interface between you and your successor — everything below is about
getting that right.

Not migrated: the conversation title (the successor can self-rename in
its follow-up if the name matters), the message history (that's the
point), and task state (you persist that to disk — step 2).

## Procedure

**1. Capture the focus.** If `/reincarnate` was invoked with text after
it, that is the successor's directive — build the handoff around it. If
not, the focus is "continue what I was doing", which makes step 2 do
the heavy lifting.

**2. Write a handoff notes file.** Before anything else, write a short
file (e.g. `/tmp/<task>-handoff.md`, or wherever the project's
`CLAUDE.md`/`AGENTS.md` says handoff notes go) covering:

- what the task is, and who asked for it
- what's done, and what's decided (with the *why* for non-obvious calls)
- what's in flight / broken right now
- what's next — concrete first actions, not vibes
- where things live: file paths with line numbers, branch names, PR
  links, relevant commands

Bullet points are fine; the note is for your successor, not the human.
Err on the side of writing it even when the follow-up feels
self-contained — you can't retroactively add what the successor turns
out to miss.

**3. Run the command.**

```bash
tclaude agent reincarnate --file /tmp/<task>-handoff.md
```

…or, for a short handoff, inline:

```bash
tclaude agent reincarnate "reload /tmp/<task>-handoff.md and continue from the 'Next' section"
```

The follow-up is **required** — the new pane comes up empty and would
otherwise sit idle. **Prefer `--file` for anything non-trivial**: an
inline follow-up passes through the shell first, which rewrites `$VAR`
and `$(…)` and eats backticks outright (`` `path` `` becomes a command
substitution). A file-sourced follow-up is read verbatim. `--file -`
reads stdin.

**4. Stop.** Once the command succeeds, do not start new work — your
pane is about to get the soft-exit. The response includes the new
conv-id and attach command; the human's terminal does *not* follow
automatically, so if the human is watching, the attach command is worth
surfacing.

## Tell the successor to stay lean

A common failure: the successor immediately re-reads every file the
predecessor had open and is back at 60% context before its first useful
turn. Write the handoff as a small navigable index — paths and line
numbers to expand *on demand* — and say so in the follow-up:
"reload only the notes file; pull in sources when a decision needs
them."

## Size and charset limits

Free-form prose always works. The delivery path — which the daemon
picks, not you — sets the limits:

- **Grouped agent**: the handoff rides the successor's inbox. Lenient —
  ≤16384 bytes, newlines and tabs fine, multi-paragraph briefs keep
  their structure.
- **Solo agent** (in no group): typed into the pane via tmux. Strict —
  ≤4096 bytes, single line, no control characters (each newline would
  submit early).

Write freely; if you turn out to be solo, the daemon rejects the
follow-up with a message telling you to single-line it.

## What can go wrong

| Symptom | Meaning / fix |
|---|---|
| `caller is not granted permission "self.reincarnate"` | The human hasn't opted in. Quote them: `tclaude setup --install-default-agent-permissions` (all self-lifecycle slugs) or `tclaude agent permissions grant default self.reincarnate`. |
| `tclaude agentd is not running.` | Ask the human to run `tclaude agentd serve` in a non-sandboxed terminal. |
| 504 spawn timeout | The new session produced no conv-id within ~30s. The pane may still come up — the human can `tclaude session attach <label>` to inspect. |
| `no_tmux` 503 | You're not running under tclaude; there is no pane the daemon can reach. Ask the human to wrap the session via tclaude. |

## Related

This skill is the imperative half. The **`agent-lifecycle`** skill is
the full reference: `compact` and `clone` and when each beats
reincarnate, `context-info` for reading your context %, the
when-should-I-act policy discussion, and the `--target <peer>` manager
pattern for reincarnating *another* agent.
