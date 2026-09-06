# Harness capabilities

Register installed providers explicitly with the daemon's `--harness` flag.
All four providers implement the same durable agent/execution/work workflow,
while their native capabilities and history precision differ.

| Provider | Native workload | Enforced sandbox selections | Exact history fork | Same-continuation guidance |
|---|---|---|---|---|
| Claude Code | Owned terminal | `workspace_write` | Unsupported; choose explicit fresh handoff | Supported native events |
| Codex CLI | Owned terminal | `read_only`, `workspace_write`, `unconfined` | Supported native turn boundary | Supported native events |
| OpenCode | Owned authenticated local server | `unconfined` | Head or before selected message, via isolated export/import | Unsupported |
| Copilot CLI | Owned terminal | `unconfined` | Unsupported; choose explicit fresh handoff | Unsupported |

Approval mode (`supervised` or `automatic`) is separate from OS confinement.
Unsupported selections are refused. The selected native version must support the
provider's required command/API contract; package-local tests and declarations
record the exercised dialects. A working directory alone is not a sandbox.

Discovery/read coverage and selectable history points are returned with each
catalog result. OpenCode's before-message boundary is exclusive. Claude head
resume cannot prove an immutable exact fork at release, so it is not advertised
as one. Copilot local history does not claim complete cloud-synced coverage.

Codex and Copilot share a durable provider-owned native home across executions.
Set `CODEX_HOME` or `COPILOT_HOME` to that exact private home when logging in.
The provider does not copy ambient credentials or delete shared authentication
on execution cleanup. See the [operating guide](backend-development.md) for
paths and setup commands.

Action credentials are per-execution protected resources. Native observations
and synchronous guidance use separate provider-owned resources. Ordinary native
primary-session correlation is not a defense against hostile same-UID processes.
Missing provenance remains unknown.

Usage reports expose source coverage, units and attribution. Missing counters or
cost are not invented. Native cost and estimated cost are separate, and a
conversation-cumulative observation is not falsely attributed to one execution.
