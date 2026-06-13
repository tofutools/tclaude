# Plan: harness independence — driving more than Claude Code

Status: investigation complete; implementation not started. Linear project
[`tclaude-harness-independence`](https://linear.app/johan-kjolhede/project/tclaude-harness-independence-00bba5a7cfda)
(JOH-149…163) is the live work tracker — this doc is the design intent.

> **Hand-off pointer:** if you are picking this up cold, read the **Plan**
> section below for the committed shape, then the **Knowledge pool** for the
> research it rests on (coupling inventory + Codex CLI facts). The plan is
> the locked-in part; the knowledge pool is reference, not commitments.

## Workflow & branching (every agent: read first)

This project uses a **long-lived integration branch: `main-codex`** ("dual
mains"). `main` stays clean / Codex-free for the whole project so the operator
can switch between a Codex-enabled build (`main-codex`) and a plain build
(`main`) at will.

- **All project work targets `main-codex`, NOT `main`.** Each increment (a
  milestone slice / one issue) is its own small PR **merged into `main-codex`**.
- Do feature work in a **worktree branched off `main-codex`**
  (`git worktree add … -b <feature> main-codex`); open the PR with **base
  `main-codex`**.
- **`main-codex` → `main` is a much-later decision**, only once the whole
  feature set is robust end-to-end (CLI + data model + agentd + dashboard).
  No PR to `main` for this project without explicit operator sign-off.
- Force-pushing `main-codex` is fine (feature branch); `main` is not.

**Completeness bar:** a robust, fully-integrated feature set across all of
tclaude — not a partial/subset release.

---

## Plan (committed shape)

Make tclaude **harness-agnostic**: it can manage sessions from multiple coding
harnesses, not just Claude Code (CC). **First additional harness: OpenAI Codex
CLI.** Other harnesses (Gemini CLI, Aider, open-source) are explicitly later —
the point of the abstraction is to make them a recipe, not a rewrite.

The core idea: introduce a **set of focused, capability-segregated Go
interfaces** that hide harness specifics, refactor existing CC behavior behind
them (no behavior change), then add `codex` implementations. **Not one
monolithic `Harness` interface** — the same feature is distributed differently
across each harness's components, so we model the *contracts* (spawn a session,
list conversations, get/set a title, install hooks, validate a model, stop a
pane) and let each harness satisfy them however its storage/command model
dictates. A small `Harness` descriptor composes the pieces and exposes
capability flags for features a harness lacks. **Expect real refactors**, not
just shims — the operator has explicitly blessed this (2026-06-13).

Persist which harness each session/conv belongs to and default it to `claude`
so every existing row and reader keeps working untouched.

**Load-bearing principle — drop the "single `.jsonl` is the whole truth"
assumption.** tclaude today assembles a `SessionEntry` entirely from CC's one
conversation file (title, summary, cwd, gitBranch all parsed from it). Codex's
storage model is different and may split metadata across the rollout file +
a sidecar/index (its session **title/rename is shipped** — issue #22526 closed
COMPLETED — but the title likely does **not** live as a turn inside the rollout
`.jsonl` the way CC's `customTitle` does). So the conv-source contract is
"assemble a SessionEntry from *this harness's* storage model" (one file, many
files, file+sidecar, or file+DB) — never "parse the one file".

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
   Live status + notifications (JOH-159) is the *shared* pipeline: the
   hook-callback's event→status switch and `notify.OnStateTransition`
   (mute ladder + cooldown) are harness-agnostic, so Codex's event subset
   drives them unchanged — *given* the Codex hook payload parses into
   `HookCallbackInput` (JOH-157's contract). The two degradations need no
   new code path — needs-attention comes from `PermissionRequest` (no
   `Notification`), and exit from the reaper's tmux→PID liveness check (no
   `SessionEnd`). Notification banners are attributed per-harness
   ("Codex: …" vs "Claude: …").
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

### Interface decomposition (sketch — refine in M1)

Segregated contracts, composed by a `Harness` descriptor. Each is satisfied
per-harness however that harness's internal model distributes the work; some
are optional (capability detection via nil / a flag):

- `Spawner` — build the in-tmux launch command + env from a spawn spec; express
  the resume form (`claude --resume <id>` vs `codex resume <id>`).
- `ConvStore` — `ListConvs(cwd) → []SessionEntry`, `Resolve(idPrefix, cwd)`,
  `Title(convID)` / `SetTitle(convID, t)`. **Assembles from the harness's full
  storage model** (CC: one `.jsonl`; Codex: rollout file ± sidecar/index). The
  `Title` getter/setter is where the CC-vs-Codex difference hides — CC reads the
  `customTitle` turn / injects `/rename`; Codex reads/writes wherever its native
  rename persists, or tclaude falls back to `conv_index.custom_title`.
- `HookInstaller` — install/check/repair the tclaude callback in the harness's
  config target (settings.json vs config.toml/hooks.json) + any trust step.
- `HookEventMap` — map this harness's hook stdin payload + event names onto
  tclaude's internal status state machine (mostly shared; CC↔Codex payloads
  already align field-for-field).
- `ModelCatalog` — validate/normalize model + effort; list valid values for the
  spawn dialog.
- `Lifecycle` (capabilities) — `RenameCommand`, `CompactCommand`,
  `SoftExitCommand` tokens (or "unsupported" → tclaude-side fallback / no-op).
  Every slash injection is gated on these so no pane gets a command it can't
  parse.
- `StatuslineInstaller` — optional; CC only for now.

This is the "different distribution of functionality across mostly-equivalent
components" the operator flagged: e.g. *rename* is one `ConvStore.SetTitle`
contract, but CC implements it by injecting `/rename` (→ `.jsonl` turn) while
Codex implements it against its own title store.

### Open questions (resolve via spikes, then update this doc)

- **Hook trust automation.** Codex requires non-managed command hooks to be
  trusted (`/hooks`) or declared managed via `requirements.toml`. Can `tclaude
  setup` register the callback as managed/trusted non-interactively, or is a
  one-time `/hooks` trust step unavoidable? (M3 / JOH-157)
- **Title model + persistence (spike).** Codex CLI rename **is shipped** (issue
  #22526, COMPLETED) but the title almost certainly does **not** live as a turn
  in the rollout `.jsonl` like CC's `customTitle`. Spike: find where Codex
  persists it (extended `SessionMeta` / sidecar `meta.json` / sessions index)
  and what the rename command/syntax is. Plan: `ConvStore.Title` **reads
  Codex's native title** when present; `conv_index.custom_title` is the
  **fallback**, not the primary. CC behavior unchanged in this project. (M4 /
  JOH-161)
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
to append). **Date-indexed, not cwd-indexed** — cwd lives inside the file. The
dir + filename `<ts>` are **local** time; timestamps *inside* the file are UTC.

**CORRECTED against real Codex CLI v0.139.0** (sampled rollout, captured in
`pkg/testharness/testdata/codex_rollout_v0139.jsonl`; the old
`RolloutLine{timestamp,item}` model below was from earlier research and is
**wrong**): each line is an envelope **`{timestamp, type, payload}`** with a
snake_case top-level `type`:
- `session_meta` — `id`, `timestamp`, `cwd`, `originator` (`codex-tui`),
  `cli_version`, `source` (`cli`), `thread_source`, `model_provider` (`openai`),
  `base_instructions:{text}`. Written **once** at session start.
- `event_msg` — `payload.type` ∈ {`task_started` (turn_id, started_at,
  model_context_window, collaboration_mode_kind), `user_message` (message,
  images, local_images, text_elements), `agent_message` (message, phase,
  memory_citation), `token_count`, `task_complete` (turn_id, last_agent_message,
  completed_at, duration_ms, time_to_first_token_ms), …}.
- `response_item` — `payload.type=message`, `role` ∈ {`developer`, `user`,
  `assistant`}, `content:[{type:input_text|output_text, text}]` (+ `phase` on
  assistant). Tool calls/outputs are also `response_item`s.
- `turn_context` — per-turn snapshot: `turn_id`, `cwd`, `workspace_roots`,
  `current_date`, `timezone`, `approval_policy`, `sandbox_policy`, `model`,
  `personality`, `collaboration_mode`, … Emitted per turn.

`token_count` (feeds context% → JOH-170) shape:
`payload.info.{total_token_usage,last_token_usage}` each =
`{input_tokens,cached_input_tokens,output_tokens,reasoning_output_tokens,total_tokens}`,
plus `payload.info.model_context_window` and a `payload.rate_limits` block.

**Title (JOH-165 — RESOLVED).** The rollout has **no `customTitle` turn** like
CC's. Titles live in **`~/.codex/state_5.sqlite`, table `threads`**, column
`title` (alongside `rollout_path`, `cwd`, `tokens_used`, `git_branch`/`git_sha`,
`model`, `first_user_message`, `preview`, `archived`, …) — a metadata index DB,
**not** the rollout file. For an un-renamed session `title` is auto-derived from
the first user message; a user rename (#22526) updates this column. So the Codex
read path must consult the state DB (or derive from the first `user_message`),
never look for a title turn in the rollout. (`~/.codex/history.jsonl` is a flat
`{session_id, ts, text}` global prompt log — not per-session metadata.)

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
| Rename / title | `/rename` → `customTitle` turn in `.jsonl` | shipped (#22526), persisted in `~/.codex/state_5.sqlite` `threads.title` (JOH-165), not rollout turns → read state DB or derive from first `user_message` |
| "everything in one file" | yes (title/summary/cwd/branch all in the `.jsonl`) | no — metadata split across rollout + sidecar/index |
| Compact | `/compact` | TBD (spike) |
| Model slugs | `claude-*` | `gpt-5.*` |
| MCP / subagents | yes | yes |
| Sandbox | tweakable built-in sandbox, off unless `sandbox.enabled` + hand-written `denyWrite` (see `docs/sandbox-hardening.md`) | OS-native: Seatbelt / bwrap+seccomp / Win restricted-token; `--sandbox {read-only\|workspace-write\|danger-full-access}`; **`workspace-write` writes only cwd+/tmp+$TMPDIR (so `$HOME` is read-only) + net-deny by default** — the tclaude-hardening integrity goal is met without config (§D, JOH-166) |
| Oversight / "auto" | oversight agent checks the worker | **Auto-review** subagent at the sandbox boundary + granular approval policies (v0.122+) (JOH-167) |

### D. CC ↔ Codex sandbox mapping (JOH-166 — researched)

Verified firsthand from openai/codex Rust source at tag **`rust-v0.139.0`**
(the doc has been wrong before on unverified Codex internals, so these are
read off the source, not the marketing docs). The headline: **Codex's
`workspace-write` sandbox already denies the exact writes
`docs/sandbox-hardening.md` asks CC operators to deny by hand** — so a
tclaude-spawned Codex agent gets the guardrail-protecting integrity property
for free, *provided* the Spawner selects a sandboxed mode and cwd isn't
`$HOME`.

**Mode mapping.** `SandboxMode` (`protocol/src/config_types.rs`, serde
kebab-case) is a 3-value enum, the same axis as CC's sandbox on/off + deny
lists but as a single preset:

| Codex `--sandbox` / `sandbox_mode` | Writable | Network | CC analog |
|---|---|---|---|
| `read-only` | nothing (reads only) | denied | `sandbox.enabled` + deny-all-write |
| `workspace-write` | **cwd + `/tmp` + `$TMPDIR`** only (+ any explicit `writable_roots`); `$HOME` read-only | **denied** (opt-in via `network_access`) | `sandbox.enabled` + the `docs/sandbox-hardening.md` `denyWrite` set |
| `danger-full-access` | everything | allowed | sandbox **off** — never for a tclaude agent |

Writable-roots logic is `SandboxPolicy::get_writable_roots_with_cwd`
(`protocol/src/protocol.rs`): in `WorkspaceWrite` it returns `cwd + /tmp +
$TMPDIR` (plus configured `writable_roots`), and the cwd root gets a
read-only `.codex` subpath carve-out. **`$HOME` is not a writable root**, so
`~/.tclaude`, `~/.claude/sessions`, and Codex's own `~/.codex`
(`hooks.json`, `state_5.sqlite`, the rollout tree) are **not writable** by
the agent's sandboxed tool subprocesses. `network_access` defaults `false`
for both `read-only` and `workspace-write` (constructors at
`protocol.rs:1010`/`1019`).

> **Caveat — the cwd==$HOME hole.** Writability follows cwd. If a Codex agent
> is spawned with `cwd = $HOME` (or any parent of `~/.tclaude` / `~/.codex`),
> the whole home tree becomes writable and the protection evaporates — the
> same "check your `allowWrite`/`additionalDirectories` lists" caveat
> `sandbox-hardening.md` already calls out for CC. The Spawner must not spawn
> a Codex agent rooted at `$HOME`.

**Default mode is a trap.** The `SandboxMode` enum's serde `#[default]` is
`read-only`, but Codex's interactive TUI selects a mode via *approval
presets*, whose agent/"Auto" preset is `workspace-write` + on-request
approvals. `codex exec` (headless) defaults to `read-only` unless
`--sandbox`/`--full-auto` is passed. So the effective default depends on
launch surface — **do not rely on it; pass `--sandbox` explicitly.**

**Linux mechanism (for the nested-sandbox question).** Codex's Linux
sandbox is a helper, `codex-linux-sandbox`, that wraps each tool command in
**bubblewrap (filesystem) + seccomp (network)** (`core/src/landlock.rs:15`)
— the *same family* CC uses for its Bash-tool sandbox. macOS = Seatbelt,
Windows = restricted token. Codex applies this to the **commands it runs**,
not to itself.

**Q: does this collide with agentd's identity model?** No.
`agentd` runs **on the host** and attributes a caller via `SO_PEERCRED` +
`/proc` walk (see `docs/plans/agentd.md` security-model discussion); tclaude
does **not** wrap the harness in its own bwrap. The only bwrap in play is the harness's own
per-tool-command sandbox — identical between CC and Codex — so the
PID-namespace property the identity model leans on is preserved, with no new
nesting introduced. **One impl-phase verification item (M4):** confirm the
Codex-spawned session carries the identity env the `/proc` walk reads
(`TCLAUDE_SESSION_ID` / `~/.claude/sessions/<pid>.json`) through to the
`tclaude agent …` tool subprocess, exactly as CC does — likely a no-op
since env inherits, but pin it with a flow test when M4 lands.

**Recommendation — Spawner contract (feeds M2 / JOH-154).** Add a
`SandboxMode` field to `SpawnSpec` (sibling of the existing
`BypassHookTrust` toggle), default **`workspace-write`**; `codexSpawner`
emits `--sandbox <mode>` (a CLI flag, not a `config.toml` edit — flags are
per-spawn, leave the user's `config.toml`/profiles untouched, and match how
`--model`/effort are already passed). CC's `Spawner` ignores the field (its
sandbox is `settings.json`-driven, not a launch flag). **Never default to
`danger-full-access`**; expose it only behind the same explicit opt-in as
CC's "no sandbox". Pair with the cwd!=$HOME guard above.

**Recommendation — `setup` contract.** There is **no Codex equivalent of
`tclaude setup --install-sandbox-hardening` to build** for the *integrity*
goal: CC needs that command because its sandbox is off-by-default and the
deny-lists are hand-written, whereas Codex's `workspace-write` denies the
guardrail-bypass writes natively. The residual is **confidentiality**:
`workspace-write` restricts *writes*, but a sandboxed Codex agent may still
*read* `$HOME` (incl. `~/.tclaude/db.sqlite`'s `-wal`) — the read-deny half
`sandbox-hardening.md` recommends for CC has no built-in Codex equivalent.
Treat read-confidentiality parity as a **lower-priority follow-up** (a
`writable_roots`/profile note in the operator guide, or accept the residual
as an out-of-scope OS-level give), not a blocker — the integrity
guarantee (can't forge identity / rewrite the daemon DB) holds on the OS
sandbox alone.

**Recommendation — dashboard/spawn-dialog surfacing (feeds M5).** Add a
sandbox-mode selector to the spawn dialog (read-only | workspace-write |
danger-full-access; default workspace-write) feeding `SpawnSpec.SandboxMode`,
and show the chosen mode as a per-agent badge on the dashboard — parity with
the CC sandbox controls. Mixed-harness groups render each harness's own
mode label.

**Acceptance status:** CC↔Codex mapping documented (above + matrix row);
Spawner/`setup` contract + default (`workspace-write`) recommended; matrix
row updated; nested-sandbox question answered. Implementation lands as M2
(Spawner field + `--sandbox`) and M5 (dialog/badge) follow-ups — this
research issue is the design input for them.

### E. Project constraints to honor (from tclaude memory/CLAUDE.md)

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

### F. Sources

- Codex storage/rollout: deepwiki openai/codex 3.3, 3.5.2; GH discussion #3827.
- Official docs: developers.openai.com/codex/cli, /codex/cli/reference,
  /codex/hooks, /codex/config-reference.
- Sandbox (§D, JOH-166), read firsthand from openai/codex `rust-v0.139.0`:
  `protocol/src/config_types.rs` (`SandboxMode`), `protocol/src/protocol.rs`
  (`SandboxPolicy` + `get_writable_roots_with_cwd`, network defaults),
  `core/src/landlock.rs` (`codex-linux-sandbox` = bubblewrap + seccomp);
  developers.openai.com/codex/concepts/sandboxing + /agent-approvals-security.
