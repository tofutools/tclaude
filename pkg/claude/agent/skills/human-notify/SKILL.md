---
name: human-notify
description: >-
  Reach the human via `tclaude agent notify-human "<message>"` — the message
  lands in the agentd dashboard's Messages tab and may carry an agent-published
  downloadable file. Permission-gated on the `human.notify` slug (group owners
  also pass); the human grants the slug to a trusted coordinating agent (e.g.
  the PO), so an agent with neither the slug nor group ownership cannot post to
  the channel. Use to send the human status updates or questions that need a
  human decision. NOT for agent-to-agent messaging — that is the `agent-coord`
  skill.
---

# Notifying the human

The human does not watch a coordinating agent's terminal continuously —
it is full of agent-to-agent traffic and tool output. When you have
something the human should see, send it with:

```bash
tclaude agent notify-human "CI is green; PR #142 is up for review."
```

The message is stored and shown in the **Messages tab** of the agentd
dashboard (the last tab). The tab carries an unread-count badge, so the
human notices new messages from whatever tab they are on. Each message
shows who sent it and offers a button that focuses that agent's
terminal window — so the human can read a message and jump straight to
the agent to respond.

If the human has OS notifications enabled (`tclaude setup`), each
notify-human **also raises a desktop notification** — so it reaches them
even when the dashboard isn't open. This is on by default once
notifications are enabled and can be silenced with
`"human_messages": false` under `notifications` in `~/.tclaude/data/config.json`.

## It is an extra channel, not a replacement

Keep doing your normal terminal output. Your regular responses in your
own terminal / agent harness output are still your primary channel — the
human reads them when they are at the keyboard. `notify-human` does
**not** replace that and is not where your routine reporting goes:
print your normal output as always. `notify-human` is an *extra*,
explicit nudge layered on top — for the occasional message that
warrants pulling the human's attention when they may be away from the
terminal.

## When to use this — and when not to

**Use it for** the things the human should see:

