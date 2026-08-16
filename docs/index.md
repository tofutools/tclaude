# tclaude

`tclaude` is a self-hosted agentic dev environment: a Go CLI plus daemon that
wraps vendor coding CLIs ("harnesses") in tmux and adds the operations layer
for running many agents seriously — durable sessions, conversation history
and search, an operations dashboard, agent-to-agent mail, teams, identity and
permissions with audit, sandboxing, and automation.

Four harnesses are supported behind one workflow: Claude Code, OpenAI Codex
CLI, OpenCode, and GitHub Copilot CLI. tclaude records the harness on every
conversation, so listings, resume, lifecycle operations, and the dashboard
route through the correct CLI automatically. Where harnesses expose different
primitives, tclaude reports the difference instead of pretending the
capability exists — see [Harnesses](harnesses.md) for the capability matrix.

Everything beyond solo sessions routes through one daemon, `agentd`: identity,
permissions, audit, spawning, and mail. The daemon is fail-closed — there is
no path around it. For the mental model behind sessions, conversations,
agents, and the daemon, read [Architecture](architecture.md).

## Start with the workflow you need

| Goal | Start here |
|---|---|
| Understand the moving parts | [Architecture](architecture.md) |
| Pick and configure a harness | [Harnesses](harnesses.md) |
| Keep coding sessions alive and move between them | [Sessions](sessions.md) |
| Find or resume past work from any harness | [Conversations](conversations.md) |
| Ask a quick, resumable question from the shell | [Ask](ask.md) |
| Work on branches in parallel | [Worktrees](worktrees.md) |
| Get alerted when an agent needs you | [Notifications](notifications.md) |
| Form agent teams that talk to each other | [Agents and groups](agents-and-groups.md) |
| Spawn agents and manage their lifecycle | [Spawning and lifecycle](spawning-and-lifecycle.md) |
| Control what agents may do, and see what they did | [Permissions and audit](permissions-and-audit.md) |
| Deploy whole rosters from templates, on a schedule | [Teams at scale](teams-at-scale.md) |
| Watch the whole fleet from a browser | [Dashboard](dashboard.md) |
| Operate away from the host machine | [Remote](remote.md) |
| Confine what agents can touch | [Sandboxing](sandboxing.md) and [network filtering](network-filtering.md) |
| Give tokenless agents git/GitHub/Linear access | [Proxies](proxies.md) |
| Run enforced multi-step workflows | [Processes](processes.md) |
| Status bars, usage reports, DB inspection | [Utilities](utilities.md) |

## Platform support

| Platform | Support |
|---|---|
| Linux | Supported |
| macOS | Supported |
| WSL | Supported as Linux, with some window-focus limitations |
| Native Windows | Not supported |

tmux is required for session management. You also need at least one harness
CLI installed and authenticated: Claude Code, Codex CLI, OpenCode, or Copilot
CLI.

## Installation

Installing tclaude has two parts: install the binary, then run setup. Setup
is required because hooks provide live status and notifications, and because
the local protocol and statusline integration is not part of the binary
itself.

### 1. Install the binary

=== "Homebrew"

    On macOS or Linux:

    ```bash
    brew install tofutools/tap/tclaude
    ```

    The formula installs tmux and builds tclaude from source. It builds only
    the `tclaude` binary; if you also want the standalone `tclaude-agentd`
    daemon, add it through the Go or prebuilt path.

=== "Go"

    Requires Go 1.26+ and tmux:

    ```bash
    go install github.com/tofutools/tclaude@latest
    ```

    The daemon is built into that binary as `tclaude agentd serve`. It also
    ships as a standalone `tclaude-agentd` binary, which is a separate
    package path and therefore a separate install:

    ```bash
    go install github.com/tofutools/tclaude/cmd/tclaude-agentd@latest
    ```

    From a source checkout, `go install . ./cmd/...` installs both at once.

