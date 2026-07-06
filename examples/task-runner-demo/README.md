# Task runner demo

A tiny, harmless set of tasks for demonstrating `tclaude task run`. Every task
makes a small, committable change to the working directory — nothing here
touches your real projects.

The tasks (see [`TODO.md`](./TODO.md)):

| # | Task                                     | What it shows                                         |
|---|------------------------------------------|-------------------------------------------------------|
| 1 | Create an empty placeholder file         | A minimal file-creating task                          |
| 2 | Write a greeting file                    | Writing file content (auto-accepted in `acceptEdits`) |
| 3 | Run a noop command and capture its output| Running a shell command inside a task                 |
| 4 | Append a completion marker to a log      | Appending to / creating a file                        |

> **Why each task writes a file:** the runner marks a task *failed* if it
> produces no committable change ("no files were changed"). So even the "run a
> noop command" step captures its output to a file — that's the change that
> gets committed.

## Run it

Run the demo in a **throwaway copy** so its commits land in a fresh repo instead
of this one:

```bash
# 1. Copy the demo somewhere disposable and make it its own git repo
cp -r examples/task-runner-demo /tmp/tclaude-demo
cd /tmp/tclaude-demo
git init -q && git add -A && git commit -qm "seed demo"

# 2. Run the tasks (opens a tmux session and attaches)
tclaude task run
```

Each task runs in a fresh Claude Code context inside the tmux session. When a
task finishes, the runner commits the change (task title = commit message),
moves the task from `TODO.md` to `DONE.md`, and starts the next one. Detach any
time with `Ctrl+B D`; the runner keeps going.

### Hands-free

Tasks 1, 2 and 4 only write files, which auto-accept under the default
`acceptEdits` mode. Task 3 runs a shell command, which normally prompts for
approval — either attach and approve it, or run fully unattended:

```bash
tclaude task run -- --dangerously-skip-permissions
```

## What you should see

After the run, the throwaway repo contains four new files and four commits:

```
placeholder.txt      # empty
hello.txt            # "Hello from the tclaude task runner!"
noop-output.txt      # "noop ok"
demo-log.md          # "Demo complete."
```

`DONE.md` records each completed task with its status, timestamp, commit hash,
prompt, and Claude's report. `git log --oneline` shows one commit per task.

## Optional: try the verify step

To also demo the post-task **verify** hook, drop a `.claude/tclaude/tasks.json`
into the copied demo dir before running:

```json
{
  "verify": "test -f placeholder.txt && test -f hello.txt",
  "verify_timeout": "10s"
}
```

The runner runs `verify` after each task; if it fails, Claude is handed the
output and asked to fix it, then the command re-runs. See
[`docs/tasks.md`](../../docs/tasks.md) for the full configuration reference.
