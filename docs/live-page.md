# Live pages

Live pages are part of [Collab](companion.md#turning-collab-off). They let an agent show
the operator any web page it serves, live and interactive inside the dashboard.
tclaude does not build the page. It renders what the agent serves, in a
sandboxed frame.

!!! note "In progress"
    So far only the daemon's stage listener exists: the part that serves a page on
    its own origin. The `tclaude agent show` command, the dashboard tab, drawing and
    live patches come in later releases.

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
- **Opaque origin and CORS.** In the sandboxed frame, the page's origin is opaque,
  so its own fetches and module scripts are CORS requests with `Origin: null`. The
  stage answers those, preflights included, with `Access-Control-Allow-Origin: null`
  and never with credentials. It drops any CORS headers the page's server sends.
- **The probe.** The stage inserts a small script into each HTML document it
  serves. Later releases use it for marks and live patches. It is as untrusted as
  the page and holds no secret. HTML that is compressed with anything but gzip
  cannot be inspected, so it is refused.

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
- Browsers resolve `*.localhost` to loopback themselves. A browser that hands
  these names to the system resolver instead trusts that resolver for them.