=== "Prebuilt release"

    Download a Linux amd64/arm64 or macOS arm64 archive from the
    [Releases page](https://github.com/tofutools/tclaude/releases), extract
    it, and move `tclaude` onto your `PATH`. Each binary and platform gets
    its own archive, named after the build that produced it: take a
    `tclaude-no-cgo_linux_*` or `tclaude-darwin_*` archive for the CLI, and
    the matching `tclaude-agentd-*` one if you also want the standalone
    daemon. The two are never packed together, so put both binaries on your
    `PATH` — that is how the daemon finds `tclaude`.

### 2. Run setup

For the standard integration plus the coordination skills and permissions
most users want:

```bash
tclaude setup --install-agent-skills --install-default-agent-permissions
```

The baseline setup (always runs):

- checks for tmux;
- installs Claude Code hooks in `~/.claude/settings.json` and offers the
  command-backed Claude Code status bar;
- when Codex is on `PATH`, offers Codex hooks and a curated Codex status
  line — no flag needed on a machine that has Codex;
- when Copilot CLI is present, offers enabling its copy-on-select clipboard
  bridging;
- registers the `tclaude://` protocol handler on WSL for clickable
  notifications; and
- asks once, on first run only, whether desktop notifications should be
  enabled.

`tclaude setup --harness codex` installs or repairs Codex hooks explicitly
(useful for scripted installs); the flag accepts `claude` and `codex` only —
OpenCode has no hook installer, and Copilot's hook drop-in is handled by the
baseline. See [Harnesses](harnesses.md) for per-harness setup details.

Optional extras are additive and idempotent:

| Flag | Adds |
|---|---|
| `--install-agent-skills` | Bundled coordination skills (`agent-*`, `human-*`, `process-templates`) for Claude Code and Codex CLI skill directories |
| `--install-proxy-skills` | Optional `proxy-git` and `proxy-linear` skills for operators using the [credential proxies](proxies.md); not included by `--install-all` |
| `--install-default-agent-permissions` | Low-risk permission slugs the bundled skills exercise, as agent defaults in `~/.tclaude/config.json` |
| `--install-sandbox-hardening` | Append-only sandbox and deny entries in `~/.claude/settings.json` that protect agentd's private state |
| `--install-resume-threshold-override` | A `claude_resume.threshold_minutes` override that suppresses Claude Code's interactive resume-from-summary prompt for scripted resumes |
| `--install-all` | All standard extras above, excluding `--install-proxy-skills` |

Proxy skills require explicit opt-in even under `--install-all`, so agents
are not shown proxy capabilities on installations where those services are
not configured.

!!! note "Skills do not start the daemon"
    The extras install skills and permissions. To use `tclaude agent`, also
    run `tclaude agentd serve` in a non-sandboxed shell.

Verify the installation whenever you upgrade or change harness versions:

```bash
tclaude setup --check
tclaude setup --check --harness codex
```

## First session

```bash
# Your default harness in the current directory
tclaude

# Pick a harness explicitly
tclaude session new --harness codex      # claude | codex | opencode | copilot

# Start detached instead of attaching immediately
tclaude session new --harness copilot --detached
```

Bare `tclaude` accepts the same launch flags as `session new`. With no
`--harness`, the choice comes from your global default spawn profile, falling
back to whichever harness is installed (Claude Code preferred). When the
daemon is running and an agent group's default directory matches your launch
directory, bare `tclaude` joins that group automatically — see
[Sessions](sessions.md) for the details.

Detach from an attached session with `Ctrl+B`, then `D`. Later:

```bash
tclaude session watch       # interactive list of running sessions
tclaude conv watch -g       # conversation history across all projects
tclaude conv resume <id>    # resumes through the recorded harness
```

## Quick questions from the shell

`tclaude ask` runs a harness in the foreground — no tmux session — prints
the answer, and returns control to your shell. The thread continues per terminal
and working directory, and piped input is attached as context:

```bash
tclaude ask "explain the data flow in this package"
git diff | tclaude ask "what correctness risks do you see?"
tclaude ask --new "start a fresh topic"
tclaude ask -i "help me refactor this interactively"
```

All four harnesses support ask. The [Ask guide](ask.md) covers continuity,
capture safety, and how the harness is chosen.

## Operate a fleet

Solo sessions work without the daemon. For anything multi-agent — groups,
messaging, spawning, permissions, the dashboard — keep `agentd` running in a
non-sandboxed terminal:

```bash
tclaude agentd serve      # or the standalone tclaude-agentd binary
```

Then open the dashboard:

```bash
tclaude agent dashboard
```

From the dashboard or `tclaude agent`, you create allow-listed groups, spawn
agents into them, message peers, manage lifecycle and permissions, and deploy
whole teams from templates. Groups freely mix agents from all four harnesses.
Start with [Agents and groups](agents-and-groups.md), then
[Spawning and lifecycle](spawning-and-lifecycle.md) and the
[Dashboard](dashboard.md).

Use `tclaude <command> --help` for the live flag reference; these guides
focus on workflows and the behavior that is easy to miss from help text.
