# Get started

In this tutorial you install tclaude, start its daemon, and open the
dashboard. The dashboard is where you will spend most of your time with
tclaude: it shows every agent you run, what each is doing, and their mail,
and it starts, stops and manages them.

**You need:** Linux or macOS, and one harness CLI logged in. See
[Before you start](index.md#before-you-start).

## 1. Install

```bash
brew install tofutools/tap/tclaude                 # Homebrew, or:
go install github.com/tofutools/tclaude@latest     # Go 1.27+
```

Homebrew also installs tmux. With `go install`, install tmux yourself.
[Installation](../index.md#installation) lists the prebuilt archives too.

## 2. Run setup

Setup installs the hooks that tell tclaude what each agent is doing (working,
idle, waiting for you), the skills agents use to message each other, and the
low-risk permissions those skills need:

```bash
tclaude setup --install-agent-skills --install-default-agent-permissions
```

It covers Claude Code and every other harness it finds on your `PATH`, and
asks about a few optional integrations. Accept the defaults.

!!! info "Go deeper"
    [Installation → Run setup](../index.md#2-run-setup) lists every setup
    option.

## 3. Start the daemon

Everything the dashboard shows goes through one daemon, `agentd`. Open a
**separate terminal**, outside any sandbox, and start it:

```bash
tclaude agentd serve
```

Leave it running. Its startup banner prints a line like
`export TCLAUDE_HUMAN_TOKEN="tclo_…"`. That token is how the daemon knows a
command comes from you, the operator, rather than from an agent. Copy that
line and run it in the terminal you will work in.

The token is new each time the daemon starts. `--persist-operator-token`
keeps it across restarts.

## 4. Open the dashboard

```bash
tclaude agent dashboard
```

This opens the dashboard in your browser with a one-time sign-in link. Run
it again whenever you need a fresh link. Two other ways in:

- the daemon's tray icon, where your desktop has one: **Open dashboard**;
- `tclaude agentd serve --auto-launch-dashboard`, which opens it each time
  the daemon starts. The **Config** tab can make that the default.

The dashboard listens on your machine only, on a new port each time the
daemon starts. [Operating remotely](../remote.md) covers reaching it from
another machine or a phone.

## 5. Find your way around

Tabs run across the top. You start on **Groups**, the home view. It is
empty for now; the next tutorial fills it. The tabs you will use first:

| Tab | What it is for |
|---|---|
| **Groups** | Every agent, grouped into teams, with live status. You start and manage agents here. |
| **Terminals** | Live terminals of your agents, in the browser. It appears once you open one. |
| **Messages** | The mail agents send each other and you. |
| **Usage** and **Costs** | Your subscription quota and spend. |

**Ctrl/Cmd+K** opens a command palette that reaches everything else.

!!! info "Go deeper"
    [Dashboard → The tabs](../dashboard.md#the-tabs) describes every tab.

## From the terminal

The dashboard is not the only way in. Bare `tclaude` starts your harness in
a tmux session from the shell, with no daemon needed, and
`tclaude session watch` lists running sessions. These commands suit
scripting and terminal-only machines; [Sessions](../sessions.md) covers
them. The Learn path uses the dashboard.

## What you learned

- `tclaude setup` installs the hooks and agent extras.
- `tclaude agentd serve` runs the daemon; keep it running.
- `tclaude agent dashboard` opens the dashboard.

**Next:** [Your first agent](first-agent.md).
