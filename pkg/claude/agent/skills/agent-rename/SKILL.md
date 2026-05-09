---
name: agent-rename
description: Rename your own Claude Code conversation via `tclaude agent rename "<title>"`. The slash command `/rename` runs inside the CC TUI so you can't call it directly; tclaude agentd injects it into your tmux pane on your behalf, gated on the `self.rename` permission. Use when the user asks you to rename the conversation/session/agent, or when you decide to give yourself a clearer name (e.g. after taking on a new role in a group).
---

# Renaming yourself

Claude Code's `/rename` slash command runs inside the TUI and is not
callable from a tool. `tclaude agent rename` works around this by
asking the local `tclaude agentd` daemon to inject `/rename <title>`
into your own tmux pane via `send-keys`. The next CC turn picks up
the new title.

## Prerequisite: daemon must be running

If you see `Error: tclaude agentd is not running.`, ask the human to
start it:

```bash
tclaude agentd serve   # in a non-sandboxed terminal
```

## Prerequisite: self.rename permission

Self-rename is opt-in. The human grants it in
`~/.tclaude/config.json`:

```json
{
  "agent": {
    "default_permissions": ["self.rename"]
  }
}
```

Or, to grant it only to specific conversations:

```json
{
  "agent": {
    "permission_overrides": {
      "<conv-id-or-prefix-or-title>": ["self.rename"]
    }
  }
}
```

If you see `Error: caller is not granted permission "self.rename"`,
the human has not opted in. Quote the JSON snippet above so they
know exactly what to add.

## Renaming

```bash
tclaude agent rename "code-reviewer-frontend"
```

Quotes are optional unless your title contains spaces — easier to
just always quote.

The new title is what `tclaude agent ls`, `tclaude conv ls`, and the
agent-coord routing layer all use to identify you. Pick something
descriptive of your current role, not your model.

## What can go wrong

- **Mid-conversation typing is lost.** The mechanic is literal
  keystroke injection into your input box: anything you'd typed but
  not submitted gets prepended to the `/rename` command. Don't rename
  while you have unsubmitted input in your textarea.

- **No live tmux session.** If the daemon can't find an alive tmux
  session for your conv-id, you'll get `no_tmux` 503. This usually
  means you started CC outside of `tclaude` and there's no tmux pane
  to inject into. Ask the human to wrap your session via tclaude.

- **CC catches up on the next turn.** The new title is reflected in
  the JSONL on the next CC turn after `/rename` lands. Tools that
  read the conv-index right after `tclaude agent rename` returns may
  still see the old title for a beat.

## Why a separate command instead of just calling /rename

You're a tool-using agent — slash commands inside the TUI aren't part
of your tool surface. Even if you wrote `/rename foo` in chat, CC
would treat it as plain text, not a command. The daemon owns the
tmux side and is outside your sandbox, so it can do the keystroke
injection that you can't.

## Etiquette

- One rename per session is usually enough. Picking a name and
  sticking with it makes the human's `tclaude agent ls` output
  stable.
- If the user asked you to rename, do it once and confirm in chat;
  don't rename repeatedly to "tune" the title.
- The human can always rename you back via plain `/rename` at any
  time — defer to their choice if there's a conflict.
