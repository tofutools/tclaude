# Operating tclaude

`tclaude-agentd` composes the replacement application, SQLite store, HTTP
API, and harness providers. Its
state directory must be new and explicitly initialized; it does not import the
existing database or adopt its live agents.

Build without installing or restarting the existing daemon:

```bash
go build -o /tmp/tclaude-agentd ./cmd/tclaude-agentd
/tmp/tclaude-agentd --state-dir /tmp/backend-example --init
/tmp/tclaude-agentd --state-dir /tmp/backend-example
```

Omitting `--harness` provides an offline catalog. Register installed providers
with `--harness claude,opencode`. Registration resolves their executables; it
does not launch a workload. The daemon targets Linux and macOS.

Initialization creates a private directory and `operator.token`. The API listens
on `api.sock` inside that directory. Treat the token as an operator credential;
request bodies cannot choose their authenticated principal. Keep directory paths
short enough for the operating system's Unix socket limit.

For example, create an agent without starting a harness:

```bash
curl --unix-socket /tmp/backend-example/api.sock \
  -H "Authorization: Bearer $(cat /tmp/backend-example/operator.token)" \
  -H 'Content-Type: application/json' \
  --data '{"id":"worker","name":"Worker","desired":{"Harness":"claude","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"workspace_write"}}' \
  http://backend/v2/agents
```

Read the durable catalog and operation states:

```bash
curl --unix-socket /tmp/backend-example/api.sock \
  -H "Authorization: Bearer $(cat /tmp/backend-example/operator.token)" \
  http://backend/v2/snapshot
```

The API currently exposes these routes:

| Method | Route | Purpose |
|---|---|---|
| POST | `/v2/agents` | Create an offline agent |
| PUT | `/v2/agents/{id}` | Update desired configuration using an expected revision |
| POST | `/v2/groups` | Create a group |
| GET | `/v2/snapshot` | Read durable state without probing workloads |
| POST | `/v2/launch` | Start a fresh execution for an agent or standalone target |
| POST | `/v2/resume` | Continue a selected logical conversation in a new execution |
| POST | `/v2/observe` | Refresh an execution's observed state |
| POST | `/v2/interact` | Send text to the selected execution |
| GET | `/v2/attach` | Open a WebSocket terminal stream |
| POST | `/v2/stop` | Stop the selected execution |
| POST | `/v2/context` | Request a supported in-place context change |
| POST | `/v2/messages` | Accept a durable message |
| POST | `/v2/messages/{id}/read` | Mark a recipient's message read |
| POST | `/v2/recover` | Reconcile stored executions with provider evidence |
| GET | `/v2/identity` | Read the authenticated execution's agent and context |
| GET | `/v2/inbox` | Read the authenticated agent's inbox (`unread_only=true` filters it) |
| POST | `/v2/inbox/{id}/read` | Mark a message read as the authenticated recipient |
| POST | `/v2/status` | Read status within an authorized resource scope |
| POST | `/v2/authority/explain` | Explain the current decision for an action and resource |
| GET | `/v2/authority` | List operator-managed grants, roles, and assignments |
| PUT / DELETE | `/v2/authority/grants/{id}` | Change a grant using its expected revision |
| PUT | `/v2/authority/roles/{id}` | Configure a named role |
| PUT / DELETE | `/v2/authority/roles/{id}/assignments` | Change a scoped role assignment |
| PUT | `/v2/groups/{id}/owner` | Set a visible owner role with configuration bounds |
| GET | `/v2/executions/{id}/access` | Read nonsecret execution-access state and revision |
| POST | `/v2/executions/{id}/access/revoke` | Revoke execution access using its expected revision |

Effect requests carry a stable `request_id`. Reuse it when checking/retrying the
same request; do not invent a new request ID to replay an uncertain effect.
Operation result codes and states describe acceptance and outcome separately.
Native recovery evidence and raw provider diagnostics are not exposed by the API.

An attachment uses `execution_id` and `request_id` query parameters plus the same
Authorization header. Text and binary WebSocket frames carry terminal bytes.
Closing the attachment disconnects that view without stopping the workload.
Stopping the daemon also leaves workloads for later recovery.

Current provider support is explicit: Claude uses a terminal workload and reports
unresolved context when its private native observation channel cannot establish
continuity. Context changes require correlated native evidence; dispatch alone
does not confirm a reset. OpenCode uses an independent server and currently requires an
explicit `unconfined` sandbox selection. Native approval rules do not provide OS
confinement; constrained OpenCode launches are refused. Legacy state is imported only from an explicit offline snapshot; starting the daemon never discovers or migrates it.

Focused verification:

```bash
go test ./internal/backend/... -race -count=1
go build ./...
```

Shutdown stops accepting requests and disconnects attachment views, then waits
for admitted request handlers to settle before closing storage or releasing the
state-directory lock. A slow admitted workflow can therefore delay shutdown.
Snapshots include public conversation and agent-conversation association
revisions; use the current association revision for context/resume requests.

## Execution agent client

Build the thin client without installing it:

```bash
go build -o /tmp/tclaude .
```

Credential-capable providers deliver a protected action credential file to each
execution. They expose its path as `TCLAUDE_BACKEND_CREDENTIAL_FILE` and the API
socket path as `TCLAUDE_BACKEND_SOCKET`. The client reads these paths by default;
`--credential-file` and `--socket` allow explicit paths. It has no automatic
operator-token fallback and never opens the database.

Inside that execution, the commands are:

```bash
/tmp/tclaude whoami
/tmp/tclaude inbox --unread
/tmp/tclaude status
/tmp/tclaude send 'Please review the change' --to AGENT_ID --request-id review-request-1
/tmp/tclaude read MESSAGE_ID --request-id read-message-1
/tmp/tclaude interact EXECUTION_ID 'Continue the task' --request-id continue-1
/tmp/tclaude stop EXECUTION_ID --request-id stop-1
```

`launch` requires an agent ID and `--expected-revision`. `resume` additionally
requires a logical conversation ID and `--expected-association-revision`.
`context EXECUTION_ID` requires `--intent clear|reset`,
`--expected-conversation`, and `--expected-association-revision`. All three
commands require `--request-id`. Scoped status includes the selected agents'
current `associations` and each execution's `context_readiness`, so an authorized
manager can obtain a peer's conversation and association revision without a global
snapshot. Read current revisions from identity/status or an authorized snapshot;
stale selections are refused. Available operations are
subject to current scoped authority and provider capabilities.

The application issues HTTP-safe credentials, and the host atomically replaces
their protected resource during renewal. The client rereads the file for every
call. No bearer can renew itself; the server schedules an application-owned
renewal sweep and joins it before closing storage. Restart suspends access until
exact recovery, and revocation or replacement invalidates old credentials.
Native observation resources are separate from action credentials: action
authentication alone cannot submit primary-context evidence.

Authority administration requires the operator credential. Configuration-bearing
grants and owner assignments carry explicit harness, model, working-directory,
approval, and sandbox bounds. An owner role is a visible scoped assignment, not
an operator bypass. The API exposes access state/revision without credential
material or provider recovery evidence.

The daemon composition accepts `--harness claude,codex,opencode,copilot`.
Only selected providers are registered, and their native executables must be
available on `PATH`. Omitting `--harness` still permits offline catalog use.

Codex and Copilot use a durable native home under the selected private state:
`<state-dir>/codex/native-home` and `<state-dir>/copilot/native-home`. These homes
hold native login and history state independently of execution terminals,
observation resources and backend-agent credentials. The backend does not import
or copy credentials from the user's existing native home. For native account
login, initialize the private directory first, then explicitly log in using
that provider's home. For example, with an absolute `state_dir` already chosen:

```bash
mkdir -p -m 700 "$state_dir/codex/native-home" "$state_dir/copilot/native-home"
CODEX_HOME="$state_dir/codex/native-home" codex login
COPILOT_HOME="$state_dir/copilot/native-home" copilot login
```

Native login is an operator setup step; these commands are not run by backend
launch or by automated tests. Native token environment authentication can also be
used where supported by the installed harness. Stopping an execution does not
remove the shared native home or log the operator out. A new replacement state
may require native login again. Do not delete its native home as execution
cleanup.

## History, workspaces, and bounded work

Enable host operations explicitly when starting the daemon:

```bash
/tmp/tclaude-agentd --state-dir "$state_dir" --harness opencode \
  --workspaces --shell /bin/sh \
  --history-source opencode:previous=/absolute/native/xdg-root
```

`--workspaces` enables Git checkout operations. `--shell` selects the executable
for standalone shells; the current host supports only explicit `unconfined`
policy. A shell has an Execution and workspace use, with no fabricated Agent or
Conversation. Existing attach, observe and stop operations apply to it.

History requests select configured source names, never native filesystem roots.
OpenCode, Codex and Copilot expose `owned` for their provider-owned history.
Claude requires an explicit `--history-source claude:NAME=/absolute/projects-root`.
Use `--claude-config-dir /absolute/config-root` to select an existing persistent
Claude configuration (including its login and history) for launches. This is
separate from the read-only history-source catalog. Selected host-sandbox
launches expose that exact configuration root, their own credential/callback
resources, and the API endpoint directory; they do not mount the backend state
parent. Without an explicit root, selected launches use
`STATE/claude/native-home`. A continuation retains its recorded configuration
root rather than switching it when the host-sandbox choice changes.

Selected native Claude, Codex, and Copilot launches share a configuration floor:
known settings files and executable-configuration directories are readonly by
default (`HarnessConfig: read` is equivalent); login and history state remain
writable. `HarnessConfig: write` explicitly opts out. Missing configuration files
are not fabricated, and symlinked catalog entries retain the legacy warning and
are skipped. Such entries and absent files therefore are not covered by this
floor. Existing files and created empty configuration directories are retained
by identity in each prepared launch. Changing a pinned file before release
requires a fresh preparation.

The native server control relay uses a retained Unix listener and proves both
its process owner and the server's TCP listener before forwarding credentials.
Linux runs that relay and server inside the same private network namespace.
On macOS the retained TCP control port uses Seatbelt's `localhost` selector,
which also matches other host-local addresses at that port. This is a partial
network-isolation mechanism, matching the legacy platform limitation, rather
than a guarantee limited to the numeric loopback address. Other TCP ports and
UDP have no control exception.

