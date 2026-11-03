# Live pages

Live pages are part of [Collab](companion.md#turning-collab-off). They let an agent show
the operator any web page it serves, live and interactive inside the dashboard.
tclaude does not build the page. It renders what the agent serves, in a
sandboxed frame.

!!! note "In progress"
    So far the daemon's stage listener, its page sources, the agent commands and
    the dashboard's Page tab exist: an agent can show, update, list and close
    pages, the operator uses them live in the Collab view and draws on top of
    them, and the agent reads the marks, renders snapshots and reads the DOM.
    Live patches come in a later release.

## Showing a page

An agent shows a page with `tclaude agent show <target>`, short for
`tclaude agent page show`:

```bash
tclaude agent show :5173/app --name landing --title "Fernwood landing"
tclaude agent show ./site            # a static directory, serves its index.html
tclaude agent show ./mock.html       # one HTML file
tclaude agent page ls
tclaude agent page close landing
```

The target is `:PORT[/path]`, `http://localhost:PORT/path`, a directory, or an
`.html` file. A file target must be inside the agent's startup directory, the
one its launch chose. The directory the agent's hooks report it last edited in
is never used, because the agent steers that with any path it touches. The
command prints the source, the page's origin with the token masked, and the
rules.

- **Validation happens before anything is dialled.** A host name or IP is
  refused, and so is `::1`, `https:`, a URL with credentials, a port of the
  daemon's own, a path outside the worktree and a dot-named path. A port target
  is proved once at show time, with the same checks the stage makes for every
  connection. A dev server that is not running, or not the agent's, is refused
  with the reason. The stage connects only to `127.0.0.1`, so a server bound only
  to `::1` is refused too, with a hint to start it with `--host 127.0.0.1`.
- **Update in place.** Showing again with the same `--name` updates that page:
  a new path or port, the same origin, and open views reload. Without `--name`
  the name comes from the target (`:5173` gives `port-5173`, `site/` gives `site`).
- **A new kind of source gets a new origin.** Re-showing a name with the other
  kind of source (a port where it was static files, or the reverse) closes the
  old origin and mints a new one. Page code that read the static files keeps
  them in memory, and it must not keep an origin that now reaches the agent's
  dev server.
- **Own pages only.** Every command resolves the page by name among the
  caller's own pages, so an agent cannot list, update or close another agent's
  page, and only the agent's current conversation can act (not a predecessor
  still running after a reincarnation). The permission is `self.page`, default-granted with the other `self.*`
  slugs. The commands are hidden, and the routes answer 404, while
  [Collab is off](companion.md#turning-collab-off).
- **Lifecycle.** A page belongs to the agent ID, so it survives a
  reincarnation. Retiring or deleting the agent closes its pages. When the operator closes a
  page in the dashboard, the agent gets an inbox message `[page <name>] closed`.
  Pages live in the daemon's memory only: restarting the daemon closes them, and
  page tokens are never written to disk.
- **Limits.** An agent may have 8 pages open, and the daemon 64.

The dashboard snapshot lists the open pages (`live_pages`), each with a revision
that changes whenever the page is shown or updated. The Page tab uses it.

## In the dashboard

![A live page in a Collab tab: the header names the agent, the target and the worktree, with the sandboxed chip, Reload, Open in overlay, Pop out and Close](assets/live-page-tab.png)

A page an agent shows opens as a **Page** tab (◧) in the Collab strip, behind
the tab you are on, so it never takes your focus. The tab has a dot until you
look at it, and a toast says `<agent> showed <title> · Open`. A page shown
again with the same name gets the dot again, and its open views reload. Pages
that were already open when the dashboard loaded open quietly, with no toast.

The header above the page says who showed it (a link to that agent's
terminal), its target and worktree, and carries a **🔒 sandboxed** chip. Hover
the chip for the page's origin and its rules, including that the agent that
showed the page can read what you type in it. Then:

- **Reload** loads the page again. Saved files under a static target and a
  re-show reload it on their own.
- **Open in overlay** floats it in an overlay window, and **Pop out** moves it
  to a browser window of its own. Like a board, a page tab can be reordered,
  grouped, dragged off the strip or onto an overlay window. A separate window
  is the dashboard's own page with the same frame in it: the page's origin
  never loads as a top-level page.
- **Close**, or the tab's ×, closes the page for good. The agent is told in
  its inbox. Moving a page between places does not close it.

### Drawing on top

![A live page in Draw mode: the boards' pen, arrow, box and text tools, the colours, undo, Clear, a note and Send to agent above the page, with a box and an arrow drawn over its form](assets/live-page-draw.png)

**Interact** / **Draw** in the header switches what the pointer does. In Draw
mode a layer over the page takes the boards' pen, arrow, box and text tools and
colours (red first), with undo and **Clear**. The layer is the dashboard's, not
the page's: the page cannot see or change the marks. Switch back to Interact to
scroll or click; the marks stay where they were drawn on the page, because they
are kept in page coordinates.

When a mark ends, the page's probe names the elements under it: a short CSS path
(`section#pay-area > button.pay`) and a text snippet. Type an optional note and
press **Send to agent**. The marks, the note, the page's path, viewport and
scroll go to the agent: its inbox gets `[page <name>] marks` with your note and
how to read them. A note with no marks is a plain `[page <name>] note`. Nothing
reaches the agent until you press Send. The batches are kept in memory with the
page (the last 50) and end with it.

The element paths and snippets come from the page, so they are treated as the
page's content: checked, shown only as text, and kept out of the inbox message.

When nothing answers for the page (the dev server stopped, say), the view says
so and offers **Retry**. A page the agent closes leaves the dashboard. With
Collab off, the Page tabs go away.

The frame is `<iframe sandbox="allow-scripts allow-forms allow-modals" allow=""
referrerpolicy="no-referrer" credentialless>`: no same-origin, pop-ups,
downloads or top-level navigation, and no browser permissions. Titles and
targets the agent wrote are shown as text. The page's URL, which carries its
token, goes only into the frame.

### The page token is the key

The stage admits a frame's `Origin: null` requests, because a sandboxed frame
has no other origin to send. So the 128-bit token in the page's host name is
what decides who reaches the page. The token goes only to the local dashboard
(its snapshot). It never goes to a linked instance's view, even an unrestricted
one, and not to any agent, including the one that showed the page. That agent
sees its page's origin with the token masked and acts on it by name. This keeps
the token from being handed out; it does not make it unlearnable. The page's
own code can read its host name and write it into what the agent reads back. An agent
holding the token could fetch its page itself, but what a static page serves
it is never read back unless the stage serves exactly what the agent's own
sandbox shows (see Page sources), so it gains nothing it could not read. Keeping the token from it also keeps it out of the agent's
transcript, and out of a clone's.

## Page sources

A page comes from one of two sources, and the stage reaches nothing else. It
never dials a host name or an IP, only the literal `127.0.0.1` at a port that is
not the daemon's own.

- **The agent's own port**, served by a dev server the agent runs (Vite, Next,
  Storybook, anything). The dev server must listen on `127.0.0.1`. A server
  bound only to `::1` (some `localhost` setups) is not found, so start it with
  `--host 127.0.0.1` or the equivalent.
  - *Sandboxed agent with a private network namespace (Linux):* the launch
    record says so, and the stage then connects only inside the agent's own
    namespace, never on the host. That loopback serves the agent's launch alone:
    the agent's processes plus the sandbox's own network helpers. The
    connection is made through util-linux `nsenter`, the same way the sandbox
    supervisor enters the namespace.
  - *Host-network agent:* every listener on `127.0.0.1:<port>` must belong to the
    agent's process tree (its pane, or the OpenCode server it runs under). The
    stage proves this from the kernel's socket tables (`/proc` on Linux, `lsof`
    on macOS) before every new connection. A port another program holds is
    refused, even when the agent shares it through `SO_REUSEPORT`.
  - *macOS:* Seatbelt gives no private network namespace, so a sandboxed agent's
    port is checked like a host-network agent's. An agent whose profile closes
    network access cannot listen on loopback at all, so it cannot serve a page
    from a port. It can still show a static file or directory.

  The stage presents `Host` and `Origin` as `localhost:<port>` to the dev server,
  so Vite's host and websocket checks pass. The dev server's websocket (HMR)
  passes through. When nothing answers, the page shows a plain reason, such as
  "nothing is listening on 127.0.0.1:5173".
