# Move agents and config

Share, move and teleport agents between nodes, and offer configuration.

## Sharing an agent with a peer

Agent offers use the same inbox, spool and transfer protocol as config offers.
On the receiving machine, allow a peer to offer agents into a particular group:

```bash
tclaude federation grant alice agents.receive --scope group=reviewers
```

`agents.receive` requires an existing group scope. It advertises that receiving
group without exposing its members or transcripts. An offer is pinned to the
group's identity: renaming preserves it, while deleting and recreating the same
name does not retarget it. Unrestricted peers may offer into active groups, but
receipt never launches an agent automatically.

On the sending machine:

```bash
tclaude federation share-agent reviewer bob --group reviewers
tclaude federation share-agent reviewer bob --group reviewers --history
```

The default shares configuration only. `--history` includes the native
conversation when the harness supports `HistoryTransfer` (currently Claude
Code, Codex, OpenCode, Copilot, Gemini and pi); other harnesses warn and share configuration only. The original
agent and transcript remain in place. Credential findings report counts and at
most three locations per kind; sharing refuses unless `--allow-flagged` is
explicit. Structured credential fields are omitted and history stays verbatim.
Each instance advertises the PAF versions it reads in its catalog
(`paf_versions`). A peer that shares a PAF version with this node receives
the offer as a PAF in the newest version both read, signed with this node's
federation key; any other peer receives agent bundle version 1.
Move and teleport follow the same rule. The neutral copy of the conversation
is left out of the PAF when the native history is large (over 256 MiB or a
third of `federation.agent_transfer_max_bytes`) or would not fit the
transfer's limits; the native history always travels.

Agent callers need `agent.share` with `peer=bob` or
`peer=bob/reviewers` scope. They may share themselves using `self`; sharing
another agent additionally requires `agent.bundle.export`. An unscoped grant
confers no remote authority. Receiving, previewing and applying offers remain
operator actions.

On the receiver:

```bash
tclaude federation offers
tclaude federation offers import <id> --cwd "$PWD"
tclaude federation offers import <id> --cwd "$PWD" --name received-reviewer --apply
```

Import goes through `agent bundle import` and normal spawn checks, creating a
fresh local agent. The offered group is the default; `--group` can choose
another active group that grants this sender `agents.receive`. Trust and the
original receiving grant must remain valid. Preview shows provenance, the
chosen group, paths, history capability, credential findings and launch
security posture. Source permissions, ownership, roles and profile names are
advisory and never become grants on the receiver.

Use `--skip-history` for configuration only. `--cwd`, `--worktree`, `--keep-paths`
and repeatable `--set name=value` retain the local agent-bundle import meanings.
Configuration selectors (`--only`, `--skip`, `--replace`) do not apply to agent
offers. Missing paths or unresolved placeholders refuse apply. Group identity
and admission are rechecked after transfer and immediately before import.

Agent archives and uncompressed native history default to a 2 GiB limit, with
 ten active offers and at most twice that limit of archived payload per peer
 per direction. Set `federation.agent_transfer_max_bytes` in the node's
 `~/.tclaude/data/config.json` to raise it (both the source and receiver enforce their
 own limit). The human-only federation config API accepts
 `{"agent_transfer_max_bytes":4294967296}` for a 4 GiB limit; zero restores the
 default. Individual JSONL records default to 256 MiB, including image and tool
 output records; `federation.agent_history_record_max_bytes` can raise that
 limit too. Over-limit exports fail before the source is paused or retired,
 naming the setting to change.

 Agent archives and history are streamed through private files. Updated peers
 fetch 16 MiB chunks, verify each chunk's SHA-256 and the final archive digest,
 and retain completed chunks across interrupted fetches and daemon restarts.
 Retrying fetch resumes at the verified prefix. A chunk uses an idle timeout,
 rather than a five-minute total-transfer deadline; the offer still expires
 after 72 hours. Older peers retain the existing single-stream protocol.
 Move status includes `transfer.{bytes_done,bytes_total,state}` as verified
 chunks arrive, where state is `transferring` or `downloaded`. The shared 72-hour expiry,
inline threshold, verification and decline behavior apply. If an apply is
interrupted after dispatching a launch, its reserved agent ID stays visible;
inspect that identity before requesting another offer. Pre-launch failures can
be corrected and retried. Declining or expiring an offer discards transfer data;
it does not stop an already dispatched agent.

### Moving an agent

A move clones native conversation history to a new destination agent, then
retires the source after the destination confirms that its reserved launch is
running. Claude, Codex, OpenCode, Copilot, Gemini and pi history are supported. A peer must advertise move
support; an older peer cannot complete a move with an ordinary import receipt.

```bash
tclaude federation move-agent worker peer-name --group receiving-group
# On the receiving instance, preview and explicitly import as usual:
tclaude federation offers import OFFER_ID --cwd /local/project
tclaude federation offers import OFFER_ID --cwd /local/project --apply
# On the source instance:
tclaude federation moves ls
tclaude federation moves show OFFER_ID
tclaude federation moves abandon OFFER_ID
```