Selected launches support a constructed sparse root or inherited read-only host
root. Automatic/inherited choice yields to the constructed root when the network
policy requires isolation; an explicit separate root always stays separate.
Inherited roots retain protected-state exclusions and explicit writable grants.
Linux still replaces `/tmp`, `/dev` and `/proc` with the sandbox scratch/device/
process views; inherited root does not expose host temporary files.

Selected launches apply directory deny rules with explicit narrower
read/write grants preserved. Linux hides a denied directory behind an empty,
read-only mount; macOS denies access through Seatbelt. Linux also supports
size-bounded writable tmpfs scratch mounts and explicit nested binds. Scratch
contents do not persist to the underlying host directory. A scratch mount cannot
cover protected state or launch-required paths. macOS refuses tmpfs because
Seatbelt provides no mount namespace; it does not substitute a host directory.

Generated sandbox directories are stable agent-owned caches. Their paths survive
later executions and missing named directories are recreated at the same paths;
a different agent receives independent paths. Standalone shells use their exact
execution identity. Generated bindings take precedence over authored launch
environment overrides and are retained in the prepared launch. Symlinked cache
bindings are refused. Aborting a preparation does not remove retained caches.

By default the agent's own cache parent is writable, permitting it to delete and
recreate named children. `--agent-dirs-mount-parent=false` instead grants each
named directory individually, so its contents are writable but its parent is not.
Neither choice grants the shared cache container or backend private state.

Selected launches execute ordered pre-launch blocks in Bash inside the admitted
OS boundary, before the native command. Blocks share shell functions and
exported environment. A failing command/pipeline or an unset declared export
stops launch with status 126 and the block name; an intentionally empty export
is valid. The retained script is granted as one read-only file, without exposing
its private parent directory, and is kept out of argv to support large blocks.
The native command and arguments remain literal even if setup changes positional
parameters. Policy save, preview and preparation never execute setup scripts.

OpenCode's selected host-sandbox launch preserves its native XDG configuration
read-only and seeds independent copies of `auth.json` and `mcp-auth.json` into
fresh session data. Continuation retains its own login, including logout and
credential refresh. `--opencode-config-dir` and `--opencode-data-dir` select the
native **app directories**, otherwise the corresponding XDG base plus `opencode`
is used (with `~/.config` and `~/.local/share` fallbacks). No unrelated native data
is copied. The native `.gitignore` bootstrap is created only when absent;
existing authored content is preserved.

Linux mounts the config into the session's private config directory. macOS uses
the original config path because it cannot remap directories; native descendants
therefore see that config base in `XDG_CONFIG_HOME`. Neither platform grants its
parent directory. Confined launches receive explicit environment values, not the
daemon's whole environment. Repeat `--opencode-env NAME` to pass selected native
API keys or other ordinary variables by name, or author literal launch environment
values. Loader, native-state and daemon-control variables remain reserved.

OpenCode also accepts an explicit native XDG root. Codex and Copilot currently
support their owned native homes only. Each refresh reports coverage; partial or
unreadable history is not presented as a complete empty result.

Using the client with an explicit authorized credential/socket:

```bash
/tmp/tclaude history refresh previous --harness opencode
/tmp/tclaude history search --query 'earlier work'
/tmp/tclaude history read CONVERSATION_ID --revision REVISION
```

Read results expose selectable points and their revisions. Point precision is a
provider capability: an OpenCode `before_message` point excludes the selected
message; it is not an inclusive message checkpoint. Claude exact fork is currently
unsupported. Explicit `fresh_handoff` starts a new context from supplied text and
is never reported as an exact native fork.

Create a checkout using `workspace create WORKSPACE_ID --request-id REQUEST_ID
--intent-file intent.json`. The intent contains `Repository`, `IntendedPath`,
`BaseRevision`, and `Branch`. Registering an existing path is a separate
`workspace register` operation and does not grant destructive ownership.
`workspace inspect ID` returns the revision needed by subsequent commands.
Use its observed actual path for the worker working directory; it can differ
from the requested spelling when a parent directory is a symlink.

A work specification pins the existing worker agent and workspace revisions,
desired configuration, source mode, brief and outcome policy. For example:

```json
{
  "SourceMode": "fresh_handoff",
  "FreshHandoff": "The prior investigation established ...",
  "WorkspaceID": "workspace-example",
  "WorkspaceRevision": 1,
  "WorkerAgentID": "worker-example",
  "WorkerAgentRevision": 1,
  "WorkerDesired": {
    "Harness": "opencode",
    "WorkingDirectory": "/absolute/owned/checkout",
    "Approval": "supervised",
    "Sandbox": "unconfined"
  },
  "Brief": "Implement the bounded change and report its commit.",
  "Outcome": {"Mode": "human_decision"}
}
```

Use actual current revisions and the worker's exact desired configuration rather
than copying the example revisions. Start with `work start WORK_ID --request-id
REQUEST_ID --spec-file work.json`, then inspect with `work inspect WORK_ID`.
The server advances durable work; HTTP reads do not drive the workflow. Restart
reconciles admitted operations instead of blindly launching or delivering again.

`work evidence --file evidence.json` records an exact work attempt and artifact
revision; `work decide --file decision.json` records an authorized outcome.
Both require `request_id`, `work_run_id`, `expected_revision`, `step`, and
`attempt`. Evidence adds `kind`, `artifact_revision` and `detail`; a decision adds
`decision` and `reason`. Caller identity is derived from authentication, not JSON.
The initial outcome mode is human decision; worker-reported success is not an
independently executed verification result.

Outcome and cancellation do not release a checkout while its worker remains
live. Stop the execution before explicit removal. `workspace remove` requires the
workspace revision; `workspace restore --file request.json` restores an owned
removed checkout from its recorded branch tip and private ownership evidence.
The restore request contains `request_id`, `workspace_id`, and
`expected_revision`. A moved retained branch is refused rather than silently
restoring different work.

An uncertain work effect is not retried automatically. `work resolve --file
request.json` is an operator-only confirmation that the effect did not occur,
with `request_id`, `work_run_id`, `expected_revision`, and a required `reason`.
It records that conclusion and releases the relevant claims; it does not replay
the effect. Use it only after establishing what happened outside the backend.

## Shared product commands

Both shipped binaries use the shared command builders in `internal/product`. The client always goes through the authenticated Unix API;
selecting a management command does not make an execution caller an operator.
`tclaude agentd serve` and `tclaude-agentd serve` invoke the same daemon command.

For an explicitly initialized replacement directory, an operator can run:

```sh
tclaude --operator-state /absolute/new-state snapshot
tclaude --operator-state /absolute/new-state agent create --file agent.json
tclaude --operator-state /absolute/new-state agent update worker --file update.json
tclaude --operator-state /absolute/new-state authority list
```

`agent.json` contains `id`, `name` and `desired`; `update.json` contains `name`,
`desired` and the agent's `expected_revision`. Desired configuration uses the
existing API fields `Harness`, `Model`, `WorkingDirectory`, `Approval`, and
`Sandbox`. Agent creation is offline and does not start a native workload.

The `group` commands create groups and assign explicit bounded ownership.
`authority` provides grant/revoke, role assignment, exact-action explanation,
and execution-access inspection/revocation. Mutations read an explicit request
JSON file, including the expected revision required by the corresponding API.
A successful deletion has no response body. Requests are never automatically
retried.

`--operator-state` reads that directory's operator token and socket. It cannot
be combined with execution credential/socket flags or inherited execution
bootstrap variables. The ordinary execution client still uses
`TCLAUDE_BACKEND_SOCKET` and `TCLAUDE_BACKEND_CREDENTIAL_FILE`, rereading the
protected credential resource for each call. Missing execution credentials do
not fall back to an operator identity.

### Local browser client

Run the shared client's `dashboard --state-dir /absolute/backend-state` command
against a running replacement backend. It prints a private, single-use login
link for a loopback listener. The link expires after five minutes; the browser
exchanges it for an HttpOnly session cookie and removes it from the address bar.
The operator token remains on disk and is never sent to browser JavaScript.

The browser uses the same authenticated Unix API as the CLI. It supports offline
agent configuration, group creation, durable messages, history reading, owned
checkout and shell controls, bounded work evidence/outcomes, and terminal
attachment. Create an available workspace and an offline worker before starting
work from history. Exact fork remains provider-dependent; choose an explicit
fresh handoff when an exact fork is unsupported. Removing a dirty checkout needs
an explicit discard selection and workspace confirmation. Cancelling a work run
does not imply its worker stopped.

Opening another terminal keeps existing attachments in the Terminals tab. Select
a terminal tab to switch without losing its output; use the arrow keys, Home or
End while a tab has focus to switch with the keyboard. Disconnect retains the
local scrollback and offers Reconnect. Close tab removes that view. Each tab
targets the exact execution originally selected; it does not follow a replacement
execution automatically.

Disconnecting a terminal or closing the browser server closes the attachment
view, not its workload. Terminal resize is advertised only when the attachment
supports it. The negotiated `tclaude.terminal.v1` WebSocket protocol uses binary
frames for terminal bytes and text JSON frames for resize control. Unnegotiated
CLI clients retain byte-stream behavior.

Browser JavaScript runs under a same-origin content policy. Inline styles are
allowed for xterm's dynamically generated terminal styles; inline scripts and
third-party scripts remain disallowed. This listener is intentionally loopback
only, not a remotely exposed operator endpoint.

For the optional installed-Chrome acceptance flow, run:

```sh
TCLAUDE_BROWSER_SMOKE=1 go test ./internal/product/browser -run TestBrowserOfflineAgentGroupAndMessageFlow -count=1
```

The test uses new disposable backend state, not the operator's database, and
launches no native model workload. Ordinary browser session/proxy and WebSocket
lifetime tests run without Chrome under `go test ./internal/product/browser`.

### Offline migration preflight

The shared client can inspect an explicit operator-created schema-v228 snapshot
bundle without contacting a daemon or writing a target database:

```sh
tclaude migration inspect --bundle /absolute/snapshot-bundle --manifest manifest.json
tclaude migration plan --bundle /absolute/snapshot-bundle --manifest manifest.json
```

