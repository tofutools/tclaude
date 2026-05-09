# Plan: `tclaude agentd` — daemon for cross-session agent coordination

Status: prototype in progress. Follow-up to the v1 `tclaude agent`
work in [`agent-coord.md`](agent-coord.md).

> **Hand-off pointer:** if you (future-Claude / a different machine)
> are picking this up cold, the live state is in
> `pkg/claude/agentd/`. Read this doc top-to-bottom — it is the
> source of truth for design intent. The TODO/DONE rolling list is
> in [`agents_todo.md`](agents_todo.md).

## Why

The v1 in-process implementation works for persistence (every agent has
write access to `~/.tclaude/db.sqlite`), but live delivery via
`tmux send-keys` only works when the *sender* happens to be allowed to
reach the tmux socket. In sandboxed CC sessions that's frequently not
the case, so messages get persisted but the receiver doesn't see a
nudge until they manually run `tclaude agent inbox ls`.

A daemon outside any per-agent sandbox sees the whole picture: it owns
the SQLite store, knows every live tmux session, can resolve identities
and groups consistently, and can hand each agent a scoped token at
session start instead of trusting environment variables.

## Goals

- Single source of truth: groups, members, messages, and tmux session
  registry all live behind one process.
- Agents talk HTTP — they no longer need to invoke the `tclaude` binary
  for messaging. `curl` (or any HTTP client) suffices.
- Tokens are scoped: each token maps to exactly one conv-id and the
  daemon enforces what that conv may do. Mutating group membership
  remains a human-only operation.
- Foreground first: `tclaude agentd serve` runs in the foreground with
  logs on stdout, so we can iterate. No service-manager integration in
  the first cut. The user starts it manually.

## Non-goals (this iteration)

- launchd / systemd integration. Comes later.
- Multi-host clustering.
- Authenticated tokens for the human ("god" token). For now the human
  is whoever can reach the loopback socket — same trust boundary as
  the existing tclaude binary.
- Migrating away from the current agent CLI. The CLI keeps working;
  when `TCLAUDE_AGENT_URL` + `TCLAUDE_AGENT_TOKEN` are set, the CLI
  becomes a thin HTTP client. Otherwise it falls back to direct DB
  access (so test fixtures and ad-hoc human use don't need the daemon).

## Transport

- Unix domain socket at `~/.tclaude/agentd.sock` (mode 0600).
- Standard library `net/http` over `net.Listen("unix", path)` so we
  keep familiar HTTP semantics — agents and skills can call `curl
  --unix-socket ~/.tclaude/agentd.sock http://_/v1/whoami`.
- Loopback TCP is *not* used. Two reasons:
  1. We want server-side peer credentials (PID/UID) for identity. TCP
     can't carry that; Unix sockets can via `getpeereid`/`SO_PEERCRED`.
  2. Bearer tokens stored in env vars don't survive Claude Code's
     `/fork` correctly — the forked conv-id is new but the inherited
     token still maps to the parent. Peer-cred + reading
     `~/.claude/sessions/<pid>.json` always reflects the *current*
     conv-id.

`~/.tclaude/agentd.sock` is the well-known location; clients don't
need a discovery file.

## Identity (no tokens)

On every request the daemon:

1. Reads the connecting peer's PID from the socket
   (`getpeereid` on macOS/BSD via `LOCAL_PEERPID`, `SO_PEERCRED` on
   Linux).
2. Walks up the process tree (same logic as `session.FindClaudePID`)
   to find the nearest `claude`/`node` ancestor.
3. Reads `~/.claude/sessions/<pid>.json` for that ancestor's
   `sessionId` — the conv-id Claude Code is *currently* executing,
   which automatically tracks `/fork`/`/clear`/`/resume`.
4. That conv-id is the request's authenticated identity.

If no `claude` ancestor exists, the caller is treated as the local
human. The same UID check guards against other users on the host
(macOS sockets default to 0600, so this is enforced by file
permissions).

Trade-off: unlike a token, this identity can't be passed around — a
child of an agent that opens the socket on its own behalf is still
that agent's conv-id. That's the right semantic for our use case.

No `agent_tokens` table is needed. No bootstrap secret. No env-var
plumbing into tmux session env.

## Endpoints

All under `/v1/`. Bodies are JSON. Errors return
`{"error": "string", "code": "string"}` with HTTP status mapped to the
agent CLI's exit codes.

### Identity

```
GET  /v1/whoami
  → 200 {"conv_id": "...", "title": "tclaude-agents", "groups": [...]}
```

### Lookup / discovery

```
GET  /v1/lookup?selector=<id|prefix|title>
  → 200 {"conv_id": "..."}
  → 404 {"error": "no conversation matches", "code": "not_found"}
  → 409 {"error": "selector matches multiple", "code": "ambiguous",
         "candidates": [...]}

GET  /v1/peers
  → 200 [{"conv_id": "...", "title": "...", "alias": "...",
          "role": "...", "descr": "...", "groups": [...]}]
```

### Groups (read-only via the daemon for agents; mutations come from a
trusted localhost CLI client without a token)

```
GET  /v1/groups                        — all groups (everyone can list)
GET  /v1/groups/{name}/members         — members of a group

POST /v1/groups                        — create        \
DELETE /v1/groups/{name}               — delete         \  human only:
POST /v1/groups/{name}/members         — add a member    | requires
DELETE /v1/groups/{name}/members/{id}  — remove a member /  no Bearer
                                                          and same-uid
                                                          loopback
```