The local dashboard uses the same responses at `GET /api/federation/moves`,
`GET /api/federation/moves/{id}` and `POST /api/federation/moves/{id}/abandon`.
These cookie-authenticated routes are local operator administration and are
refused through peer views.

Moves require history; `--skip-history` is refused. Paths, group membership and
permissions follow the agent-offer import rules. The receiving agent has a new
agent ID and conversation ID, with a durable `moved_from` link containing the
source instance, agent and offer. The source retains a `moved_to` tombstone.
Both links are visible in operator-only move status.

An offer captures the history at the time it is sent. The source remains active
while the receiver reviews and launches the clone; later source history and
pending mail are retained locally. No continuous synchronization takes place.
Agent callers need `agent.move` scoped to the destination peer and ordinary
retire authority for the source. Exporting another agent also needs
`agent.bundle.export`. Move and retire authority are checked again before
retirement; a revoked grant leaves the move blocked and the source intact.

Outgoing state advances from `awaiting_confirmation` to `confirmed`, `retiring`
and `moved`. An import's ordinary `applied` receipt does not retire the source.
The dedicated confirmation binds the archive digest and both identities to a
verified live destination launch; selected Codex app-server launches must also
be ready. A confirmation older than five minutes cannot authorize retirement.
Restart recovery retains launch and retirement progress without creating another
clone. If the source rotates to a new conversation while waiting, retirement is
blocked; abandon the old move and offer the current generation.

Declined, expired and abandoned moves leave the source intact. Abandon is
idempotent and ignores later confirmations. Once retirement starts it cannot be
abandoned. An independently created destination clone remains running after
abandon. Successful retirement stops the source and removes its normal agent
authority while retaining its conversation and worktree.

For fresh-ID moves and older peers, mail to the retired source is refused with
`agent_moved` for senders still authorized through its former group's current peer
mail grant. The refusal does not disclose the new address. Other senders receive
the usual unknown-agent or authorization refusal. Stable-ID moves between peers
with home-mail support instead use the home custody route described below.

## Direct operator moves

Dashboard moves may request `direct_if_allowed: true` on
`POST /api/federation/move-agent` (the `/v1` route accepts the same body).
The receiver decides whether to launch automatically: unrestricted trust needs
an available landing directory; a restricted peer needs `agents.receive` for
the receiving group and an applied landing policy. Explicit directory or candidate hints from restricted senders are ignored on
the automatic path; the receiver uses its own landing resolution.
Automatic moves otherwise use the same repository, source-path, group-default
and policy fallback order as offer imports. Permission and policy are checked
again before launch. If automatic landing is unavailable, the offer remains
available for the receiving operator to inspect and accept.

The initial response includes `move_id` and `disposition: "checking"`.
Poll `GET /api/federation/moves/{move_id}` for `pending_acceptance`, or `landed`
with `target_agent` and `cwd`. Existing move state and error fields describe
refusal, expiry or abandonment. The source stays active until the receiver
confirms that its successor is running. Receivers without direct-move support
continue to receive ordinary pending offers. Pulling an agent uses the existing
peer action endpoint and always moves toward the requester; moves between two
other nodes must be issued from the source node's dashboard.

Direct operator moves remain ordinary moves: the self-service teleport freeze
and per-chain/hour/day teleport limits do not apply. Node capacity, admission
and the applied policy's live-worker limit still apply.

## Teleport yourself to another node

An agent with `self.teleport` scoped to a trusted peer can run:

```bash
tclaude agent teleport laptop --group helpers --note "Continue investigating the failing test"
tclaude agent teleport group:debuggers --group helpers --clone --credentials local
tclaude agent teleport --node auto --require 'os=linux' --group helpers
tclaude agent teleport status
tclaude agent teleport --home --group helpers
```

Teleport transfers native Claude Code, Codex, OpenCode, Copilot, Gemini or pi conversation history using the
existing agent-offer transport. Move mode keeps the source running until the
receiver confirms that the imported generation is running. `--clone` keeps the
source running. There is no destination retry after an offer might have been
delivered. Selection failures include candidate reasons; missing or stale node
metrics exclude a candidate. Automatic selection also needs peer-scoped
`node.read` authority. Agent-visible candidate information follows that scope.

When both nodes advertise stable agent identity, move and teleport keep the
`agent_id`; each arrival still remints the harness conversation ID. Clone keeps
its existing fresh-ID behavior. Older peers fall back to fresh IDs, and the
arrival briefing names which mode was used.

The local dashboard operator can teleport an active agent with
`POST /api/agents/{conv-or-agent_id}/teleport` (the local CLI/API equivalent is
`POST /v1/agents/{conv-or-agent_id}/teleport`). The body accepts
`{peer?, group?, note?, clone?, home?, carry_permissions?}`; `home` excludes
`peer`. This retains the human actor and does not borrow the agent's
`self.teleport` grants. Freeze, quotas, receiver landing authority and credential
scanning still apply. Export starts immediately, and the source remains active
until the destination-running confirmation; teleport does not wait for idle.
Local progress is available at `GET /api/federation/moves/{move_id}`.
Recall from home can use `POST /api/federation/agents/{agent_id}/recall`, with an
optional `group` (defaulting to a retained home group) and `note`. It requests
`home: true` from the current host. The returned `move_id` belongs to that host;
home watches its placeholder's presence become `here` for completion, since
remote move administration remains unavailable.
For a visitor with a retained paused backup, Send home and Recall instead use
the durable report-return path and respond with HTTP 202
`{state, return_id, offer}`. The home group is not a new landing choice in this
case: the original backup and memberships resume. Home still watches presence
become `here`.

