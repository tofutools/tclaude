# tclaude

[![CI Status](https://github.com/tofutools/tclaude/actions/workflows/ci.yml/badge.svg)](https://github.com/tofutools/tclaude/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tofutools/tclaude)](https://goreportcard.com/report/github.com/tofutools/tclaude)
[![Docs](https://img.shields.io/badge/docs-tofutools.github.io%2Ftclaude-blue)](https://tofutools.github.io/tclaude/)

`tclaude` is a self-hosted agentic dev environment: a Go CLI plus daemon that
wraps vendor coding CLIs ("harnesses") in tmux and adds the operations layer
you need to run many agents seriously. Durable sessions, searchable history,
a fleet dashboard, agent-to-agent mail, teams with identity and permissions,
sandboxing, and automation, all running on your own machine.

It wraps five harnesses — [Claude Code](https://claude.ai/code),
[OpenAI Codex CLI](https://developers.openai.com/codex/cli),
[OpenCode](https://opencode.ai),
[GitHub Copilot CLI](https://github.com/features/copilot/cli), and
[Google Gemini CLI](https://github.com/google-gemini/gemini-cli) (beta) — behind one
workflow, so a team can mix vendors and switch as models improve. tclaude is
MIT-licensed; the models are the only external part.

![The tclaude operations dashboard watching a mixed-harness fleet](docs/assets/dashboard-groups.png)

## What it adds

- **Fleet dashboard**: start, watch and steer every agent from the browser:
  status, cost, context, branch, PR and CI, down to its live terminal.
- **Durable sessions and history**: agents keep running when you look away;
  resume any harness, and search every conversation across all of them.
- **Agent mail and teams**: groups of agents that message each other,
  deployable from reusable templates.
- **Identity, permissions and audit**: kernel-verified callers, scoped grants,
  and a trail of what was allowed and denied.
- **Sandboxing and network filtering**: one profile, enforced through
  bubblewrap, Seatbelt or the harness's own sandbox.
- **Automation**: scheduled nudges, workflows, credential proxies, and
  shell-native `tclaude ask`.
- **Across machines**: link nodes through a self-hosted
  [hub](https://tofutools.github.io/tclaude/federation/); move, teleport or
  snapshot agents, and resume them on another harness or model with the
  [Portable Agent Format](https://tofutools.github.io/tclaude/paf/).

Harnesses don't all expose the same primitives; tclaude says so instead of
pretending. See the
[capability matrix](https://tofutools.github.io/tclaude/harnesses/).

## Install

Linux and macOS (WSL counts as Linux). You also need tmux and at least one
harness CLI.

```bash
brew install tofutools/tap/tclaude                 # Homebrew, or:
go install github.com/tofutools/tclaude@latest     # Go 1.26+
```

Prebuilt archives are on the
[Releases page](https://github.com/tofutools/tclaude/releases). Then run setup
once, which installs the hooks tclaude needs:

```bash
tclaude setup --install-agent-skills --install-default-agent-permissions
```

[Installation](https://tofutools.github.io/tclaude/#installation) covers
every option, including the standalone `tclaude-agentd` daemon.

## Quick start

```bash
tclaude agentd serve          # start the daemon; keep it running
                              # (copy the TCLAUDE_HUMAN_TOKEN export it prints)
tclaude agent dashboard       # open the dashboard in your browser
```

In the dashboard, **+ new group** sets a working directory, and the group's
spawn button starts an agent with a first task. Watch its status, open its
terminal with **focus**, and read the team's mail under **Messages**.

Prefer the shell? Bare `tclaude` starts your harness in a tmux session, and
`tclaude ask "…"` answers a quick question.

**New here? Follow the [Learn path](https://tofutools.github.io/tclaude/learn/):**
short hands-on tutorials from installing to running your first agent team.

## Documentation

- [Learn](https://tofutools.github.io/tclaude/learn/): tutorials for new users.
- [Architecture](https://tofutools.github.io/tclaude/architecture/): how
  sessions, conversations, agents and the daemon fit together.
- [Harnesses](https://tofutools.github.io/tclaude/harnesses/): the five
  harnesses and what each supports.
- [Full documentation](https://tofutools.github.io/tclaude/): everything else,
  from fleet operations to sandboxing and federation.

[License](LICENSE) (MIT)
