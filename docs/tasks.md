# Task Management

Run a list of tasks sequentially with Claude Code, with automatic git commits and progress tracking.

## Overview

Define tasks in a `TODO.md` file at the root of your project (or in a custom directory with `-C`). Each task has a title and a prompt. When you run `tclaude task run`, tasks are executed one by one in a tmux session. After each task:

1. All repository changes are committed to git (using the task title as the commit message)
2. The task is removed from `TODO.md` and recorded in `DONE.md` with status info
3. Claude Code's context is cleared (each task runs in a fresh session)
4. The next task starts automatically

When all tasks are done, a desktop notification is sent.

## Prerequisites

- **tmux** - Required for the task runner session
- **Run setup** - For notifications: `tclaude setup`

## TODO.md Format

Tasks are defined as `##` headers followed by the prompt text:

```markdown
## Add input validation

Add input validation to the user registration endpoint.
Validate email format, password strength, and required fields.
Return appropriate error messages.

## Write API tests

Write integration tests for all REST API endpoints using
the httptest package. Cover success and error cases.

## Update README

Update the README with the new API endpoints, request/response
examples, and setup instructions.
```

Each `## ` header starts a new task. The header text becomes the task title (and git commit message). Everything until the next header or end of the file is the prompt sent to Claude Code.

### Plan Mode

Prefix a task title with `[plan]` to run it with `--permission-mode plan` instead of the default `acceptEdits`. This is useful for tasks that require architectural planning or design work where you want Claude to propose changes without applying them directly.

```markdown
## [plan] Design the new billing API

Design the REST API for the billing service. Consider
authentication, rate limiting, and error handling.

## Implement billing endpoints

Build the billing endpoints based on the plan.
```

The `[plan]` prefix is stripped from the task title before it's used as a git commit message.

### Plan Auto-Accept Mode

Prefix a task title with `[plan-auto]` to run Claude in plan mode, then automatically accept the plan and proceed to implementation — all in a single task. Claude first creates a plan (using `--permission-mode plan`), and when the plan is ready (detected via the `ExitPlanMode` permission request hook), the task runner automatically accepts it so Claude exits plan mode and implements the changes.

```markdown
## [plan-auto] Design and implement billing API

Design the REST API for the billing service, then implement it.
Consider authentication, rate limiting, and error handling.
```

The session remains interactive throughout — you can still attach to approve permissions or answer questions during implementation. The standard grace period (5 seconds) applies before auto-accept, so if you start typing before then, your interaction takes priority.

## Commands

### task add

Add a task to `TODO.md`.

```bash
tclaude task add "Fix login bug" "Fix the null pointer exception in the login handler"

# Specify prompt only, and let Claude determine the title (which is used as commit message)
tclaude task add "Fix the null pointer exception in the login handler"

# Add a task that requires planning (runs with --permission-mode plan)
tclaude task add --plan "Design auth system" "Design the authentication architecture"

# Add a task that plans then auto-accepts and implements
tclaude task add --plan-auto "Design and build auth" "Design and implement the auth system"

# Add to a specific directory's TODO.md
tclaude task add -C /path/to/project "Fix login bug" "Fix the null pointer exception"
```

### task list

List pending tasks from `TODO.md`.

```bash
tclaude task list

# List tasks from a specific directory
tclaude task list -C /path/to/project
```

### task run

Run all tasks sequentially.

```bash
# Run tasks (starts tmux session and attaches)
tclaude task run

# Run detached (in the background)
tclaude task run -d

# Run in a specific directory
tclaude task run -C /path/to/project

# Pass extra flags to Claude Code
tclaude task run -- --dangerously-skip-permissions
```

**Flags:**

| Flag               | Description                                                  |
|--------------------|--------------------------------------------------------------|
| `-d, --detached`   | Start detached (don't attach to session)                     |
| `-C, --dir <path>` | Directory containing task files (defaults to current)        |
| `-w, --watch`      | Watch for new tasks instead of exiting when TODO.md is empty |

> **Note:** The `-C, --dir` flag is available on all task subcommands (`add`, `list`, `run`) and the parent `task` command itself.

## How It Works

When you run `tclaude task run`:

1. A tmux session is created for the task runner
2. For each task in `TODO.md`:
    - Claude Code launches interactively with the task prompt
    - You can attach to the session to approve permissions, answer questions, or monitor progress
    - When Claude is done, type `/exit` to finish the task
    - The runner commits changes, updates tracking files, and starts the next task
3. A notification is sent when all tasks are complete (or if a task fails)

### Interactive Session

The task runner creates a tmux session you can attach to and detach from freely:

```bash
# If you started detached, attach with:
tclaude session attach <session-id>

# Detach at any time:
Ctrl+B D

# The task runner continues in the background
```

Inside the session, Claude Code runs with full interactive capabilities. You can:

- Approve or deny tool permissions
- Answer questions Claude asks
- Provide additional context
- Type `/exit` when you're satisfied with the result

### Git Commits

After each task the changes are committed with the task title as the commit message.

### Failure Handling

If a task fails (Claude exits with an error), the runner:

- Records the failure in `DONE.md` with the error message
- Commits any partial changes
- Stops execution (does not continue to the next task)
- Sends a notification

## DONE.md

Completed tasks are appended to `DONE.md` with status information:

```markdown
## Add input validation

- **Status:** completed
- **Completed:** 2026-03-07 14:30:00
- **Commit:** a1b2c3d

<details>
<summary>Prompt</summary>

Add input validation to the user registration endpoint.
Validate email format, password strength, and required fields.
Return appropriate error messages.

</details>

<details>
<summary>Report</summary>

Claude's response describing what was done...

</details>

---
```

## Tips

- **Edit TODO.md between tasks** — The runner re-reads `TODO.md` before each task, so you can add, remove, or reorder tasks while the runner is active.
- **Use descriptive titles** — Task titles become git commit messages, so keep them clear and concise.
- **Start detached for long task lists** — Use `tclaude task run -d` and check back later. You'll get a notification when everything is done.
- **Pass Claude flags** — Use `--` to forward flags like `--dangerously-skip-permissions` or `--allowedTools` for unattended execution.
