# Task Management

Run a list of tasks sequentially with Claude Code, with automatic git commits and progress tracking.

## Overview

Define tasks in a `TODO.md` file at the root of your project. Each task has a title and a prompt. When you run `tclaude task run`, tasks are executed one by one in a tmux session. After each task:

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

Each `## ` header starts a new task. The header text becomes the task title (and git commit message). Everything until the next header or end of file is the prompt sent to Claude Code.

## Commands

### task add

Add a task to `TODO.md`.

```bash
tclaude task add "Fix login bug" "Fix the null pointer exception in the login handler"
```

### task list

List pending tasks from `TODO.md`.

```bash
tclaude task list
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

| Flag | Description |
|------|-------------|
| `-d, --detached` | Start detached (don't attach to session) |
| `-C, --dir <path>` | Directory to run tasks in (defaults to current) |
| `--no-tmux` | Run directly without tmux |

## How It Works

When you run `tclaude task run`:

1. A tmux session is created for the task runner
2. For each task in `TODO.md`:
    - Claude Code launches interactively with the task prompt
    - You can attach to the session to approve permissions, answer questions, or monitor progress
    - When Claude is done, type `/exit` to finish the task
    - The runner commits changes, updates tracking files, and starts the next task
3. A notification is sent when all tasks complete (or if a task fails)

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

After each task, two commits are created:

1. **Task commit** — all code changes, with the task title as the commit message
2. **Tracking commit** — updates to `TODO.md` and `DONE.md`

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
