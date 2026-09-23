# Run one agent turn

`tclaude run` starts a fresh, non-interactive harness process, waits for it to
finish, and writes its answer to standard output. It does not create a tmux
session or resume a previous `tclaude ask` thread. The harness may still keep
its own conversation history.

```bash
tclaude run "summarize this package"
tclaude run --harness codex --workdir /path/to/project "fix the failing test"
tclaude run --harness shell --workdir /path/to/project "go test ./..."
tclaude run --harness shell --timeout 2m "go test ./..."
```

The prompt or shell command is a positional argument. Quote it as one shell
argument when it contains spaces or shell syntax. `--harness` accepts `claude`
(the default), `codex`, `opencode`, `copilot`, and `shell`. The `shell` harness
runs the text through `$SHELL -c`, falling back to `/bin/sh`.

The child receives no standard input. Its exit status becomes `tclaude run`'s
exit status; timeout returns 124. `--timeout` takes a positive Go duration such
as `30s` or `10m`. On Linux and macOS, timeout stops the child process group.

## Sandbox

`--sandbox` selects a harness-native sandbox mode for Claude Code or Codex.
The accepted values follow `tclaude session new --help`. Without an explicit
mode, the harness's non-interactive adapter chooses its own default (Codex
uses read-only mode).

`--sandbox-impl tclaude-layer` runs under tclaude's OS sandbox. Use
`--sandbox-profile NAME` with it to apply a named profile; the global sandbox
profile is also resolved. The named profile and global profile are read from
tclaude's local database. The harness-native sandbox and tclaude layer are
mutually exclusive for this command. This one-shot layer path supports Claude,
Codex, and shell. It refuses profiles with Unix socket rules because those
rules require the managed session's socket materialization step.

```bash
tclaude run --harness shell --sandbox-impl tclaude-layer \
  --sandbox-profile build "go test ./..."
```

A tclaude layer launch refuses to start if its required sandbox engine or
profile cannot be resolved.
