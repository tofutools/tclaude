---
name: agent-run
description: Run a fresh, non-interactive Claude, Codex, OpenCode, Copilot, or shell turn with `tclaude run`. Use when an agent needs a one-shot command with optional piped input, timeout, or CPU/memory/PID limits in its own cgroup.
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

## Sandbox and resource limits

The child runs with your existing OS privileges and inherits your sandbox. `tclaude run` needs no agentd permission. Inside tclaude's sandbox the installed harness executables are available, so a Claude agent can run `--harness codex` and vice versa; the other harness still needs its state directory (credentials) granted by your sandbox profile.

To bound a run, pass `--cpu CORES`, `--memory QUANTITY` and/or `--pids N` (or just `--cgroup`):

```bash
tclaude run --harness shell --cpu 2 --memory 4GiB --pids 256 "go test ./..."
```

tclaude agentd creates the cgroup and moves the run into it, so the daemon must be running with cgroup delegation (Linux only). Limits are clamped to your own ceilings; unset axes inherit them. The cgroup and anything left in it are removed when `tclaude run` exits.
