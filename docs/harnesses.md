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
| **Conversation list & search** (`conv ls`/`search`) | ✅ cwd-indexed `.jsonl` | ✅ date-indexed rollout + state DB |
| **Rename** | ✅ in-pane `/rename` (writes the conversation file) | ✅ out-of-band (writes Codex's title store) |
| **Compact** | ✅ in-pane `/compact` | ✅ in-pane `/compact` |
| **Graceful stop** | ✅ `/exit` | ✅ `/quit` |
| **Reincarnate / clone** | ✅ | ✅ (rename degrades to the title store) |
| **Hooks / live status** | ✅ `~/.claude/settings.json` | ✅ `~/.codex/hooks.json` (+ one-time trust) |
| **OS sandbox at spawn** | ⚙️ configured in `settings.json` | ✅ `--sandbox` flag, secure default for agents |
| **Approval posture at spawn** | ⚙️ configured in `settings.json` | ✅ `--ask-for-approval` flag, non-blocking default for agents |
| **Guardian auto-review** | ❌ not applicable | ⚙️ opt-in `--auto-review` (experimental) |
| **Status bar** | ✅ command-backed statusline | ⚠️ curated built-in status items |
| **Dashboard** | ✅ | ✅ (with a harness badge + per-harness spawn menu) |

Legend: ✅ supported · ⚙️ available, opt-in / configured elsewhere · ⚠️ partial ·
❌ not available.

### Sandbox & approval defaults (Codex)

Codex has a built-in OS-level sandbox and an approval policy, both selectable at
launch. tclaude uses them to keep **unattended, daemon-spawned** Codex agents
safe and non-blocking:

- **`--sandbox`** — `read-only` | `workspace-write` | `danger-full-access`.
  Daemon-spawned Codex agents (via `agent spawn`, resume, clone, reincarnate)
  default to **`workspace-write`** containment, which makes only the working
  directory (plus `/tmp`/`$TMPDIR`) writable — `$HOME` stays read-only, so the
  agent can't tamper with `~/.tclaude`, `~/.codex`, or `~/.claude`. To reach
  that default, the daemon launches with a tclaude-managed **permission
  profile** (`codex -p tclaude-agent`) rather than the raw `--sandbox` flag:
  Codex's `workspace-write` sandbox otherwise blocks the agentd Unix socket, so
  a sandboxed agent couldn't run `tclaude agent …` at all. The profile gives the
  same workspace-write containment **plus** an allowlist for exactly that socket
  (JOH-207). At spawn time, when the launch directory is inside a Git repo, the
  profile also grants write access to that repo's Git common dir so agents can
  commit from linked worktrees while the rest of `$HOME` stays read-only.
  `read-only` and `danger-full-access` still use the plain `--sandbox` flag. A
  direct `tclaude session new --harness codex` is *your* session, so it does
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
