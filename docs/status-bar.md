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
piece that needs a GitHub credential, and the bar does not hold one:

- **agentd is asked first.** The daemon resolves this branch's pull request for
  the dashboard's Branch column anyway, behind a 90-second cache, so
  `GET /v1/statusline/branch-pr` is a database read: no GitHub traffic, no
  credential spent, no permission grant, and no audit row on a surface that
  re-renders several times a second. It is also the only path that works in a
  pane whose sandbox denies `~/.config/gh`.

  agentd resolves branch links **on demand, not on a schedule** — the only two
  things that drive it are the dashboard's `/api/snapshot` (which runs only
  while a browser is polling it) and this route. So the status bar's own ask is
  what triggers the work when nobody has the dashboard open: the first ask on a
  cold cache returns nothing and schedules the resolution, and the next render's
  ask, ≤15 seconds later, gets the answer.
- **`gh pr view <branch>` is the fallback**, with the pane's own credentials,
  exactly as it always has been. A pull request is the only success: anything
  else — no daemon, a cold cache, a branch with no PR — falls through to `gh`,
  so an empty answer costs exactly what it cost before this route existed. The
  same ask that misses also schedules the daemon's resolution, so the next
  render's ask lands.

Neither the **directory** nor the **branch** is taken from the caller. The
daemon resolves both from the pane's own recorded location; the branch the
status bar sends is compared against that and then discarded, and a mismatch
answers with nothing rather than guessing. That is what lets the route carry no
permission slug: no caller-supplied value reaches the `gh` the resolution runs,
where a URL argument would re-aim it at another repository. A returned PR whose
URL does not belong to the repo the bar is rendering is discarded as well.

Both paths are best-effort: no PR, no `gh`, no daemon all render the same way —
a compare URL instead of a PR URL, never an error.

This is deliberately **not** the GitHub proxy. Routing it through
`tclaude proxy github` would spend the operator's credential, need the
`proxy.github.read` grant, and write an audit row per render. The proxy's own
`pr ls --head` remains the gated, audited way for an agent to ask GitHub
directly — see [git-proxy.md](git-proxy.md).

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

| Data                        | Cache Location | TTL        |
|-----------------------------|----------------|------------|
| Git info (repo, branch, PR) | SQLite DB      | 15 seconds |

- Git cache is **per-repo** (keyed by repo root hash), so parallel sessions in different repos don't interfere
- The PR rides that same 15-second snapshot. Asking agentd costs a cache read,
  so there is nothing to throttle harder — the daemon's own 90-second
  resolution interval is what actually bounds how often GitHub is reached
- Under a `tclaude-layer` sandbox the cache moves to the pane's own `/tmp`,
  since the database is not writable there
- Rate limits, context window, cost, and model info come fresh from Claude Code on each invocation — no caching needed

## How It Works

Claude Code pipes JSON session data to the status bar command via stdin. The JSON includes model info, workspace directory, context window usage, cost, and rate limits. The status bar combines this with cached git data to render the output.

The command is hidden from `tclaude --help` since it's only meant to be called by Claude Code.
