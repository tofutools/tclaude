# Remote Access (network-exposed dashboard)

Reach the **agentd dashboard** — the whole-fleet view (groups, agents, cron,
costs, messages) — from your phone or another machine over the network, behind
strong authentication.

> **This is different from [Remote Control](remote-control.md).** Remote
> *control* arms Claude Code's own per-session Remote Access (one agent's
> terminal + permission approvals, via claude.ai + the Claude app). Remote
> *access* (this page) exposes tclaude's **fleet dashboard** over the network —
> the multi-agent view Claude Code's app does not give you. They compose: arm
> agents for remote control, and reach the fleet switchboard via remote access.

By default agentd's dashboard is **loopback-only** (`127.0.0.1`) and reachable
through `tclaude agent dashboard`. Remote access adds a **separate HTTPS
listener** you opt into, which never weakens the loopback path.

## Security model

The dashboard is a network-exposed **agent control plane** — it can spawn and
kill agents, approve permissions, and inject keystrokes into panes. So the
remote listener is built to a public-internet bar and requires **two factors on
every request**:

1. **mTLS** — a client certificate (installed on your phone) issued by a CA that
   `tclaude remote-access setup` generates. Enforced at the TLS layer: a
   connection without a valid client cert is refused before any request is
   processed.
2. **A passphrase** — entered once on the device, which mints a signed,
   restart-surviving session cookie (30-day TTL). Login attempts are
   rate-limited.

All secret material — the CA, server and client keys, the passphrase hash
(PBKDF2-HMAC-SHA256), and the cookie-signing key — lives as `0600` files under
`~/.tclaude/data/remote-access/`, never in `config.json`. Client **private** keys are
only ever inside the one-time `.p12` you install on the device; the server keeps
the public client cert (for the record) but never the key.

This design means the three reachability options are the *same* hardened build:

- **LAN** (`0.0.0.0:PORT`) — zero-infra, same network only. Self-signed server
  cert ⇒ a one-time browser trust warning. (A self-signed cert is also not a
  "trusted secure context", so PWA-install / Web Push won't work on plain LAN —
  see the deferred phases.)
- **Mesh VPN** (Tailscale / WireGuard) — bind to the tailnet interface; reach it
  anywhere with no public exposure. `tailscale serve` can front a real cert.
- **Public tunnel** (Cloudflare Tunnel / ngrok) — bind loopback, let the tunnel
  terminate a real cert. mTLS + passphrase still apply end-to-end.

## Setup

```bash
tclaude remote-access setup --bind 0.0.0.0:8443
```

This generates the CA + server/client certs, prompts for a **passphrase** and a
**`.p12` password**, writes the first device's `.p12`, and sets
`remote_access.enabled` + `bind` in `config.json`. **Restart agentd** to start
the listener.

Extra cert SANs (a tailnet name, a tunnel hostname) — local IPs and the hostname
are always included:

```bash
tclaude remote-access setup --bind 0.0.0.0:8443 --host myhost.tailnet.ts.net
```

Add another device later (issues a new `.p12` from the existing CA without
rotating it):

```bash
tclaude remote-access add-client tablet
```

Inspect config + issued devices:

```bash
tclaude remote-access status
```

## On the phone

1. Transfer the `.p12` (printed by `setup`, under
   `~/.tclaude/data/remote-access/clients/<name>.p12`) to the device and install it as
   a client certificate:
   - **iOS:** open the `.p12` → install the configuration profile (Settings →
     Profile Downloaded), then enter the `.p12` password.
   - **Android:** Settings → Security → *Install a certificate* → *VPN & app user
     certificate*.
2. Browse to `https://<machine-host-or-ip>:<port>` — the `<port>` is whatever you
   passed to `--bind` (the examples here use `8443`). Accept the self-signed
   warning (LAN preset), pick the installed client certificate when prompted,
   then enter the passphrase.

Once unlocked you get the full dashboard. Delete the transferred `.p12` from the
phone's Downloads after install — the identity now lives in the OS keychain.

## Configuration

```jsonc
{
  "remote_access": {
    "enabled": true,
    "bind": "0.0.0.0:8443"
  }
}
```

Set `enabled` to `false` (or remove the block) and restart agentd to take the
listener down; the loopback dashboard is unaffected either way.

### From the dashboard

