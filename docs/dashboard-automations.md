# Automations and Plugins

The **Automations** tab holds work that runs without you pressing anything:
scheduled jobs, standing orders, triggers and summary exports. **Plugins** is a
separate tab for shell steps you switch on and off.

## Automations

Subtabs are **All** (the default), **Exports**, **Cron jobs** and **Standing
orders**. **Triggers** appears with `features.triggers` on, and **Processes** with
`features.processes` on. Each is under
[Config › Experimental](dashboard-config.md#experimental).

Lists have a filter bar, sortable columns and page sizes from 25 to 200. Use **+
new cron job** and **+ new standing order** to add one. A badge on the tab counts
active exports.

### Exports

Conversation summary jobs, with a progress stepper and a download. Start one from
an agent's ⚙ menu with `summary…`. **dismiss** deletes the delivered file.

### Cron jobs

Send a message or start an agent on a schedule, as an interval of 30 seconds or
more, or a cron expression. Each job has run-now, a recent-runs log, edit,
duplicate, enable and delete.

**Spawn** jobs need `features.triggers`. They take a spawn profile, roles and a
concurrency rule: Forbid, Replace (stop the live worker first), or Allow with a
max number of live workers. With the flag off, existing spawn jobs still list,
but you can't edit or duplicate them.

The CLI equivalent is `tclaude agent cron`.

### Standing orders

Standing instructions tied to events: session start, a user prompt, or before or
after a tool, plus each harness's own hooks. You can add a condition on the
directory, prompt, tool name or tool input, and set delivery, cadence, minimum
interval and debounce. Each target shows whether it is supported, degraded or
unsupported.

### Triggers

Rules on PR opened, updated or merged, CI failed or succeeded, or an agent going
idle or awaiting input. A rule starts an agent or sends a templated message, and is
scoped globally or to a group. Each is armed, cooling or disabled.

### Processes

`/automations/processes` lists process templates and opens the graph editor. Runs
are not listed here; they live in [Groups](dashboard-groups.md#agents-and-processes).

The list has rename, describe, versions, delete, import and export, and **Edit with
agent** hands a template to a scribe agent. The editor has these node types: Task,
Decision, Parallel, Wait, Subprocess, Start and End. Other highlights:

- **Edit with agent** docks the scribe beside the canvas, as 💬 chat or ⌨ terminal.
  A version the scribe saves is reviewed on the canvas, change by change: A accept,
  R reject, J and K to walk.
- **Subprocess nodes (▣):** double-click to drill in, and Esc or Alt+↑ to leave. ⇪
  extracts a body as its own template, ⊞ expands it in place, ▣ wraps a selection,
  ⇲ unwraps one.
- Drop a palette item onto a connector to splice it in.

See [Processes](processes.md#in-the-dashboard) for the whole flow.

For automation workflows, see [Teams at scale](teams-at-scale.md).

## Plugins

The **Plugins** tab appears when you have a plugin. To keep it visible anyway, turn on
**Plugins tab** in [Config › Appearance](dashboard-config.md#appearance).

A plugin is a named list of shell steps (check, run, stop) with a status lamp:
active, not active, off, still active or unknown. Click a step's lamp to run or stop
it. **check** runs one plugin's checks, and **check all** runs everything. Checks also
repeat every minute. A warning badge on the tab counts problems. You can also edit
plugins and install them from the catalog.
