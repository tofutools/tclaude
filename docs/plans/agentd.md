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
- ~~Authenticated tokens for the human.~~ **Superseded.** The daemon now
  fails closed and authenticates the human operator with an interim
  operator token — see "Identity & classification" and "Security model".
- Migrating away from the current agent CLI. The CLI keeps working
  and routes every call through the daemon over the well-known
  Unix socket; when the socket isn't reachable, the CLI exits with
  a clear error rather than falling back to direct DB writes.

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

## Identity & classification (fail-closed)

Every request is classified into exactly one of three caller classes —
there is no fail-open "assume human" path. The single function
`classify()` (`agentd/identity.go`) is the *only* place this decision is
made; every auth helper routes through it.

On every request the daemon:

1. Reads the connecting peer's PID from the socket (`SO_PEERCRED` on
   Linux, `LOCAL_PEERPID` on macOS/BSD). agentd runs host-side, so the
   kernel reports the caller's real *host* PID.
2. Walks the host process tree to the nearest `claude`/`node` ancestor.
3. Resolves that ancestor's conv-id — from `~/.claude/sessions/<pid>.json`
   (the `sessionId` Claude Code is *currently* executing, which tracks
   `/fork`/`/clear`/`/resume`), or, as a fallback, from the daemon's own
   `sessions` table keyed by host pid.
4. Verifies the `X-Tclaude-Human-Token` header against the operator
   token, when present.

The resulting class:

- **Agent** — a Claude Code ancestor with a resolved conv-id. That
  conv-id is the request's identity.
- **Human** — the human operator: a caller presenting a valid operator
  token, or a request delegated from the cookie-authenticated dashboard.
- **Unconfirmed** — neither. Refused `403` (`code: "unconfirmed"`). A
  peer whose PID could not be read is refused `401`.

**Precedence is load-bearing:** a Claude Code ancestor wins over any
operator token. The human exports the token into their shell, so a CC
session launched from that shell inherits it; if the token could promote
a caller, such an agent would escalate to human. An agent-family caller
is therefore never offered the token branch — and agentd additionally
scrubs `TCLAUDE_HUMAN_TOKEN` from the environment of every CC session it
spawns.

### The operator token

The operator token positively authenticates the human on the CLI /
Unix-socket path. It is:

- minted fresh each daemon lifetime (`crypto/rand`, `tclo_` prefix),
  held only in memory — never persisted, never written through `slog`
  (slog → `~/.tclaude/output.log`);
- delivered **only via the daemon's startup banner** — there is no
  fetch endpoint and no `classify()` exception. When stdout is a TTY the
  banner prints a ready-to-paste `export TCLAUDE_HUMAN_TOKEN=…` line.
  When stdout is **not** a TTY (backgrounded / redirected, e.g. into the
  log) it never prints the token — it would land in the log — and
  instead tells the operator to relaunch agentd attached to a terminal.

The human copies the `export TCLAUDE_HUMAN_TOKEN=…` line from the banner
into their shell. To see the token, agentd must be launched attached to
a terminal. A restart of agentd mints a new token; the human re-copies
it. Agents need no token and are unaffected. The cookie-authenticated
dashboard continues to work with no token set.

(A future iteration will replace this banner / copy-paste story with
secure on-disk token storage — an OS keychain that agents are blocked
from reading; until then, banner-only.)

## Security model

The fail-closed identity layer is a real boundary **only in composition
with the OS sandbox** — stated plainly, no over-claim.

- **vs a sandboxed agent** (bwrap PID-namespace): a real boundary. The
  agent runs in its own PID namespace with a namespaced `/proc`; it
  cannot see the human's process, cannot learn host PIDs, and cannot
  read host files outside its sandbox mount, so it cannot obtain the
  operator token — fail-closed then denies it every human-only
  operation. It also cannot escape its Claude Code ancestry in agentd's
  host-side process walk, so it is always classified as the agent it is.
- **vs a non-sandboxed same-uid process**: *not* a boundary. Any
  same-uid process can read `/proc/<human-pid>/environ` (hence the
  token) and can mutate `~/.tclaude` state directly anyway. This is the
  accepted same-uid residual.

So: the OS sandbox confines the agent; the operator token gates the
human path; neither is a standalone boundary.

**Known edge — the stray-node caller.** A human running `tclaude agent`
from a shell incidentally descended from a non-Claude `node` process is
classified agent-family, and the operator token cannot promote it
(agent-ness wins). It is refused fail-closed. Workaround: run operator
commands from a clean terminal, or use the dashboard. This is accepted —
tightening the classification to rescue it would reopen an agent-startup
escalation window.

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
  → 200 [{"conv_id": "...", "title": "...",
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

## CLI as thin client (no fallback)

The existing `tclaude agent …` commands stay. When the daemon's
Unix socket exists (`~/.tclaude/agentd.sock`, probed by
`DaemonAvailable()`), the CLI sends an HTTP request over it.
**There is no direct-DB fallback in production.** If the socket is
unreachable, the CLI exits with `Error: tclaude agentd is not running.`
That keeps the auth model honest: an agent can't bypass the daemon's
peer-cred gate by happening to find the daemon down.

Tests for the CLI side exercise the `Direct` helper functions
explicitly (decoupled from `DaemonAvailable`); DB-level CRUD is
covered in `pkg/claude/common/db/agent_test.go`.

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
5. ✅ CLI thin client: when the socket exists, `tclaude agent …`
   commands route through HTTP. Otherwise they fall back to direct
   DB access (so tests and ad-hoc human use don't need the daemon).
   `whoami`, `lookup`, `ls`, `message`, `inbox ls/read`, and all
   `groups …` commands switch on `DaemonAvailable()`.
6. ⏳ End-to-end manual demo (waiting on user to start the daemon).

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
docs/sandbox-hardening.md      ← operator guide: sandboxing agents — what
                                 the Security model above composes with
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
