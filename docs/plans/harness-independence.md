# Plan: harness independence — driving more than Claude Code

Status: investigation complete; implementation not started. Linear project
[`tclaude-harness-independence`](https://linear.app/johan-kjolhede/project/tclaude-harness-independence-00bba5a7cfda)
(JOH-149…163) is the live work tracker — this doc is the design intent.

> **Hand-off pointer:** if you are picking this up cold, read the **Plan**
> section below for the committed shape, then the **Knowledge pool** for the
> research it rests on (coupling inventory + Codex CLI facts). The plan is
> the locked-in part; the knowledge pool is reference, not commitments.

---

## Plan (committed shape)

Make tclaude **harness-agnostic**: it can manage sessions from multiple coding
harnesses, not just Claude Code (CC). **First additional harness: OpenAI Codex
CLI.** Other harnesses (Gemini CLI, Aider, open-source) are explicitly later —
the point of the abstraction is to make them a recipe, not a rewrite.

The core idea: introduce a **`Harness` seam** that captures everything that
differs between CC and Codex, refactor existing CC behavior behind it (no
behavior change), then add a `codex` implementation. Persist which harness each
session/conv belongs to and default it to `claude` so every existing row and
reader keeps working untouched.

Phased so each milestone is independently shippable and low-risk:

1. **M1 — Harness seam + read-only Codex conv-index.** Define the seam; add
   `harness` columns to `sessions` + `conv_index` (default `'claude'`); Codex
   rollout parser; `tclaude conv ls`/`search` enumerate Codex sessions. Observe
   only — no spawning.
2. **M2 — Spawn & resume Codex (CLI).** `tclaude session new --harness codex`;
   resume → `codex resume`; per-harness model/effort mapping; `CodexSim` flow
   test.
3. **M3 — Codex hooks & live status.** Install hooks into `~/.codex/config.toml`
   / `hooks.json` + hook-trust; per-harness `tclaude setup`; the existing
   hook-callback already parses Codex's payload; live status + notifications.
4. **M4 — agentd / agent lifecycle for Codex.** Daemon spawn/stop/resume;
   degrade reincarnate/clone/rename/compact via capability flags (titles stored
   tclaude-side where Codex lacks `/rename`).
5. **M5 — Dashboard & polish.** Harness badge; per-harness spawn menus;
   mixed-harness groups; docs + the "add a harness" recipe.

**Why it's tractable:** Codex's hooks were modeled on Claude Code's — same
3-level config shape, near-identical snake_case stdin payload, overlapping event
names. So tclaude's hook-callback layer and the agentd identity model (socket
peer creds, harness-agnostic) already generalize. The real work concentrates in
**spawn**, **conv-index scan/parse**, **hook-config install location**,
**persisting the harness type**, and **graceful lifecycle degradation**.

**Out of scope:** harnesses beyond Codex; tclaude stays an orchestrator, not a
work engine (tasks/tickets stay in Linear/Jira/GHI).

### Open questions (resolve via spikes, then update this doc)

- **Hook trust automation.** Codex requires non-managed command hooks to be
  trusted (`/hooks`) or declared managed via `requirements.toml`. Can `tclaude
  setup` register the callback as managed/trusted non-interactively, or is a
  one-time `/hooks` trust step unavoidable? (M3 / JOH-157)
- **Title model.** Codex has no `customTitle` turn. Plan: store titles
  tclaude-side in `conv_index.custom_title`. Worth adopting for CC too, or keep
  CC's round-trip-through-`.jsonl` behavior? (M4 / JOH-161 — don't change CC in
  this project.)
- **Effort ↔ reasoning mapping.** Is tclaude's effort concept 1:1 with Codex
  reasoning levels, or do we expose them as distinct per-harness knobs? (M2)
- **Compaction.** Does Codex expose a scriptable compaction command/flag, or is
  `agent compact` a no-op on Codex? (M4)
- **Naming/rename scope.** How far to rename `Claude*`/`buildClaudeCmd`/
  `TCLAUDE_*` internals — full rename at a natural rewrite point vs. leave
  consistent. Avoid a half-rename. (M5 / JOH-163)

---

## Knowledge pool (reference — not locked in)

