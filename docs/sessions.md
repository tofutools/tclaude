# Session Management 📺

Run Claude Code in persistent tmux sessions with status tracking.

## Prerequisites

- **tmux** - Required for session management
- **Run setup** - For status tracking and notifications: `tclaude setup`

## Commands

### session new

Start Claude in a new tmux session.

```bash
# Start a new session in current directory
tclaude session new

# Start in a specific directory
tclaude session new -C /path/to/project

# Resume an existing conversation
tclaude session new --resume <conv-id>

# Start detached (don't attach immediately)
tclaude session new -d
```

**Flags:**

| Flag               | Description                                              |
|--------------------|----------------------------------------------------------|
| `-d, --detached`   | Start session without attaching                          |
| `-C, --dir <path>` | Directory to start the session in                       |
| `--resume <id>`    | Resume an existing conversation                          |
| `--label <name>`   | Custom label for the session                             |

### session ls

List active sessions.

```bash
# List sessions
tclaude session ls

# Interactive watch mode
tclaude session ls -w

# Include exited sessions
tclaude session ls -a

# Filter by status
tclaude session ls --show idle
tclaude session ls --hide exited
```

**Flags:**

| Flag           | Description                                  |
|----------------|----------------------------------------------|
| `-w, --watch`  | Interactive watch mode                       |
| `-a, --all`    | Include exited sessions                      |
| `--show <s>`   | Show only these statuses                     |
| `--hide <s>`   | Hide these statuses                          |
| `--sort <col>` | Sort by: id, directory, status, age, updated |

**Status values:** `idle`, `working`, `awaiting_permission`, `awaiting_input`, `error`, `exited`

### session attach

Attach to an existing session.

```bash
# Attach by session ID
tclaude session attach <id>

# Force attach (even if the session already has clients attached)
tclaude session attach -f <id>
```

### session kill

Kill one or more sessions.

```bash
# Kill a specific session
tclaude session kill <id>

# Kill all sessions
tclaude session kill --all

# Kill only idle sessions
tclaude session kill --idle

# Force (no confirmation)
tclaude session kill -f <id>
```

## Interactive Watch Mode

Press `w` or use `-w` flag to enter interactive mode.

### Navigation

| Key       | Action            |
|-----------|-------------------|
| `↑`/`k`   | Move up           |
| `↓`/`j`   | Move down         |
| `Enter`   | Attach to session |
| `q`/`Esc` | Quit              |

### Search

| Key      | Action                          |
|----------|---------------------------------|
| `/`      | Start search                    |
| `Esc`    | Clear search / exit search mode |
| `Ctrl+U` | Clear search input              |
| `↑`/`↓`  | Exit search and navigate        |

### Actions

| Key       | Action                           |
|-----------|----------------------------------|
| `Del`/`x` | Kill session (with confirmation) |
| `r`       | Refresh list                     |
| `h`/`?`   | Show help                        |

### Filtering

| Key     | Action               |
|---------|----------------------|
| `f`     | Open filter menu     |
| `Space` | Toggle filter option |
| `Enter` | Apply filter         |

### Sorting

| Key      | Action            |
|----------|-------------------|
| `1`/`F1` | Sort by ID        |
| `2`/`F2` | Sort by Directory |
| `3`/`F3` | Sort by Status    |
| `4`/`F4` | Sort by Age       |
| `5`/`F5` | Sort by Updated   |

Press the same key again to toggle ascending/descending/off.

## Session Status 🔮

Sessions report their status via Claude hooks:

| Status                | Color     | Description                          |
|-----------------------|-----------|--------------------------------------|
| `idle`                | 🟡 Yellow | Claude is waiting for input          |
| `working`             | 🟢 Green  | Claude is processing                 |
| `awaiting_permission` | 🔴 Red    | Needs permission approval            |
| `awaiting_input`      | 🔴 Red    | Waiting for user input               |
| `error`               | 🔴 Red    | Last turn ended in an error          |
| `exited`              | ⚫ Gray    | Session has ended                    |

## Pre-compact guard

The pre-compact guard **refuses Claude Code's own automatic compaction until context has grown past a floor.**

It exists because Claude Code can compact *too early*. CC sizes its compaction window from the model class, defaulting to **200K even on a 1M-context model**, so a 1M session can auto-compact at the 200K boundary — about **20% of the 1M status bar**. If you'd rather let context accrue and then [reincarnate](agent.md) (a directed handoff) than have CC blindly summarise at 20%, the guard holds compaction off until a chosen level.