The dashboard's **Config tab** has a **Remote access** section that edits the
same `remote_access` block: an **enabled** toggle, a **listen interface** field,
and an **HTTPS port** (default `8443`). It shows a live status line — warning
when the toggle is on but no material has been generated yet, and reminding you
that starting or stopping the listener takes effect after an **agentd restart**.

Below it, a **certificate management** panel does what the `tclaude
remote-access` CLI does, from the browser:

- **First-time setup / regenerate** — generate the CA, server cert, first
  device and passphrase (and optionally enable the listener). Regenerate rotates
  the CA and **invalidates every installed device** — it asks for confirmation.
- **Add a device** — issue a new `.p12` from the existing CA and download it to
  install on the device; also download the CA cert.
- **Add host name(s)** — reissue the **server** cert (under the existing CA)
  with extra SANs so a new address — a public URL, a tailnet name — verifies
  cleanly. This is non-destructive: **installed devices keep working** (the CA
  is unchanged); restart agentd to serve the updated cert.
- **Cert valid for** — the server cert's current SAN list, so you can see at a
  glance which IPs/hostnames a device may dial without a name mismatch.

This panel is available on the loopback dashboard **and** over the remote
listener: a remote session has already cleared the client certificate and the
passphrase and is already a full control-plane operator, so cert management sits
at the same privilege tier. The passphrase / `.p12` passwords are entered into
the form, used immediately, and never stored beyond the existing `0600`
material on disk.

## Alternative: expose the loopback dashboard behind your OWN auth

The mTLS + passphrase listener above is the batteries-included path. But
sometimes you already have an authentication layer you'd rather reuse — a
reverse proxy with SSO, a mesh VPN, Cloudflare Access, an identity-aware proxy.
In that case it's more natural to expose the **local** dashboard endpoint and
let your layer gate it, than to enable the second (mTLS) listener.

For that, agentd lets the loopback dashboard bind to a **non-loopback host**:

| Priority | Where | Value |
|----------|-------|-------|
| 1 (highest) | `tclaude agentd serve --dashboard-bind <host>` | e.g. `0.0.0.0` |
| 2 | `agent.dashboard_bind` in `config.json` | e.g. `0.0.0.0` |
| 3 (default) | — | `127.0.0.1` (loopback only) |

```jsonc
{
  "agent": {
    "dashboard_bind": "0.0.0.0",   // host ONLY — the port is dashboard_port
    "dashboard_port": 8080
  }
}
```

It's a **host**, not a `host:port` — the port stays `dashboard_port` /
`--dashboard-port`. `0.0.0.0` (or `::`) exposes every interface; a specific IP
binds one. agentd logs a loud warning at startup whenever it binds non-loopback.

From the dashboard, set it in the **Config tab → Agent coordination →
"Dashboard bind"** field (right below "Dashboard port"). **Don't confuse it with
the Config tab's separate "Remote access → listen interface" field: that one is
`remote_access.bind`, the mTLS listener above, not this.**

> ⚠ **This puts the control plane on the network with only a cookie + operator
> token in front of it.** Only ever set it when your own auth (reverse proxy /
> VPN / IAP) actually fronts the port — otherwise anyone who can reach it can
> spawn/kill agents and approve permissions. If you want the daemon itself to do
> the authenticating, use the mTLS + passphrase listener above instead.

When bound non-loopback, agentd relaxes its same-origin CSRF check from the
fixed loopback URL to **host-relative** (the request's `Origin` host must match
the host it was served on) — the same model the mTLS listener uses — so the
dashboard works through whatever hostname your proxy presents, while the
`SameSite=Strict` session cookie keeps cross-site requests out. Default
(loopback) behaviour is unchanged.

## Human approvals over remote access

When an agent hits a permission-gated action it can't self-satisfy (an
`--ask-human` call, a cross-agent action), it **blocks** waiting for you to
decide. These requests appear in the dashboard's **Messages tab** under a
**🔐 Access requests** folder — each an Approve / Decline / Always-allow card —
and a blinking badge plus a top banner flag them from any tab. Because that
surface rides the dashboard's own auth, approvals are actionable **remotely**
over either path above (the mTLS listener or your own-auth bind), not just from
a browser on the host. (This replaces the old host-only browser popup.)

## Caveats

- Very old devices may not import the modern `.p12` encryption profile; a legacy
  profile would be a follow-up.
- The **regular (desktop) dashboard** is served in full over the remote listener
  — every tab and API works from another computer. It is **desktop-first**,
  though: a mobile-optimized rendering, PWA install, and Web Push are deferred
  follow-ups.
