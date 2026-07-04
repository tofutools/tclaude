# Harnesses

tclaude started life as a wrapper around [Claude Code](https://claude.ai/code).
It is now **harness-agnostic**: the session, conversation, agent-coordination,
and dashboard machinery can drive more than one coding *harness* (the underlying
agentic CLI). Claude Code is the default; **OpenAI Codex CLI** is the second
supported harness.

A *harness* is whichever CLI actually runs the model in the tmux pane — `claude`
or `codex`. tclaude owns everything around it: the tmux session, the status
tracking, the conversation index, the agent group/messaging layer, and the
dashboard. Each harness plugs into the same seam and contributes only the parts
that are genuinely harness-specific (how to launch it, where it stores
conversations, which in-pane commands it understands).

!!! note "Experimental"
    Multi-harness support is new. Claude Code is the mature, fully-supported
    path; the Codex integration is usable end-to-end but newer. File issues if
    something behaves differently between the two.

!!! note "`--harness shell` is not a harness"
    `tclaude session new --harness shell` starts a plain, ephemeral
    interactive shell — no conversation, no hooks. It's handled entirely
    inside the `session` package and is deliberately **not** registered
    here: it won't show up in `tclaude setup --harness`, `agent spawn
    --harness`, group spawn templates, or `conv ls`, none of which apply to
    a session with no conversation. See [Shell sessions](sessions.md#shell-sessions).

## Choosing a harness

Every place that launches a session takes a `--harness` flag. It defaults to
`claude`, so nothing changes unless you ask for Codex.

```bash
# Start a Claude Code session (the default — --harness claude is implied)
tclaude session new

# Start a Codex CLI session
tclaude session new --harness codex

# Spawn a Codex agent into a group (via the daemon)
tclaude agent spawn --group mygroup --name worker --harness codex
```

The harness is **persisted per conversation** (a `harness` column on the
session/conv tables, defaulting to `claude`). Every later operation — resume,
`conv ls`, rename, stop, reincarnate, clone — looks up the conversation's
recorded harness and does the right thing for it automatically. You do not pass
`--harness` again when resuming or managing an existing conversation.

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
callback and preserves any hooks you already have. Codex additionally requires
the hook config to be **trusted** once (a supply-chain safeguard); tclaude
writes the config so Codex's first-run trust prompt covers it. Re-running
`tclaude setup --harness codex` is always safe.

## Capability matrix

Each harness exposes a different surface. tclaude detects what a harness can do
through capability flags and degrades gracefully where a harness lacks a feature
(for example, Codex has no in-pane rename, so renames use Codex's title store
instead of slash-command injection).

| Capability | `claude` — Claude Code | `codex` — Codex CLI |
|---|---|---|
| **Spawn** | ✅ `claude` | ✅ `codex` |
| **Resume** | ✅ `claude --resume <id>` | ✅ `codex resume <id>` |
| **Ad-hoc ask** (`tclaude ask`) | ✅ `claude [-p]`, conv-id pre-minted (`--session-id`) | ✅ `codex exec` (capture, read-only) / TUI (interactive), conv-id discovered post-turn |
| **Live-streamed ask output** (print mode → a TTY) | ✅ `--output-format stream-json`, answer rendered token-by-token | ➖ buffered (`codex exec` prints the final message at the end) |
| **Conversation list & search** (`conv ls`/`search`) | ✅ cwd-indexed `.jsonl` | ✅ date-indexed rollout + state DB |
| **Rename** | ✅ in-pane `/rename` (writes the conversation file) | ✅ out-of-band (writes Codex's title store) |
| **Compact** | ✅ in-pane `/compact` | ✅ in-pane `/compact` |
| **Graceful stop** | ✅ `/exit` | ✅ `/quit` |
| **Remote control** ([guide](remote-control.md)) | ✅ Claude's built-in Remote Access (claude.ai/code + mobile app); arm per-agent, at spawn, or by profile/group default | ❌ no built-in remote access |
| **Reincarnate / clone** | ✅ | ✅ (rename degrades to the title store) |
| **Hooks / live status** | ✅ `~/.claude/settings.json` | ✅ `~/.codex/hooks.json` (+ one-time trust) |
| **OS sandbox at spawn** | ✅ per-session `inherit`/`on`/`off` (delivered as a `--settings` override); `inherit` (default) keeps your `settings.json` config | ✅ managed profile (default) or raw `--sandbox` flag |
| **Approval posture at spawn** | ✅ per-session `--permission-mode` (inherit + Claude's modes); `inherit` (default) keeps `settings.json` + the agentd approval popup | ✅ `--ask-for-approval` flag, non-blocking default for agents |
| **AskUserQuestion timeout at spawn** | ✅ per-session `inherit`/`never`/`60s`/`5m`/`10m` (delivered as a `--settings` override); `inherit` (default) keeps your `settings.json` value — set an interval per-agent / by profile so an unattended agent auto-continues instead of stalling on a question | ➖ no AskUserQuestion dialog |
| **Auto-approve review** | ⚙️ `auto` permission mode — a separate supervisor model approves/blocks each action | ⚙️ opt-in `--auto-review` (guardian subagent, experimental) |
| **Status bar** | ✅ command-backed statusline | ⚠️ curated built-in status items |
| **Dashboard** | ✅ | ✅ (with a harness badge + per-harness spawn menu) |

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
    tclaude-managed **permission profile** launched as `codex -p tclaude-agent`.
    It gives the same `workspace-write` containment (only the working directory
    plus `/tmp`/`$TMPDIR` writable; `$HOME` read-only, so the agent can't tamper
    with `~/.tclaude`, `~/.codex`, or `~/.claude`) **plus** an allowlist for
    exactly the agentd Unix socket — which the raw `--sandbox` modes block, so
    only under this profile can a sandboxed agent run `tclaude agent …`. At spawn
    time, when the launch directory is inside a Git repo, the profile also grants
    write access to that repo's Git common dir so agents can commit from linked
    worktrees while the rest of `$HOME` stays read-only (JOH-207). Daemon-spawned
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

These are launch-time flags only — tclaude never edits your `~/.codex/config.toml`.
The one file it manages is a standalone `~/.codex/tclaude-agent.config.toml`
(the permission profile above), installed by `tclaude setup` and self-healed at
spawn time; your own config and profiles are left untouched. The research behind
the defaults lives in the `tclaude-harness-independence` Linear project
(JOH-166/JOH-167/JOH-200/JOH-207).

### Sandbox at spawn (Claude Code)

Claude Code's OS sandbox lives in `settings.json` (a `sandbox` block), not a
launch flag — there is no `claude --sandbox`. tclaude still offers a **per-session
override** in the spawn dialog, profiles, and `tclaude session new`/`agent spawn
--sandbox`, delivered via Claude Code's `claude --settings '<json>'` (a JSON
string that merges over your user/project settings; only managed/policy settings
outrank it). Three modes:

- **`inherit`** *(default, recommended)* — adds **no** override. The agent runs
  under whatever your `settings.json` already configures (global, project, and
  any `tclaude setup --install-sandbox-hardening` you applied). This is why a
  daemon-spawned Claude agent's containment never silently changes: unlike Codex
  (where no flag means *no* sandbox, so the daemon must impose one), Claude Code's
  `settings.json` *is* the operator's chosen posture, so tclaude leaves it alone.
- **`on`** — forces the OS sandbox **on** for this session even if `settings.json`
  leaves it off. It injects the same `sandbox` block as the global hardening
  (single source of truth), so the **agentd Unix socket stays reachable** (the
  agent can still run `tclaude agent …`) and `~/.tclaude` / `~/.claude/sessions`
  are hidden (read + write), so the sandboxed agent can't snoop on or tamper with
  shared daemon state.
- **`off`** — forces the sandbox **off** for this session even if `settings.json`
  enables it (the agent's Bash runs unconfined).

This is the per-session counterpart to the **global** hardening guide
([`sandbox-hardening.md`](sandbox-hardening.md) / `tclaude setup
--install-sandbox-hardening`), which locks down your user-level `settings.json`
once for *all* agents; the two share the same `on` block so they can't drift.

### Permission mode at spawn (Claude Code)

The **approval axis** for Claude Code is its permission mode. The spawn dialog
(a "Permission mode" dropdown), profiles, and `--ask-for-approval` thread it
through to `claude --permission-mode <mode>`. Modes: **`inherit`** *(default,
recommended)* adds no override — the agent keeps your `settings.json` permission
rules and the agentd approval popup, so a daemon-spawned agent behaves exactly as
before; then Claude Code's six modes — `plan` (read-only), `acceptEdits`,
`default`, `auto` (classifier), `dontAsk` (auto-deny), `bypassPermissions`
(skip all checks). Because tclaude agents run **detached**, the dialog's live
hint flags the modes that can block on a prompt no human can answer, auto-deny,
or remove all guardrails. The OS sandbox (above) and the permission mode are
**orthogonal** — both layers apply.

> Codex's approval axis (`--ask-for-approval`) is still CLI/profile-only — it is
> not surfaced as a dialog dropdown yet; only Claude Code's permission modes are.

## What stays the same across harnesses

Everything tclaude owns is harness-agnostic and works identically for both:

- **Sessions** — tmux detach/reattach, `session ls`, attach, kill.
- **Conversations** — `conv ls`/`search` enumerate both harnesses' conversations
  side by side; resume works for either.
- **Agent coordination** — groups, cross-session messaging, the inbox,
  permissions, cron nudges. A group can mix Claude and Codex agents.
- **Dashboard** — one console for all agents, with a per-agent harness badge.
- **Identity & permissions** — agentd authorizes coordination RPCs by socket
  peer credentials regardless of harness.

## Adding another harness

The seam is designed so a third harness (Gemini CLI, Aider, …) is a *recipe*,
not a rewrite. See **[Adding a harness](adding-a-harness.md)**.
