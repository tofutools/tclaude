# Harnesses

A *harness* is the vendor coding CLI that actually runs the model inside a
tclaude-managed tmux pane. tclaude is harness-agnostic: sessions,
conversations, `ask`, agent groups, lifecycle, and the dashboard all work
across every harness, and a group can freely mix them.

Six coding harnesses and one shell pseudo-harness are supported:

| `--harness` | Harness | Binary in the pane |
| --- | --- | --- |
| `claude` (default) | Claude Code | `claude` |
| `codex` | OpenAI Codex CLI | `codex` |
| `opencode` | OpenCode | managed `opencode serve` + an `attach` client |
| `copilot` | GitHub Copilot CLI | `copilot` |
| `gemini` | Google Gemini CLI (beta) | `gemini` |
| `pi` | pi (beta) | `pi` |
| `shell` | ordinary shell (no model) | `$SHELL` or `/bin/sh` |

tclaude owns everything around the pane — the tmux session, status tracking,
the conversation index, groups and messaging, the dashboard — while each
harness contributes only what is genuinely harness-specific: how to launch it,
where it stores conversations, and which in-pane commands it understands.

!!! note "Shell has two launch forms"
    `session new --shell` (and the equivalent direct `--harness shell` form)
    starts the lightweight standalone shell session described under
    [Sessions](sessions.md#shell-sessions). `agent spawn --harness shell`
    instead uses the managed agent pipeline: group enrollment, worktrees,
    spawn profiles, and tclaude's OS sandbox are available, but model,
    reasoning, hooks, and harness-native sandbox options are not.

## How capabilities work

The harnesses are not equally capable, and tclaude does not pretend they are.
Each harness declares a set of focused capability contracts — can it be asked a
one-shot question, does it have a conversation store, a rename path, a sandbox
catalog, an approval catalog, hooks, and so on. Every feature gates on the
declared contract, not on the harness name:

- **An absent capability is an honest refusal.** Asking a harness for
  something it does not support produces a clear error or a graceful
  degradation with a message — never a silent no-op, and never a fake success.
- **Unknown names fail closed.** An unrecognized `--harness` value is an error
  naming the valid set; it never silently falls back to Claude Code.
- **Degradation is per-feature, not per-harness.** Codex has no in-pane
  `/rename` command, so renames go through its title store instead; OpenCode
  has no hooks, so its live status comes from its managed server instead. The
  rest of the surface is unaffected.

The [capability matrix](#capability-matrix) below is the user-level summary of
those contracts. When a cell says no, the corresponding command tells you so
too.

## Choosing a harness

Every launch surface (`tclaude`, `tclaude session new`, `tclaude agent spawn`)
takes `--harness`. When the flag is omitted, resolution is:

1. **Explicit flag** — always wins.
2. **Global default spawn profile** — set from the dashboard or
   `tclaude agent profiles default set`; its harness/model/effort apply to
   fresh terminal launches, each field overridable per launch. A profile for
   the launch's harness also supplies its launch posture (sandbox, sandbox
   implementation, approval, and the other per-launch knobs; see
   [Sessions](sessions.md)).
3. **First installed harness** — Claude Code is checked first; otherwise the
   registry is walked in sorted order (codex, copilot, gemini, opencode, pi) and the first
   harness whose binary is on `PATH` wins. With nothing installed the launch
   reports the missing `claude` executable.

Agent and group spawns resolve through the fuller spawn-profile precedence
described in [Spawning and lifecycle](spawning-and-lifecycle.md).

```bash
tclaude session new --harness codex
tclaude agent spawn --group crew --name worker --harness opencode
tclaude session new --harness copilot --model gpt-5.4
tclaude agent spawn --group crew --non-interactive --harness shell \
  --initial-message 'go test ./...'
```

### Persistence and resume posture

The harness is **persisted per conversation**. `conv resume`, rename, stop,
compact, reincarnate, and clone all look up the recorded harness
automatically; you never re-specify it. (The lower-level
`session new --resume` is the exception: it searches one harness's store, so
pass `--harness` there or use `conv resume`.)

A resume also **replays the recorded launch posture**: every posture flag you
do not pass explicitly (`--sandbox`, `--sandbox-impl`, `--ask-for-approval`,
tool governance, timeouts, drives, and the rest) is refilled from the
conversation's recorded launch, and the carried flags are echoed so you can
see them. A recorded value the resuming harness cannot honor is dropped with a
warning. Model and effort are remembered by the harness itself.

## Capability matrix

✅ yes · ⚠️ partial / with caveats · ❌ no

| Capability | Claude Code | Codex CLI | OpenCode | Copilot CLI | Gemini CLI | pi | Shell |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Federated model gateway (`--model-proxy`) | Yes, ordinary workers | Yes, TUI and app-server | No: provider isolation | Yes, HTTP Responses BYOK | No: native Gemini dialect | No: not yet bound | No |
| Sessions: spawn / resume | ✅ | ✅ | ✅ managed server + attach | ✅ | ✅ | ✅ | ✅ spawn only |
| One-shot [`ask`](ask.md) | ✅ live-streamed | ✅ buffered | ✅ buffered | ✅ buffered | ✅ buffered | ✅ live-streamed | ❌ |
| Portable agent-bundle history | ✅ JSONL | ✅ rollout JSONL | ✅ native export/import | ✅ native event log | ✅ native chat JSONL | ✅ native session JSONL | ❌ |
| Clone with conversation copy | ✅ native copy | ✅ rollout JSONL; app-server forks the thread | ✅ native export/import | ✅ native event log | ✅ native chat JSONL | ✅ native session JSONL | ❌ |
| [Cross-harness history translation](#cross-harness-history-translation) | ✅ | ✅ | ✅ | ✅ images by path | ✅ | ✅ | ❌ |
| [Conversation](conversations.md) list & search | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ |
| Agent groups & messaging | ✅ | ✅ | ✅ | ⚠️ one launch topology only | ⚠️ send-keys only, not yet exercised against a live pane | ⚠️ send-keys only | ⚠️ durable inbox only; never injected into the shell |
| Automatic prompt-based name refinement | ✅ contained one-shot | ✅ read-only one-shot | ❌ generated name retained; no tested unattended sandbox posture | ❌ generated name retained; no tested unattended sandbox posture | ❌ generated name retained; no tested unattended sandbox posture | ✅ read-only-tools ephemeral one-shot | ❌ no model |
| Standing-order native hook selectors | ✅ inline or next-turn | ✅ inline or next-turn | ✅ observed events, next-turn | ⚠️ installed baseline only, next-turn | ⚠️ installed baseline only, next-turn | ❌ not yet exercised | ❌ no hook source |
| Account quota spawn gate | ✅ cached quota windows | ✅ cached quota windows | ❌ no single account quota | ✅ cached quota windows | ❌ account quota is not captured in session logs | ❌ no single account quota | ❌ no model |
| Rename | ✅ in-pane `/rename` | ✅ title store | ✅ server API | ✅ in-pane `/rename` | ✅ tclaude title overlay | ✅ in-pane `/name` | ❌ |
| Compact / reincarnate | ✅ | ✅ | ✅ (server API, no keystrokes) | ✅ | ✅ in-pane `/compress` | ✅ in-pane `/compact`; pinned auto-compact window | ❌ |
| Séance (ask posture) replay | ✅ | ✅ | ❌ | ❌ | ✅ fork of a copy | ✅ continues a copy | ❌ |
| [Remote control](remote.md) | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| [Status line](utilities.md#status-line) | ✅ command-backed | ⚠️ curated built-in items | ⚠️ OpenCode's own TUI status | ❌ | ❌ | ⚠️ extension footer segment: tclaude's pinned-window context meter | ❌ |
| [Task runner](tasks.md) | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| Native sandbox-profile path rules / reopen beneath deny | ✅ sandbox on; caveats | ✅ managed Linux profile + probe | ⚠️ soft tool permissions only | ❌ native command sandbox does not cover file tools; use tclaude-layer | ❌ fixed native sandbox profile; use tclaude-layer | ❌ no native sandbox | ❌ use tclaude-layer |
| Global OS filesystem rules in profile editor | ✅ settings + managed baseline | ✅ managed baseline | ❌ native config is soft tool control | ❌ native policy only confines commands | ❌ tools.sandbox selects posture, not path rules | ❌ no native policy | ❌ no native policy |
| Activated proxy destination filtering | ✅ Linux/macOS | ✅ Linux/macOS | ✅ Linux/macOS | ❌ private routed posture does not enforce destination rules | ⚠️ Linux only; no Darwin carriage smoke | ❌ no proxy carriage smoke; filtered launches use the packet gateway | ❌ no shell carriage smoke |
| Built-in OS sandbox | ✅ | ✅ | ❌ command filter only | ❌ asserted off | ⚠️ macOS only (`seatbelt`) | ❌ | ❌ |
| [tclaude’s built-in sandbox](sandboxing.md) | ✅ | ✅ | ✅ (wraps the server) | ✅ | ✅ | ✅ the only boundary | ✅ |
| Usage / cost reporting | ✅ real + what-if cost | ✅ what-if cost | ✅ native pricing what-if | ⚠️ Copilot AIU units, no USD | ✅ what-if cost | ✅ pi's catalog price; what-if under a subscription login | ❌ |
| Bundled skills via `setup --install-agent-skills` | ✅ `~/.claude/skills` | ✅ shared `~/.agents/skills` + `$CODEX_HOME/skills` | ✅ shared roots; external skills must be enabled | ✅ shared `~/.agents/skills` | ✅ shared `~/.agents/skills` | ❌ | ❌ |
| Hooks via `tclaude setup` | ✅ | ✅ | ❌ (server liveness instead) | ✅ | ✅ settings.json hooks | — per-launch extension, no setup | ❌ |
| Directory pre-trust (`--trust-dir`) | ✅ | ✅ | — no trust dialog | ✅ | ✅ | ✅ | — |
| Tool governance (`--tools`) | ❌ | ❌ | ✅ | ❌ | ❌ | ⚠️ approval posture picks the tool set | ❌ |
| Fast mode | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| API/RPC drive | — n/a | ⚠️ experimental `--codex-app-server` | ✅ inherent | ⚠️ experimental `--copilot-api` | — | ⚠️ experimental `--pi-drive` | — n/a |

### Cross-harness history translation

An agent that changes harness (cross-harness morph) keeps its conversation:
tclaude decodes the source harness's native history into a neutral
transcript and installs it as a native conversation of the target harness,
which then resumes it like any of its own. The install goes through the
same import path as portable agent-bundle history, so the target's identity
reminting, cwd remapping and indexing apply unchanged. The source history is
only read.

Only the *effective model context* is carried: the active branch, from the
last compaction on (the compaction summary stands in for what came before),
without scaffolding the target harness injects itself (system and developer
prompts, environment and instruction-file blocks, hook context). Translation
is explicit about what does not map 1:1:

- **Reasoning** never crosses vendors. Signatures and encrypted reasoning are
  only valid for the provider that issued them.
- **Tool calls and results** are history, not executable calls. Older than the
  last three human turns, they become one-line stubs (name, short arguments,
  status and output preview) pointing to numbered entries in the saved full
  transcript. Recent results retain up to 4 KiB each, or a smaller configured
  `agent.morph.tool_result_clip_bytes` cap. Without a saved transcript, existing
  clipping remains (default 16384 bytes). Attachments keep their normal handling.
  The CLI and launch preview show approximate source and carried token counts;
  these are estimates, not a provider tokenizer. The notice tells the successor
  to read full tool output only when needed and avoid repeating skills or
  environment checks unless something looks wrong.
- **Images** stay inline where the target's user turns accept them. Copilot
  references them by a saved path instead.
- **Context budget.** The carried history fills at most
  `agent.morph.history_budget_percent` (default 60) of the target model's
  context window. The preview shows the window, its source, and the history
  budget. It uses fresh local Codex/OpenCode model metadata, Copilot prompt
  limits, or the known Claude/Gemini model limits; it does not fetch metadata.
  Unknown or automatic models show **unknown model, assumed 32k**.
  `agent morph --history-window-tokens N` overrides that assumption (1024 to
  10000000); a verified smaller destination limit still wins. A Copilot
  profile's explicit context limit and a Claude auto-compact ceiling also
  bound the budget. This option persists when the morph waits for idle and
  changes only the carried-history budget, not the model's native capacity.
  The estimate charges three tokens per four UTF-8 bytes plus message/media
  overhead. Calibration covers prose, code, logs, JSON, Unicode and identifiers;
  it is a conservative safety estimate, not a provider billing count.

  Local near-budget resume verification is opt-in, skips by default, and
  spends one cheap Codex model call:
  `TCLAUDE_MORPH_RESUME_SMOKE=1 TCLAUDE_MORPH_NEAR_BUDGET=1 scripts/test.sh ./pkg/claude/agentd/ -run TestMorphResumeRealCLI -v -count=1`.
  Do not enable it in CI or copy provider credentials to CI.

  The original task and latest user instructions stay pinned,
  followed by the newest complete turns that fit. A giant pinned turn stays
  in the full transcript with a clear read-before-continuing pointer instead
  of overflowing the native window. The preview shows kept/dropped entry
  percentages and explains the selection. A cut of at least 10% warns; at
  25% the operator must confirm `--confirm-history-loss` (or the launch
  dialog checkbox). The source then writes one summary of the earlier work,
  carried with the pins and whatever native tail fits. No model call runs
  behind the operator's back. The dialog shows the source name, deadline
  and cancellation while waiting, and restores that state when reopened.
  Timeout, cancellation or a refusal leave the source unchanged. During a
  provider outage, an explicit `--proceed-without-summary` or the dialog's
  loss-confirmation checkbox instead carries readable history with its full
  transcript pointer. The destination is warned that earlier context may be
  missing. The fallback never runs without that confirmation. The budget is checked again before applying the morph.
- **Resets and rewinds** carry only the active native history. Gemini compression
  snapshots are labelled as summaries; clear/checkpoint, OpenCode revert and
  Codex/Copilot rewind boundaries are applied before carrying turns. Removed
  history is counted in the loss report. A Copilot truncation event has only
  counts, not the identities of retained messages, so it requires the existing
  agent-written handoff path instead of guessing which history survived.
- **Codex compaction summaries** are encrypted in current Codex releases. A
  Cross-harness morph asks the source for a handoff of its remembered context
  and carries that alongside the readable tail. The existing pending dialog
  shows the source and deadline; timeout or cancellation leaves it unchanged.
  During a provider outage, explicitly confirm `--proceed-without-summary` or
  the dashboard loss checkbox to continue with readable history, the saved
  transcript pointer and a clear warning about the unreadable summary.

The neutral transcript and loss report are the context of the
[Portable Agent Format](paf.md), which specifies their file form.

Every translation produces a loss report (counts and categories only, never
transcript content) and keeps the full, unclipped transcript as a Markdown
file beside it. Clipping and omission markers point the target agent at that
file. The same full transcript is also kept as `transcript.jsonl`, a PAF
context file. Inline images and documents it references are the translation's
own saved attachments, or, for those the translation dropped, files under
`context-media/`. This copy is best effort: if it cannot be written, the
translation goes ahead without it.

If the translated history cannot be installed natively, the target instead
gets a native conversation of one turn that points the agent at the history
saved as files: `briefing.md` (the budgeted history as plain text) and the full
transcript. A third rung, the handoff, replaces the history with a handoff the
agent writes for its successor before the switch. It is used on request, and
automatically when the source history cannot be read. The loss report records
which rung was used.

A morph keeps these files in a per-agent directory under
`~/.tclaude/api/morph/`. The agent must be able to read them, and tclaude's
private data directory is walled off from a sandboxed agent. Under tclaude's
sandbox the parent is hidden and each launch reopens only its own agent's
directory, the same way spawn attachments are granted.

The rest of this page walks each harness: setup, maturity, models, sandbox
and approval knobs, and the extras only that harness has.

## Claude Code

The default harness and the reference implementation of every contract.
First-class throughout.

**Setup.** `tclaude setup` installs the hooks that power live status tracking
into `~/.claude/settings.json`, and offers the command-backed
[status line](utilities.md#status-line). Hooks are idempotent; re-running
setup repairs them.

**Models and effort.** `--model` accepts the aliases `fable`, `opus`,
`sonnet`, `haiku`, `opusplan` — `fable`, `opus`, and `sonnet` also take the
`[1m]` long-context suffix, e.g. `sonnet[1m]` — or any full `claude-*` model
ID. `--effort` is
`low`/`medium`/`high`/`xhigh`/`max`. Empty means "let the harness decide".

Haiku 5.5 uses the pinned API ID `claude-haiku-5-5` and a native 1M-token
window; no `[1m]` suffix is needed. With Claude Code v2.1.293 or later,
`haiku` resolves to 5.5 on the Anthropic API and to 4.5 on some other
providers. tclaude forwards the alias unchanged and displays the model,
window, and cost Claude Code reports. See [Claude Code model configuration](https://code.claude.com/docs/en/model-config).
Haiku 5.5's API rates increase for prompts over 100K tokens; OpenCode uses
its runtime provider catalog for these tiers, so its cost reporting requires
that catalog to include the model and its rates. Copilot's `claude-haiku-5.5`
suggestion is prepared ahead of its pinned CLI catalog; Copilot determines
availability and reports cost in native usage credits.

**Sandbox.** Claude Code's own OS sandbox is configured in `settings.json`,
not a launch flag, so tclaude's `--sandbox` delivers a per-session settings
override with three modes:

- `inherit` *(default)* — no override; the agent runs under whatever your
  `settings.json` configures.
- `on` — forces the sandbox on for this session, with the agentd socket
  reachable and `~/.tclaude` hidden.
- `off` — forces Claude Code's own sandbox off. Under
  `--sandbox-impl tclaude-layer` this is what tclaude itself sets, because its
  own wall is enforcing — see [Sandboxing](sandboxing.md) for the two-axis
  model.

**Approvals.** The approval axis is Claude Code's permission mode
(`--ask-for-approval`, rendered as `--permission-mode`): `inherit`,
`default`, `plan`, `acceptEdits`, `auto`, `dontAsk`, `bypassPermissions`.
Daemon-spawned agents default to `auto`, where a supervisor model approves
safe actions and blocks unsafe ones — the most autonomous mode that keeps
guardrails, suited to a detached pane. A direct `session new` applies no
default; your own configuration rules.

**Claude-only extras:**

- **Auto memory is off by default.** tclaude launches Claude Code with
  `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` so agents sharing a checkout do not
  cross-pollute the per-project memory store. Opt back in per launch with
  `--auto-memory`. `CLAUDE.md` is unaffected. See
  [memory-files](utilities.md#tclaude-memory-files) for inspecting the store.
- **The agent view is off by default.** tclaude launches Claude Code with
  `CLAUDE_CODE_DISABLE_AGENT_VIEW=1`, so left arrow in a managed pane is a plain
  cursor key. The view forks a conversation into a background daemon outside
  tmux, where tclaude cannot track it, deliver messages to it, or attach to it,
  and quitting the view ends the pane. Opt back in per launch with
  `--context-features agent-view=on` if you accept that; `tclaude task run`
  has no opt-in. `claude` run outside tclaude is unaffected.
- **Cross-session messaging is off by default.** Claude Code ships its own
  agent-to-agent mesh (the `ListAgents` and `SendMessage` tools over a
  per-session socket). That is a second coordination channel with none of
  tclaude's group membership, permission slugs, or audit trail, so tclaude
  closes it and leaves `tclaude agent send` as the way agents coordinate. Opt
  back in per launch with `--peer-messaging`. **In-harness subagents are
  unaffected either way** — the deny names `ListAgents`, never `SendMessage`.
  See [Peer messaging](utilities.md#claude-code-peer-messaging).
- **Startup-context trimming** (`--context-features`) removes bundled skills,
  unused tool schemas, and system-prompt blocks from an agent's startup
  context, per spawn or per profile. Nothing is trimmed unless you ask. See
  [Utilities](utilities.md#startup-context-trimming).
- **Auto-compaction window** (`--auto-compact-window`) pins the token capacity
  Claude Code's auto-compaction (and tclaude's context meters) reason from —
  useful to make a long-lived agent on a 1M-window model compact while it is
  still sharp. See [Utilities](utilities.md#auto-compact-window).
- **AskUserQuestion timeout** (`--ask-user-question-timeout`
  `inherit`/`never`/`60s`/`5m`/`10m`) makes an unattended agent auto-continue
  instead of stalling forever on a clarifying question.
- **Remote control** (`--remote-control`, or toggled later) arms Claude Code's
  built-in Remote Access so the session is reachable from claude.ai/code and
  the Claude mobile app. Claude Code only. See [Remote](remote.md).
- **Background shells and monitors** are tracked per task, so an agent waiting
  on one shows `⚙+N` / `👁+N` instead of `idle` in the
  [dashboard](dashboard.md).

### Relocated config, and what that means for `claude mcp`

Every tclaude-launched Claude pane runs with `CLAUDE_CONFIG_DIR` pinned to the
harness state root, so it reads **`~/.claude/.claude.json`** — not the ambient
`~/.claude.json` that a bare `claude` in your terminal opens. That keeps
account, onboarding and per-project trust state inside the one directory every
sandbox posture keeps writable; without it a constructed-root launch cannot see
the top-level file and parks on the login wizard. The relocated file is copied
from the ambient one **once**, at the first tclaude Claude launch on the
machine, and the two evolve independently from then on.

Almost nothing in that file is configuration you maintain by hand — it is
account state, per-project records, caches and counters, all of which *should*
diverge per launch context. The settings you do edit (`settings.json`,
`settings.local.json`, `plugins/`) already live under `~/.claude/`, the state
root both launch kinds share, so they need no special handling.

The one exception is MCP registrations, which Claude Code stores in the config
file rather than in settings — under `mcpServers` for `--scope user`, and under
`projects.<dir>.mcpServers` for `--scope local`, the default. **So a bare
`claude mcp add` configures your ambient config only, and the server will not
appear in a tclaude pane's `/mcp` list.** Plugin-provided servers still appear,
because plugin registration lives under the shared state root — which is why
the failure looks selective rather than total.

Set the config dir on any `claude mcp` command whose result you want tclaude
panes to see:

```bash
CLAUDE_CONFIG_DIR=~/.claude claude mcp add --transport http my-server http://localhost:4001/mcp/http
CLAUDE_CONFIG_DIR=~/.claude claude mcp list
CLAUDE_CONFIG_DIR=~/.claude claude mcp remove my-server
```

Panes read the config at launch, so restart or resume the session to pick up a
change. To configure both your terminal and your agents, run the command twice
— once with the prefix and once without.

If you have servers that predate this and want them carried over in bulk, merge
`mcpServers` from `~/.claude.json` into `~/.claude/.claude.json` by hand; there
is deliberately no automatic sync, so that tclaude never writes MCP entries
into a config file a running pane is concurrently updating.

## Codex CLI

First-class: the common contracts — sessions, conversations, ask, groups,
lifecycle, hooks, dashboard — are production paths for Codex.

**Setup.** A plain `tclaude setup` detects `codex` on `PATH` and offers to
install its hooks into `~/.codex/hooks.json` (or run
`tclaude setup --harness codex` explicitly). The install is surgical and
idempotent, and it atomically trusts only the absolute-path tclaude hooks it
just installed — Codex requires command hooks to be trusted, and unrelated
user or repository hooks stay on Codex's normal review path. Trust fails
closed on Codex versions tclaude has not verified. Setup also curates Codex's
built-in [status line items](utilities.md#status-line).

**Models and effort.** `--model` offers a suggestion list of current OpenAI
IDs and accepts any custom OpenAI ID; Claude slugs are rejected with a clear
error. `--effort` uses the same five levels as Claude Code, with `max` mapped
to `xhigh` for models without a max tier.

**Sandbox.** Codex has a real native OS sandbox, selected per launch:

- `tclaude-agent` *(daemon default)* — not a raw Codex mode but a
  tclaude-managed **permission profile** (`codex -p tclaude-agent-<launch>`),
  giving `workspace-write` containment plus an allow-listed agentd socket so
  the sandboxed agent can still run `tclaude agent …`, while `~/.tclaude` is
  denied entirely. When launched inside a Git repo it also grants the minimal
  repository root needed to create and commit in sibling worktrees.
- `workspace-write` / `read-only` — raw confined Codex modes, passed through.
  No agentd-socket grant, so agents under these cannot reach `tclaude agent`.
- `danger-full-access` — Codex's sandbox off; also what
  `--sandbox-impl tclaude-layer` sets, since tclaude’s sandbox then
  enforces.

**Approvals.** `--ask-for-approval` maps to Codex's policy set: `never`
*(daemon default — an unattended pane must not deadlock on a prompt)*,
`untrusted`, `on-failure` (deprecated), `on-request`. A direct `session new`
injects no default and respects your `config.toml`. The `-p` /
`--permission-profile` flag runs the pane under a named Codex permission
profile (`codex -p <name>`) instead; it is mutually exclusive with
`--sandbox`.

**Codex-only extras:**

- **Fast mode** — `--fast-mode inherit|on|off` toggles Codex's fast mode at
  launch; it can also be flipped in-pane.
- **`--auto-review`** *(experimental)* — routes approval prompts to Codex's
  guardian subagent, which decides in your place, fail-closed. Off by
  default; the underlying Codex key is experimental. It has no effect under
  approval policy `never`, which creates no approval requests.
- **App-server drive** *(experimental)* — by default tclaude drives Codex via
  tmux send-keys. `--codex-app-server` (or the `codex_app_server` profile
  field, or the dashboard control) opts a spawn into Codex's authenticated
  app-server API instead: durable message delivery, rename, compaction, and
  interrupt as typed calls. Requires Codex CLI 0.147.0 or newer; an explicitly
  selected drive **fails closed** rather than silently falling back to
  send-keys. The drive carries over to resume, reincarnate, and clone;
  `tclaude agent codex-app-server status` diagnoses it, and
  `tclaude agent resume <agent> --send-keys` durably rolls a stopped agent
  back.
- **Reincarnation guidance** — Codex agents should normally run to full
  context and let Codex's native automatic compaction work, rather than
  reincarnating for context pressure (that pattern is for Claude Code).

## OpenCode

Supported via a managed, server-authoritative path: agentd starts and
authenticates a per-session `opencode serve`, mints the conversation on the
server, supervises and reaps it, and the tmux pane is only an
`opencode attach` client. A bare `session new --harness opencode` without the
daemon is refused — the pane is never allowed to start its own server. Full
status/SSE mapping is intentionally capability-gated and still partial.

**Setup.** No hooks: `tclaude setup` has nothing to install for OpenCode.
Liveness and status come from the managed server's event stream.

**Models.** The catalog is fetched from OpenCode itself and cached with
background refresh, so it may briefly report empty rather than guess.

**Tool governance.** OpenCode gets an axis no other harness has:
`--tools allow|ask|deny` applies one permission action uniformly to its
built-in bash, glob, grep, LSP, task, and skill tools. `allow` (default) runs
them without prompting; `ask` prompts (and can stall a detached agent);
`deny` blocks them. It is independent of the approval selector below and is
preserved across clone, resume, and reincarnate.

**Sandbox — read this caveat.** OpenCode's `access-control` mode (the
`--sandbox` default) compiles tclaude's path and permission rules into
per-session OpenCode tool rules. It **is a command filter, not OS
confinement**: shell redirection, symlinks, and subprocess binaries bypass its
lexical command/path checks. Because it reads like a sandbox without confining
like one, every spawn surface warns when it is the only boundary. For a real
wall, use `--sandbox-impl tclaude-layer`, which puts tclaude's own OS sandbox
(bubblewrap on Linux, Seatbelt on macOS) around the tool-executing server.
The explicit `off` mode removes path scoping but keeps approval and tool
governance active.

**Approvals.** `--ask-for-approval` is a tclaude-compiled per-session
permission policy: `deny` *(default — edits and web tools denied)*, `ask`
(a present human approves representable edits), `allow-tools`
(auto-accepts scoped edits and explicitly enabled web tools).

**Other gaps, stated plainly:** no séance replay for `ask` threads, no
directory-trust dialog (so nothing to pre-trust), no status line
installation, and no remote control.

## Copilot CLI

An evidence-driven adapter: each capability was promoted only after being
proven against a pinned real binary (1.0.77/1.0.78), which is why the gaps
below are named rather than papered over. What it has today: spawn, resume,
model/effort, in-pane rename/compact, hooks, a cold conversation store,
`ask`, directory pre-trust, usage/context telemetry, and a measured approval
posture.

**Setup.** tclaude installs its hooks as a tclaude-owned drop-in file,
`<COPILOT_HOME>/hooks/tclaude.json`, merged by Copilot with your own hooks —
no trust step, no config edits. `tclaude setup` also offers enabling
Copilot's copy-on-select when the binary is present.

**Models and effort.** Copilot brokers models from several vendors
(`claude-*`, `gpt-*`, `gemini-*`, `mai-*`) and exposes no machine-readable
catalog, so tclaude's list is a **suggestion list, not an allow-list**: any
single bounded token is forwarded verbatim (case preserved, for BYOK IDs) and
Copilot does the authoritative validation. `--effort` accepts
`none`/`minimal`/`low`/`medium`/`high`/`xhigh`/`max`; Copilot's docs describe
`max` as an Anthropic-model tier, so pair it with a model that has it.

**Sandbox.** Copilot owns a real experimental OS sandbox (MXC), but it has no
per-launch lever tclaude can safely use, so tclaude's catalog is an
**assertion, not a switch**: `inherit` (default) leaves Copilot's own
configuration alone, and `off` — what `--sandbox-impl tclaude-layer` resolves
to — *verifies* the inner sandbox is not engaged and refuses the launch when
it cannot prove that (including when `--experimental` would let the pane
re-enable it mid-session). There is no `on`. The supported confined posture
is therefore tclaude’s sandbox with Copilot's sandbox asserted down.

**Approvals.** Three measured tokens:

- `allow-tools` *(default for daemon spawns)* — renders `--allow-all-tools
  --no-ask-user` plus one `--add-dir` per directory the sandbox profile
  grants.
- `inherit` — no permission flags at all; Copilot's own defaults and your
  configuration decide.
- `yolo` — Copilot's widest posture; also opens the directory axis.
  **Without `--sandbox-impl tclaude-layer` this leaves the agent with no file
  boundary at all**, and tclaude warns at spawn.

Folder trust is a separate gate no approval flag can clear: Copilot's trust
modal blocks *before any provider contact*, so an untrusted detached pane
never reaches its first turn. `--trust-dir` seeds the entry in
`<COPILOT_HOME>/config.json` ahead of launch.

**Copilot-only specifics:**

- **API drive** *(experimental)* — the default drive is tmux send-keys.
  `--copilot-api` (or the `copilot_api` profile field / dashboard checkbox)
  drives `copilot --ui-server`'s embedded JSON-RPC endpoint instead: message
  delivery, rename, and compaction as typed calls, plus live context and
  usage read over RPC rather than from the durable log. The endpoint is an
  unauthenticated host loopback listener, a **pre-trusted launch directory is
  mandatory** (an untrusted dir is refused, not parked), and soft exit
  deliberately stays on keystrokes. Off unless you ask.
- **Detached-topology restriction.** Copilot agents are usable as detached
  group agents in exactly **one launch topology**:
  `--sandbox-impl tclaude-layer` with sandbox mode `off` — tclaude's wall
  enforcing, Copilot's own sandbox asserted down, so the launch has exactly
  one claimed boundary. Every other spelling is refused as
  `sandbox_restricted`. Interactive human sessions are not restricted this
  way.
- **Usage in AI credits.** Cost is carried in the nano-AI-credit units
  Copilot emits; the dashboard derives a *virtual* USD value from the fixed
  1 credit = $0.01 gross subscription rate (labeled as derived, not billed).
  The dashboard also samples premium-request quota for metered plans.
- **No status line, no remote control, no tool governance, no streaming
  `ask`** — each an honest absence, refused or degraded with a message.

## Gemini CLI

!!! warning "Beta"
    Gemini CLI support is new and has seen far less real-world use than the
    other harnesses. Expect rough edges, especially in the interactive pane,
    and please report problems you hit.

The Gemini CLI adapter, pinned to 0.62.0, covers:

- Spawn and exact resume, with a pre-minted conversation id, so daemon
  spawns are enrolled before the pane starts.
- The launch briefing as Gemini's `-i` first turn.
- Model selection.
- Conversation listing and search.
- Buffered `ask` and séance.
- Rename, in-pane compaction and soft exit.
- Live status through hooks, and directory pre-trust.
- Gemini's approval modes, and its own sandbox modes.
- tclaude's sandbox, with both Linux network engines and the separate
  filesystem root.
- Usage with a what-if cost.

It was written from the CLI's source without a Gemini account. CI runs the
real pinned binary in two places:
- **Against a local mock model:** session files, resume, the séance fork,
  hook firing, folder trust and every approval mode.
- **Behind the proxy network engine:** its model egress.

What is not machine-checked is the interactive pane: typed lifecycle
commands, dialogs, and how it reacts to keystrokes.

**Models.** `--model` offers Gemini's own aliases (`auto`, `pro`, `flash`,
`flash-lite`) and current concrete ids as suggestions; any other single
Gemini model token is forwarded and Gemini validates it. Claude and OpenAI
slugs are rejected with a clear error. Gemini CLI has **no reasoning-effort
launch option**, so `--effort` is refused rather than silently dropped.
Thinking budgets live only in settings files. The only settings paths a
launch can redirect are Gemini's system settings files, and Gemini ignores
those unless the file and its directories are owned by root. So tclaude
cannot set a budget for a single launch without editing your settings.

**Lifecycle.** Compaction types `/compress`; soft exit sends Escape and a
double Ctrl+C (Gemini quits on the second press inside a 3 s window), with
`/quit` as the typed form. Gemini has no rename command and no title of its
own, so rename writes a title into tclaude's conversation index, which the
conversation listing shows over Gemini's generated summary. Nothing is typed
into the pane.

**Conversation ids.** Every fresh Gemini launch is given its conversation id
up front (`gemini --session-id`), including a plain interactive
`session new`, so it is resumable and tracked from the first moment even
without hooks. A launch `--name` is kept in tclaude's conversation index,
since Gemini has nowhere to store one.

**Conversations.** tclaude reads Gemini's own chat files under
`~/.gemini/tmp/<project>/chats/` (or `$GEMINI_CLI_HOME/.gemini`), replaying
the JSONL record stream the way the CLI does — rewinds, metadata updates and
legacy whole-file `.json` sessions included. A session counts as a
conversation exactly when Gemini's own `--resume` would offer it: it has real
content and is not a subagent session. Resume is scoped to the project
directory, as in the CLI.

**Portable history.** Gemini portable history preserves native chat messages, metadata updates and rewind
records. Import remints session and message IDs, maps the receiver project, and resumes
through Gemini’s own session store. Legacy JSON sessions are converted to JSONL.
Settings, trust decisions, credentials and filesystem checkpoints do not travel.

**Ask.** `tclaude ask --harness gemini` runs headless `gemini --prompt` and
returns the buffered answer. In headless mode Gemini denies any tool call
that would need approval. Live streaming is not supported.

**Séance and non-interactive runs.** A séance replays the predecessor's
recorded posture: its Gemini sandbox mode, its `--approval-mode`, and, for a
generation that ran under tclaude's sandbox, that sandbox again. Gemini has no
flag that keeps a headless `--resume` out of the conversation, so the turn
runs on a fork:
- tclaude copies the predecessor's session file and starts
  `gemini --session-file <copy>`, which imports the copy into a new session.
- The predecessor's own file is only read.
- After the answer, tclaude deletes the copy and the new session Gemini wrote
  for it.

`--print-cmd` shows the copy as `<copy-of-…>`. Non-interactive spawns use the
same posture replay.

**Setup and live status.** `tclaude setup` installs the tclaude callback
into the `hooks` section of `~/.gemini/settings.json` (or
`$GEMINI_CLI_HOME/.gemini/settings.json`). It installs for `SessionStart`,
`BeforeAgent`, `BeforeTool`, `AfterTool`, `AfterAgent`, `Notification` and
`SessionEnd`,
and the callback maps them onto tclaude's turn states: working, idle, and
awaiting permission while Gemini shows a tool-confirmation dialog. The
installed command discards its output and always exits 0, so it can never
block, retry or comment on a Gemini turn. A `settings.json` with comments
is left alone, and setup asks you to add the hooks by hand. Gemini runs
settings hooks **only in trusted folders**, so live status needs the
project directory trusted. One known gap: Gemini fires no hook when a turn
ends because every pending tool was declined or cancelled (Esc, or "No" on
the confirmation). Such a row keeps showing working or awaiting permission
until the next prompt.

**Renderer and mouse wheel.** By default Gemini CLI renders inline and leaves
its history in the terminal's scroll-back. Inside tclaude's tmux pane, and in
the dashboard terminal, the mouse wheel then arrives as Up/Down keys, which
Gemini reads as prompt history. Gemini's alternate-screen renderer
(`ui.useAlternateBuffer`) handles the mouse itself, so the wheel scrolls the
conversation, as in Claude Code's fullscreen TUI. `tclaude setup` offers to
enable it in `~/.gemini/settings.json`; an existing value is left alone.

**Directory trust.** Folder trust is on by default in Gemini CLI. In an
untrusted folder the pane stops on a trust dialog, hooks are off, and
headless `ask` fails. `--trust-dir` (or the dashboard's spawn option)
records the launch directory as `TRUST_FOLDER` in
`~/.gemini/trustedFolders.json`, or wherever
`GEMINI_CLI_TRUSTED_FOLDERS_PATH` points. It never overrides an explicit
`DO_NOT_TRUST` for the same directory, and it refuses to rewrite a file
it cannot parse strictly. Gemini treats a malformed trust file as a fatal
startup error.

**Pass-through arguments.** Arguments after `--` that would make the pane
disagree with what tclaude recorded — `--resume`, `--session-id`, `-i`/`-p`,
`--model`, `--worktree`, the approval and trust options, `--sandbox`, `--acp`,
one-shot listing options — are refused with the dedicated tclaude option to
use instead. So are bare positional arguments (Gemini would read them as the
initial prompt, replacing the briefing, or as a subcommand) other than the
values of Gemini's list options such as `--include-directories`.

**Sandbox.** tclaude's built-in sandbox (`--sandbox-impl tclaude-layer`) wraps
the Gemini pane like any other harness. Gemini's own sandbox mode has these
values:
- `inherit` (the default) leaves your Gemini sandbox settings alone.
- `off` exports `GEMINI_SANDBOX=false`, which outranks `--sandbox` and
  settings.json `tools.sandbox`, and exports an empty `SANDBOX` (so a workspace `.env` cannot refill it).
- `seatbelt` (macOS only, with `--sandbox-impl harness-builtin`) runs Gemini
  under its own Seatbelt sandbox. It exports `GEMINI_SANDBOX=sandbox-exec`
  and `SEATBELT_PROFILE=permissive-open`, plus the same empty `SANDBOX`. In
  this mode:
  - Writes are confined to the project, temp and cache directories.
  - `~/.gemini` and the credential files are write-protected.
  - Outbound network stays open.

  Gemini runs its hooks inside that profile, where tclaude's database is
  read-only, so tclaude brokers the hook callbacks through agentd, as it does
  under tclaude-layer. The profile also hides Gemini's own credentials and
  trust store from the sandboxed CLI, which has these effects:
  - The mode is refused for a Google sign-in. Use an API key or Vertex AI.
  - tclaude exports `GEMINI_CLI_TRUST_WORKSPACE=true` for a folder your trust
    store (or `--trust-dir`) trusts.
  - Sandbox-profile filesystem rules are refused, because the profile is
    Gemini's, not tclaude's.

  Weaker than tclaude-layer in several ways:
  - Unix sockets are unrestricted, so the agent can reach tclaude's tmux
    server and run commands outside the sandbox through it.
  - It can read `~/.tclaude`.
  - Git writes to a linked worktree's main repository are denied.
  - Gemini refuses to start in a sensitive directory such as `$HOME`.
  - Under `inherit`, an operator's own `tools.sandbox: true` selects this
    sandbox without tclaude knowing, so hook callbacks are not brokered. Gemini also keeps this mode's chats under
  `~/.cache/.gemini`. tclaude lists conversations from both places, but Gemini
  can resume a conversation only in the mode that created it. Agents cannot
  spawn Seatbelt-mode Gemini children; a human can.

tclaude-layer launches always use `off`. A container sandbox would re-run
Gemini outside tclaude's wall and out of reach of its hooks. Inside the
wall, `~/.gemini` stays writable for chats and credentials. Its policy and
code surface is floored read-only (see
[the harness-config floor](sandboxing.md#the-harness-config-floor)). Agents
may spawn Gemini children only in this walled topology, the same rule as
Copilot. A filtered network list is enforced by the Linux packet gateway.
tclaude checks it against the route of Gemini's selected auth type, using the
`net-google-gemini-api` and `net-google-gemini-login` packs (see
[Network filtering](network-filtering.md#gemini-cli)).

**Approvals.** `--ask-for-approval` renders Gemini's `--approval-mode`. The
semantics below come from Gemini's built-in policy files, not from a live pane:

- `yolo` *(default for daemon spawns)*: every tool call runs without
  confirmation, shell commands included. Gemini's own file tools stay inside
  the workspace; its shell commands are confined only by tclaude's sandbox.
  Gemini's `ask_user` tool still asks a human, since it is a question rather
  than a permission.
- `auto_edit`: workspace edits and web fetches run unattended, but shell
  commands still ask.
- `default`: edits, shell commands, web fetches and skills all ask.
- `plan`: read-only, so every mutating tool is denied.
- `inherit`: no flag; settings.json `general.defaultApprovalMode` decides.

Gemini drops `yolo` and `auto_edit` back to `default` in a folder it does not
trust, which is one more reason to pass `--trust-dir`. If your settings set
`security.disableYoloMode` or `admin.secureModeEnabled`, Gemini refuses
`--approval-mode=yolo` at startup. Pick another mode for those launches, or
make it the profile's default. For agent spawn
lineage, `yolo` counts as unattended command execution (the same weight as
Copilot `allow-tools` or Codex `never`), and an `inherit` child counts as the
broadest posture. The policy engine (`--policy`, `--allowed-tools`) is not
modeled, so those options are refused as pass-through arguments.

Known gap: the lineage classes assume the workspace's own Gemini
configuration is untouched. An agent that can edit files (Gemini `auto_edit`,
or an edits-capable agent of another harness) can write
`<cwd>/.gemini/settings.json`, including MCP server commands. In a trusted
folder, the next Gemini launch there loads that file. The same holds for
other harnesses' project config files. It is a sandbox-axis concern: a
profile can deny writes to `.gemini/`.

**Usage.** Gemini stamps each model message in its session file with that
call's token usage. tclaude reads that back on every dashboard and agent-API
read, the same read-through pattern Copilot uses:
- The context meter shows the latest call's prompt size against Gemini's own
  per-model limit (1,048,576 tokens, or 256k for Gemma 4), the same figure the
  pane's footer shows.
- Output tokens (thinking included) are summed over the conversation.

Gemini records no price, so tclaude prices each model call itself at Gemini
API pay-per-token rates. Whether that estimate counts as real spend or a
**what-if cost** follows `cost.gemini_billing` (Config tab → Gemini billing):
under the default `auto`, a session whose Gemini settings select an API key
(`security.auth.selectedType` `gemini-api-key`) or Vertex AI is shown as real
API spend, while a Google sign-in, or no readable selection, stays a what-if
estimate behind the same opt-in Codex uses (`cost.show_on_subscription`).
`api` and `subscription` force either side, for example when a Google sign-in
uses a paid Cloud project. Either way the figure is tclaude's estimate, not
Google's invoice. The classification is applied while a session runs and
covers its whole conversation: a running or resumed conversation moves to
the current side, while an exited one keeps the side it was recorded under. Cached prompt tokens get the cached rate,
thinking tokens are priced as output, and Pro models switch to the long-context
rate for prompts over 200k tokens. Calls dropped by a rewind or a `/compress`
still count, because they were made. A model without a published rate, such as
an older preview, adds nothing rather than borrowing another model's price.
The output total
covers only the turns that survive in the file, so it can go down after a
rewind or a `/compress`. After a compression, the context reading keeps its
old value until the next model call reports usage.

**Not yet:**
- The proxy network engine on macOS (it works on Linux).

Gemini's own sandbox is offered only as Seatbelt on macOS. On Linux it is a
container that would run the agent outside tclaude's hooks, so use tclaude's
sandbox there.

Gemini has no counterpart for remote control, fast mode, a command-backed
status line, or tclaude's task runner.

## pi

[pi](https://github.com/earendil-works/pi) (`@earendil-works/pi-coding-agent`,
1.1.0 or newer) is the open-source coding agent from earendil-works.

**What works.** Spawn and resume (`--session-id`, and resume by session file
so a pane never stops on pi's fork prompt), model and thinking level
(`--model provider/id`, `--effort` as pi's `--thinking`), `/name`, `/compact`
and a soft `/quit`, and directory pre-trust in pi's own `trust.json`. pi uses
your own `~/.pi/agent`: its providers, logins, settings and telemetry choice.
tclaude passes no credentials into pi.

**Live status needs no setup.** pi has no hook configuration, so tclaude ships
a small pi extension inside its binary, writes it read-only under
`~/.tclaude/pi-extension` (checked byte for byte before every launch, and
mounted read-only inside tclaude's sandbox), and loads it per launch with
`--extension`. It reports turns, tools,
dialogs, compaction, `/clone`/`/new` session switches and exit to tclaude the
way other harnesses' hooks do. Nothing is written to `~/.pi`, and a slow or
missing tclaude never stalls pi.

**Approval and sandboxing.** pi has no approval prompts and no sandbox of its
own. `auto` (the default) runs every tool, including shell commands,
unattended. `ask` loads a second tclaude extension, written read-only beside
the status one, that asks you in the pane before every tool call that is not
read-only: pi's `read`, `grep`, `find` and `ls`, and MCP tools whose server
marks them read-only, run unasked; shell commands, edits and everything else
open a confirmation showing the call's arguments (control characters shown, never obeyed). The pane shows as awaiting
permission until you answer, in the pane or the dashboard's terminal. A
declined call is blocked, and the model is told so. With no one to ask (pi's
print mode), such a call is blocked. `ask` gates tool calls, not code pi
itself loads: an extension in the workspace's `.pi/extensions` runs when pi
starts, so approve a write there as you would approve running it. `ask` is a
human-gated posture: an agent that itself asks a human (Claude `default`,
Codex `on-request`) may spawn a pi child in it. `read-only` exposes only
`read`, `grep`, `find` and `ls`. Because nothing else confines `auto`, a fresh `auto`
launch is refused unless tclaude's own sandbox wraps it (`--sandbox-impl
tclaude-layer`, which spawn profiles for pi recommend) or the human ticks
**Allow launch with unenforced rules** in the dashboard spawn dialog.

**One-shots.** [`tclaude ask`](ask.md) runs `pi --print` (streamed through
`pi --mode json` on a terminal) limited to pi's read-only tools, and an
interactive ask loads the permission extension. A fresh thread is
`--session-id`, a follow-up resumes the same session file. Automatic session
naming is an unpersisted (`--no-session`) read-only one-shot. A séance
continues a copy of the predecessor's session file under its recorded
posture, inside tclaude's sandbox when the generation ran there, and
removes the copy afterwards.

**Inside tclaude's sandbox** pi runs against a walled copy of its agent
directory, not your own (`$PI_CODING_AGENT_DIR`, else `~/.pi/agent`). pi runs
an API key that starts with `!` as a shell command, and `models.json` and
`settings.json` can name commands and code too, so your agent directory is
read-only inside the wall, files that exist and files that do not, apart
from its session store: a sandboxed agent cannot change what the next,
possibly unsandboxed, pi runs. Each conversation has its own walled copy,
under `~/.tclaude/pi-agent`, so no two sandboxed agents share one. It is
rebuilt from yours before every walled launch and removed once its
conversation has no session left:

- every entry pi reads (`settings.json`, `models.json`, `extensions/`,
  `skills/`, the prompt files, and so on) is a link to yours, so pi sees your
  configuration, and a write through a link fails;
- `sessions` is a link to your session store, which stays writable, so
  conversations land where tclaude and your own pi find them (a session
  directory your settings move elsewhere is granted too);
- `auth.json` is a writable copy of yours, so an OAuth login (ChatGPT, say)
  refreshes inside the wall. Only a genuine refresh of your ChatGPT
  (`openai-codex`) login travels back to your `auth.json`: newer, with a
  rotated refresh token, for the same ChatGPT account, and only its tokens
  and expiry (capped at 60 days). Nothing else does: no API key, no other
  field, no provider you are not logged in to, and no other provider's
  login, since pi trusts more of some logins than their tokens (Copilot's
  API host comes from its token). tclaude carries the refresh back before
  every walled launch and every 30 seconds while the daemon runs, and
  carries your own newer login into the copy the same way. A provider whose
  refresh tokens rotate, other than ChatGPT, keeps your original login,
  which a walled refresh may leave stale, so pi outside the wall may ask
  you to log in again. A `/login` inside the wall stays in the copy until
  the next walled launch rebuilds it.

pi cannot install its managed `fd` and `rg` from inside the wall; install them
on the host (or run pi once outside it) and `find`/`grep` use them. Other
harnesses' homes are not visible. In a constructed root (such as a filtered
network on Linux) tclaude mounts the `pi` package and its Node runtime
read-only, wherever they are installed.

**Filtered network.** A filtered launch checks the model's route against
your network rules and refuses, with the missing destination, when they do not
allow it. tclaude reads the provider from `--model provider/id` (or, without
one, the `defaultProvider` in pi's `settings.json`; a resume uses the model the
session records) and maps it to the provider's host: `anthropic`
(`net-anthropic`), `openai` (`net-openai-codex`), `openai-codex` (ChatGPT,
`net-openai-chatgpt`), `google` (`net-google-gemini-api`), and `openrouter`,
`xai`, `groq`, `mistral`, `deepseek`, `cerebras`, `together`, `fireworks`,
`zai`, `moonshotai` and `huggingface`, which need an allow row for their API
host. A provider with a `baseUrl` in `models.json` (a local Ollama, say) needs
that endpoint. Anything else, such as a provider an extension registers,
Bedrock, Vertex, Azure or GitHub Copilot, refuses the filtered launch rather
than guessing, as does an `httpProxy` in pi's settings. The launch also runs pi
offline (`PI_OFFLINE=1`, `PI_SKIP_VERSION_CHECK=1`, `PI_TELEMETRY=0`): no
version check, catalog refresh, install telemetry or tool download. Outside a
filtered network tclaude sets none of these and your pi settings decide.
Filtered pi launches use the packet gateway; the proxy engine is not yet
offered for pi.

**Conversations.** tclaude lists and searches pi's session files wherever pi
keeps them (`~/.pi/agent/sessions`, or the `sessionDir` your settings name),
titles them by pi's session name, and follows the session tree the way pi does:
the current branch, from the newest compaction on. Clone, agent bundles and
federation carry the session file as is under a new id; a cross-harness morph
translates it, and a morph to pi needs a `provider/model`, since pi resumes
with the model its history names. The translation is budgeted to the model's
context window: the window `models.json` declares for a custom model, else the
one pi last reported for it, else an assumed 32k; history that does not fit
needs the same confirmation as any other morph.

For pi, tclaude reads version 3 session files. Reopen older version 1/2 files
in pi first so pi migrates them; tclaude refuses unsupported versions rather
than silently dropping their history. A live extension-backed pane that sends
no status hooks within 30 seconds shows **⚠ status missing** in the dashboard.
Check the loaded tclaude extension, extension API compatibility and configured
hook executable (for pi, `TCLAUDE_PI_HOOK_BIN`). The warning clears when a valid
hook arrives; the daemon logs it once per launch.

**Context meter and cost.** The extension reports pi's own context usage and
what the session has spent, priced from pi's model catalog, on every event.
Under a subscription login (ChatGPT, GitHub Copilot) that price is only what
the tokens would have cost, so it is recorded as what-if cost. A pinned
auto-compaction window (`--auto-compact-window`) makes the extension compact
between runs once the context reaches it; pi's own threshold near the model's
full window stays the backstop. A cost factor for pi can be set under
`cost.harness_factors`.

**Extension drive (experimental).** `tclaude agent spawn --pi-drive` (or
`pi_drive` in the spawn request) takes the new agent off tmux send-keys. The
same tclaude extension holds a long poll on the agent socket
(`POST /v1/whoami/pi-drive`) and runs what the daemon sends through pi's own
API: inbox messages and reminders as user turns (steered into a running turn,
as a typed message would be), compaction with its follow-up, rename
(`setSessionName`) and the soft stop. As with the other API drives, an
opted-in agent is channel-only: while the extension is not polling, messages
stay in the inbox and are delivered when it reconnects, and a rename,
compaction or soft stop fails instead of being typed (a stop still ends the
pane). Only the pi process itself can hold the channel: a process pi started
(a tool's shell or script) is refused. The choice is per launch, recorded with
the agent and kept on resume; a launch without it is told so, and its extension
stops asking.

`tclaude ask` and prompt-based name refinement are supported; see
[Asking questions](ask.md) for pi's one-shot behavior.

## Related pages

- [Sessions](sessions.md) — launching, attaching, and the shell hack.
- [Sandboxing](sandboxing.md) — the two sandbox axes and profile model.
- [Network filtering](network-filtering.md) — filtered launches per harness.
- [Spawning and lifecycle](spawning-and-lifecycle.md) — spawn profiles,
  lifecycle verbs, séance.
- [Utilities](utilities.md) — status line, context trimming, usage tooling.
- [Adding a harness](adding-a-harness.md) — the contributor recipe.

### Federated model gateway

Claude Code, Codex and Copilot workers support `--model-proxy <name>@<peer>`
through a session-bound loopback gateway. Claude Code uses Anthropic Messages;
Codex uses OpenAI Responses through a launch-only custom provider, covering its
TUI and app-server drive. Copilot CLI 1.0.91 or newer uses HTTP Responses BYOK
with `COPILOT_OFFLINE=true`, covering its TUI and embedded API drive. Copilot
requires an explicit model (`auto` is refused); offline mode disables GitHub
login, telemetry, GitHub MCP and web tools. Provider headers, key commands,
registry inputs and ambient authentication cannot replace the launch bearer.

Explicit `off` overrides profile defaults. Saved provider logins cannot replace
the pinned gateway route; conflicting overrides refuse the launch. See
[Model gateways](federation.md#model-gateways-claude-code) for permissions,
limits, usage and shutdown controls.

OpenCode remains unsupported even though it supports custom provider URLs and
already carries named HTTP proxies. Its managed server loads persistent
account/org and managed configuration after inline provider config; plugin
hooks can also change provider settings. The existing filtered-launch guards
do not prove gateway isolation across ordinary launches and resumes. A future
binding must establish that invariant for the authoritative server and all
primary, small-model and subagent choices. See the pinned upstream
[config merge order](https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/config/config.ts)
and [provider hooks](https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/provider/provider.ts).

Gemini CLI remains unsupported. `GOOGLE_GEMINI_BASE_URL` redirects only its
native Gemini API-key route, which uses GenerateContent rather than Anthropic
Messages or Responses. A binding needs native path/model authorization,
streaming usage accounting and output reservations, plus forced API-key auth
that cannot fall back to saved Google OAuth, Vertex settings or `.env` inputs.
See [Gemini configuration](https://geminicli.com/docs/reference/configuration/).
Both harnesses refuse a gateway request with the reason before launching.
Non-interactive one-shot runs also refuse a proxy choice.

### tclaude-managed instructions

Group/user managed rules and skills are scoped to each managed agent launch,
never installed into the user's harness HOME. Rules use the startup briefing and
are re-injected after compaction/clear from the frozen launch snapshot.

| Harness | Managed skill loading |
| --- | --- |
| Claude Code | Per-launch `--plugin-dir` when the installed CLI advertises it; briefing paths otherwise |
| Codex | Agent-private SKILL.md paths in the briefing |
| OpenCode | Agent-private SKILL.md paths in the briefing |
| Copilot | Agent-private SKILL.md paths in the briefing |
| Gemini | Agent-private SKILL.md paths in the briefing |
| pi | Native per launch: the launch directory as a command-line package (`--extension`), so a managed skill shadows a same-named ambient one; rules appended to the system prompt by tclaude's pi extension every turn, so they survive compaction and pi still loads the operator's `APPEND_SYSTEM.md` |

The fallback lists instructions to read on demand; it does not claim native
slash-command registration. See [Managed skills and rules](agents-and-groups.md#managed-skills-and-rules).
