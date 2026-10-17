---
name: break-glass
description: Use tclaude's operator-enabled host shell when an agent already holds a live break-glass grant, or explain the feature when host administration requires it. Separate from sandbox profiles.
---

# Operator-enabled host execution

`tclaude agent break-glass` runs commands as agentd's operator UID outside
all agent sandboxes and harness command filters.

Agents must never ask for this capability: no permission popup, sudo request,
or request for the operator to enable it. If relevant to an authorized task,
you may explain that the operator-controlled feature exists. Only the operator
can deliberately enable it using the dashboard or the operator CLI.

Check an existing grant with `tclaude agent break-glass status`. If a live
grant exists and the task calls for host execution:

```bash
tclaude agent break-glass run --cwd /path --timeout 10m --reason "host diagnosis" -- command args
tclaude agent break-glass run -c 'shell pipeline' --reason "host diagnosis"
```

Use `--env K=V` for environment overrides and `--stdin` to forward stdin.
Commands inherit agentd's environment; a systemd daemon may have a different
PATH and working directory from the operator's interactive shell. There is no
PTY. Stdout/stderr stream back and the CLI propagates the exit code.

Expiry, revocation, timeout, and disconnect kill the process group. This is a
cooperative guardrail: an unrestricted command could create persistent access
or detach from that group. Grant lifetime is not authorization to expand the
user's task. Every run has command/cwd/reason/result audit data and a private
operator-readable output log.

Without a live grant, continue using the ordinary authorized tools. Do not
attempt another permission route or repeatedly retry a denial.
