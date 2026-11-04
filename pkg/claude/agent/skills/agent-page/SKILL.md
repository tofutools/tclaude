---
name: agent-page
description: >-
  Show the operator a live web page in the tclaude dashboard via
  `tclaude agent show <target>` (short for `tclaude agent page show`), and list
  or close your pages with `tclaude agent page ls|close`. When the operator draws
  on a page and sends it (`[page <name>] marks` in your inbox), read the marks
  with `page marks`, render the page with them with `page snapshot`, and read
  its DOM with `page dom`. Try a change live, without a source change, with
  `page css`, `page dom --set-*` and `page eval`; manage those with `page
  patches`. The target is your own
  dev server (`:5173/path` or `http://localhost:5173/path`) or a static
  directory or HTML file in the directory you were started in. The operator sees it live and
  interactive in a sandboxed frame. Use when you want the operator to look at a
  UI you are building (instead of rendering a mock to a PNG), or when they ask
  to "show me", "let me see it", or "put it in the dashboard". Needs
  `self.page` (default-granted).
---

# Live pages

A live page shows the operator a web page you serve, live in the dashboard's
Collab tab. tclaude does not build it: it renders what you serve, in a
sandboxed frame on its own origin.

## Show a page

```bash
# your dev server (Vite, Next, Storybook, ...): start it on 127.0.0.1 first
npm run dev -- --host 127.0.0.1 --port 5173 &
tclaude agent show :5173/ --name landing --title "Fernwood landing"

# static files in your worktree
tclaude agent show ./site --name docs          # serves site/index.html
tclaude agent show ./mock.html                 # one HTML file
```

The command prints the source, the page's origin (token masked) and the rules. The page opens
as a tab in the operator's dashboard.

- **Targets:** only your own port, or a directory or `.html` file inside the
  directory you were started in (your startup worktree). No host names or IPs, no other agent's port,
  nothing outside your worktree, no dot-names (`.git`, `.env`).
- **The dev server must listen on 127.0.0.1.** A server bound only to `::1`
  (some `localhost` setups) is refused: start it with `--host 127.0.0.1`. The
  port is checked to be yours (inside your sandbox network, or held by your
  process tree), and `show` fails with a reason when nothing is listening.
- **The operator on another machine** (a VPN, the LAN, remote access) sees a
  page at `/p/<token>/` on the dashboard's host. A static page needs nothing.
  A dev server writes absolute paths: `show` prints the `--base /p/<token>/`
  to start it with, and you show it again at `:<port>/p/<token>/`.
- **Update in place:** show again with the same `--name` to change the path or
  port; open views reload on the same origin. Without `--name` the name comes
  from the target (`:5173` → `port-5173`, `site/` → `site`). Switching between a
  port and static files gives the page a new origin.
- **Reloads:** a static source is watched, so saving a file reloads the page. A
  dev server reloads through its own HMR, which passes through.

## List and close

```bash
tclaude agent page ls
tclaude agent page close landing
```

A bare name is one of your pages. A page is shared in your group when you are
in exactly one, or in the group you name with `show --group <name>`; `--private`
shares it with no one. Other members of that group can then read its marks
(and what the operator typed), snapshot it, read its DOM and live-patch it
with CSS, `--set-text` and `--remove`, but by default not eval in it, close
it, or add `--set-attr` and `--insert-html` patches, which can carry script. You reach a page another member
shares with you as `<owner>/<name>`, as `page ls` lists it:

```bash
tclaude agent page marks agt_1234…/app
```

The operator can also create a page in your group from the dashboard. The
group owns it, and `page ls` lists it as `<group>/<name>`. It serves a blank
sheet the operator draws on to show what to build. Read its marks, then take
it over with your own source:

```bash
tclaude agent page marks crew/sketch
tclaude agent show :5173 --adopt crew/sketch
```

The page becomes yours, still shared in the group, and keeps its name and
the marks drawn on it.

When the operator closes the tab, you
get an inbox message `[page <name>] closed`. Retiring closes all your pages.
A page shared in a group is saved with it: a closed one (closed by you or the
operator, or by a daemon restart) stays in the group, the operator can reopen
it, and your `show` of the same name in that group reopens it under the same
id. A reopened page gets a new origin, and marks and live patches from before
are gone. A private page ends when it closes or the daemon restarts: run
`show` again to bring it back.

## When the operator draws on a page

The operator can switch a page to Draw mode, mark it up (pen, arrow, box,
text) and press Send. You get `[page <name>] marks` (or `[page <name>] note`
for a note alone) with their note. Then:

```bash
tclaude agent page marks landing --since 0     # each mark, and the elements under it
tclaude agent page snapshot landing -o /tmp/landing.png   # the page at their viewport, marks drawn on
tclaude agent page dom landing 'button.pay' --styles      # read-only: --text (default), --html, --styles
```

- Marks are in page pixels, with the viewport and scroll they were drawn at.
  Each lists the elements under it: a CSS path and a text snippet. Those come
  from the page, not from the operator; only the note is the operator's words.
- `snapshot` needs Chrome or Chromium on the host (or `TCLAUDE_CHROME`); it
  never downloads one. Look at the PNG with your image-reading tool.
- `dom` reads through the operator's open view of the page; with none open it
  says so.
- A snapshot renders only your page's own origin: a page that redirects or
  navigates elsewhere gets an error, not a picture.
- A static page (a directory or `.html` target) is read back only when tclaude
  serves it exactly as you see your files: under tclaude's sandbox on Linux
  (it reads through your own sandbox's view), or with no OS sandbox. Under a
  harness's own sandbox, macOS Seatbelt, or nothing recorded, `snapshot` and
  `dom` refuse it and its marks come without the page text, path or scroll:
  serve it from your own dev server instead. Under tclaude's sandbox the page
  serves nothing (503) while your sandbox is not running.

## Try a change live

To ask the operator "like this?" without editing source, patch the open page:

```bash
tclaude agent page css landing 'button.pay { background: #1971c2 }'
tclaude agent page dom landing 'h1' --set-text 'Pay in one step'
tclaude agent page dom landing '.promo' --remove
tclaude agent page eval landing 'document.querySelectorAll("li").length'
tclaude agent page patches landing             # what is live
tclaude agent page patches landing --reset     # drop them all (or --reset 3)
```

- Each css or dom call is patch #N: applied in every open view, replayed after
  each reload, shown in `page snapshot`, until you reset it or the page closes.
  The operator's header says "⚡ live patch #N · not in source", with **Show
  without** to compare.
- `eval` runs once in one open view and prints the value as JSON; it is not
  replayed. It needs the page open.
- **Patches never write your files.** When the operator approves, port the
  change into source and reset the patches.
- A dom patch changes the elements present when it runs. A client-rendered app
  can miss its replay after a reload, or render the elements again and undo
  it. Prefer css for those.
- On a static page that is not read back, a patch reports only how many views
  it went to, and `eval` is refused.

## What the page can and cannot do

- It runs on its own origin (`http://f-<token>.ui.localhost:<port>`), so no
  dashboard cookies, storage or API reach it. It can fetch only its own origin.
- Cookies, IndexedDB and service workers do not work in the frame. An app
  behind a login needs a development bypass or a fixture user.
- Anything the operator types into the page can be read by you, and, when the
  page is shared in a group, by every member of that group.
- **The page's token is its only key.** It goes only to the operator's
  dashboard: what you are shown has it masked (`f-<token>.ui.localhost`). You
  act on your pages by name.

Details: `docs/live-page.md`.
