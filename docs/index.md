# Claude Code Integration 🤖✨

Powerful session and conversation management for [Claude Code](https://claude.ai/code).

## Supported Platforms

| Platform                          | Status                |
|-----------------------------------|-----------------------|
| macOS                             | ✅ Fully supported     |
| Linux (native)                    | ✅ Fully supported     |
| WSL (Windows Subsystem for Linux) | ⚠️ Partial*           |
| Windows (native)                  | ❌ Not yet implemented |

*\* Clickable notifications only focus the correct window if the target Windows Terminal tab is already selected.*

## Features

- 📺 **Session Management** - Run Claude in tmux sessions, attach/detach anytime
- 🔮 **Status Tracking** - See when Claude is working, idle, or waiting for input
- 📊 **Status Bar** - Rich statusline with context usage, rate limits, git links
- 🔔 **OS Notifications** - Get notified when sessions need attention (opt-in)
- 🔍 **Interactive Watch Modes** - Browse sessions and conversations with search, filtering, sorting
- 🧠 **Semantic Search** - Find conversations by meaning using local embeddings (Ollama)
- ⚡ **Session Indicators** - Know which conversations have active sessions (⚡ attached, ○ active)
- ⚙️ **Task Management** - Run multiple tasks automatically
- 🤝 **Agent Coordination** - Cross-session messaging, groups, agent spawn/clone/reincarnate, and scheduled nudges via `tclaude agent` + `agentd` (experimental)
- 📊 **Agent Dashboard** - Browser operations console for groups, agents, permissions, and cron jobs (experimental)

## Installation

### Prerequisites

- [Go](https://go.dev/dl/) 1.26+ (for `go install`)
- [tmux](https://github.com/tmux/tmux) — required for session management. `tclaude setup` offers to install it for you on macOS (via Homebrew) and prints package-manager hints on Linux.

### 1. Install the binary

=== "go install"

    ```bash
    go install github.com/tofutools/tclaude@latest
    ```

=== "Prebuilt binary"

    Download the archive for your platform from the
    [Releases page](https://github.com/tofutools/tclaude/releases). The Linux builds are
    named `tclaude-no-cgo_linux_<arch>` and the macOS build `tclaude-darwin_darwin_arm64`.
    Extract it (the `tclaude` binary sits in a versioned subdirectory) and move it onto
    your `PATH`:

    ```bash
    tar -xzf tclaude-*.tar.gz
    sudo mv tclaude*/tclaude /usr/local/bin/
    ```

### 2. Run setup

```bash
# Baseline setup + the two extras most users want
tclaude setup --install-agent-skills --install-default-agent-permissions
```

The **baseline** always runs (you can't turn it off) and:

- Checks that tmux is installed (offers to install it on macOS)
- Installs hooks in `~/.claude/settings.json` for status tracking
- Installs the status bar for Claude Code's statusline
- Sets up clickable notifications for your platform (terminal-notifier on macOS, D-Bus + xdotool/kdotool on Linux, the `tclaude://` protocol handler on WSL)
- Asks if you want to enable desktop notifications

### Optional extras

The `--install-*` flags add extras **on top of** the baseline — they don't replace it. All are idempotent, so re-running `tclaude setup` with different flags is safe.

| Flag | Adds | When you want it |
|------|------|------------------|
| `--install-agent-skills` | Materialises the bundled `agent-*` skills into `~/.claude/skills/` so agents know about the coordination commands. | Using [Agent Coordination](agent.md) |
| `--install-default-agent-permissions` | Grants the `self.*` permission slugs those skills exercise (`self.rename`, `self.compact`, `self.reincarnate`, `self.clone`, `self.schedule`) as agent defaults. | Using [Agent Coordination](agent.md) |
| `--install-sandbox-hardening` | Adds the `sandbox.*` / `permissions.deny` entries that deny agents direct access to agentd's state. Append-only and idempotent. | Only if you run agents inside the [Claude Code sandbox](sandbox-hardening.md) |
| `--install-all` | Every extra above. | You want it all |

!!! note "Agent coordination needs the daemon running"
    The two agent extras only install skills and permissions. To actually use the
    coordination features you also run `tclaude agentd serve` in a non-sandboxed shell —
    see [Agent Coordination](agent.md) for the full picture.

### Verify

```bash
tclaude setup --check
```

## Quick Start 🚀

```bash
# Start Claude in a new tmux session
tclaude session new

# Or resume an existing conversation
tclaude session new --resume <conv-id>

# Interactive session browser
tclaude session ls -w

# Interactive conversation browser
tclaude conv ls -w
```

## Commands

| Command            | Description                                                |
|--------------------|------------------------------------------------------------|
| `session new`      | Start Claude in a tmux session                             |
| `session ls`       | List sessions (`-w` for interactive)                       |
| `session attach`   | Attach to a session                                        |
| `session kill`     | Kill sessions                                              |
| `web`              | Serve a session via web terminal (deprecated)              |
| `conv ls`          | List conversations (`-w` for interactive, `-g` for global) |
| `conv search`      | Search conversation text                                   |
| `conv search-embeddings` | Semantic search by meaning (requires Ollama)         |
| `conv index-embeddings`  | Build/update semantic search index                   |
| `conv resume`      | Resume a conversation                                      |
| `conv delete`      | Delete a conversation                                      |
| `conv prune-empty` | Delete empty conversations                                 |

## Interactive Watch Mode Keys ⌨️

Both `session ls -w` and `conv ls -w` support these keys:

| Key                | Action                          |
|--------------------|---------------------------------|
| `/`                | Start text search               |
| `s`                | Start semantic search           |
| `↑`/`↓` or `j`/`k` | Navigate                        |
| `Enter`            | Attach/create session           |
| `Del`/`x`          | Delete/kill (with confirmation) |
| `h` or `?`         | Show help                       |
| `Esc`              | Clear search / quit             |
| `q`                | Quit                            |

Session watch also supports:

| Key     | Action                  |
|---------|-------------------------|
| `f`     | Filter menu (by status) |
| `1`-`5` | Sort by column          |

## Documentation

- [Session Management](sessions.md) - Detailed session commands
- [Conversation Management](conversations.md) - Detailed conversation commands
- [Agent Coordination](agent.md) - Cross-session messaging, groups, lifecycle, and scheduling via `tclaude agent` + `agentd` (**experimental**)
- [Agent Dashboard](dashboard.md) - Browser operations console for the agent system (**experimental**)
- [Sandboxing Agents](sandbox-hardening.md) - Operator guide: lock down the Claude Code sandbox so `agentd`'s coordination guardrail holds
- [Git Worktrees](worktrees.md) - Parallel development with multiple branches
- [OS Notifications](notifications.md) - Get notified when sessions need attention
- [Status Bar](status-bar.md) - Rich status bar for Claude Code's statusline
- [Web Terminal](web-terminal.md) - Access sessions from your phone or browser (deprecated)
- [Semantic Search](semantic-search.md) - Search conversations by meaning
- [Task Management](tasks.md) - Run multiple tasks automatically