Both commands print redacted JSON reports. Blocking diagnostics produce a
nonzero exit status after printing the report. The plan includes deterministic
identity/reference mappings and preservation decisions; it does not activate
legacy authority, replay unfinished work, or perform the final target import.
No source directory or manifest is inferred from the current user's home.

To perform the offline conversion into a new private product directory:

```sh
tclaude migration import --bundle /absolute/snapshot-bundle --manifest manifest.json \
  --state-dir /absolute/new-imported-state
tclaude migration report --state-dir /absolute/new-imported-state
tclaude-agentd serve --state-dir /absolute/new-imported-state
```

The bundle contains an operator-created schema-v228 SQLite snapshot, a manifest
with relative paths, exact byte sizes and SHA-256 hashes, and any referenced
configuration/attachment files. Create it while the old writer is stopped or
using a consistent SQLite backup; do not copy a changing main database without
its committed WAL. This command does not discover live state or make a source
snapshot for you. Keep source paths and manifest entries immutable during import.

Conversion preserves supported durable identities, correspondence and attachment
content, configuration, historical usage and attribution. Untranslatable authored
features and authority are retained inactive with explicit diagnostics. Old
runtime handles are discarded, imported rules remain disabled, and uncertain
work is not replayed. Missing attachment content blocks conversion unless
`--metadata-only-attachments` is explicitly selected; report availability counts
then distinguish retained metadata from available bytes.

The target must be a new directory, or an exact retry of the same completed or
pending import. A pending target cannot serve requests. The product lock excludes
a running target daemon. Readiness is published only after database verification
and durable publication. An exact retry verifies imported semantics and refuses
changed targets or write sidecars; it never overwrites later product work. Once
you start operating the imported product, use ordinary product operations rather
than rerunning import to repair it.

`migration report` is a read-only, redacted receipt/mapping/diagnostic view. It
does not expose raw source messages, configuration values, credentials or
attachment bytes. Keep the target offline while using import verification and
report commands. Neither command starts the daemon or a native workload.

Saved configuration profiles have immutable revisions. Use
`configuration-profile save --file profile.json` with `request_id`, `id`,
`revision_id`, `name`, `desired`, and `expected_revision` (zero for a new
profile). `configuration-profile list` lists the current entries;
`configuration-profile get ID --revision REVISION` reads a selected revision.
The browser's Configurations page offers the same save and create-agent flow.

To create or update an agent from a saved revision, supply
`configuration_profile` containing the returned `ProfileID`, `RevisionID`, and
`ContentHash`, and omit `desired`. Supplying both is rejected. The application
resolves the exact stored configuration and records the selected revision on
the agent. Launch freezes that provenance and configuration on the execution.
Editing a profile or updating an agent cannot change an existing execution.
Catalog administration currently requires the explicit operator identity;
updates to an agent still require the caller's current configuration authority.

Saved defaults use exact configuration revisions. Read them with
`configuration-defaults`, and replace them using `configuration-defaults save
--file defaults.json`, including `request_id`, `expected_revision`, `global`
and `harnesses`. Each selection contains the profile ID, revision ID and content
hash returned by the configuration catalog. A new agent can select
`configuration_default: "global"` or a harness name instead of inline desired
settings or an explicit profile. Changing a default does not change existing
agents or executions. The browser Configurations page can set and clear these
defaults and create agents from a displayed saved revision.

Correspondence supports subjects, reply threads, To/CC audiences and bounded
attachments. Use `send --to AGENT --cc-operator --subject SUBJECT --attach FILE`,
or the browser composer. Operator inbox receipts use `read --operator MESSAGE`.
Retiring an agent requires an offline terminal execution state; reactivation is
explicit and does not restart its previous execution. Cloning configuration
creates a distinct agent and does not copy grants or ownership.

`usage refresh --conversation ID` explicitly collects supported native usage;
`usage query --conversation ID` reads durable observations. An execution target
is also accepted, with exact execution attribution only when the source proves
it. Conversation-wide counters are reported separately. Coverage distinguishes
complete, partial, unknown and unsupported data; cost retains its reported
currency and whether it is native or a historical estimate. Refreshing a source
again must not duplicate its totals. The browser Usage view shows observations
without adding successive cumulative samples together.

`activity --agent ID` (or `--conversation`, `--execution`, `--work`) reads
attributed operations and outcomes for exactly one authorized target. The
browser offers the same view from an agent row. These reads do not reconcile or
restart workloads, and historical imports do not confer operational authority.

### Process and team operations

The same authenticated client exposes `definition validate|save|list|inspect`,
`program-profile save|list|inspect`, `process start|inspect|evidence|resolve-blocked`,
`decision list|inspect|submit`, `automation save|list|inspect|run|occurrences`,
and `team deploy|list|inspect|rebrief|advance-phase|stand-down`. Mutations take `--file` with an explicit request ID
and the expected revision required by the operation. They do not retry native
work automatically. Program profiles declare an executable, argument prefix,
bounded output, timeout, confinement, and workspace execution authority.

In the browser, open **Processes → New process** to author a graph, or choose
**Edit process** on a saved definition. The editor provides task, decision,
fork/join, wait and end nodes. Drag nodes to arrange them, connect their ports
(or use the connection form), and edit the selected node in the side panel.
Tasks can bind a worker at launch, select an existing agent, pin a saved program
profile, or address human work. Decision connections name the permitted answer.

A task can also contain a **Plan**, explicit plan approval, ordered **Checks**,
and a **Review**. Edit each stage with the same agent/program/human performer
controls; human work may explicitly address the operator. Move checks up or down
to change their execution order. Saving, exporting, copying and reopening retain
the authored task and its stages as one unit. Starting the process binds every
stage's named workers and authorizes every pinned program, including nested ones.
The application compiles the task into a bounded graph; the original task's
completion waits for all its stages.

Plan approval offers **approve** or **rework**. Rework opens a new plan attempt
and approval window without spending the work budget. A rejected check or review
restarts work and its downstream checks, retaining the accepted plan and earlier
results. Checks and review share the task's work attempt limit; exhausting it
opens a blocked decision whose retry/rework action extends the work budget.
The monitor shows each stage, activation, attempt, decision and retained evidence.

For native agent stages, **Fresh context** stops only a concluded execution owned
by that exact task activation, using ordinary stop authority, and waits for
observed exit before launching again. **Reuse context** requires a live primary
execution and current interaction authority. Its input is admitted and consumed
once; a restart with a consumed but unsettled input reports uncertainty instead
of replaying it. Agent and human stages receive bounded recorded stage feedback;
program input keeps its declared JSON shape. These controls do not import legacy
YAML stages or add implicit timeout/contact policies.

The Parameters and Outcome controls edit typed inputs/defaults and required
evidence. Undo/redo and node copy/paste operate on the local draft. **Validate**
checks the draft through the application; **Save revision** writes an immutable
revision with the graph positions. A stale save keeps local edits available for
export and offers an explicit reload. Source text is preserved separately from
the executable graph and is editable through Source. Export/Import copy transfers
a v2 process JSON document; importing creates a new definition identity. It does
not activate a process or import an old YAML template implicitly.

Choose **New team template** or **Edit team template** in Processes to configure
members, roles, workspace policy, launch waves, briefing timing, typed parameters,
advisory phases and pinned automation revisions. A member can copy settings from
a saved configuration; these settings are stored in the team revision rather
than following later profile changes. Renaming a member updates wave and briefing
references in the draft. Each member must belong to exactly one wave.

Saving a template does not deploy it. Use **Deploy team** on the saved card to
select the mission, target group, workspaces and parameter values. Existing
deployments keep their pinned definition; **Rebrief** explicitly selects a newer
revision. Team export/import-copy uses v2 JSON and creates a separate identity.

A process start selects an immutable definition reference (definition ID,
revision ID, content hash, and kind `process`) or an explicit graph. Its deadline,
workspace scope, parameters, performer bindings, and authorized program profile
revisions are part of the request. `GET /v2/work/{id}` includes exact node attempts
and decision IDs; decision submission uses the current window revision and one
of its permitted answers. Reconnecting clients can read these records before
submitting an answer. Browser Processes and Decisions views use these same APIs.

The daemon composes a program host under its private state directory. Work runs
through the instance-owned reconciliation worker; an HTTP handler admits the
operation rather than owning its lifetime. Private host resource receipts and
execution authentication generations are not public process projections.

An execution can request bounded access with `access-request ask --file request.json`.
The request names an exact action/resource and, for configuration effects, explicit
configuration bounds. The Decisions tab shows access requests separately from work
verdicts, including the requested scope, reason and expiry. An operator approves or
denies that exact request; approval cannot edit it into broader authority. The
requesting client then retries the ordinary operation explicitly. Approval does
not replay a failed command, and an access decision cannot complete a work stage.

Message delivery commits the inbox entry before attempting a native notification.
The daemon sends a fixed inbox notice to a currently controlled primary execution
only when the recipient still requests notifications and the sender still has
message authority for that recipient. It does not inject the message body into the
terminal. A notice accepted by the native runtime is `delivered`; this is neither
an inbox read nor work completion. Offline recipients and an unconfigured native
operator notification channel are `unavailable`, with the message still retained.
A crash or uncertain native result after dispatch leaves `unknown` and is never
replayed automatically. The API exposes these outcomes on each message recipient.


### Blocked work and deployed teams

A bounded retry policy parks an exhausted attempt in a blocked decision window.
`process resolve-blocked --file resolution.json` supplies `decision_id`, the exact
`attempt`, `expected_window_revision`, `expected_run_revision`, `action`, `reason`,
and optional `evidence_refs`. Choose only an action offered by the window: retry,
rework, waive, or cancel. Keep the entire request and request ID unchanged when
retrying after a lost response. A waiver records an explicit exception; it does
not satisfy required verified evidence. The browser Decisions view uses this
same operation for blocked work.

Team deployment selects a pinned team definition and an explicit target:
`new_group` creates a group; `existing_group` reinforces the selected group with
new deployment-owned members. Workspace selection supplies either a shared
workspace or a member-key map, according to the definition. Existing workspaces
require their exact revisions; new checkouts require explicit creation intent.
The Processes and teams view offers existing available workspaces. The CLI/API
also supports explicit new-checkout intent.

