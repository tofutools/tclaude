# Tour the dashboard

The dashboard is a browser view of everything tclaude runs: your groups, every
agent's live status, their terminals and their mail. In this tutorial you
take a short tour of the parts you will use every day.

**You need:** [Your first agent team](first-team.md) completed, with the
daemon running and `TCLAUDE_HUMAN_TOKEN` exported. If you already cleaned up
that team, start one agent again from your project directory:

```bash
tclaude agent groups create tour --descr "dashboard tour"
tclaude agent spawn tour --name guide \
  --initial-message "Read this project's README and summarize it in three bullet points."
```

## 1. Open it

```bash
tclaude agent dashboard
```

The command asks the daemon for a one-time sign-in link and opens it in your
browser. The link works once and expires within a minute; run the command
again whenever you need a fresh one.

Tabs run across the top. You start on **Groups**, the home view.

## 2. Read the Groups view

Each group is a block, with one row per agent:

<!-- Regenerate the images on this page with scripts/docs/learn-images.sh -->
![The Groups view. The hello group lists alice, working, and bob, idle, with their model, state, age, working directory and branch. Below it, the Ungrouped block holds one plain session.](../assets/learn-dashboard-groups.png)

Read a row from left to right:

- the **status dot**: `●` online, `○` offline;
- the **harness line**: which harness and model the agent runs, and what it
  has cost so far;
- the **Name**;
- the **State** pill: `working`, `idle`, `awaiting_permission` and so on.
  A small meter beside it shows how much of the context window is used;
- **CWD** and **Branch**: where the agent works, with PR and CI status once
  it has a pull request.

Now start a plain session from your terminal, as in the first tutorial:

```bash
tclaude -d
```

While the daemon runs, every new session registers with it, so this one
appears too, in the **Ungrouped** block at the bottom. You can drag it onto a
group to make it a member.

!!! info "Go deeper"
    [Dashboard → The member row](../dashboard.md#the-member-row) explains every
    column, and the [badge glossary](../dashboard.md#badge-glossary) every pill.

## 3. Look inside an agent

Open the row's **⚙** menu and choose **web window**.

![alice's ⚙ menu open in the Groups view. It lists view messages, term, web term, open window and web window at the top, then edit, permissions and more.](../assets/learn-dashboard-agent-menu.png)

The agent's live terminal opens as a pane in the **Terminals** tab, which appears once a
terminal is open. It is the real session, not a copy, so what you type there
reaches the agent. Switch back to **Groups** when you are done; the agent
keeps running.

**web term** opens a fresh shell in the agent's directory instead of the
agent itself. **open window** and **term** do the same as their **web**
twins, in the browser by default, or in a native terminal window if you set
`dashboard.default_terminal` to `"native"`.

!!! info "Go deeper"
    [Dashboard → Zooming into a terminal](../dashboard.md#zooming-into-a-terminal)
    and [Terminals](../dashboard.md#terminals).

## 4. Read and send mail

Open the **Messages** tab. The left side lists mailboxes: all messages, one
folder per group and agent, and a folder for notifications sent to you. Pick
your group to read its mail, including each agent's first task brief.

To write to an agent, go back to the pane you opened in step 3 and press
**✉ Message** (Ctrl/Cmd+M). This queues a message for the agent rather than
typing into its terminal. To write to several agents at once, use a group's
**⚙** menu → **✉ message** in **Groups** and tick the members. Either way,
the message lands in the agent's inbox and appears in its terminal.

!!! info "Go deeper"
    [Dashboard → Messages](../dashboard.md#messages).

## 5. Manage an agent from the ⚙ menu

Back in **Groups**, open an agent's **⚙** menu again. Start with these:

- **view messages**: that agent's mail only;
- **edit**: its name, role and description;
- **permissions**: what the agent is allowed to do;
- **retire**: end the agent and keep its conversation, as `tclaude agent
  retire` does.

To rename an agent, click its name in the row. To stop or wake one, click its
status dot.

The rest of the menu (clone, reincarnate, morph, snapshot, schedule) manages
an agent's lifecycle. You can ignore it until you need it.

!!! info "Go deeper"
    [Spawning and lifecycle → Lifecycle verbs](../spawning-and-lifecycle.md#lifecycle-verbs)
    explains each lifecycle action.

## 6. Clean up

Retire `guide` from its ⚙ menu, or from the terminal:

```bash
tclaude agent retire guide
tclaude agent groups archive tour
```

End the plain session you started in step 2 from **Groups** by clicking its
status dot, or attach to it with `tclaude session watch` and exit the
harness.

## What you learned

- `tclaude agent dashboard` opens the dashboard with a one-time link.
- **Groups** shows every agent's status, harness, cost, context and branch.
  Sessions outside any group appear under **Ungrouped**.
- An agent's **⚙** menu opens its live terminal and manages it.
- **Messages** shows all mail. **✉ Message** on a pane, or a group's
  **✉ message**, writes to agents.

**Next:** the [Dashboard](../dashboard.md) reference covers every other tab,
including **Usage** and **Costs** for your quota and spend.
