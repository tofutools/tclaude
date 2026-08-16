# Utilities

Small tools that round out everyday use: the status line, activity and
subscription reporting, Claude Code memory-file management, database
inspection, and the Claude Code context knobs.

## Status line

### Claude Code: command-backed status bar

`tclaude setup` offers to install tclaude's status bar as Claude Code's
command-backed statusline (`statusLine: {type: command, command: "tclaude
status-bar"}` in `~/.claude/settings.json`). Requires Claude Code >= 2.1.80,
which delivers rate limits in the statusline input so no extra API polling is
needed.

Line one shows the model label with its effective context-window marker
(re-based onto a pinned [`--auto-compact-window`](#auto-compact-window) when
set, matching the dashboard meter), a context bar with a compaction-buffer
indicator, 5-hour and 7-day rate-limit bars with reset timers, a 7-day Sonnet
bar when it is above zero, and session cost on API plans. Line two shows the
git branch and working directory plus a repo/compare/PR link. Git data is
cached per repo with a short TTL, so the bar stays cheap.

### Codex CLI: curated built-in items

Codex CLI has no command-backed status line, so tclaude cannot install its own
renderer there. Instead, `tclaude setup` (or `tclaude setup --harness codex`)
curates Codex's *built-in* footer items in `~/.codex/config.toml` under
`[tui] status_line`: model with reasoning level, context remaining, git
branch, five-hour limit, weekly limit, and thread id.

The managed value is guarded by a `# tclaude:managed-status-line` marker
comment. Delete the marker to own the setting yourself — tclaude never
clobbers a user-managed value.

Codex context and token telemetry for tclaude's own meters comes from Codex's
durable rollout `token_count` events rather than from the footer, so the
dashboard's Codex context figures work whether or not the curated items are
installed.

OpenCode shows only its own TUI status, and Copilot CLI has no status line at
all; see the [capability matrix](harnesses.md#capability-matrix).

## tclaude stats

Claude Code activity stats, read from Claude Code's own
`~/.claude/stats-cache.json`: sessions, messages, tokens, daily activity, and
per-model usage/cost.

```bash
tclaude stats            # last 7 days
tclaude stats -d 30      # last 30 days
tclaude stats -t         # token detail
tclaude stats -j         # raw cache as JSON
```

The data source is Claude Code's; Codex, OpenCode, and Copilot activity is
not included here — the [dashboard](dashboard.md) is where cross-harness
usage lives.

## tclaude usage

Anthropic subscription limits: queries the Anthropic usage API with your
Claude Code OAuth credentials and prints 5-hour and 7-day utilization
percentages (plus whether extra usage is enabled). Responses are briefly
cached.

```bash
tclaude usage
tclaude usage --json     # raw API response
```

This command is Anthropic-subscription-specific. The
[dashboard](dashboard.md) separately surfaces usage for the other providers.

## tclaude memory-files

Inspect and clean Claude Code's per-project auto-memory markdown files under
`~/.claude/projects/<encoded-dir>/memory/`. Aliases: `mem-files`, `memory`.

```bash
tclaude memory-files ls           # list memory files for this project
tclaude memory-files cat          # print them, MEMORY.md first
tclaude memory-files clean        # remove them
tclaude memory-files prune-index  # drop dangling MEMORY.md index entries
```

By default the scope is the project **and its git worktree siblings**, because
they share an encoded-name prefix and can cross-poison one another's memory.
`--prefix` switches to plain encoded-name-prefix matching; `--no-siblings`
restricts to the exact directory.

Context: tclaude launches Claude Code sessions with auto memory **off** by
default (`CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`), precisely because agents
sharing a checkout would otherwise write into one shared per-project store.
Re-enable per launch with `--auto-memory`. See
[Harnesses](harnesses.md#claude-code).

## tclaude db

Inspects tclaude's own SQLite database at `~/.tclaude/data/db.sqlite`. (A
pre-split `~/.tclaude/db.sqlite` is auto-relocated into `data/`; the command's
help text still prints the legacy path, but the `data/` path is the real one.)

```bash
tclaude db schema              # canonical CREATE statements
tclaude db schema --json       # structured per-table columns and FKs
tclaude db schema --relations  # identity audit: conv-id vs agent-id keying
```

The command operates on the live database and migrates it to the current
schema version first.

## Startup-context trimming

Claude Code loads a fixed body of startup context into every session —
bundled skills, tool schemas, system-prompt blocks — sized for a
general-purpose assistant. A worker agent spawned for one narrow job has to
read past all of it. `--context-features` (on `tclaude session new` and
`tclaude agent spawn`, plus spawn profiles and the dashboard spawn dialog)
lets you choose per agent what loads.

Every feature in the catalog is in one of three states: **default** (tclaude
injects nothing; Claude Code and your `settings.json` decide), **trim**
(removed from this agent's startup context), or **keep** (stays, even if a
profile trimmed it). Nothing is trimmed unless you ask.

```bash
# A bare feature name means "trim"
tclaude agent spawn --group crew --name lean-worker \
  --context-features bundled-skills,workflows,artifact

# Keep one thing a lean profile trimmed
tclaude agent spawn --group crew --profile lean --name needs-artifacts \
  --context-features artifact=on

# List the catalog, with what each trim costs
tclaude session new --help-context-features
```

Resolution follows the usual stack — explicit request, then the group's
default profile, then the global default profile — except the tiers do not
merge: the most specific tier that says anything wins entirely, so one
profile always tells the whole story of what an agent loads.

Claude Code only; the other harnesses expose no equivalent switches, and
asking for a trim there is an error rather than a silent no-op.

## Auto-compact window

`--auto-compact-window` (Claude Code only) pins the context capacity that
Claude Code's auto-compaction reasons from, via
`CLAUDE_CODE_AUTO_COMPACT_WINDOW`. It accepts `450000`, `450k`, or `0.5M`;
unset uses the model's own threshold.

The point is long-lived agents on large-window models: pin the window below a
1M model's real capacity and the agent compacts while it is still sharp,
instead of degrading toward the end of an enormous window. Claude Code caps
the value at the model's actual window, and tclaude re-bases every context
meter, bar, and percentage — dashboard and
[status line](#status-line) alike — onto whichever is smaller, so what you
see always matches what compaction will act on.

Set it per launch, per spawn profile, or in the dashboard spawn dialog; like
other posture flags it is replayed on resume, reincarnate, and clone.
