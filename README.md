# tclaude

[![CI Status](https://github.com/tofutools/tclaude/actions/workflows/ci.yml/badge.svg)](https://github.com/tofutools/tclaude/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tofutools/tclaude)](https://goreportcard.com/report/github.com/tofutools/tclaude)
[![Docs](https://img.shields.io/badge/docs-tofutools.github.io%2Ftclaude-blue)](https://tofutools.github.io/tclaude/)

Coding-harness CLI extensions and utilities — tmux-based session management, conversation search, usage tracking, a rich status bar, and an experimental multi-agent coordination layer. Drives [Claude Code](https://claude.ai/code) (the default) and, experimentally, [OpenAI Codex CLI](https://developers.openai.com/codex/cli).

## Install

### Homebrew (macOS / Linux)

```bash
brew install tofutools/tap/tclaude
```

This pulls in `tmux` automatically and builds `tclaude` from source (the Go
toolchain is fetched as a build dependency). Then run setup as below.

### go install

Requires [Go](https://go.dev/dl/) 1.26+ and [tmux](https://github.com/tmux/tmux) (`tclaude setup` can install tmux for you on macOS).

```bash
go install github.com/tofutools/tclaude@latest

# Baseline setup + the two extras most users want
tclaude setup --install-agent-skills --install-default-agent-permissions
```

`tclaude setup` always installs the baseline integration (Claude Code hooks, the status bar, and the clickable-notification handler). The `--install-*` flags add optional extras on top:

| Flag | Adds | When you want it |
|------|------|------------------|
| `--install-agent-skills` | Bundled `agent-*` skills in `~/.claude/skills/`, `~/.agents/skills/`, and `$CODEX_HOME/skills` (default `~/.codex/skills`) | Using the agent coordination features |
| `--install-default-agent-permissions` | Grants the `self.*` slugs those skills use | Using the agent coordination features |
| `--install-sandbox-hardening` | Locks down agentd state in the Claude Code sandbox | Only if you run agents inside the CC sandbox |
| `--install-all` | Everything above | You want it all |

Prefer not to build from source? Grab a prebuilt binary for your platform (Linux amd64/arm64, macOS arm64) from the [Releases page](https://github.com/tofutools/tclaude/releases), put it on your `PATH`, then run `tclaude setup`.

See the **[Installation guide](https://tofutools.github.io/tclaude/#installation)** for the full walkthrough.

## Harnesses

tclaude is **harness-agnostic**: it drives [Claude Code](https://claude.ai/code) (the default) and, experimentally, [OpenAI Codex CLI](https://developers.openai.com/codex/cli). Every command that launches a session takes `--harness claude|codex` (default `claude`); the choice is persisted per conversation, so resume, listing, rename, stop, reincarnate, and clone all do the right thing automatically.

```bash
tclaude session new --harness codex            # start a Codex session
tclaude setup --harness codex                  # install Codex hooks (~/.codex/hooks.json)
tclaude agent spawn --group g --name w --harness codex   # spawn a Codex agent
```

| Capability | `claude` — Claude Code | `codex` — Codex CLI |
|---|---|---|
| Spawn / resume | ✅ | ✅ |
| Conversation list & search | ✅ | ✅ |
| Rename | ✅ in-pane `/rename` | ✅ out-of-band title store |
| Compact | ✅ `/compact` | ❌ not available |
| Graceful stop | ✅ `/exit` | ✅ `/quit` |
| Hooks / live status | ✅ `settings.json` | ✅ `~/.codex/hooks.json` (+ trust) |
| OS sandbox at spawn | ⚙️ via `settings.json` | ✅ `--sandbox` (secure default for agents) |
| Approval posture at spawn | ⚙️ via `settings.json` | ✅ `--ask-for-approval` (non-blocking default) |
| Guardian auto-review | ❌ | ⚙️ opt-in `--auto-review` (experimental) |
| Status bar | ✅ command statusline | ⚠️ curated built-in items |
| Dashboard | ✅ | ✅ |

Full guide: **[Harnesses](https://tofutools.github.io/tclaude/harnesses/)** · teaching tclaude a new harness: **[Adding a harness](https://tofutools.github.io/tclaude/adding-a-harness/)**.

**[Documentation](https://tofutools.github.io/tclaude/)** | **[License](LICENSE)**