The home record becomes an inactive **away** placeholder in its groups. Its
memberships, ownership, permanent permissions, scheduled intent and inbox remain
at home; they confer no authority on the visiting node. Ordinary visiting
copies lose their local grants when they depart. An explicitly retained paused
backup keeps its dormant local state until recovery or deletion. Returning with
`teleport --home` goes directly home from any hop and restores the original home
record with the new history. The receiver still controls admission and landing.

A private, single-use continuation travels in the authenticated bundle. Trusted
intermediate hosts can see it. Returning or recovering a paused backup invalidates
that departure's continuation; every subsequent departure gets a new one. Live
or unrelated ID collisions refuse, and explicitly retiring or deleting an away
home record permanently refuses later return under that ID. Home records each confirmed arrival when all participating nodes support home-routed
mail. The placeholder and Recall use the latest confirmed host; location updates
retry while home is offline. A multi-hop return is accepted from any trusted host
holding the valid continuation. Recall still requires the existing peer move grants.
Older peers retain first-hop location and bounce behavior.
A superseded roaming copy retained as an independent clone gets a fresh ID.

Every receiving import, move or teleport starts with a short factual arrival
briefing: identity mode, receiver-selected cwd and reason, local Git state,
source-reported facts, and what was not transferred. Missing facts stay unknown.
The briefing uses startup context or inbox, never raw pane keystrokes.

Mail for an away stable agent is held durably at home and forwarded directly to
the confirmed current host. An old host hands an envelope to home, preserving
its original sender, rather than relaying along the travel chain. If home is
untrusted or lacks the capability, the old host bounces with the home address.
Offline trusted nodes retry until the original mail TTL expires; this does not
provide a route around an offline home. Home rechecks the sender's live mail and
attachment grants, and the current host authorizes delivery against home. Removing
the agent's home group membership revokes that group's ingress authority.

Regular custody has the ordinary per-recipient queue cap and mail TTL; daemon
notes retain their existing queue policy. Final outcomes
reach the original sender; a custody acknowledgement is not delivery. Delivery
markers travel in stable bundles, and inbound dispatch is fenced during departure,
so a lost acknowledgement followed by another hop cannot duplicate the inbox
message. Forwarding supports only mail to that stable continuation, never generic
commands or traffic addressed to other recipients. Local direct sends, per-recipient
group broadcast copies, operator messages and daemon notes addressed to the away
agent also enter home custody, using normal local sender authority. The sender
sees “forwarded to <node>” until the final host acknowledges delivery. Clones keep
independent inbox identities.

Credentials and uncommitted working-tree changes are not transferred. Permission carry remains an explicit departure
opt-in with receiver-controlled admission; returning home preserves home grants.

Automatic landing requires a group-scoped peer grant `agents.teleport.receive`
and an applied node profile with `teleport_landing`. Its `group` and
`spawn_profile` resolve to immutable local IDs; `cwd` is an existing receiver
path and `max_live` bounds live plus reserved teleport workers from that peer.
The profile's existing `worker_permissions` are installed before launch.
Receiver-owned launch settings must match the history's harness. Changes or
revocation before dispatch stop automatic launch. Without automatic landing,
`agents.receive` can admit the usual pending offer for an operator to preview and
accept. Neither permission is granted by default. Unrestricted peers hold peer
slugs implicitly, but still need a landing policy for automatic teleport.

An example landing block in the receiver's node profile is:

```json
{
  "teleport_landing": {
    "group": "helpers",
    "cwd": "/work/project",
    "spawn_profile": "claude-worker",
    "max_live": 2,
    "repo": "project",
    "credentials_default": "local",
    "credentials_allowed": ["local"]
  }
}
```

`--git-ref main` uses this policy's optional `repo`, an entry in
`federation repos`, to prepare a private, detached checkout through the same
allowlist and Git isolation as remote jobs. Its immutable ID, revision, enabled
state, clone identity and receiving groups are rechecked before launch. The
preview names the repo/ref and the result records the resolved commit. A pinned
full SHA can use the shared checkout's verified local-object fallback; branch
refs never fall back to stale objects. The operator clone is not modified.
Successful checkouts are retained because an agent's native history can resume
there; clean them only after confirming that no running or resumable agent needs
the directory. Without `--git-ref`, landing uses the policy's `cwd`.

`--credentials local|proxy:<name>@<peer>` chooses a credential mode. Resolution
uses an explicit flag, then `teleport_landing.credentials_default`, then `local`.
`credentials_allowed` is a simple exact allowed list (default `["local"]`). Local
mode uses provider credentials already on the receiver. Proxy mode uses the named gateway through the Claude Code binding described below. Modes never
silently fall back to each other. The chosen mode appears in the import preview,
continuation briefing and audit.