`team list` and `team inspect ID` show the admitted roster, workspace ownership,
briefing operations, owned rhythms, phases and rebrief history after restart.
Rebrief explicitly selects another compatible immutable definition revision;
it does not replace running configurations or silently follow the latest version.
An advisory phase advances a checklist, not work evidence. Standdown stops and
retires only the deployment's roster, with current group management and each
stop/retirement permission checked independently. Checkouts and history remain
available; removal is a separate operation.

### Trusted pull request and activity conditions

Register each GitHub pull request at daemon startup, for example:

```sh
tclaude-agentd serve --state-dir /absolute/private/state \
  --github-source review=owner/repository#42 \
  --github-token-file /absolute/private/github.token
```

The credential file must be a private regular file. Omit it for unauthenticated
public reads. Up to four exact sources may be configured. Rules select the
configured source name and exact repository/pull-request resource; they cannot
provide a URL or credential. Collection is read-only and runs through the
existing reconciliation worker, polling each source at most once per minute.

`pull_request.changed` reports open, draft, closed or merged. `ci.completed`
reports succeeded, failed, pending or unknown for the selected head's observed
check runs and commit statuses. This is not a claim about branch protection or
required-check approval. Empty, partial, failed or stale reads cannot establish
positive completion or continuous dwell. Source identity remains stable across
repeated snapshots; an unchanged cached observation does not gain freshness.

Claude's protected native callback can report idle and awaiting-input activity
from its Notification hooks, retaining the original observation timestamp.
Tool activity, user input and uncertain activity invalidate those conditions.
Process liveness or an empty queue never establishes idle. Other providers
currently report unknown activity for these conditions. Unsupported or stale
observations break dwell eligibility.

## Browser automation

The Automation tab lists schedules, triggers, and standing orders. Create a
message rule directly or select a saved process/team template. Templates remain
pinned to their selected immutable revision when you reopen an existing rule.
Choose explicit recipients, an authority owner, allowed actions and targets,
configuration bounds, and an authority expiry. These bounds do not grant new
permissions: each effect still needs the owner's current authority.

Schedules accept a five-field cron expression with an IANA timezone or an
interval. Trigger rules select a configured source, exact resource, matching
values and freshness/dwell settings. Standing orders require a harness with
same-continuation guidance support. Enable/disable preserves the authored
revision and source cursor. Occurrence history shows per-recipient results and
links to any resulting work; Run now admits a separate deduplicated occurrence.
Process deadlines are relative to each occurrence's eligibility time.

A team rule can reinforce an existing group or name a new group explicitly.
A new-group ID is a fixed target, so a later occurrence cannot recreate it while
it already exists. Workspace selections remain exact, including retained
creation intents; editing a rule does not silently refresh its workspace pins.

The CLI exposes the same activation mutation:

```bash
tclaude automation set-enabled RULE_ID --file activation.json
```

The file contains `request_id`, `expected_revision`, and `enabled`. Repeating
that exact request is safe after a lost response; a changed request under the
same ID is rejected.

Process work cards provide **Inspect process graph**. The viewer uses the run's
pinned graph and durable node states, including nodes that have not activated.
Select a node to inspect all of its activations and attempts, retry times,
execution/operation identifiers, decisions, and attributed evidence. Refresh
reads the current run revision; it does not restart work or infer completion.

The message workspace groups visible messages into chronological threads and
supports subject/body/agent/attachment search, operator inbox/unread and agent
filters, and attachment-only filtering. A matching thread includes its available
reply context. Bulk marking changes only the operator recipient on the matching
messages, never another agent's read status or unmatched context messages.
Thread text export includes sender, recipients, body and attachment metadata;
attachment contents remain available through their separate download controls.
Terminal panes support tabbed or split layout and explicit pane ordering. The
current browser tab remembers execution IDs, sizes, selection and layout across
reloads; restored panes remain disconnected until reconnected. Pop-out opens
one exact execution in a separate window and disconnects the original only
after the new authenticated attachment confirms readiness. Closing either view
never stops the workload. Scrollback remains local to the view and is not
persisted across reloads.
Group settings in the browser support renaming, membership selection, member
ordering and changing the bounded owner role. `PUT /v2/groups/{id}` accepts
`name`, ordered `members`, and `expected_revision`; it checks current
`group.membership.manage` authority in the same transaction as the update.
Removing membership does not stop or retire the agent. An owner must be changed
or cleared through the existing owner endpoint before removing that member.
New members must be active; retired members already in the group can be retained
or removed. The CLI exposes the same request as `group update ID --file FILE`.

The Access workspace exposes role creation and action editing, exact scoped
assignments, assignment limit updates/removal, and direct grant editing/revocation.
Each save uses the displayed revision, so a concurrent authority change is a
conflict to review. Group-owner assignments stay under Group settings.
Configuration-bearing permissions require complete harness/model/directory/
approval/confinement allow-lists; the explicit disabled choice grants no such
authority. Grants remain subject to the owning operation's current authority and
lifecycle checks; declaring an action does not override an operator-only boundary.

Workspace navigation keeps the selected tab in the `tab` URL parameter and in
this browser tab’s session preferences. Reload and browser back/forward restore
that workspace; unknown tab values fall back to Groups. The Commands picker
(Ctrl/Cmd+K) searches visible workspaces and common authoring actions. It opens
the same forms as their normal buttons and does not execute a workload by
itself. Alt+1 through Alt+9 select the corresponding visible workspace tab;
these shortcuts do not intercept typing in form controls.
The browser's **Presentation and sound** controls select regular, wizard or
slop-machine mode. Wizard mode restores the tower/party/rite vocabulary,
purple-and-gold styling, casting and cursor effects, and the Tavern radio
collection. Effects respect reduced-motion preferences; labels and decoration
never alter product permissions or reported execution state. `dashboard
--wizard` and `dashboard --slop` open directly in a mode, as do `?wizard=1` and
`?slop=1`. Ctrl/Cmd+Alt+Shift+W or S toggles the respective mode.

Operator presentation preferences are saved with revision checks through
`GET/PUT /v2/presentation`. The preferences include mode, master sound,
separate music/effects volumes, radio in regular mode and an optional explicit
station. A saved station survives mode changes; “Use mode’s default station”
selects the Tavern for wizard mode and the Vegas lounge otherwise. Audio starts
only with Play and does not restart on a page reload. SomaFM streams require an
internet connection; the authenticated metadata route accepts only catalogued
stations and shows an unavailable message on failure.

Terminal tools can prepare bounded text drafts or load a local text file into a
draft without sending it. Explicitly bind the draft to a connected pane before
sending; switching panes or reconnecting requires a new binding. Key buttons act
on the currently selected connected pane. Scrollback search, selection copy,
text export and font size controls operate on the selected pane's current buffer,
not the complete native conversation history. Text-file loading is not a native
attachment upload.

Attention shows unread operator messages and current work/access decisions.
Opening an attention item navigates to its workspace without marking it read or
approving it. The dashboard checks for changes every ten seconds while visible;
it also checks in the background when this window's desktop notifications are
enabled. Desktop permission is requested only from the explicit enable button.
Only newly observed operator messages notify, never the initial backlog, and
sign-out stops checks and closes this window's notices. Unchanged snapshots do
not rebuild workspace controls; current API revisions still protect mutations.

Usage can be selected from recorded executions/conversations or by an explicit
historical target ID. Observed-time range filters, per-unit reading bars and
paged JSON export preserve the source coverage and attribution. The query keeps
only the current version of a cumulative source reading; a chart point is not
spend or tokens consumed during that time interval. Conversation-wide readings
stay labelled as such even under an execution target, and reported decimal costs
are shown without repricing or currency conversion. Exports identify whether
additional pages remain unloaded.

History offers active/archived and harness filters, title/indexed-text search,
ordering and paged conversation lists. Reads replace the current transcript;
role/text filters help inspect it without changing the selected native point.
Coverage and omitted parts remain visible, including in text exports. Archive
and title changes use the displayed catalog revision and retain edits on conflict.
Archive is catalog metadata, not deletion of native conversation content. Only
provider-advertised points can be selected, and starting work still requires
explicit worker/workspace and history-use choices.

Activity provides recorded target selection, kind and started-time filters, paged
inspection and JSON export. Text search covers only pages already loaded; export
includes the applied API filter and whether more pages remain. Stable record and
actor IDs, outcomes, reasons and imported provenance remain visible. Reading or
exporting activity does not acknowledge messages or decide work.

### Starting with an initial brief

An offline agent's **Start with brief** action sends an explicit initial message
through `POST /v2/launch` (`initial_message`). Ordinary Start and bulk Start keep
their existing behavior. The brief is delivered through the provider's prepared
input path before workload release, rather than a later terminal or message send.
Providers must advertise this capability and prove the exact input correlation
before release. Input is limited to 32 KiB of UTF-8 without NUL.

The launch operation retains a private digest for retry comparison. Reusing its
request ID with different or removed brief text is a conflict; exact retries
return the admitted outcome without another native preparation, including an
uncertain outcome. The text is not added to aggregate execution snapshots.
Initial briefs apply to explicit fresh launches; Resume does not replay them.
This request-level control does not yet define saved-profile startup context.

### Requested effort in launch configurations

Agent configuration, saved configuration revisions and team members accept an
optional `Effort` string. The browser exposes it as “Requested native effort /
variant”. Copying a saved configuration into a team copies this value too.
Blank leaves the native default in effect. A profile edit changes future selections;
existing agents and executions retain their pinned configuration, including effort.
JSON-based CLI/API configuration requests use the same `Effort` field.

Providers forward the request without translating level names: Claude uses
`--effort`, Codex uses `model_reasoning_effort`, Copilot uses `--effort=`, and
OpenCode uses the prompt's `variant`. Use a level or variant supported by the
selected native model. Native versions, model support and native policy can reject
or constrain the request; the stored value describes the request, not a measured
effective setting. Tokens must start with a lowercase letter or digit and contain
at most 64 lowercase letters, digits, underscores or hyphens. This setting does
not change approval or sandbox authority.

### Working-directory browser

Launch configuration and team member forms can browse directories on the backend
host. Enter an explicit absolute path, open a child or parent directory, and
choose **Use this directory** to fill the current form. Cancel leaves the field
unchanged. Selection does not save configuration, create a workspace, or launch
an agent; those operations retain their ordinary validation and authority checks.

