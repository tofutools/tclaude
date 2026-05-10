# Popup transport hardening — residual /proc threat

## Today's approval popup security

- 32-hex-char unguessable approval ID in the URL (bearer token).
- Loopback-only listener (127.0.0.1) with explicit RemoteAddr check.
- HttpOnly + SameSite=Strict session cookie set on first GET,
  required on POST (defense-in-depth against CSRF and scraped-URL
  replay).
- Origin / Referer must point at the popup base URL.

## What's NOT closed

A same-user process can read `/proc/<browser-launcher pid>/cmdline`
to discover the popup URL, issue a GET to receive the Set-Cookie,
then POST `/approve/{id}/approve` itself. The popup endpoints have
no way to distinguish a browser client from a curl-as-the-same-user
attacker on a TCP socket — only Unix sockets give us peer
credentials, and browsers don't speak those.

Same-user processes are already an implicit shared trust boundary
(an attacker with same-user privs can talk to `agentd.sock` directly
via peer creds), so the popup doesn't open a new gap — but it also
doesn't close the existing one.

## Future work to actually fix this

- **Native dialogs.** Replace the loopback HTTP popup with platform
  dialogs (zenity / osascript / Win32 MessageBox). No URL exists to
  scrape. Loses the dashboard-reuse story (no shared port for the
  eventual GCP-IAM dashboard view), but the dashboard could keep
  loopback HTTP while approvals move out-of-band.
- **Tray-icon-mediated approve.** Pair the popup with the tray icon
  (see `system-tray-icon.md`): the popup's Approve/Deny buttons
  could *also* require a tray click within N seconds. Tray IPC is
  process-private to the daemon's GUI thread. Friction-heavier but
  raises the bar.
- **Don't pass URL via argv.** Launch the browser with a known
  origin and have the daemon hand the approval ID via a side channel
  the browser can fetch (e.g. a fixed welcome page that grabs a
  per-session ID via a cookie set on `127.0.0.1:<port>/`). Tricky:
  browsers still need *some* URL, and any URL has to land in argv
  somewhere. Marginal win.

## Files
- `pkg/claude/agentd/popup.go` — popup HTTP handlers + cookie auth
- `pkg/claude/agentd/tray.go` — tray icon (would gain mediation logic)
