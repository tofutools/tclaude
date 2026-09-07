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

Terminal panes support tabbed or split layout and explicit pane ordering. The
current browser tab remembers execution IDs, sizes, selection and layout across
reloads; restored panes remain disconnected until reconnected. Pop-out opens
one exact execution in a separate window and disconnects the original only
after the new authenticated attachment confirms readiness. Closing either view
never stops the workload. Scrollback remains local to the view and is not
persisted across reloads.