The operator-only `GET /v2/directories?path=...` endpoint returns directory names,
canonical current/parent paths, and an optional `NextAfter` cursor. It reads no
file contents and performs no filesystem mutations. Hidden directories require
`hidden=true`; `limit` defaults to 100 and is capped at 200. Each read inventories
at most 8192 entries and refuses larger directories. Pages reflect the live
filesystem, so concurrent renames or removals can change subsequent results.

### Archiving saved configurations

Configurations can be filtered as active, archived or all. Archive hides fresh
selection actions while retaining every saved revision and all existing agent
and execution settings. Restore explicitly makes the entry selectable again.
Profiles still selected as global or harness defaults must be cleared or replaced
in Defaults first. Editing cannot silently restore an archived entry.

`POST /v2/configuration-profiles/{id}/archive` requires operator authentication,
`request_id`, `expected_revision` and an explicit `archived` boolean. The
`configuration-profile archive ID --file FILE` command sends the same request. Exact
retries return their stored result; changed intent or a stale revision conflicts.
Fresh agent configuration/default selection checks active status in the storage
transaction. Existing agents may still run their pinned configuration, and Clone
configuration copies those exact settings into an independent agent. Archive does
not stop workloads, delete native history, or discard immutable profile revisions.

### Reusable profile startup suggestions

A saved configuration revision can carry an optional `Startup` object with
`AgentName`, `Context`, and `InitialMessage`. Profile creation/editing exposes
these fields; create-from-profile/default prefills the suggested agent name.
**Start with brief** reads the agent's exact saved revision and opens its context
and message for review. When both are present, launch combines them with one blank
line and delivers the result through the ordinary prepared-input contract.

Editing a profile does not move an existing agent's selection. Archived revisions
remain readable for that agent. Suggested text is not included in aggregate
snapshots and never launches on save, cancel, refresh or reload. Plain Start,
Resume and bulk Start retain their existing behavior. Clone configuration remains
a native-settings copy and does not acquire a live profile reference.

The API's configuration-profile save body accepts `startup` alongside `desired`;
CLI JSON files use the same shape. Startup suggestions participate in immutable
content hashes and request identity. Name is limited to 256 UTF-8 bytes; combined
context and message to 32 KiB, without NUL. An absent/empty startup object preserves
the content hash of older native-settings-only profiles.

### Workspace inventory

The Workspaces tab filters and sorts registered resources by ownership, lifecycle,
path, repository, branch and recorded Git status. Status includes its observation
time; use Inspect or the bounded Inspect visible action to refresh it explicitly.
Export writes only the filtered inventory and its active claims to a local JSON
file. Active execution/work claims are shown by exact identity and block the
cleanup control. Owned checkout removal still requires explicit confirmation and
refuses dirty files by default; external directories have no removal action.
Restore recreates a removed owned checkout from its retained branch.

### Imported launch metadata

Offline v228 importer format 2 maps requested effort into agent/profile settings,
valid saved agent-name/context/brief fields into profile startup suggestions, and
the explicit disabled flag into the archived lifecycle. A pinned initial spawn
configuration's blank effort remains blank; it does not inherit a later row value.
No native effort aliases or role/permission grants are inferred.

Malformed UTF-8 SQLite text blocks conversion before target publication because
JSON cannot retain those bytes exactly. The source snapshot remains untouched.
Startup text that violates other target limits stays in the exact retained source record
and is identified by `profile_startup_retained_unmapped` in the redacted report.
Invalid effort is preserved verbatim with `requested_effort_requires_review`;
normal launch validation still refuses it. An unrecognized disabled flag archives
the profile and emits `profile_disabled_unrecognized` for explicit operator review.

Format-1 import destinations are not overwritten or accepted as format-2 exact
retries. Use a new destination for a new conversion. Exact format-2 retries still
verify the complete typed target, including effort, startup text and archive state.
This development change does not run an import against any live installation.

### Wizard terminal palette

Wizard mode uses the retained arcane terminal palette, including conventional
red/green/yellow ANSI meanings. Select Neutral terminal colours in wizard mode
to keep the regular palette. This operator preference is saved with presentation
settings and applies to current and newly opened terminal panes without reconnecting
or sending input. Same-origin pop-outs re-read saved preferences when another
window announces a successful save; the announcement contains no preference values
and cannot authorize a write. Reload saved preferences remains available when
BroadcastChannel is unavailable. Radio playback still requires local intent.
### Preview message attachments

Message attachments offer explicit Preview for PNG, JPEG, GIF, WebP and text.
Images require matching format signatures; text is rendered inertly and capped
at the first 64 KiB. Preview does not mark a message read. Unsupported content
remains available through the separate authenticated Download action. Closing,
signing out or leaving the page discards pending results and releases image URLs.
This is message attachment viewing; native terminal file staging is separate.

### Archive obsolete automation

Standalone schedules, triggers and standing orders can be archived from Automation.
Archiving confirms the exact rule ID, checks current management authority and
revision, and disables fresh dispatch while retaining authored revisions, cursors,
occurrences and already-admitted work. The archived filter keeps history inspectable.
Restore leaves the rule disabled; Enable is a separate explicit action.
Deployment-owned rhythms remain controlled by their deployment lifecycle.
The API is `POST /v2/automation/rules/{id}/archived` with `request_id`,
`expected_revision` and `archived`; exact command retries return their stored
result without repeating the state change, after current authority is checked.

### Terminal file uploads

The terminal's file panel accepts a selected file, one dropped file, or a pasted
image of up to 8 MiB. Selection does not upload. **Upload to selected execution**
stages the bytes in the provider's private upload directory and shows the exact
execution, file size, SHA-256 and historical path receipt. **Insert uploaded path
into draft** only fills the terminal draft; sending that draft is another explicit
action. Switching, disconnecting, reconnecting, closing the page or signing out
invalidates the local file selection and path insertion target. An upload already
admitted by the backend may still finish after the browser disconnects.

`POST /v2/terminal-files` takes `request_id`, `execution_id`, `filename` and
base64 `content`. It requires current `execution.file.stage` authority on that
exact execution, independently of terminal interaction permission. The response
contains an operation and file receipt; clients must inspect the operation state,
including `refused`, `admitted` and `uncertain`, before using any path. Identical
retries return the recorded outcome under current authority without publishing
again. Changed bytes or names conflict under the same request ID. Each execution
has a limit of 100 admitted files and 128 MiB in total.

Claude, Codex, Copilot, OpenCode and shell runtimes expose the focused staging
capability. Unsupported views omit it. Files are published with exclusive names
and private permissions after a current execution/authority check; uncertain
publication never causes automatic input or re-publication. Uploaded content is
retained separately from runtime stop cleanup. A receipt does not promise the
file remains present forever, and this API does not download arbitrary native
paths. The configured harness's own access policy still governs reading a staged
path once the operator sends it.

### Reuse process snippets

Saved snippets in the process editor stores selected nodes, internal connections
and their positions in the backend catalog. Insert copies them into the current
draft with new node IDs and supports Undo; it does not save a definition or start
work. Complete graph validation remains required, and retained performer/profile
references may need updating before save. Rename and Delete use displayed revisions;
conflicts retain the draft and require reloading the catalog. Unknown or corrupt
stored formats remain visible for management but cannot be inserted.

The operator-only API is `GET /v2/process-snippets` and
`POST /v2/process-snippets/{id}` with `request_id`, `action` (`create`, `rename`,
`delete`), `expected_revision`, and the applicable `name` and version-1 `selection`.
Selections are bounded to 100 nodes, 300 internal edges and 256 KiB; the active
catalog is bounded to 1,000 entries and 8 MiB of selections. Exact mutation retries
return the original admission without resurrecting deleted entries. Legacy imported
snippet records remain retained evidence; automatic typed conversion is separate.

### Group hierarchy

Group settings can move a group under an exact parent or return it to the top
level. The nested inventory shows stable IDs alongside names, including duplicate
names. This is organization only: memberships, owners, grants and lifecycle actions
remain scoped to their existing exact groups; no authority is inherited.

`PUT /v2/groups/{id}/parent` is operator-only and takes `request_id`,
`expected_revision`, and `parent_group_id` (empty for top level). The transaction
rejects missing parents, cycles, and hierarchies deeper than 64 levels. Exact
request retries return the admitted result; changed intent or stale fresh writes
conflict. Name and membership updates retain the parent relationship.

Offline conversion preserves legacy group parent IDs through the existing identity
mapping and verifies the resulting topology on exact retry. Invalid source trees
are refused before target publication. No hierarchy mutation receipt or authority
is imported.

### Files referenced by terminals

The terminal download panel reads an explicit relative path or an absolute path
inside the selected active execution's working directory. Embedded OSC8 `file:`
links and absolute local paths use the same download path on Ctrl/⌘-click. Ordinary
HTTP(S) links require the same modifier gesture and open without an opener;
unsupported schemes and remote file hosts are refused. No output automatically
starts a download or sends input.

`GET /v2/execution-files?execution_id=...&path=...` requires distinct current
`execution.file.read` authority on that execution. The backend resolves the
working directory, confines reads to it, rejects escapes and nonregular files,
and bounds a file to 32 MiB. It rechecks current authority and execution identity
before returning bytes as an attachment with an inert content type. Stopped,
replaced, unavailable and unauthorized executions fail visibly. Private provider
upload directories and files outside the execution directory are not implicitly
readable through this route.

The browser discards pending file reads after a pane switch, reconnect, pagehide
or sign-out. Downloaded files are not rendered inline. These are bounded current
filesystem reads, not immutable native-history artifacts.


## Group launch defaults

Group settings can pin an exact saved launch configuration revision and create a
member from it. The pin includes the working directory and saved startup
suggestions. Editing the saved configuration later does not change that pin or
existing agents. Clear a group's default before archiving the selected profile.
Parent groups do not supply implicit defaults or authority.

Creating a member commits the agent and ordered group membership together. A
lost response can be retried with the same request and returns the original
result, including after the default changes. Changed intent under the same
request ID is rejected. Creation does not launch a process. **Start with brief**
lets the operator review the pinned context and initial message before launching.

Operator routes are `GET`/`PUT /v2/groups/{id}/configuration` and
`POST /v2/groups/{id}/agents`. Default updates use their own expected revision;
member creation checks both group and default revisions. Refresh after a lost
default-update response before editing again. Legacy group default settings
remain retained import evidence; offline conversion does not activate these new
group defaults or member-creation receipts.
### Saved group order

