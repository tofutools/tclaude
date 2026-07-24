# Startup-context trimming

Claude Code loads a fixed body of context into every session before your first
word: its bundled skills, tool schemas for every capability the harness offers,
and several system-prompt blocks. That set is sized for a general-purpose
assistant.

A tclaude worker agent is usually spawned for **one narrow job**. Everything else
the harness advertises is context it still has to read past on the way to its
actual brief — and it competes with that brief for the agent's attention.

Startup-context trimming lets you choose, **per agent**, how much of that the
agent loads. The point is focus: raising the share of the window that is about
the task. Fewer distractions, less context rot, a more pointed agent. Spending
fewer tokens is a side benefit, not the reason.

Claude Code only. Codex and OpenCode expose no equivalent switches, so the
control is hidden for them and asking for a trim is an error rather than a
setting that silently does nothing.

## The three states

Every feature in the catalog is one of:

| State | Meaning |
| -- | -- |
| **Default** | tclaude injects nothing. Claude Code's own default — and your own `settings.json` — decide. |
| **Trim** | The feature is removed from this agent's startup context. |
| **Keep** | The feature stays, even if a profile or group default trimmed it. |

Only the features you actually steer are stored, so a profile you never touched
changes nothing, and an untrimmed agent behaves exactly as it did before this
feature existed.

**Keep** exists for two cases: overriding a lean profile for one spawn, and
overriding a `CLAUDE_CODE_DISABLE_*` you exported in your own shell.

## Where you set it

- **Spawn dialog** — a `Context…` button beside `Permissions…`, with a badge
  showing how many features are trimmed or kept.
- **Spawn profile editor** — a `Startup context` row, so a whole class of agents
  shares one context shape.
- **Group templates** — via a template-local or referenced profile, so a deployed
  task force gets it per role.
- **CLI** — `tclaude agent spawn --context-features …` and
  `tclaude session new --context-features …`.

```bash
# Trim the heavy items for one worker. A bare feature name means "trim".
tclaude agent spawn --group crew --name lean-worker \
  --context-features bundled-skills,workflows,artifact

# Keep one thing a profile trimmed.
tclaude agent spawn --group crew --profile lean --name needs-artifacts \
  --context-features artifact=on

# Ignore the profile's trims entirely for this one spawn.
tclaude agent spawn --group crew --profile lean --name full-context \
  --context-features none

# List the catalog, with what each trim costs and which are the big wins.
tclaude session new --help-context-features
```

## How the tiers resolve

Same stack as every other launch field — explicit request, then the group's
default profile, then the global default profile — with one deliberate
difference:

**The tiers do not merge.** The most specific tier that says anything wins
*entirely*.

Merging would make an agent's effective startup context the union of every
profile in its lineage, so reading one profile would never tell you what your
agent actually loads. That is the same action-at-a-distance this feature exists
to remove. One profile tells the whole story.

A consequence worth knowing: clearing every row in the spawn dialog is a real
instruction ("trim nothing"), and it beats a profile that trims. It is not the
same as leaving the profile to speak.

## Relaunches keep it

An agent spawned lean comes back lean. The resolved set is recorded per session
and carried across **resume**, **clone** and **reincarnate** — otherwise the
first handoff would hand the successor the full startup load and overwrite the
record, making the loss permanent. That would fail exactly the long-running
agents that benefit most.

A from-group template snapshot also captures it, so re-snapshotting a lean roster
gives you a lean template.

## The catalog

Run `tclaude session new --help-context-features` for the live list with
descriptions. The ★ entries are the largest wins; the dashboard's **lean** button
trims exactly those.

| Feature | What trimming it removes |
| -- | -- |
| ★ `bundled-skills` | Claude Code's own shipped skills (dataviz, artifact design, pdf, …) |
| ★ `workflows` | The `Workflow` orchestration tool — one of the largest single tool schemas |
| ★ `artifact` | The Artifact publishing tool and its design skills |
| `explore-plan-agents` | The built-in Explore / Plan subagent definitions |
| `cron` | `CronCreate` / `CronList` / `CronDelete` (tclaude has `tclaude agent cron`) |
| `background-tasks` | Background task tools — **also removes background Bash** |
| `agent-view` | The in-harness agent/fleet view (the dashboard covers this) |
| `advisor-tool` | The advisor tool |
| `claude-code-skill` | The skill documenting Claude Code's own settings and hooks |
| `claude-api-skill` | The Claude API / SDK reference skill and its trigger instructions |
| `policy-skills` | Organization policy skills |
| `git-instructions` | The system-prompt block on commit / PR conventions |
| `org-memory` | Organization-level memory files |
| `claude-ai-connectors` | Gmail / Drive / Calendar tools — **not** ordinary MCP servers such as Linear |
| ⚠ `claude-mds` | `CLAUDE.md` / `AGENTS.md` themselves |

`claude-mds` is the one to think twice about: it drops the repo's own agent
instructions, including this project's `CLAUDE.md`. An agent expected to follow
project conventions should keep it. The dashboard shows that warning inline.

`background-tasks` has a similar edge — it takes background Bash with it, which
some workflows depend on.

## How a trim is delivered

Two paths, because Claude Code splits the switches that way:

- Most of the catalog rides a `CLAUDE_CODE_DISABLE_*` environment variable, which
  is why tclaude never has to edit your `settings.json` — the same approach the
  auto-memory control uses.
- The few with no environment twin (`claude-ai-connectors`) join the per-session
  `--settings` payload, alongside the sandbox block and the AskUserQuestion
  timeout.

Trim sets the variable to `1`. Keep sets it to the **empty string** rather than
omitting it, so a `CLAUDE_CODE_DISABLE_*=1` exported in your own shell cannot
override an agent that asked to keep the feature. Default emits nothing at all.

## A caveat worth keeping

These switches were catalogued by inspecting a specific Claude Code build rather
than from a documented, stable contract, and Claude Code is free to rename or
retire any of them.

A trim that stops working **degrades in the safe direction**: the agent keeps a
capability it did not need. A stale catalog entry costs context, never
correctness. Still, if Claude Code's startup context visibly changes shape after
an upgrade, it is worth re-checking — `/context` inside a spawned agent is the
cheap test, and comparing it against an untrimmed sibling tells you whether a
trim is still landing.

## See also

- [Harness capability matrix](harnesses.md) — what each harness supports
- [Agent coordination](agent.md) — spawn profiles, group templates, permissions
- [Dashboard](dashboard.md) — the spawn dialog and profile editor
