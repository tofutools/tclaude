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

Effect requests carry a stable `request_id`. Reuse it when checking/retrying the
same request; do not invent a new request ID to replay an uncertain effect.
Operation result codes and states describe acceptance and outcome separately.
Native recovery evidence and raw provider diagnostics are not exposed by the API.

An attachment uses `execution_id` and `request_id` query parameters plus the same
Authorization header. Text and binary WebSocket frames carry terminal bytes.
Closing the attachment disconnects that view without stopping the workload.
Stopping the development server also leaves workloads for later recovery.

Current provider support is explicit: Claude uses a terminal workload and reports
unknown context readiness when no authenticated native observation exists; clear
is unsupported. OpenCode uses an independent server and currently requires an
explicit `unconfined` sandbox selection. Native approval rules do not provide OS
confinement; constrained OpenCode launches are refused. This development server
has no legacy data importer or production deployment integration.

Focused verification:

```bash
go test ./internal/backend/... -race -count=1
go build ./...
```
