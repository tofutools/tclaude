# Learn tclaude

The rest of these docs are reference: they describe every command and option.
This section is different. It is a short path of hands-on tutorials that take
you from a fresh install to running a team of agents, one step at a time.
Each page builds on the one before it and ends by pointing to the
reference pages for that step.

## Before you start

You need:

- Linux or macOS (WSL counts as Linux);
- [tclaude installed](../index.md#installation);
- tmux (Homebrew installs it for you);
- at least one harness CLI, installed and logged in. The tutorials use
  [Claude Code](https://claude.ai/code), the default. Codex CLI, OpenCode,
  Copilot CLI and Gemini CLI work too: wherever a tutorial runs `tclaude`, add
  `--harness codex` (or `opencode`, `copilot`, `gemini`).

You do not need to read anything else first. Each tutorial explains the terms
it uses. [Architecture](../architecture.md) has the full mental model when you
want it.

## The path

| Step | Tutorial | You will | Time |
|---|---|---|---|
| 1 | [Your first session](first-session.md) | Run a coding agent inside tclaude, detach from it, and come back | 10 min |
| 2 | [Find and resume your work](find-and-resume.md) | Find yesterday's conversation, pick it up again, and ask quick questions from the shell | 10 min |
| 3 | [Your first agent team](first-team.md) | Start the daemon, spawn two agents into a group, and watch them message each other | 20 min |
| 4 | [Tour the dashboard](dashboard-tour.md) | Read your agents' status, look into their terminals, read their mail, and manage them from the browser | 15 min |
| 5 | [Link two machines](../federation-setup.md) | Connect two tclaude instances through a hub so agents on each can mail each other | 30 min |

Steps 1 and 2 cover solo use and need no daemon. Step 3 starts the daemon,
which everything multi-agent goes through.
The last step is optional: it needs a second machine.
