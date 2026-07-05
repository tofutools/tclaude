---
name: human-clipboard
description: >-
  Copy text to the HUMAN's system clipboard via `tclaude agent clipboard` — for
  when the human asks you to put a draft, command, or snippet on their
  clipboard. The `tclaude agentd` daemon runs the platform copy tool on the
  host (your sandbox can't reach the display). Permission-gated on the
  `human.clipboard` slug: NOT granted by default and NOT implied by group
  ownership, so you need an explicit grant or a per-call `--ask-human` popup
  approval. Use when the human says "copy this to my clipboard / put X on my
  clipboard". NOT for agent-to-agent data passing — that is `agent-coord`.
---

# Copying to the human's clipboard

When the human asks you to put something on their clipboard — a drafted
message, a shell command, a code snippet — copy it with:

```bash
tclaude agent clipboard "git rebase -i main"
```

Your sandbox can't reach the human's display, so the **`tclaude agentd`
daemon performs the write** on the host using the platform copy tool
(`wl-copy`/`xclip`/`xsel` on Linux, `pbcopy` on macOS, `clip.exe` under
WSL). If no tool is available the command fails with a clear error rather
than silently dropping the copy.

## Prefer `--file` / stdin for anything non-trivial

Inline command-line text suffers shell quoting (backticks are eaten) and
is visible in `/proc`. For long, multi-line, or code-heavy content, pass
it via a file or stdin:

```bash
tclaude agent clipboard --file draft.md      # content from a file
generate-snippet | tclaude agent clipboard --file -   # content from stdin
```

Content is copied **verbatim** — leading/trailing whitespace and newlines
are preserved. The payload is capped at 256 KiB.

## Permission

Copying is gated on the **`human.clipboard`** slug. Unlike `human.notify`,
it is **not** default-granted **and not implied by group ownership** —
writing the operator's real clipboard is a machine surface that always
needs an explicit grant or a one-off approval. Without the grant you get a
`403` naming the slug.

To copy just this once without the grant, add `--ask-human <duration>`:

```bash
tclaude agent clipboard --ask-human 60s "text to copy"
```

That creates an access request in the dashboard Messages tab, showing a
**preview of exactly what would be copied** before they Approve / Deny.
Timeout = deny. Do not retry in a loop if denied. A browser auto-open or
OS banner only happens when the human has opted into those extra
access-request alerts in tclaude config.

## When to use this — and when not to

**Use it** when the human explicitly wants something on their clipboard so
they can paste it elsewhere (into a browser, another app, a chat).

**Do not use it for:**

- **Passing data to another agent.** That is `agent-coord`
  (`tclaude agent message`). The clipboard reaches the *human's machine*,
  not an agent.
- **Routine output.** Your normal terminal output is still your primary
  channel; the clipboard is only for content the human asked to have on
  hand for pasting.