Move group earlier/later in Group settings orders siblings without changing their
parent, direct member order or authority. The flat stable-ID order is saved in
operator presentation preferences and applies at every nesting level. New groups
follow saved entries in snapshot order. Reload saved group order also reloads the
saved presentation preferences; conflicting saves remain visibly unsaved until
reloaded. Preferences are revision-checked and do not start or stop workloads.

The main roster uses the same group hierarchy and saved sibling order as Group
settings. Filtered matching descendants retain their ancestor headings for
context. Choosing a group in the roster filter still selects only its direct
members; parentage does not expand membership. Group filter labels include stable
IDs to distinguish duplicate names. Select visible selects matching agent IDs,
not ancestor groups or implicit descendants.

## Group descriptions and task links

Edit group details stores a description, descriptive mission and optional task
or repository link on the exact group. Both Group settings and the main roster
show the metadata. Text is rendered literally; links require HTTP(S), open only
on explicit navigation, and are never fetched or previewed by the backend.
Mission text is descriptive and is not automatically sent to agents.

`PUT /v2/groups/{id}/details` is operator-only and checks the group's current
revision. Concurrent name, membership, parent or detail changes cause a visible
conflict instead of overwriting the edited state. Empty details clear the record;
ordinary group mutations preserve it. Offline import maps valid legacy details
and verifies them on exact retries. Unsupported text or link values remain in
retained source evidence with a `group_details_retained_unmapped` diagnostic.

## Group member limits

Group settings exposes an operator-controlled member limit. The count includes
active, non-retired agents directly in that group, whether or not an execution is
running. It excludes child groups and retained retired members. Zero removes the
configured limit; existing technical membership bounds still apply. Accepted
limits are whole numbers from zero to 2147483647.

`PUT /v2/groups/{id}/capacity` takes `max_active_members` and the exact
`expected_revision`. Lowering the limit never stops or retires agents. Over-limit
groups can still be renamed, reordered, and have members removed. Fresh additions,
creation from group defaults, and team reinforcement check capacity atomically
with their durable admission; failure creates no partial agent or deployment.
Reactivation also checks every direct group retaining that agent. Group hierarchy
neither shares nor inherits capacity.

Offline import preserves supported legacy limits and verifies them on exact
retry. Unsupported values remain in retained source evidence with a
`group_capacity_retained_unmapped` diagnostic. Imported membership remains intact
even if it already exceeds a configured limit.

## Imported group launch defaults

Offline conversion preserves each group's legacy default profile ID as an exact
immutable group configuration reference. Source IDs are resolved independently
of profile names and aliases. Import does not create a new member, start work,
or restore legacy authority. The regular Launch defaults and Create member from
default controls read the imported selection through the same application API.

Archived profile selections remain inspectable, but cannot create fresh members
until the operator explicitly restores or replaces that profile. Missing or
unsupported selections are refused by source preflight or retained with a
`group_default_retained_unmapped` diagnostic; conversion never guesses another
profile. Exact import retries verify both the configuration row and pinned
profile reference and refuse changed targets.

Imported profile references preserve their source namespace: stable row IDs,
profile names and aliases are resolved separately. A numeric profile name does
not select the same-numbered row. The global default's stable-ID preference takes
precedence over its retained name; aliases resolve their exact profile ID.

### Clone a group without starting work

Group settings → **Clone group** creates a separate top-level group. Choose
whether to copy active members as new offline agents and whether to retain the
source's exact pinned launch default. Descriptions, mission, links and the
selected member limit are copied; retired members are skipped. The new agents
keep their copied desired configuration and source lineage. Source groups and
shared memberships remain unchanged.

The copy has no owner or copied permissions, executions, messages, workspaces,
or automation. Starting work and assigning authority remain explicit actions.
Archived configuration selections cannot be used to create fresh copies.

`POST /v2/groups/{id}/clone` accepts a request ID, new group ID/name, source group
revision, exact member revisions when copying members, and the default revision
when copying the default. Admission checks the snapshot and commits the complete
copy atomically. An identical retry returns its stored result, including after
restart or later source changes; changed intent conflicts. Stale source changes
or rejected configurations leave no partial copy.

### Transfer saved configurations

Configurations → **Export configurations** selects a portable JSON bundle.
**Import configurations** accepts a file or pasted bundle, previews its entries,
then lets the operator select entries, rename copies, or replace exact displayed
configuration IDs/revisions. The dialogs adapt the old dashboard's profile
transfer components. Up to 128 entries and 1 MiB are accepted.

Import commits all selected entries and its retry receipt atomically. A stale
replacement or an attempt to archive a selected default leaves the entire batch
unchanged. An identical lost-response retry returns the stored outcome; changing
choices starts a new command. Archived state and startup suggestions are retained;
replacements create immutable revisions and do not change existing agent pins,
launch work, copy authority, or change default selections.

The bundle format is `tclaude-configuration-profiles`, version 1. Preview and
import use operator-only `POST /v2/configuration-transfer/inspect` and `/import`.
Legacy spawn-profile bundles with fields outside the replacement catalog are
explicitly unsupported, rather than silently losing their settings. Close,
page exit, and sign-out invalidate pending dialog responses; unsaved pasted or
loaded input has discard/unload protection.

### Save an agent's settings for reuse

An active agent row's **Save settings as configuration** opens the ordinary
configuration editor with its displayed desired settings and an agent-name
suggestion. Review or edit the draft, then explicitly save a new independent
configuration. Opening or cancelling writes nothing. The source agent, its
saved-profile selection and any running execution remain unchanged. This copies
authored desired settings; it does not probe effective native settings or copy
runtime state, authority, messages or launch intent.

### Configured launch environments

Agent settings and saved configurations expose literal environment name/value
rows. Group Launch defaults can supply shared values; Create member from default
shows the effective configured environment and accepts explicit overrides. The
precedence is group, pinned saved configuration, then explicit member overrides.
The resulting agent settings are copied into each admitted execution. Later
profile or group edits do not change that execution or existing agent settings.

The typed `Environment` map carries only explicitly configured values. The API
never reads the backend's ambient environment into this projection. Values are
not interpolated as shell expressions. Provider-owned credential, home, callback,
loader, and policy control names are reserved. All four provider adapters validate
and copy the values before preparation and pass them through the process boundary.

Configuration authority includes exact allowed `Environments` sets alongside the
existing harness/model/directory/approval/confinement lists. Omitting the new list
permits only an empty configured environment, preserving the scope of old grants.
The Access, group owner, and automation editors can author exact sets. Current
release and durable launch retry checks use the admitted environment.

Offline conversion preserves valid legacy profile, agent, and group environment
rows. An unsupported or ambiguous set remains in retained source evidence with a
`launch_environment_retained_unmapped` diagnostic. Conversion creates no execution
or active grant. Group environment does not dynamically inherit from parent groups.

### Group shells

Group settings offers **Open group shell** for an explicitly selected available
checkout. The dialog previews the group's configured environment and accepts
literal per-shell overrides; an agent launch profile does not apply to a shell.
The shell is explicitly unconfined, retains its group/configuration revisions and
effective environment, and appears on that exact group's card for attachment or
ordinary stop. Group edits never modify an existing shell.

`POST /v2/shells` accepts `environment` and optional `group` with `GroupID`,
`Revision`, and `ConfigurationRevision`. Selecting group configuration requires
the operator. Ordinary delegated shell starts must match current workspace
start-shell authority and an exact environment allow-list, including the empty
set. Admission and release recheck authority. An exact request retry checks
current authority and returns the durable operation before mutable workspace,
group, or provider preparation; changing the authored request conflicts. An
admitted or uncertain operation is never started again by retrying. The normal
workspace-use claim remains until shell exit is observed.

### Usage and cost overview

Usage includes an expandable overview with UTC date presets/month navigation,
harness and exact-agent filters, source selection, daily charts, attributed
readings, coverage details, and JSON export. The chart adapts the previous Costs
renderer; labels, totals and export preserve exact integer/decimal values.

`POST /v2/usage/summary` is an operator-only read of recorded observations. Its
`filter` requires an inclusive `After` and exclusive `Before` (at most 366 days),
with optional `Harness`, `AgentID`, and `ConversationID`. It does not collect new
native usage. A bounded query includes each cumulative source's prior baseline
in the same read transaction and refuses an oversized result instead of silently
truncating totals.

Daily values represent changes observed on that UTC day, rather than invented
consumption timing. Compatible complete cumulative readings contribute their
difference; event readings contribute their value. Missing baselines, resets,
unit/currency changes and incomplete coverage produce visible exclusions.
Sources, cumulative/event accounting, native/historical records, attribution
precision, currencies and native/estimate cost kinds stay separate. Conversation
usage does not inherit an agent identity. Missing or unpriced usage is not zero
spend, and no token prices or currency conversions are inferred.

### Configured launch support

Agent and configuration forms and the team member editor show the configured adapter's supported approval/confinement choices. Changing harness or policy refreshes this read-only explanation without altering authored values. Unsupported or unavailable-provider settings can still be saved as offline intent; they require correction or provider configuration before launch.

`GET /v2/launch-support?harness=<name>` is operator-only and reads the adapter's static declaration. It reports whether that provider is configured, whether policy support is known, supported approval and sandbox modes, host sandbox preparation, and prepared-initial-input capability. It performs no native preparation, credential delivery, storage write or execution. This is not an installation, authentication, authority or runtime readiness check. Providers that omit the declaration remain explicitly unknown. The actual preparation and release gates remain authoritative.
### Capture a group as a team template

Groups offers **Save group as team template**. It opens the existing team editor with independent copies of the displayed active direct members, retaining order, names and desired launch settings including effort and environment. Stable template member keys are generated independently of names. Nothing is written until **Save team revision**, and saving does not launch work. Cancelling leaves the group and definition catalog unchanged.

The editor explains the capture boundary: retired members and child groups are omitted; live owner/role authority, messages, runtime state and rhythms are not copied. Description and mission are retained as source notes rather than delivered briefings. Review owner, roles, workspace policy and briefings explicitly in the draft. Separate member workspaces are selected initially. Later source group, member or saved-configuration edits do not alter the captured draft or saved team.


### Team member environment editing

