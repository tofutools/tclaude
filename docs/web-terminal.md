# Web Terminal

Access your Claude Code sessions from a phone or any browser over the network.

## How It Works

The web terminal mirrors an existing tmux-based Claude session via xterm.js and WebSocket. Both the desktop terminal and the browser see the same session simultaneously - you can watch and interact from either.

## Quick Start

```bash
# Start a Claude session first
tclaude

# In another terminal, start the web server
tclaude web --user myuser --pass mypass

# Open https://localhost:8443 in your browser
```

## Accessing from Your Phone (LAN)

Bind to all interfaces so the server is reachable on your network:

```bash
tclaude web --user myuser --pass mypass --bind 0.0.0.0
```

Then open `https://<your-ip>:8443` on your phone and accept the self-signed certificate warning.

### WSL Users

With WSL2 mirrored networking (`networkingMode=mirrored` in `%USERPROFILE%\.wslconfig`), the server is directly reachable from other devices on your LAN.

If your Windows machine can't reach the server on the LAN IP (a known WSL2 quirk), use a TCP proxy from Windows (e.g. the `proxy` command from [tofu](https://github.com/GiGurra/tofu)):

```powershell
# On Windows (PowerShell)
# tclaude proxy 0.0.0.0:9443 localhost:8443
```

Then access `https://<windows-ip>:9443` from your phone.

## Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-p, --port` | Port to listen on | `8443` |
| `-u, --user` | Username for basic auth (required) | |
| `--pass` | Password for basic auth (required) | |
| `--bind` | Address to bind to | `127.0.0.1` |
| `--no-tls` | Disable TLS | `false` |
| `--new-cert` | Force regenerate TLS certificate | `false` |

## TLS Certificates

A self-signed TLS certificate is generated on first use and saved to `~/.tclaude/claude-web/`. The certificate is reused across restarts (same fingerprint), so you only need to trust it once.

To trust the certificate in Chrome:
1. Navigate to `chrome://settings/certificates`
2. Import `~/.tclaude/claude-web/cert.pem` as a trusted authority

To regenerate the certificate:

```bash
tclaude web --new-cert --user myuser --pass mypass
```

## Multiple Sessions

If you have multiple Claude sessions running, specify which one to serve:

```bash
# List sessions
tclaude session ls

# Serve a specific session
tclaude web --user myuser --pass mypass abc123
```

If only one session is running, it auto-detects.

## Terminal Sizing

The terminal resizes to the smallest connected client, so all viewers (desktop + phone) see the same content.