How it works: tclaude installs a `PreCompact` hook that, when the guard is enabled, compares the conversation's used context against a per-window-size floor and returns a `block` decision to Claude Code if it's still below the floor. It is **fail-open** — if the guard is off, the trigger can't be classified, or the context snapshot is missing, compaction proceeds. It only ever *delays* an early compaction; it never forces one. By default it blocks only Claude Code's **automatic** compaction, never a `/compact` you type yourself (set `block_manual` to also guard manual compaction).

Enable it in `~/.tclaude/config.json` or via the dashboard **Config** tab:

```json
{
  "pre_compact_guard": {
    "enabled": true,
    "block_manual": false,
    "thresholds": [
      { "window_size": 200000,  "min_tokens": 150000 },
      { "window_size": 1000000, "min_tokens": 800000 }
    ]
  }
}
```

`thresholds` maps a context-window size (tokens) to the minimum used context (tokens) required before compaction is allowed on that window. Omit `thresholds` to use the built-in defaults shown above (hold off until 150K/200K and 800K/1M). The reported window is matched to the nearest configured size, so a slightly-off window (e.g. 1048576) still resolves to its class.

> Note: the guard refuses an *early* compaction; it does not move CC's trigger point. If CC re-attempts auto-compaction every turn past its boundary, the guard refuses each attempt (and logs it to `~/.tclaude/output.log`) until the floor is reached.

## Resume-from-summary prompt

When you resume a conversation that is **both old and large**, Claude Code shows an interactive *"Resume from summary"* chooser — a multiple-choice prompt offering to compact the session before resuming. That's fine when you're sitting at the keyboard, but it **breaks tclaude's scripted resume**: the daemon (and watch-mode resume) launch a detached `claude --resume` in a tmux pane and drive it with `send-keys`, and a tmux-driven flow can't answer a TUI it didn't expect — so the resume just hangs on the chooser.

tclaude suppresses the chooser for the panes it spawns by raising the thresholds Claude Code uses to decide whether to show it. CC only shows the prompt when **both** the session age (`CLAUDE_CODE_RESUME_THRESHOLD_MINUTES`, default 70) **and** the estimated size (`CLAUDE_CODE_RESUME_TOKEN_THRESHOLD`, default 100,000 tokens) are exceeded, so lifting **either** one high enough switches it off. tclaude applies these as environment variables on the spawned `claude` process **only** — it never writes them into `~/.claude/settings.json`, so your manual `claude` runs are untouched and the values live in tclaude's own config (where the dashboard **Config** tab and its diff viewer can edit them). The overrides are Claude-Code-specific; Codex CLI has no such prompt and ignores them.

Install the default suppression with either flag (idempotent — it skips if you've already configured a value, and never overwrites it):

```bash
tclaude setup --install-resume-threshold-override   # or: --install-all
```

That writes a large `threshold_minutes` (≈1000 years) so a resumed session's age can never reach it. Tune it by hand in `~/.tclaude/config.json` or via the dashboard **Config** tab:

```json
{
  "claude_resume": {
    "threshold_minutes": 525600000,
    "token_threshold": 100000
  }
}
```

Omit a field to leave that threshold on Claude Code's own default; set a small value (e.g. `0`) to make the prompt *always* show. The thresholds are undocumented, version-specific Claude Code knobs (verified against CC 2.1.187), so tclaude treats them as best-effort — if a future CC build renames or drops them the override simply becomes a no-op rather than an error.

## Tmux Integration

tmux is run with `-L tclaude` to create an isolated environemt and a namespace for sessions. 

```bash
# List all tclaude tmux sessions
tmux -L tclaude ls

# Manually attach
tmux -L tclaude attach -t abc123

# Detach from inside tmux
Ctrl+B D
```

### Recommended Tmux Configuration

There are two approaches for scroll support - **choose one, not both**:

#### Option 1: Tmux Mouse Mode

```bash
# Enable mouse support (scroll, click, resize panes)
set -g mouse on
```

Scroll wheel works inside tmux, but the native terminal scrollbar is hidden.

#### Option 2: Native Terminal Scrollbar

```bash
# Disable alternate screen buffer - keeps native scrollbar
set -ga terminal-overrides ',*256color*:smcup@:rmcup@'
```

Keeps your terminal's native scrollbar visible. This disables the `smcup` (enter alternate screen) and `rmcup` (exit alternate screen) terminal capabilities.

**Trade-off:** Full-screen applications (vim, less, etc.) will leave their content in your scrollback history instead of restoring the previous screen when they exit.

#### Why Not Both?

Using both options simultaneously causes conflicts - the scroll wheel behavior becomes unpredictable. Pick whichever suits your workflow better.

### Other Useful Settings

```bash
# Increase scrollback buffer (default is 2000)
set -g history-limit 10000
```

Reload config after changes: `tmux -L tclaude source ~/.tmux.conf`