Captured during the M0 investigation (2026-06-13). Verify against current code
before relying on file:line citations.

### A. tclaude ↔ Claude Code coupling inventory

Where tclaude assumes CC today, and what each becomes behind the seam:

| Coupling point | Where | CC assumption | Generalization |
|---|---|---|---|
| Binary + spawn flags | `pkg/claude/session/new.go` (`buildClaudeCmd`, `runNew`) | literal `claude`, `--resume/--effort/--model`, `sh -c` in tmux | `harness.BuildSpawnCmd(...)` |
| Identity env | `new.go` | `TCLAUDE_SESSION_ID`, `TCLAUDE_AUTO_COMPACT` | harness-agnostic; keep |
| Conv storage path | `common/convops/convops.go` (`ClaudeProjectsDir`, `PathToProjectDir`, `GetClaudeProjectPath`) | `~/.claude/projects/<encoded-cwd>/<id>.jsonl`, cwd → dir name | per-harness conv source: root + scan strategy |
| Conv parser | `convops.go` (`parseJSONLSession`, `SessionEntry`) | reads `type`=`summary`/`custom-title`, `summary`, `customTitle`, `cwd`, `gitBranch`, `sessionId` | per-harness `Parse(bytes) → SessionEntry` |
| Conv-index cache | `common/db/convindex.go`, `convops.LoadSessionsIndex` | scans one cwd-dir; SQLite `conv_index` cache keyed by project path | add `harness` col; merge sources |
| Hook install | `session/hooks.go` (`InstallHooks`, `RequiredHooks`, `ClaudeSettingsPath`) | writes `~/.claude/settings.json` `hooks` JSON | per-harness config target + serialization + trust |
| Hook callback | `session/hook_callback.go` (`HookInput`) | snake_case stdin JSON → SQLite status | already harness-agnostic; tag harness |
| Slash injection | `agentd/reincarnate.go`, `agentd/power.go` (`injectSlashCommand`) | `/rename`, `/compact`, `/exit` into tmux pane | gate on capability flags |
| Statusbar | `pkg/claude/statusbar/` | reads CC statusLine JSON from stdin; installs `statusLine` in settings.json | CC-only for now |
| Model/effort | `common/model.go`, `common/effort.go` (`ValidateModel`, `ValidateEffort`) | `claude-*` slugs, CC effort levels | per-harness validators |
| Process detection | `session/process_*.go`, `agentd/identity.go` | parent proc named `claude`/`node` | add `codex` |
| DB schema | `common/db/migrate.go` (head **v55**) | `sessions`/`conv_index` no harness col | add `harness TEXT NOT NULL DEFAULT 'claude'` |

`RequiredHooks` already covers: `UserPromptSubmit, Stop, StopFailure,
PermissionRequest, PreToolUse, PostToolUse, PostToolUseFailure, SubagentStart,
SubagentStop, Notification, SessionStart, SessionEnd, PostCompact`. That's a
near-superset of Codex's event set.

There is **no pre-existing harness abstraction** — "harness" appears only in
dashboard comments referring to the model/effort line. Clean slate.

### B. Codex CLI facts (sources below)

**Storage.** Rollout files at `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`
(+ `.jsonl.zst` for cold sessions; resume materializes back to plain `.jsonl`
to append). **Date-indexed, not cwd-indexed** — cwd lives inside the file. Each
line is a `RolloutLine{timestamp, item}` wrapping a `RolloutItem`:
- `SessionMeta` — `id`, `source`, `cwd`, `model_provider`, `cli_version`.
- `ResponseItem` — model responses + tool calls (role, content, tool outputs).
- `EventMsg` — `UserMessage`, `AgentMessage`, `AgentReasoning`, `TokenCount`,
  `TurnComplete`, lifecycle.
- `TurnContext` — model, approval_policy, sandbox_policy snapshot.
- `Compacted` — history-compaction output.
No `customTitle` concept → derive a display title from the first `UserMessage`.