- A milestone or status update worth surfacing ("all three workers
  finished; the branch is ready to merge").
- A question that blocks progress and only the human can decide
  ("worker hit a schema-design fork — need a call before continuing").

**Do not use it for:**

- **Agent-to-agent coordination.** Messaging a peer agent, a PO, or a
  group is the `agent-coord` skill (`tclaude agent message` / `reply`).
  `notify-human` reaches the *human*, not an agent.
- **Chatter.** Each message is a row in the human's Messages tab and
  bumps the unread badge. One message per genuinely notable event — not
  a running commentary.

## Sending

```bash
tclaude agent notify-human "<message>"
tclaude agent notify-human --subject "blocker" "<message>"
tclaude agent notify-human --file status.md          # body from a file
tclaude agent notify-human --file -                  # body from stdin
tclaude agent notify-human "Report ready" --attach report.md
tclaude agent notify-human "Site bundle ready" --attach dist/ --name site.zip
tclaude agent notify-human "Artifacts ready" -a report.md -a chart.png
tclaude agent notify-human --subject "Dashboard mock" -a mock.png   # no body
```

Prefer `--file` for long, multi-line, or code-heavy bodies — it
sidesteps shell quoting (an inline backtick is eaten by the shell).
Same reasoning as `tclaude agent message --file`.

### Sending without a body

A body is normally required, but you may omit it when the message **is**
the attachment: `--subject` plus at least one `--attach` is a complete
notification on its own. The subject names what arrived and the file
carries the content, so there is no need to invent a line of prose
restating it — a rendered mock, a screenshot, or a report sent for review
is often clearer with just a subject.

The two halves are both required for that form. A subject with nothing
attached is a headline over an empty page, and an attachment with no
subject says nothing about what arrived; either alone is rejected. Keep
writing a body whenever you actually have something to say about the file
— what to look at, what decision you need, what changed.

Use repeatable `--attach <path>` flags when the result itself is a file the
human should receive. A single file keeps its name and type. Up to 20 attached
files are published SEPARATELY, each keeping its own name and type, so the
human can open an image instead of unpacking an archive. A directory (zipped as
`<directory>.zip`) or a larger set is packaged into `export.zip`. Force either
shape with `--zip` / `--separate`. `--name` overrides the download filename and
implies `--zip` when several paths are attached. The daemon copies the bytes into its
private data directory, and the Messages reader exposes an authenticated
download button, so this also works through remote dashboard access. The reader
renders some types in place rather than only offering the download: an image
gets a thumbnail and a zoomable preview, and a Markdown file up to 1 MiB is
rendered directly in the message, with a **View** control that opens it in a
resizable viewer (whose Rendered/Source toggle reaches the original text) — so a written report is better sent as `report.md` than as
`report.txt`. A rendered report can illustrate itself: attach the images
alongside it (`--attach report.md --attach chart.png`) and reference one by its
filename (`![the chart](chart.png)`) to have it appear in the document. An
image at an `http(s)` URL is NOT fetched when the human opens the message; it
shows as a placeholder naming the host that the human must click to load, so
prefer attaching the picture. The cap is
256 MiB per published artifact. Deleting the message deletes the stored bytes.
Top-level symlinks are rejected; pass the resolved path explicitly. Stored
attachments are also capped at 512 MiB per stable agent and 2 GiB total, so
delete older attachment messages before publishing when a quota is full. Count
caps (1,000 published files per stable agent, 10,000 total) also apply,
including to empty files.

### Putting a decision to the human

When you need the human to choose — a design direction, a trade-off, which
of two approaches — ask with options instead of prose:

```bash
tclaude agent notify-human -s "Which layout for the fleet brief?" \
  "Tabs keep it compact; a sidebar shows more at once. Mock attached." \
  --option "Tabs" --option "Sidebar" --recommend 2 --attach mock.png
```

Give 2 to 6 short, single-line options; `--recommend N` marks the one you
suggest (counting from 1). The human sees one button per option in their
companion window (when they have you pinned) and in the Messages reader, and
voice reads the question out if they have voice on for you. Their pick
arrives in your inbox as a message starting `Decision: <option>`, with the
question it answers and any note they added — even if you are offline by
then. Only the first answer counts. Keep working on what does not depend on
the answer instead of waiting idle; one decision per message.

Decisions are part of the experimental Collab feature. If `--option` is refused
with `collab_disabled`, send the same message without it and list the choices in
the body; the human answers with a reply.

Use decisions for taste and direction calls, where the human's judgement is
the point. Do not use them for things you can decide yourself, and do not
ask the same question twice.

### Putting an action on a message

`--action` puts one button on the message, chosen from a fixed list:

```bash
tclaude agent notify-human "The build is broken here; have a look." --action terminal
tclaude agent notify-human "The layout is on the board." --action board:<id>
tclaude agent notify-human "Ready for review." --action pr:<url>
tclaude agent notify-human "Usage is close to the limit." --action tab:usage
```

- `terminal` opens a shell in your own tracked directory. It takes no path.
- `board:<id>` opens a board.
- `pr:<url>` opens a GitHub PR (`https://github.com/<owner>/<repo>/pull/<n>`)
  you presented with `tclaude agent present-pr` first.
- `tab:<name>` goes to `groups`, `terminals`, `automations`, `processes`,
  `plugins`, `access`, `messages`, `usage`, `diagnostics` or `config`.

The daemon resolves the target and writes the button's text, including the
directory and, for a terminal, that it runs outside your sandbox. Nothing
happens until the human clicks. You get no output back, and no word that they
clicked. A message takes one action, and a message is a decision or an action,
not both. Actions need Collab, like decisions: on `collab_disabled`, send the
message without `--action`.

## Permission

Sending is gated on the **`human.notify`** permission slug. It is
**not** in the global defaults — the human grants it to a trusted
coordinating agent (typically the Product Owner). Owning any active group
contributes an ordinary unscoped `human.notify` grant unless an explicit
same-slug **deny** override is set. An agent without an effective grant gets a
`403` naming the slug.

If you need to notify the human just this once and you do not hold the
slug, add `--ask-human <duration>`:

```bash
tclaude agent notify-human --ask-human 60s "<message>"
```

That creates an access request in the dashboard Messages tab. Timeout =
deny. Do not retry in a loop if denied. A browser auto-open or OS banner
only happens when the human has opted into those extra access-request
alerts in tclaude config.

## How the human reads it

The human opens the dashboard (`tclaude agent dashboard`) and the
**Messages** tab. Messages are listed newest-first with sender, group,
subject, body, and a read/unread marker; the human marks them read,
focuses the sending agent's window, or clears read messages. There is
a reply button too: a reply, or the pick on a decision, comes back to your
inbox. The focus button opens your window so the human can talk to you
there.
