# Your first agent team

So far every session has been one agent working alone with you. In this
tutorial you start the tclaude daemon, put two agents in a group, and watch
them message each other. That is the foundation for everything else tclaude
does with teams: groups, mail, the dashboard, and permissions.

**You need:** [Your first agent](first-agent.md) completed. Use the same
project directory as there: your harness has already asked you to trust it,
and agents spawned in the background cannot answer that question.

If you followed [Get started](get-started.md), steps 1 and 2 are done
already: skip to step 3.

## 1. Install the agent extras

Agents learn how to message each other from small bundled skills. Install
them, along with the low-risk default permissions those skills use:

```bash
tclaude setup --install-agent-skills --install-default-agent-permissions
```

!!! info "Go deeper"
    [Installation → Run setup](../index.md#2-run-setup) lists every extra.

## 2. Start the daemon

Everything multi-agent goes through one daemon, `agentd`. It spawns agents,
knows who is who, decides what each agent may do, and delivers mail. Agents
may run sandboxed, but the daemon must not be. Open a **separate terminal**,
outside any sandbox, and start it:

```bash
tclaude agentd serve
```

Leave it running. Its startup banner prints a line like
`export TCLAUDE_HUMAN_TOKEN="tclo_…"`. That token is how the daemon knows a
command comes from you, the operator, rather than from an agent. Copy that
line and run it in the terminal where you will type the rest of this
tutorial.

The token is new each time the daemon starts. After a restart, export the new
one.

!!! info "Go deeper"
    [Agents and groups → The agentd daemon](../agents-and-groups.md#the-agentd-daemon)
    and [Identity](../agents-and-groups.md#identity-who-is-calling) explain the
    daemon and the token.

## 3. Open the dashboard

```bash
tclaude agent dashboard
```

Your browser opens the dashboard. It is empty for now. Keep it open next to
your terminal; you will watch the team appear in it.

## 4. Create a group

A **group** is a team: a list of agents that are allowed to talk to each
other. From your project directory:

```bash
cd ~/code/my-project
tclaude agent groups create hello --descr "my first team"
```

## 5. Spawn two agents

`agent spawn` starts a new agent session in the background, adds it to the
group and hands it a first task. Start with `bob`, who answers questions:

```bash
tclaude agent spawn hello --name bob \
  --initial-message "When a teammate messages you, answer in one or two sentences with tclaude agent reply."
```

Then `alice`, who asks one:

```bash
tclaude agent spawn hello --name alice \
  --initial-message "Ask your teammate bob, with tclaude agent message, what this project's main entry point is. When bob replies, tell me his answer."
```

Each agent starts in the current directory, using your default harness.
If an agent sits still and never starts working, it is probably waiting on
the harness's "trust this folder?" prompt: attach to it (step 6) and accept
it, or spawn again with `--trust-dir`.

Teams can mix harnesses: add `--harness codex` (or `opencode`, `copilot`,
`gemini`) to either spawn.

!!! info "Go deeper"
    [Spawning and lifecycle → Spawning an agent](../spawning-and-lifecycle.md#spawning-an-agent)
    covers names, roles, profiles and worktrees.

## 6. Watch them work

In the dashboard, the `hello` group now shows both agents with a live
status. Open the **Messages** tab to see alice's question and bob's reply
arrive as mail.

<!-- Regenerate the images on this page with scripts/docs/learn-images.sh -->
![The dashboard's Messages tab with the hello group selected. The list shows alice's question to bob and bob's reply, and the reader shows bob's answer about the project's entry point.](../assets/learn-dashboard-messages.png)

From the terminal you can see the same:

```bash
tclaude agent groups members hello
tclaude agent inbox ls --target alice    # bob's reply
tclaude agent inbox ls --target bob      # alice's question
```

To see an agent's own terminal, attach to its session. `tclaude session
watch` lists both agents next to your other sessions. Select `alice`, press
**Enter**, and read her summary of bob's answer. Detach with **Ctrl+B D** as
usual.

!!! info "Go deeper"
    [Agents and groups → Messaging](../agents-and-groups.md#messaging) explains
    delivery and the inbox. [Dashboard](../dashboard.md) tours every tab.

## 7. Send an agent a message yourself

You can mail any agent from the dashboard. In **Groups**, open the `hello`
group's **⚙** menu (next to the group name), choose **✉ message**, tick
`alice`, and write:

> Thanks! Now ask bob where the tests live.

(`tclaude agent message` is the agents' own command; it does not work from
your shell.) The message lands in alice's inbox and pops into her terminal. She answers in
her terminal, not by mail: agents can't reply to the operator's mail directly.
Mail to you goes through `notify-human`, a permission-gated channel covered
in the reference.

## 8. Clean up

Retiring an agent stops its session and removes it from the team. The
conversation is kept and can be resumed or reinstated later.

```bash
tclaude agent retire alice
tclaude agent retire bob
tclaude agent groups archive hello
```

You can stop the daemon with **Ctrl+C** when you are done. Solo sessions keep
working without it.

## What you learned

- `agentd` runs outside any sandbox and is the single place teams go
  through. You talk to it as the operator through `TCLAUDE_HUMAN_TOKEN`.
- A group is the list of agents allowed to talk to each other.
- `agent spawn` starts an agent with a task. Agents mail each other with
  `agent message` and `agent reply`.
- The dashboard shows the team's status and mail.
- `agent retire` ends an agent but keeps its conversation.

**Next:** [Tour the dashboard](dashboard-tour.md). Agents are also the
starting point for permissions, sandboxing, templates and more. [Permissions and audit](../permissions-and-audit.md)
explains what agents may do, and [Teams at scale](../teams-at-scale.md) shows
how to deploy whole teams at once.
