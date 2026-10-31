# Federation reference

Contracts for people building on or debugging federation: what the dashboard and peers exchange. For how to use these features, see [Federation](federation.md).

## Peer UI request contract

The receiving daemon's `agentd.PeerViewHandler(instanceID)` accepts requests
from an authenticated federation transport. The transport supplies the pinned
calling instance ID; browser parameters and HTTP headers cannot select it.
This handler is separate from the local dashboard, whose cookie authentication
and response shape remain unchanged. Proxy transport and remote actions are
separate features.

Peer reads carry a `peer_view` object with the peer label, `included` concept
names and `omitted` entries (`feature`, `requires`). These describe capabilities,
never hidden object names or counts. A restricted `/api/snapshot` contains only
visible groups and agents, using roster, presence and status grants to select
fields. Status comes from the same shared cached gather as the local dashboard.
The peer projection uses the normal dashboard snapshot and row types, including
`conv_id`, `state.status`, `state.model`, `state.effort_level`, numeric context
fields and `task_ref_url` / `task_ref_label`. Lists and maps for withheld
concepts are empty, unsupported scalars are blank/zero, and optional private
fields are omitted. Paths, worktrees, permissions, spawn configuration,
account usage and local notification/approval content are not shared through
roster/presence/status grants. The harness spawn catalog is empty for restricted
peers: local provider model suggestions and readiness diagnostics are private
configuration. Group roles require roster access; online fields
require presence or status access. Status granted on one visible group does
not populate an agent's row in another group without that grant. The deduped
`agents` list combines only authorized data and lists only visible groups.
Task URLs have query strings and fragments stripped. A field classification
guard and final deny filter cover snapshot, group, member, agent, task and
state fields; newly added fields default to withheld until classified.

Single-group and single-agent reads use `/api/groups/{name}` and
`/api/agents/{id}` in the same row schema; invisible objects return 404.
Restricted snapshots always send their small registry fields, with
`static_unchanged` false, regardless of a supplied `static_version`. A client
must not reuse registry blobs from a local, different-peer or older-authority
snapshot. Unrestricted peers get the complete dashboard snapshot plus metadata.

`GET /api/harnesses/availability` exposes detailed installed harness paths,
versions and boolean credential presence under the instance-wide
`node.harnesses.read` peer grant. This permission defaults off and is implied
only by unrestricted trust. `node.read`, group grants, and unrelated node
permissions do not imply it. Node profiles and node pools may carry this
unscoped grant; group scopes are rejected.

`GET /api/instance` exposes node metadata under `node.read`. Costs and audit
collections require their own instance grants above. Those grants default off,
are never implied by group access, and can also be supplied by node profiles
or pools. Only unrestricted trust implies them. Without a grant, a collection
read succeeds with no rows and reports the omitted concept in metadata. An
audit grant exposes the entire node's audit collection, including local actions.

`GET /api/pr-checks` and `GET /api/pr-details` back the PR link, CI and review
hover cards. PR links reach a peer only inside the complete snapshot, so these
reads are held only through unrestricted trust; a restricted peer gets 403.

`POST /api/operator-message` accepts `{to, subject, body}`, where `to` is a
visible stable agent ID. It requires `message.direct` in a live group containing
the recipient. A visible recipient without that grant yields 403 naming the
missing permission and visible group. No attachment, agent impersonation or
all-live broadcast is accepted. Mail is persisted with federation sender
identity and the attempt is recorded in `federation audit` as the calling peer's
operator.

