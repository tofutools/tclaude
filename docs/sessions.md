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
tclaude session new /path/to/project

# Resume an existing conversation
tclaude session new --resume <conv-id>

# Start detached (don't attach immediately)
tclaude session new -d
```

**Flags:**

| Flag              | Description                                              |
|-------------------|----------------------------------------------------------|
| `-d, --detach`    | Start session without attaching                          |
| `--resume <id>`   | Resume an existing conversation                          |
| `--label <name>`  | Custom label for the session                             |
| `--compact <pct>` | Auto-compact at this context usage percentage (see below) |

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
tclaude session ls --status idle
tclaude session ls --hide exited
```

**Flags:**

| Flag           | Description                            |
|----------------|----------------------------------------|
| `-w, --watch`  | Interactive watch mode                 |
| `-a, --all`    | Include exited sessions                |
| `--status <s>` | Show only this status                  |
| `--hide <s>`   | Hide this status                       |
| `--sort <col>` | Sort by: id, dir, status, age, updated |

**Status values:** `idle`, `working`, `awaiting-permission`, `awaiting-input`, `exited`

### session attach

Attach to an existing session.

```bash
# Attach by session ID
tclaude session attach <id>

# Force attach (detach other clients)
tclaude session attach -d <id>
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

| Status                | Color     | Description                 |
|-----------------------|-----------|-----------------------------|
| `idle`                | 🟡 Yellow | Claude is waiting for input |
| `working`             | 🟢 Green  | Claude is processing        |
| `awaiting-permission` | 🔴 Red    | Needs permission approval   |
| `awaiting-input`      | 🔴 Red    | Waiting for user input      |
| `exited`              | ⚫ Gray    | Session has ended           |

## Auto-Compact

Claude Code compacts context at ~83% usage (200K window) or ~95% (1M window) by default. With larger context windows, this can lead to context rot, higher costs, and slower responses. Auto-compact lets you trigger `/compact` at a lower threshold.

When enabled, the status bar tracks context usage and the Stop hook sends `/compact` via tmux when the threshold is exceeded. After compaction, the state resets so it can trigger again.

### Per-session (CLI flag)

```bash
# Compact at 50% context usage
tclaude --compact 50

# Also works with session new and task run
tclaude session new --compact 40
tclaude task run --compact 60
```

### Global (config file)

Set `auto_compact_percent` in `~/.tclaude/config.json`:

```json
{
  "auto_compact_percent": 50
}
```

The CLI flag overrides the config file, so you can set a global default and override per-session.

### Status bar

When auto-compact is configured, the status bar shows the threshold alongside context usage (e.g. `30%/50%`) and the context bar rescales so it fills completely as usage approaches the limit.

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
