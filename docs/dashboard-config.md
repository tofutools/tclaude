# Config tab

The **Config** tab edits tclaude's settings file,
`~/.tclaude/data/config.json`, in a form. Use this page to find where a setting
lives. Each section has its own address, such as `/config/notifications`. Bare
`/config` opens the section you last had open in this browser tab.

## Using it

- **Search settings…** filters rows by label, description, key or option. Every
  word must match, the rail shows hits per section, and Esc clears it.
- **More…** on a row shows the full explanation.
- **Save changes…** shows a line diff before writing anything. Dots on the rail mark
  sections with unsaved changes. **Reload** throws away your edits.
- **Badges** on a row say when a change takes effect or what it covers:
  `agentd restart`, `refresh` (next dashboard refresh), `next launch`, `Claude Code`
  (that harness only), `deprecated`, `irreversible`.

## Appearance

`/config/appearance`: how the dashboard looks.

| Group | Label | Key | Default |
|---|---|---|---|
| Activity bots | Regular mode, Wizard mode 🧙, Slop mode 🎰 | `dashboard.activity_bots.*` | emoji; emoji; sprites |
| Tabs | Plugins tab | `dashboard.always_show_plugins_tab` | off |
| Tabs | Vegas music in regular mode | `slop.vegas_in_regular_mode` | off |
| Header & rows | Horizontal scroll | `dashboard.hscroll_follow` | on |
| Header & rows | Group quick options | `dashboard.group_quick_options` | `hover` |
| Overlays | Preview background | `dashboard.overlay_backdrop` | `dim`; also `blur`, `both`, `none` |
| Overlays | Window controls | `dashboard.window_controls` | `auto`; also `macos`, `windows`, `linux`, `classic` |
| Header & rows | Group descriptions (deprecated) | `dashboard.show_group_description` | off |
| Header & rows | CI on finished PRs | `dashboard.show_final_pr_checks` | off |
| Header & rows | Agent hide button | `dashboard.show_agent_hide_button` | off |
| Header & rows | Toolbar follows the clicked group | `dashboard.fused_toolbar_fixed_node` | on (key is `false`) |
| Header & rows | Collapse empty Usage views | `dashboard.usage_collapse_subviews` | off |
| Header & rows | Open PRs indicator | `dashboard.always_show_open_prs`, `dashboard.recent_pr_window_days` | on; 3 days |
| Slop mode 🎰 | Hide the pull lever | `slop.hide_pull_lever` | off |

## Terminals & windows

`/config/terminals`: which terminal opens, and how windows and web terminals behave.

| Group | Label | Key | Default |
|---|---|---|---|
| Terminal | Terminal | `terminal` | auto-detect |
| Terminal | Default terminal | `dashboard.default_terminal` | `web` |
| Terminal | Directory picker | `dashboard.default_directory_picker` | `web` (remote dashboards always use it) |
| Terminal | TUI color scheme | `tui.color_scheme` | `default` |
| Window focus | Window title | `focus.window_title` | on |
| Window focus | Raise only | `focus.raise_only` | off |
| Window focus | Auto-tile | `focus.tile.enabled` | off |
| Window focus | Tiling layout | `focus.tile.layout`, `.resize`, `.gap`, `.margin` | `grid`; off; 8; 0 |
| Terminals tab | Stay after last close | `dashboard.stay_on_terminals_after_last_close` | off |
| Web terminal attachment | Resize strategy | `dashboard.terminal_attach.mode` | `repair` |
| Web terminal attachment | Wait times | `dashboard.terminal_attach.*_ms` | 0; 250; 250 |
| Web terminal tabs | Auto-close tabs | `dashboard.close_terminal_tabs` | Shell tabs close when their session ends |

## Voice

