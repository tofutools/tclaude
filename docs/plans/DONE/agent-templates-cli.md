# `tclaude agent templates` — CLI parity for group templates

Shipped 2026-05.

Group templates ([`group-templates`](group-templates.md), PR #143)
landed dashboard-only: the daemon grew the full `/v1/templates`
endpoint surface, but there was no `tclaude agent templates …` CLI
client. Every other agentd surface — `groups`, `cron`, `permissions`,
`sudo` — has a CLI subcommand tree; templates was the odd one out.

This slice ships that CLI tree: a thin client over the existing
`/v1/templates` endpoints, no new daemon work.

## What shipped

### `tclaude agent templates` subcommand tree

`pkg/claude/agent/templates.go`, registered in `agent.go`:

| Command | Endpoint | Notes |
|---|---|---|
| `ls` | `GET /v1/templates` | table: name, agent count, owner(s), descr |
| `show <name> [--json]` | `GET /v1/templates/{name}` | human view; `--json` emits the wire shape |
| `create --file <path>` | `POST /v1/templates` | reads template JSON from a file / stdin |
| `edit <name> --file <path>` | `PATCH /v1/templates/{name}` | full replace; body `name` may rename |
| `rm <name>` | `DELETE /v1/templates/{name}` | deletes the blueprint only |
| `instantiate <name> --group <g> [--task / --task-file] [--cwd] [--descr]` | `POST /v1/templates/{name}/instantiate` | creates a group, spawns the team |
| `from-group <group> <template-name>` | `POST /v1/templates/from-group` | snapshots a live group |

Design choices:

- **`create` / `edit` take JSON via `--file`**, not flags. A template is
  a nested structure (agents, each with a multi-line task brief), so a
  file is the honest scriptable primitive — a comma-spec flag could not
  express it. `show <name> --json` emits exactly that shape, so
  `show … --json > t.json` → edit → `edit … --file t.json` is the
  edit loop. `--file -` reads stdin.
- **`instantiate` is fully flag-driven** — its inputs are flat
  (`group_name`, `task`, `cwd`, `descr`). The multi-line task can come
  from `--task` or `--task-file` (mutually exclusive, via the shared
  `resolveBodyInput` helper that `cron`/`spawn` use).
- A partial (or total) spawn failure on `instantiate` is a **non-zero
  exit** (`rcIOFailure`) so scripts notice — the per-agent errors are
  printed; the group and any spawned agents still exist for the human
  to finish by hand.

### `DaemonOpts.Timeout`

`pkg/claude/agent/client.go` gained a `Timeout time.Duration` field on
`DaemonOpts`; when set, `daemonReq` uses a per-request client with that
timeout. `instantiate` passes `5m` — instantiation spawns the whole
team sequentially (each spawn polls for a conv-id), which runs well
past the default 10s client timeout. General-purpose plumbing, not
templates-specific.

### Permissions

No new slugs. The daemon already gates `/v1/templates*` —
`templates.manage` for CRUD + `from-group`, `templates.instantiate`
for `instantiate` (both effectively human-only by default). The CLI
inherits that enforcement; a refused call surfaces as the daemon's
error envelope mapped to the matching `rc*` exit code.

### Shell completion

`completeTemplateNames` (`completion.go`) — every template name,
prefix-filtered — wired onto `instantiate`'s template-name argument;
`from-group`'s group argument reuses `completeGroupNames`.

## Files

- `pkg/claude/agent/templates.go` — the subcommand tree + `runTemplates*`
  handlers.
- `pkg/claude/agent/agent.go` — `templatesCmd()` registered in the
  `tclaude agent` tree.
- `pkg/claude/agent/client.go` — `DaemonOpts.Timeout`.
- `pkg/claude/agent/completion.go` — `completeTemplateNames`.
- `pkg/claude/agent/templates_test.go` — unit tests.

## Tests

`pkg/claude/agent/templates_test.go` — the thin-client layer: a
`stubDaemon` helper swaps `DaemonAvailableImpl` / `DaemonRequestImpl`
(the existing test seams) so each `runTemplates*` is exercised without
a live daemon. Covers: `ls` table formatting + empty state; `show`
human view, `--json` round-trip, empty-name + 404→`rcNotFound`;
`create` body marshalling, stdin, missing-`--file`, malformed JSON;
`edit` PATCH path + rename notice; `rm` DELETE path; `instantiate`
happy path (body fields + the `Timeout` opt), partial-failure non-zero
exit, missing `--group`, `--task`/`--task-file` mutual exclusion;
`from-group` body + missing args.

The `/v1/templates` endpoints themselves are already flow-tested from
PR #143 (`agentd/templates_flow_test.go`), so the CLI work needed only
its thin-client wiring + output formatting covered — which the unit
tests do.

## Notes

- Doc placed straight in `DONE/` — the feature ships in one PR, so the
  `TODO/`→`DONE/` move would be empty churn within a single branch
  (same handling as `group-templates.md` in #143).
