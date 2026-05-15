# tclaude

## What is tclaude?

`tclaude` is a cross-platform CLI tool written in Go that extends Claude Code with session management, conversation utilities, and developer workflow features. 
It wraps Claude Code sessions in tmux for detach/reattach, provides conversation search/management, usage tracking, a web terminal,
and a custom status bar.

## Build & Test

```bash
go build ./...                    # Build all packages
go test ./...                     # Run all tests
go test ./pkg/claude/conv/...     # Run tests for a specific package
golangci-lint run ./...           # Lint
go install .                      # Install locally
```

CI runs `go test ./...` and `golangci-lint run ./...` across Linux, macOS, and Windows (amd64 + arm64).

## Architecture

**Entry point:** `main.go` - call `pkg/claude.Cmd()` which builds the cobra command tree.

**Command framework:** Uses [cobra](https://github.com/spf13/cobra) via [boa](https://github.com/GiGurra/boa) (type-safe param wrappers). All commands use `boa.CmdT[ParamType]` with `common.DefaultParamEnricher()`.

**Package layout under `pkg/claude/`:**

| Package     | Purpose                                                                                                                                                                           |
|-------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `session`   | Core tmux-based session management (new, list, attach, kill, watch). Sessions stored in SQLite (`~/.tclaude/db.sqlite`). Hook callbacks update session status. |
| `conv`      | Conversation management (list, search, AI search, resume, copy, move, delete, prune). Reads Claude's `.jsonl` conversation files; SQLite (`conv_index`) is the source-of-truth cache. The legacy `sessions-index.json` file is written-but-never-read for external-tooling compatibility. |
| `agent`     | `tclaude agent` CLI — a thin client that talks to `agentd` over the Unix socket: messaging, groups, lifecycle (spawn/clone/reincarnate), cron, permissions, dashboard launch. Bundled `agent-*` skills live under `agent/skills/`. |
| `agentd`    | `tclaude agentd` daemon — HTTP-over-Unix-socket server that owns the DB, tmux nudges, permission gating, the approval popup, the browser dashboard, the cron scheduler, and the system tray. Identity from socket peer credentials. Flow tests in `*_flow_test.go`. |
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

## Testing

Two layers, both run under bare `go test ./...`:

- **Unit tests** sit next to the code they cover and exercise individual functions / handlers / DB ops in isolation.
- **Flow tests** live in `pkg/claude/agentd/*_flow_test.go` and exercise multi-step coordination (spawn → /rename → resume, reincarnate-of-r-N, clone alias derivation, delete cleanup) via the daemon's HTTP mux. The daemon, conv, agent, session — all production code paths run unchanged. Only the two subprocess boundaries are mocked.

**The two boundaries** (and only two) are interface vars in production source:

- `clcommon.Default Tmux` — the tmux command builder. `LiveTmux{}` runs real `tmux -L tclaude …`; tests assign a `*testharness.TmuxSim` that routes `send-keys` to a simulated CC instance.
- `agentd.Spawn Spawner` — `tclaude session new` invocations. `LiveSpawner{}` forks the real subprocess; tests assign a `simSpawner` that builds a `CCSim` + writes the SessionRow the production hook callback would have written.

Tests swap these in `flow_setup_test.go` with `t.Cleanup` restoration:

```go
prevTmux := clcommon.Default
clcommon.Default = m.Tmux
t.Cleanup(func() { clcommon.Default = prevTmux })
```

**Simulators** under `pkg/testharness/`:

- **`CCSim`** owns a real `.jsonl` under `~/.claude/projects/<encoded-cwd>/<convID>.jsonl`. Receives keystrokes via `Receive(text)`, buffers until `"Enter"` arrives, then dispatches through a handler list. Default handlers cover `/rename` (writes a `customTitle` turn), `/exit` (final user turn + flips alive=false), `/compact` (summary turn), and a fallback that writes a user turn. Tests register custom behaviors via `cc.OnInput(prefix, handler)` and async-process delays via `cc.SetCommandDelay(prefix, dur)`. Zero DB writes — CC's job is the `.jsonl`; the daemon owns SQLite.
- **`TmuxSim`** is a pure tmux substitute. `Command(args ...)` answers `has-session` against an alive flag, routes `send-keys` to the attached `CCSim.Receive`, models `kill-session`. Zero DB writes.
- **`Flow`** wraps a `World` with a Given/When/Then DSL — `HaveGroup`, `HaveAliveSession`, `Spawn`, `Reincarnate`, `Clone`, `Delete`, plus surface assertions like `AssertGroupMember`, `AssertSentContains`.

**Assertion philosophy:** verify at real surfaces — `GET /v1/groups/{name}/members` (what `tclaude agent groups members` would render), `conv.ListSessions(projectDir)` (what `tclaude conv ls` walks), `agent.FreshConvRowResolved` (what the dashboard refreshes through). The simulator's `.jsonl` is impl detail of the mock layer; the production read path is the system under test. New scenarios should reach for these surfaces, not poke `.jsonl` files directly.

When discovering a new CC or tmux quirk that bites in production, **encode it in the simulator** — `cc.OnInput` for behavior, `cc.SetCommandDelay` for timing — so the regression fails the relevant flow test. Over time the sims accrete the institutional knowledge of "things that have surprised us."

See `docs/plans/testharness-v2.md` for the full design.

## Active design / TODO docs

- `docs/plans/agent-coord.md` — design for `tclaude agent` (cross-session messaging, groups, inbox).
- `docs/plans/agentd.md` — design for `tclaude agentd` HTTP-over-Unix-socket daemon. Identity comes from socket peer credentials (`LOCAL_PEERPID` / `SO_PEERCRED`), not tokens; tmux delivery happens out-of-sandbox.
- `docs/plans/agents_todo.md` — root README explaining the layout below.

### TODO / DONE layout

```
docs/plans/
├── TODO/
│   ├── high-prio/   pick from here first
│   ├── med-prio/    worth doing, not blocking
│   └── future/      deferred / "if shows up in practice" / cross-machine
└── DONE/
    └── *.md         one shipped feature per file (no index — mod times tell the story)
```

**One file per coherent feature** on BOTH sides (kebab-case slug). Each TODO file is self-contained: it states what's open, briefly notes shipped context, lists relevant source files, and any open questions. **Pick up work by listing one tier dir** rather than loading the whole backlog into context.

After shipping, **move the TODO file into `DONE/`** — keep its filename, rewrite the body to describe what shipped (CLI surface, schema migrations, slugs, file paths, test scenarios, commit refs if known). One file per feature, NOT a single monolithic log. No index file: `ls -lt docs/plans/DONE/` orders by ship date, and `grep -rli` finds features by keyword. Reprioritise TODO files by `mv`-ing between tier dirs — the directory IS the priority.