A peer the receiver trusts unrestricted acts there as its operator does: it may
land an agent on any mode the receiver can resolve, whatever
`credentials_allowed` lists, and its requester-pays policy defaults to `allowed`
instead of `off` (an explicit profile or grant policy still decides).

The dashboard's Teleport and Clone dialog offers this as **Model access**:
- the receiver's default;
- its own login (`local`);
- your own gateways, paid by you through a lease (`proxy:<name>@self`);
- gateways other nodes share with the receiver.

It asks the receiver which modes it accepts (`GET /api/teleport-credentials?group=G`
through the peer view), and greys out what it would refuse, with the reason. A
gateway that speaks the wrong protocol for the agent's harness is not offered.
Pulling a peer's agent here offers the gateways shared with this node, and only
when that peer trusts this node unrestricted.

**Your Claude subscription** is offered too, for a Claude Code agent, once you
stored a token (see
[Running as yourself on another node](federation-models.md#running-as-yourself-on-another-node-claude-subscription)) and the
receiver runs a tclaude that lands such teleports. The flow:

1. The offer carries only the mode `claude-subscription`.
2. Your dashboard sends the token for that offer right after, through this
   node's encrypted peer channel (`POST /api/teleport-subscription`). The
   receiver holds it in memory only.
3. An automatic landing waits up to two minutes for the token; an operator's
   accept uses it when present.
4. Without it, the agent lands offline, marked as running on your
   subscription, and never starts on the receiver's own login. Send the token
   from its ⚙ menu, or its badge, to start it.

A restricted sender needs `claude-subscription` in the landing policy's
`credentials_allowed`.

Both sides durably charge attempts, including clones and failed launches. Under
`federation.teleport.limits`, `per_hour`, `per_day`, `per_chain` and
`revisit_minutes` default to 4, 12, 16 and 10. Landing policies can tighten these
limits. `allow_return` on both the node and landing policy permits an explicit
`--home` return within the revisit window; all other current grants and limits still apply. Older provenance hops
are explanatory, not authority to act as another principal.

An operator can run `tclaude federation teleport off` (and `on` to restore it).
`tclaude federation teleport status` reads the current switch as
`{"disabled": true|false}`. The local dashboard has the same read and write
contract at `GET` and `PUT /api/federation/teleport`; the CLI uses
`GET` and `PUT /v1/federation/teleport`. The dashboard routes require the local
operator cookie and are refused through peer views.
This freezes new incoming/outgoing teleports and prevents uncommitted launch or
retirement on that instance. Apply it on each node to freeze a fleet; disconnected
nodes cannot be changed by a local switch. Existing agents and backup leases keep running.

### Keep a paused backup

```bash
tclaude agent teleport server --group helpers --keep-paused-backup
tclaude agent teleport report "Fixed the index; committed abc123 and ran the tests"
tclaude agent teleport --home --note "Findings and next steps"
# On the origin, the operator or an agent with covering agent.resume authority:
tclaude agent teleport recover agt_<source-id>
```

`--keep-paused-backup` stops the source after confirmed landing, retaining its
identity, inbox, permissions, native history and working directory. It is not
retired. `agent ls` shows `paused (teleported to <instance>/<agent>)`, and
`teleport status` includes its lease, epoch and any recovery error. Humans can
inspect all teleport records; an agent sees only its own records. Mail to the
paused identity remains in its inbox. Dormant slots include pending landings
and recovering backups, separately from live node capacity. Clone plus backup
is refused; a leased roaming copy must report or return home before another
teleport. Ordinary resume/power-on cannot bypass the lease.

The origin owns the durable lease epoch. Renewals use the existing federation
channel, every 30 seconds by default. After five minutes without renewal plus
two minutes of grace **while the origin is online**, auto recovery advances the
epoch and resumes the original backup. Disconnects, daemon runtime restarts and
laptop suspend restart the origin's full observation window; offline time does
not count. Manual recovery uses the same wait, with no force bypass.

Availability takes precedence during an origin outage: the remote keeps working
when the origin sleeps, disconnects, or restarts, and a remote daemon restart
resumes renewals without stopping its agent. A true partition may leave both
copies running temporarily. The recovery inbox briefing and operator notification
explicitly warn that the remote may still be alive: check current state before
repeating destructive or one-time work. On reconnect the origin's newer epoch
supersedes the remote, which stops by default. The optional `clone` policy keeps
it as an independent clone and queues a coordination briefing instead.

Configure these defaults under `federation.teleport.backup` in local config:

```json
{
  "renew_seconds": 30,
  "lease_seconds": 300,
  "grace_seconds": 120,
  "dormant_max": 4,
  "recovery": "auto",
  "superseded": "stop"
}
```

`recovery` accepts `auto|manual`; `superseded` accepts `stop|clone`. Lease duration
must cover at least two renewal intervals. Timings are pinned when the backup is
reserved, and the origin's required cadence travels with the offer so different
receiver defaults cannot trigger false failover. Recovery mode and superseded
policy are local and re-read live.
Retiring the backup or replacing its generation releases its dormant slot and
supersedes the old remote on its next contact. Revoking `self.teleport` prevents
further accepted renewals; recovery still observes the full online wait.

`report` and a leased copy's `--home` commit bounded findings (up to 16 KiB for
report). Without explicit findings, `--home` captures a bounded native transcript
tail; if it cannot read history it refuses with guidance to supply `--note`.
The roaming copy then stops. Only after its node verifies the stop does
the origin deliver the findings and resume the original identity. Retries use
the same durable return ID and never launch a second identity. Findings arriving
after lease-loss recovery still reach the original inbox once, without another
resume. Shutdown process identity survives daemon restarts, so a missing tmux
pane alone cannot certify exit. Lost connectivity
leaves a return pending; use `teleport status` to inspect it. An operator can
retire the stopped roaming identity after the origin acknowledges recovery.
An ordinary `--home` without a paused backup still uses the normal teleport
policy, group selection and revisit limits above.

## Receiving agent working directories

Clone, move and teleport imports resolve the working directory on the receiving
node. The source's `paths.cwd` and optional `paths.repo_url` are hints, not
permission to fetch a repository or create a source-selected directory.
The first available receiver-owned choice wins:

1. An explicit `--cwd` or `--landing` candidate selected by the operator.
2. An enabled `federation repos` entry allowing the receiving group whose URL
   matches the source hint. The receiver creates an isolated checkout of its
   own clone's HEAD commit; source branches, hooks and Git configuration are
   not copied. Explicit teleport `--git-ref` still requires its configured
   allowlisted repo and isolated checkout.
3. The source cwd, if it exists locally. HOME, `/` and system directories remain
   visible choices but are skipped automatically.
4. The receiving group's default directory.
5. The automatic teleport landing policy directory.

If none resolves, import refuses with `landing_unresolved` and the candidate
list. There is no implicit HOME fallback. `--keep-paths` explicitly selects the
source path and refuses if it is unavailable; it does not fall through to a
repo or group default. Missing paths and paths not owned by the receiving
service user are refused. An explicit broad directory or a broad group default
is allowed when owned by that user; remote terminal file downloads retain their
independent `root_too_broad` restriction.

The import preview and result include `landing` with `cwd`, `reason`, `exists`,
`source_cwd`, `source_repo` and `candidates`. Reasons are `explicit`, `repo_match`,
`same_path`, `group_default`, `landing_policy` and `none`. Repo candidates also
carry `checkout_required: true` and `repo: {id, name, ref}`; the planned path
need not exist until apply. Preview never creates a checkout. Candidate IDs are
`repo:<stable repo ID>`, `same_path`, `group_default` and `landing_policy`.
Send `landing` in the import body or use `federation offers import --landing`;
`cwd` takes precedence. Apply recomputes the selection and rechecks the repo,
receiving grant and directory before dispatch. Results and audits record the
resolved cwd and reason. Sender responses include `receiver_decides: true` and
`source_repo`; the sender cannot promise a receiver filesystem path.

## Message history across nodes

The dashboard peer proxy can read `GET /api/peer/INSTANCE/mailboxes` and
`GET /api/peer/INSTANCE/mailbox?id=all|human|CONV|group:NAME&q=&sender=&page=&page_size=`.
These reuse the local Messages sidebar and newest-first page shapes, including
`messages`, `page`, `page_size`, `total` and `total_unfiltered`. Pages default to
50 messages and are capped at 500. Prefix each message ID with its owning node
when merging pages from several nodes; send any action back to that node.

Both reads require the instance-wide peer grant `node.messages.read`, which is
off by default and is never implied by `message.direct`. **This grant exposes
private message bodies across the entire node**, including human notifications
and agent-to-agent mail. It is not group-scoped. Unrestricted trust includes it.
Reading a page does not change any message's read state.

`POST /api/peer/INSTANCE/mailbox/mark-read` accepts `{ids:[ID],read:true|false}`
or `{conv:CONV,read:true}` under the separate, off-by-default instance-wide
`node.messages.manage` grant. It changes agent mail read state only. Delete,
wipe and human message mutations are not shared through these routes; a
human notification's read state is `node.messages.mark-read`'s
([Human inbox across your own nodes](federation-work.md#human-inbox-across-your-own-nodes)).
Deleting agent mail (`/api/mailbox/delete`, `/api/mailbox/wipe`) and human
notifications (`/api/human-messages/delete`, `/api/human-messages/clear`) has
no peer grant: a peer that trusts you unrestricted serves them as it does its
own operator.

A human notification's files are part of the mail `node.messages.read` shows.
`GET /api/peer-attachments/INSTANCE/MESSAGE/ATTACHMENT` (local-only) reads one
from the peer in base64 chunks of at most 4 MiB
(`GET /api/human-attachment-chunk?message=&attachment=&offset=&length=` on the
peer, under the same grant; `length=0` reads only the file's name, type and
size) and serves it to the dashboard with Range support. The
peer's declared type is narrowed to images (not SVG), video, audio or plain
text, anything else is an opaque download, and the response is a sandboxed,
`nosniff` attachment. `GET /api/peer-message-export/INSTANCE/MESSAGE` (local-only,
`?attachments=1` for the zip) is the reader's download for a peer's
notification: the peer's `GET /api/human-message-export-parts?message=` (same
grant) returns the Markdown its own export serves, and this node zips it with
the files read in chunks.

A file download a peer trusting this node unrestricted serves (a snapshot's
or an agent's `.paf`, a group archive, a template) crosses the same way.
`GET /api/peer-download/INSTANCE?path=PATH` (local-only) asks the peer to stage
`PATH` (`POST /api/peer-downloads {"path"}`, a peer view job there; only the
download routes stage, and only under unrestricted trust), reads it in 4 MiB
chunks (`GET /api/peer-downloads/ID?offset=`; a staged file goes away once
its last chunk is read, or 10 minutes after its last read), and serves it as
an opaque, sandboxed, `nosniff` attachment with Range support. A download is
at most `federation.agent_transfer_max_bytes` (2 GiB by default), the cap
agent archives already cross nodes under.

Uploads go the other way, in 512 KiB chunks. Today the only upload is a spawn's
attachments on a peer's group, from the shared spawn dialog.
`POST /api/peer-upload/INSTANCE?path=PATH` (local-only) opens a staged upload
on the peer (`POST /api/peer-uploads`) and appends the body chunk by chunk
(`POST /api/peer-uploads/chunk?id=&offset=`). It then has the peer run the
upload route on the whole file (`POST /api/peer-uploads/finish
{"id","path","content_type"}`, a peer view job) and answers that route's reply.
Only the upload routes (`/api/spawn-attachments`) take a staged upload, and
only under unrestricted trust. The chunk route alone may carry a 512 KiB
request; every other peer view request keeps its 32 KiB body limit. An upload
is at most `federation.agent_transfer_max_bytes`, on top of the route's own
caps.

Agent mail's files cross the same way, under the same grant:
`GET /api/peer-agent-attachments/INSTANCE/MESSAGE/ATTACHMENT` (local-only)
reads the peer's `GET /api/agent-attachment-chunk?message=&attachment=&offset=&length=`.
An agent-mail attachment id is a stored attachment's row id, or for a file a
startup briefing lists under "Attached files" the negative of its position in
that list. The peer serves a briefing file only from inside its own
spawn-attachment directories.

Neither grant authorizes replies or approval decisions. Reply delivery to an
agent uses the existing `operator-message` route and its group-scoped
`message.direct` permission. Human notification replies, marking human
notifications read, and one-shot ask-human decisions keep the unrestricted-only
human-inbox routes described above. Peer access decisions, away cover,
permanent approvals and process obligations remain local.

A fused view retains each selected node's last successful page and its receipt
time during an outage. The peer proxy returns `peer_unreachable` with reason
`peer_offline` or `peer_timeout` and optional `last_seen`; a missing history
grant returns HTTP 403 `permission`. Show those states instead of treating them
as an empty mailbox. Stop using retained pages after local trust is removed.


The optional large-history flow moves more than 300 MiB through a hub and then
returns home with the destination's added turns. It is deliberately outside
normal CI. Run it with:

```bash
TCLAUDE_LARGE_AGENT_TRANSFER=1 scripts/test.sh ./pkg/claude/agentd -run '^TestFederation_LargeTeleportHistoryRoundTrip$' -count=1 -timeout 1200s
```

The test uses private simulated nodes and elevated test-hub bandwidth; it does
not change a running operator's nodes or settings. The ordinary chunk-spool
regression also interrupts a chunk, reopens the spool, and resumes from the
verified prefix.

OpenCode history uses its supported `export`/`import` CLI projection, not a copy
of the native SQLite database. Session, message and part IDs are reminted, native
cwd metadata is remapped, and message/tool text is preserved. Import does not
carry session permissions or sharing state; the receiving managed server
reapplies its own permission policy. Private agent XDG stores remain private.

Copilot portable history uses its native append-only event log. The receiver
remints the session and event identities, writes fresh workspace metadata, and
resumes with the native CLI. Databases, checkpoints, rewind files, permissions,
and other executable artifacts are not transferred. Full conversation events
remain available to the native resume path.

## Receiver-approved permission carry

Permission carry is off by default. Add `--carry-permissions` to
`federation share-agent`, `federation move-agent`, or `agent teleport` (including
`--clone`) to request it. Agent callers additionally need the peer-scoped
`self.teleport.permissions` permission. Ownership and temporary sudo leases
never travel.

The receiver previews each permission as applied, remapped, or dropped with a
reason. Unrestricted trust allows ordinary grants. A restricted peer needs a
receiving-group grant such as:

```sh
tclaude federation grant node-a agents.receive.permissions --scope group=support --permission-slug groups.members.spawn
```

Omit `--permission-slug` to allow ordinary known slugs. Group scopes are remapped
to the landing group; unmapped source-local agent, target, profile, peer or path
selectors are dropped. Restricted group capabilities are narrowed to the landing
group. Source denies carry and receiver policy denies win.

Permission administration, sandbox administration, `human.*`, and federation
administration grants require unrestricted trust **and** explicit receiving
operator approval (`federation offers import --allow-sensitive-permissions`, or
the import dialog's explicit opt-in). Automatic landing never opts into them.
`--drop-permissions` discards all requested carry. The receiving policy is checked
again immediately before dispatch; a changed decision refuses the launch.
The visiting copy's grants are retired with it on departure. Returning-home
identity and grant restoration are separate from this fresh-ID import path.

Local archive export and import both need `--carry-permissions`; applying carried
grants additionally requires normal permission-administration authority. The
arrival message and federation audit summarize receiver decisions.

## Managed instructions on agent transfers

Managed group/user skills and rules travel with agents in PAF, into a private
agent overlay. Neither transfer nor return installs them into a user's global
harness skill roots. The receiver's rules always apply. Older peers refuse only
transfers that actually carry managed items; a human can choose
`--omit-managed-instructions` to omit them explicitly.

An incoming code-bearing skill requires per-transfer receiver approval, even
with automatic agent admission. Preview the incoming bundle to inspect its files
and script text, then apply with `--approve-managed-scripts DIGEST`, using the
preview's `managed_instruction_digest`. Agents cannot approve this. A human can
instead omit managed items. Clone overlays are independent, so retiring their
source does not remove the clone's skills.

## Keeping resilience snapshots for a peer

A peer's [resilience policy](paf.md#resilience-snapshots) sends encrypted
snapshots of its agents to this node for safekeeping. Accept them by granting
the peer-only, unscoped `snapshots.receive` permission. Custody takes no
operator step per snapshot, so even an unrestricted peer needs this explicit
grant:

```bash
tclaude federation grant alice snapshots.receive
```

Snapshots are sealed to this node's key as well as the sender's, so this
node could decrypt them; tclaude stores them encrypted and never previews or
imports them. `tclaude agent snapshot held` lists them, and
`tclaude agent snapshot held --rm-peer alice` deletes a peer's copies, for
example after revoking the grant. Retention follows the sender's policy and
is bounded at 2 GiB and 200 agents per peer.

## Offering configuration to a peer

Config offers go to the remote operator, and are never applied automatically,
including between unrestricted peers. On the receiving machine, grant the
sender the peer-only, unscoped `config.offer` permission:

```bash
tclaude federation grant alice config.offer
```

Send either an existing config bundle or a selection of current configuration:

```bash
tclaude federation offer-config bob setup.json
tclaude federation offer-config bob --only roles --only profiles
```

Sending and managing offers are operator-only. The sender rechecks supplied
files for credential patterns and omits structured credentials, using the local
config export boundary described above. Flagged free text refuses sending
unless `--allow-flagged` is explicit; content is never silently rewritten.

The receiver sees offers in `federation inbox` and manages them with:

```bash
tclaude federation offers
tclaude federation offers import <id>                  # fetch if needed, then preview
tclaude federation offers import <id> --only roles --apply
tclaude federation offers decline <id>
```

Preview includes the source peer, offer ID, digest, expiry, before/after diffs
and security tags. Import uses the existing config import path: `--only`,
`--skip`, repeatable `--set name=value`, `--keep-paths` and `--replace` retain
those meanings. Applying selected items finishes the offer and discards the
rest. A partial apply failure reports items already written and leaves the
offer available for review and retry. Use `--peer` to disambiguate duplicate
IDs from different peers. Revoking trust or `config.offer` blocks further
fetch/import, while local decline remains available.

`federation offers --outgoing` shows sent offer outcomes; `federation outbox`
shows transport delivery and receipts. Offers expire after 72 hours. Each peer
may have ten active config offers, totaling at most 64 MiB in each direction;
a single bundle is limited to 16 MiB. Small payloads (up to 256 KiB) travel in
sealed envelopes. Larger payloads use the encrypted hub stream relay when the
operator fetches or previews; the sender must be online then. Explicit
`federation offers fetch <id>` downloads without importing. Exact length,
SHA-256 and authenticated stream completion are checked before a private
spool file becomes ready. Failed transfers remain retryable. Expiry and
decline remove payload files, including after daemon restart; terminal receipt
metadata remains visible. No remote bundle becomes an agent prompt.

## Portable local setup bundles

Setup transfer also works without federation:

```bash
tclaude config export --file setup.json
tclaude config import --file setup.json                 # preview and diff
tclaude config import --file setup.json --apply --replace
```

The versioned JSON envelope records creation time and the creating tclaude
version. Its independently selectable sections are `profiles`, `roles`,
`saved-prompts`, `templates`, `sandbox-profiles`, `default-permissions`, `process-templates`
(current YAML source, without runtime history), and `config` (an allowlist of
portable preferences). Existing component export formats remain embedded in
the bundle. Local identities, group assignments, federation credentials and
trust, remote-access settings, keychain references, and credential environment
entries are excluded. Templates reference the separate roles and profiles
sections; include those sections when bringing dependencies to a new machine.

Both commands accept repeatable `--only section[/name]` and
`--skip section[/name]`. Import previews before/after values and marks changes
to permissions, sandbox policy, roles, templates and launch profiles with a
`security` tag. Nothing is written without `--apply`; conflicting values need
`--replace` or exclusion. Identical items are left alone. Components are applied
in dependency order; an apply failure reports items already written, and does
not roll back earlier components. Sandbox include graphs are imported together.

Structured paths under the exporting user's home use `${HOME}`. Other absolute
paths become named placeholders with their original values in metadata. Preview
shows those originals and lists missing bindings and the exact
`--set name=value` flag to resolve each one (repeatable). Use `--keep-paths` to
retain all exported original paths; explicit `--set` values override individual
originals. Applying with unresolved
placeholders is refused. Prompts, scripts and process sources remain verbatim;
review their machine assumptions before using them on another host.

Export detects common credential patterns in free text, reports the item and
field without printing the suspected value, and refuses unless you exclude
those items or explicitly pass `--allow-flagged`. This is a detection aid, not a
guarantee that arbitrary prose or scripts contain no secrets; inspect the file
before sharing it. Structured credential fields are always omitted.

Agents need explicit `config.export` / `config.import` grants; neither is granted
by default or through group ownership. `config.import` permits changes to
agent authority, including default permissions and sandbox access. Human CLI
callers use the same daemon APIs without those agent grants. Bundles currently
transfer as local files; these commands do not send anything to a peer.

## Portable local agent bundles

Agent transfer uses a single archive file, separate from the existing export
jobs. Export writes a [Portable Agent Format](paf.md) file by default (as does
[`tclaude paf export`](paf.md#exporting-and-importing-an-agent)), which carries
the agent's configuration and native history plus a harness-neutral copy of
the conversation when the harness can decode it. `--format v1` writes the
legacy agent bundle version 1 ZIP instead, for importing into an older
tclaude. A PAF export is signed with the
node's federation key when one exists; exporting never creates a key. Import
accepts either format and tells them apart by content, not file name.

```bash
tclaude agent bundle export reviewer --file reviewer.paf --history
tclaude agent bundle import --file reviewer.paf --cwd "$PWD" --group team
tclaude agent bundle import --file reviewer.paf --cwd "$PWD" --group team --apply
tclaude agent bundle export reviewer --file reviewer.zip --history --format v1
tclaude agent bundle import --file reviewer.zip --cwd "$PWD" --group team
```

Preview is the default. `--apply` creates a fresh local agent through the normal
spawn permission checks and launch path; it requires an existing receiving
`--group`. `--name` overrides the source name. Source group names, roles and
permission provenance are advisory: ownership, permission overrides and role
references never become grants on the receiver. The inline profile includes
launch posture (including sandbox and approval settings), shown in the preview
alongside an explicit security summary and checked by normal receiver policy.
Role guidance, startup context, initial task text and task-reference links travel
as configuration. Source profile names are informational; no receiver profile is
looked up by that name.

`--cwd` remaps the working directory. `--keep-paths` reuses recorded originals
that exist locally; missing directories refuse apply with a remapping hint.
Optional worktree hints can be remapped with `--worktree`. Profile path
placeholders use the same `--set name=value` and `--keep-paths` conventions as
setup bundles. Working directories, branches and paths inside prompts or tool
output are hints, not filesystem contents; no repository or worktree is copied.

`--history` includes Claude Code JSONL, a Codex rollout, OpenCode native export,
Copilot history, Gemini native chat JSONL or a pi session JSONL through the harness's
`HistoryTransfer` capability. Import remints native conversation identity and
remaps known cwd metadata before resuming it as a new local agent. Message and
tool-output text is preserved. `--skip-history` requests configuration
only on import. Sidecar databases, credentials, hooks and artifacts are excluded.
Credential detection uses the setup-bundle boundary, reporting counts per kind
and the first three locations. Export refuses flagged text unless
`--allow-flagged` is explicit; history is never sanitized or silently rewritten.
Inspect archives before sharing them.

A PAF import is fully verified before preview: hashes, schemas, signatures
and ZIP layout. A PAF that requires a feature or extension this tclaude does
not support is refused. One line of its neutral context may be at most
64 MiB. `--harness` and `--model` land the agent on another harness or model; the
conversation is then translated, as described in
[Exporting and importing an agent](paf.md#exporting-and-importing-an-agent).
A PAF that carries only the neutral context, with no native history, is
translated the same way. A
group PAF is not an agent bundle and is refused here.

The version 1 `manifest.json` records format/version, creation time, tclaude
version, configuration and optional transcript length/checksum. Unknown fields
within version 1 are ignored; unknown versions refuse. Archives accept only
`manifest.json` and optional `history/transcript.jsonl`, with bounded compressed
and uncompressed sizes, no symlinks, duplicate entries or arbitrary extraction.
Agents need explicit `agent.bundle.export` / `agent.bundle.import` grants;
neither is default-granted or implied by ownership. Import also requires normal
spawn authority. These commands operate on local files and do not contact peers.
