---
name: rename-conv
description: Rename the current Claude Code conversation when work state changes — PR opened, merged, closed, or an issue is being investigated. Invoke proactively after `gh pr create`, `gh pr merge`, `gh pr close`, or when starting investigation of a ticket so the conversation list reflects current state. Title format is `[State:<owner>/<repo>/pull/<n>] short summary` for PRs, or `[State:<key>] summary` for tickets.
---

# Rename the current conversation when work state changes

You should invoke this skill **right after** you cause a state milestone:

- `gh pr create` succeeded → set title to `[PR:<owner>/<repo>/pull/<n>] <pr title>`
- `gh pr merge` succeeded → set title to `[Merged:<owner>/<repo>/pull/<n>] <pr title>`
- `gh pr close` (without merge) → set title to `[Closed:<owner>/<repo>/pull/<n>] <pr title>`
- Started investigating a ticket → `[Investigating:<KEY-123>] <issue summary>`
- Work paused / handed off → `[Paused] <one-line context>`

The point is that `tclaude conv ls` becomes a worklog instead of a wall of
auto-derived first prompts.

## How to call

```bash
tclaude conv rename . "<new title>"
```

`.` resolves to the current conversation via `$TCLAUDE_SESSION_ID` (set by
tclaude when launching a session).

To clear a custom title (e.g. PR was closed and you want a clean title back):

```bash
tclaude conv rename . --strip
```

## Renaming any conversation (not just the current one)

```bash
tclaude conv rename abc12345 "[Merged:my-org/my-repo/pull/417] fix river ctx"
```

## Don't invoke

- For trivial intermediate states (a single commit pushed, no PR yet).
- When the user explicitly asked you not to rename.
- For one-off scratch sessions that won't be revisited.

## Installing this skill

This skill lives in `examples/skills/` in the tclaude repo. Copy or symlink it
to make it active:

```bash
ln -s "$(pwd)/examples/skills/rename-conv" ~/.claude/skills/rename-conv
```
