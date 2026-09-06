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
`program-profile save|list|inspect`, `process start|inspect|evidence`,
`decision list|inspect|submit`, `automation save|list|inspect|run|occurrences`,
and `team deploy|inspect`. Mutations take `--file` with an explicit request ID
and the expected revision required by the operation. They do not retry native
work automatically. Program profiles declare an executable, argument prefix,
bounded output, timeout, confinement, and workspace execution authority.

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

Message delivery commits the inbox entry before attempting a native notification.
The daemon sends a fixed inbox notice to a currently controlled primary execution
only when the recipient still requests notifications and the sender still has
message authority for that recipient. It does not inject the message body into the
terminal. A notice accepted by the native runtime is `delivered`; this is neither
an inbox read nor work completion. Offline recipients and an unconfigured native
operator notification channel are `unavailable`, with the message still retained.
A crash or uncertain native result after dispatch leaves `unknown` and is never
replayed automatically. The API exposes these outcomes on each message recipient.
