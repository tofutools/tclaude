# Status Bar

A rich status bar for Claude Code's statusline feature.

## Overview

tclaude provides a status bar command that Claude Code calls automatically to display contextual information below the input area. It shows model info, git links, context usage, and subscription rate limits.

**Requires Claude Code >= 2.1.80.**

**Example output:**

```
o4.6(200k) ████░░░░▒▒ 42% | 5h ░░░░░░░░░░ 8% (3h41m) | 7d ░░░░░░░░░░ 5% (2d9h)
[main] | 🔗 https://github.com/user/project
```

## Setup

The easiest way to install:

```bash
tclaude setup
```

This adds the status bar configuration to `~/.claude/settings.json`. You can also install it manually:

```json
{
  "statusLine": {
    "type": "command",
    "command": "tclaude status-bar"
  }
}
```

Check if it's installed:

```bash
tclaude setup --check
```

## What It Shows

### Line 1: Context & Rate Limits

```
o4.6(200k) <bar> N% | 5h <bar> N% (timer) | 7d <bar> N% (timer) | sonnet <bar> N% (timer)
```

| Element | Description |
|---------|-------------|
| `o4.6` | Short model label (first letter lowercase + version) |
| `(200k)` | The context window the bar beside it is measured against — see [Which window the context bar measures](#which-window-the-context-bar-measures) |
| Context bar | Context window usage with compaction buffer indicator |
| `5h` | 5-hour rate limit utilization and reset timer |
| `7d` | 7-day rate limit utilization and reset timer |
| `sonnet` | 7-day Sonnet limit (only shown when > 0%) |
| `$N.NN` | Session cost (API plan only, shown when no rate limits) |

Rate limits come directly from Claude Code's statusline input (added in 2.1.80), so they're always fresh — no API calls or caching needed.

**Progress bars** are color-coded:
- Green: normal usage
- Yellow: moderate usage
- Red: high usage

**Context bar** includes a compaction buffer indicator (`▒▒`) showing the ~16.5% reserved for compaction. Color thresholds are adjusted relative to the effective usable space.

**Reset timers** show time until the limit resets: `(45m)`, `(3h30m)`, or `(2d9h)`.

#### Which window the context bar measures

The window in the model label is the one the percentage and the bar are relative to — the **effective** window, meaning the smaller of:

- the model's real context window, and
- any [auto-compaction window](sessions.md) pinned for this agent (`CLAUDE_CODE_AUTO_COMPACT_WINDOW`, set per spawn, by profile, or with `--auto-compact-window`).

Most agents pin nothing, so the marker just names the model's own window: `o4.6(200k)`, or `o5(1M)` on an extended-context model. Pin an agent to 450k of a 1M window and it reads `o5(450k)` — and `47%` there means 47% of 450k, i.e. nearly half-way to the next auto-compaction, not 47% of a million.

Only that one number is shown. Claude Code's own `used_percentage` is always measured against the model's *full* window even when a pin is in force, so on a pinned agent the raw figure would understate how close compaction is; tclaude re-bases it and the label says what it was re-based onto. The dashboard's context meter shows the same effective window, so the two never disagree.

The marker is omitted entirely when no window is known yet — no pin, and no `context_window_size` from Claude Code — leaving a bare `o4.6`.

### Line 2: Git Info

```
[branch] | 🔗 <url>
```

| Element | Description |
|---------|-------------|
| `[main]` | Current git branch (cyan) |
| `📂 /path/to/project` | Current working directory (shown when not in a git repo) |
| `🔗 <url>` | Git repo URL, branch diff URL, and/or PR URL |

**Git links** adapt to context:
- **On default branch:** shows the repo URL
- **On a feature branch:** shows a compare URL (`repo/compare/main...branch`)
- **With an open PR:** shows the PR URL

### Where the PR comes from

Everything the status bar asks git for is local. The pull request is the one
piece that needs a GitHub credential, and it has two paths:

- **With agentd's GitHub proxy configured**, the lookup goes through the daemon
  (`/v1/github/pr/list --head <branch>`), which holds the token. This is what
  keeps the PR link working in a pane whose sandbox denies `~/.config/gh` —
  the posture the proxy exists to make workable. It needs the
  `proxy.github.read` permission slug like any other proxy read, and the
  repository's remote still has to pass the operator's allow-list. See
  [git-proxy.md](git-proxy.md).
- **Otherwise**, it shells out to `gh pr view <branch>` with the pane's own
  credentials, exactly as it always has.

Both are best-effort: no PR, no `gh`, no grant, or no daemon all render the
same way — a compare URL instead of a PR URL, never an error.

## Usage Command

You can also check your subscription limits directly (uses the Anthropic OAuth API):

```bash
# Human-readable output
tclaude usage

# Raw JSON from the API
tclaude usage --json
```

## Caching

The status bar caches git data to stay fast (it runs after every assistant message):

| Data                            | Cache Location | TTL        |
|---------------------------------|----------------|------------|
| Git info (repo, branch)         | SQLite DB      | 15 seconds |
| PR lookup, via `gh`             | SQLite DB      | 15 seconds |
| PR lookup, via the agentd proxy | SQLite DB      | 90 seconds |

- Git cache is **per-repo** (keyed by repo root hash), so parallel sessions in different repos don't interfere
- The PR has its own clock because the two paths cost different things. A `gh`
  call is a local subprocess; a proxied one spends the **operator's** GitHub
  credential and writes an audit row, and a status line re-renders several
  times a second. 90 seconds is the same interval agentd already uses for its
  own dashboard PR resolution. A carried-forward result — including "this
  branch has no PR", which is the usual answer on a fresh branch — is
  republished with the time it was actually looked up, not the time the
  surrounding snapshot was gathered
- Under a `tclaude-layer` sandbox the cache moves to the pane's own `/tmp`,
  since the database is not writable there
- Rate limits, context window, cost, and model info come fresh from Claude Code on each invocation — no caching needed

## How It Works

Claude Code pipes JSON session data to the status bar command via stdin. The JSON includes model info, workspace directory, context window usage, cost, and rate limits. The status bar combines this with cached git data to render the output.

The command is hidden from `tclaude --help` since it's only meant to be called by Claude Code.
