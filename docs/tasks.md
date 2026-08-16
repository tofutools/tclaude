# Task runner

`tclaude task` (alias `tasks`) runs a list of tasks sequentially with Claude
Code: each task gets a fresh context, its changes are committed with the task
title as the commit message, and the list advances automatically — with
optional verify and review loops between tasks.

!!! note "Claude Code only"
    The task runner drives Claude Code's interactive plan-mode, hook, skill,
    and `/exit` contracts directly; it is not on the shared harness seam and
    does not run tasks through the other harnesses. For mixed-harness team
    execution, see [Agents and groups](agents-and-groups.md); for structured,
    repeatable workflows, see [Processes](processes.md).

## The model

Tasks live in a `TODO.md` at the root of your project (or a directory chosen
with `-C`). `tclaude task run` starts a tmux session and works through them
one by one. After each task:

1. If no files changed (and the agent made no commits), the runner notifies
   you and waits for manual intervention.
2. All repository changes are committed, with the task title as the message.
3. The task moves from `TODO.md` to `DONE.md` with status, timestamp, commit
   hash, and the agent's report.
4. The next task starts in a fresh Claude Code context.

The session stays interactive throughout — attach to approve permissions,
answer questions, or add context — and a desktop notification is sent when
the list is done (run `tclaude setup` once for
[notifications](notifications.md)).

## TODO.md format

Each `## ` header starts a task; the header is the title (and commit
message), and everything until the next header is the prompt:

```markdown
## Add input validation

Add input validation to the user registration endpoint.
Validate email format, password strength, and required fields.

## Write API tests

Write integration tests for all REST API endpoints using
the httptest package. Cover success and error cases.
```

The runner re-reads `TODO.md` before each task, so you can add, remove, or
reorder tasks while it is active.

### Plan markers

Prefix a title with `[plan]` to run that task with
`--permission-mode plan` instead of the default `acceptEdits` — Claude
proposes changes without applying them. Prefix it with `[plan-auto]` to plan
first and then auto-accept the plan and implement, in one task: the runner
detects the finished plan via the `ExitPlanMode` permission-request hook and
accepts it after a 5-second grace period (start typing before then and your
interaction takes priority). Both markers are stripped from the commit
message.

## Commands

```bash
# Add a task (title + prompt, or prompt only — Claude derives the title)
tclaude task add "Fix login bug" "Fix the NPE in the login handler"
tclaude task add "Fix the NPE in the login handler"

# Plan variants of add
tclaude task add --plan "Design auth system" "Design the auth architecture"
tclaude task add --plan-auto "Design and build auth" "Design and implement it"

# List pending tasks (bare `tclaude task` does the same)
tclaude task list

# Run all tasks: starts a tmux session and attaches
tclaude task run
tclaude task run -d          # detached; check back later
tclaude task run -w          # watch mode: wait for new tasks instead of exiting
tclaude task run -C ~/proj   # -C works on add/list/run and the parent command
tclaude task run -- --dangerously-skip-permissions   # extra Claude Code flags
```

Attach and detach freely with `tclaude session attach <session-id>` and
`Ctrl+B D`; the runner continues in the background. Type `/exit` in the pane
when you are satisfied with a task's result. See [Sessions](sessions.md).

## Project configuration

Optional per-project settings live in `.claude/tclaude/tasks.json`:

```json
{
  "verify": "go test ./...",
  "max_verify_iterations": 5,
  "verify_timeout": "2m",
  "review_skill": "task-review",
  "max_review_iterations": 3,
  "review_timeout": "5m",
  "review_diff": true,
  "stuck_timeout": "5m",
  "max_stuck_nudges": 3
}
```

| Field | Description |
|---|---|
| `verify` | Shell command run after each task to verify success |
| `max_verify_iterations` | Fix-and-retry attempts on verify failure (default 3) |
| `verify_timeout` | Timeout per verify run (default `"1m"`) |
| `review_skill` | Claude Code skill run as a review agent after verify passes |
| `review_prefix` | Text prepended to review feedback when fed back to the agent |
| `max_review_iterations` | Review-and-fix cycles per task (default 1) |
| `review_timeout` | Timeout per review run (default `"5m"`) |
| `review_diff` | Pass the git diff to the review agent (default `true`) |
| `stuck_timeout` | Idle time before a nudge; `"0s"` disables (default `"5m"`, min `"30s"`) |
| `max_stuck_nudges` | "continue" nudges before giving up (default 3) |

### Verify

When `verify` is set, the runner executes it after Claude finishes. On
failure, Claude is given the output and asked to fix the issue, then the
command re-runs — up to `max_verify_iterations` times. If retries are
exhausted, the runner notifies you and waits for manual intervention.

### Review

When `review_skill` is set, the runner invokes that skill as an automated
review agent after verify passes, giving it the git diff of everything the
task changed (committed and uncommitted; set `review_diff: false` for skills
that read files or run commands themselves). If the review returns feedback,
Claude is asked to address it, verify runs again, and another review cycle
begins — up to `max_review_iterations`. An empty review output means the
review passed. Unlike verify, exhausting review iterations does not block:
the runner proceeds anyway.

A review skill is an ordinary skill file under
`.claude/skills/<skill-name>/SKILL.md`:

```markdown
---
name: task-review
description: Review the diff when a task is finished.
disable-model-invocation: true
---

You are an expert code reviewer. Analyze the following diff and provide a
thorough code review. If there is nothing worth changing, do not output
anything.
```

`disable-model-invocation: true` keeps the skill from triggering a model call
when invoked interactively, where no diff exists; the task runner always
invokes the model itself via `claude --print` with the diff.

### Stuck-agent detection

If Claude stays in a working state with no hook activity for longer than
`stuck_timeout`, the runner sends a `continue` message to the pane, up to
`max_stuck_nudges` times. If the agent is still stuck after that, a desktop
notification is sent and detection is disabled for the rest of that task.

Hooks do not fire while a tool command is actively running, so a slow but
healthy build or test that outlasts `stuck_timeout` triggers a spurious nudge
(which the agent typically ignores). Set `stuck_timeout` above your longest
expected tool call, or `"0s"` to disable the feature.

## Failure handling and DONE.md

If a task fails (Claude exits with an error), the runner records the failure
in `DONE.md` with the error message, commits any partial changes, stops
without starting the next task, and sends a notification.

Completed tasks are appended to `DONE.md` with their status, completion time,
commit hash, and collapsible sections carrying the original prompt and
Claude's closing report.

!!! warning
    Driving the task runner with a Claude Pro or Max subscription may violate
    Anthropic's terms of service. Use at your own risk.