- **A static directory or one HTML file in the agent's worktree.** It is served
  read-only. No symbolic link is ever followed, not even one that points back
  inside the worktree. Names that start with a dot (`.git`, `.env`) are refused,
  and only common web file types are served. A directory serves its
  `index.html`. A single-file page serves only that file. The target itself may
  not be a hidden path.

  How the files are reached depends on the agent's sandbox:

  - *tclaude's sandbox on Linux:* the page is served through the agent's own
    view of the file system (its mount namespace, through `/proc/<pid>/root`).
    What the sandbox hides, overlays or binds applies exactly as it does for
    the agent, even after the agent renames a directory. If that view cannot be
    opened, for example because the agent is not running, the page serves
    nothing (503) rather than the host's files.
  - *No OS sandbox* (`off`, `resource-only`, or a harness whose own sandbox is
    recorded off; for Claude Code, its launch-time verdict must say off, since
    a managed policy can keep its sandbox on): the agent reads what the daemon
    reads, so the page is served from the host. The launch is re-read, so a
    sandbox restored on the same conversation stops the read-back at once.
  - *Anything else* (a harness's own sandbox, macOS Seatbelt, or a launch with
    nothing recorded): the page is served from the host, leaving out by path
    what the recorded profile hides, with the same 404 as a missing file. The
    stage cannot see the harness's own rules, or match Seatbelt's exactly, so
    nothing of such a page is read back to the agent (see below).

  tclaude's protected directories are never served. The view is re-read when the
  agent reincarnates or comes back in a new sandbox, and the open views reload.
  The daemon watches the
  source and asks open views to reload when a file is saved. It finds saves by
  walking the source a few times a second with the same no-follow rules, not with
  inotify, so a symbolic link cannot point the watcher outside the worktree.
  Hard links are not served either.

