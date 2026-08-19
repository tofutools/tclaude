# Harnesses

A *harness* is the vendor coding CLI that actually runs the model inside a
tclaude-managed tmux pane. tclaude is harness-agnostic: sessions,
conversations, `ask`, agent groups, lifecycle, and the dashboard all work
across every harness, and a group can freely mix them.

Four harnesses are supported:

| `--harness` | Harness | Binary in the pane |
| --- | --- | --- |
| `claude` (default) | Claude Code | `claude` |
| `codex` | OpenAI Codex CLI | `codex` |
| `opencode` | OpenCode | managed `opencode serve` + an `attach` client |
| `copilot` | GitHub Copilot CLI | `copilot` |

tclaude owns everything around the pane — the tmux session, status tracking,
the conversation index, groups and messaging, the dashboard — while each
harness contributes only what is genuinely harness-specific: how to launch it,
where it stores conversations, and which in-pane commands it understands.

!!! note "`--harness shell` is not a harness"
    `--shell` starts a plain interactive shell in a managed tmux session —
    a convenience hack with no conversation, hooks, model, or sandbox. It is
    deliberately not part of the harness lineup. See
    [Sessions](sessions.md#shell-sessions).

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
   fresh terminal launches, each field overridable per launch.
3. **First installed harness** — Claude Code is checked first; otherwise the
   registry is walked in sorted order (codex, copilot, opencode) and the first
   harness whose binary is on `PATH` wins. With nothing installed the launch
   reports the missing `claude` executable.

Agent and group spawns resolve through the fuller spawn-profile precedence
described in [Spawning and lifecycle](spawning-and-lifecycle.md).

```bash
tclaude session new --harness codex
tclaude agent spawn --group crew --name worker --harness opencode
tclaude session new --harness copilot --model gpt-5.4
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

| Capability | Claude Code | Codex CLI | OpenCode | Copilot CLI |
| --- | --- | --- | --- | --- |
| Sessions: spawn / resume | ✅ | ✅ | ✅ managed server + attach | ✅ |
| One-shot [`ask`](ask.md) | ✅ live-streamed | ✅ buffered | ✅ buffered | ✅ buffered |
| [Conversation](conversations.md) list & search | ✅ | ✅ | ✅ | ✅ |
| Agent groups & messaging | ✅ | ✅ | ✅ | ⚠️ one launch topology only |
| Rename | ✅ in-pane `/rename` | ✅ title store | ✅ server API | ✅ in-pane `/rename` |
| Compact / reincarnate | ✅ | ✅ | ✅ (server API, no keystrokes) | ✅ |
| Séance (ask posture) replay | ✅ | ✅ | ❌ | ❌ |
| [Remote control](remote.md) | ✅ | ❌ | ❌ | ❌ |
| [Status line](utilities.md#status-line) | ✅ command-backed | ⚠️ curated built-in items | ⚠️ OpenCode's own TUI status | ❌ |
| [Task runner](tasks.md) | ✅ | ❌ | ❌ | ❌ |
| Built-in OS sandbox | ✅ | ✅ | ❌ command filter only | ❌ asserted off |
| [tclaude-layer outer sandbox](sandboxing.md) | ✅ | ✅ | ✅ (wraps the server) | ✅ |
| Usage / cost reporting | ✅ real + what-if cost | ✅ what-if cost | ✅ native pricing what-if | ⚠️ Copilot AIU units, no USD |
| Hooks via `tclaude setup` | ✅ | ✅ | ❌ (server liveness instead) | ✅ |
| Directory pre-trust (`--trust-dir`) | ✅ | ✅ | — no trust dialog | ✅ |
| Tool governance (`--tools`) | ❌ | ❌ | ✅ | ❌ |
| Fast mode | ❌ | ✅ | ❌ | ❌ |
| API/RPC drive | — n/a | ⚠️ experimental `--codex-app-server` | ✅ inherent | ⚠️ experimental `--copilot-api` |

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
  `--sandbox-impl tclaude-layer` sets, since tclaude's outer wall then
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
  interrupt as typed calls. Requires Codex CLI 0.147.x; an explicitly
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
background refresh, so it may briefly report empty rather than guess. It is a
suggestion list, not an allow-list: model fields also accept arbitrary IDs for
user-configured local or network providers. IDs retain OpenCode's
`provider/model` shape so tclaude can carry them through its managed prompt
API, with final resolution left to OpenCode.

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
governance active. OpenCode has no `stacked` contract; that selection is
refused by name.

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
is therefore tclaude's own outer wall with Copilot's sandbox asserted down.

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

## Related pages

- [Sessions](sessions.md) — launching, attaching, and the shell hack.
- [Sandboxing](sandboxing.md) — the two sandbox axes and profile model.
- [Network filtering](network-filtering.md) — filtered launches per harness.
- [Spawning and lifecycle](spawning-and-lifecycle.md) — spawn profiles,
  lifecycle verbs, séance.
- [Utilities](utilities.md) — status line, context trimming, usage tooling.
- [Adding a harness](adding-a-harness.md) — the contributor recipe.
