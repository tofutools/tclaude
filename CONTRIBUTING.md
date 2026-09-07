# Contributing

Build and validate both product entrypoints:

```sh
go build ./...
go test -race ./... -count=1
golangci-lint run ./...
```

The binaries are the module-root `tclaude` and `cmd/tclaude-agentd`. To install
both when explicitly intended, use `go install . ./cmd/...`. The nested
`tclaude agentd serve` command uses the same daemon command as the standalone
binary. Supported platforms are Linux and macOS.

Tests exercise production application, SQLite and public transport paths using
disposable state. Swap external native process boundaries when necessary, retain
real ownership and admission checks, and label fixture-backed native evidence
accurately. Never use private live state for tests. The root acceptance test
builds both binaries and verifies actual daemon shutdown and restart.

Install tmux for terminal/host tests. With Linux Chrome installed, run the offline
browser acceptance suite:

```sh
TCLAUDE_BROWSER_SMOKE=1 go test -race ./internal/product/browser -count=1 -timeout 10m
```

CI runs the full race suite on Linux and macOS and browser acceptance on Linux.
Tests requiring authenticated native turns are not silently substituted for this
coverage. Native capability limitations must remain explicit.

Keep product behavior in the application layer, native mechanisms in cohesive
providers or host implementations, and client projections in transport/product.
See [architecture](docs/architecture.md). Plans and review handoffs belong in the
external work tracker, not committed roadmap documents.

Every PR begins with Background / Purpose and records appropriate validation and
an independent cold review. Commit before review, fix valid findings, and bind
approval to the final published head. Never include private session links or
credentials in committed files or PR text.
