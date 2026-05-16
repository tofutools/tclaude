# Hook-driven agent actions (composable A2A wiring)

User-configurable rules that route Claude Code hook events into
agent coordination actions. The shape: **"on hook X, send
instruction Y to agent Z (or group:N)"** — composable, not a
hardcoded workflow.

## Motivation

Today the agentd daemon receives hook callbacks that update session
status (online/offline, idle/working, context_pct, etc.). That data
is used internally for the dashboard and for the (planned) context-
nudge thresholds, but **the user has no way to wire their own
behavior to those events**. A worker finishes a task → no way to
auto-ping the PO. A worker's context crosses 50% → only the
hardcoded nudge logic can react.

CC Agent Teams ships specific lifecycle hooks (`TeammateIdle`,
`TaskCreated`, `TaskCompleted`, `BeforeImplementation`). Useful in
isolation but **rigid** — the rules are baked in, not user-defined.

The flexibility the user wants: a small rule grammar where the
human (or eventually a coordinator agent) can declare *what
should happen when an event fires*, rather than the daemon
hardcoding the response.

## Rule shape (sketch)

Stored in `~/.tclaude/config.json` (or a sibling
`~/.tclaude/hook_rules.toml`) as a list of rules:

```toml
[[rule]]
when    = "hook.SubagentStop"            # event name (see below)
match   = { conv = "*", group = "tclaude-devs" }  # filter
action  = "agent.message"
target  = "tclaude-devs-product-owner"
body    = "Worker {{conv.title}} just finished a turn. Last commit: {{shell:git -C {{conv.cwd}} log -1 --oneline}}"

[[rule]]
when    = "hook.context_pct.crossed"
match   = { threshold = 50, group = "tclaude-devs" }
action  = "agent.message"
target  = "self"                         # the agent that crossed
body    = "Context at 50%; consider reincarnating at next breakpoint."

[[rule]]
when    = "hook.error"
match   = { tool = "Bash", exit_code = "!=0" }
action  = "agent.message"
target  = "group:tclaude-devs"           # broadcast
body    = "Bash failed in {{conv.title}} ({{conv.cwd}}): {{event.error}}"
```

Each rule is a `(when, match, action, target, body)` quintuple.
v1 keeps actions to a small allowlist:

- `agent.message` — send a message via the existing daemon path.
- `agent.compact` / `agent.reincarnate` — invoke lifecycle verbs.
- `cron.add` — schedule a one-shot or recurring nudge.

Anything more exotic ("run a shell command") deferred — the rule
engine is not a general workflow runtime, just an event router.

## Event sources

- **CC hooks** the daemon already receives: PostToolUse,
  Notification, SessionStart, Stop, SubagentStop. Surfaced as
  `hook.<name>` event keys.
- **Daemon-internal events**: context_pct crossed (paired with
  per-group thresholds, see
  [`per-group-reincarnate-thresholds.md`](per-group-reincarnate-thresholds.md)),
  agent online / offline, group member added / removed.
- **fsnotify-derived** (when [`fsnotify-monitor.md`](fsnotify-monitor.md)
  ships): rename, new conv, conv jsonl rewrite.

The same event broadcaster proposed in
[`dashboard-realtime-push.md`](dashboard-realtime-push.md) can serve
both: SSE handler sees events, rule engine sees events. Single
source of truth.

## Why this is the *flexible* version

Per the design preference saved in memory: tclaude is a tool, not
an opinionated framework. Hardcoded workflows like
"BeforeImplementation always notifies the lead" lock users into one
shape. A rule engine where the user expresses **their own** wiring
covers:

- "On worker idle, ping PO" — the original ask.
- "On Stop, run a status snapshot summary into a notes file" — via
  agent.message with a templated body.
- "On context crossing per-group threshold, message the agent (and
  log the nudge to the PO too)" — multi-target.
- "On error in a code-reviewer agent, automatically /compact and
  resume" — multi-action chain.
- "On SubagentStop for any agent in group `polecats`, call
  `agent stop --target <conv>`" — ephemeral auto-decommissioning
  workers (Gas-Town `polecat` pattern), composed from existing
  primitives rather than baked into the daemon.

Without baking any of those into the daemon source.

## Templating

Rule body uses a small `{{...}}` syntax:

- `{{conv.title}}`, `{{conv.cwd}}`, `{{conv.role}}` — caller's
  resolved fields.
- `{{event.<key>}}` — event-specific fields (e.g. `event.threshold`,
  `event.error`).
- `{{shell:<cmd>}}` — escape hatch (run a shell command, capture
  stdout). Sandboxed (no &&, no |). Use sparingly; this is the seam
  through which the rule engine could grow into a workflow runtime
  if we're not careful.

Open question: do we even need shell-escape in v1, or should
templating be strictly substitution? Lean **strict substitution
v1**, add shell-escape behind a slug if it shows up as a real need.

## Authority / safety

The rule engine runs in the daemon's context (no user sandbox).
That means rules can effectively perform any action a human could
on the daemon socket. Two guard rails:

- **Rules can only be added by humans** (no `claude` ancestor in
  the caller's process tree at write time). No agent self-rule-
  injection.
- **Each rule action goes through the existing slug machinery** —
  e.g. `agent.message` from a rule still needs to satisfy whatever
  permission gate would apply to a human calling that endpoint
  (i.e. it usually passes, since humans bypass). This means a rule
  cannot escalate beyond what its (human) author can do.

## CLI

`tclaude agentd rules ls | add | rm | test`. The `test` verb
dry-runs a rule against a synthetic event payload to preview body
expansion without firing the action — debug aid.

## Dashboard

A "Rules" tab (likely after the framework migration). Lists rules,
shows match counts + last-fire time, lets human edit them. Lower
priority than the rule engine itself; CLI is enough for v1.

## Test coverage

Per project convention, flow tests under
`pkg/claude/agentd/*_flow_test.go`:

- Rule fires on matching event; action executes against the
  resolved target.
- Filter narrows by group / conv / threshold.
- Body templating substitutes fields correctly.
- Non-matching event does NOT fire the rule (regression guard
  against over-eager match).
- Rule loop / cycle protection: a rule whose action is `agent.message`
  to a group that includes the source must not infinitely fan out
  (cap at N hops per originating event).

## Out of scope (for later)

- **Agent-authored rules.** Today rules are human-only. A future
  slug `rules.add` could let a coordinator agent (e.g. PO) wire
  its own "ping me when X" rules. Big trust escalation; defer.
- **Stateful rules** (e.g. "fire only after N occurrences within
  T"). Useful for de-flapping, design-heavy.
- **Cross-machine rules** — out of scope while tclaude is single-
  host.

## Files (when implementing)

- `pkg/claude/agentd/rules.go` — rule engine + matcher
- `pkg/claude/common/config/rules.go` — rule loading + schema
- `pkg/claude/agentd/events.go` (shared with realtime-push) —
  event broadcaster
- `pkg/claude/agent/rules_cli.go` — `agentd rules` verbs

## Cross-references

- [`per-group-reincarnate-thresholds.md`](per-group-reincarnate-thresholds.md)
  — context-pct events feed into rules.
- [`dashboard-realtime-push.md`](dashboard-realtime-push.md) —
  shares the event broadcaster.
- [`fsnotify-monitor.md`](fsnotify-monitor.md) — additional event
  source.
- [`context-nudges.md`](context-nudges.md) — the simpler version
  of "fire on threshold cross"; the rule engine is its general-
  case generalisation.
