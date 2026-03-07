# tclaude

[![CI Status](https://github.com/tofutools/tclaude/actions/workflows/ci.yml/badge.svg)](https://github.com/tofutools/tclaude/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tofutools/tclaude)](https://goreportcard.com/report/github.com/tofutools/tclaude)
[![Docs](https://img.shields.io/badge/docs-tofutools.github.io%2Ftclaude-blue)](https://tofutools.github.io/tclaude/)

Claude Code CLI extensions and utilities.

Extracted from [GiGurra/tofu](https://github.com/GiGurra/tofu) into its own repo for independent development and easier installation.

## Installation

```bash
go install github.com/tofutools/tclaude/cmd/tclaude@latest
```

## Features

- **session** - Start and manage Claude Code sessions
- **conv** - Browse, search, resume, copy, move, and prune conversations
- **git** - Git utilities for Claude Code workflows
- **worktree** - Git worktree management for parallel Claude sessions
- **stats** - Session and conversation statistics
- **usage** - API usage tracking
- **setup** - Setup and configuration helpers
- **statusbar** - macOS status bar integration
- **web** - Web UI for session monitoring

## Usage

```bash
# Start a new Claude session (default command)
tclaude

# List conversations
tclaude conv list

# Resume a conversation
tclaude conv resume

# See all commands
tclaude --help
```

## Development

```bash
# Build
go build ./cmd/tclaude

# Run tests
go test ./...

# Run from source
go run ./cmd/tclaude --help
```

## Documentation

Full documentation available at **[tofutools.github.io/tclaude](https://tofutools.github.io/tclaude/)**.

## License

MIT
