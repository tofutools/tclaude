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

### Usage bar shows stale data

The Anthropic OAuth usage endpoint (`/api/oauth/usage`) rate limits aggressively — as few as ~5 requests per access token before returning 429. Once rate limited, the endpoint may stay blocked for hours with no `Retry-After` header.

Tracked upstream in [anthropics/claude-code#31637](https://github.com/anthropics/claude-code/issues/31637).

**Root cause:** Rate limits are per-access-token, not per-account. Refreshing the OAuth token resets the counter.

**Workaround (manual):** If usage data is stuck/stale, you can force a token refresh:

```bash
# Get your refresh token
cat ~/.claude/.credentials.json | grep -o '"refreshToken":"[^"]*"' | cut -d'"' -f4

# Refresh the token (replace YOUR_REFRESH_TOKEN below)
curl -s -X POST https://console.anthropic.com/v1/oauth/token \
  -H "Content-Type: application/json" \
  -d '{
    "grant_type": "refresh_token",
    "refresh_token": "YOUR_REFRESH_TOKEN",
    "client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
  }'
```

!!! warning
    Refresh tokens are **one-time use**. You must save both the new `access_token` and `refresh_token` from the response back into `~/.claude/.credentials.json`, or future refreshes will fail.

**Planned:** tclaude will implement automatic token refresh on 429 so this is handled transparently.
