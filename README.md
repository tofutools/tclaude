# tclaude

[![CI Status](https://github.com/tofutools/tclaude/actions/workflows/ci.yml/badge.svg)](https://github.com/tofutools/tclaude/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tofutools/tclaude)](https://goreportcard.com/report/github.com/tofutools/tclaude)
[![Docs](https://img.shields.io/badge/docs-tofutools.github.io%2Ftclaude-blue)](https://tofutools.github.io/tclaude/)

`tclaude` is a tmux-based control layer for agentic coding CLIs. It gives
[Claude Code](https://claude.ai/code) and
[OpenAI Codex CLI](https://developers.openai.com/codex/cli) the same durable
session, conversation, and multi-agent operating environment.

Claude Code remains the default harness for compatibility; Codex is a
first-class supported harness. tclaude records the harness on each conversation,
so the conversation browser, `conv resume`, agent lifecycle, and dashboard
operations continue through the correct CLI automatically.

## What it adds

- **Persistent sessions** — run either harness in an isolated tmux server,
  detach and reattach, and see live working/idle/blocked status.
- **Conversation tools** — browse, text-search, and resume work from either
  harness; archive, copy, move, or delete Claude Code transcripts.
- **Fast terminal questions** — use `tclaude ask` for a resumable answer from
  your shell, including piped input such as `git diff`.
- **Agent coordination** — create mixed-harness groups, message peers, spawn and
  manage agents, schedule nudges, delegate permissions, and deploy reusable task
  forces through `agentd`.
- **Operations dashboard** — manage the fleet, messages, profiles, permissions,
  templates, audit history, usage, and experimental process-template authoring from a browser.
- **Developer utilities** — worktree helpers, semantic conversation search,
  notifications, subscription/activity reporting, and Claude-specific task and
  statusline integrations.

Harnesses do not expose identical primitives. Remote Control, the command-backed
status bar, and the sequential task runner are Claude Code-specific; Codex has
its own sandbox, approval, and statusline integration. See the
[capability matrix](https://tofutools.github.io/tclaude/harnesses/#capability-matrix)
for the precise differences.

## Install

tclaude supports Linux and macOS. WSL is treated as Linux; native Windows is not
supported. You also need the CLI for whichever harness you intend to run.

Choose one installation method:

**Homebrew (macOS / Linux)**

```bash
brew install tofutools/tap/tclaude
```

The formula installs tmux and builds tclaude from source.

**Go** — requires Go 1.26+ and tmux:

```bash
go install github.com/tofutools/tclaude@latest
```

**Prebuilt release** — download a Linux amd64/arm64 or macOS arm64 archive from
the [Releases page](https://github.com/tofutools/tclaude/releases), extract it,
and put `tclaude` on your `PATH`.

### Run setup

Installation is not complete until setup has installed the hooks and local
integration:

```bash
# Claude Code integration plus the agent skills/permissions most users want
tclaude setup --install-agent-skills --install-default-agent-permissions

# Install or repair Codex hooks explicitly
tclaude setup --harness codex

# Verify both sides at any time
tclaude setup --check
tclaude setup --check --harness codex
```

Plain `tclaude setup` configures Claude Code and, when Codex is found on `PATH`,
offers to configure Codex too. Setup is idempotent. The optional `--install-all`
flag also installs Claude sandbox hardening and the scripted-resume threshold
override; review those policies before enabling them.

Full walkthrough: [Installation and quick start](https://tofutools.github.io/tclaude/#installation).

## Quick start

```bash
# Claude Code is the default
tclaude session new

# Codex is an equal launch target
tclaude session new --harness codex

# Detach with Ctrl+B D, then browse or reattach later
tclaude session watch
tclaude conv watch -g

# Ask from the current shell; the thread continues per terminal + directory
tclaude ask "what should I know before changing this package?"
git diff | tclaude ask "spot correctness risks in this diff"
```

To operate multiple agents, keep the daemon running in a non-sandboxed terminal:

```bash
tclaude agentd serve
```

Then open `tclaude agent dashboard` or use `tclaude agent` from another shell.
Groups can freely mix Claude Code and Codex agents.

## Documentation

- [Getting started](https://tofutools.github.io/tclaude/) — installation,
  setup, first sessions, and a map of the CLI.
- [Harnesses](https://tofutools.github.io/tclaude/harnesses/) — choosing Claude
  or Codex, setup, capabilities, sandboxing, and approvals.
- [Sessions](https://tofutools.github.io/tclaude/sessions/) and
  [conversations](https://tofutools.github.io/tclaude/conversations/) — the
  everyday tmux and history workflows.
- [Ask](https://tofutools.github.io/tclaude/ask/) — shell-native questions,
  piped input, thread continuity, and harness selection.
- [Agent coordination](https://tofutools.github.io/tclaude/agent/) and
  [dashboard](https://tofutools.github.io/tclaude/dashboard/) — groups,
  messaging, lifecycle, profiles, permissions, templates, and task forces.
- [Processes](https://tofutools.github.io/tclaude/processes/) — the opt-in
  process-template library and editor; runtime execution is temporarily unavailable.
- [Remote control](https://tofutools.github.io/tclaude/remote-control/) and
  [remote access](https://tofutools.github.io/tclaude/remote-access/) — two
  distinct ways to operate away from the host terminal.
- [Adding a harness](https://tofutools.github.io/tclaude/adding-a-harness/) —
  contributor guide to the capability-based harness seam.

[Full documentation](https://tofutools.github.io/tclaude/) · [License](LICENSE)
