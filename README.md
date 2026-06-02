# tclaude

[![CI Status](https://github.com/tofutools/tclaude/actions/workflows/ci.yml/badge.svg)](https://github.com/tofutools/tclaude/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tofutools/tclaude)](https://goreportcard.com/report/github.com/tofutools/tclaude)
[![Docs](https://img.shields.io/badge/docs-tofutools.github.io%2Ftclaude-blue)](https://tofutools.github.io/tclaude/)

Claude Code CLI extensions and utilities — tmux-based session management, conversation search, usage tracking, a rich status bar, and an experimental multi-agent coordination layer.

## Install

Requires [Go](https://go.dev/dl/) 1.26+ and [tmux](https://github.com/tmux/tmux) (`tclaude setup` can install tmux for you on macOS).

```bash
go install github.com/tofutools/tclaude@latest

# Baseline setup + the two extras most users want
tclaude setup --install-agent-skills --install-default-agent-permissions
```

`tclaude setup` always installs the baseline integration (Claude Code hooks, the status bar, and the clickable-notification handler). The `--install-*` flags add optional extras on top:

| Flag | Adds | When you want it |
|------|------|------------------|
| `--install-agent-skills` | Bundled `agent-*` skills in `~/.claude/skills/` | Using the agent coordination features |
| `--install-default-agent-permissions` | Grants the `self.*` slugs those skills use | Using the agent coordination features |
| `--install-sandbox-hardening` | Locks down agentd state in the Claude Code sandbox | Only if you run agents inside the CC sandbox |
| `--install-all` | Everything above | You want it all |

Prefer not to build from source? Grab a prebuilt binary for your platform (Linux amd64/arm64, macOS arm64) from the [Releases page](https://github.com/tofutools/tclaude/releases), put it on your `PATH`, then run `tclaude setup`.

See the **[Installation guide](https://tofutools.github.io/tclaude/#installation)** for the full walkthrough.

**[Documentation](https://tofutools.github.io/tclaude/)** | **[License](LICENSE)**
