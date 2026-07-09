# Coding-Harness Integration 🤖✨

Powerful session and conversation management for agentic coding CLIs. tclaude is **harness-agnostic**: it drives [Claude Code](https://claude.ai/code) (the default) and, experimentally, [OpenAI Codex CLI](https://developers.openai.com/codex/cli) — see **[Harnesses](harnesses.md)**.

## Supported Platforms

| Platform                          | Status                |
|-----------------------------------|-----------------------|
| macOS                             | ✅ Fully supported     |
| Linux (native)                    | ✅ Fully supported     |
| WSL (Windows Subsystem for Linux) | ⚠️ Partial*           |
| Windows (native)                  | ❌ Not yet implemented |

*\* Clickable notifications only focus the correct window if the target Windows Terminal tab is already selected.*

## Features

- 🔌 **Multiple Harnesses** - Drive Claude Code (default) or OpenAI Codex CLI via `--harness`; the choice is persisted per conversation ([details](harnesses.md), experimental)
- 📺 **Session Management** - Run a harness in tmux sessions, attach/detach anytime
- 🔮 **Status Tracking** - See when Claude is working, idle, or waiting for input
- 📊 **Status Bar** - Rich statusline with context usage, rate limits, git links
- 🔔 **OS Notifications** - Get notified when sessions need attention (opt-in)
- 🔍 **Interactive Watch Modes** - Browse sessions and conversations with search, filtering, sorting
- 🧠 **Semantic Search** - Find conversations by meaning using local embeddings (Ollama)
- ⚡ **Session Indicators** - Know which conversations have active sessions (⚡ attached, ○ active)
- ⚙️ **Task Management** - Run multiple tasks automatically
- 🤝 **Agent Coordination** - Cross-session messaging, groups, agent spawn/clone/reincarnate, and scheduled nudges via `tclaude agent` + `agentd` (experimental)
- 📊 **Agent Dashboard** - Browser operations console for groups, agents, permissions, and cron jobs (experimental)
- 📱 **Remote Control** - Arm Claude Code's built-in Remote Access from the dashboard/CLI, default it per profile or group, and keep it across relaunches ([details](remote-control.md), Claude Code only)

## Installation

Installing tclaude is **two steps regardless of method**: install the binary, then run `tclaude setup` to configure it. Setup is not optional — without it tclaude has no Claude Code hooks, status bar, or notification handler, so session tracking and notifications won't work.

### Prerequisites

- [Go](https://go.dev/dl/) 1.26+ (for `go install`, and as a build dependency for the Homebrew formula)
- [tmux](https://github.com/tmux/tmux) — required for session management. `tclaude setup` offers to install it for you on macOS (via Homebrew) and prints package-manager hints on Linux. (Homebrew pulls in `tmux` automatically.)

### 1. Install the binary

=== "Homebrew"

    macOS / Linux. Pulls in `tmux` automatically and builds `tclaude` from
    source (the Go toolchain is fetched as a build dependency).

    ```bash
    brew install tofutools/tap/tclaude
    ```

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

Run this once after installing, no matter how you installed the binary:

```bash
# Baseline setup + the two extras most users want
tclaude setup --install-agent-skills --install-default-agent-permissions
```

The **baseline** always runs (you can't turn it off) and:

- Checks that tmux is installed (offers to install it on macOS)
- Installs hooks in `~/.claude/settings.json` for status tracking
- Installs the status bar for Claude Code's statusline
- Offers to enable Claude Code's fullscreen TUI (`"tui": "fullscreen"`) — tclaude runs Claude Code inside tmux, where the flicker-free alternate-screen renderer works best (only asked when you haven't already set `tui` yourself)
- Sets up clickable notifications for your platform (terminal-notifier on macOS, D-Bus + xdotool/kdotool on Linux, the `tclaude://` protocol handler on WSL)
- Asks if you want to enable desktop notifications

!!! tip "Using Codex CLI?"
    Install Codex's hooks (into `~/.codex/hooks.json`) with `tclaude setup --harness codex`. See **[Harnesses](harnesses.md)** for the full multi-harness guide.

### Optional extras

The `--install-*` flags add extras **on top of** the baseline — they don't replace it. All are idempotent, so re-running `tclaude setup` with different flags is safe.

| Flag | Adds | When you want it |
|------|------|------------------|
| `--install-agent-skills` | Materialises the bundled `agent-*` skills into `~/.claude/skills/` for Claude Code and both `~/.agents/skills/` and `$CODEX_HOME/skills` (default `~/.codex/skills`) for Codex CLI so agents know about the coordination commands. | Using [Agent Coordination](agent.md) |
| `--install-default-agent-permissions` | Grants the `self.*` permission slugs those skills exercise (`self.rename`, `self.compact`, `self.reincarnate`, `self.clone`, `self.schedule`, `self.remote-control`, `self.task`, `self.pr`, `self.tags`) as agent defaults. | Using [Agent Coordination](agent.md) |
| `--install-sandbox-hardening` | Adds the `sandbox.*` / `permissions.deny` entries that deny agents direct access to agentd's state. Append-only and idempotent. | Only if you run agents inside the [Claude Code sandbox](sandbox-hardening.md) |
| `--install-resume-threshold-override` | Writes a `claude_resume.threshold_minutes` override to `~/.tclaude/config.json` that suppresses Claude Code's interactive "Resume from summary" prompt for tclaude-spawned panes. Skip-if-set; never overwrites a value you configured. | If detached/scripted resumes hang on the resume chooser |
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
# Start Claude Code in a new tmux session (--harness claude is the default)
tclaude session new

# Or start a Codex CLI session
tclaude session new --harness codex

# Or resume an existing conversation (harness is remembered automatically)
tclaude session new --resume <conv-id>

# Interactive session browser
tclaude session watch

# Interactive conversation browser
tclaude conv watch
```

## Commands

| Command            | Description                                                |
|--------------------|------------------------------------------------------------|
| `session new`      | Start Claude in a tmux session                             |
| `session ls`       | List sessions (`-w` for interactive)                       |
| `session watch`    | Interactive session browser (shortcut for `session ls -w`) |
| `session attach`   | Attach to a session                                        |
| `session kill`     | Kill sessions                                              |
| `conv ls`          | List conversations (`-w` for interactive, `-g` for global) |
| `conv watch`       | Interactive conversation browser (shortcut for `conv ls -w`) |
| `conv search`      | Search conversation text                                   |
| `conv search-embeddings` | Semantic search by meaning (requires Ollama)         |
| `conv index-embeddings`  | Build/update semantic search index                   |
| `conv resume`      | Resume a conversation                                      |
| `conv delete`      | Delete a conversation                                      |
| `conv prune-empty` | Delete empty conversations                                 |

## Interactive Watch Mode Keys ⌨️

Both `session watch` (`session ls -w`) and `conv watch` (`conv ls -w`) support these keys:

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

- [Harnesses](harnesses.md) - Drive Claude Code or Codex CLI; the per-harness capability matrix (**experimental**)
- [Adding a Harness](adding-a-harness.md) - Contributor recipe for teaching tclaude a new coding CLI
- [Session Management](sessions.md) - Detailed session commands
- [Conversation Management](conversations.md) - Detailed conversation commands
- [Agent Coordination](agent.md) - Cross-session messaging, groups, lifecycle, and scheduling via `tclaude agent` + `agentd` (**experimental**)
- [Agent Dashboard](dashboard.md) - Browser operations console for the agent system (**experimental**)
- [Remote Control](remote-control.md) - Drive agents from your phone via Claude Code's built-in Remote Access; arm at spawn, default per profile/group (**Claude Code only**)
- [Remote Access](remote-access.md) - Reach the fleet dashboard over the network (LAN/mesh/tunnel) behind mTLS + a passphrase (**experimental**)
- [Sandboxing Agents](sandbox-hardening.md) - Operator guide: lock down the Claude Code sandbox so `agentd`'s coordination guardrail holds
- [Git Worktrees](worktrees.md) - Parallel development with multiple branches
- [OS Notifications](notifications.md) - Get notified when sessions need attention
- [Status Bar](status-bar.md) - Rich status bar for Claude Code's statusline
- [Semantic Search](semantic-search.md) - Search conversations by meaning
- [Task Management](tasks.md) - Run multiple tasks automatically