Spoken phrases and the opt-in hands-free settings. See
[Voice and dictation](companion.md#voice-and-dictation).

| Setting | Key | Default |
|---|---|---|
| Send phrase | `dashboard.voice.phrases`, action `send` | on, `send now` |
| Wake phrase | `dashboard.voice.phrases`, action `wake` | off, `hey tofu`; must differ from the send phrase while both are on |
| Mark dictated messages | `dashboard.voice.mark_dictated` | on |
| Comma at a pause | `dashboard.voice.comma_pause_seconds` | off; 0.7 seconds when on, 0.3 to 10 |
| Period at a pause | `dashboard.voice.period_pause_seconds` | off; 1.5 seconds when on, 0.3 to 10 |
| Stop phrase | `dashboard.voice.phrases`, action `stop` | on, `stop listening`; ends continuous mode |
| Continuous mode | `dashboard.voice.continuous_enabled` | off |
| Continuous mode auto-stop | `dashboard.voice.continuous_idle_seconds` | 120; 30 to 3600 |
| Wake phrase listener | `dashboard.voice.wake_enabled` | off; needs the wake phrase on too |
| On-device speech | in this browser, not saved in config | not installed |
| Send after silence | `dashboard.voice.silence_send_seconds` | off; 2 to 30 seconds |

## Notifications

`/config/notifications`: when and how tclaude tells you an agent needs attention.
Everything still lands in [Messages](dashboard-messages.md). See
[Notifications](notifications.md).

| Group | Label | Key | Default |
|---|---|---|---|
| Delivery | Desktop notifications | `notifications.enabled` | off |
| Delivery | Deliver via | `notifications.delivery` | `os` |
| Delivery | Cooldown | `notifications.cooldown_seconds` | 5 |
| What raises a banner | Notify me when an agent… | `notifications.transitions` | idle, permission, input, error, exit |
| What raises a banner | Human messages | `notifications.human_messages` | on |
| What raises a banner | Access requests | `agent.access_request_auto_open_browser`, `agent.access_request_system_notification` | off; off |
| What raises a banner | Presented PRs | `agent.present_pr_notification` | off |
| Advanced | Raw transition rules | `notifications.transitions` | same list |
| Advanced | Notification command | `notifications.notification_command` | OS default |

## Agents & spawning

`/config/agents`: joining groups, spawn guardrails, naming and default permissions.

| Group | Label | Key | Default |
|---|---|---|---|
| Terminal startup groups | Auto-join | `session.auto_join_group` | on |
| Terminal startup groups | Auto-create | `session.auto_join_or_create_group` | off |
| Spawn guardrails | Spawn restriction | `agent.spawn_group_restriction` | on |
| Spawn guardrails | Spawn allowed groups | `agent.spawn_allowed_groups` | empty |
| Spawn guardrails | Spawn rate limit | `agent.spawn_max_per_hour` | 10 (0 is unlimited) |
| Spawn guardrails | Clone cooldown | `agent.clone_cooldown` | 1m |
| Naming | Normalize spawn names | `agent.spawn_name_normalize` | on |
| Naming | Name the tmux session | `agent.spawn_label_from_name` | off |
| Permissions | Default permissions | `agent.default_permissions` | empty |

## Context & sessions

`/config/context`: compaction, nudges, resume prompts and transcript retention.

| Group | Label | Key | Default |
|---|---|---|---|
| Compaction | Pre-compact guard | `pre_compact_guard.enabled`, `.block_manual`, `.thresholds` | off; off; 150k of 200k, 800k of 1M |
| Compaction | Context nudge | `agent.context_nudge.enabled`, `.min_pct`, `.interval_pct` | off; 30; 10 |
| Sessions | Resume-from-summary prompt | `claude_resume.threshold_minutes`, `.token_threshold` | Claude Code's 70; 100000 |
| Sessions | Transcript retention | `claude_cleanup_period_days` | Claude Code's 30; written to `~/.claude/settings.json` |

## Usage & costs

`/config/costs`: cost display, estimates and rate limits. **Open usage limits &
budgets…** opens the limits manager. See [Usage](dashboard-usage.md).

| Group | Label | Key | Default |
|---|---|---|---|
| Cost display | Cost display multipliers | `cost.estimate_factor`, `cost.harness_factors.*` | 1; blank inherits |
| Cost display | Show cost on subscription | `cost.show_on_subscription` | off |
| Cost display | Gemini billing | `cost.gemini_billing` | `auto` |
| Cost display | OpenCode legacy long-context cutoff | `opencode.legacy_long_context_pricing_cutoff` | 272000 |
| Subscription usage | Poll Anthropic usage API | `usage.poll_anthropic_api` | off |
| Subscription usage | Idle timeout | `usage.idle_timeout` | 72h |
| Rate limits | Rate-limit gating | `ratelimit` | off |
| Rate limits | 5-hour window, 7-day window | `ratelimit.five_hour_percent_max_used`, `ratelimit.seven_day_percent_max_used` | 99; 99.9 when on |

## Ask & scribe

`/config/assistants`: defaults for [`tclaude ask`](ask.md) and for scribe agents.

| Group | Label | Key | Default |
|---|---|---|---|
| tclaude ask | Ask — profile | `ask.profile` | none |
| tclaude ask | Ask — sandbox | `ask.sandbox` | `global` |
| tclaude ask | Ask — model | `ask.model` | `sonnet` |
| tclaude ask | Ask — effort | `ask.effort` | `medium` |
| Scribes | Scribe — profile | `scribe.profile` | harness default |

Model and effort are disabled while a profile is selected. For **Ask — sandbox**
options, see [Ask sandbox policy](ask.md#sandbox-policy).

## Steward

`/config/steward`: only shown with Collab on. See
[What the Steward may do](companion.md#what-the-steward-may-do).

| Group | Label | Key | Default |
|---|---|---|---|
| Access | Reads | `steward.reads` | `operator` |
| Access | Writes | `steward.writes` | `messaging` |
| Access | Default permissions | `steward.default_permissions` | empty |
| Launch | Spawn profile | `steward.profile` | global default profile |
| Knowledge base | Source download | `steward.source_download` | `ask` |
| Name and startup | Name | `steward.name` | Steward |
| Name and startup | Startup prompt | `steward.startup_prompt` | empty |

## Daemon & network

`/config/daemon`: how agentd starts, where the dashboard listens, and remote access.
Rows with the `agentd restart` badge need one.

| Group | Label | Key | Default |
|---|---|---|---|
| Startup | Auto-launch dashboard | `agent.auto_launch_dashboard` | off |
| Startup | System tray | `agent.disable_tray` | tray shown |
| Local dashboard | Dashboard port | `agent.dashboard_port` | random free port |
| Local dashboard | Dashboard bind | `agent.dashboard_bind` | `127.0.0.1` |
| Security | Persist operator token | `agent.persist_operator_token`, `agent.persist_operator_token_keychain` | off; off |
| Security | Broker limits | `broker.enforce_limits` | off |
| Remote access | Remote dashboard | `remote_access.enabled` | off |
| Remote access | Listen interface, HTTPS port | `remote_access.bind` | `0.0.0.0:8443` |

Certificate management is below the Remote access group. See
[Operating remotely](remote.md).

## Logs & housekeeping

`/config/logs`: log verbosity and rotation, and cleanup of retired agents.

| Group | Label | Key | Default |
|---|---|---|---|
| Logging | Log level | `log_level` | `info` |
| Logging | Startup timings | `startup_timing` | off |
| Logging | Record hooks | `record_hooks` | off |
| Log rotation | Max log size | `log_rotation.max_size` | 10 MiB (0 disables) |
| Log rotation | Keep | `log_rotation.keep` | 5 |
| Retired-agent cleanup | Retired-agent auto-cleanup | `agent.retired_cleanup.enabled`, `.after_days` | off; 365 days |

Auto-cleanup is irreversible.

## Experimental

`/config/experimental`: feature flags, under **Feature flags**.

| Label | Key | Default | What it adds |
|---|---|---|---|
| Processes | `features.processes` | off | [Processes](processes.md) |
| Collab | `features.collab` | off | [Companion, Steward](companion.md) and [boards](boards.md) |
| Triggers | `features.triggers` | off | [Triggers](dashboard-automations.md#triggers), and spawn cron jobs |
| Groups Route map | `features.groups_route_map` | off | [Group routes](group-routes.md) map view |
| Group attachments | `features.group_attachments` | off | A group's link or attachment panel |
| Command palette in terminals | `features.terminal_command_palette_shortcut` | off | Ctrl/Cmd-K inside a terminal |
| Recorded sandbox details | `features.recorded_sandbox_details` | off | A `›` details link on the sandbox badge |
| Agent dirs: mount parent | `features.agent_dirs_mount_parent` | **on** | See [Sandboxing](sandboxing.md) |

## Advanced

`/config/advanced`: **Sudo (raw JSON)** edits `agent.sudo`.

## Settings with no form

These keys only exist in `config.json`. The form keeps them when it saves.

- Top level: `status_snapshot`, `host`, `experimental_harnesses`, `conv_watch`,
  `session_watch`, `audit`, `federation`, `updates`, `snapshots`, `routes`.
- `agent`: `branch_history_pr_enrichment`, `resource_delegation_dir`, `morph`,
  `spawn_legacy_injection`, `spawn_inline_max_chars`, `message_inline_max_chars`,
  `git_proxy`, `linear_proxy`, `awb_proxy`, `http_proxies`, `model_proxy_disabled`.
- `session`: `tmux_name_style`, `auto_name_from_prompt`.
- `notifications`: `known_types`.
