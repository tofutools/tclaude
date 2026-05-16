---
name: agent-schedule
description: Schedule recurring nudges and check-ins via `tclaude agent cron {ls, add, rm, logs}`. The agentd scheduler ticks every 30s and fires due jobs as agent_messages (when sender + target share a group) or direct tmux send-keys (solo target). Use to set up periodic status pings to peer agents (e.g. a Product Owner agent pinging workers every 10 minutes), self-check-ins, or any task that's currently a `/loop` or external cron. Self-targeted scheduling needs `self.schedule` (default-granted alongside the other self-lifecycle slugs); cross-agent scheduling needs `agent.schedule` OR being an owner of a group containing the target.
---

# Recurring scheduled jobs

You can ask the agentd scheduler to fire a message at a regular
interval — to yourself or to a peer in a shared group — without
spinning your own /loop. The scheduler ticks every 30 seconds, picks
up due jobs, and delivers them via the same paths you use manually
(agent_messages + flush nudge for grouped targets, direct send-keys
for solo targets).

## Verbs

| Command                                                     | What it does                                                                          |
|-------------------------------------------------------------|---------------------------------------------------------------------------------------|
| `tclaude agent cron ls`                                     | List jobs visible to you (your own + jobs targeting you + jobs in groups you own)     |
| `tclaude agent cron add --target <sel> --interval 10m --body "..." [--subject "..."] [--name "..."]` | Schedule a new job. Defaults to self-target when `--target` is omitted. Give the body inline with `--body`, or read it from a file with `--file <path>` (`--file -` reads stdin). |
| `tclaude agent cron rm <id>`                                | Delete a job by ID (from `cron ls`)                                                   |
| `tclaude agent cron logs <id> [--limit N]`                  | Show recent fires (newest first), with status (`ok` / `send_failed` / `no_target`)    |

## Permissions

| Slug              | Default-granted | What it covers                                                                                       |
|-------------------|-----------------|------------------------------------------------------------------------------------------------------|
| `self.schedule`   | yes             | Manage your own scheduled jobs (target == you).                                                      |
| `agent.schedule`  | no              | Manage jobs targeting ANOTHER agent. Group owners can manage jobs on members they own without this. |

`self.schedule` is seeded by `tclaude setup --install-default-agent-permissions`
alongside `self.compact`, `self.reincarnate`, and `self.clone`.

## When to use

- **Periodic status pings.** Coordinator / Product-Owner agents
  pinging workers every 10–30 min: "what's blocking you, anything
  to escalate, do you need a peer pinged?". Lets you stay
  asynchronous without manually polling.
- **Self check-ins on long-running work.** "Every 30 min, remind me
  to commit and push if I haven't in a while." Self-targeted; no
  cross-agent permission needed.
- **Watchdog nudges.** "Every hour, check if any worker has been
  silent > 30 min and ping them."

## When NOT to use

- **One-shot delays** (e.g. "check this in 5 minutes"). Cron is for
  recurring work; for one-shots, use `/loop` or just sleep + act.
  v2 may add a one-shot bool flag.
- **Sub-30s intervals.** The scheduler tick is 30s, so the minimum
  interval is also 30s. The CLI rejects shorter values.
- **Jobs that need exact wall-clock timing** (e.g. "fire at 09:00
  daily"). v1 is interval-based only — no cron expressions yet.
  Use a host cron / systemd-timer for those.

## Example — PO pings workers every 10 minutes

You're a Product Owner agent in group `team-alpha` with two workers
(`backend-worker` and `frontend-worker`). You want both pinged every
10 minutes so you can keep momentum without babysitting:

```bash
# Self-pings the workers via the daemon — both are in your group, so
# routing goes through agent_messages + flush nudge.
tclaude agent cron add \
  --target backend-worker \
  --interval 10m \
  --name po-ping-backend \
  --subject "status check" \
  --body "What's the latest? Anything blocked? Need me to coordinate with anyone?"

tclaude agent cron add \
  --target frontend-worker \
  --interval 10m \
  --name po-ping-frontend \
  --subject "status check" \
  --body "What's the latest? Anything blocked? Need me to coordinate with anyone?"
```

Workers will receive these as inbox messages with the subject
auto-prefixed: `[cron:po-ping-backend] status check`. The prefix
makes it obvious this is a scheduled nudge vs a hand-typed message.

### Long or code-heavy job body — use `--file`

Instead of `--body "..."`, pass `--file <path>` to read the job's
message body from a file (`--file -` reads stdin). `--body` and
`--file` are mutually exclusive. Reach for `--file` whenever the body
is long, multi-line, or contains code: a `--body` string typed on the
command line goes through the shell first, and **backticks in it are
eaten** — `` `cmd` `` runs as a command substitution before tclaude
sees it. A body read from a file is taken verbatim, so it is the safe
choice for any non-trivial cron message.

```bash
tclaude agent cron add --target backend-worker --interval 30m \
  --name nightly-brief --file /tmp/cron-brief.md
```

To check execution history:

```bash
tclaude agent cron ls         # see all your scheduled jobs
tclaude agent cron logs 3     # see recent fires for job #3
tclaude agent cron rm 3       # de-schedule when you're done
```

## Manager pattern (cross-agent)

Same as the rest of the lifecycle verbs. To schedule a job on
ANOTHER agent (instead of yourself):

```bash
# Requires agent.schedule, OR being an owner of a group containing the target.
tclaude agent cron add \
  --target some-other-agent \
  --interval 30m \
  --body "Self-check: do you still know your task?"
```

Group owners get this for free on members of groups they own — the
same "implicit power" rule used by `agent.reincarnate`,
`agent.compact`, `agent.rename`, etc.

## Delivery semantics

- **Group-routed (sender + target share a group):** scheduler
  inserts an `agent_messages` row and triggers the existing flush
  pipeline. The message survives the target being offline — it'll
  land next time the target's pane is alive.
- **Solo (no shared group):** scheduler types the body directly via
  `tmux send-keys`. **Requires the target's pane to be alive at fire
  time.** When it isn't, `cron logs` shows `no_target` for that fire
  — the message is lost (no inbox row to retry from).

For reliable delivery to a target you don't share a group with: add
yourself + target to a shared group first, then schedule the job.

## Catch-up policy

If the daemon is down for a while and you have a job that fires every
10 min, the scheduler does NOT replay every missed slot on restart.
It fires once, resets `last_run_at` to now, and resumes the normal
cadence from there. No avalanche of backlogged messages.
