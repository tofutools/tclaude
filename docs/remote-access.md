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
`~/.tclaude/remote-access/`, never in `config.json`. Client **private** keys are
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
   `~/.tclaude/remote-access/clients/<name>.p12`) to the device and install it as
   a client certificate:
   - **iOS:** open the `.p12` → install the configuration profile (Settings →
     Profile Downloaded), then enter the `.p12` password.
   - **Android:** Settings → Security → *Install a certificate* → *VPN & app user
     certificate*.
2. Browse to `https://<machine-host-or-ip>:8443`. Accept the self-signed warning
   (LAN preset), pick the installed client certificate when prompted, then enter
   the passphrase.

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

## Caveats

- Very old devices may not import the modern `.p12` encryption profile; a legacy
  profile would be a follow-up.
- The **regular (desktop) dashboard** is served in full over the remote listener
  — every tab and API works from another computer. It is **desktop-first**,
  though: a mobile-optimized rendering, PWA install, and Web Push are deferred
  follow-ups.
