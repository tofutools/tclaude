# Learn tclaude

tclaude runs coding agents for you and gives you one place to watch and steer
them all: the **dashboard**, a browser view of every agent you run. From it
you start agents, see at a glance which are working, idle or waiting for you,
open their live terminals, read the mail they send each other and you, and
organize them into teams.

This section is a short path of hands-on tutorials. It takes you from a fresh
install to running a team of agents, one step at a time, almost all of it in
the dashboard. Each page builds on the one before it and ends by pointing to
the reference pages for that step.

## Before you start

You need:

- Linux or macOS (WSL counts as Linux);
- [tclaude installed](../index.md#installation);
- tmux (Homebrew installs it for you);
- at least one harness CLI, installed and logged in. The tutorials use
  [Claude Code](https://claude.ai/code), the default. Codex CLI, OpenCode,
  Copilot CLI and Gemini CLI work too: pick one under **Harness** when you
  start an agent.

You do not need to read anything else first. Each tutorial explains the terms
it uses. [Architecture](../architecture.md) has the full mental model when you
want it.

## The path

| Step | Tutorial | You will | Time |
|---|---|---|---|
| 1 | [Get started](get-started.md) | Install tclaude, start its daemon, and open the dashboard | 10 min |
| 2 | [Your first agent](first-agent.md) | Create a group, start an agent, watch it, open its terminal and message it | 15 min |
| 3 | [Your first agent team](first-team.md) | Add a second agent, watch the two message each other, read their mail, and manage them from the ⚙ menu | 20 min |
| 4 | [Find and resume your work](find-and-resume.md) | Wake stopped agents, bring back retired ones, and turn past conversations into agents | 10 min |

## The command line

Everything the dashboard does has a CLI equivalent: `tclaude` alone starts
your harness in a tmux session from the shell, and `tclaude agent …` drives
agents. It suits scripting and terminal-only machines. The tutorials show it
in short **From the terminal** notes; [Sessions](../sessions.md),
[Conversations](../conversations.md) and
[Spawning and lifecycle](../spawning-and-lifecycle.md) are the reference.
