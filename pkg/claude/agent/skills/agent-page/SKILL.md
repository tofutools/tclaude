---
name: agent-page
description: >-
  Show the operator a live web page in the tclaude dashboard via
  `tclaude agent show <target>` (short for `tclaude agent page show`), and list
  or close your pages with `tclaude agent page ls|close`. The target is your own
  dev server (`:5173/path` or `http://localhost:5173/path`) or a static
  directory or HTML file in the directory you were started in. The operator sees it live and
  interactive in a sandboxed frame. Use when you want the operator to look at a
  UI you are building (instead of rendering a mock to a PNG), or when they ask
  to "show me", "let me see it", or "put it in the dashboard". Needs
  `self.page` (default-granted) and Collab on.
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

You can act only on pages you showed. When the operator closes the tab, you
get an inbox message `[page <name>] closed`. Retiring closes all your pages.
Pages end when the daemon restarts: run `show` again to bring one back (it gets
a new origin).

## What the page can and cannot do

- It runs on its own origin (`http://f-<token>.ui.localhost:<port>`), so no
  dashboard cookies, storage or API reach it. It can fetch only its own origin.
- Cookies, IndexedDB and service workers do not work in the frame. An app
  behind a login needs a development bypass or a fixture user.
- Anything the operator types into the page can be read by you.
- **The page's token is its only key.** It goes only to the operator's
  dashboard: what you are shown has it masked (`f-<token>.ui.localhost`). You
  act on your pages by name.

Details: `docs/live-page.md`.
