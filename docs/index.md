# Claude Code Integration 🤖✨

Powerful session and conversation management for [Claude Code](https://claude.ai/code).

![Demo](demo.gif)

*The demo shows `tofu claude` — tclaude was originally part of [GiGurra/tofu](https://github.com/GiGurra/tofu) and has since been extracted into its own repo. The commands are the same.*

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

## Installation

After installing tclaude, run the setup command:

```bash
# Install tclaude
go install github.com/tofutools/tclaude@latest

# Set up Claude integration (hooks, notifications, protocol handler)
tclaude setup
```

This will:
- Check that tmux is installed (required for session management)
- Install hooks in `~/.claude/settings.json` for status tracking
- Install the status bar for Claude Code's statusline
- Check for notification tools (terminal-notifier on macOS, xdotool on Linux)
- Register the protocol handler for clickable notifications (WSL)
- Ask if you want to enable desktop notifications

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
| `web`              | Serve a session via web terminal                           |
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
- [Git Worktrees](worktrees.md) - Parallel development with multiple branches
- [OS Notifications](notifications.md) - Get notified when sessions need attention
- [Status Bar](status-bar.md) - Rich status bar for Claude Code's statusline
- [Web Terminal](web-terminal.md) - Access sessions from your phone or browser
- [Semantic Search](semantic-search.md) - Search conversations by meaning
- [Git Sync](git-sync.md) - Sync conversations across devices (**experimental — may eat your data**)
- [Task Management](tasks.md) - Run multiple tasks automatically

## Recording a Demo

The `demo.tape` file is a [VHS](https://github.com/charmbracelet/vhs) script:

```bash
# Install VHS and dependencies
go install github.com/charmbracelet/vhs@latest
sudo apt-get install -y ffmpeg ttyd

# Record
cd docs/claude
vhs demo.tape  # Outputs demo.gif and demo.mp4
```