The team member editor includes the same literal environment variable rows as launch configurations. Copying a saved configuration replaces the member draft's environment by value, including clearing it when the saved configuration has none. Apply commits the edited rows to the local team draft; Save persists a new immutable team revision. Existing agents, executions and source configurations are unaffected. Add/remove changes participate in unapplied-change protection, and the application refuses invalid or reserved environment names before saving the team definition.

### Program configurations in the browser

Processes includes a Program configurations panel for creating and editing saved
command revisions. Arguments are a JSON string array and environment values are
literal strings; neither is shell-expanded by the editor. Set the command timeout,
output limit, sandbox mode and required effect authority explicitly. Saving creates
no work. A process selects and pins a saved revision, so later configuration edits
do not alter existing process definitions or runs. Concurrent edits return a
conflict and retain the local form for inspection or copying.

### Sandbox profile editor and path preview

Configurations includes a Sandbox profiles panel. Create, copy, edit, inspect,
archive and restore policies through the authenticated authoring API. Expand the
filesystem, network, environment, resource and setup sections to edit literal
rules. Includes select exact immutable revisions; later edits to an included
profile do not change a saved selection. Save conflicts retain the draft, and an
unchanged retry after a lost response returns the original result.

Preview validates the draft and pinned includes and observes filesystem paths
without creating missing directories or running setup scripts. It reports path
kind, canonical spelling, missing paths and grants intersecting the backend's
protected state directory. Included observations retain their source revision.
The combined include preview shows filesystem/environment overrides, ordered
setup, and each independent network/socket constraint. Engine choice has explicit
include precedence; private namespace and harness configuration floors remain
restrictive. A shared include is composed separately within each sibling before
those siblings combine. These observations and composed values are not an
enforcement receipt. Agent, saved configuration, team member and shell forms can
select an active saved profile. Selection pins exact revisions and resolved policy
identity; later profile edits do not change an existing selection. The launch
preview reports whether the configured adapter supports host sandbox preparation.
Actual launch also checks the selected policy, host capabilities and current
authority, and refuses unsupported policies before native release.


### Sandbox destination packs

The sandbox editor lists the retained destination-pack catalog with exact domains,
ports and a content hash. Choose Off, Allow or Deny for each pack after inspecting
its entries. Unknown IDs, duplicate IDs and conflicting polarities are rejected
before persistence. Selections survive save and reopen; the
catalog does not silently include extra provider destinations or subdomains.
Packs are authoring conveniences, not guarantees of complete provider connectivity.
Saving a pack reference does not grant network access or launch a workload.

### Sandbox profile transfer

Each sandbox catalog card can export its exact immutable revision and complete
include graph as a versioned JSON bundle. Import accepts a file or pasted JSON,
validates all hashes and dependencies without host lookup, and previews each
revision before creating independent named copies. Every included revision gets
a new profile identity, including when the source graph pins different revisions
of the same profile. Include references are remapped to those exact copies.
The whole graph and original-result retry receipt commit in one transaction;
existing target identities cause a conflict without partial publication.

Transfer preserves authored policy text and pack IDs. It does not import grants,
defaults, executions or enforcement receipts, and does not run setup scripts or
create host directories. It accepts the replacement bundle format only; legacy
files are not silently interpreted as equivalent policies. The logical bundle
limit is 16 MiB, with a separately bounded transport envelope. Source profile
names are export-time labels; exact revision references identify policy content.

### Resolved sandbox policy identity

Sandbox path preview now shows a separate resolved-policy identity. The authored
revision hash still identifies the saved document; the resolved identity also
covers canonical host paths, exact included revisions, scope precedence and the
expanded destination pack contents. Combined network constraints show explicit
allow/deny destinations instead of unresolved pack names, while retaining each
source restriction independently. Pack display labels do not affect identity.

This is a bounded, versioned authoring result, not launch authorization or proof
of isolation. No setup command runs during preview. Actual launch integration
must recheck host identity, current authority, provider resources and enforcement
capabilities before using a retained policy.

### Process descriptions and documentation

Every process node and task stage can retain a description and longer documentation in its saved immutable definition. These plain-text notes survive copy, export, stage compilation, and run inspection. They are separate from worker briefs and human prompts and do not alter execution instructions or authority. Descriptions are bounded to 16 KiB and documentation to 64 KiB; the editor and run monitor display authored markup literally.

### Process output-name authoring

Task nodes can save an ordered set of up to 128 published output names. Names use lowercase letters, digits, periods, underscores and hyphens, begin with a letter or digit, and are at most 128 bytes. The editor removes duplicate lines while preserving order. Copies and exports retain the declarations; clearing them creates an explicit new definition revision.

As in the legacy engine, runtime production of captures is unavailable. Starting an inline or pinned process with capture declarations returns an unsupported error before creating a run. The editor states this limitation. An older pinned revision remains non-executable even after captures are removed from a newer revision.

### Process contact-schedule authoring

Task performers and plan/check/review performers can retain a contact cadence, positive contact budget, and escalation target. Cadence uses a positive Go duration such as `30m`; budgets are 1–10000 and escalation targets are bounded nonempty text. Changing performer kind retains an applied schedule. Clearing all three fields removes it in a new revision.

These settings preserve the legacy authoring contract. They do not create notification jobs or grant authority to the named target. As in the legacy engine, starting a process with a contact schedule is explicitly unsupported, including schedules on nested task stages. The editor explains this before execution.

### Process overview and parameter help

The process editor's Overview action authors the process description and longer
plain-text documentation. Both are saved in the immutable graph revision and
remain visible when starting and inspecting that pinned process. Process and
team parameters can also carry an optional display name and documentation.
Changing the display name leaves the parameter's exact input key unchanged;
launch and deployment forms submit values under that key. Documentation is
rendered as literal text, including markup-like content. Empty optional prose
fields are omitted from persisted JSON.

### Legacy sandbox profile conversion

Offline v228 import converts representable sandbox profiles into archived,
immutable replacement revisions. Include names resolve only within that snapshot
and become exact revision references. The archived catalog supports inspection
and explicit independent copies; imported defaults, assignments, grants and
runtime state are not activated. Legacy `network_access=none` retains its coupled
closed Unix-socket posture when no newer network axis was authored.

A policy with unsupported fields, spelling aliases that need a target alias
representation, conflicting representations, invalid quantities or unresolved
includes remains wholly in source evidence with a redacted diagnostic. Dependent
profiles remain pending too. No partial editable policy is published and no setup
script or host lookup runs during conversion. These pending cases remain parity
work; they are not accepted feature exclusions.

Exact retries verify both policy documents and the scalar lifecycle/revision
indexes. A destination produced by an older evidence-only conversion is refused
if it lacks the newly expected typed records; the importer never rewrites that
existing destination. Use a fresh explicit destination for the new conversion.

Imported sandbox profiles carry a durable imported marker. They remain archived and reject restore or same-identity edits; inspect, export, and explicit independent copy remain available. Independent copies use fresh identities and normal editable lifecycle rules.

### Process wait authoring

Wait nodes can retain a duration, an RFC3339 timestamp, a named signal, or a
combination. The editor saves these fields without rewriting their text and
explains their runtime limits. Timestamp and signal declarations preserve the
legacy authoring capability; they do not create wake subscriptions. A process
containing either is refused before run creation, including an older pinned
revision after a newer revision clears the fields. Duration-only waits retain
their existing execution and restart behavior.

Performer timeout text is retained in task and compound-stage authoring. Blank
uses existing defaults. Programs support positive timeouts up to one hour:
the deadline begins at node readiness, includes admission delay, is persisted
with the attempt, and never extends the run deadline or saved program limit.
The exact pinned profile revision contributes its timeout before initial or
retry readiness; queue delay cannot restart that tighter clock. Compiled
activation budgets survive restart and cannot be supplied by authored graphs.
A retry receives its own readiness-relative bound. Expired queued work is
rejected before native issuance. Existing host deadline enforcement and
observed-exit cleanup handle launched programs. Longer timeouts and agent/human
timeout declarations remain representable but execution is explicitly refused
before creating work; they are never silently ignored. Timeout text is not a
parameter interpolation surface.

