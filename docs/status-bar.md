# Status Bar

A rich status bar for Claude Code's statusline feature.

## Overview

tclaude provides a status bar command that Claude Code calls automatically to display contextual information below the input area. It shows model info, git links, context usage, and subscription rate limits.

**Requires Claude Code >= 2.1.80.**

**Example output:**

```
o4.6 ████░░░░▒▒ 42% | 5h ░░░░░░░░░░ 8% (3h41m) | 7d ░░░░░░░░░░ 5% (2d9h)
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
o4.6 <bar> N% | 5h <bar> N% (timer) | 7d <bar> N% (timer) | sonnet <bar> N% (timer)
```

| Element | Description |
|---------|-------------|
| `o4.6` | Short model label (first letter lowercase + version) |
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
- Rate limits, context window, cost, and model info come fresh from Claude Code on each invocation — no caching needed

## How It Works

Claude Code pipes JSON session data to the status bar command via stdin. The JSON includes model info, workspace directory, context window usage, cost, and rate limits. The status bar combines this with cached git data to render the output.

The command is hidden from `tclaude --help` since it's only meant to be called by Claude Code.