## The stage: how a page is kept away from the dashboard

The page's code runs in the operator's browser. The stage listener makes sure it
never runs with the dashboard's origin, cookies or API.

- **Its own origin.** Each page is served at
  `http://f-<token>.ui.localhost:<dashboard port>/`, on the dashboard's port.
  Browsers resolve `*.localhost` to loopback without DNS. The origin is a
  different site from the dashboard's `127.0.0.1`, so the dashboard's session
  cookie (host-only, `SameSite=Strict`) is never sent to it.
- **A separate handler.** A request for any name under `ui.localhost` is split off
  before the dashboard mux. The stage has no route to `/api`, `/static` or any
  dashboard page. It only passes requests to the page's own source, so
  `/api/...` on a page origin reaches the page's dev server, never the dashboard.
  An unknown or closed token answers 404. The stage answers only connections
  that arrived on the loopback listener.
- **An unguessable token.** The `<token>` has 128 random bits. Closing a page
  forgets it, and showing the page again mints a new one.
- **Frames only.** Documents are served only to requests a browser marks
  `Sec-Fetch-Dest: iframe`. A top-level visit to the page origin is refused, and so
  is a request with no `Sec-Fetch-Dest`. The daemon's own snapshot renderer is the
  one exception: it presents a second, per-page snapshot token.
- **No credentials cross.** `Cookie`, `Authorization`, every `Proxy-*` header,
  forwarding headers and `Referer` are stripped from requests. `Set-Cookie` is
  stripped from responses.
- **A daemon-set CSP.** The stage sets the page's Content-Security-Policy itself
  and drops any the page's server sends, so a page cannot loosen it. It allows
  the page's own origin plus `data:` and `blob:`, inline scripts and `eval` (dev
  servers need them), and connections to the page's own origin only
  (`connect-src`). Only the dashboard may frame the page (`frame-ancestors`), and
  `sandbox` keeps the document sandboxed even outside the frame. Other hosts are
  allowed only by a per-page operator toggle, off by default.
- **Its own frame only.** A request whose `Origin` is neither `null` (the
  sandboxed frame) nor the page's own origin is refused, so another site that
  learned a token cannot drive the dev server.
