# Work in parallel with worktrees

Two agents editing the same checkout trip over each other: one switches
branch under the other, or both edit the same file. A **git worktree** gives
each agent its own checkout of the same repository, on its own branch. In
this tutorial you start two agents, each in a fresh worktree, and clean up
when they are done.

**You need:** [Your first agent](first-agent.md) completed, with the `hello`
group pointing at a git repository with at least one commit on `main` (or
`master`).

## 1. Start an agent on a new branch

Open the spawn dialog on `hello`. Set **Name** to `greeter` and **Initial
msg** to a small task, for example:

> Add a short greeting to the README and commit it.

**Sync worktree branch with name** is on by default, so naming the agent
already chose **+ create new worktree…** under **Worktree**, with a
**New branch** called `greeter`. Change **New branch** to `feat/greeting`,
keep **Base branch** at `main`, and click **Spawn**.

tclaude creates the branch and a separate checkout for it next to your
project, and starts `greeter` there. Its row shows the worktree under
**CWD** and the branch under **Branch**.

!!! tip "Name and branch in one go"
    Leave **New branch** as it is, and the branch takes the agent's name.

## 2. Start a second one in parallel

Spawn again with **Name** `fixer`, a branch `fix/typo`, and a task such as:

> Find one typo in the docs, fix it, and commit.

Both agents now work at the same time, each in its own directory and branch.
Neither sees the other's uncommitted changes. When an agent opens a pull
request, its row's **Branch** column shows PR and CI pills.

!!! tip "Let the lead do it"
    A lead prepares worktrees for its workers itself. Ask it to give each
    worker its own worktree, as `alice` did in
    [Run a team through a lead](team-lead.md#2-give-the-lead-a-task). Starting
    worktree agents by hand, as here, suits one-off tasks.

## 3. Clean up

When a branch has been merged, retire its agent with the bin button. The
retire dialog offers **Also delete the git worktree + branch**, ticked by
default. It removes the worktree directory, including any uncommitted
changes in it. It deletes the branch only if the branch is merged; an
unmerged branch is kept, and the result says how many commits it holds. A
squash-merged branch counts as unmerged. The main checkout, and worktrees
another agent still uses, are never removed.

For leftovers, such as worktrees from agents you retired without deleting,
open the group's **⚙** menu and choose **🧹 cleanup worktrees…**. It lists
the stale worktrees in the group's repository and removes the ones you pick.
**Also delete the feature branch** deletes their branches once the commits are
merged or pushed. A branch holding the only copy of its commits is kept unless
you also tick the separate option that lists those branches and their commit
counts.

!!! info "Go deeper"
    [Spawning and lifecycle → Spawning an agent](../spawning-and-lifecycle.md#spawning-an-agent)
    covers the worktree options, including monorepos.

## From the terminal

The same from the shell, run inside your project. `agent spawn` needs the
daemon; the `worktree` commands work without it:

```bash
cd ~/code/my-project
tclaude agent spawn hello --name docs-bot --worktree feat/docs \
  --initial-message "Add a short greeting to the README and commit it."
tclaude worktree add fix/readme      # a worktree and a session in one step
tclaude worktree ls
tclaude worktree rm fix/readme -D    # remove it and its merged branch
```

[Worktrees](../worktrees.md) covers these, including a shell wrapper that
moves your shell between worktrees.

## What you learned

- **+ create new worktree…** in the spawn dialog starts an agent on its own
  branch and checkout.
- Agents in different worktrees work in parallel without sharing files.
- Retiring an agent can remove its worktree and branch; the group's
  **🧹 cleanup worktrees…** handles leftovers.

**Next:** [Link two machines](../federation-setup.md), or the
[Dashboard](../dashboard.md) reference for every other tab.