The "human-only" gate replaces the v1 process-tree heuristic. Inside
the daemon we know exactly who is calling because we issued every
token. A request *with* a Bearer token is an agent and is refused for
mutating endpoints. A request *without* one is the human (or another
local-uid process) and is allowed.

### Messaging

```
POST /v1/messages
  body: {"to": "<selector>", "subject": "...", "body": "..."}
  → 200 {"id": 42, "delivered": true|false, "via_group": "alpha"}
  → 403 {"error": "not in a shared group", "code": "auth"}

GET  /v1/inbox?unread=true&limit=20
  → 200 [{"id": 42, "from": "...", "from_short": "...",
          "group": "alpha", "subject": "...", "preview": "...",
          "created_at": "...", "read": false}]

GET  /v1/messages/{id}
  → 200 {full message, headers + body; marks read unless
         ?keep-unread=1}
```

### Session lifecycle

No registration endpoint. The daemon resolves identity from the
connecting peer's PID on every request (see "Identity"), so there's
nothing to register. Sessions come and go without daemon-side
bookkeeping.

## Tmux delivery

Lives in the daemon. The daemon already has tmux access (it was started
from a non-sandboxed shell). On every successful `POST /v1/messages`
that resolves to a target with a known live tmux session, it does
`tmux -L tclaude send-keys -t <session>:0.0 "[system: …]" Enter` and
sets `delivered_at`.

The daemon also re-validates tmux session names on a ticker (every
~5s), pruning stale `sessions.tmux_session` values. This fixes the v1
issue where the DB row pointed to a tmux session that had since been
recreated under a different name.

## CLI fallback

The existing `tclaude agent …` commands stay. When the binary detects
`TCLAUDE_AGENT_URL` + `TCLAUDE_AGENT_TOKEN`, it goes through HTTP.
Otherwise it falls back to the direct-DB path. This keeps:

- Unit tests working without a daemon.
- The human's interactive use working out of the box.
- A single binary surface for users who don't want the daemon.

## How agents reach the daemon

There is no env-var setup. Anything that can talk HTTP over a Unix
socket can hit the daemon:

```
# from a Bash tool inside CC:
curl -sS --unix-socket ~/.tclaude/agentd.sock http://_/v1/whoami

# or via the CLI fallback (which just wraps the curl):
tclaude agent whoami
```

The daemon figures out which conv is calling on its own. Whether the
session was launched by `tclaude` or by `claude` directly doesn't
matter — both end up as a `claude`/`node` process whose PID has a
`~/.claude/sessions/<pid>.json` file we can read.

## Rollout plan

1. ✅ Daemon scaffold: Unix socket at `~/.tclaude/agentd.sock`,
   peer-PID resolution (darwin: `LOCAL_PEERPID`, linux:
   `SO_PEERCRED`), `/v1/whoami`.
2. ✅ Messaging endpoints + tmux delivery from inside the daemon.
3. ✅ Group endpoints: read open to anyone; mutate restricted to
   "no CC ancestor in process tree" callers (enforced server-side).
4. ⏳ End-to-end manual demo: `tclaude agentd serve` running, two
   live CC sessions, send/receive a message, verify tmux nudge.
5. ⏳ CLI fallback: when the socket exists, `tclaude agent …` becomes
   a thin client. Otherwise it falls back to direct DB access (so
   tests and ad-hoc human use don't need the daemon).
6. ⏳ Cleanup of any token/registration code from the v0 design.
   The `agent_tokens` table was scoped out before it was added to
   the schema, so nothing to migrate.

Demo after (4): `tclaude agentd serve` in one terminal, then
`curl --unix-socket ~/.tclaude/agentd.sock http://_/v1/whoami` from
inside a CC session and watch the daemon return *that conv's*
identity automatically (no token, no env-var). Then
`POST /v1/messages` and verify the `[system: …]` nudge lands in the
target session's tmux pane.

## File map (where things live)

```
docs/plans/agentd.md           ← this doc
docs/plans/agent-coord.md      ← v1 agent-coord design (still relevant)
docs/plans/agents_todo.md      ← rolling TODO/DONE
pkg/claude/agentd/agentd.go    ← cobra wiring for `tclaude agentd`
pkg/claude/agentd/serve.go     ← Unix socket setup + http.Server
pkg/claude/agentd/discovery.go ← well-known socket path
pkg/claude/agentd/identity.go  ← peer-cred → conv-id middleware
pkg/claude/agentd/peer_*.go    ← platform-specific PID lookup
pkg/claude/agentd/handlers.go  ← all /v1/* HTTP handlers
pkg/claude/agentd/json.go      ← writeJSON / writeError helpers
pkg/claude/agent/lookup.go     ← exported helpers (ResolveSelector,
                                 CurrentConvID, DisplayTitle, …)
pkg/claude/agent/*             ← v1 CLI commands (still wired up;
                                 will become a thin client in step 5)
```

## Open questions to revisit

- **CLI vs HTTP at the agent's edge.** Right now `tclaude agent …`
  still hits SQLite directly. Step 5 makes it route through the
  socket. Open: do we want the CLI to *prefer* the daemon when both
  paths work, fall back to direct DB only on socket failure? Yes,
  probably — gives consistency with the daemon's tmux delivery.
- **`/fork` semantics for messages.** When CC forks, the new conv
  has no inbox until something writes there. Should the daemon
  carry-forward unread messages from the parent? Probably no — fork
  is a "fresh start" affordance. But the parent's outbox should
  still be queryable from the fork (subject to group membership).
  Not blocking; revisit when forks come up in practice.
- **Federation across hosts.** Out of scope, but the design (per-host
  daemon owning local conv pool) makes a future federation layer
  tractable. See `agents_todo.md`.
