#!/usr/bin/env bash
# Run the local verification matrix:
#   - build, test, lint
#
# Flow tests in pkg/claude/agentd run under a bare `go test` —
# boundaries are mocked via interface assignment (clcommon.Default,
# agentd.Spawn), so no toolchain dependency or build tag is needed.
set -euo pipefail

go build -o /dev/null ./...
go test ./...
golangci-lint run ./...
