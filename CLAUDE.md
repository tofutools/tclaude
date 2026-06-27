# tclaude

## What is tclaude?

`tclaude` is a cross-platform CLI tool written in Go that extends agentic coding CLIs with session management, conversation utilities, and developer workflow features.
It wraps a coding harness's sessions in tmux for detach/reattach, provides conversation search/management, usage tracking, a web terminal,
and a custom status bar.

It is **harness-agnostic**: Claude Code is the default harness and OpenAI Codex CLI is the second supported one, selected per session via `--harness claude|codex` and persisted per conversation. The pluggable seam lives in `pkg/claude/harness` (see the [Harnesses](#harnesses) section). Much of the codebase still carries historical `Claude`/`claude`/`TCLAUDE_` prefixes in identifiers and on-disk env vars even though the code behind them is harness-agnostic — this is deliberate (see the naming note below), so do not read those names as "Claude-Code-only".

## Build & Test

```bash
go build ./...                    # Build all packages
go test ./...                     # Run all tests
go test ./pkg/claude/conv/...     # Run tests for a specific package
golangci-lint run ./...           # Lint
go install .                      # Install locally
```

CI runs `go test ./...` and `golangci-lint run ./...` across Linux and macOS (amd64 + arm64).

**Platform support:** tclaude supports Linux and macOS only. **Windows is not a supported target, and there are no plans to support it outside of WSL** — on Windows, run tclaude inside a WSL distribution (where it behaves as Linux). Some `*_windows.go` build-tagged files survive from earlier Windows support; they are vestigial and unmaintained — don't rely on them or treat Windows as a live target.

## Architecture

**Entry point:** `main.go` - call `pkg/claude.Cmd()` which builds the cobra command tree.

**Command framework:** Uses [cobra](https://github.com/spf13/cobra) via [boa](https://github.com/GiGurra/boa) (type-safe param wrappers). All commands use `boa.CmdT[ParamType]` with `common.DefaultParamEnricher()`.

**Package layout under `pkg/claude/`:**

| Package     | Purpose                                                                                                                                                                           |
|-------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `session`   | Core tmux-based session management (new, list, attach, kill, watch). Sessions stored in SQLite (`~/.tclaude/db.sqlite`). Hook callbacks update session status. |
| `conv`      | Conversation management (list, search, AI search, resume, copy, move, delete, prune). Reads Claude's `.jsonl` conversation files; SQLite (`conv_index`) is the source-of-truth cache. The legacy `sessions-index.json` file is written-but-never-read for external-tooling compatibility. |
| `harness`   | The harness-agnostic seam. A `Harness` descriptor composes capability-segregated contracts (`Spawner`, `ModelCatalog`, `Lifecycle`, `ConvStore`, `HookInstaller`, `SandboxCatalog`, `ApprovalCatalog`) + `Supports*`/`Can*` flags; a registry (`Register`/`Resolve`/`ResolveSpawnable`/`Names`) keyed by name. `claude.go` + `codex*.go` are the two implementations. Default = `claude`. See the [Harnesses](#harnesses) section. |
| `agent`     | `tclaude agent` CLI — a thin client that talks to `agentd` over the Unix socket: messaging, groups, lifecycle (spawn/clone/reincarnate), cron, permissions, export (`export show`/`submit` — deliver a dashboard-requested shareable artifact), dashboard launch. Bundled `agent-*` skills live under `agent/skills/`. |
| `agentd`    | `tclaude agentd` daemon — HTTP-over-Unix-socket server that owns the DB, tmux nudges, permission gating, the approval popup, the browser dashboard, the cron scheduler, per-agent export jobs (`export.go` — the async, agent-produced "📋 summary…" export, distinct from the group's synchronous `groups_export.go`), and the system tray. Identity from socket peer credentials. Flow tests in `*_flow_test.go`. |
| `worktree`  | Git worktree management for parallel Claude sessions on different branches.                                                                                                       |
| `memoryfiles` | `tclaude memory-files` — inspect/clean Claude's per-project memory markdown (`~/.claude/projects/<encoded>/memory/`, top-level `.md` only). Sibling-scan strategy: default = target repo's live git worktrees (precise); `--prefix` = encoded-name-prefix scan (catches removed-worktree leftovers, may over-match child/dotted dirs); `--no-siblings` = exact dir. Subcmds: `ls` (sizes/mtimes), `cat` (full contents, MEMORY.md first, separator banners), `clean` (delete with `--include`/`--exclude` globs, `--dry-run`/`-y`, to-delete/to-keep preview + confirm; also prunes the MEMORY.md index entries for the files it deletes), `prune-index` (sweep ALL dangling MEMORY.md entries — list items linking to a now-missing memory file, e.g. files deleted by hand or by Claude — without deleting any files; same scan flags + `--dry-run`/`-y` preview + confirm). |
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

**`pkg/common/`:** Shared utilities (dirs, file locking, size parsing, slog setup + size-based log rotation).

## Key patterns

- Platform-specific code uses Go build tags: `_linux.go`, `_darwin.go`, `_unix.go` (plus vestigial `_windows.go` files — Windows is not a supported target; see Build & Test)
- Session state is stored in SQLite with WAL mode for concurrent access from hook callbacks
- Interactive list views (sessions, conversations) use bubbletea with the shared `table` package
- The status bar command is hidden (`cmd.Hidden = true`) - it's invoked by Claude Code's statusline feature, not directly by users

## Harnesses

tclaude drives more than one coding harness. **Claude Code** is the default; **OpenAI Codex CLI** is the second. User docs: `docs/harnesses.md` (overview + capability matrix) and `docs/adding-a-harness.md` (contributor recipe). Design + research live in the `tclaude-harness-independence` Linear project.

**The seam (`pkg/claude/harness`)** is deliberately *not* one monolithic interface — the same feature is distributed differently per harness (a rename is `/rename` → `.jsonl` turn for CC, but an out-of-band title-store write for Codex). So it models focused, capability-segregated contracts composed by a `Harness` descriptor with `Supports*`/`Can*` capability flags (a `nil` sub-contract = "unsupported"; callers gate on the flag and degrade gracefully). Contracts: `Spawner` (launch/resume command), `ModelCatalog` (validate model/effort), `Lifecycle` (in-pane slash tokens — `RenameCommand`/`CompactCommand`/`SoftExitCommand`, `""` = unsupported), `ConvStore` (assemble conversations from the harness's full storage model — *not* "parse the one file"), `HookInstaller` (install/check/repair the callback + trust), `SandboxCatalog` + `ApprovalCatalog` (Codex launch-time `--sandbox` / `--ask-for-approval`, both `nil` for CC).

**Key facts for working in this area:**
- The harness is **persisted per conversation** (`harness TEXT NOT NULL DEFAULT 'claude'` on `sessions` + `conv_index`). Every lifecycle op resolves the conv's recorded harness via `harnessForConv` / `harness.Resolve` and does the right thing; spawn tags the row, everything else reads it.
- **CC's `HookInstaller` is attached in the `session` package** (`session/hook_installer.go`, via an `init()` that sets `Default().Hooks = ccHookInstaller{}`), not in `claude.go` — it wraps `InstallHooks`/`CheckHooksInstalled`/`ClaudeSettingsPath`, kept in `session` to avoid an import cycle. Codex's installer lives in the harness package (`codex_hooks.go`).
- **send-keys is an injection sink.** In-pane slash injections (`/rename`, `/compact`, `/exit`, `/quit`) go through `agentd`'s `deliverRename` / `injectSlashCommand`, gated on the `Supports*` flag *and* charset-gated (titles via `isValidRenameTitle` / the length-exempt `isValidRenameSink`). Lifecycle tokens are compile-time constants — never interpolate user input into them. Cold-review any PR touching this path.
- **The hook callback (`HookCallbackInput`) is already harness-agnostic** — it parses CC's and Codex's snake_case stdin field-for-field, so live status + notifications are a shared pipeline.
- **`SpawnBinaries()`** drives the process-tree walk that recognises a hook callback's harness ancestor, so a newly-registered spawnable harness is matched without editing that walk.

### Naming: historical `Claude`/`claude`/`TCLAUDE_` prefixes are intentional

A deliberate decision (JOH-163): identifiers like `buildClaudeCmd`, `FindClaudePID`, `ClaudeProjectsDir`, and env vars like `TCLAUDE_SESSION_ID` keep their `Claude`-flavored names even though the code behind them is harness-agnostic. They are **historical, not Claude-Code-specific** — they operate on whatever harness a conversation records. A mass rename was rejected as high-churn / low-value / half-rename-risk. **Rule:** only rename one at a *clean, contained, natural* rewrite point; a broader rename belongs in its own focused PR + review, never smuggled into a feature change. Don't "fix" these prefixes opportunistically.

## Testing

Two layers, both run under bare `go test ./...`:

- **Unit tests** sit next to the code they cover and exercise individual functions / handlers / DB ops in isolation.
- **Flow tests** live in `pkg/claude/agentd/*_flow_test.go` and exercise multi-step coordination (spawn → /rename → resume, reincarnate-of-r-N, clone title derivation, delete cleanup) via the daemon's HTTP mux. The daemon, conv, agent, session — all production code paths run unchanged. Only the two subprocess boundaries are mocked.

**The subprocess boundaries** (and only these) are swappable vars in production source:

- `clcommon.Default Tmux` — the tmux command builder. `LiveTmux{}` runs real `tmux -L tclaude …`; tests assign a `*testharness.TmuxSim` that routes `send-keys` to a simulated CC instance.
- `agentd.Spawn Spawner` — `tclaude session new` invocations. `LiveSpawner{}` forks the real subprocess; tests assign a `simSpawner` that builds a `CCSim` + writes the SessionRow the production hook callback would have written.
- `agentd.runPluginShell` — the Plugins tab's step executor (`sh -c <check/run>`). Production runs the human's own shell commands; tests stub it only when a test would otherwise execute real external tools (e.g. the catalog's `docker`/`claude` probes) — hermetic commands like `true`/`touch` go through the real path.

Tests swap these in `flow_setup_test.go` with `t.Cleanup` restoration:

```go
prevTmux := clcommon.Default
clcommon.Default = m.Tmux
t.Cleanup(func() { clcommon.Default = prevTmux })
```

**In-process `session` seams (a distinct, narrow category).** The "(and only these)" rule above scopes the *subprocess* boundaries. The `session` package additionally exports a couple of in-process `…ForTest` swaps — `SetRotateAgentConvForTest` (inject a one-shot transient `db.RotateAgentConv` failure to drive the post-`/clear` identity-migration retry path) and `SetClearInjectTimingsForTest` (shrink the `/clear` readiness-poll knobs so flow tests don't sit on the production delay). They live in a regular `.go` file (not `_test.go`) only because `_test.go` exports reach just the same package's test binary, and these are exercised from `agentd` flow tests in another package. They are **fault-injection / timing knobs, not subprocess mocks**, and are sanctioned for that use — keep any new ones rare, `…ForTest`-suffixed, and production-unreachable.

**Simulators** under `pkg/testharness/`:

- **`CCSim`** owns a real `.jsonl` under `~/.claude/projects/<encoded-cwd>/<convID>.jsonl`. Receives keystrokes via `Receive(text)`, buffers until `"Enter"` arrives, then dispatches through a handler list. Default handlers cover `/rename` (writes a `customTitle` turn), `/exit` (final user turn + flips alive=false), `/compact` (summary turn), and a fallback that writes a user turn. Tests register custom behaviors via `cc.OnInput(prefix, handler)` and async-process delays via `cc.SetCommandDelay(prefix, dur)`. Zero DB writes — CC's job is the `.jsonl`; the daemon owns SQLite.
- **`TmuxSim`** is a pure tmux substitute. `Command(args ...)` answers `has-session` against an alive flag, routes `send-keys` to the attached `CCSim.Receive`, models `kill-session`. Zero DB writes.
- **`Flow`** wraps a `World` with a Given/When/Then DSL — `HaveGroup`, `HaveAliveSession`, `Spawn`, `Reincarnate`, `Clone`, `Delete`, plus surface assertions like `AssertGroupMember`, `AssertSentContains`.

**Assertion philosophy:** verify at real surfaces — `GET /v1/groups/{name}/members` (what `tclaude agent groups members` would render), `conv.ListSessions(projectDir)` (what `tclaude conv ls` walks), `agent.FreshConvRowResolved` (what the dashboard refreshes through). The simulator's `.jsonl` is impl detail of the mock layer; the production read path is the system under test. New scenarios should reach for these surfaces, not poke `.jsonl` files directly.

When discovering a new CC or tmux quirk that bites in production, **encode it in the simulator** — `cc.OnInput` for behavior, `cc.SetCommandDelay` for timing — so the regression fails the relevant flow test. Over time the sims accrete the institutional knowledge of "things that have surprised us."

## Code review

CodeRabbit reviews every PR automatically, but it is frequently rate-limited or out of usage credits. When that happens its status check still goes **green** — but as a no-review *skip*, not a review or an approval. A green CodeRabbit check does not by itself mean the PR was reviewed.

When CodeRabbit has not produced a real review, do an **independent review** before merge:

- The reviewer must be a **fresh agent** — a local sub-agent, or a spawned review agent — that sees the PR diff **cold**: given only the diff and a review instruction, not the design backstory or how the change was built. The point is a review uncorrelated with the author's assumptions, so it catches what the author already rationalised away.
- Triage its findings the same way CodeRabbit's would be: fix the valid ones, document any deliberate skips.

## Agent group / worker policy

`tclaude` is built by a multi-agent group ("tclaude-dev"): a human operator, a PO (product-owner) coordinating agent, and dev/worker agents. The worker policy:

- **One dev/worker agent per feature.** Each worker owns a single feature and stays focused on it.
- **Same-feature follow-ups reuse that agent.** Follow-up work on the same feature — or something very similar — goes back to the agent that did the original task; it still has the context.
- **Unrelated work goes to a fresh agent.** A different feature, or a more unrelated task, gets a new agent with its own brief — never an existing agent carrying foreign context.
- **Idle agents are cheap.** A finished worker can sit idle in the group at low cost; there is no need to retire it promptly.
- **The operator prunes idle agents.** Retiring/removing agents from the group is the human operator's call. The PO may *recommend* cleanups or work-org changes at any time, but does not retire agents on its own initiative.

## Work tracking

tclaude-dev's work tracker is an external Linear board, not this repo. The actual
board/team and access details live in the operator's **private Claude Code project
memory** (deliberately not committed here, to avoid leaking internal locations). A
fresh agent picking up coordination should read its memory for the current Linear
setup, and keep the board current as work ships.

Design intent, research, and roadmaps live in Linear (e.g. the
`tclaude-harness-independence` project), not in-repo — this repo carries code,
the user docs under `docs/`, and inline rationale in code comments. Referencing a
Linear issue ID in a comment is welcome when a relevant one happens to exist, but
it is **optional** — not a requirement, and never a review blocker. (This is a
personal project; don't treat occasional Linear references as a coding guideline.)
