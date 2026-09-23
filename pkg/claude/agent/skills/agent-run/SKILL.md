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

## Sandbox inheritance

The child runs with your existing OS privileges and inherits your sandbox. `tclaude run` itself needs no agentd permission or running daemon. When you are already inside a tclaude layer, normally omit `--sandbox-impl`: another layer may fail because nested namespaces are unavailable. This does not loosen your current sandbox.

When you are outside a sandbox and need a tclaude layer, use `--sandbox-impl tclaude-layer --sandbox-profile NAME`. Profile resolution reads the local tclaude database. Without a named profile, the global profile applies.
