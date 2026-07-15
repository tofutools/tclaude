# tclaude

`tclaude` is a tmux-based control layer for agentic coding CLIs. It gives
[Claude Code](https://claude.ai/code) and
[OpenAI Codex CLI](https://developers.openai.com/codex/cli) one durable place
for sessions, conversation history, agent coordination, and fleet operations.

Claude Code is the default harness for compatibility. Codex is a first-class
supported harness: choose it at launch, and tclaude remembers the choice for
every later resume and lifecycle operation. Where the two CLIs expose different
primitives, tclaude reports the difference instead of pretending the capability
exists. See [Harnesses](harnesses.md) for the matrix.

## Start with the workflow you need

| Goal | Start here |
|---|---|
| Keep coding sessions alive and move between them | [Session management](sessions.md) |
| Find or resume work from either harness | [Conversation management](conversations.md) |
| Ask a quick, resumable question from the shell | [Ask](ask.md) |
| Run a coordinated team of agents | [Agent coordination](agent.md) and the [dashboard](dashboard.md) |
| Work on branches in parallel | [Git worktrees](worktrees.md) |
| Search old Claude Code conversations by meaning | [Semantic search](semantic-search.md) |
| Model a repeatable, auditable workflow | [Processes](processes.md) |
| Reach the fleet away from the host | [Remote control](remote-control.md) or [remote access](remote-access.md) |

## Platform support

| Platform | Support |
|---|---|
| macOS | Supported |
| Linux | Supported |
| WSL | Supported as Linux, with some window-focus limitations |
| Native Windows | Not supported |

tmux is required for session management. You also need Claude Code, Codex CLI,
or both installed and authenticated.

## Installation

Installing tclaude has two parts: install the binary, then run setup. Setup is
required because hooks provide live status and notifications, and because the
local protocol/statusline integration is not part of the binary itself.

### 1. Install the binary

=== "Homebrew"

    On macOS or Linux:

    ```bash
    brew install tofutools/tap/tclaude
    ```

    The formula installs tmux and fetches the Go toolchain needed to build.

=== "Go"

    Requires Go 1.26+ and tmux:

    ```bash
    go install github.com/tofutools/tclaude@latest
    ```

=== "Prebuilt release"

    Download a Linux amd64/arm64 or macOS arm64 archive from the
    [Releases page](https://github.com/tofutools/tclaude/releases), extract it,
    and move `tclaude` onto your `PATH`.

### 2. Run setup

For the standard Claude Code integration plus the coordination skills most
users want:

```bash
tclaude setup --install-agent-skills --install-default-agent-permissions
```

Install or repair Codex hooks explicitly when you use Codex:

```bash
tclaude setup --harness codex
```

A plain `tclaude setup` configures Claude Code and, when Codex is on `PATH`,
offers to configure Codex too. Codex hook trust is handled conservatively; see
[Per-harness setup](harnesses.md#per-harness-setup) for the exact behavior.

The baseline setup:

- checks for tmux;
- installs Claude Code hooks and the command-backed statusline;
- offers to configure the fullscreen Claude Code TUI;
- configures clickable notifications for the host platform; and
- asks whether desktop notifications should be enabled.

Optional extras are additive and idempotent:

| Flag | Adds |
|---|---|
| `--install-agent-skills` | Bundled coordination skills for Claude Code and Codex CLI |
| `--install-default-agent-permissions` | Low-risk `self.*` grants used by those skills |
| `--install-sandbox-hardening` | Claude Code sandbox rules that protect agentd's private state |
| `--install-resume-threshold-override` | A Claude Code-only workaround for scripted resumes blocked by the summary chooser |
| `--install-all` | All optional extras above |

!!! note "Skills do not start the daemon"
    The coordination extras install skills and permissions. To use
    `tclaude agent`, also run `tclaude agentd serve` in a non-sandboxed shell.

Verify the installation whenever you upgrade or change harness versions:

```bash
tclaude setup --check
tclaude setup --check --harness codex
```

## First session

```bash
# Claude Code (the compatibility default)
tclaude session new

# Codex CLI
tclaude session new --harness codex

# Start detached instead of attaching immediately
tclaude session new --harness codex --detached
```

Detach from an attached tmux session with `Ctrl+B`, then `D`. Later:

```bash
tclaude session watch       # interactive list of running sessions
tclaude conv watch -g       # conversation history across all projects
tclaude conv resume <id>    # resumes through the recorded harness
```

You can also omit `session new`: running `tclaude` by itself starts a default
Claude Code session, and the root command accepts the same launch flags.

## Quick questions from the shell

`tclaude ask` runs a harness in the foreground, prints the answer, and returns
control to the same shell. The conversation continues per terminal and working
directory:

```bash
tclaude ask "explain the data flow in this package"
git diff | tclaude ask "what correctness risks do you see?"
tclaude ask --new "start a fresh topic"
tclaude ask -i "help me refactor this interactively"
```

Claude Code is the default for a fresh ask; a saved spawn profile can select
Codex. The [Ask guide](ask.md) explains continuity, capture safety, and profile
selection.

## Operate an agent fleet

Keep the daemon running in a non-sandboxed terminal:

```bash
tclaude agentd serve
```

Then open the dashboard:

```bash
tclaude agent dashboard
```

The same system is available through `tclaude agent`: create allow-listed
groups, message peers, spawn Claude or Codex agents, manage lifecycle and
permissions, save launch profiles, schedule nudges, and deploy task forces. A
group can mix harnesses. Start with the [Agent quick start](agent.md#quick-start)
or the [Dashboard guide](dashboard.md).

## Command map

| Command | Purpose | Harness notes |
|---|---|---|
| `session` | Start, list, attach, focus, and stop tmux sessions | Claude, Codex, or an ephemeral shell |
| `conv` | List, search, archive, resume, copy, move, and delete conversations | Shared list/search/resume; archive and file mutation are Claude Code-only |
| `ask` | Ask from the current shell using a resumable per-directory thread | Claude and Codex; configured profile chooses fresh-thread harness |
| `agent` / `agentd` | Coordinate and operate agent groups | Mixed-harness groups supported |
| `worktree` | Create, restore, switch, list, and remove Git worktrees | `worktree add` auto-launches Claude; use `--detached` then start Codex explicitly |
| `process` | Inspect and operate repeatable process templates/runs | Opt-in `features.processes` surface |
| `task` | Run a `TODO.md` queue with verify/review/commit loops | Claude Code only |
| `stats` / `usage` | Activity and subscription usage | Claude Code/Anthropic-specific CLI reports; dashboard also surfaces Codex usage |
| `memory-files` | Inspect and clean project memory files | Claude Code only |
| `remote-access` | Configure mTLS + passphrase access to the fleet dashboard | Independent of coding harness |

Use `tclaude <command> --help` for the live flag reference. Detailed guides
focus on workflows and the behavior that is easy to miss from help text.

## All guides

### Core use

- [Harnesses](harnesses.md) — setup, capability matrix, sandbox, approvals.
- [Sessions](sessions.md) — launch, attach, watch mode, status, shell sessions.
- [Conversations](conversations.md) — shared list/search/resume, plus Claude
  transcript archive and file management.
- [Ask](ask.md) — shell-native questions and persistent ask threads.
- [Worktrees](worktrees.md) — parallel branches and worktree-aware sessions.
- [Semantic search](semantic-search.md) — local Ollama-backed search.
- [Notifications](notifications.md) — desktop alerts and click-to-focus.

### Agent operations

- [Agent coordination](agent.md) — identity, groups, messaging, lifecycle,
  profiles, permissions, skills, task forces, and scheduling.
- [Agent dashboard](dashboard.md) — the browser operations console.
- [Remote control](remote-control.md) — Claude Code's built-in phone/website
  access, managed per agent or group.
- [Remote access](remote-access.md) — securely expose the fleet dashboard over
  a LAN, mesh VPN, or tunnel.
- [Sandbox hardening](sandbox-hardening.md) — protect agentd state while keeping
  the coordination socket available.
- [Processes](processes.md) — opt-in process templates, runs, evidence, and
  performers.

### Claude Code-specific utilities

- [Task management](tasks.md) — sequential `TODO.md` execution with verification
  and review loops.
- [Status bar](status-bar.md) — Claude Code's command-backed statusline.

### Contributors

- [Adding a harness](adding-a-harness.md) — implement and register another
  coding CLI through the capability-based seam.
