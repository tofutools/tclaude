# Harnesses

tclaude started life as a wrapper around [Claude Code](https://claude.ai/code).
It is now **harness-agnostic**: the session, conversation, agent-coordination,
and dashboard machinery can drive more than one coding *harness* (the underlying
agentic CLI). **Claude Code, OpenAI Codex CLI, OpenCode, and GitHub Copilot CLI
are registered harnesses.** OpenCode support covers the managed serve-and-attach
launch path, its conversation store, ad-hoc ask, and per-session tool
permissions; full status mapping remains intentionally capability-gated. Copilot
is a deliberately minimal first wave — launch, exact resume, model/effort, and
the in-pane control commands, and nothing else yet (see [GitHub Copilot CLI
(first wave)](#github-copilot-cli-first-wave)). Claude remains the default so
existing commands and databases keep their historical behavior when no harness
is recorded.

A *harness* is whichever CLI actually runs the model in the tmux pane —
`claude`, `codex`, `copilot`, or an `opencode attach` client. tclaude owns everything
around it: the tmux session, the status tracking, the conversation index, the
agent group/messaging layer, and the dashboard. Each harness plugs into the
same seam and contributes only the parts that are genuinely harness-specific
(how to launch it, where it stores conversations, which in-pane commands it
understands).

The common contracts are production paths, not an experimental alternative:
sessions, conversations, `ask`, mixed-harness agent groups, lifecycle, hooks,
and the dashboard all understand Codex. The harnesses still expose different
native features, so use the [capability matrix](#capability-matrix) instead of
assuming every Claude Code control has a Codex equivalent (or vice versa).

!!! note "`--harness shell` is not a harness"
    `tclaude session new --harness shell` starts a plain, ephemeral
    interactive shell — no conversation, no hooks. It's handled entirely
    inside the `session` package and is deliberately **not** registered
    here: it won't show up in `tclaude setup --harness`, `agent spawn
    --harness`, group spawn templates, or `conv ls`, none of which apply to
    a session with no conversation. See [Shell sessions](sessions.md#shell-sessions).

## Choosing a harness

The primary launch surfaces (`tclaude`, `session new`, and `agent spawn`) take a
`--harness` flag. For a fresh raw terminal session (`tclaude` or `tclaude
session new`), an omitted flag inherits the dashboard's global default spawn
profile. With no global profile, tclaude chooses a harness installed on `PATH`,
preferring Claude Code when both Claude and Codex are available; with neither
installed, it retains the historical Claude fallback so launch reports the
missing executable. An explicit flag always wins. Agent/group launches use
saved profiles and their fuller precedence described in [Agent
Coordination](agent.md#spawn-profiles).

```bash
# Start Claude Code explicitly, regardless of the global profile
tclaude session new --harness claude

# Start a Codex CLI session
tclaude session new --harness codex

# Spawn a Codex agent into a group (via the daemon)
tclaude agent spawn --group mygroup --name worker --harness codex

# Spawn an OpenCode agent. agentd starts the authenticated server and the pane
# attaches to the server-minted conversation.
tclaude agent spawn --group mygroup --name worker --harness opencode

# Keep OpenCode's path policy, but require approval for its built-in tool block
tclaude agent spawn --group mygroup --name worker --harness opencode --tools ask

# Start a GitHub Copilot CLI session (interactive human sessions; see the
# first-wave caveats below before reaching for agent spawning)
tclaude session new --harness copilot --model gpt-5.4
```

The harness is **persisted per conversation** (a `harness` column on the
session/conv tables, defaulting to `claude`). Conversation-oriented and agent
lifecycle operations such as `conv resume`, rename, stop, reincarnate, and clone
look up that recorded harness automatically.

The lower-level `session new --resume` command is the exception: it selects a
harness before searching that harness's conversation store. Add `--harness
codex` there when resuming a Codex conversation, or use `conv resume`, which
detects the harness for you.

OpenCode's supported launch surface is currently the agentd-owned
`agent spawn`/agent resume path. Its default `access-control` mode applies
tclaude-generated, per-session OpenCode tool rules: reads and representable
edits follow relative path patterns, while bash, glob, grep, LSP, task, and
skill default to `allow`. Set the independent `--tools allow|ask|deny` axis to
change that whole tool block uniformly. OpenCode's access-control mode is a
command filter, not confinement or an OS sandbox. Shell redirection, symlinks,
and subprocess binaries bypass its fixed command/path checks and reach outside
the authored paths.
Because `access-control` reads like a sandbox without confining like one, the
spawn dialog, profile/role editors, and `session new` surface an operator
warning whenever it is selected (the same channel as Claude Code's
unsandboxed-autonomy warning) — attaching a filesystem/network sandbox profile
does not change this by itself, since those profiles compile into the same soft
OpenCode rules.

On Linux or macOS, selecting sandbox implementation `tclaude-layer` adds an OS boundary
around the agentd-owned, tool-executing OpenCode server and records the
OpenCode sandbox mode as `tclaude-layer`. The attach pane remains outside that
boundary. Under the supported host-open posture, the authenticated loopback
control plane, host network, and ambient host Unix sockets remain reachable,
so this remains a partial boundary even though the established outer
implementation earns the dashboard row's `🔒`. The compact tooltip reports
`Status: ON` and `Implementation: TClaude`; it does not restate these caveats.
The inner OpenCode access profile permits all paths and does not compile the
sandbox profile's path scoping; the selected approval policy and independent
tool-governance setting remain active. Linux uses bubblewrap; macOS uses
Seatbelt through `sandbox-exec`. On macOS, per-agent mutable XDG privacy covers
OpenCode data, cache, and state. When an ambient global OpenCode config
directory exists, the config base is not redirected because Seatbelt cannot
project it onto a private path: that directory is daemon-final read-only, but
non-OpenCode config writes inside the wall target the real host config base and
remain governed by the filesystem policy. With no ambient global config, the
empty private config base remains in use.

The Linux executor also has a versioned, tclaude-owned Unix relay for carrying
its authenticated control plane across an isolated or filtered network
namespace. The server receives only an inherited listener fd; agentd dials the
recorded Unix socket directly, and `opencode attach` runs behind a pre-bound
local shim. OpenCode still refuses isolated profiles because its hosted model
traffic cannot cross that boundary. Filtered mode supports explicit-provider
configs only: the launch model and frozen `OPENCODE_CONFIG_CONTENT` must name
exactly one `@ai-sdk/openai-compatible` provider, one model, and a concrete
`options.baseURL` covered by the authored network list. Project/custom config,
ambient XDG and `$HOME/.opencode` config, model-catalog fetches, stored auth,
and plugins are suppressed for that launch; a model-level `provider` override
and an active persistent account/organization refuse because either can replace
the inspected adapter or route after inline config is parsed. The selected
provider-empty private config directories are daemon-final read-only, and both their
canonical contents and account/org absence are re-proved before every initial
server exec and persisted restart;
opaque, default, dynamically routed, and managed `/etc/opencode` provider
sources refuse with the remedy of making the route explicit or using network
open. The relay itself remains Linux-only.

OpenCode's sandbox-profile network permissions for its built-in `webfetch` and
`websearch` tools remain soft rules evaluated by OpenCode. They are separate
from the filtered `tclaude-layer` nft boundary, which packet-enforces every
process below the tool-executing server; the two must not be read as equivalent
security claims.
OpenCode deliberately has no `stacked` contract: profile apply and launch
refuse that selection by name instead of degrading to a single wall.
The explicit `off` mode removes path scoping but keeps the selected approval
and tool-governance policies. A bare direct `session new --harness
opencode` is refused because it has no authenticated managed-server handoff;
the pane is never allowed to start an independent OpenCode server.

## Per-harness setup

`tclaude setup` installs the hooks that power live status tracking and
notifications. Hooks live in a different place for each harness, so setup takes
the same `--harness` flag:

```bash
# Install Claude Code hooks (the default) into ~/.claude/settings.json
tclaude setup

# Install Codex hooks into ~/.codex/hooks.json
tclaude setup --harness codex
```

The Codex hook install is surgical and idempotent — it adds only tclaude's
callback and preserves any hooks you already have. Codex requires command hooks
to be trusted before they run; an explicit `tclaude setup --harness codex`
atomically trusts only the absolute-path tclaude hooks it just installed,
leaving unrelated user and repository hooks on Codex's normal review path. A
plain `tclaude setup` detects Codex on PATH and asks before installing and
trusting its hooks (`--yes` accepts that prompt). Declining leaves Codex
untouched. Re-running setup is safe and repairs stale trust after a command or
install-path change. Automatic trust fails closed on Codex versions whose
private hash contract tclaude has not verified; setup then leaves Codex's normal
manual hook review in place.

## Capability matrix

Each harness exposes a different surface. tclaude detects what a harness can do
through capability flags and degrades gracefully where a harness lacks a feature
(for example, Codex has no in-pane rename, so renames use Codex's title store
instead of slash-command injection).

| Capability | `claude` — Claude Code | `codex` — Codex CLI | `opencode` — OpenCode |
|---|---|---|---|
| **Spawn** | ✅ `claude` | ✅ `codex` | ✅ managed `serve` + pane `attach` |
| **Resume** | ✅ `claude --resume <id>` | ✅ `codex resume <id>` | ✅ managed server + `attach --session <id>` |
| **Ad-hoc ask** ([guide](ask.md)) | ✅ `claude [-p]`, conv-id pre-minted (`--session-id`) | ✅ `codex exec` (capture, read-only) / TUI (interactive), conv-id discovered post-turn | ✅ `opencode run --agent plan` (capture, best-effort read-only) / full TUI (interactive), server-minted conv-id discovered post-turn |
| **Live-streamed ask output** (print mode → a TTY) | ✅ `--output-format stream-json`, answer rendered token-by-token | ➖ buffered (`codex exec` prints the final message at the end) | ➖ buffered |
| **Conversation list & search** (`conv ls`/`search`) | ✅ cwd-indexed `.jsonl` | ✅ date-indexed rollout + state DB | ✅ cold `session list --format json` + tclaude cache |
| **Rename** | ✅ in-pane `/rename` (writes the conversation file) | ✅ out-of-band (writes Codex's title store) | ✅ authenticated server API; local title cache when cold |
| **Compact** | ✅ in-pane `/compact` | ✅ in-pane `/compact` | ✅ managed TUI API (`session.compact`, no keystrokes) |
| **Graceful stop** | ✅ `/exit` | ✅ `/quit` | ✅ managed TUI API (`app.exit`, no keystrokes) |
| **Remote control** ([guide](remote-control.md)) | ✅ Claude's built-in Remote Access (claude.ai/code + mobile app); arm per-agent, at spawn, or by profile/group default | ❌ no built-in remote access | ❌ no hosted relay |
| **Reincarnate / clone** | ✅ | ✅ (rename degrades to the title store) | ✅ managed resume + title store |
| **Hooks / live status** | ✅ `~/.claude/settings.json` | ✅ `~/.codex/hooks.json` (+ setup-managed trust) | ⚠️ managed liveness; full SSE mapping pending |
| **Built-in OS sandbox** (`SupportsBuiltinOSSandbox`) | ✅ SRT | ✅ native `--sandbox` | ❌ none; `access-control` is a command filter, not confinement |
| **OS sandbox at spawn** | ✅ implementation selector: built-in, tclaude-layer, stacked, or off; built-in mode offers `inherit`/`on` | ✅ implementation selector: built-in, tclaude-layer, stacked, or off; built-in mode offers managed profile (default) or raw confined modes | ⚠️ tclaude-layer confines the agentd-owned tool executor with documented caveats; off is explicit; `stacked` refuses; built-in `access-control` is a command filter, not confinement |
| **Filtered network under `tclaude-layer`** ([contract](sandboxing.md#isolated-with-agentd-network-posture)) | ⚠️ Linux CIDR/ports plus DNS-to-IP host/domain leases; Linux denies active (DNS-name deny is Partial under Allow all); macOS native loopback-only lists; mixed macOS lists are NotEnforced/open; exact provider context and explicit model endpoint coverage required; the inspected set includes the cached remote managed settings, whose live fetch and hourly in-process poll can still re-route a running session | ⚠️ same Linux allow/deny gateway and native-loopback boundary; DNS-name deny is Partial under Allow all; provider route resolved from Codex's own effective config via app-server `config/read`, so enterprise/MDM layers are included, and ChatGPT sign-in resolves to `chatgpt_base_url` plus `auth.openai.com` | ⚠️ Linux allow-list packet floor plus active denies (DNS-name deny is Partial under Allow all) for explicit-provider configs only; opaque/default/dynamic routes refuse, and the local convenience presets remain launch-refused — denies included — because they name no explicit provider endpoint |
| **Approval posture at spawn** | ✅ per-session `--permission-mode` (inherit + Claude's modes); `auto` (default) runs the supervisor classifier, non-blocking for detached agents; `inherit` keeps `settings.json` + the agentd approval popup | ✅ `--ask-for-approval` flag, non-blocking default for agents | ✅ per-session `deny` (default), `ask`, or `allow-tools`; access-control keeps the tool baseline enabled, while `off` never auto-approves bash |
| **Built-in tool governance at spawn** | ➖ not a separate axis | ➖ not a separate axis | ✅ `--tools allow|ask|deny` applies uniformly to bash, glob, grep, LSP, task, and skill in `access-control`; `allow` is the backward-compatible default |
| **AskUserQuestion timeout at spawn** | ✅ per-session `inherit`/`never`/`60s`/`5m`/`10m` (delivered as a `--settings` override); `inherit` (default) keeps your `settings.json` value — set an interval per-agent / by profile so an unattended agent auto-continues instead of stalling on a question | ➖ no AskUserQuestion dialog | ❌ adapter pending |
| **Auto-approve review** | ⚙️ `auto` permission mode — a separate supervisor model approves/blocks each action | ⚙️ opt-in `--auto-review` (guardian subagent, experimental) | ❌ no reviewer equivalent |
| **Directory pre-trust at spawn** ([below](#directory-trust-at-spawn)) | ✅ opt-in `trust_dir` seeds `projects.<dir>.hasTrustDialogAccepted` in `~/.claude.json` | ✅ opt-in `trust_dir` seeds `[projects."<dir>"] trust_level` in `~/.codex/config.toml` | ➖ no trust dialog, nothing to seed |
| **Auto memory at spawn** | ⚙️ **off by default** — tclaude injects `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` so agents sharing a repo don't cross-pollute Claude Code's one per-project memory store; opt back in per-spawn or by profile (`auto_memory`). Does not affect `CLAUDE.md` | ➖ no auto-memory system | ➖ no auto-memory system |
| **Auto-compaction window at spawn** | ✅ per-agent token window (`CLAUDE_CODE_AUTO_COMPACT_WINDOW`), set per spawn, by profile, or with `--auto-compact-window`; accepts `450000` / `450k` / `0.5M`. Unset uses the model's own threshold. Pin it below a 1M model's real window so a long-lived agent compacts while it is still sharp. Claude Code caps it at the model's actual window, and tclaude re-bases every context meter, bar and percentage onto whichever is smaller | ➖ no equivalent setting (Codex manages its own compaction) | ➖ no equivalent setting |
| **Startup-context trimming at spawn** ([guide](startup-context.md)) | ✅ per-agent catalog of `default`/`keep`/`trim` switches for bundled skills, unused tool schemas and system-prompt blocks — set per spawn, per profile, or in a group template; nothing is trimmed unless you ask | ➖ no equivalent switches | ➖ no equivalent switches |
| **Status bar** | ✅ command-backed statusline | ⚠️ curated built-in status items | ⚠️ OpenCode TUI status only |
| **Background shell tracking** ([dashboard](dashboard.md)) | ✅ `Bash` with `run_in_background` — tracked per task id and reconciled against live descendant processes, so an agent waiting on one shows `⚙+N` instead of `idle` | ➖ no background-shell mechanism | ➖ no background-shell mechanism |
| **Monitor tracking** ([dashboard](dashboard.md)) | ✅ `Monitor` watches — tracked per task id, bounded by the harness-enforced `timeout_ms`, and reconciled against live descendant processes (websocket watches have no process and are left to their deadline), so an agent watching a CI job shows `👁+N` instead of `idle`. Unavailable where Claude Code itself withholds the tool: Bedrock, Google Cloud's Agent Platform, Microsoft Foundry, `DISABLE_TELEMETRY` / `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, and the `background-tasks` context feature turned off | ➖ no monitor mechanism | ➖ no monitor mechanism |
| **Usage, cost & context** ([dashboard](dashboard.md)) | ✅ context %, tokens & real/what-if cost from the statusline hook | ✅ context %, tokens & what-if cost folded from appended rollout `token_count` records by a durable follower | ✅ context %, tokens & model projected from `/event` SSE usage and seeded from message history on reconnect; blank when model metadata is unavailable. OpenCode's reported non-zero cost is real spend, while zero-cost subscription usage gets a WHAT-IF estimate from native `/config/providers` pricing, including reasoning, cache, explicit long-context tiers, and the configurable legacy fallback (default cutoff: 272k context tokens per call) |
| **Dashboard** | ✅ | ✅ (with a harness badge + per-harness spawn menu) | ✅ (harness badge + managed launch) |

Legend: ✅ supported · ⚙️ available, opt-in / configured elsewhere · ⚠️ partial ·
❌ not available.

`copilot` is deliberately absent from the matrix above: its first wave
implements only the top four rows. See below.

### Group-route activation matrix

Group routes are a tclaude platform capability shared by harnesses; the harness
selected for a session does not widen the route boundary. See
[Group routes](group-routes.md) for the contract and exact-head evidence.

| Host boundary | Activation status | Evidence contract |
|---|---|---|
| Linux | ✅ Full, with a route-capable launch | Authenticated namespace-local helper and opaque named TCP stream; the existing provider/host policy floor remains in force. |
| macOS | ⚠️ Partial | Bounded exact Seatbelt TCP slots; same-port host-local reachability remains disclosed; provider and Internet policy floors remain in force. |
| Other platforms | ❌ unavailable | Explicit unsupported error; no silent policy downgrade. |

### GitHub Copilot CLI (first wave)

`--harness copilot` drives the [GitHub Copilot
CLI](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference)
TUI directly — there is no managed side server, unlike OpenCode. This wave
covers **interactive human sessions**:

| Capability | `copilot` — GitHub Copilot CLI |
|---|---|
| **Spawn** | ✅ `copilot`, with a caller-preset conv-id (`--session-id <uuid>`), a launch-time name (`--name=`), and an optional first turn (`-i <prompt>`) |
| **Resume** | ⚠️ `copilot --resume=<id>` (exact id only — never the picker, an id prefix, or a session name), but **only for a session you launched with an explicit `--session-id <uuid>`**: with no conversation store tclaude never discovers a Copilot conv-id on its own, so `--resume` has nothing to look up. Conversation discovery lands with the ConvStore wave |
| **Model & effort at spawn** | ✅ `--model=` (including `auto`) and `--effort=` (`low`…`max`, the same levels as everywhere else in tclaude). Note `max`: the flag accepts the whole vocabulary, but the docs describe `max` as the highest-depth tier **for Anthropic models** — Copilot may reject it for a GPT model, so pair it with a model that has that tier |
| **Rename / compact / graceful stop** | ✅ in-pane `/rename`, `/compact`, `/exit`. `/exit` closes the *current* session: with other sessions open in the same CLI it foregrounds the newest remaining one instead of quitting, so tclaude keeps its hard-kill fallback for a pane that doesn't actually exit |
| **Hooks / live status** | ✅ `<COPILOT_HOME>/hooks/tclaude.json` — a tclaude-**owned** drop-in file, merged by Copilot with the user's own hooks; no trust step, no `config.json` involvement. Registers `SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop`, `SessionEnd`, so a pane reports working/idle per turn. See [below](#copilot-hooks) for what is deliberately left out |
| **Everything else in the matrix** | ➖ not yet — no conversation store, ad-hoc ask, sandbox, approval, tool governance, directory pre-trust, remote control, usage/cost, or status bar |

Two consequences are worth stating plainly:

- A Copilot conversation does **not** appear in `conv ls`, search, or the
  dashboard's conversation views — those need the conversation store a later
  wave adds. Live status *does* work, via the hooks below.
- Copilot agents are not usable as detached agents in practice. Approval-lineage
  classification fails closed for a harness with no approval catalog, so a
  Copilot child is refused rather than spawned with an unproven posture.

The restraint is deliberate rather than incidental. This adapter was written
against the official GitHub documentation, without a Copilot binary available
to record fixtures. Documented launch flags are a stable contract; a
session-state layout, a hook payload, or a sandbox/approval guarantee is not —
and a descriptor that advertises a contract tclaude cannot honor is worse than
one that advertises none, because callers detect an absent contract and degrade
but cannot detect a lying one. Copilot's model catalog is likewise treated as a
suggestion list, not an allow-list: it brokers models from several vendors at
once (`claude-*`, `gpt-*`, `gemini-*`, `mai-*`) and exposes no machine-readable
catalog, so tclaude forwards any single bounded token verbatim — case included,
for custom/BYOK ids — and lets Copilot do the authoritative validation.

Two Copilot options tclaude deliberately never emits: `--mouse` / `--no-mouse`
(an explicit value is **persisted** to the user's configuration, so it is not
a per-spawn flag), and `-p/--prompt` (headless mode, which exits after
completion — a TUI pane wants `-i`).

#### Copilot hooks

Copilot's hook support rests on two behaviors the fixture lab observed from the
real 1.0.77 binary and GitHub documents neither of. Both are pinned by
committed captures under
`pkg/claude/harness/copilotfixture/testdata/<version>/hooks`, so a CLI that
changes either one fails a test instead of silently breaking live status.

1. **A tclaude-owned drop-in file fires.** `<COPILOT_HOME>/hooks/tclaude.json`
   is loaded with no `config.json` present, no folder trust and no git repo,
   and Copilot *merges* it with the user's own hooks. tclaude therefore never
   edits the shared `config.json` — which it could not do safely anyway: the
   CLI rewrites that file itself ("This file is managed automatically"),
   migrates a `hooks` key out of it into `settings.json`, and prefixes it with
   `//` banner lines that are not valid JSON.
2. **Claude Code's event names select Claude Code's payload.** Registered under
   their PascalCase names, Copilot's events arrive as `hook_event_name`,
   `session_id`, ISO-8601 timestamps, `tool_input` as an object — even Claude's
   tool *names* (`Bash`, not Copilot's `bash`). tclaude needs no translator:
   the same `HookCallbackInput` decodes Copilot as it already does Codex.

Four deliberate omissions, each for its own reason:

- **`PreToolUse` is not installed.** A non-zero hook exit *denies the user's
  tool call*, and tclaude's callback can legitimately fail when its receiver is
  down. `PostToolUse` reports the same tool a moment later at no such risk.
- **`PermissionRequest` is not installed.** It fires even under
  `--allow-all-tools` (it runs the rules engine, not a prompt), and it is the
  one event that ignores the dialect — it answers a PascalCase registration
  with a camelCase payload carrying no `session_id`. Copilot's real
  human-is-blocked signal appears to be `Notification(permission_prompt)`;
  enrolling it is TCL-976 work.
- **`UserPromptTransformed` is not installed** — same turn as
  `UserPromptSubmit`, and its payload is the model-facing rendering of the
  prompt.
- **No standing-order selectors.** Copilot *does* read hook stdout as a control
  channel, so tclaude's installed command ends in `>/dev/null`: a hook that
  prints `{"decision":"block"}` makes the agent keep working (one probe turned
  a single prompt into nine forced continuation cycles). Using that channel
  needs a designed, verified response contract; discarding stdout until then is
  the safe default.

Two more properties an operator may notice. Every entry carries an explicit
`timeoutSec` because hooks BLOCK the turn and Copilot's default timeout is 30
seconds. And Copilot fires `SessionStart` *after* the turn's first prompt —
the opposite of every other harness — which is why the descriptor carries
`SessionStartAfterPrompt`: without it, the session announcement would report a
just-started turn as idle.

`SessionEnd` stays best-effort. It has only been observed on clean runs, it
cannot fire on a SIGKILL, and it is at-least-once rather than exactly-once, so
exit detection remains the reaper's tmux/PID liveness.

#### Compatibility fixtures

The evidence that closes the gap above lives in
`pkg/claude/harness/copilotfixture`. It runs the **real pinned CLI**
(`@github/copilot@1.0.77`) against a deterministic localhost mock provider and
diffs sanitized observations against goldens in `testdata/<version>/`.

The path is **credential-free by construction**. Setting
`COPILOT_PROVIDER_BASE_URL` activates Copilot's BYOK mode, which the CLI
documents as not requiring GitHub authentication; on top of that the runner
removes every GitHub/Copilot token variable from the child environment and sets
`COPILOT_OFFLINE=true`, so a regression back into an auth dependency fails the
suite instead of passing on a machine that happens to be logged in. No
credential, enterprise policy, or real session content is involved.

Runs are also hermetic. `COPILOT_HOME` alone is not enough: it covers
config/session state, while `COPILOT_CACHE_HOME` redirects only Copilot's own
package cache and the bundled `Microsoft/DeveloperTools` cache still resolves
through `XDG_CACHE_HOME` then `HOME`. The runner redirects all four.

Scenarios covered: streaming text, a tool-call round trip, deterministic
provider failure, session enrollment plus exact resume, `--model` precedence,
`--effort` pass-through over a complete **OpenAI Responses**-wire turn, and a
launch driven by the **production spawner's own command string** rather than a
parallel flag table that could drift from it.

Both provider wires are covered, and they are genuinely different contracts
rather than a flag toggle: `completions` posts `messages[]` to
`/chat/completions` and ends its SSE at `data: [DONE]`, while `responses` posts
`input[]` plus a separate `instructions` string to `/responses` and ends at
`response.completed` with no sentinel. Reasoning effort is observable **only**
on the responses wire — the completions request body carries no effort key at
all — so the effort scenario runs there or it would assert nothing while
looking green.

Fixtures record *shape*, never content: endpoint, body key set, message roles,
tool-name set, the `x-initiator` discriminator, and event-type sequence. The
~26 kB system prompt and the tool schemas are reduced to a digest — committing
them would be a large, version-coupled blob that churns on every CLI bump while
proving nothing tclaude depends on. UUIDs, timestamps, ports, and absolute
paths are normalized before anything is written.

Running it, given the pinned CLI on PATH:

```bash
TCLAUDE_COPILOT_FIXTURE_SMOKE=1 go test ./pkg/claude/harness/copilotfixture/...
```

Without that variable the real-binary scenarios skip and only the (binary-free)
sanitizer unit tests run, so `go test ./...` stays green on a machine with no
Copilot install. CI runs the gated job on Linux and fails if any scenario
reports anything other than an explicit pass.

Version drift is deliberately manual: the pin lives in `version.go`, the test
asserts `copilot --version` matches it, and re-recording is an explicit
`-update` run whose diff **is** the compatibility evidence — so a floating or
auto-updating install cannot absorb a contract change silently.

### Sandbox & approval defaults (Codex)

Codex has a built-in OS-level sandbox and an approval policy, both selectable at
launch. The dashboard's primary **Sandbox** selector chooses the implementation
(Codex built-in, tclaude built-in, stacked, or Off). Selecting Codex built-in
reveals **Codex sandbox mode** below it. tclaude uses these controls to keep
**unattended, daemon-spawned** Codex agents safe and non-blocking:

- **Codex built-in mode** — the nested mode control (and `--sandbox`) offers
  **`tclaude-agent`** (shown as “Managed workspace + agent coordination” and
  recommended), plus the confined raw Codex modes `workspace-write` and
  `read-only`. Sandbox Off is selected in the primary implementation control
  and resolves to Codex's native `danger-full-access` mode.
  - **`tclaude-agent`** is *not* a Codex `--sandbox` mode — it selects a
    tclaude-managed **permission profile**. Each session launches with a
    unique `codex -p tclaude-agent-<launch-id>` profile derived from the
    `tclaude-agent` baseline, so concurrent agents cannot overwrite one
    another's repository grants.
    It gives the same `workspace-write` containment (only the working directory
    plus `/tmp`/`$TMPDIR` writable; `$HOME` read-only) while explicitly
    denying all filesystem access to `~/.tclaude`. The daemon exposes a
    state-free agent endpoint at `~/.tclaude/api/agentd.sock`, and the profile
    allowlists that socket so a sandboxed agent can run
    `tclaude agent …`. At spawn
    time, when the launch directory is inside a Git repo, the profile also grants
    write access to a minimal repository root: normally the safe container
    where tclaude creates default sibling worktrees, which also covers the
    original/main worktree and Git common dir. Codex protects `.git` pointer
    targets with a more-specific read-only mount, so tclaude separately grants
    the checkout's exact Git admin directory (the path reported by `git
    rev-parse --git-dir`). That lets an agent create `../<repo>-<branch>` and
    commit there while the rest of `$HOME` stays read-only. A container at/above
    `$HOME` is never granted; in that layout the original worktree is the narrow
    fallback root. A sandbox profile carrying a `deny` row over `$HOME` narrows
    this further: beneath the deny, tclaude reopens only the active workspace
    and exact verified Git common/admin paths, never the whole repository
    container. Direct sibling-worktree creation is therefore incompatible with a
    denied Home and must be created or brokered before launch. Such a reopen
    beneath a deny is Linux-only on Codex and requires the verified split-policy
    probe; it is refused on macOS. The
    operator, Codex, and Claude Code all use the same canonical state-free
    endpoint; agentd temporarily also serves the legacy
    `~/.tclaude-agentd.sock` and `~/.tclaude/agentd.sock` paths for
    older clients and installed settings. Daemon-spawned
    Codex agents (via `agent spawn`, resume, clone, reincarnate) default to it.
  - **`workspace-write` / `read-only` / `danger-full-access`** are passed through
    as the raw `--sandbox` flag. They do **not** get the agentd-socket allowlist
    (Codex ignores permission profiles when `--sandbox` is set), so an agent
    under one of these modes can't reach `tclaude agent`; `danger-full-access`
    turns the sandbox off entirely. `--sandbox tclaude-agent` is accepted as a
    shorthand and normalized to the managed profile.
  - A direct `tclaude session new --harness codex` is *your* session, so it does
    **not** inject a default — it respects your `config.toml`.
- **`--ask-for-approval`** — daemon-spawned Codex agents default to **`never`**
  so an unattended pane with no human at the keyboard can't deadlock waiting for
  an approval prompt. A direct `session new` again respects your config.
- **`--auto-review`** *(experimental, opt-in)* — routes a Codex agent's approval
  prompts to Codex's *guardian* subagent, which auto-decides in your place
  (fail-closed). Off by default; the underlying Codex key is still experimental,
  so treat it as unstable.

These are launch-time flags only. Directory trust is the one exception, and it
is not Codex-specific — see **Directory trust** below. For Codex it adds an
idempotent trusted-project entry to `~/.codex/config.toml` before the pane
starts. The managed
sandbox baseline lives in `~/.codex/tclaude-agent.config.toml`, installed by
`tclaude setup`. Spawn-time copies use launch-unique filenames and are removed
when their Codex process exits. If a persistent-config merge fails transiently,
the valid copy is retained so agentd can retry rather than silently losing the
choice. If Codex writes an app-tool **Always allow** choice into that active
temporary profile, agentd parses the complete TOML document and promotes only
explicit per-tool `approval_mode = "approve"` decisions into the persistent
`~/.codex/config.toml`; unrelated profile settings are ignored, and malformed
profiles are refused. Pane-exit cleanup repeats the reconciliation as a fallback.
Existing global decisions are never overwritten, including conflicting
decisions. A bounded startup sweep removes old copies left by forced stops or
host crashes. Agentd startup recovery reconciles only profiles whose recorded
Codex launch command still belongs to a live tmux pane; stopped-session
leftovers are left for the age-bounded sweep. Your other config and profiles
are left untouched. The research behind the defaults lives in the
`tclaude-harness-independence` Linear project
(JOH-166/JOH-167/JOH-200/JOH-207).

### Sandbox at spawn (Claude Code)

> The modes below decide *whether* containment is enforced. What it then does to
> a running agent — deny + reopen, the capability gate, and the failure modes —
> is in [Sandboxing](sandboxing.md).

Claude Code's OS sandbox lives in `settings.json` (a `sandbox` block), not a
launch flag — there is no `claude --sandbox`. tclaude still offers a **per-session
override** in the spawn dialog, profiles, and `tclaude session new`/`agent spawn
--sandbox`, delivered via Claude Code's `claude --settings '<json>'` (a JSON
string that merges over your user/project settings; only managed/policy settings
outrank it). Three modes:

- **`inherit`** *(default, recommended)* — does not override whether the sandbox
  is enabled. The agent runs under whatever your `settings.json` already
  configures (global, project, and any
  `tclaude setup --install-sandbox-hardening` you applied). This is why a
  daemon-spawned Claude agent's containment never silently changes: unlike Codex
  (where no flag means *no* sandbox, so the daemon must impose one), Claude Code's
  `settings.json` *is* the operator's chosen posture. For daemon-spawned agents
  inside a Git repository, tclaude merges only proof-pinned `filesystem.allowWrite`
  entries using the same proof-pinned repository paths; Claude Code merges these
  arrays with the operator's existing scopes. tclaude also merges one
  non-profile host-control rule for the exact named tmux socket hosting
  tclaude's agent panes. Linux enforces it through Claude's `denyRead`
  `/dev/null` mask whenever the inherited sandbox is enabled. Claude's built-in
  macOS settings have no exact Unix-socket deny. The hardening installer adds
  `allowAllUnixSockets: true` only when the key is missing and preserves an
  operator-selected `false`; with `false`, the exact `allowUnixSockets`
  allowlist applies instead. tclaude does not nest another Seatbelt around the
  harness. The explicit `tclaude-layer` implementation owns its existing tmux
  host-control rule instead.
- **`on`** — forces the OS sandbox **on** for this session even if `settings.json`
  leaves it off. It injects the same `sandbox` block as the global hardening
  (single source of truth), so the **agentd Unix socket stays reachable** (the
  agent can still run `tclaude agent …`) and `~/.tclaude` / `~/.claude/sessions`
  are hidden (read + write), so the sandboxed agent can't snoop on or tamper with
  shared daemon state. The same proof-pinned repository write paths described
  above are included.
- **`off`** — forces the sandbox **off** for this session even if `settings.json`
  enables it (the agent's Bash runs unconfined).

On Linux the tmux rule is socket-specific: agents may still run the `tmux`
binary against a private socket they own, but cannot connect to the existing
`tclaude` server and use `capture-pane`, `send-keys`, or session mutation.
Claude's built-in macOS sandbox cannot provide that exact distinction: broad
Unix-socket access leaves the tclaude server reachable, while an
operator-selected `allowAllUnixSockets: false` blocks every socket not listed
in `allowUnixSockets`. The sandbox editor shows the generated Claude rule and
this platform limitation as read-only launch context alongside Codex's managed
baseline.

This is the per-session counterpart to the **global** hardening guide
([`sandbox-hardening.md`](sandbox-hardening.md) / `tclaude setup
--install-sandbox-hardening`), which locks down your user-level `settings.json`
once for *all* agents; the two share the same `on` block so they can't drift.

### Permission / approval mode at spawn

The **approval axis** for Claude Code is its permission mode. The spawn dialog
(a "Permission mode" dropdown), profiles, and `--ask-for-approval` thread it
through to `claude --permission-mode <mode>`. Modes: **`auto`** *(default,
recommended)* — a supervisor model approves safe actions and blocks unsafe ones,
the most autonomous mode that keeps guardrails and the one best suited to a
detached pane; **`inherit`** adds no override, keeping your `settings.json`
permission rules and the agentd approval popup; then Claude Code's remaining
modes — `plan` (read-only), `acceptEdits`, `default`, `dontAsk` (auto-deny),
`bypassPermissions` (skip all checks). Because tclaude agents run **detached**,
the dialog's live hint flags the modes that can block on a prompt no human can
answer, auto-deny, or remove all guardrails — `inherit` included, since whatever
posture your `settings.json` holds is usually an interactive one. The OS sandbox (above) and the permission mode are
**orthogonal** — both layers apply.

Codex uses the same dashboard/profile control for its `--ask-for-approval`
axis: `never` (daemon default/recommended), `untrusted`, deprecated
`on-failure`, and `on-request`. The catalog comes from the same harness-owned
source used by CLI and profile validation, so UI options cannot drift from the
accepted policy set.

### Directory trust at spawn

Claude Code and Codex both block a first launch in a directory they do not yet
trust behind a *"do you trust this folder?"* dialog. A tclaude-spawned agent
runs detached in a tmux pane with nobody at its TUI, so that dialog is a startup
gate that can leave a freshly spawned agent frozen before it does anything.

tclaude can seed the trust record ahead of launch. Each harness keeps its own,
in unrelated shapes:

| Harness | Trust store | Entry |
| --- | --- | --- |
| Claude Code | `~/.claude.json` | `projects.<dir>.hasTrustDialogAccepted = true` |
| Codex CLI | `~/.codex/config.toml` | `[projects."<dir>"] trust_level = "trusted"` |
| OpenCode | ➖ | no trust dialog, nothing to seed |

Turn it on with the spawn dialog's **"Pre-trust this directory"** checkbox, a
spawn profile's `trust_dir`, or `tclaude session new --trust-dir`. The checkbox
names the file it will edit, and it is hidden for a harness with no trust
dialog.

This is the one launch control that writes to a config file tclaude does not
own, so it is deliberately narrow:

- **Never a default.** Off unless you explicitly ask for it. Requesting it for
  a harness with no trust dialog is an error, not a silently dropped flag.
- **Auto-trusted only for default sibling worktrees.** A worktree at the
  location tclaude itself picks (`../<repo>-<branch>`) is trusted automatically
  for both harnesses, so a freshly cut worktree doesn't stall.
- **Agents cannot widen it.** An agent-initiated spawn may pre-trust *only*
  such a sibling worktree; any other path it names is a `trust_dir_restricted`
  refusal. Ask a human to spawn that child instead. Note the layout check is
  paired with the dir write-proof, which independently requires the agent to
  prove write access to the worktree's Git admin dir — the two together are the
  guard, not the naming convention on its own.
- **Conservative writes.** Atomic (temp + rename), idempotent (an
  already-trusted dir is a clean no-op), and fail-safe — a config shape the
  editor cannot edit safely is refused rather than corrupted.
- **Best-effort.** If the write fails the agent still launches; clear the
  dialog once via the dashboard's focus button.

Note that `~/.claude.json` is a large file Claude Code rewrites constantly, so
that seed is last-writer-wins against a concurrent Claude Code write. It is
bounded — the idempotent no-op means a dir is written at most once, ever — and
what it could revert is Claude-owned churn, never your trust setting.

### OpenCode managed server

Each daemon-spawned OpenCode session has one agentd-owned
`opencode serve --hostname 127.0.0.1 --port <port>` process. agentd generates a
unique password, stores it only in private daemon state, and supplies it to the
server and attach client through their environments. The pane command is always
`opencode attach http://127.0.0.1:<port> --dir <cwd> --session <ses_…>`; it
never starts a second server and the password never appears in the command or
process arguments.

With sandbox implementation `tclaude-layer` on Linux, agentd starts that
`opencode serve` process as the child of the bubblewrap boundary and persists
the exact versioned launch spec beside the runtime row. The attach command is
not wrapped. The launch contract makes OpenCode's `~/.opencode` and XDG
data/cache/config/state directories writable without widening the rest of
Home, then re-hardens `~/.opencode/bin` read-only so a confined tool cannot
replace the executable used by a later host invocation. The frozen profile
environment, including generated agent-directory variables, is applied to the
server because its children execute the tools; the attach client remains only
a UI. A restart revalidates and replays that persisted spec; a wrapped row with
a missing, corrupt, or no-longer-valid spec is refused rather than restarted
unwrapped. The mode is normalized to `tclaude-layer` in the launch record;
pairing that mode with `harness-builtin`, or pairing the OpenCode mode `off` with the outer
implementation, is a launch error.

OpenCode has no `harness-builtin` OS sandbox. Leaving the implementation unset
keeps the historical behavior — its command filter plus the explicit warning —
but explicitly pinning `harness-builtin` is rejected. Pure replay of an older
recorded `harness-builtin` value is grandfathered because old rows did not
persist whether it was pinned or defaulted, and the two spellings grant the
same OpenCode posture. The dashboard therefore offers resolved defaults,
`tclaude-layer`, the disclosed-but-refused stacked option, and Off; it does not
mislabel OpenCode's access-control command filter as a built-in OS sandbox.

agentd waits for authenticated health, asks the server to mint the conversation
ID, delivers the startup prompt through `prompt_async`, consumes the
authenticated SSE stream, and treats both the server and attach pane as the
session's liveness contract. Resume reconstructs the same topology around the
recorded `ses_…` conversation. Model and reasoning-variant choices are loaded
from `opencode models openai --verbose` rather than a hard-coded catalog.

For managed launches, agentd compiles the resolved sandbox profile, approval
choice, and built-in tool governance into an ordered OpenCode permission suffix and stores that suffix with
the runtime. New sessions receive it at creation. A healthy reused server or a
resumed/restarted runtime reads the session through the authenticated public
API, appends the suffix only when it is absent, and verifies the server retained
it before considering reconciliation successful. This keeps the session policy
authoritative even when user or agent configuration contributes earlier rules.

The defaults are `access-control` + approval `deny` + tools `allow`: the working directory and
explicit read roots are readable, but edits, web tools, and unaudited
permissions are denied. `ask` lets a present human approve representable edits
and profile-enabled web tools, but can block a detached agent. `allow-tools`
automatically accepts scoped edits and explicitly enabled web tools.

The separate **Tool governance** selector applies one OpenCode permission
action to bash, glob, grep, LSP, task, and skill: `allow` runs them without a
prompt and preserves tclaude's earlier OpenCode behavior; `ask` prompts before
use and can stall a detached agent; `deny` blocks them. OpenCode v1.18.4
defines these three actions as run, prompt, and block respectively, and
evaluates the last matching rule as authoritative (see the [OpenCode permission
reference](https://opencode.ai/docs/permissions/)). The value is available in
the spawn dialog, spawn profiles, roles, and `agent spawn --tools`; clone,
resume, and reincarnate preserve the resolved launch value. It is intentionally
independent of the edit/web approval selector.

Those tool permission keys are separate from
`read`/`edit`/`external_directory` and cannot express the same lexical disk
boundary, so tool-driven disk access can reach outside the authored paths. This
is an accepted limitation of the soft sandbox, not an expansion of its
path-scoped file permissions. Tool governance remains authoritative in `off`:
`allow`, `ask`, and `deny` still apply uniformly to bash, glob, grep, LSP, task,
and skill even though no directory or OS containment remains. An `off` launch rejects an assigned
filesystem or network sandbox profile rather than silently discarding it;
select `access-control` or remove the incompatible profile.

Sandbox-profile network access controls OpenCode's `webfetch` and `websearch`
tools only; it is not process-level network isolation. Protected tclaude,
harness, and tmux paths named directly are denied after ordinary directory
grants. OpenCode evaluates lexical paths rather than resolved filesystem
targets, so these tool rules do not prevent traversal through a symlink that
already exists inside an allowed root; use a Claude/Codex OS sandbox when that
is a required security boundary. The protected tclaude denies are
unconditional: no profile can carve an exception out of them.

OpenCode conversations are enumerated through the supported
`opencode session list --format json` surface, including when no managed server
is live. Its per-session `directory` is the cwd/resume identity; tclaude mirrors
the list into `conv_index` for common dashboard and title readers. Rename uses
the authenticated managed-server API when available and a tclaude-local title
overlay when the conversation is cold. Direct reads or writes of OpenCode's
private `opencode.db` schema are deliberately avoided.

Full SSE-to-status mapping remains incomplete; see [the OpenCode
exploration](opencode-exploration.md) for the researched contracts and
remaining gaps.

The dashboard spawn dialog and spawn-profile editor show Codex's **Approval
reviewer** as a separate control: leave it unset/use the human reviewer, or
route eligible requests to **Codex auto-review**. This changes who decides an
approval request, not when one is created or what the sandbox permits. In
particular, auto-review has no effect with `never`, because that policy creates
no approval requests.

Agent-initiated spawns also enforce approval lineage: a parent cannot choose a
child posture with broader automatic command acceptance than its recorded launch
posture. Both sides are resolved to a normalized capability shape before they
are compared, so the same rules apply in every direction — Claude→Claude,
Codex→Codex, and cross-harness both ways. Claude `auto` is in-sandbox review,
not a boundary-escalation grant, so a Codex `never` parent may delegate to it;
`bypassPermissions` can only be minted by a parent that already holds it, or by
a human. See [Agent coordination](agent.md#spawn) for the capability matrix.

## What stays the same across harnesses

The common tclaude surfaces remain harness-agnostic:

- **Sessions** — tmux detach/reattach, `session ls`, attach, kill.
- **Conversations** — `conv ls`/`search` enumerate Claude, Codex, and OpenCode
  conversations side by side; `conv resume` resolves each through its owning
  harness and relaunches it in the recorded cwd.
- **Agent coordination** — groups, cross-session messaging, the inbox,
  permissions, cron nudges. A group can mix all three harnesses.
- **Dashboard** — one console for all agents, with a per-agent harness badge.
- **Identity & permissions** — agentd authorizes coordination RPCs by socket
  peer credentials regardless of harness.

## Adding another harness

The seam is designed so a third harness (Gemini CLI, Aider, …) is a *recipe*,
not a rewrite. See **[Adding a harness](adding-a-harness.md)**.