- **Opaque origin and CORS.** In the sandboxed frame, the page's origin is opaque,
  so its own fetches and module scripts are CORS requests with `Origin: null`. The
  stage answers those, preflights included, with `Access-Control-Allow-Origin: null`
  and never with credentials. It drops any CORS headers the page's server sends.
- **The probe.** The stage inserts a small script into each HTML document it
  serves (`stage_probe.js`). It reports the frame's scroll, names the elements
  under a mark and answers read-only DOM reads, all over `postMessage` with the
  dashboard view. It is as untrusted as the page and holds no secret: the page's
  own scripts can read and forge what it sends. So the view reads only messages
  from its own frame's window, carrying the nonce it minted for the frame's
  current load, within size caps (64 KiB a message, 50 elements, 200-character
  snippets) and the schema, and shows them only as text. The daemon checks the
  same again. HTML that is compressed with anything but gzip cannot be
  inspected, so it is refused.

What a page cannot use in the frame: cookies, `localStorage` (until the probe
provides a stand-in), IndexedDB and service workers. An app behind a login needs
a development bypass or a fixture user.

Known limits of the boundary:

- A page can still navigate its own frame to any URL. CSP does not govern
  navigation, so a page can send data out in a GET or make blind GETs to other
  loopback services. Its fetches stay limited to its own origin unless the
  operator allows other hosts.
- The frame may open `alert`, `confirm` and `prompt` dialogs (`allow-modals`). A
  dialog comes from the page, not from tclaude. Never type the operator token
  into one.
- Under a harness's own sandbox, a static page may show the operator a file
  only the harness's rules hide (a `denyRead` in Claude Code's `settings.json`,
  say). It is not read back to the agent, but the page's own script runs with
  it. Put deny rules in a sandbox profile, or use tclaude's sandbox.
- Browsers resolve `*.localhost` to loopback themselves. A browser that hands
  these names to the system resolver instead trusts that resolver for them.

## Marks, snapshots and DOM reads for the agent

```bash
tclaude agent page marks <name> [--since N]       # the batches the operator sent
tclaude agent page snapshot <name> -o page.png    # the page as a PNG, with the marks
tclaude agent page dom <name> <selector> [--text|--html|--styles]
```

- **`page marks`** prints each batch: the note, the path, viewport and scroll,
  and each mark's kind, points in page pixels and the elements under it.
  `--json` gives the same as data. The inbox message names the batch to read
  from.
- **`page snapshot`** renders the page in a headless Chrome on the host, at the
  operator's last viewport and scroll, with the latest batch of marks drawn on
  top (`--seq N` for another, `--no-marks` for none). Chrome is the host's own:
  `google-chrome`, `chromium` or `chromium-browser` on `PATH`, the macOS app
  bundles, or `TCLAUDE_CHROME`. tclaude never downloads a browser; without one,
  the command says so. Chrome runs on a throwaway profile with its background
  networking off, and the daemon drives it over a pipe, not a debugging port.
  A snapshot renders the page's own origin and nothing else: every other request
  (a redirect, a navigation, a frame, an image) is refused inside Chrome, and a
  page that tries to leave its origin gets an error instead of a picture. The
  page's snapshot token goes only on the page's own document requests. A page
  the operator allowed to load other hosts is not rendered for the agent.
  One snapshot runs at a time per agent.
- **`page dom`** reads up to 10 elements matching a selector in the operator's
  open view of the page: their text, outer HTML or computed styles. It is
  read-only, and needs the page open in the dashboard (it is read through that
  view's frame). Otherwise it says no view is open. A large answer keeps the
  first elements that fit and says it was cut.
- **Static pages are read back when they are served exactly as the agent sees
  them:** under tclaude's sandbox on Linux, or with no OS sandbox. Otherwise
  `page snapshot` and `page dom` refuse the page, and its marks arrive without
  the page's own data: no text under them, no path, scroll or viewport, and
  points relative to the frame. The operator's
  marks and note still arrive. After the agent comes back in a new sandbox,
  `page dom` and the text under marks wait until the page has loaded again.
  Serve the page from your own dev server to use them otherwise.

There are no MCP tools for live pages; the CLI is the agent's surface.
