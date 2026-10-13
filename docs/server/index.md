# Running tclaude on a server

tclaude runs just as well on a headless box (a VM, a home server, a cloud
instance) as on a laptop. The difference is only in who keeps it running and
how you reach it.

**tclaude does not install a login service for you.** There is no
`tclaude setup --install-service`. How `agentd` starts is your call: a systemd
user unit, a system unit under a dedicated account, a launchd agent, a unit
your config management templates, or `tclaude agentd serve` in a `tmux` or
`screen` window you leave behind. The pages here show the common shapes so the
choice is easy:

- [systemd](systemd.md): Linux user or system units, including cgroup
  delegation for per-agent resource limits.
- [launchd](launchd.md): a macOS LaunchAgent.
- [Ansible](ansible.md): a generic play that templates and enables the
  systemd user unit.

## What has to keep running

| Process | What it is | Who starts it |
|---|---|---|
| `agentd` | The daemon: API, dashboard, scheduler, spawns. `tclaude agentd serve`, or the standalone `tclaude-agentd serve` (same code; keep `tclaude` on `PATH` beside it). | You, via your supervisor. |
| The tclaude tmux server | `tmux -L tclaude`. Every agent pane lives in it. | Whatever first creates a session, normally `agentd` on the first spawn. You can also run it as its own unit (see [systemd](systemd.md#keeping-agents-alive-across-agentd-restarts)). |
| The harness CLIs | `claude`, `codex`, `opencode`, `copilot`, `gemini`, `pi`. One per agent, inside its pane. | `agentd`, through `tclaude session new`. They must be on the `PATH` that `agentd` and the tmux server see. |

`agentd` itself can stop and start without touching running agents. It picks
up the panes it finds on the tclaude tmux server. The tmux server is the part
whose death ends every agent. Agents whose panes died show as offline and come
back with Resume in the dashboard, or `tclaude agent resume <agent>`.

Paths that matter to a supervisor:

- `~/.tclaude/config.json`: settings. The `agent.*` keys mirror the
  `agentd serve` flags (`--help` names each one).
- `~/.tclaude/data/`: SQLite database and private daemon state.
- `~/.tclaude/api/agentd-socket/agentd.sock`: the socket agents and the CLI
  talk to.
- `~/.tclaude/output.log`: the daemon log, rotated by `agentd` itself.
- `$TMUX_TMPDIR/tmux-<uid>/tclaude` (default `/tmp/tmux-<uid>/tclaude`): the
  tmux socket.

## What a headless box changes

**No tray, no browser.** Pass `--no-tray` (or set `agent.disable_tray`).
`agentd` also skips the tray on its own when it can't reach one, but the flag
keeps the log quiet.

**A stable port and a token you can find.** With no terminal attached, the
startup banner does not print the operator token (`--print-human-token`
defaults to `auto`). Set a fixed port and persist the token so the CLI on the
server can authenticate as you:

```bash
tclaude agentd serve --no-tray --dashboard-port 7878 --persist-operator-token
```

`--persist-operator-token` writes a 0600 `~/.tclaude/data/operator_token` that
the CLI reads automatically. The config-file forms are `agent.dashboard_port`
and `agent.persist_operator_token`. Do not use `--print-human-token=always`
under a supervisor. It writes the bearer token to the journal or log file.

**Harness logins.** Log each harness in once, as the user `agentd` runs as,
from an SSH shell on the server. Most offer a flow that does not need a local
browser. For example, Claude Code prints a URL and takes a pasted code (or
`claude setup-token` for a long-lived token), and Codex has
`codex login --device-auth`. Check each CLI's own help for the others. If
another tclaude instance is already logged in and [federated](../federation-setup.md)
with this one, `tclaude harness credentials push` can copy credentials over.
Then confirm `agentd` can find the binaries:

```bash
tclaude harness ls
```

**Reaching the dashboard.** The dashboard listens on loopback. Pick one way in:

- **SSH tunnel.** Simplest, and nothing new is exposed. With the fixed port
  above:

    ```bash
    ssh -L 7878:127.0.0.1:7878 you@server
    tclaude agent dashboard --print     # run on the server, inside that SSH session
    ```

    Open the printed `http://127.0.0.1:7878/?init_token=…` URL in your local
    browser within about a minute.

- **A private network** such as Tailscale or WireGuard: bind the dashboard to
  that interface with `--dashboard-bind` (or `agent.dashboard_bind`). Its only
  gate is a cookie plus the operator token, so do this only on a network you
  already trust.
- **`tclaude remote-access`**: tclaude's own mTLS + passphrase HTTPS listener,
  for a phone or anything outside your private network. See
  [Operating remotely](../remote.md).

**Linking machines.** To have agents on this server talk to agents on another
machine, link the instances through a hub. See
[Federation setup](../federation-setup.md).

## Then open the dashboard

Once `agentd` runs under your supervisor of choice, open the dashboard (over
your tunnel or network) and work from there. Spawning, attaching to panes in
the browser terminal, approvals, and logs all work the same as on a desktop.
See [Dashboard](../dashboard.md).
