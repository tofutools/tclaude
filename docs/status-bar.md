# Status Bar

A rich status bar for Claude Code's statusline feature.

## Overview

tclaude provides a status bar command that Claude Code calls automatically to display contextual information below the input area. It shows model info, workspace details, git links, context usage, subscription limits, and extra usage status.

**Example output:**

```
[Opus 4.6 2.1.37] | /home/user/project | https://github.com/user/project
ctx ████░░░░▒▒ 42% | 5h ░░░░░░░░░░ 8% (3h41m) | 7d ░░░░░░░░░░ 5% (2d9h) | sonnet ░░░░░░░░░░ 0% (4d11h)
extra usage: off
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

### Line 1: Session Info

```
[Model Version] | Dir | Git Links
```

| Element | Description |
|---------|-------------|
| `[Opus 4.6 2.1.37]` | Model name and Claude Code version (cyan) |
| `📂 /path/to/project` | Current working directory |
| `🔗 <url>` | Git repo URL, branch diff URL, and/or PR URL |

**Git links** adapt to context:
- **On default branch:** shows the repo URL
- **On a feature branch:** shows a compare URL (`repo/compare/main...branch`)
- **With an open PR:** appends the PR URL

### Line 2: Usage & Limits

```
ctx <bar> N% | 5h <bar> N% (timer) | 7d <bar> N% (timer) | sonnet <bar> N% (timer)
```

| Element | Description |
|---------|-------------|
| `ctx` | Context window usage with compaction buffer indicator |
| `5h` | 5-hour rate limit utilization and reset timer |
| `7d` | 7-day rate limit utilization and reset timer |
| `sonnet` | 7-day Sonnet limit (premium/max only) |
| `$N.NN` | Session cost (API plan only, hidden on subscription plans) |

**Progress bars** are color-coded:
- Green: normal usage
- Yellow: moderate usage
- Red: high usage

**Context bar** includes a compaction buffer indicator (`▒▒`) showing the ~16.5% reserved for compaction. Color thresholds are adjusted relative to the effective usable space.

**Reset timers** show time until the limit resets: `(45m)`, `(3h30m)`, or `(2d9h)`.

### Line 3: Extra Usage

```
extra usage: off
```

or when enabled:

```
extra usage: on | 12.50 / 100.00 | <bar> 13%
```

Shows the overuse allowance status, credits used vs monthly limit, and utilization.

## Usage Command

You can also check your subscription limits directly:

```bash
# Human-readable output
tclaude usage

# Raw JSON from the API
tclaude usage --json
```

## Caching

The status bar caches data to stay fast (it runs after every assistant message):

| Data                        | Cache Location                         | TTL        |
|-----------------------------|----------------------------------------|------------|
| Git info (repo, branch, PR) | `~/.cache/tclaude/claude-git-<hash>.json` | 15 seconds |
| Subscription limits         | `~/.cache/tclaude/claude-usage.json`      | 15 seconds |

- Git cache is **per-repo** (keyed by repo root hash), so parallel sessions in different repos don't interfere
- Usage cache is **shared** since it's account-level data
- All cache writes are **atomic** (write to temp file + rename) to avoid corruption from parallel sessions
- Context window percentage, cost, and model info come fresh from Claude Code on each invocation
- **Eager refresh:** Hook callbacks automatically refresh the usage cache when Claude becomes idle, awaits permission, or awaits input — so the status bar shows fresh data right when you're looking at it

## How It Works

Claude Code pipes JSON session data to the status bar command via stdin. The JSON includes model info, version, workspace directory, context window usage, and cost. The status bar combines this with cached git data and subscription limits to render the output.

The command is hidden from `tclaude --help` since it's only meant to be called by Claude Code.

## Known Issues

### Usage API rate limiting (429)

The Anthropic OAuth usage endpoint (`/api/oauth/usage`) rate limits aggressively — as few as ~5 requests per access token before returning 429. Once rate limited, the endpoint may stay blocked for hours with no `Retry-After` header.

Tracked upstream in [anthropics/claude-code#31637](https://github.com/anthropics/claude-code/issues/31637).

**Root cause:** Rate limits are per-access-token, not per-account.

**Mitigation:** tclaude caches usage data to stay well under the rate limit. The cache TTL depends on your credential setup (see workaround below).

**If you hit 429 anyway:** Run `/login` inside Claude Code to get a fresh token.

### Workaround: Split credentials

The recommended fix is to give tclaude its own OAuth credentials, separate from Claude Code:

```bash
tclaude credentials split
```

This copies your current Claude Code credentials into `~/.tclaude/api-credentials.json` and removes them from Claude Code's store. Next time Claude Code starts, it will prompt you to log in again, creating a fresh independent token.

**After splitting:**
- tclaude and Claude Code each have their own access/refresh tokens — no more conflicts
- tclaude can safely refresh its own token on 429 without invalidating Claude Code's session
- Cache TTL drops from 5 minutes to 30 seconds, so the status bar updates much more frequently

**Check your current credential status:**

```bash
tclaude credentials status
```

> **Note:** If you haven't split credentials, tclaude uses Claude Code's shared token with a conservative 5-minute cache TTL and no automatic token refresh (to avoid invalidating Claude Code's in-memory token). You can also set `TCLAUDE_DEBUG_REFRESH=1` to force token refresh in shared mode, but this is not recommended for normal use.
