# Web Terminal

Access your Claude Code sessions from a phone or any browser over the network.

## How It Works

The web terminal mirrors an existing tmux-based Claude session via xterm.js and WebSocket. Both the desktop terminal and the browser see the same session simultaneously - you can watch and interact from either.

## Quick Start

```bash
# Start a Claude session first
tclaude

# In another terminal, start the web server
tclaude web
```

Credentials are auto-generated (username defaults to hostname, password is random). The bind address defaults to your local hostname, making it reachable on the LAN. The server prints the credentials and connection URL on startup.

## Connecting from Your Phone

Use `--qr` to display a QR code containing the full URL with embedded credentials:

```bash
tclaude web --qr
```

Scan the QR code with your phone camera and accept the self-signed certificate warning. You can also press **space** at any time to show the QR code.

To bind to all interfaces explicitly:

```bash
tclaude web --bind 0.0.0.0
```

### Mobile Features

On touch devices, the web terminal provides:
- **Input bar** at the bottom for text entry with proper autocorrect/IME support
- **Tab** and **Enter** buttons next to the input field
- **Extra keys panel** (ESC, arrow keys) toggled via the `⋮` button

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
| `-u, --user` | Username for basic auth | hostname |
| `--pass` | Password for basic auth | auto-generated |
| `--bind` | Address to bind to (tab-completable) | hostname |
| `--qr` | Show QR code for easy mobile connection | `false` |
| `--no-tls` | Disable TLS | `false` |
| `--new-cert` | Force regenerate TLS certificate | `false` |

## TLS Certificates

A self-signed TLS certificate is generated on first use and saved to `~/.tclaude/claude-web/`. The certificate includes your hostname and local IPs as SANs. It is reused across restarts (same fingerprint), so you only need to trust it once.

To trust the certificate in Chrome:
1. Navigate to `chrome://settings/certificates`
2. Import `~/.tclaude/claude-web/cert.pem` as a trusted authority

To regenerate the certificate:

```bash
tclaude web --new-cert
```

## Multiple Sessions

If you have multiple Claude sessions running, specify which one to serve:

```bash
# List sessions
tclaude session ls

# Serve a specific session
tclaude web abc123
```

If only one session is running, it auto-detects.

## Terminal Sizing

The terminal uses tmux `window-size latest`, so the most recently active client dictates the terminal size. When you interact from your phone, the terminal resizes to fit; the desktop terminal shows dot-fill in the unused area.

## Keyboard Shortcuts

While the server is running:
- **Space** - Show QR code
- **q** or **Ctrl+C** - Stop the server
