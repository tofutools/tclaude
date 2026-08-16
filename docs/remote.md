# Operating remotely

Two distinct capabilities let you operate away from the host, and they are
easy to conflate:

1. **Remote dashboard access** — tclaude's own hardened HTTPS listener that
   serves the full [dashboard](dashboard.md) to another machine or your
   phone, protected by mTLS client certificates plus a passphrase. Works for
   every harness, because it is a view onto `agentd`.
2. **Claude Code Remote Control** — Claude Code's built-in remote-session
   feature, reachable through claude.ai/code and the Claude mobile app.
   Claude Code only; tclaude arms and tracks it but ships no transport.

The first watches and operates your whole fleet; the second continues one
Claude Code conversation from your phone. They are independent — use either
or both.

## Remote dashboard access

By default the dashboard listens on loopback only. `tclaude remote-access`
adds a separate, opt-in HTTPS listener; the loopback dashboard is unchanged.
Every request must pass two factors:

- an **mTLS client certificate** — connections without one are refused at the
  TLS layer, before any application code runs;
- a **passphrase** — the login mints a signed session cookie with a 30-day
  TTL; login attempts are rate-limited. The passphrase is stored as a
  PBKDF2-HMAC-SHA256 hash (600k iterations).

### Setup

```bash
tclaude remote-access setup
```

This generates a CA (10-year) plus server and client certificates (2-year),
prompts for the passphrase and a `.p12` password, writes the first device's
`.p12` bundle, and sets `remote_access.enabled` and `bind` in config.json.
Restart `agentd` to apply. Defaults and knobs:

- `--bind` defaults to `0.0.0.0:8443`.
- `--host` adds extra SANs (DNS names or IPs the server cert should cover).
- `--client` names the first device.
- `--regenerate-certs` rotates the CA and invalidates every installed device.

Then per additional device:

```bash
tclaude remote-access add-client phone   # issue another .p12 from the CA
tclaude remote-access status             # config + issued devices
```

Material lives as 0600 files under `~/.tclaude/data/remote-access/`.

!!! note
    Some CLI help strings still print the legacy `~/.tclaude/remote-access/`
    path, which is only read as a fallback. The real location is
    `~/.tclaude/data/remote-access/`.

Client private keys exist only inside the one-time `.p12` you install on the
device — the host keeps no copy.

On a phone: install the `.p12` (iOS profile install; Android VPN & app user
certificate), browse to `https://host:8443`, accept the self-signed warning
(on a LAN), pick the certificate when prompted, and enter the passphrase.
You get the full desktop dashboard — it is desktop-first, with no PWA or Web
Push on a self-signed LAN setup.

The Config tab's Remote access panel mirrors the CLI: enable toggle, listen
interface, HTTPS port, first-time setup and regenerate, add device (with CA
download), and non-destructive addition of host SANs. The panel works over
the remote listener itself (same privilege tier), and admin actions are
recorded in the [audit trail](permissions-and-audit.md).

### Where to run it

The same hardened build fits three deployment shapes:

- **LAN** — bind `0.0.0.0:8443` and connect directly.
- **Mesh VPN** — bind the tailnet/VPN IP; `tailscale serve` can front it with
  a real certificate.
- **Public tunnel** — bind loopback and let the tunnel terminate its own TLS;
  mTLS and the passphrase still apply underneath.

### What is and isn't exposed

The full browser dashboard is served — every tab works, and human approvals
(Messages → 🔐 Access requests) are actionable remotely, replacing any need
to be at the host for `--ask-human` popups. One exception: the
[TUI dashboard](dashboard.md#the-tui-dashboard) client has no client-cert
options, so it cannot connect through this listener.

An alternative for infrastructure you already trust: bind the plain loopback
dashboard to a network interface via `agent.dashboard_bind` (host only; the
port stays `agent.dashboard_port`) or `tclaude agentd serve
--dashboard-bind`. That path has cookie and operator-token auth only, so use
it strictly behind your own reverse proxy, SSO, VPN, or IAP — `agentd` warns
loudly on a non-loopback bind. The localhost listener is kept alongside, and
this is also the supported network path for the standalone TUI dashboard.

## Claude Code Remote Control

Claude Code has a built-in remote-session feature: with the host logged in to
claude.ai (OAuth, not an API key), a running session can be paired to
claude.ai/code and the Claude mobile app, and you continue the conversation
from your phone. This is **Claude Code only** — the other harnesses have no
equivalent, tclaude's controls are hidden or rejected for them, and a
default-on policy is a silent no-op for a Codex agent. tclaude ships no
transport of its own; it arms, tracks, and defaults Claude Code's native
feature.

### Toggling it

```bash
tclaude agent remote-control            # toggle (the default verb)
tclaude agent remote-control on
tclaude agent remote-control off
tclaude agent remote-control status
```

The daemon injects Claude Code's `/remote-control` toggle into the agent's
pane. `status` reads the live pane's footer pill — answering "can I connect
right now": on, failed, or off — and self-heals the tracked flag; when the
pane is unreadable it falls back to the last-known value. The check runs on
demand only, never polled.

Toggling your own session needs the `self.remote-control` permission. A
manager toggles another agent with `--target <selector>` (title, conversation
id, or an 8+-character prefix), which needs the global `agent.remote-control`
slug or group ownership covering the target — the same manager pattern as
the other lifecycle verbs in
[Spawning and lifecycle](spawning-and-lifecycle.md). An agent without the
grant can fall back to `--ask-human <timeout>` (self only).

### At spawn

```bash
tclaude session new --remote-control
tclaude agent spawn mygroup --remote-control
```

or tick "Start with remote control" in the dashboard's spawn dialog. The
default is resolved once at spawn: explicit per-spawn value, then group
policy, then spawn-profile default, then off. Group policy is set with
`tclaude agent groups set-remote-control <group> optin|deny|inherit` (omit to
clear; gated on `groups.settings.remote-control-policy` or `groups.admin`,
human-only by default), and the dashboard shows a click-to-cycle policy chip
per group. These are defaults, not locks — the agent's toggle still works
afterwards.

### Tracking

An armed agent's dashboard row shows the **📱** badge; clicking it opens the
live session in a web terminal, and the row's ⚙ menu carries the toggle. The
badge reflects tclaude's tracked, best-known state — the harness offers no
readback beyond the on-demand `status` probe. The armed state survives
relaunch: resume, reincarnate, and clone re-arm from the source agent's
last-known value.
