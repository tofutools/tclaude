# tclaude

[![CI Status](https://github.com/tofutools/tclaude/actions/workflows/ci.yml/badge.svg)](https://github.com/tofutools/tclaude/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tofutools/tclaude)](https://goreportcard.com/report/github.com/tofutools/tclaude)
[![Docs](https://img.shields.io/badge/docs-tofutools.github.io%2Ftclaude-blue)](https://tofutools.github.io/tclaude/)

`tclaude` is a self-hosted agentic dev environment: a Go CLI plus daemon that
wraps vendor coding CLIs ("harnesses") in tmux and adds the operations layer
you need to run many agents seriously. The bottleneck is no longer writing
code — it is operating the things that write code. tclaude is that layer:
durable sessions, searchable history, a fleet dashboard, agent-to-agent mail,
teams with identity and permissions, sandboxing, and automation, all running
on your own machine.

It wraps four harnesses — [Claude Code](https://claude.ai/code),
[OpenAI Codex CLI](https://developers.openai.com/codex/cli),
[OpenCode](https://opencode.ai), and
[GitHub Copilot CLI](https://github.com/features/copilot/cli) — behind one
workflow, so a team can mix vendors and switch as models improve. Everything
routes through one daemon, `agentd`, which owns identity, permissions, audit,
spawning, and mail — fail-closed. tclaude is MIT-licensed; the models are the
only external part.

![The tclaude operations dashboard watching a mixed-harness fleet](docs/assets/dashboard-groups.png)

## What it adds

- **Durable sessions and history** — every harness runs in an isolated tmux
  server: detach, reattach, resume, and watch live working/idle/blocked
  status. Conversations outlive their sessions and are indexed across all
  harnesses for listing, text search, and local semantic search.
- **Fleet observability** — a browser dashboard answers "what is my fleet
  doing" at a glance: per-agent status, cost, context left, branch/PR/CI,
  linked tickets, quota forecasts — with zoom from fleet to group to one
  agent's live terminal and back out.
- **Agent mail and teams** — every agent and the operator have a mailbox.
  Groups are allow-listed teams — flat or hierarchical, mixed-vendor by
  design — deployable from reusable templates with one mission statement.
- **Identity, permissions, and audit** — callers are identified from kernel
  socket peer credentials, not tokens. Grants are scoped, elevation asks the
  human, and the audit trail records denials as well as what landed.
- **Sandboxing and network filtering** — one Kubernetes-shaped sandbox
  profile, projected onto bubblewrap (Linux), Seatbelt (macOS), the
  harnesses' native sandboxes, and packet- or proxy-based network filtering.
  Children can never be less confined than their parents.
- **Automation** — scheduled nudges, standing orders, process-template
  workflows, credential proxies for tokenless git/GitHub/Linear access, and
  everyday helpers: shell-native `tclaude ask`, git worktree lifecycle,
  desktop notifications, and status bars.

The harnesses do not expose identical primitives, and tclaude reports the
differences instead of pretending capabilities exist: features degrade with a
clear message when a harness lacks the contract. See the
[capability matrix](https://tofutools.github.io/tclaude/harnesses/) for
exactly what each harness supports.

## Install

tclaude supports Linux and macOS. WSL is treated as Linux; native Windows is
not supported. You also need the CLI for whichever harness you intend to run.

Choose one installation method:

**Homebrew (macOS / Linux)**

```bash
brew install tofutools/tap/tclaude
```

The formula installs tmux and builds tclaude from source. It builds only the
`tclaude` binary; if you also want the standalone `tclaude-agentd` daemon, add
it through the Go or prebuilt path below.

**Go** — requires Go 1.26+ and tmux:

```bash
go install github.com/tofutools/tclaude@latest
```

The daemon is built into that binary as `tclaude agentd serve`. It also ships
as a standalone `tclaude-agentd` binary, which is a separate package path and
therefore a separate install:

```bash
go install github.com/tofutools/tclaude/cmd/tclaude-agentd@latest
```

From a source checkout, `go install . ./cmd/...` installs both at once.

**Prebuilt release** — download a Linux amd64/arm64 or macOS arm64 archive
from the [Releases page](https://github.com/tofutools/tclaude/releases),
extract it, and put `tclaude` on your `PATH`. Each binary and platform gets
its own archive,
named after the build that produced it: take a `tclaude-no-cgo_linux_*` or
`tclaude-darwin_*` archive for the CLI, and the matching `tclaude-agentd-*` one
if you also want the standalone daemon. The two are never packed together, so
put both binaries on your `PATH` — that is how the daemon finds `tclaude`.

### Run setup

Installation is not complete until setup has installed the hooks and local
integration:

```bash
# Harness hooks/status integration plus the agent skills and default
# permissions most users want
tclaude setup --install-agent-skills --install-default-agent-permissions

# Verify at any time
tclaude setup --check
```

Plain `tclaude setup` configures Claude Code, and auto-detects Codex and
Copilot on `PATH` to offer their integrations too. Setup is idempotent. The
optional `--install-all` flag also installs Claude Code sandbox hardening and
the scripted-resume threshold override; review those policies before enabling
them. Credential-proxy skills are a separate opt-in, excluded even from
`--install-all`, so agents are not shown proxy capabilities that are not
configured:

```bash
tclaude setup --install-proxy-skills
```

Full walkthrough: [Getting started](https://tofutools.github.io/tclaude/).

## Quick start

Start a solo session — bare `tclaude` launches your default harness in the
current directory:

```bash
tclaude                              # default harness (Claude Code unless
                                     # your default profile says otherwise)
tclaude session new --harness codex  # or opencode, or copilot

# Detach with Ctrl+B D, then browse or reattach later
tclaude session watch
tclaude conv watch -g
tclaude conv resume <id>             # resumes through the recorded harness
```

Ask from your shell without a session; the thread continues per terminal and
directory:

```bash
tclaude ask "what should I know before changing this package?"
git diff | tclaude ask "spot correctness risks in this diff"
```

To operate a fleet, keep the daemon running in a non-sandboxed terminal and
open the dashboard:

```bash
tclaude agentd serve      # or the standalone tclaude-agentd binary
tclaude agent dashboard
```

From there, `tclaude agent` (and the dashboard) create groups, spawn agents
into them, message peers, and manage permissions. Groups freely mix agents
from all four harnesses.

## Documentation

Full documentation lives at
[tofutools.github.io/tclaude](https://tofutools.github.io/tclaude/):

- [Getting started](https://tofutools.github.io/tclaude/) — install, setup,
  first session, and an orientation map.
- [Architecture](https://tofutools.github.io/tclaude/architecture/) — the
  mental model: sessions, conversations, agents, and the daemon.
- [Harnesses](https://tofutools.github.io/tclaude/harnesses/) — the four
  harnesses, per-harness setup, and the capability matrix.
- [Sessions](https://tofutools.github.io/tclaude/sessions/),
  [conversations](https://tofutools.github.io/tclaude/conversations/),
  [ask](https://tofutools.github.io/tclaude/ask/), and
  [worktrees](https://tofutools.github.io/tclaude/worktrees/) — everyday use.
- [Agents and groups](https://tofutools.github.io/tclaude/agents-and-groups/),
  [spawning and
  lifecycle](https://tofutools.github.io/tclaude/spawning-and-lifecycle/),
  and [teams at scale](https://tofutools.github.io/tclaude/teams-at-scale/) —
  fleet operations.
- [Permissions and
  audit](https://tofutools.github.io/tclaude/permissions-and-audit/) — the
  grant model, elevation, and the audit trail.
- [Dashboard](https://tofutools.github.io/tclaude/dashboard/) and
  [remote](https://tofutools.github.io/tclaude/remote/) — observing and
  reaching the fleet.
- [Sandboxing](https://tofutools.github.io/tclaude/sandboxing/),
  [network filtering](https://tofutools.github.io/tclaude/network-filtering/),
  and [proxies](https://tofutools.github.io/tclaude/proxies/) — confinement.
- [Processes](https://tofutools.github.io/tclaude/processes/) and
  [tasks](https://tofutools.github.io/tclaude/tasks/) — automation.
- [Adding a harness](https://tofutools.github.io/tclaude/adding-a-harness/) —
  contributor guide to the capability-based harness seam.

[License](LICENSE) (MIT)
