# Plan: `tclaude conv rename` + agent-driven conversation titles

Status: draft, not implemented.

## Goal

Let Claude Code agents rename the current conversation as work state changes, so the conversation list reads like a worklog instead of a wall of auto-derived first-prompt snippets. Example:

- During work → `[PR:my-org/my-repo/pull/417] fix: pass app lifetime ctx to runner.Start`
- After merge  → `[Merged:my-org/my-repo/pull/417] fix: pass app lifetime ctx to runner.Start`
- For an issue → `[Investigating:PROJ-2940] river client shuts down at boot`
- After PR close (no merge) → `[Closed:my-org/my-repo/pull/417] ...`

The agent recognises state milestones it just caused (`gh pr create`, `git commit`, `gh pr merge`, `git push` on a merged branch, etc.) and renames itself accordingly. State changes that happen *outside* the session (a colleague merges the PR while you're away) are picked up by an external poller — out of scope for this plan, but the data model should leave room for it.

## UX

### Manual

```bash
# Rename by full ID
tclaude conv rename abc12345-def6-7890-abcd-ef1234567890 "[PR:my-org/my-repo/pull/417] fix river ctx"

# Rename by short prefix
tclaude conv rename abc12345 "[Merged:my-org/my-repo/pull/417] ..."

# Rename by current title (handy if it was already renamed once)
tclaude conv rename "[PR:my-org/my-repo/pull/417] fix river ctx" "[Merged:my-org/my-repo/pull/417] fix river ctx"
```

### Agent-driven

Claude Code skill `rename-conv` (or similar). Description triggers the agent to invoke when work milestones are crossed (PR opened/merged/closed, branch pushed for review, etc.). Skill body tells Claude how to call `tclaude conv rename`, including how to discover the current session ID (env var, see "Identifying the current session" below). Auto-trigger relies on the agent noticing the milestone — same model as `commits.md`, `pr.md` skills.

## CLI spec

```
tclaude conv rename <selector> <new-name>
```

**`<selector>`** resolves to exactly one conversation. Resolution order:

1. Full conversation ID (`abc12345-def6-7890-abcd-ef1234567890`) — direct match.
2. Short prefix (`abc12345`) — match if uniquely identifying.
3. Current custom title — exact string match across the project's conversations.
4. (Optional, future) Current auto-derived title — only if uniquely identifying.

If multiple conversations match, fail with a list of candidates and require disambiguation by ID. Same conv-id formats listed in `conversations.md` already apply.

**`<new-name>`** is a free-form string; tclaude only enforces:

- Non-empty, max length (e.g. 200 chars).
- No newlines or null bytes.
- No filesystem-special characters that would break sidecar storage (see below).

**Flags:**

| Flag           | Description                                                  |
|----------------|--------------------------------------------------------------|
| `-g, --global` | Allow selectors to match any project, not just current.      |
| `-y, --yes`    | Don't prompt when overwriting an existing custom title.       |
| `-s, --strip`  | Remove the custom title (revert to auto-derived first-prompt). |

(`-s` lets the agent un-rename when a state ends, e.g. PR was closed without merge and you want a clean title back.)

**Exit codes:** `0` success, `1` selector miss, `2` ambiguous selector, `3` invalid name, `4` IO failure.

## Storage

Two viable shapes; pick one before implementing.

### Option A — sidecar metadata file (preferred)

`~/.claude/projects/<project-slug>/<conv-id>.title` containing the custom title as a single line of UTF-8.

- **Pro:** non-destructive — the original conversation `.jsonl` stays untouched, so an upgrade or rebuild of `tclaude` can drop the rename feature without losing data.
- **Pro:** easy to delete (`-s` just removes the sidecar).
- **Con:** `conv ls` has to scan two files per conversation; small overhead on large project dirs.

### Option B — write into the conversation `.jsonl`

Add a `custom_title` field to the conversation header.

- **Pro:** single source of truth.
- **Con:** mutating the file Claude Code itself owns risks corruption; harder to recover; CLI now has a write-lock concern.

**Recommendation:** A. Reads in `conv ls`/`conv search` should prefer the sidecar title when present, fall back to the existing derivation otherwise.

## Identifying the current session

For the agent path, `tclaude conv rename` must be invocable without the agent typing out a UUID. Options:

1. **Env var** set by tclaude when launching the session, e.g. `TCLAUDE_CONV_ID=<uuid>`. `tclaude conv rename .` (or `rename --current`) resolves to `$TCLAUDE_CONV_ID`. Cleanest.
2. **Detect from tmux pane** — fragile; skip.
3. **Claude Code's own session-id env var** if exposed (`CLAUDE_SESSION_ID`?) — verify before designing around it.

Whichever wins, document it in the skill's body so the agent knows the exact incantation.

## Skill spec (sketch)

`~/.claude/skills/rename-conv/SKILL.md`:

```markdown
---
name: rename-conv
description: Rename the current Claude Code conversation when work state changes — PR created/merged/closed, issue opened, milestone reached. Invoke proactively after running `gh pr create`, `gh pr merge`, or similar state-changing commands so the conversation list reflects current state. Format: `[State:<owner>/<repo>/pull/<n>] short summary`.
---

# Rename the current conversation

Use after a state milestone you just caused. Examples:

| After                         | New title                                              |
|-------------------------------|--------------------------------------------------------|
| `gh pr create` succeeded      | `[PR:<owner>/<repo>/pull/<n>] <pr title>`              |
| `gh pr merge` succeeded       | `[Merged:<owner>/<repo>/pull/<n>] <pr title>`          |
| PR closed without merge       | `[Closed:<owner>/<repo>/pull/<n>] <pr title>`          |
| Started investigating an issue | `[Investigating:<key>] <issue summary>`               |
| Work paused / handed off      | `[Paused] <one-line context>`                          |

## How to call

```bash
tclaude conv rename "$TCLAUDE_CONV_ID" "<new title>"
```

(or `--current` shorthand once implemented.)

## Don't invoke

- For trivial intermediate states (commit pushed but no PR yet).
- When the user explicitly told you not to rename.
```

## Open questions

1. **Source of state truth** — agent-driven only, or should tclaude also expose `tclaude conv title-from-pr <id>` that polls GitHub and rewrites? If yes, that's a separate command, not part of this plan.
2. **Concurrency** — two `rename` calls racing on the same sidecar: rely on atomic-write (`O_EXCL` + rename) and last-writer-wins. Probably fine.
3. **Migration** — none needed if we go with sidecar storage.
4. **Watch mode display** — `conv ls -w` should presumably show custom titles in a dedicated column or replace the auto-derived one; needs UX decision.
5. **Skill auto-invocation reliability** — skill descriptions only nudge the agent; verify in dogfooding that PR-create / merge actually do trigger the rename without explicit prompting.

## Out of scope

- External poller that updates titles when the PR state changes outside a session.
- Cross-project rename (the `-g` flag covers selection, but the title format is project-relative).
- Title templating language (`{repo}`, `{pr_number}`, etc.) — agent constructs the string itself for now.
