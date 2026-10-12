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
| 3 | [Set up your team](team-setup.md) | Give a group a startup context, save a spawn profile, and start agents from it | 10 min |
| 4 | [Run a team through a lead](team-lead.md) | Make an agent the lead, let it run workers, follow their mail and pull requests, and answer its notifications | 25 min |
| 5 | [Keep agents going](keep-agents-going.md) | Watch an agent's context, schedule check-ins, stop and wake it, clone, reincarnate or morph it, and bring back earlier work | 25 min |
| 6 | [Work in parallel with worktrees](parallel-worktrees.md) | Start agents on their own branches and checkouts, and clean up after merging | 15 min |
| 7 | [Link two machines](../federation-setup.md) | Connect two tclaude instances through a hub so agents on each can mail each other. Start with a group and an agent on each machine, as in step 4 | 45 min |

The last step is optional and more advanced: it needs a second machine,
and it uses [permissions](../permissions-and-audit.md) the earlier steps did
not cover.

## The command line

Everything the dashboard does has a CLI equivalent: `tclaude` alone starts
your harness in a tmux session from the shell, and `tclaude agent …` drives
agents. It suits scripting and terminal-only machines. The tutorials show it
in short **From the terminal** notes; [Sessions](../sessions.md),
[Conversations](../conversations.md) and
[Spawning and lifecycle](../spawning-and-lifecycle.md) are the reference.
