# Get started

In this tutorial you install tclaude, start its daemon, and open the
dashboard. The dashboard is where you will spend most of your time with
tclaude: it shows every agent you run, what each is doing, and their mail,
and it starts, stops and manages them.

**You need:** Linux or macOS, and one harness CLI logged in. On Linux you also need
bubblewrap for tclaude's sandbox. See
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
asks about a few optional integrations. Accept the defaults. One question sets how
long Claude Code waits when an agent asks you something; that one changes a global
Claude Code setting, and setup never answers it for you, even with `--yes`.

On a new install, agents start inside **tclaude's own sandbox**. If setup warns
that the sandbox can't run on this host yet, agents won't start until that's fixed.
The next steps show you where to find the fix.

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
empty for now; the next tutorial fills it. On its right is the **library**: saved
profiles, six starter roles, prompts and templates that you'll use later.

The tabs you will use first:

| Tab | What it is for |
|---|---|
| **Groups** | Every agent, grouped into teams, with live status. You start and manage agents here. |
| **Terminals** | Live terminals of your agents, in the browser. |
| **Messages** | The mail agents send each other and you. |
| **Diagnostics** | Checks on your install, plus logs and the audit trail. |
| **Config** | Your settings. |

**Usage** and **Plugins** stay hidden until they have something to show.
**Ctrl/Cmd+K** opens a command palette that reaches everything else.

## 6. Check Doctor

A number on **Diagnostics** means tclaude has a question or found a problem. Open
**Diagnostics**, then **Doctor**. Its **Needs attention** view lists what to do:

- a blue **decision** is a question only you can answer, such as which sandbox new
  agents use. Answer it in place.
- a red **blocking** problem stops agents from starting, such as missing `tmux` or
  bubblewrap. Doctor gives you the command to fix it.

!!! info "Go deeper"
    [Dashboard → The tabs](../dashboard.md#the-tabs) describes every tab.

## From the terminal

The dashboard is not the only way in. Bare `tclaude` starts your harness in
a tmux session from the shell, and `tclaude session watch` lists running sessions.
This works without the daemon, unless a group owns the directory; then tclaude asks
before starting a solo session. These commands suit
scripting and terminal-only machines; [Sessions](../sessions.md) covers
them. The Learn path uses the dashboard.

## What you learned

- `tclaude setup` installs the hooks and agent extras.
- `tclaude agentd serve` runs the daemon; keep it running.
- `tclaude agent dashboard` opens the dashboard.
- **Diagnostics › Doctor** tells you what your install still needs.

**Next:** [Your first agent](first-agent.md).
