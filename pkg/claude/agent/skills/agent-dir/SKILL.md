---
name: agent-dir
description: Report — or open a terminal in — the directory an agent is working in, via `tclaude agent dir`. Claude Code can't read its own "where am I building" state or spawn a terminal window from a tool; tclaude agentd tracks the most-recent dir you've edited files in (the PostToolUse hook records it) and, being outside your sandbox, can open a terminal there. Use when the user asks "what directory are you working in / building in", "where are you", "open a terminal here / in the repo", or "/agent-dir". Manager pattern: `tclaude agent dir <peer>` reports another agent's dir; `tclaude agent dir <peer> --open` opens a terminal in it.
---

# Reporting where you're working

Claude Code records a single launch `cwd` in its conversation jsonl —
the directory it was started in. But that's often *not* where you're
actually building: started in `~/git`, you might spend the whole
session editing files under `~/git/some-repo/pkg/...`. A tool can't
read either value back, and definitely can't open a terminal window.

`tclaude agent dir` fills both gaps. It asks the local `tclaude agentd`
daemon, which tracks three directories per agent:

- **current** — the directory of the most-recent file you *edited*
  (Edit / Write / NotebookEdit). The PostToolUse hook records it on
  every file edit. This is "where you're building" and is what `dir`
  prints by default.
- **worktree** (`--worktree`) — the git working-tree root that
  contains the current dir: a linked-worktree root, or the main repo
  root. Falls back to the start dir when the current dir isn't in a
  git repo. This is the right one when you want "the repo I'm in",
  not the deep subdir of the last file.
- **start** (`--start`) — where Claude Code was launched.

## Prerequisite: daemon must be running

If you see `Error: tclaude agentd is not running.`, ask the human to
start it:

```bash
tclaude agentd serve   # in a non-sandboxed terminal
```

## Reporting

```bash
tclaude agent dir              # current working dir of self
tclaude agent dir --worktree   # git worktree/repo root of self
tclaude agent dir --start      # launch dir of self
```

The output is the bare path, one line, nothing else — so it composes:

```bash
cd "$(tclaude agent dir)"    # (run by the human, not you)
```

If the daemon has no edit recorded yet — a fresh agent, or one that's
only been reading files and running commands — `current` falls back to
the launch dir. The JSON form (`source` field) distinguishes `hook`
(tracked) from `fallback` (no edit seen yet).

## Opening a terminal

```bash
tclaude agent dir --open               # terminal in the current working dir
tclaude agent dir --worktree --open    # terminal in the git worktree/repo root
tclaude agent dir --start --open       # terminal in the launch dir
```

You cannot spawn a terminal window yourself — it's not part of your
tool surface, and your sandbox wouldn't allow it. The daemon runs
outside the sandbox and does it for you: it opens your platform's
terminal emulator (Windows Terminal on WSL, gnome-terminal / konsole /
etc. on Linux, Terminal.app on macOS) with an interactive shell
already `cd`'d into the directory.

This is for when the **human** asked you to "open a terminal here" —
it pops a window on their desktop. Don't call it speculatively.

## Manager pattern: another agent's directory

Pass a selector (title, conv-id, or 8+-char prefix) to query a peer
instead of yourself:

```bash
tclaude agent dir worker-1            # worker-1's current working dir
tclaude agent dir worker-1 --open     # open a terminal in worker-1's dir
```

Reporting another agent's directory is read-only and ungated — the
same conv data the dashboard already shows. Opening a terminal in it
is likewise ungated; it's the human's machine and they asked.

## What can go wrong

- **`no_known directory` / not_found.** The agent has no session row
  yet — it wasn't started under tclaude, or the daemon has never seen
  a hook from it. There's nothing to report.
- **`current` equals `start`.** Expected for an agent that hasn't
  edited any files yet — the tracker only moves on Edit / Write /
  NotebookEdit, not on Read / Grep / Bash.
- **Stale after a big `cd`.** The tracker follows *file edits*, not
  shell `cd`. If you `cd` somewhere in Bash but don't edit a file
  there, `current` won't move. Edit a file and it catches up.

## Why a separate command

You're a tool-using agent: you can't introspect Claude Code's own cwd,
and you can't open OS windows. The daemon owns both the tracking (fed
by the hook) and the terminal side (outside your sandbox), so it does
what you can't.
