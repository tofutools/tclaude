# tclaude

## What is tclaude?

`tclaude` is a cross-platform CLI tool written in Go that extends Claude Code with session management, conversation utilities, and developer workflow features. 
It wraps Claude Code sessions in tmux for detach/reattach, provides conversation search/management, git-based conversation sync, usage tracking, a web terminal, 
and a custom status bar.

## Build & Test

```bash
go build ./...                    # Build all packages
go test ./...                     # Run all tests
go test ./pkg/claude/conv/...     # Run tests for a specific package
go vet ./...                      # Lint
go install .                      # Install locally
```

CI runs `go test ./...` and `go vet ./...` across Linux, macOS, and Windows (amd64 + arm64).

## Architecture

**Entry point:** `main.go` - call `pkg/claude.Cmd()` which builds the cobra command tree.

**Command framework:** Uses [cobra](https://github.com/spf13/cobra) via [boa](https://github.com/GiGurra/boa) (type-safe param wrappers). All commands use `boa.CmdT[ParamType]` with `common.DefaultParamEnricher()`.

**Package layout under `pkg/claude/`:**

| Package     | Purpose                                                                                                                                                                           |
|-------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `session`   | Core tmux-based session management (new, list, attach, kill, watch). Sessions stored in SQLite (`~/.tclaude/db.sqlite`). Hook callbacks update session status. |
| `conv`      | Conversation management (list, search, AI search, resume, copy, move, delete, prune). Reads Claude's `.jsonl` conversation files and `sessions-index.json`.                       |
| `git`       | **Experimental.** Git-based conversation sync across devices. Uses `~/.claude/projects_sync` as a separate git working directory. Subject to rewrite; no guarantees against data loss. |
| `worktree`  | Git worktree management for parallel Claude sessions on different branches.                                                                                                       |
| `stats`     | Activity statistics from Claude's `~/.claude/stats-cache.json`.                                                                                                                   |
| `usage`     | Standalone subscription usage limits via Anthropic OAuth API.                                                                                                                     |
| `statusbar` | Status bar output for Claude Code's statusline feature (hidden command, reads JSON from stdin). Uses rate limits from Claude Code's statusline input (>= 2.1.80).                 |
| `web`       | **Deprecated.** Web terminal server - serves tmux sessions via xterm.js + WebSocket. Claude Code now has built-in remote access.                                                    |
| `setup`     | One-time setup: installs hooks in `~/.claude/settings.json`, registers protocol handler, configures notifications.                                                                |
| `selftest`  | Hidden integration tests for manual verification of credentials and API access.                                                                                                   |

**Shared utilities under `pkg/claude/common/`:**

| Package     | Purpose                                                                               |
|-------------|---------------------------------------------------------------------------------------|
| `config`    | tclaude config file (`~/.tclaude/config.json`)                                        |
| `convops`   | Shared conversation operations (used by both `conv` and `convindex`)                  |
| `convindex` | Conversation index management                                                         |
| `db`        | SQLite store (`~/.tclaude/db.sqlite`) for session state and notification cooldown. WAL mode, pure-Go via `modernc.org/sqlite`. Auto-migrates legacy JSON files on first open. |
| `notify`    | Desktop notifications (D-Bus on Linux, terminal-notifier on macOS, PowerShell on WSL) |
| `table`     | Interactive sortable table UI using bubbletea                                         |
| `terminal`  | Terminal detection and window focus (platform-specific)                               |
| `usageapi`  | Anthropic OAuth usage API client (used by `usage` command and `selftest`, no longer used by statusbar) |
| `wsl`       | WSL detection and PowerShell path resolution                                          |

**`pkg/common/`:** Shared utilities (dirs, file locking, size parsing).

## Key patterns

- Platform-specific code uses Go build tags: `_linux.go`, `_darwin.go`, `_windows.go`, `_unix.go`
- Session state is stored in SQLite with WAL mode for concurrent access from hook callbacks
- Interactive list views (sessions, conversations) use bubbletea with the shared `table` package
- The status bar command is hidden (`cmd.Hidden = true`) - it's invoked by Claude Code's statusline feature, not directly by users