Team briefing mission placeholders are explicitly enabled per briefing with
`Syntax: "mission-v1"` (the editor's Mission placeholders choice). Exact
`{{task}}` and `{{mission}}` tokens expand once from the admitted deployment's
mission. Values containing tokens are not expanded again. Initial input,
after-ready messages, and explicit rebriefs use that retained mission and
pinned authored briefing revision; recipient routing is unchanged. An absent
syntax keeps text literal. Expanded input is checked before checkout creation,
and independent message and initial-input limits still apply. Saving a template
does not deploy or send anything.

### Explicit process parameter expansion

Process Overview can enable `mustache-v1` parameter expansion for that immutable
revision. Existing definitions with no parameter syntax remain literal. References
such as `{{ params.release_key }}` use declared keys, independently of display
labels, in agent briefs, human prompts, explicit decision questions and individual
program arguments. Compiled plan/check/review inputs follow the same rule.

Required values and typed defaults are resolved before expansion. Missing optional
values become empty text; strings are inserted literally and other typed values
use compact JSON without numeric rounding. Substituted text is not interpreted
again, split into arguments or passed through a shell. A field is bounded to
128 KiB and total expanded input to 1 MiB, with existing narrower program limits
still enforced before admission. The resolved graph is durable across restart;
its saved definition remains unchanged.

Configuration identities, approved program commands/prefixes, environment,
workspace, audiences, routes, notes, wait/retry/contact policy and arbitrary JSON
input are never expanded. Decision questions can be authored separately from
node names; a missing question retains the node-name fallback. Parameter keys in
this syntax begin with a letter or underscore and contain letters, digits and
underscores. An undeclared reference is rejected before saving or starting.

Legacy compound-task escalation loops can be authored and retained without
starting them. A failure route (`fail`, `failed`, `failure`, or `error`) may
enter a dedicated human decision with exactly `retry` back to that task and
`cancel` to a cancelled end. It cannot be the entry or receive another source.
Only this retry edge is exempt from ordinary acyclic validation and appears as
a dashed return. All authored edges survive save/reopen and copy/export.

Compilation records that authoring-only declaration separately from its DAG;
clients cannot supply compiled escalation metadata. Starting any revision with
such a loop is explicitly refused before work admission, including an older
pinned revision. This matches the legacy runtime's unsupported-loop boundary;
ordinary task retry budgets and blocked-work resolution remain executable.

### Human task answer vocabulary

Human task performers, including plan/check/review stages, can author ordered `Choices` and an exact `ChoiceOutcomes` mapping to `pass` or `fail`. Each single-line, trimmed, case-insensitively unique answer requires one mapping; empty vocabulary retains the existing `complete`/`reject` answers. The editor uses matching answer/outcome lines, and immutable revisions, copies and exports retain them.

Decision windows offer the admitted answers. Settlement uses the attempt's retained mapping: `pass` follows ordinary `complete` success and `fail` follows ordinary `reject` routing and retry policy. Submitted labels remain in decision evidence. Labels such as `cancel` or `waive` have only their mapped task outcome; they cannot activate separate cancellation or waiver behavior. Decision-node answer routing and current audience authority are unchanged.

Process connector labels support Automatic, Always show, and Hide unless selected
in the connection inspector. Automatic uses the graph renderer's outcome rules;
selected connections remain readable. The preference belongs to editor layout
and identifies the exact source, destination and answer/outcome tuple. Saving,
reopening, export/import and node/snippet copies retain it; copied endpoints are
remapped and deleted connections discard their preferences. Label visibility
never changes an edge's routing verdict or grants execution authority.

### Plan approval retry authoring

A plan with explicit approval can retain an `ApprovalRetry` declaration with
positive `MaxAttempts`, optional positive literal-duration `Backoff`, and
`OnFail` set to default, `fresh-attempt`, or `feedback-same-session`. The plan
inspector exposes these fields. Immutable revisions, copy/export and reopening
preserve the declaration; removing approval clears its dependent retry policy.

These declarations are authoring-only, matching the legacy runtime boundary.
Starting any pinned revision containing one is refused before work admission;
the runtime does not silently replace the requested policy with ordinary plan
rework. Clear the declaration to use the existing executable approval workflow.

Approval retry attempt counts retain the positive signed 64-bit authoring range.
The API emits exact decimal strings and accepts both integer and string JSON
input, so browser editing preserves values above JavaScript's safe integer range.

### Retry modes and independent stage policies

Retry policies retain optional `OnFail` as `fresh-attempt` or
`feedback-same-session`; an absent mode retains existing fresh-attempt behavior.
Task, plan, check, and review inspectors expose the mode alongside the attempt
budget, delay, and explicit failure classes. Copies and immutable revisions
retain these fields.

Independent check/review retry policies and feedback-same-session retries are
currently authoring-only, as in the legacy runtime. A start requesting either
is refused before creating work, including a pinned older revision after the
latest draft clears it. Clear independent check/review retries to retain the
existing shared work budget; choose fresh attempts for executable task or plan
retries. No unsupported declaration is silently ignored.

Automation occurrence retries do not accept process-only retry modes.

### Decision performer authoring

The decision inspector supports a human decision or an agent/program decider.
Automated deciders retain the same explicit worker binding or immutable program
profile reference, input, timeout, and contact declarations as task performers.
They are saved under the decision's optional Decider field and survive copies
and immutable revisions. Human audience settings remain retained for an explicit
switch back to human mode; they are not an execution fallback.

As in the legacy runtime, automated decisions are authoring-only. Starting a
pinned revision with an automated decider is refused before work admission.
Clearing only the latest revision does not change an older pin. Plan approvals
and human escalation-loop audiences remain manual decisions.

### Process worker configuration copies

Agent performer inspectors can copy an active saved launch configuration into
an independent new-worker declaration, then edit its model and effort. The
remaining desired settings, including literal environment values, are retained
and displayed. Subsequent edits or archival of the source configuration do not
change this value. Use the explicit existing/bound-worker action to remove the
new-worker declaration.

New-worker creation in a process is authoring-only and is refused before run
admission. Saving or copying these settings creates neither agents nor
executions. Existing exact worker bindings retain their ordinary behavior.

### Explicit process start nodes

The process palette includes an optional Start node with its own stable identity,
name, prose and layout. A graph permits at most one, with exactly one unlabelled
outgoing route, no incoming routes, and no performer or retry settings. When
present it must be the selected entry. Existing graphs need no Start node.

Start routing creates no native work. It settles through ordinary graph
transitions and resumes after restart even when admission committed before the
initial transition completed. Copies, snippets and monitoring retain the node
instead of replacing it with an invisible edge.

### Authored retry counts and execution limits

Process retry counts retain the signed 64-bit authoring range. Counts above 100
can be saved, copied and reopened, but starting such a pinned revision is
refused before admission. This matches the legacy separation between authoring
and executable retry budgets. Clearing a newer revision does not change an old
pin.

The API preserves numeric JSON for existing bounded counts, keeping their
definition hashes unchanged; counts above JavaScript's safe integer range are
emitted as exact decimal strings. Both integer and string inputs are accepted.
The editor keeps these values exact. Runtime counters remain bounded to 100;
automation occurrence policies still reject counts outside 0–100.

Human process tasks retain a separate question and longer context in the shared
performer editor, including plan, check and review stages. Either field may be
used alone. When both are present, the admitted decision displays the question,
a blank line, then the context as literal text. Explicit process parameters apply
to both fields; saved revisions retain the original authored text. Existing
prompt-only definitions retain their original presentation and serialization.

### Legacy process source import

The process catalog's **Import legacy process** action inspects a YAML or JSON
`ProcessTemplate` using the retained v1 authoring parser. Inspection reports
source diagnostics and the performers that require explicit target mappings.
Human audiences are selected by stable identity, worker settings are independent
copies of selected configurations, and program mappings require an exact pinned
profile with the same literal executable and no argument prefix. Source profile
names never select a target implicitly.

Conversion returns an unsaved draft. **Open unsaved copy** opens the ordinary
process editor; only **Save revision** publishes a new definition. Neither
inspection nor conversion starts work, creates workers, or changes source
profiles. The original source remains available in the editor's Source view.
Selecting another file invalidates the earlier preview even if the new file
cannot be read.

The converter currently reports rather than approximates ordinary multi-verdict routes, and durations outside the editor's exact numeric
range. A refusal leaves the source intact for inspection and does
not publish a partial definition.


Parameter defaults retain their exact JSON text through process and team editing,
copy/export/import, and launch input. Public definition reads include a
`DefaultJSON` text companion to the existing `Default` value; the browser uses
that text when preparing ordinary JSON write requests. Stored model serialization
and existing revision hashes are unchanged. Decimal and large-integer parameter
values are sent without passing through JavaScript number serialization, including
nested object/array values. Legacy source conversion reads numeric defaults from
the bounded YAML tree and preserves alias/merge precedence without a floating-point
intermediate.


### Imported joins and conditional arrivals

Legacy `join: all` and `join: any` become explicit join nodes before the original node. Original node IDs, work, source text, and layout positions remain intact; incoming connector preferences follow the redirected edges. Generated join IDs are deterministic and avoid every authored node ID. The join is ordinary editable graph structure after conversion. A non-entry legacy Start control marker is itself represented by a join under its original identity and name, with its declared all/any policy (or an all-join for a simple pass-through); it does not become an invalid second v2 entry node.

All-joins wait for every incoming route that can still arrive. Only settled decisions with a durable answer remove unselected paths; unanswered or unresolved decisions remain possible. Already-arrived joins are reconsidered when another decision rules out a pending candidate. This survives backend reopen without replaying a reducer. Any-joins activate once on the first arrival, while losing branches remain accounted for until settled. These rules do not waive required outcome evidence or suppress active work.

Process duration inputs retain integer nanoseconds through legacy conversion, editor changes, saved snippets, export/import, and definition reopen. Seconds accept decimal text with up to nine fractional digits, through `9223372036.854775807`. Public definition, conversion, and snippet responses include a `ProcessDurationNS` companion for values outside JavaScript's safe integer range; existing numeric fields and immutable stored bytes remain unchanged. Browser writes serialize exact numeric nanoseconds. Execution still applies the existing run deadlines and admission rules.

Imported process nodes retain `RoutingMode: single-route-v1`, including through saved snippets and independent copies. Ordinary nodes settle their sole connector independently of its label, and manual decision answers select exact connectors without interpreting names such as `cancel` or `waive` as control commands. Ordinary task rejection still fails or blocks the work; a connector named `reject` does not turn that rejection into success. Multiple ordinary routes remain editable and persist in immutable revisions, but start refuses them before work admission, matching the old engine's execution boundary. Use an explicit fork for simultaneous branches or a decision for selected routes. Existing native v2 nodes retain their current routing mode.

### Template library lifecycle

Process and team templates can be archived from the library and restored with their exact displayed library revision and request identity (`POST /v2/definitions/{id}/archive`). Active, archived and combined views expose the stable template ID for confirmation. An archived template cannot be edited until explicitly restored; a stale editor cannot restore it by saving. Response-loss retries return the original lifecycle receipt without replaying an older archive or restore.

Archiving changes library visibility, not immutable content or authority. Historical revisions and explicit pinned references remain readable and usable by existing workflows; archive is not cancellation or permanent history deletion. Restoration does not start or deploy anything.

### Disbanding groups

Group settings provides **Disband group** (Disband party in wizard mode), with
an exact-ID confirmation and a preview of retained members and their other
groups. Disbanding removes active membership and group role assignments,
archives and disables schedules targeting that group, and moves its children
to the top level. Agents, conversations, messages, work history, immutable
configuration pins, and checkouts remain. Retire members explicitly through the
roster first when desired; disbanding never implicitly stops native work.

The revision-checked `POST /v2/groups/{id}/disband` requires a request ID and
`expected_revision`, and the separate `group.disband` authority on that exact
group. Active group-scoped work, group shells, or team deployments block the
operation with the identity to settle or stand down first. The transaction
fences fresh group admissions, retains a non-reusable group identity, and
records the original result for exact retries. Reading that caller’s exact
receipt requires a currently valid principal, rather than a group role which
the disband itself removed; a fresh effect still requires current disband
authority. Retired agents and revoked execution credentials cannot replay it.
Busy-group conflicts expose a fixed cleanup instruction and the blocking
resource ID in the dialog, separately from stale-revision conflicts. There is
no restore action.
