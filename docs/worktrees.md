# Worktrees

Git worktrees give each branch its own checkout directory, and `tclaude
worktree` pairs that with sessions: one worktree per branch, one coding
session per worktree, so parallel work never fights over a single checkout.
Each worktree keeps its own conversation history, and switching tasks is a
directory change rather than a stash.

## Creating a worktree

```bash
# New branch + worktree + session
tclaude worktree add feat/my-feature

# Base it on another branch (default: main/master)
tclaude worktree add feat/my-feature --from-branch develop

# Copy an existing conversation into the new worktree
tclaude worktree add feat/my-feature --from-conv abc12345

# Create only — no session
tclaude worktree add feat/my-feature -d
```

`worktree add` creates the branch if needed, adds a worktree at
`../<repo>-<branch>` (slashes in the branch name become `--`; `-p/--path`
overrides), optionally copies a conversation over (`--from-conv`, with `-g`
to search globally), and then starts a session in the new directory unless
`-d/--detached` is set.

The launched session resolves its harness the same way a bare `tclaude`
does: global default spawn profile first, then an installed harness from
`PATH` (Claude Code preferred). It is not pinned to any harness — though the
printed progress strings still say "Starting Claude session". `worktree add`
takes no harness or model flags of its own; to force a specific harness,
create the worktree detached and launch explicitly:

```bash
tclaude worktree add feat/my-feature -d
tclaude session new -C ../myrepo-feat--my-feature --harness codex
```

Agent spawns can also create their worktree at spawn time with `tclaude
agent spawn --worktree` and friends — see
[Spawning](spawning-and-lifecycle.md).

## Restoring a worktree

```bash
tclaude worktree restore feat/my-feature
```

`restore` recreates a removed worktree from the local branch, or — when the
branch only exists on the remote — fetches it and creates a tracking branch
first. Like `add`, it then starts a session in the restored directory unless
`-d` is set, through the same harness resolution.

## Listing and removing

```bash
tclaude worktree ls        # path / branch / commit table (-v for more)
tclaude worktree rm feat/my-feature
tclaude worktree rm feat/my-feature -D    # also delete the branch
```

`rm` accepts a branch name or a path and refuses to remove a worktree with
uncommitted changes unless `-f/--force` is passed.

## Switching between worktrees

`switch` (aliases `s`, `checkout`, `c`) prints the target worktree's path. A
subprocess cannot change your shell's directory, so actually cd-ing needs a
small shell wrapper — source the one for your shell from the repo's
`scripts/` directory:

=== "Zsh"

    Add to `~/.zshrc`:

    ```bash
    source /path/to/tclaude/scripts/tclaude-worktree-switch.zsh
    ```

=== "Bash"

    Add to `~/.bashrc`:

    ```bash
    source /path/to/tclaude/scripts/tclaude-worktree-switch.bash
    ```

=== "Fish"

    Add to `~/.config/fish/config.fish`:

    ```fish
    source /path/to/tclaude/scripts/tclaude-worktree-switch.fish
    ```

The wrapper intercepts `tclaude worktree switch` (and its aliases), runs the
real command, and `cd`s to the printed path on success; every other tclaude
invocation passes through untouched. Then:

```bash
tclaude worktree switch feat/my-feature
tclaude worktree s main
```

## Worktrees and sessions

Each worktree is its own project directory, so sessions launched in it index
their conversations there: `tclaude conv ls` inside a worktree shows that
branch's history, and the conversation copied in by `--from-conv` resumes in
the worktree, not the original checkout. Sessions in different worktrees of
the same repo run fully in parallel — separate checkouts, separate
conversation histories, no shared working tree to trip over.

!!! tip "Claude Code's own worktree prompt"
    When an agent calls Claude Code's `EnterWorktree` tool for a worktree
    outside the directory Claude Code manages itself, the confirmation is a
    hardcoded safety check no allow-rule or hook can pre-approve — the agent
    waits for a keystroke. An operator who wants that to run unattended can
    grant the agent `auto-permit.enter-worktree`; see
    [auto-permit](permissions-and-audit.md#auto-permit-pre-consenting-to-a-human-only-prompt).

## From the conversation browser

In [conversation watch mode](conversations.md#watch-mode), `W` creates a
worktree from the selected conversation: it prompts for a branch name, then
creates the worktree with that conversation copied into it.

## Example workflow

```bash
tclaude worktree add feat/auth-refactor
# ... work in the new session ...

# Urgent bug? Second worktree, second session, no stashing
tclaude worktree add fix/critical-bug --from-branch main

tclaude worktree switch feat/auth-refactor
tclaude worktree switch fix/critical-bug

tclaude worktree rm fix/critical-bug -D
tclaude worktree ls
```
