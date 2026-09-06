# tclaude

`tclaude` runs agentic work across Claude Code, Codex CLI, OpenCode and Copilot
CLI. A Go daemon owns durable agents, conversations, executions, correspondence,
workspaces and work outcomes. The CLI and local browser use the same authenticated
application API.

An agent is a continuing identity; an execution is one running attempt. Stopping
an execution does not erase its agent or history. Work requests pin their inputs
and record evidence and attributed decisions. After a restart, the daemon
reconciles retained resources without blindly repeating uncertain effects.

## Build and start

Linux and macOS are supported. Install the native harnesses you want to use and
tmux for terminal workloads. To build the two binaries:

```sh
go build -o /tmp/tclaude .
go build -o /tmp/tclaude-agentd ./cmd/tclaude-agentd
/tmp/tclaude-agentd --state-dir /tmp/my-tclaude --init
/tmp/tclaude-agentd serve --state-dir /tmp/my-tclaude
```

The directory must be new. Without `--harness`, the daemon supports offline
catalog and correspondence work. Select installed providers explicitly, for
example `--harness claude,codex,opencode,copilot`. Configure native login against
the provider's documented private home before running a workload.

In another terminal:

```sh
/tmp/tclaude --operator-state /tmp/my-tclaude snapshot
/tmp/tclaude dashboard --state-dir /tmp/my-tclaude
```

The dashboard prints a short-lived, single-use local login link. Operator tokens
stay on disk. Executions receive their own renewable protected credentials and
only their granted authority; missing credentials never become operator access.

## Workflows

- Configure offline agents and reusable configuration profiles; organize groups
  and deploy pinned team definitions.
- Send threaded messages with To/CC recipients and attachments, including offline
  recipients and the operator. Inbox delivery and native notice outcomes are
  separate.
- Find prior conversations, read supported history points, continue work, or
  explicitly choose a fresh handoff when exact fork is unavailable.
- Create and retain owned Git checkouts, run standalone shells, and attach to
  supported terminals without changing workload ownership.
- Run processes with agent, program and human stages; schedule attributed
  automation; inspect exact evidence, decisions and uncertain outcomes.
- Inspect scoped authority, source-attributed usage and durable activity.

Harness capabilities differ. Confinement, native callbacks and history precision
are reported explicitly; unsupported behavior is refused. No quota or successful
outcome is inferred from missing native evidence.

See the [operating guide](docs/backend-development.md),
[architecture](docs/architecture.md), and [contributing guide](CONTRIBUTING.md).
Legacy state requires an explicit offline snapshot and import; the daemon does
not open the old database or adopt old live handles.
