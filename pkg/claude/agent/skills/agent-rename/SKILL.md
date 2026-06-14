---
name: agent-rename
description: >-
  Rename your own tclaude-managed conversation via `tclaude agent rename
  "<title>"`. tclaude agentd applies the harness-specific rename path on your
  behalf (Claude Code uses `/rename` injection; Codex CLI updates its title
  store), gated on the `self.rename` permission. Use when the user asks you to
  rename the conversation/session/agent, or when you decide to give yourself a
  clearer name (e.g. after taking on a new role in a group). Manager pattern:
  `tclaude agent rename "<title>" --target <peer>` renames ANOTHER agent
  (requires the `agent.rename` slug, OR being an owner of a group containing the
  target).
---

# Renaming yourself

`tclaude agent rename` asks the local `tclaude agentd` daemon to apply
the rename through the right mechanism for your harness. For Claude
Code, the daemon injects `/rename <title>` into your tmux pane because
the slash command is not callable from a tool. For Codex CLI, the
daemon updates Codex's title store directly because Codex has no
in-pane rename command.

## Prerequisite: daemon must be running

If you see `Error: tclaude agentd is not running.`, ask the human to
start it:

```bash
tclaude agentd serve   # in a non-sandboxed terminal
```

## Prerequisite: self.rename permission

Self-rename is opt-in. The fastest path is
`tclaude setup --install-default-agent-permissions`, which grants
`self.rename` (alongside `self.compact` and `self.reincarnate`) as
defaults in one shot. Manual alternatives:

**Option 1 — globally for every agent.** Either edit
`~/.tclaude/config.json`:

```json
{
  "agent": {
    "default_permissions": ["self.rename"]
  }
}
```

…or run:

```bash
tclaude agent permissions grant default self.rename
```

**Option 2 — only for one specific conversation.** This grant lives
in SQLite (`agent_permissions`), not config.json. Run:

```bash
tclaude agent permissions grant <conv-id-or-title> self.rename
```

(The CLI resolves the selector to a full conv-id and persists the
grant under it. Per-agent grants ADD to the defaults; they don't
replace them.)

If you see `Error: caller is not granted permission "self.rename"`,
the human has not opted in. Quote one of the commands above so they
know exactly what to run.

## Renaming

```bash
tclaude agent rename "code reviewer frontend"
```

The new title is what `tclaude agent ls`, `tclaude conv ls`, and the
agent-coord routing layer all use to identify you. Pick something
descriptive of your current role, not your model.

### Title charset (strict)

Titles are restricted to **`[A-Za-z0-9_\-\[\]{}() ]`, 1–64
characters**, with these extra rules:

- **Single ASCII spaces only.** Consecutive spaces (`"  "`) are
  rejected. Tabs, newlines, NBSP, etc. are rejected.
- **No slashes, quotes, punctuation outside the brackets/parens**, no
  unicode, no control characters.

This is a hard security constraint enforced by the daemon, not a
style preference: for harnesses renamed by pane injection, the title
becomes literal `tmux send-keys` input, so anything in it would land
in the input box. A permissive charset would let an agent sneak a
newline + another `/<command>` into a "rename" and execute arbitrary
slash commands.

Examples that work:

```bash
tclaude agent rename code-reviewer-frontend
tclaude agent rename code_reviewer_frontend
tclaude agent rename "code reviewer frontend"
tclaude agent rename "[reviewer] frontend"
tclaude agent rename "reviewer(frontend)"
```

Anything else is rejected with `invalid_title` (HTTP 400) — both
client-side (fast fail) and daemon-side (the actual gate). The error
body says **REJECTED** explicitly so you know not to retry with a
similar title; pick a different one that uses only the allowed
characters.

## What can go wrong

- **Mid-conversation typing can be lost on injection-based harnesses.**
  For Claude Code the mechanic is literal keystroke injection into
  your input box: anything you'd typed but not submitted gets
  prepended to the `/rename` command. Don't rename while you have
  unsubmitted input in your textarea.

- **No live tmux session.** If the daemon can't find an alive tmux
  session for your conv-id, you'll get `no_tmux` 503. This usually
  means you started outside of `tclaude` and there's no tmux pane for
  the daemon to reach. Ask the human to wrap your session via tclaude.

- **Claude Code catches up on the next turn.** For Claude Code, the
  new title is reflected in the JSONL on the next turn after `/rename`
  lands. Tools that read the conv-index right after `tclaude agent
  rename` returns may still see the old title for a beat.

## Why a separate command instead of just calling /rename

You're a tool-using agent — slash commands inside the TUI aren't part
of your tool surface. Even if you wrote `/rename foo` in chat, a
harness that supports slash commands would treat it as plain text, not
a command. The daemon owns the tmux side and the harness-specific title
store updates, so it can do the operation that you can't.

## Manager pattern: rename ANOTHER agent

`tclaude agent rename` accepts an optional `--target <selector>`
that swaps the action onto a peer instead of yourself. Useful when
spawning a worker into a role and you want its tab/title to reflect
that role from the outside:

```bash
tclaude agent rename "auth-refactor-worker" --target worker-1
```

Auth model: the caller passes if EITHER

- they hold the `agent.rename` slug (default human-only — granted
  via `tclaude agent permissions grant <caller> agent.rename`), OR
- they own at least one group that contains the target.

Same charset gate applies. The response includes `caller_conv` so
the audit trail records who renamed it. `--ask-human` is **not**
honored on the cross-agent path — manager pattern is opt-in via
explicit grants.

## Etiquette

- One rename per session is usually enough. Picking a name and
  sticking with it makes the human's `tclaude agent ls` output
  stable.
- If the user asked you to rename, do it once and confirm in chat;
  don't rename repeatedly to "tune" the title.
- The human can always rename you back via plain `/rename` at any
  time — defer to their choice if there's a conflict.
- For the cross-agent path: don't rename peers without a clear
  reason. The target's identity (especially if it's already
  speaking to the human in chat) shouldn't shift under their
  feet.
