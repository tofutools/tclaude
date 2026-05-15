# Dashboard auto-launch on daemon startup

`tclaude agentd serve` can now open the browser dashboard for you on
startup, so you no longer have to run `tclaude agent dashboard`
separately after every daemon start.

## CLI / config surface

Two opt-ins that OR together — either one turns it on:

- `tclaude agentd serve --auto-launch-dashboard` — per-run flag.
- `agent.auto_launch_dashboard: true` in `~/.tclaude/config.json` —
  persistent default, so a service/autostart launch opts in without
  carrying the flag.

Off by default — a fresh daemon doesn't pop a browser tab uninvited.

## Implementation

- `config.AgentConfig.AutoLaunchDashboard bool`
  (`json:"auto_launch_dashboard,omitempty"`).
- `serveParams.AutoLaunchDashboard` — the `--auto-launch-dashboard`
  flag.
- `runServe` loads config and, once the popup listener is up
  (`popupBaseURL` set), calls `autoLaunchDashboard()` when
  `shouldAutoLaunchDashboard(flag, cfg)` is true.
- `autoLaunchDashboard()` mints a single-use init token in-process
  (`mintDashboardInitToken`) and opens `popupBaseURL/?init_token=…` —
  exactly the token-exchange URL the tray's "Open dashboard" click and
  `tclaude agent dashboard` produce. No socket round-trip through the
  human-only `/v1/dashboard/open`: the daemon IS the human side.
- Best-effort: a missing loopback listener (headless / failed bind) or
  a failed browser launch is logged and ignored — the daemon keeps
  running.
- `dashboardBrowserOpener` is a package var (defaults to `openBrowser`)
  so tests exercise the launch path without spawning a real browser.

## Files

- `pkg/claude/common/config/config.go` — `AutoLaunchDashboard` field.
- `pkg/claude/agentd/serve.go` — flag + call site.
- `pkg/claude/agentd/dashboard.go` — `shouldAutoLaunchDashboard`,
  `autoLaunchDashboard`, `dashboardBrowserOpener`.
- `pkg/claude/agentd/dashboard_autolaunch_test.go` — decision table +
  single-use-token URL + no-loopback-URL coverage.

## Notes

The flag could not live on `tclaude agent dashboard` (as first
phrased) — that command's whole job is to launch the dashboard. The
"don't launch it separately" intent belongs on the daemon's `serve`.
