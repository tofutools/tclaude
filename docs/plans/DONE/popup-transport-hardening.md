# Popup transport hardening — init-token exchange for approvals

## The gap

The agent-approval popup runs on the loopback HTTP server. `tclaude
agentd` opens a browser at `/approve/{id}`; the first GET minted a
session cookie and set it for *any* caller, and POSTs to
`/approve/{id}/approve` were gated only by that cookie + an Origin pin.

A same-user process (an agent) could read the popup URL out of the
browser launcher's argv (`/proc/<pid>/cmdline`), GET it to receive the
`Set-Cookie`, then POST `approve` — approving its own pending
permission request. The cookie + Origin pin only ever stopped drive-by
browser-tab CSRF, never a non-browser process.

## What shipped

The popup now uses the same authorization-code-style token exchange as
the dashboard (see `dashboard-init-token-auth.md`):

- A shared, **scoped** init-token store — `pkg/claude/agentd/inittoken.go`
  — backs both the dashboard `/` and the popup `/approve/{id}`. Tokens
  are single-use, 60s TTL, in-memory, and carry a scope, so a token
  minted for one purpose cannot be redeemed for another.
- `handlePopupApprove`'s GET hands out the session cookie only in
  exchange for a valid init token scoped to that specific approval
  (`approve:{id}`), then 303-redirects to the bare path so the one-shot
  token drops out of history. A bare GET with no token and no cookie is
  refused.
- The init token is minted at approval creation — `realRequestHumanApproval`
  embeds it in the URL it launches — and by the tray's "open approval"
  click (`mintInitToken`, in-process). Both are human-controlled paths.
- The dashboard's `mintDashboardInitToken` / `consumeDashboardInitToken`
  were generalised into `mintInitToken(scope)` / `consumeInitToken(tok,
  scope)`; no behaviour change for the dashboard (scope `"dashboard"`).

## Files

- `pkg/claude/agentd/inittoken.go` — new: scoped single-use init-token
  store (`mintInitToken`, `consumeInitToken`, `initScopeDashboard`,
  `initScopeApprove`).
- `pkg/claude/agentd/popup.go` — the launched URL carries an init token;
  `handlePopupApprove` GET requires the token exchange.
- `pkg/claude/agentd/dashboard.go` — store moved out to `inittoken.go`;
  uses the generalised, dashboard-scoped helpers.
- `pkg/claude/agentd/tray.go` — "open approval" mints an approval-scoped
  token; "open dashboard" uses `initScopeDashboard`.
- `pkg/claude/agentd/popup_auth_flow_test.go` — new flow tests.

## Test scenarios (`popup_auth_flow_test.go`)

- `TestPopupAuth_RefusesBareGet` — bare GET `/approve/{id}` → 403.
- `TestPopupAuth_TokenExchange` — bogus token refused; valid token →
  303 + cookie; token single-use.
- `TestPopupAuth_TokenScopedToApproval` — approval A's token is rejected
  at approval B.
- `TestPopupAuth_DecideRequiresCookie` — POST without the cookie
  refused; POST after a successful exchange records the decision.

## Residual — accepted

The daemon embeds the init token in the URL handed to the browser, so
it lands in the browser launcher's argv. A same-user process that reads
`/proc/<pid>/cmdline` can still race the human's browser for the
single-use token. Winning that race means beating a browser the daemon
launches immediately, and losing — or a daemon restart — burns the
token.

Closing the window entirely means preventing a process from reading
another process's argv, which is a sandbox responsibility, not
tclaude's. This was a deliberate scoping decision.

## Considered and declined

The earlier version of this doc listed deeper options — native OS
dialogs (zenity / osascript / Win32) and tray-icon-mediated approval.
Both move the *decision* off loopback HTTP entirely and would close the
argv-scrape race, but at the cost of the rich HTML popup (body preview,
live countdown, +5min extend) and a per-platform dialog implementation.
Given the residual is a sandbox concern and the token exchange already
raises the bar substantially, these were declined as out of scope.