**Commands.** `codex` (TUI), `codex exec`/`codex e` (headless, streams stdout or
JSONL), `codex resume <SESSION_ID>` / `codex resume --last` / `--all`. Flags:
`--model/-m`, `--profile/-p`, `-c/--config key=value`, `--cd/-C <path>`,
`--sandbox/-s {read-only|workspace-write|danger-full-access}`, `PROMPT` (or `-`
for stdin). No flag to retrieve the session id at start → learn it from the
`SessionStart` hook payload (`session_id`), same late-binding pattern CC uses.

**Hooks.** Configured in `~/.codex/config.toml` `[[hooks.Event]]` tables **or**
`~/.codex/hooks.json`; layered (user → repo `.codex/` → plugins → enterprise
`requirements.toml`). **Same 3-level shape as CC**: event → matcher → `hooks:
[{type:"command", command, statusMessage?, timeout?}]`. Events: `SessionStart,
SubagentStart, PreToolUse, PermissionRequest, PostToolUse, PreCompact,
PostCompact, UserPromptSubmit, SubagentStop, Stop` (**no** `Notification` /
`SessionEnd`).

**Hook stdin payload** (shared fields): `session_id`, `cwd`, `hook_event_name`,
`model`, `permission_mode` (`default|acceptEdits|plan|dontAsk|bypassPermissions`),
`transcript_path`, `turn_id`. Event-specific: PreToolUse adds `tool_name`,
`tool_use_id`, `tool_input`; PostToolUse adds `tool_response`. → **Matches
tclaude's existing `HookInput` struct** (`session_id`/`cwd`/`hook_event_name`/
`permission_mode`/`tool_name`/`tool_input`/`transcript_path`) almost
field-for-field. Output: exit 0 = continue; exit 2 + stderr = block; optional
JSON on stdout (`{continue, stopReason, systemMessage, suppressOutput}` plus
event-specific `hookSpecificOutput`/`decision`).

**Hook trust.** Non-managed command hooks require explicit trust (`/hooks` to
inspect/trust); managed hooks (from `requirements.toml`/MDM) are trusted by
policy. `codex exec` can run enabled hooks without persisted trust for that
invocation. → setup must resolve this (see open questions).

**Config.** `~/.codex/config.toml`, `--profile` → `~/.codex/<name>.config.toml`,
`CODEX_NON_INTERACTIVE=1`. Has MCP support, subagents, `/model`, image in/out,
web search, local code-review agent, approval modes.

### C. CC ↔ Codex capability matrix

| Capability | Claude Code | Codex CLI |
|---|---|---|
| Headless | `claude --print` | `codex exec` / `codex e` |
| Resume | `claude --resume <id>` | `codex resume <id>` / `--last` |
| Conv layout | cwd-indexed dir | date-indexed tree (cwd in file) |
| Hooks | settings.json `hooks` | config.toml `[hooks]` / hooks.json |
| Hook payload | snake_case | same field names (+ extras) |
| Hook trust | n/a | explicit/managed required |
| Notification event | yes | no → use `PermissionRequest` |
| SessionEnd event | yes | no → use `Stop` + proc-exit |
| Rename / customTitle | `/rename` → `.jsonl` turn | none → store tclaude-side |
| Compact | `/compact` | TBD (spike) |
| Model slugs | `claude-*` | `gpt-5.*` |
| MCP / subagents | yes | yes |

### D. Project constraints to honor (from tclaude memory/CLAUDE.md)

- **Migrations:** head is v55; grab the next free number, **renumber if a
  parallel branch lands first**; make it **idempotent** (pragma_table_info
  guard + single tx) per the v54 wedge.
- **Self-healing over backfill:** fill `harness` on rescan via the scanner +
  `UpsertConvIndex ON CONFLICT`, not a one-shot migration.
- **E2E sim tests for every feature** under `pkg/claude/agentd/*_flow_test.go`;
  add a `CodexSim` to testharness v2; encode Codex quirks in the sim.
- **send-keys is an injection sink** — gate every slash injection on the
  harness capability flag; cold-review any PR touching that path.
- **Status enum is scattered** across ~10 switches (incl. two
  `matchesShowFilter` funcs) — mapping Codex events touches all of them.

### E. Sources

- Codex storage/rollout: deepwiki openai/codex 3.3, 3.5.2; GH discussion #3827.
- Official docs: developers.openai.com/codex/cli, /codex/cli/reference,
  /codex/hooks, /codex/config-reference.
