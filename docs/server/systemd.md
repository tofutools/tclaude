# systemd

These units are examples to copy and adapt. tclaude does not install them.
Read [Running tclaude on a server](index.md) first for what has to keep
running and how you reach the dashboard.

## A user unit with lingering

A user unit runs `agentd` as you, with your `~/.tclaude`, your harness logins,
and no root. Lingering keeps your user manager, and so the unit, running when
you are not logged in.

`~/.config/systemd/user/tclaude-agentd.service`:

```ini
[Unit]
Description=tclaude agentd

[Service]
Type=simple
EnvironmentFile=%h/.config/tclaude/agentd.env
ExecStart=%h/.local/bin/tclaude agentd serve --no-tray --dashboard-port 7878 --persist-operator-token
# Stop only agentd, not the tmux server it started (see below).
KillMode=process
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Point `ExecStart` at wherever `tclaude` is installed (`command -v tclaude`).
`tclaude-agentd serve` works the same if you installed the standalone daemon,
as long as `tclaude` is on the unit's `PATH` too.

```bash
sudo loginctl enable-linger "$USER"
systemctl --user daemon-reload
systemctl --user enable --now tclaude-agentd.service
systemctl --user status tclaude-agentd.service
```

## PATH and environment

A unit does not read your shell profile. Its `PATH` is the user manager's
default, which usually lacks `~/.local/bin`, `~/go/bin`, npm's global prefix,
and mise or asdf shims. `agentd` resolves the harness binaries on its own
`PATH`, and every agent pane inherits the environment of whatever started the
tmux server. In the unit above, that is `agentd`.

The simplest fix is to capture the `PATH` your login shell already has, once,
into the `EnvironmentFile`:

```bash
mkdir -p ~/.config/tclaude
printf 'PATH=%s\n' "$PATH" > ~/.config/tclaude/agentd.env
```

Check what you got: it should include the directory of each harness CLI
(`command -v claude codex opencode copilot gemini`) and, for mise, its shims
directory (usually `~/.local/share/mise/shims`), not only `mise` itself. Add
other variables your harnesses need (proxy settings, `CODEX_HOME`) to the same
file. Keep secrets out of it unless the file is 0600.

After the unit is up, `tclaude harness ls` lists what the daemon actually
finds on its `PATH`.

## Restarts, and whether the agents survive them

The agents live in the tclaude tmux server, not in `agentd`. A new `agentd`
picks up the panes that are still running. Whether those panes are still
running depends on the unit's cgroup:

- With systemd's default `KillMode=control-group`, stopping or restarting the
  unit kills everything in its cgroup. That includes the tmux server `agentd`
  auto-started on the first spawn, and so every agent.
- With `KillMode=process`, as above, only `agentd` is stopped. The tmux server
  stays up, and the restarted `agentd` finds the agents where they were.
  systemd logs a "left-over process" notice on the next start. That notice is
  expected here.

For the policy, `Restart=on-failure` with a short `RestartSec` is the right
default. `tclaude update --apply` re-executes `agentd` in place (same PID), so
an update does not look like an exit to systemd.

If the tmux server itself dies, or the machine reboots, the agents are gone.
They show as offline. Resume them from the dashboard or with
`tclaude agent resume <agent>`.

## Journal hygiene

The unit's stdout and stderr go to the journal
(`journalctl --user -u tclaude-agentd`). `agentd` keeps its own rotated log
in `~/.tclaude/output.log` as well, and the dashboard's Diagnostics › Logs view reads that.

Without a terminal, the startup banner leaves the operator token out
(`--print-human-token` defaults to `auto`). Do not set
`--print-human-token=always` in a unit. It writes the bearer token to the
journal, where other local users may be able to read it. Use
`--persist-operator-token` and let the CLI read the 0600 file instead.

## Hardening options to avoid

- **`PrivateTmp=yes`**. The tmux socket lives under `/tmp/tmux-<uid>/` (or
  `$TMUX_TMPDIR`). With a private `/tmp`, `agentd` cannot see a tmux server
  started outside the unit, and anything outside the unit cannot see the one
  inside it. Your shell's `tclaude session attach` would then talk to a
  different server than the daemon. The sandbox's capability probe also falls
  back with a warning when it cannot reach the server. See
  [where the probe runs](../sandboxing.md#where-the-sandbox-capability-probe-runs).
  If you need a private `/tmp`, set the same absolute `TMUX_TMPDIR` for
  `agentd`, the tmux server, and your shell.
- **`ProtectControlGroups=`** makes `/sys/fs/cgroup` read-only, and then
  per-agent cgroups cannot be joined. Leave it off if you use resource limits.
- **`ProtectHome=`**. `agentd` needs your home directory: `~/.tclaude`, the
  harness config and credential directories, and your repositories.

## Cgroup delegation (resource limits)

Delegation matters only if you use the sandbox's
[resource limits](../sandboxing.md) (`memory`, `cpu`, `pids` on a profile),
the `resource-only` sandbox implementation, or per-agent accounting. tclaude
then creates one cgroup v2 node per agent launch. It needs a delegated
subtree that it can write. Without one, a launch with authored limits, or
under `resource-only`, is refused, and the error names the `Delegate=` line
it wants. tclaude's own sandbox without limits only degrades to a notice. If
you use none of these, you can skip this section.

First check that your user manager has the controllers to hand down:

```bash
cat /sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/cgroup.controllers
```

If `cpu` or `memory` (or `pids`, if you set PID limits) is missing, add a
drop-in with `sudo systemctl edit user@.service`:

```ini
[Service]
Delegate=cpu memory pids
```

Then log out and back in, or reboot. tclaude supports two layouts.

### Delegating to the agentd unit

Add this to the `[Service]` section of `tclaude-agentd.service`:

```ini
Delegate=cpu memory pids
DelegateSubgroup=tclaude-supervisor
OOMPolicy=continue
```

`DelegateSubgroup=` (systemd 254+) puts `agentd` and its children in a
`tclaude-supervisor` child, which keeps the unit's own cgroup process-free.
cgroup v2 requires that before tclaude can create per-agent nodes beside it.
`OOMPolicy=continue` keeps systemd from stopping the whole unit when one
agent hits its own memory limit. The default, `stop`, would take every agent
down with it.
tclaude finds the delegated node from its own cgroup, so no flag is needed.
`pids` is needed only for PID limits.

This is the compact option. The tmux server and every agent pane then live
inside the agentd unit, so `KillMode=process` is what keeps them alive
across a restart.

### Keeping agents alive across agentd restarts

The cleaner layout gives the tmux server its own long-lived, delegated unit,
and points `agentd` at that unit's cgroup. `agentd` can then be restarted or
upgraded freely, and the agent panes and their cgroups are never in its
cgroup.

`~/.config/systemd/user/tclaude-tmux.service`:

```ini
[Unit]
Description=tclaude tmux server

