# Diagnostics tab

**Diagnostics** is where you find out what happened or why something isn't working.
It has four subtabs, each with its own address: **Audit** (`/diagnostics/audit`, the
default), **Logs**, **Performance** and **Doctor**. A blue or red badge on the tab
means Doctor has something for you.

Old bookmarks to `/audit`, `/logs`, `/debug`, `/doctor` and `/activity/...` still
work. The command palette has an entry for each subtab. On a peer's page, Audit needs
that feature, and the others need unrestricted trust.

## Doctor

What this host and install still need for tclaude to work properly. Start here when
something doesn't work. The default **Needs attention** view shows what matters, and
**All checks** shows everything. The sidebar groups checks into **Decisions**,
**Sandbox**, **Tools**, **Harnesses** and **Hooks & setup**.

- 🔵 **decision**: a question only you can answer, such as the default sandbox for
  new agents (see [The install default](sandboxing.md#the-install-default)). Answer
  it in place.
- 🔴 **blocking**: something stops tclaude or spawns, such as missing `tmux`, no
  harness, or tclaude's sandbox being the default on a host where it can't run.
- 🟡 **warning**: degraded behavior, such as missing harness hooks.
- ⚪ **info** and ✅ **ok**: optional extras and passing checks.

A fix is a command to **copy** (`sudo apt install bubblewrap`, `tclaude setup`) or a
button. The view never changes the host by itself. Results are cached for a minute;
**Re-check** refreshes them.

## Audit

The trail of **changes** made through the daemon: spawns, messages, lifecycle actions,
group and permission changes, sudo grants, proxy calls and power actions. Read-only
polling isn't recorded.

Columns are When, Actor (operator, agent, tclaude or unknown), Action (⊞ means it came
from the dashboard), Target, Detail and Outcome (ok, denied, rejected or err). **Denied
attempts are included.** Spawn rows have a `?` with the request and resolved
parameters. Search and filter by source and outcome.

Records are kept for 30 days (`audit.retention_days`; negative keeps them forever).
See [Permissions and audit](permissions-and-audit.md) for what's recorded.

## Logs

A paged view of `~/.tclaude/data/output.log`, with level pills, structured fields,
search, a minimum level, a since filter, and a 2-second live stream. Turn on
**rotated** to include rotated files.

**⤓ export** downloads every line matching your filter as JSON lines, oldest first. It
ignores paging and the view's 64 MiB limit, so it can cover more than the tab shows.

### Reading a 🚫 refusal

The 🚫 [badge](dashboard-groups.md#badge-glossary) means the daemon refused a callback
from a session. Each refusal logs a `broker: refused a brokered callback` warning, with
the caller's process, the row it resolved to, the ID it claimed, and which proof was
missing. Repeats within ten seconds are counted, not logged again.

Two related lines are about timing, not refusal:

- `broker: caller exited before the daemon handled its callback` means the caller left
  first. No badge is set, because a later callback replaces it. It's usually INFO:
  harnesses drop callbacks when they redraw or exit. It's a WARN only when the caller
  had waited at least half its time budget, which points at a slow daemon.
- `broker: slow brokered request` is written when a callback takes over a second, and
  breaks the time down by phase.

## Performance

**⏱ Profile daemon…** captures CPU, heap, goroutine, block and mutex profiles and a
trace of this agentd. Each capture lists its CPU split and links to pprof's web UI.
**zip** downloads it, and **share…** sends its files to an agent. See
[Profiling agentd](profiling.md).

Below that is poll latency per endpoint: p50, p90, p99, max, a sparkline and a
breakdown by phase. It resets when the daemon restarts or with the reset button.
