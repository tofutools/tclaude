# Dashboard init-token auth — agents can't reach the admin `/api/*`

## Problem

The agentd dashboard (loopback HTTP on `127.0.0.1:<random>`) exposes `/api/*`
admin mutations that deliberately bypass the per-agent permission system via
`asDashboardHumanPeer`. The only gate was `checkDashboardAuth` (session cookie +
Origin/Referer pin). Both checks were defeatable by a same-user, non-browser
process — i.e. any agent that can open a loopback TCP connection:

- `GET /` issued the `tclaude_dashboard_session` cookie to *any* caller,
  with no authentication.
- `Origin` / `Referer` are forgeable outside a browser.
- The port isn't secret — `/v1/info` returns `popup_base_url`.

Net: an agent could `GET /v1/info` → `GET /` (scrape the cookie) → `POST /api/sudo`
(grant itself any slug) — a total bypass of `requirePermission`. The Claude Code
bash sandbox blocking loopback TCP was the only thing preventing it in practice,
and that's a harness property, not a tclaude guarantee.

## What shipped — authorization-code-style token exchange

The dashboard session cookie is now obtainable only by exchanging a short-lived,
single-use **init token**, and init tokens are minted only over the
peer-credential-authenticated Unix socket (or in-process by the tray).

**CLI / endpoint surface:**

- New `GET /v1/dashboard/open` on the daemon Unix-socket mux. Gated by
  `requireHuman`: a caller with a Claude Code ancestor (an agent) gets 403; the
  human gets `{"url": "<loopback>/?init_token=<tok>"}`. No slug — the dashboard is
  human-only by definition; there is no agent path to ladder up to.
- `tclaude agent dashboard` now calls `/v1/dashboard/open` (was `/v1/info`).
  `--print` emits a one-shot URL whose token expires in ~60s.

**Dashboard root (`handleDashboardRoot`, loopback server):**

- `GET /?init_token=X` — validates + consumes X, sets the HttpOnly /
  SameSite=Strict session cookie, then 303-redirects to the bare path so the
  one-shot token never lingers in the address bar, history, or an access log.
- `GET /` with a valid cookie — serves the page (refresh / second tab).
- `GET /` with neither — 403. The cookie is no longer handed out for free.

**Tray:** "Open dashboard" mints an init token in-process (the tray *is* the
daemon → no socket round-trip) and opens `<loopback>/?init_token=<tok>`.

**Init tokens:** in-memory map, 60s TTL, single-use, opportunistic GC. A daemon
restart drops every pending token — the human just reopens the dashboard.

`requireHuman` (`head_aliases.go`) was tightened to also fail closed on an
unidentifiable peer (PID 0), matching the guard already in `requirePermission`.

## Files

- `pkg/claude/agentd/dashboard.go` — init-token store, `mintDashboardInitToken` /
  `consumeDashboardInitToken`, rewritten `handleDashboardRoot`, new
  `handleDashboardOpen`.
- `pkg/claude/agentd/head_aliases.go` — `requireHuman` PID-0 fail-closed.
- `pkg/claude/agentd/serve.go` — `/v1/dashboard/open` route + startup banner text.
- `pkg/claude/agentd/tray.go` — in-process token mint on "Open dashboard".
- `pkg/claude/agent/dashboard.go` — CLI uses `/v1/dashboard/open`.
- `pkg/claude/agentd/dashboard_auth_flow_test.go` — flow tests.

## Test scenarios (`dashboard_auth_flow_test.go`)

- `TestDashboardOpen_RefusesAgents` — an agent peer gets 403 from
  `/v1/dashboard/open`.
- `TestDashboardOpen_HumanGetsTokenURL` — a human peer gets a URL embedding an
  `init_token`.
- `TestDashboardAuth_TokenExchangeFlow` — bare `GET /` refused; bogus token
  refused; a valid token → 303 + Set-Cookie; the token is single-use; a request
  carrying the cookie serves the page.

## Residual / not closed

- A same-user process can still scrape the *human browser's* on-disk cookie store
  (`/proc`, the browser profile dir) — the genuine same-user trust floor, far
  above "make one HTTP request", and blocked by the CC bash sandbox anyway.
- The pid-tree identity walk (`convIDForPID`) can be evaded by an agent that
  double-forks + `setsid` to reparent under init. That already defeats
  `requirePermission` on the Unix socket — this change brings the dashboard to
  parity with the rest of agentd, not beyond it.
- The popup `/approve` endpoints had a related gap; it was closed with the same
  init-token-exchange technique — see `docs/plans/DONE/popup-transport-hardening.md`.
