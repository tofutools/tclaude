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

You can pipe input into `tclaude run`:

```bash
cat report.txt | tclaude run "summarize this report"
cat data.json | tclaude run --harness shell "jq '.items | length'"
```

For agent harnesses, piped input is included in the prompt. It can be the
entire prompt, or is appended after the positional prompt under a separator.
The piped prompt is limited to 96 KiB because the harness adapters pass it as
a command argument; use a file path in the prompt for larger input.
For `--harness shell`, stdin is passed unchanged to the child command; a
shell command argument is still required. The child's exit status becomes `tclaude run`'s
exit status; timeout returns 124. `--timeout` takes a positive Go duration such
as `30s` or `10m` and includes reading piped input. On Linux and macOS,
timeout stops the child process group.

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

## Agent permission

Agents need the `agent.run` permission and a running tclaude daemon. The
permission supports `sandbox_profile` scoping, like `groups.members.spawn`:

```bash
tclaude agent permissions grant <agent> agent.run --scope 'sandbox_profile=build'
```

A grant scoped to `build` permits a tclaude-layer run using that profile. It
does not authorize native sandbox runs or a different profile. Without an
explicit `--sandbox-profile`, a tclaude-layer run is checked against the global
profile. An unscoped grant permits any supported run.

`agent.run` governs the `tclaude run` CLI path. The child executes with the
caller's OS privileges; the caller's own sandbox is the execution boundary.
The permission does not prevent an agent from invoking a harness or shell
binary directly.
