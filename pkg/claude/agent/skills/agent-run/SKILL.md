---
name: agent-run
description: Run a fresh, non-interactive Claude, Codex, OpenCode, Copilot, or shell turn with `tclaude run`. Use when an agent needs a one-shot command with optional piped input, timeout, or a named tclaude sandbox profile.
---

# One-shot runs

`tclaude run` starts a fresh foreground process and prints its output. It does not create or resume a tclaude conversation. The default harness is Claude; select another with `--harness codex|opencode|copilot|shell`.

```bash
tclaude run --harness codex "summarize this package"
cat report.txt | tclaude run "summarize the report"
cat data.json | tclaude run --harness shell "jq '.items | length'"
tclaude run --harness shell --timeout 2m "go test ./..."
```

For agent harnesses, piped stdin becomes prompt text. If there is also a positional prompt, the input is appended under a `piped input (stdin)` separator. For `--harness shell`, stdin is passed unchanged to the shell command. A prompt can come entirely from stdin for agent harnesses; shell still needs a command argument.

The piped agent prompt is limited to 96 KiB because it becomes a command argument. For larger input, point the harness at a file instead.

`--workdir` selects the child's directory. The child exit status is returned; a timeout exits 124. Consult `tclaude run --help` and `docs/run.md` for sandbox choices and harness limits.

## Permission

An agent needs an `agent.run` grant. The daemon must be running to authorize the call. An operator can grant access to a particular tclaude sandbox profile:

```bash
tclaude agent permissions grant <agent> agent.run --scope 'sandbox_profile=build'
tclaude run --harness shell --sandbox-impl tclaude-layer --sandbox-profile build "go test ./..."
```

The grant is checked against the resolved profile. A grant scoped to `build` does not authorize a native sandbox run or a different profile. With `--sandbox-impl tclaude-layer` and no explicit `--sandbox-profile`, the global profile is used. An unscoped `agent.run` grant permits any supported run. If authorization is denied, ask the operator for a suitable grant rather than trying another route.

This permission gates the `tclaude run` CLI path; the child runs with your existing OS privileges. Your own sandbox remains the execution boundary.