[Service]
Type=forking
EnvironmentFile=%h/.config/tclaude/agentd.env
ExecStart=/usr/bin/tmux -L tclaude start-server \; set-option -g exit-empty off
ExecStop=/usr/bin/tmux -L tclaude kill-server
Delegate=cpu memory pids
DelegateSubgroup=server
OOMPolicy=continue
Restart=on-failure

[Install]
WantedBy=default.target
```

The panes inherit this unit's environment, so it gets the same
`EnvironmentFile`. `exit-empty off` keeps the server up with no agents on
it. Stopping this unit stops every agent.

Find the unit's cgroup path:

```bash
systemctl --user show -p ControlGroup --value tclaude-tmux.service
# e.g. /user.slice/user-1000.slice/user@1000.service/app.slice/tclaude-tmux.service
```

Then give `agentd` that path under `/sys/fs/cgroup`, and order it after the
tmux unit. In `tclaude-agentd.service`, drop `KillMode=process`, add the
`[Unit]` lines, and replace the `ExecStart=` line (a `Type=simple` unit allows
only one):

```ini
[Unit]
Requires=tclaude-tmux.service
After=tclaude-tmux.service

[Service]
ExecStart=%h/.local/bin/tclaude agentd serve --no-tray --dashboard-port 7878 --persist-operator-token \
  --resource-delegation-dir /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/tclaude-tmux.service
```

The same setting can come from `TCLAUDE_RESOURCE_DELEGATION_DIR` or
`agent.resource_delegation_dir` in `config.json`. The precedence is flag,
then environment, then config file. At startup `agentd` checks that the
directory is under `/sys/fs/cgroup` and provides `cpu` and `memory`. It also
checks that the tmux server is running inside it. In this mode `agentd` never
starts a tmux server itself, because one it started would land in its own
cgroup. If the tmux unit is down, startup and new sessions fail with an error
naming the unit.

## A system unit under a dedicated user

To run tclaude as its own account, not yours, use system units with `User=`.
systemd then sets `HOME` to that user's home. Install the units in
`/etc/systemd/system/`, replace `%h` with the account's home, and add
`User=tclaude` and `Group=tclaude` to each `[Service]` section. You no longer
need lingering. Use `WantedBy=multi-user.target`.

Everything else carries over. The delegated path becomes
`/sys/fs/cgroup/system.slice/tclaude-tmux.service`, and `Delegate=` on a
`User=` service hands that subtree to the account. The `user@.service` drop-in
above does not apply to system units. Two things change in
practice:

- Harness logins, `~/.tclaude`, and the socket all belong to that account. Run
  logins and CLI commands as it, for example
  `sudo -iu tclaude tclaude agent dashboard --print`.
- The repositories the agents work in must be readable and writable by that
  account.

## Then open the dashboard

With the unit running, open the dashboard. For example, over an SSH tunnel to
the fixed port:

```bash
ssh -L 7878:127.0.0.1:7878 you@server
tclaude agent dashboard --print
```

See [Reaching the dashboard](index.md#what-a-headless-box-changes) for the
other ways in, and [Dashboard](../dashboard.md) for what to do there.
