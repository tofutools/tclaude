# macOS launchd

On a Mac that stays on (a Mac mini in a closet, a build machine), a per-user
LaunchAgent keeps `agentd` running. tclaude does not install one for you. This
is an example to adapt.

`~/Library/LaunchAgents/com.example.tclaude-agentd.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.example.tclaude-agentd</string>

  <key>ProgramArguments</key>
  <array>
    <string>/opt/homebrew/bin/tclaude</string>
    <string>agentd</string>
    <string>serve</string>
    <string>--no-tray</string>
    <string>--dashboard-port</string>
    <string>7878</string>
    <string>--persist-operator-token</string>
  </array>

  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/Users/you/.local/bin:/Users/you/.local/share/mise/shims:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>

  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <!-- Leave the tmux server (and every agent) running when agentd exits. -->
  <key>AbandonProcessGroup</key>
  <true/>

  <key>StandardOutPath</key>
  <string>/Users/you/Library/Logs/tclaude-agentd.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/you/Library/Logs/tclaude-agentd.log</string>
</dict>
</plist>
```

Load it, check it, and restart it after edits:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.tclaude-agentd.plist
launchctl print gui/$(id -u)/com.example.tclaude-agentd
launchctl kickstart -k gui/$(id -u)/com.example.tclaude-agentd
```

To remove it, run `launchctl bootout gui/$(id -u)/com.example.tclaude-agentd`.

## Caveats

**PATH.** launchd gives jobs a minimal `PATH` (`/usr/bin:/bin:/usr/sbin:/sbin`)
and does not read your shell profile. Set the full `PATH` in
`EnvironmentVariables`. Include Homebrew, the directories of each harness CLI,
npm's global bin, and mise or asdf shims. The plist does not expand `~` or
`$HOME`, so write absolute paths. `printf '%s\n' "$PATH"` in a login shell
shows what you have. Agent panes inherit the environment of whatever started
the tmux server, normally `agentd`, so this `PATH` is theirs too.
`tclaude harness ls` shows what the daemon finds.

**Restarts.** `KeepAlive` with `SuccessfulExit` set to false restarts
`agentd` when it crashes, but not after a clean exit. `AbandonProcessGroup`
keeps launchd from cleaning up the job's processes when `agentd` stops. That
keeps the tmux server, and the agents in it, alive for the next `agentd` to
pick up. Agents lost to a reboot show as offline. Resume them from the
dashboard or with `tclaude agent resume <agent>`.

**Logs and the token.** `agentd` writes its own rotated log to
`~/.tclaude/output.log`. The `StandardOutPath` file only catches startup
output. Without a terminal, the startup banner leaves the operator token out.
Do not add `--print-human-token=always`, which would write the bearer token to
that log file. With `--persist-operator-token` the token goes to a 0600
`~/.tclaude/data/operator_token` that the CLI reads. Pass
`--persist-operator-token-keychain` to keep it in the login keychain instead.

**Tray.** A LaunchAgent in your GUI session can show the menu bar icon. Drop
`--no-tray` if you want it. On a Mac you only reach over SSH, keep the flag.

**No resource limits.** Per-agent cgroups are Linux only. On macOS, a profile
that authors `resource_limits` is refused by default. See [Sandboxing](../sandboxing.md).

**Harness logins** work as described in
[Running tclaude on a server](index.md#what-a-headless-box-changes). Run them
in a shell as the same user. On macOS, some harnesses keep their credentials
in the login keychain, which must be unlocked for the job to read it.

## Then open the dashboard

At the Mac, `tclaude agent dashboard` opens it. From elsewhere, use an SSH
tunnel to the fixed port, a private network, or `tclaude remote-access`. See
[Reaching the dashboard](index.md#what-a-headless-box-changes) and
[Dashboard](../dashboard.md).
