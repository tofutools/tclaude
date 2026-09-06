# Replacement backend development server

`tclaude-backend-dev` composes the replacement application, SQLite store, HTTP
API, and harness providers. It runs independently of the existing daemon. Its
state directory must be new and explicitly initialized; it does not import the
existing database or adopt its live agents.

Build without installing or restarting the existing daemon:

```bash
go build -o /tmp/tclaude-backend-dev ./cmd/tclaude-backend-dev
/tmp/tclaude-backend-dev --state-dir /tmp/backend-example --init
/tmp/tclaude-backend-dev --state-dir /tmp/backend-example
```

Omitting `--harness` provides an offline catalog. Register installed providers
with `--harness claude,opencode`. Registration resolves their executables; it
does not launch a workload. The development server targets Linux and macOS.

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
Stopping the development server also leaves workloads for later recovery.

Current provider support is explicit: Claude uses a terminal workload and reports
unresolved context when its private native observation channel cannot establish
continuity. Context changes require correlated native evidence; dispatch alone
does not confirm a reset. OpenCode uses an independent server and currently requires an
explicit `unconfined` sandbox selection. Native approval rules do not provide OS
confinement; constrained OpenCode launches are refused. This development server
has no legacy data importer or production deployment integration.

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
go build -o /tmp/tclaude-backend-agent ./cmd/tclaude-backend-agent
```

Credential-capable providers deliver a protected action credential file to each
execution. They expose its path as `TCLAUDE_BACKEND_CREDENTIAL_FILE` and the API
socket path as `TCLAUDE_BACKEND_SOCKET`. The client reads these paths by default;
`--credential-file` and `--socket` allow explicit paths. It has no automatic
operator-token fallback and never opens the database.

Inside that execution, the commands are:

```bash
/tmp/tclaude-backend-agent whoami
/tmp/tclaude-backend-agent inbox --unread
/tmp/tclaude-backend-agent status
/tmp/tclaude-backend-agent send 'Please review the change' --to AGENT_ID --request-id review-request-1
/tmp/tclaude-backend-agent read MESSAGE_ID --request-id read-message-1
/tmp/tclaude-backend-agent interact EXECUTION_ID 'Continue the task' --request-id continue-1
/tmp/tclaude-backend-agent stop EXECUTION_ID --request-id stop-1
```

`launch` requires an agent ID and `--expected-revision`. `resume` additionally
requires a logical conversation ID and `--expected-association-revision`.
`context EXECUTION_ID` requires `--intent clear|reset`,
`--expected-conversation`, and `--expected-association-revision`. All three
commands require `--request-id`. Read current revisions from identity/status or
an authorized snapshot; stale selections are refused. Available operations are
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