Every dashboard route is classified in the peer mapping. A route without a
peer grant of its own serves an unrestricted peer through this node's own
handler and returns the default-deny 403 to any other peer. The routes that
stay with this node (see
[Changing a peer's groups](federation-peers.md#changing-a-peers-groups)), and any unmapped route,
return 403 even for unrestricted peers. Local-only features cannot be enabled by a grant through
this handler; their `omitted.requires` identifies the related federation grant
where one exists, or `local_only`. A route guard test requires an explicit
classification when a dashboard route is added.

## Node summaries and polling

`GET /api/node-summary` is the lightweight map-card endpoint on both the local
dashboard and the authenticated peer dispatcher. It returns:

```json
{
  "presence": "online",
  "shared_groups": 2,
  "shared_agents": 4,
  "online_agents": 3,
  "waiting_for_input": 1,
  "peer_view": {"peer": "laptop", "included": [], "omitted": []}
}
```

Counts describe only identifiable shared agents, deduplicated across visible
groups. Online counts need presence or status permission; attention counts
need status permission and count online `awaiting_input` and
`awaiting_permission` agents (idle agents are not attention). An unrestricted
peer, or the local operator, also counts loose active agents. `resources` and
`health` are optional and require `node.read` for peers. Health is the cached
resource observation status (`current`, `stale`, or `warming`), not an inferred
all-clear for all agents. Host-wide agent totals are removed from resources;
the card uses the authorized shared counts. A successful response means the
receiving daemon is online; transport failure/offline/staleness is marked by
the frontend using its own receive time. Local responses have no `peer_view`.

Summaries reuse the shared status gather and cached host readings; they do not
build full dashboard snapshots or launch resource probes. They return a strong
`ETag` over the filtered response plus `Cache-Control: private, no-cache`.
Clients can send `If-None-Match` to get a bodyless 304 for an unchanged summary.
Trust and grants are reevaluated before the validator, so a scope/permission
change changes the authorized response/ETag rather than retaining old data.
A 304 keeps the previously received body and its metadata. Transport caches
must key representations by receiving node and authenticated calling peer,
never share them across callers, and forward validators through to agentd.
Full snapshots continue to return `Cache-Control: no-store`. A peer's
snapshot also carries an `ETag` over its authorized projection, excluding its
`generated_at` timestamp, so a conditional read of an unchanged snapshot is a
bodyless 304.

Peer snapshot and summary reads share an immutable private status gather for
up to 1.5 seconds, independently of persisted replay markers. Trust, grants,
visibility and response projection are checked on every request, including
conditional summary reads. Local dashboard/CLI status reads still invalidate
immediately on database writes. Disabling the status cache also disables this
peer coalescing window.

Polling contract for the node-switching, map and merged-view frontends:

- A per-node view polls full `/api/snapshot` data only for the displayed node,
  using the existing dashboard cadence. Stop/abort its poll when switching
  away, and discard snapshots from an earlier selected-node generation.
- The map polls `/api/node-summary` only. Poll every 5 seconds per node, stagger initial offsets across nodes and add jitter;
  never trigger a synchronized fleet-wide full snapshot poll.
- The merged regular view polls full snapshots only for nodes whose groups
  are on screen. Suspend full polls for hidden/collapsed-out nodes and use
  summary data when only a map card is being shown.
- Avoid overlapping polls to a node. Back off on failures, stop polling when
  the view is hidden, and distinguish cached/stale data from an offline node.
  Recheck omitted features after every new authorized response.

## Local dashboard Fleet status

`GET /api/federation/status` is the cookie-authenticated local dashboard wrapper
for `GET /v1/federation/status`. Federation administration is local-only;
trusted peers, including unrestricted peers, cannot call this route through
the peer-view dispatcher.

For the Fleet chip row, use `GET /api/federation/status?summary=1`. The same
query also works on the CLI API. It returns the local identity (`instance_id`,
`name`, `fingerprint`), configuration (`enabled`, optional `hub_url`), optional
cached `hub` connection state, and `peers`. Summary peers include only trusted
linked instances, with their stable `instance_id`, `label`, `name`, trust
`level`, `trusted`, `online`, and `last_seen` fields (plus fingerprint, version,
and trust time). Instances that sent a trust request are always listed. Add `&untrusted=1` to
also list the other hub-visible instances not trusted here (`trusted: false`, with `instance_id`, `name`, `fingerprint`,
`online`, `last_seen` and `version`), as the map's Show untrusted view does.
An empty peer list is `[]`. Presence comes from the cached
hub directory; no remote requests or status probes run during this read.

Summary responses omit `peer_grants`, `outbox`, and `remote`, and avoid loading
those records or remote catalogs. The full status response still includes
hub-visible untrusted instances and the administration fields. Responses are
private and uncached. Poll this local list at a relaxed interval, pause when
the dashboard is hidden, and poll each visible remote map card separately.

The rest of the local Fleet administration API mirrors the CLI API: replace
`/v1/federation/` with `/api/federation/`, retaining the HTTP method, JSON
request/response, query parameters, and path variables. Every wrapper checks
the local dashboard session before calling the shared handler as the local
human. Peer-view transport refuses all of these administration routes,
including for unrestricted peers. `HEAD` reads on enrollment-token and profile
GET routes preserve read-only semantics on both API surfaces.

| Method | Tail under `/api/federation/` | Existing `tclaude federation` CLI |
| --- | --- | --- |
| GET | `status`, `audit` | `status`, `audit` |
| POST | `config` | `connect`, `disconnect` |
| GET / POST | `enroll-tokens` | `enroll-token ls`, `enroll-token create` |
| POST | `enroll-tokens/{id}/revoke` | `enroll-token revoke` |
| GET | `enrollments` | `enrollments` |
| POST | `enroll/preview`, `enroll` | `enroll --preview`, `enroll` |
| POST | `peers/trust`, `peers/untrust` | `trust`, `untrust` |
| GET / POST / DELETE | `grants` | `grants`, `grant`, `revoke` |
| GET / POST | `profiles` | `profile ls`, `profile create` |
| GET / PUT / DELETE | `profiles/{name}` | `profile show`, `profile update`, `profile rm` |
| POST | `profiles/{name}/apply` | `profile apply` |
| PUT | `default-peer-profile` | `profile default` |
| GET / POST | `nodes/groups` | `nodes groups ls`, `nodes groups create` |
| DELETE | `nodes/groups/{name}` | `nodes groups rm` |
| POST / DELETE | `nodes/groups/{name}/members` | `nodes groups add`, `nodes groups rm` |

Important shared request shapes for dashboard clients:

- Trust uses `instance`, optional `label`, `level`, `profile`,
  `no_default_profile`, `preview`, `preview_token`, and
  `confirm_fingerprint`. Changing to unrestricted requires the exact displayed
  fingerprint; selecting a profile preserves the CLI preview/apply contract.
  Untrust uses `instance`.
- Grants use `peer`, `slug`, optional `scope` and `spawn_policy`. Read a peer's
  grants with `?peer=<instance_id>`; a local pool selector is `group:<name>`.
- Profiles are `{name, revision, definition}` with the existing definition
  schema. Updates must send the current revision. Applying a profile uses
  `{peer, apply, preview_token, confirm_fingerprint}`: first preview with
  `apply: false`, then commit using the returned token. The default-profile
  request is `{profile}` (an empty string clears it).
- Pool creation uses `{name}`; adding or removing a member uses `{peer}`.
- Enrollment token creation uses `{profile, uses, ttl_seconds, trust_level}`.
  The bearer is returned only at creation; token listings expose public
  metadata. Revoke by public token ID. Joining uses `{master, token}` for
  preview, then adds `preview_token` for enrollment. Responses preserve the
  existing consent and fingerprint fields.

Full status exposes hub-visible untrusted instances for the trust screen.
Trusting one of these instances pins its identity through the existing trust
operation. Trust requests are kept in memory. Summary status lists linked trusted
peers for the chip row, plus instances that asked to be trusted.

## Dashboard peer proxy

The browser uses its local dashboard session for
`/api/peer/{instance_id}/<same tail>`: for example,
`GET /api/peer/inst_.../snapshot` or
`GET /api/peer/inst_.../node-summary`. Select peers by their stable pinned
instance ID from the local Fleet status response; labels are display names.
The local daemon opens a single encrypted federation stream to the selected
instance, which serves the request through its peer-view dispatcher using
the authenticated sender's instance ID. Browser cookies, authorization tokens,
and caller identity headers are removed before transport. No local-human
wrapper runs on behalf of a peer.

Every dashboard route is classified for peer use: reads such as `snapshot`,
`groups/{group}`, `agents/{agent}`, `instance`, `costs`, `audit` and `node-summary`,
and `POST operator-message`, which uses the same scoped messaging permission as a
direct peer-view request. An unrestricted peer reaches nearly all routes, and anything
unmapped is refused by the receiving dispatcher. Nested peer proxies and
Fleet administration remain local-only. `If-None-Match` and `ETag` pass through, including bodyless 304 responses.
The local proxy keeps its private/no-store cache policy. Errors such as a
receiving node's 403 or a hidden agent's 404 retain their status and JSON body.
Responses are validated as JSON and served with a fixed JSON content type,
`nosniff`, and a sandbox CSP. A peer cannot publish executable content under
the local dashboard origin.

An unknown or no longer trusted selector returns 403 with `code: not_trusted`.
Unavailable peers return 502 with `code: peer_unreachable` and
`reason: peer_offline`; deadlines return 504 with the same code and
`reason: peer_timeout`. These errors include `last_seen` when the cached hub
directory has an observation. Concurrency saturation returns 503 with
`code: peer_busy`. Requests have a 15-second deadline, bounded JSON bodies,
and bounded concurrent streams. The proxy does not retry or fan out requests.
Clients should keep their last successful view, mark it stale, and back off.

A dashboard poll of `snapshot` or `node-summary` may declare its interval in
`X-Tclaude-Poll-Ms`. The local daemon then reads through one copy per peer and
URL, shared by every dashboard and tab: polls within a second of the last read
share it (concurrent ones wait for the read in flight), later ones ask the peer
again with `If-None-Match`, so an unchanged peer answers without a body.
Nothing runs between polls, and a copy nobody polls for two minutes is dropped.
The served copy carries `Age` in seconds; the browser's own `If-None-Match` is
answered locally. A failed read is served as the same error a direct request
would get. This node's catalog broadcasts (trust, grants, group shares) expire
every copy and a peer's inbound catalog expires its own, so the next poll asks
again; trust is still checked on every request.
Requests without the header are proxied as one exchange each, as above.

The stable `peer_view.included` / `peer_view.omitted[].feature` concept keys are
`agents.status`, `groups`, `groups.roster`, `groups.presence`, `messaging`,
`node.summary`, `health`, `costs`, `audit`, `terminals`, `spawn` (a grant that
launches automatically), `spawn.request` (any visible group takes a request), and
`local_dashboard`, `node.harnesses`, and `node.exec`. `local_dashboard` covers local administration, registries,
and lifecycle controls. An omitted concept carries a `requires` permission or
`local_only`. Clients must tolerate additive concept keys; absence of an
omission is not a grant for an unknown endpoint.

Poll full snapshots only for nodes whose regular dashboard views are visible.
Map cards use relaxed, staggered summary polls. Merged views poll the nodes
that contribute visible groups. Pause hidden dashboards, avoid overlapping
requests to one node, and back off on failure.

Remote browser terminals use the local-only dashboard WebSocket
`GET /api/federation/terminal?peer=<instance_id>&agent=<agent_id>&mode=watch|interactive`.
The local dashboard cookie and origin checks protect the browser endpoint;
peers cannot call it or chain an attach through another node. It opens the same
encrypted, incarnation-pinned terminal stream as CLI federation attach, with
unchanged target-side watch/attach grants, indicators, viewer limits and audit.
`GET /api/federation/sessions?peer=<instance_id>` returns the existing CLI session
array, adding `watch`, `attach` and `takeover` flags; `instance` is the stable peer ID and
`peer` its display label. Cached catalogs may be stale; the target authorizes
both initial opens and every ongoing session.

The socket sends a JSON `hello` first (`mode`, `peer`, `agent`, `cols`, `rows`,
`shared`, `others`, `take_over`), then binary output. Its dimensions come from
the target pane, and later JSON `size` frames report the grid to render. When
`shared` is true, the browser's `resize` messages report the size its panel
could show and count toward the shared size; JSON `others` frames update what
else is attached. Against an older peer `shared` is false and `resize` messages
are dropped. `take_over` says the peer grants `sessions.takeover`; adding
`&take_over=1` to the socket path then asks the target to disconnect the other
clients first, and is refused with `denied` otherwise. The CLI attach socket
opts into the same sharing with `share_size=1` and gets the same hello.
Interactive binary input passes through the existing fixed-key/literal-text
encoder; watch input is discarded. After xterm consumes each output frame,
the browser sends `{"type":"credit","bytes":N}` with its exact byte count.
Credits cannot exceed outstanding output, keeping the 256 KiB window bounded
through the browser renderer. Operational refusals use a hello-less JSON
`closed` frame after upgrade; authentication/origin failures remain HTTP errors.
Established sockets close with a reason: `exit`, `reincarnated`, `revoked`,
`untrusted`, `kicked`, `taken_over`, `offline`, or `error`; initial opens also use `denied` and
`limit`. A stalled connection can be aborted without delaying viewer cleanup.
Detach closes the viewer and never stops the agent.

File paste and drop use local-only `POST /api/federation/terminal-attachments?terminal=`
with the encoded remote socket path and the same multipart `file` fields and
JSON response as local terminal uploads. It requires interactive attach, which
already types freely into the agent, so any file type is accepted. Bodies
travel over the shared authenticated bundle stream, not control frames.
The receiver validates the pinned attach authority during transfer and before
and after staging, verifies length/digest/FIN, then applies the existing
file/count/total caps. An older receiver still accepts PNG/JPEG/WebP only and
refuses other files with `415`; the dashboard shows that refusal with a hint
that the remote node may need a newer tclaude. It returns paths
on the target, including its existing sandbox attachment root where needed.
Remote `/api/terminal-file` downloads remain refused with `403 not_shared`. Download
through `/api/federation/terminal-file`, which needs `sessions.files.read`.

## Remote operator actions

Remote actions use the peer-view dispatcher and the existing dashboard proxy.
The receiver authorizes every request and rechecks durable move authority before
retiring a source. It never gives the requester local human identity or the
source agent's own grants. Whole-agent stop/retire/clone/move actions need their
peer grant on every affected active group; clone/retire/move also cover owned groups.

- `POST /api/groups/{name}/spawn`: `{brief, name?, role?, profile?, cwd?,
  harness?, model?, effort?, roles?, sandbox_profile?, launch?}` into any visible group (`spawn.request`). With
  `groups.members.spawn` it launches automatically under receiver-owned launch
  policy; without it the request waits for the operator. Every launch
  setting, `launch`'s keys included, is accepted only where the peer may
  choose it (see
  [Launch settings a peer may choose](federation-work.md#launch-settings-a-peer-may-choose)); any other field or
  launch key is refused. With `groups.members.spawn.worktree`,
  `GET`/`POST /api/worktrees` and `GET /api/worktrees/progress` serve the
  dialog's worktree list and creation (also needing `.cwd`); listing a
  directory shows its repositories, worktrees and branches, and a new
  worktree goes to this node's default location (a peer cannot name a
  `path`). Other worktree routes stay local-only. Returns a durable spawn-request row
  (202); `GET /api/spawn-requests/{id}` reads only that initiating peer's
  request while the group stays visible to it.
- `GET /api/spawn-options/{name}`: what the peer may set for a spawn into a
  visible group: `{group, approval: "automatic"|"operator", overrides:
  {cwd, harness, model, effort, profile, roles, sandbox, permissions,
  environment, owner, features, prompts, worktree}, profiles, harnesses?,
  defaults?, catalog}`.
  The dashboard's spawn dialog reads it to disable what the peer may not set;
  `catalog` says the peer holds `node.spawn.read`, and only then are
  `defaults` named.
- `node.spawn.read`, read-only (other methods on these paths stay
  local-only): `GET /api/spawn-profiles`, `/api/spawn-profile-default`,
  `/api/spawn-launch-defaults`, `/api/saved-prompts`, `/api/roles`,
  `/api/sandbox-profiles`, `/api/sandbox-profile-default`,
  `/api/spawn-harness-policy`, `/api/claude-settings/default-model`,
  `/api/spawn/effective-sandbox`, `/api/spawn-launch-catalog` (the harness
  and sandbox implementation catalogs), `POST /api/sandbox-profile-enforcement` (a
  preview that writes nothing), and, for a visible group,
  `GET /api/groups/{group}/sandbox-profile` and
  `/api/groups/{name}/spawn-harness-policy`. A group named in a query or
  body must be visible too; the enforcement preview treats the draft as a new
  profile; and `effective-sandbox`'s `dir` (a project-settings walk) is
  limited for a restricted peer to a visible group's default or pinned
  directory. With `catalog`, spawn-options also names the group's default
  profile (`defaults.group_profile`) and `default_prompts`.
- `POST /api/agents/{id}/stop[?force=1]`: `groups.members.stop`.
- `POST /api/agents/{id}/resume`: `groups.members.resume`. No options:
  recreating a missing launch directory and switching a Codex drive stay
  with the local operator.
- `POST /api/agents/{id}/restart`: `groups.members.resume` plus
  `groups.members.stop`; refused unless the agent is fully idle.
- `POST /api/agents/{id}/sandbox-restart`: `{action: "restore"|"unlock"}`
  under the same grants. `unlock` restarts the agent with its sandbox off and
  is accepted only from an unrestricted peer.
- `POST /api/agents/{id}/retire`: `groups.members.retire`; worktree deletion
  and its options are accepted only from an unrestricted peer.
- `POST /api/agents/{id}/clone`: `groups.members.clone`, with optional
  `{follow_up, no_copy_conv}` and receiver-owned source defaults.
- `POST /api/agents/{id}/move`: `{group}`; `agent.move` plus source
  `groups.members.retire`. The destination is the requesting peer, except
  that an unrestricted peer may add `to` (another trusted node's instance ID).
  The move then runs as this node's own operator move to that node, with
  `group` naming the receiving group there. The fused dashboard view uses this
  for a drop between two peers.
- `POST /api/agents/{id}/teleport`: `{group, note?, clone?, home?, carry_permissions?}`
  with the same source authority and existing teleport landing/confirmation policy.
  A `home: true` recall is accepted only when the requesting node is the
  agent's recorded home; the normal return-home policy applies.
- `POST /api/operator-message` retains the existing scoped messaging contract.

IDs are stable remote agent IDs, and `group` on move/teleport names the
receiving group on the requesting node. No other third-node delegation is accepted.
Use `/api/peer/{instance_id}/<tail>` or the CLI's
`/v1/federation/peer/{node}/<tail>` proxy. The CLI equivalent is
`tclaude federation action ACTION --node PEER --agent ID` (ACTION includes
`resume`, `restart` and `sandbox-restart --sandbox unlock|restore`), adding
`--group GROUP --brief TEXT` for spawn, `--group GROUP` for move/teleport,
or `spawn-status --job ID` to inspect a remote launch. Message uses
`--body TEXT [--subject TEXT]`; clone uses `--follow-up`/`--no-copy-conv`.
The action command never retries mutations automatically.

## Local dashboard bundle offers

Local cookie-authenticated routes mirror the offer CLI handlers:
`POST /api/federation/offer-config` accepts `{peer,bundle?,only?,skip?,allow_flagged?}`;
`POST /api/federation/share-agent` accepts `{agent,peer,group,history?,allow_flagged?}`;
`POST /api/federation/profiles/{name}/offer` accepts `{peer}` after the profile
revision has been applied. `GET /api/federation/bundle-offers?direction=in|out`
returns offer rows. POST `/{id}/fetch`, `/{id}/import` and `/{id}/decline` act on
an incoming offer; add `?peer=INSTANCE_ID` to disambiguate its source.
Import defaults to preview. Its body preserves the CLI fields: `apply`, `only`,
`skip`, `replace`, `values`, `keep_paths`; agent offers also accept `cwd`,
`worktree`, `group`, `name`, `skip_history` and `set`. Apply is explicit and uses
the same live admission, credential findings, path and launch checks as the CLI.
Responses retain the existing preview diffs and provenance. Every route is
local operator administration and is refused through peer views.

After fetching, `state: "ready"` means the private payload is verified.
`GET /api/federation/bundle-offers/{id}/contents?peer=INSTANCE_ID` returns
`{type,entries:[{path,size,kind}]}`. Agent entries are `manifest.json` and optional
`history/transcript.jsonl`, or for an agent PAF its `manifest.json` and every
entry that manifest lists; config entries are `bundle.json` and
`sections/<section>.json`. Entries are data labels, never host paths.
Add `path`, optional byte `offset` and `max_bytes` to read one entry:
`{path,kind,size,text,truncated,offset,next_offset?}`. Reads default to 256 KiB
and accept at most 1 MiB; invalid UTF-8 is replaced. `next_offset` is the next
byte offset when more text remains. Entries are JSON or JSONL, except PAF
attachments, which have kind `file`.
`GET` or `HEAD` `/{id}/download?peer=INSTANCE_ID` returns the original payload
as an attachment with octet-stream, sandbox, nosniff and private/no-store
headers. Neither read fetches implicitly: pending returns 409 `not_fetched`,
expired/declined/applied returns 410 `offer_state`, unknown entry paths return
404 `path`, and revoked receiving admission returns 403 `admission`.

The matching CLI commands use the same human-only `/v1` handlers:
`federation offers contents ID --peer PEER` lists entries; add
`--path history/transcript.jsonl --offset 0 --max-bytes 262144` to read text.
`federation offers download ID OUTPUT --peer PEER` saves the original verified
bundle without importing and refuses an existing output file. Both require a
ready offer and preserve current trust/receive admission.

## Local dashboard labels and hub setup

`GET /api/federation/node-labels` returns `{labels:[...]}`; POST accepts
`{add:[...],remove:[...]}` and shares the CLI's label validation and normalized
label set. GET/HEAD are reads. These cookie-authenticated routes are local-only.
Hub setup already uses `POST /api/federation/config` with optional
`enabled`, `hub_url`, `name`, `invite` and `hub_ca_file` fields, and
`POST /api/federation/join` for a join code (see [Joining](federation-peers.md#joining)). Omitted fields
are preserved; CA file paths refer to this node's filesystem. URL validation,
configuration writes and client reload are shared with `federation connect`.

## Local dashboard jobs, repositories and viewers

Local cookie-authenticated `/api/federation/jobs` mirrors the CLI:
POST submits `{peer|node|nodes,repo,ref,group,harness?,command,timeout_seconds,
require?,prefer?}`, GET returns `{jobs:[...]}`, and GET `/{id}` returns the durable
job row. POST `/{id}/cancel`, `/approve`, `/retry` and `/acknowledge-stopped`
share the CLI handlers; the last requires `{acknowledge_stopped:true}` and a
confirmed stopped, uncertain local job. GET `/{id}/logs` returns completed output
JSON. GET `/{id}/follow` streams `application/vnd.tclaude.job-frames` using the
CLI frame contract. Job actions use this local route with an explicit trusted
peer selection, including when the dashboard is displaying a remote node.

GET `/api/federation/repos` returns `{repos:[...]}`; POST accepts
`{name,url,clone,groups:[...]}`, PUT `/{name}` also carries the current `revision`,
and DELETE `/{name}` disables it. Repository aliases, inspected canonical local
paths, receiving groups and revisions retain the CLI validation and checks.
GET/HEAD list requests never mutate the allowlist. Each repository row also carries
`group_names`, the current names in `definition.groups` order, omitting deleted
groups while preserving the relative order of surviving names. Create and update
responses carry this field too; the stored group IDs remain the authority.

GET `/api/federation/viewers?session=...` lists incoming terminal viewer rows
(`id`, `peer`, `agent`, `session`, `group`, `read_only`, `started`, `incoming`).
The optional session filter matches an agent ID or session name. POST
`/viewers/{id}/kick` disconnects the named incoming viewer and returns `{ok:true}`.
All these routes are local operator administration and are refused through peer views.

## Local dashboard away administration

`GET /api/federation/away` reads `{away: null|{cover,since,until}}`;
POST the same path with `{cover,until?}` selects a trusted covering operator and
returns `{away,warnings}`. `POST /api/federation/return` ends forwarding.
`POST /api/federation/answer` sends `{ticket,decision:"approve"|"deny"}` using
the exact ticket supplied in the covering notice, returning `{envelope_id,state}`.
These local cookie-authenticated routes share the CLI's `/v1/federation` handlers.
A delegated answer is one-shot and never grants persistent permission. Peer
access requests remain decidable only by the receiving local operator and are
never forwarded to an away cover. None of these administration routes is peer-viewable.

## Dashboard incremental job output and identity transitions

The local dashboard can poll `GET /api/federation/jobs/{id}/output` for a
submitted outbound job. The corresponding CLI API is
`GET /v1/federation/jobs/{id}/output`; CLI live follow remains available.
Both use the existing job ownership authorization. These dashboard routes
are local-only, including for unrestricted peers.

Output accepts an opaque `cursor` (omit it to start at the beginning) and
`max_bytes`, default 65536, range 32768–262144. Replies contain `chunks`
(`{stream:"stdout"|"stderr",data:string,encoding:"base64"}`), the next `cursor`, `state`,
`done`, and optionally `truncated` when more output remains. Each read is
bounded and closes its authenticated stream; it does not wait for future
output. Decode base64 into bytes and use a separate streaming UTF-8 decoder per channel
across polls, preserving characters split between frames. `max_bytes` counts
original bytes. Poll only while the output panel is visible. When `done` becomes
true, fetch the verified terminal `/logs` result. Inbound or unstarted jobs
return 409, as do peers without incremental-output support. A simultaneous
CLI follow may occupy the same per-job stream slot.

Trusted rows in `/api/federation/status`, including `?summary=1`, carry
`identity_transition` while a signed identity successor is `pending` or
`conflict`. Its fields are `state`, `old_id`, `new_id`, `new_fingerprint`,
`received_at`, `accept_after`, and an optional `reason`. Pending means the
local detection window has started; `accept_after` is the earliest possible
acceptance, not confirmation that acceptance succeeded. Conflict means
competing signed successors require explicit operator recovery. Completed
history is available through the read-only, human-only
`GET /api/federation/identity/rotations`, mirroring `/v1`.

`tclaude federation identity recover-peer OLD_ID NEW_ID` previews recovery without
changing authority. Applying recovery requires independently checking the
replacement fingerprint with that node's operator, then passing
`--fingerprint NEW_FINGERPRINT --apply`. The dashboard exposes no identity
recovery actions.

The local dashboard also supports identity rotation through
`POST /api/federation/identity/rotate`, using `{}` for preview and
`{"apply":true}` after operator confirmation. This mirrors the human-only CLI
handler and is refused on every peer path. Preview includes the current
identity and fingerprint, `window_seconds`, `hop_count`, `hop_limit`, `pending`,
and consequence flags in `effects`. It generates no successor key. Apply
returns the signed rotation plus `old_fingerprint` and `new_fingerprint`;
the same fingerprint fields appear on `local.chain` in the rotations read.
A pending rotation refuses another apply, and four retained hops refuse a
fifth rotation until explicit re-pairing. Recovery and old-key revocation
remain CLI-only.
