---
name: agent-schedule
description: >-
  Schedule recurring nudges and check-ins via `tclaude agent cron {ls, add, rm,
  logs}`. The agentd scheduler ticks every 30s, discards offline ticks by
  default, and can opt jobs into durable offline delivery. Use
  to set up periodic status pings to peer agents (e.g. a
  Product Owner agent pinging workers every 10 minutes), self-check-ins, or any
  task that's currently a `/loop` or external cron. Self-targeted scheduling
  needs `self.schedule` (default-granted alongside the other self-lifecycle
  slugs); cross-agent scheduling needs `agent.schedule` OR being an owner of a
  group containing the target.
---

# Recurring scheduled jobs

You can ask the agentd scheduler to fire a message at a regular
interval — to yourself or to a peer in a shared group — without
spinning your own /loop. The scheduler ticks every 30 seconds, picks
up due jobs, and delivers them to recipients that are online at fire time.
Offline ticks are discarded by default so a crashed agent does not return to a
backlog of stale automated nudges. `--queue-when-offline` opts a job into the
durable inbox queue used by manual messages. A live pane temporarily blocked on
human input is still online: its accepted nudge waits until the pane is safe.

## Verbs

| Command                                                     | What it does                                                                          |
|-------------------------------------------------------------|---------------------------------------------------------------------------------------|
| `tclaude agent cron ls`                                     | List jobs visible to you (your own + jobs targeting you + jobs in groups you own)     |
| `tclaude agent cron add --target <sel> --interval 10m --body "..." [--subject "..."] [--name "..."]` | Schedule a new job. Defaults to self-target when `--target` is omitted and waits for the first due interval. Give the body inline with `--body`, or read it from a file with `--file <path>` (`--file -` reads stdin). |
| `tclaude agent cron add --cron "*/5 * * * *" --body "..."` | Same, but on a cron expression instead of a fixed interval (mutually exclusive with `--interval`). Standard 5-field syntax plus `@hourly`/`@daily`/…; evaluated in the daemon's local timezone unless prefixed `CRON_TZ=<zone>`. |
| `tclaude agent cron add --interval 10m --run-immediately --body "..."` | Opt into exactly one immediate first fire, then continue from that fire on the normal cadence. Omit the flag to wait. |
| `tclaude agent cron add --interval 10m --queue-when-offline --body "..."` | Persist ticks for offline recipients. By default offline ticks are discarded so stale scheduled nudges do not build up. |
| `tclaude agent cron rm <id>`                                | Delete a job by ID (from `cron ls`)                                                   |
| `tclaude agent cron logs <id> [--limit N]`                  | Show recent fires (newest first), including `ok`, `skipped_offline`, `partial_offline`, and failure statuses. |

## Permissions

| Slug              | Default-granted | What it covers                                                                                       |
|-------------------|-----------------|------------------------------------------------------------------------------------------------------|
| `self.schedule`   | yes             | Manage your own scheduled jobs (target == you).                                                      |
| `agent.schedule`  | no              | Manage jobs targeting ANOTHER agent. Group owners can manage jobs on members they own without this. |

`self.schedule` is seeded by `tclaude setup --install-default-agent-permissions`
alongside the other self-lifecycle default slugs (`self.rename`,
`self.compact`, `self.clone`, `self.remote-control`). Self-reincarnation needs no slug.

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
- **Jobs that need sub-minute precision.** Cron expressions are supported, but
  the scheduler checks every 30 seconds rather than promising second-level
  wall-clock delivery.

## Example — PO pings workers every 10 minutes

You're a Product Owner agent in group `team-alpha` with two workers
(`backend-worker` and `frontend-worker`). You want both pinged every
10 minutes so you can keep momentum without babysitting:

```bash
# Pings each worker while it is online; add --queue-when-offline to retain missed ticks.
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
Both jobs wait ten minutes before their first delivery. Add
`--run-immediately` when one immediate first ping is intentional. If a worker
is offline when a tick fires, that tick is discarded unless the job was created
with `--queue-when-offline`.

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

- **Default:** liveness is checked before persistence. An offline solo target
  gets no inbox row and the run records `skipped_offline`. A group job delivers
  to its online eligible members, recording `partial_offline` when it skipped
  others or `skipped_offline` when it skipped everyone.
- **With `--queue-when-offline`:** the scheduler inserts durable inbox rows for
  offline recipients too. The normal flush pipeline delivers them after the
  recipient returns.
- **Regular messages are unaffected:** peer, reply, operator, lifecycle, and
  other non-cron messages retain their own durable delivery semantics.

For reliable delivery to a target you don't share a group with: add
yourself + target to a shared group first, then schedule the job.

## Catch-up policy

If the daemon is down for a while and you have a job that fires every
10 min, the scheduler does NOT replay every missed slot on restart.
It fires once, resets `last_run_at` to now, and resumes the normal
cadence from there. No avalanche of backlogged messages.
