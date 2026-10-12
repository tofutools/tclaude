# Find and resume your work

Agents come and go, but tclaude keeps their conversations. In this tutorial
you find past work in the dashboard and pick it up again: a stopped agent, a
retired one, and a conversation you had outside any group.

**You need:** [Your first agent team](first-team.md) completed: `alice` in
`hello`, and `bob` retired.

## 1. Wake a stopped agent

A stopped agent stays in its group with an empty status dot, `○`. Stop
`alice` from her status dot (**Soft exit**), then click the dot again to
wake her. She resumes the same conversation, through the harness she ran
on.

To wake every stopped agent in a group at once, use the green power button
under the group's name.

Stopped agents are shown by default. If you have hidden them, the **▾ view**
menu at the top of **Groups** has **show offline**.

## 2. Bring back a retired agent

Retired agents leave their groups. Open **▾ view** and tick
**show retired**. A **Retired** block appears at the bottom of **Groups**,
listing `bob`.

<!-- Regenerate the images on this page with scripts/docs/learn-images.sh -->
![The Groups view with show retired and show conversations ticked. The Retired block lists bob with a reinstate button, and the Conversations block lists a past conversation, "fix the flaky login test", with a promote button.](../assets/learn-dashboard-resume.png)

Click **reinstate** on `bob`'s row. He becomes an agent again and appears in
**Ungrouped**. Drag his row onto `hello`, then click his status dot to wake
him. His old permissions do not come back.

## 3. Turn a past conversation into an agent

Conversations you had outside any group are not agents: for example a
`tclaude` session in a directory that no group uses. (A session started in a
group's directory joins that group automatically.) **▾ view** → **show conversations** lists the
recent ones in a **Conversations** block. **promote** turns one into an
agent, which you can then drag onto a group and wake like `bob`.

## 4. Filter the view

With many agents, type in the filter box at the top of **Groups**. It
matches group names and each agent's name, role, description, directory and
branch.

!!! info "Go deeper"
    [Spawning and lifecycle → retire, reinstate, delete](../spawning-and-lifecycle.md#retire-reinstate-delete)
    and [Promote](../spawning-and-lifecycle.md#promote).

## From the terminal

The dashboard lists recent conversations, but it cannot search inside them.
For that, use the CLI. It indexes every harness's transcripts in one list:

```bash
tclaude conv ls -g                        # every conversation, newest first
tclaude conv search -g "three sentences"  # search inside them
tclaude conv resume -g <conv-id>          # continue one in a new session
```

`conv resume` relaunches through the harness that recorded the conversation,
in its own project directory. `tclaude ask` answers a quick question in the
shell without starting a session:

```bash
tclaude ask "what does the main function in this project do?"
```

[Conversations](../conversations.md) and [Ask](../ask.md) cover both.

## What you learned

- The status dot and the group's power button wake stopped agents.
- **▾ view** shows retired agents (**reinstate**) and plain conversations
  (**promote**).
- The CLI searches inside conversations: `tclaude conv search`.

**Next:** [Keep agents going](keep-agents-going.md).
