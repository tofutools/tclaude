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

Neither links two tclaude instances together; for agents on different
machines that need to reach each other, see [Federation](federation.md).

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

Client private keys exist only inside the one-time `.p12` you install on the
device — the host keeps no copy.

On a phone: install the `.p12` (iOS profile install; Android VPN & app user
certificate) and trust the generated CA on the device — the add-device flow
offers a CA download. With the CA trusted, the browser reaches
`https://host:8443` without warnings; pick the certificate when prompted and
enter the passphrase. Do not click through a certificate warning instead of
installing the CA: an untrusted server certificate on this setup means the
CA is missing from the device — or that something else is answering as the
dashboard and could capture the passphrase.
You get the full desktop dashboard — it is desktop-first, with no PWA or Web
Push on a self-signed LAN setup.

The Config › Daemon & network › Remote access mirrors the CLI: enable toggle, listen
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

### Live pages from another machine

Pages agents show in the dashboard work from another machine too: over the
remote-access listener, over a plain dashboard bind, and behind `tailscale
serve` or another TLS-terminating proxy. Each page is served under
`/p/<remote token>/` on the host you reached the dashboard at. No new ports
or settings are needed. On the regular dashboard listener a page's link is a
capability URL: anyone who can reach the listener and holds it sees the page
until it closes. On the remote-access listener the client certificate is also
required. Pages there share the dashboard's origin, so their isolation rests
on CSP path matching and on the dashboard API's Origin and cookie checks. A
dev server needs `--base /p/<remote token>/`, which `tclaude agent show`
prints. See [Live pages](live-page.md#viewing-pages-from-another-machine).

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

Plain HTTP this way is supported, including agent capture (`guide capture`) and
the copy buttons. A copy that has to fetch its text first, such as Copy Markdown
on a message, can be refused by the browser there, and says so. A drawing board's Ctrl V works there;
its right-click Paste can't read the system clipboard, and says so. Browser notifications cannot work there: browsers allow them
only over HTTPS or on localhost, and the dashboard says so where you would turn
them on. [Dictation](companion.md#voice-and-dictation) is the same: browsers
refuse the microphone there, and the mic is disabled and its tooltip says why. To
dictate, use `localhost` on the host, or on a tailnet put `tailscale serve` in
front of the bind and open the HTTPS address it gives you.

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
clear; gated on `groups.settings.remote-control-policy` or `groups.admin`; a group owner
has it for owned groups), or use `📱 remote policy` in the group's ⚙ menu. These are defaults, not locks — the agent's toggle still works
afterwards.

### Tracking

An armed agent's dashboard row shows the **📱** badge; clicking it opens the
live session in a web terminal, and the row's ⚙ menu carries the toggle. The
badge reflects tclaude's tracked, best-known state — the harness offers no
readback beyond the on-demand `status` probe. The armed state survives
relaunch: resume, reincarnate, and clone re-arm from the source agent's
last-known value.

### Opening the session in the Claude app

The agent's menu — the row's ⚙ menu on the desktop, and on a phone the menu
a touch and hold (or ⋯, or a terminal's ⋯) opens — offers **Open in Claude
app ↗**: a plain link to the session's `https://claude.ai/code/…` address.
On a phone with the Claude app installed the link opens the session in the
app; elsewhere it opens claude.ai in the browser. It is an alternative to
tclaude's own terminal, not a replacement.

The daemon learns the link by reading the footer pill, on demand only: when
`status` or a toggle reads the pane. The dashboard snapshot then carries the
last link read from the agent's live session (it is dropped when Remote
Access is switched, or a read finds it off), and never logs it. Where the
dashboard has no link yet, the entry reads:

- **Open in Claude app ↗** while Remote Access is on: the tap reads the pane
  once for the link, then opens it.
- **Turn on Remote Access & open** while it is off, on a running Claude Code
  agent of this node: the tap switches it on, waits up to 10 s for the link,
  then opens it. A toast says when it did not come up.

The tab is opened inside the tap and pointed at the link when it arrives,
because a browser opens tabs only from a tap; where the browser blocks it, the
toast offers a second **Open in Claude app ↗** tap.

The link controls the session for whoever is signed in to its Claude account,
so it is treated like an interactive terminal. A [federated](federation.md)
peer gets it, and with it the same Open entry, only for agents in groups where
it holds `sessions.attach` (or unrestricted trust); watching
(`sessions.watch`) or reading status is not enough. Operators often use one
Claude account across their own nodes, so the link opens from any of them;
claude.ai itself opens it only for that account. A peer's page never switches
Remote Access on.
