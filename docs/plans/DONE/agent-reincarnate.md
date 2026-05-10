# Agent reincarnate — open follow-ups

`tclaude agent reincarnate [follow-up]` is shipped. Identity (group
memberships, permission grants, group ownerships) migrates from old
conv to new conv; old pane gets `/exit` injected; new pane optionally
gets a follow-up handoff message. Default `self.reincarnate` slug is
granted alongside the other self-lifecycle slugs.

The handoff sequence is detailed in `docs/plans/agent-coord.md` /
`docs/plans/agentd.md`. This file tracks **only open work** — for the
shipped behaviour see those docs and the DONE log.

## Open

- **Optional title preservation.** CC owns the conv title (set via
  `/rename` inside CC). It's not in our DB, so reincarnate can't carry
  it forward — the new agent has to self-rename in its follow-up if it
  cares. Could be wired up via a hook that captures the title on
  rename, or by parsing the conv jsonl. Not pulled by need.
- **Heavier alternative if a regression appears with switch-client.**
  Today the daemon runs `tmux list-clients -t <old>` and
  `tmux switch-client -c <tty> -t <new>` for each attached client
  before injecting `/exit` on the old pane (verified working). If a
  future regression breaks that path, the IPC fallback would be:
  signal the foreground `tclaude attach` process to kill its tmux
  subprocess and exec into a fresh
  `tclaude session attach <new-label>`. Not needed today; keep on the
  shelf.
- **Surface archived state in the dashboard.** Reincarnate strips
  groups + permissions on the old conv, so archived convs don't
  appear in dashboard Groups/Agents tabs by construction. Open
  follow-up: surface the `conv_index.archived_at` column in the tabs
  so archived convs are visually distinct (greyed out / faded) when
  the user explicitly opts in to view them. Verified once that
  `handleDashboardSnapshot` doesn't surface `-x` rows; re-check if
  user reports archived rows slipping through (e.g. partial migration).

## Files
- `pkg/claude/agentd/reincarnate.go` — orchestration body
- `pkg/claude/agentd/dashboard.go` — `handleDashboardSnapshot`
- `pkg/claude/common/db/agent_succession.go` — succession chain
