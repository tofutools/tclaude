# agentd fail-closed identity + operator token — shipped

agentd no longer assumes "no Claude Code ancestor ⇒ the human". It fails
closed: a request is either a confirmed **agent**, a confirmed **human**
(operator token, or the cookie-authenticated dashboard), or
**unconfirmed** → `403`.

This is the Phase-2 follow-up to the agentd caller-identity security
investigation (the guardrail-documentation work is the separate
`agentd-identity-guardrail-docs` PR, #167). The human's call was: stop
fail-open, positively authenticate the human with an interim token.

## What shipped

### A — the `classify()` single chokepoint (`agentd/identity.go`)

`peer` gained `HumanTokenValid` and `DashboardHuman` fields. A new
`callerClass` enum + `classify(*peer) callerClass` is the *only* place
the human-vs-agent decision is made:

- `classUnidentified` (PID 0) → `401`
- `classAgent` (Claude Code ancestor + resolved conv-id)
- `classAgentUnknown` (CC ancestor, conv-id unresolved) → `403`
- `classHuman` (`DashboardHuman`, or no CC ancestor + valid token)
- `classUnconfirmed` (no CC ancestor, no token) → `403`,
  `code: "unconfirmed"`, with a self-explanatory body

Precedence is load-bearing: a CC ancestor wins over the token, so an
agent that inherited `TCLAUDE_HUMAN_TOKEN` cannot escalate to human.

Every human-vs-agent decision site routes through `classify()` (or the
`authedCaller` helper): `requirePermission`, `requireHuman`,
`requireAgent`, `requireCrossAgentPermission`, `requireGroupLinkAuthority`,
`requireScopedLinkAuthority`, `authCronWrite`, `authCronWriteGroup`, the
cron-visibility filters (`handleCronList`, single-job log read), the six
`sudo.go` gates, `handleAgentDir`'s cross-agent terminal spawn, and
`requireNotifyHumanPermission`. No raw `HasClaudeAncestor` policy test
survives outside `classify()` — there is no exception.

### B — the operator token (`agentd/humantoken.go`)

Minted at daemon startup (`crypto/rand`, `tclo_` prefix), held only in
memory — never persisted, never written through `slog` (slog →
`~/.tclaude/output.log`). The startup banner prints it only when stdout
is a TTY; otherwise it prints a pointer, so the secret can never land in
a redirected log. Verified via the `X-Tclaude-Human-Token` header with a
constant-time compare.

### C — token delivery: the startup banner only

The operator token is delivered **solely** by the daemon's startup
banner — there is no fetch endpoint and no CLI command. When stdout is a
TTY the banner prints a ready-to-paste `export TCLAUDE_HUMAN_TOKEN=…`
line; the human copies it into their shell. When stdout is not a TTY
(backgrounded / redirected) the banner never prints the token and tells
the operator to relaunch agentd attached to a terminal.

(An earlier cut of this feature had a `GET /v1/auth/token` endpoint +
`tclaude agent token` command; both were removed — the endpoint was
gated on the legacy `!HasClaudeAncestor` heuristic, the one place that
partially un-did fail-closed, and the token is already on the banner. A
future iteration replaces the banner with secure on-disk token storage.)

### D — CLI header injection (`agent/client.go`)

The three daemon-request builders (`daemonReq`, `DaemonGetRaw`,
`DaemonPostRaw`) attach `X-Tclaude-Human-Token` from `TCLAUDE_HUMAN_TOKEN`
when set. Agents never have the var set.

### E — Symptom-B conv-id fallback (`common/db/sessions.go`, `identity.go`)

New `db.FindSessionByPID`. `convIDForPID` falls back to the `sessions`
table (host pid → conv-id) when a CC ancestor's
`~/.claude/sessions/<pid>.json` is missing or transiently unreadable —
so a freshly-started agent is identified, not mis-classified as
`classAgentUnknown`.

### F — spawn-env scrub (`agentd/lifecycle.go`)

`liveSpawnNew` / `liveSpawnResume` set `cmd.Env` to the daemon
environment minus `TCLAUDE_HUMAN_TOKEN` — defence-in-depth so a spawned
agent never inherits the human's token.

### G — docs

`docs/plans/agentd.md`: the "Identity (no tokens)" section became
"Identity & classification (fail-closed)"; a new "Security model"
section states the honest threat model (a real boundary only in
composition with the OS sandbox; not a standalone boundary against a
non-sandboxed same-uid process); the human-token non-goal is marked
superseded.

## UX / migration

A human running a human-only `tclaude agent` command with no token set
now gets a `403` whose body says exactly how to fix it: set
`TCLAUDE_HUMAN_TOKEN` to the operator token printed on the agentd
startup banner. The dashboard still works with no token
(cookie-authenticated). agentd restart mints a new token — re-copy it
from the banner. Agents need no token and see no behaviour change.

Known accepted edge: a human invoking `tclaude agent` from a shell
incidentally descended from a non-Claude `node` process is classified
agent-family and the token cannot promote it — run operator commands
from a clean terminal, or use the dashboard.

## Threat model (no over-claim)

The token is a real boundary only *with* the OS sandbox: a bwrap
PID-namespace agent cannot read the human's environment and cannot
escape its CC ancestry in the host-side process walk. Against a
non-sandboxed same-uid process the token is readable from
`/proc/<pid>/environ` and `~/.tclaude` is writable directly — accepted
same-uid residual.

## Files

- `agentd/identity.go` — `peer` fields, `callerClass`, `classify`,
  `authedCaller`, `writeUnconfirmed`/`writeUnidentified`/`writeAgentUnknown`,
  the routed auth helpers, `convIDForPID` fallback.
- `agentd/humantoken.go` — token mint/verify, the startup banner, the
  spawn-env scrub.
- `agentd/serve.go` — token generation at startup.
- `agentd/{agent_dispatch,groups_links,cron_handlers,sudo,dir,notify_human,head_aliases}.go`
  — auth sites routed through `classify()`.
- `agentd/lifecycle.go` — spawn-env scrub.
- `agent/client.go` — `X-Tclaude-Human-Token` header injection from
  `TCLAUDE_HUMAN_TOKEN`.
- `common/db/sessions.go` — `FindSessionByPID`.
- `docs/plans/agentd.md` — revised.

## Tests

`agentd/failclosed_test.go`: `classify()` across all five classes plus
the load-bearing precedence cases; operator-token verify (correct /
wrong / absent / no-token-generated); agent-with-inherited-token → still
`classAgent` (token ignored); dashboard mutation via
`asDashboardHumanPeer` still succeeds (gap-2 regression); a fail-closed
assertion at a previously-inline site (`authCronWrite`) so a future
un-centralised check is caught (gap-1 regression); an end-to-end
fail-closed check through the production mux.
