# Harnesses

tclaude started life as a wrapper around [Claude Code](https://claude.ai/code).
It is now **harness-agnostic**: the session, conversation, agent-coordination,
and dashboard machinery can drive more than one coding *harness* (the underlying
agentic CLI). **Claude Code, OpenAI Codex CLI, and OpenCode are registered
harnesses.** OpenCode support covers the managed serve-and-attach launch path,
its conversation store, ad-hoc ask, and per-session tool permissions; full
status mapping remains intentionally capability-gated. Claude remains the
default so existing commands and databases keep their historical behavior when
no harness is recorded.

A *harness* is whichever CLI actually runs the model in the tmux pane —
`claude`, `codex`, or an `opencode attach` client. tclaude owns everything
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
change that whole tool block uniformly. This is deliberately described as lexical soft access
control, not an OS sandbox: OpenCode does not resolve symlinks before permission
evaluation, and tool permissions such as bash/glob/grep cannot be scoped to the
same lexical disk boundary, so they can reach outside the authored paths.
Because `access-control` reads like a sandbox without confining like one, the
spawn dialog, profile/role editors, and `session new` surface an operator
warning whenever it is selected (the same channel as Claude Code's
unsandboxed-autonomy warning) — attaching a filesystem/network sandbox profile
does not change this, since those profiles compile into the same soft OpenCode
rules rather than an OS sandbox, pending a future OS sandbox layer for OpenCode.
The explicit `off` mode removes path scoping but keeps the selected approval
policy; bash is never auto-approved there. A bare direct `session new --harness
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
| **OS sandbox at spawn** | ✅ per-session `inherit`/`on`/`off` (delivered as a `--settings` override); `inherit` (default) keeps your `settings.json` config | ✅ managed profile (default) or raw `--sandbox` flag | ⚠️ no native OS sandbox; `access-control` (default) applies lexical path rules but cannot prevent symlink traversal, `off` removes scoping |
| **Approval posture at spawn** | ✅ per-session `--permission-mode` (inherit + Claude's modes); `auto` (default) runs the supervisor classifier, non-blocking for detached agents; `inherit` keeps `settings.json` + the agentd approval popup | ✅ `--ask-for-approval` flag, non-blocking default for agents | ✅ per-session `deny` (default), `ask`, or `allow-tools`; access-control keeps the tool baseline enabled, while `off` never auto-approves bash |
| **Built-in tool governance at spawn** | ➖ not a separate axis | ➖ not a separate axis | ✅ `--tools allow|ask|deny` applies uniformly to bash, glob, grep, LSP, task, and skill in `access-control`; `allow` is the backward-compatible default |
| **AskUserQuestion timeout at spawn** | ✅ per-session `inherit`/`never`/`60s`/`5m`/`10m` (delivered as a `--settings` override); `inherit` (default) keeps your `settings.json` value — set an interval per-agent / by profile so an unattended agent auto-continues instead of stalling on a question | ➖ no AskUserQuestion dialog | ❌ adapter pending |
| **Auto-approve review** | ⚙️ `auto` permission mode — a separate supervisor model approves/blocks each action | ⚙️ opt-in `--auto-review` (guardian subagent, experimental) | ❌ no reviewer equivalent |
| **Auto memory at spawn** | ⚙️ **off by default** — tclaude injects `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` so agents sharing a repo don't cross-pollute Claude Code's one per-project memory store; opt back in per-spawn or by profile (`auto_memory`). Does not affect `CLAUDE.md` | ➖ no auto-memory system | ➖ no auto-memory system |
| **Status bar** | ✅ command-backed statusline | ⚠️ curated built-in status items | ⚠️ OpenCode TUI status only |
| **Background shell tracking** ([dashboard](dashboard.md)) | ✅ `Bash` with `run_in_background` — tracked per task id and reconciled against live descendant processes, so an agent waiting on one shows `⚙+N` instead of `idle` | ➖ no background-shell mechanism | ➖ no background-shell mechanism |
| **Dashboard** | ✅ | ✅ (with a harness badge + per-harness spawn menu) | ✅ (harness badge + managed launch) |

Legend: ✅ supported · ⚙️ available, opt-in / configured elsewhere · ⚠️ partial ·
❌ not available.

### Sandbox & approval defaults (Codex)

Codex has a built-in OS-level sandbox and an approval policy, both selectable at
launch. tclaude uses them to keep **unattended, daemon-spawned** Codex agents
safe and non-blocking:

- **Launch containment** — the spawn dialog (and `--sandbox`) offers four
  options: **`tclaude-agent`** (the recommended default), plus the three raw
  Codex modes `workspace-write` | `read-only` | `danger-full-access`.
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

These are launch-time flags only. Directory trust is the one exception:
an explicit `trust_dir` opt-in, and every verified default sibling worktree,
adds an idempotent trusted-project entry to `~/.codex/config.toml` before Codex
starts so a detached agent cannot freeze on the trust-folder modal. The managed
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
  arrays with the operator's existing scopes.
- **`on`** — forces the OS sandbox **on** for this session even if `settings.json`
  leaves it off. It injects the same `sandbox` block as the global hardening
  (single source of truth), so the **agentd Unix socket stays reachable** (the
  agent can still run `tclaude agent …`) and `~/.tclaude` / `~/.claude/sessions`
  are hidden (read + write), so the sandboxed agent can't snoop on or tamper with
  shared daemon state. The same proof-pinned repository write paths described
  above are included.
- **`off`** — forces the sandbox **off** for this session even if `settings.json`
  enables it (the agent's Bash runs unconfined).

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

### OpenCode managed server

Each daemon-spawned OpenCode session has one agentd-owned
`opencode serve --hostname 127.0.0.1 --port <port>` process. agentd generates a
unique password, stores it only in private daemon state, and supplies it to the
server and attach client through their environments. The pane command is always
`opencode attach http://127.0.0.1:<port> --dir <cwd> --session <ses_…>`; it
never starts a second server and the password never appears in the command or
process arguments.

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
path-scoped file permissions. In `off`, bash may ask under `ask` or
`allow-tools`, but is never automatic; tool governance is not applied, because
`off` remains the explicit no-containment posture. An `off` launch rejects an assigned
filesystem or network sandbox profile rather than silently discarding it;
select `access-control` or remove the incompatible profile.

Sandbox-profile network access controls OpenCode's `webfetch` and `websearch`
tools only; it is not process-level network isolation. Protected tclaude,
harness, and tmux paths named directly are denied after ordinary directory
grants. OpenCode evaluates lexical paths rather than resolved filesystem
targets, so these tool rules do not prevent traversal through a symlink that
already exists inside an allowed root; use a Claude/Codex OS sandbox when that
is a required security boundary. An explicit break-glass grant is emitted last
so the acknowledged narrower exception wins.

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
