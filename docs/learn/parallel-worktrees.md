# Work on branches in parallel

An agent that is halfway through a feature owns your checkout. When an urgent
fix comes in, you would normally stash, switch branches, and hope nothing
gets mixed up. In this tutorial you give each branch its own directory (a git
**worktree**) and its own session, so two agents work side by side without
touching each other's files.

**You need:** [Your first session](first-session.md) completed, and a git
repository with a `main` (or `master`) branch to experiment in.

## 1. Start a feature in its own worktree

From your repository:

```bash
cd ~/code/my-project
tclaude worktree add feat/greeting
```

This does three things:

1. creates the branch `feat/greeting` from `main`;
2. checks it out in a new directory next to your repository,
   `../my-project-feat--greeting` (a `/` in the branch name becomes `--`);
3. starts a session there with your default harness.

Give the agent a small task, for example:

> Add a `--greeting` option to the CLI that prints a friendly hello.

Then detach with **Ctrl+B D** and let it work.

## 2. Start an urgent fix next to it

The feature agent is busy. Start a second worktree for the fix, also from
`main`:

```bash
tclaude worktree add fix/typo
```

A second session opens in `../my-project-fix--typo`. Ask it to fix something
small, then detach again. Both agents now run at the same time, each in its
own checkout. Neither sees the other's uncommitted changes.

!!! info "Go deeper"
    [Worktrees → Creating a worktree](../worktrees.md#creating-a-worktree)
    covers choosing the base branch, copying a conversation into the new
    worktree, and creating one without a session.

## 3. See what is running where

```bash
tclaude worktree ls
tclaude session watch
```

`worktree ls` lists every worktree with its path and branch. `session watch`
shows both sessions, and the **PROJECT** column tells them apart by
directory. Attach to either one with **Enter**.

## 4. Move your shell between worktrees

`tclaude worktree switch` finds a worktree by branch name. A command cannot
change your shell's directory by itself, so install a small shell wrapper
once. For bash:

```bash
mkdir -p ~/.config/tclaude
curl -fsSL -o ~/.config/tclaude/worktree-switch.bash \
  https://raw.githubusercontent.com/tofutools/tclaude/main/scripts/tclaude-worktree-switch.bash
echo 'source ~/.config/tclaude/worktree-switch.bash' >> ~/.bashrc
```

Open a new terminal, then jump around:

```bash
tclaude worktree switch feat/greeting
tclaude worktree switch main      # or master, whichever your main checkout is on
```

The zsh and fish wrappers sit next to it in the repository's `scripts/`
directory.

!!! info "Go deeper"
    [Worktrees → Switching between worktrees](../worktrees.md#switching-between-worktrees)
    has the zsh and fish versions.

## 5. Each worktree has its own history

A worktree is its own project directory, so its conversations are listed
there:

```bash
tclaude worktree switch fix/typo
tclaude conv ls
```

This shows only the fix's conversation. Tomorrow, `tclaude conv resume`
picks it up in the right worktree.

## 6. Clean up

Once the fix is merged, exit its session, then step back to the main
checkout (use `master` if that is your default branch). You cannot remove
the worktree your shell is standing in. Pull, so git can see the merge,
then remove the worktree and its branch:

```bash
tclaude worktree switch main
git pull
tclaude worktree rm fix/typo -D
```

`rm` asks for confirmation, and refuses if the worktree has uncommitted
changes, so you cannot lose work by accident. `-D` deletes the branch only
if it is merged into your current branch. After a squash merge, git cannot
tell; add `-f` to force it (this also skips the uncommitted-changes check).

## What you learned

- `tclaude worktree add <branch>` creates a branch, a separate checkout and
  a session in one step.
- Sessions in different worktrees run in parallel without sharing files.
- `worktree switch`, with the shell wrapper, moves your shell between them.
- Each worktree keeps its own conversation history.
- `worktree rm -D` cleans up a finished branch.

**Next:** [Your first agent team](first-team.md). Agents can get their own
worktrees too: `tclaude agent spawn --worktree <branch>`.
