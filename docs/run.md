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

The child executes with the caller's OS privileges and inherits the caller's
existing sandbox; `tclaude run` never adds a tclaude sandbox layer of its own.
An agent inside tclaude's sandbox can still run any installed harness: every
tclaude-layer launch on Linux exposes the installed `claude`, `codex`,
`opencode` and `copilot` executables read-only, together with their npm
package roots and `node` when a harness is a Node.js launcher. Only the
executables are exposed; a harness still needs its own state directory (for
example `~/.codex` for Codex credentials) granted by the sandbox profile.

The dashboard's sandbox profile editor has a shortcut for that grant: under
**＋ add common rule → Harness state for `tclaude run`**, each preset inserts
write rows for one harness's login and session state, plus read-only rows for
that harness's settings, hooks, skills and similar surfaces. Keep the
read-only rows: those files run in your next unsandboxed session of that
harness. `~/.claude/sessions` is tclaude-protected, so the Claude Code preset
grants the other entries of `~/.claude` one by one.

## Resource limits

`--cgroup` runs the command in a fresh cgroup. `--cpu`, `--memory` and
`--pids` set limits on it and imply `--cgroup`:

```bash
tclaude run --harness shell --cpu 2 --memory 4GiB --pids 256 "go test ./..."
tclaude run --harness codex --memory 2GiB "fix the failing test"
```

`--cpu` is a number of cores (at least 0.01), `--memory` accepts quantities
such as `512MiB` or `4GB`, and `--pids` bounds processes plus threads.

The cgroup is created by `tclaude agentd`, which must be running with a
delegated cgroup v2 subtree (see [Sandboxing](sandboxing.md) for
`Delegate=` setup); this works from inside tclaude's sandbox, where
`/sys/fs/cgroup` is not visible. agentd moves the calling `tclaude run`
process into a new cgroup beside the caller's own, so the command and
everything it starts are counted there. The caller must itself run inside
agentd's delegated subtree, which is the case for agents agentd launched.
Limits never exceed the caller's existing ceilings: a looser request is
lowered, and an axis left unset inherits the caller's ceiling, with a note on
stderr. When `tclaude run` exits, agentd kills anything left in the cgroup and
removes it. Resource limits are Linux only.

Agents can use `tclaude run` without an agentd permission. Without the cgroup
flags it needs no running daemon.
