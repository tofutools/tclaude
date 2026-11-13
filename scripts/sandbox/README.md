# Dashboard sandbox

`scripts/sandbox/ui.sh` serves the real dashboard and daemon API, built from
the working tree, over seeded data under a throwaway HOME. Use it to look at a
dashboard change in a real browser before you open the PR.

```bash
scripts/sandbox/ui.sh up               # build, seed the full fixture, print the URL
scripts/sandbox/ui.sh up --port 7790   # a fixed port, on 127.0.0.1 only
scripts/sandbox/ui.sh up --foreground  # the same, serving until stopped (for agent show)
scripts/sandbox/ui.sh up --peer        # plus a live federation peer and hub
scripts/sandbox/ui.sh down             # stop it and delete its data
scripts/sandbox/ui.sh tour [--peer]    # the route tour on a fresh sandbox
scripts/sandbox/ui.sh smoke [--peer] [NAME]  # every Playwright test, or one file
```

The server is `TestSandboxServe` in `pkg/claude/agentd/sandbox_serve_test.go`:
a test binary over the flow-test world. Tmux is simulated, and HOME, TMPDIR and
the XDG dirs point into a `mktemp -d` directory, so nothing reaches
`~/.tclaude`, the real daemon or the real tmux server. The clock is live and the
API is writable: you are the logged-in operator, so creating and moving tasks,
drawing and sending messages all work and persist until `down`.

## Showing it to the operator

`tclaude agent show :PORT` (needs `self.page`) puts the sandbox in the
operator's dashboard, live. On a host-network agent the stage accepts only a
server in your own process tree, and a plain `up` detaches its server. Run
`ui.sh up --foreground --port N` through your harness's background run
instead, with its output redirected to a file. A bare `&` in a tool call's
short-lived shell leaves your process tree. The foreground run serves until
`ui.sh down`, or until the run is stopped. It works for every fixture but
`page`.

The stage sends no Referer and rewrites Origin. The sandbox, and only the
sandbox, reads a missing Referer as its own, so the dashboard loads there.
Requests answered 400 or above are logged to `sandbox-out/ui/server.log`.
When a sandbox stops, its log is kept as `sandbox-out/<name>-last.log`
(`ui-last.log` for `up`).

## What the `full` fixture shows

The docs fleet (the `fleet` docshots fixture) is the base:

- seven agents across five harnesses, in every state, with PRs and CI
- usage, costs and quota
- messages and a notification
- audit rows
- a sandbox profile, a group template and a process template

On top of it the full fixture adds the following. It creates them through the
production routes wherever one exists.

- **The webshop Task board:**
  - tasks in every column, including the two custom columns In review and QA
  - assigned and unassigned, blocked, done and archived tasks
  - multiple refs, including PR refs
  - comments, a file attachment, and drawings of a drawing board
  - an operator question with choices, and an operator task another task waits on
- **Drawing boards** with shapes on them.
- **Automations:** cron jobs (interval, cron expression, disabled), a standing order, a trigger and exports.
- **Processes:** a run of the release-train template, waiting on its human decision.
- **Access:** permission defaults, a per-agent grant and a time-bounded sudo grant.
- **Usage:** a spend limit.
- **Diagnostics:** log lines in Diagnostics → Logs.
- **Federation:** one trusted peer node, plus the Steward with a decision for you.
  It has no node behind it, so it shows as unreachable; `--peer` links a live
  one instead (below).

`ui.sh up --fixture NAME` serves a docshots fixture instead (`fleet`, `collab`,
`steward`, `learn`, `page`, `collab-fleet`, `collab-quiet`).

## A live federation peer (`--peer`)

Without `--peer` the sandbox is one node. Its trusted peer has nothing behind
it, so the map shows it unreachable and the hub-backed Fleet pages show their
empty states. `ui.sh up --peer` (and `tour --peer`, `smoke --peer`) links a
live one, the way `TestFleetJourney` runs two nodes and a hub:

- **A hub.** A `tclaude-hub` runs inside the sandbox server, on its own
  loopback port. The sandbox claims its admin, so the Hub page and hub runs
  work.
- **The peer `ci-runner` (`ci@runner`).** It is a second daemon:
  `TestSandboxPeerInstance`, a child process of the server, with its own
  throwaway HOME. It holds two groups, four agents and a task board on
  `ci-fleet`.
- **Trust.** The peer enrolls with the sandbox on the node profile
  `ci-runners`, so the two trust each other at the restricted level. The peer
  grants the sandbox roster, presence and task board reads on `ci-fleet`, the
  roster of `release-checks`, and its harness list.
- **A stranger.** `guest@laptop` is on the hub but not trusted, so Fleet
  config → Peers has an instance waiting to be trusted.

So the map shows a reachable peer card with its counts and both trust
directions. `/groups?node=<id>` is the peer's own Groups page, read through
the hub, and Profiles and Audit have rows. The peer's id and seeded names
land under `peer` in `sandbox-out/ui/fixture.json`. Spawns, models, sharing,
offers, remote jobs and moves stay empty: nothing has used them yet.

`--peer` adds about two seconds to the start. `down` stops all three: the
server stops the hub with itself, and the peer when its stdin closes. A peer
that is still running gets TERM like the server. The peer's log is kept as
`sandbox-out/<name>-peer-last.log`.

## What it cannot show

- **Terminals.** A live terminal needs a real tmux and pty, so the Collab view
  lists the simulated sessions but cannot attach to them.
- **Performance captures.** A CPU capture needs a busy live daemon.

## The smoke tests

`ui.sh smoke` runs every test in `smoke/tests/` against a fresh sandbox, and
`ui.sh smoke NAME` runs one file. Each run's results and screenshots land in
`sandbox-out/results-<run>/`.

- `tour`: the route tour, described below.
- `markdown-editor`: the Markdown editor in the task edit form and the New
  task form. It checks typing, the toolbar, and Preview and back with the
  height kept. It checks that dragging the corner still resizes the editor,
  and that no page errors occur.

## The route tour

`smoke/tests/tour.spec.mjs` loads every route the router knows:
`KNOWN_TABS` × `KNOWN_SUBTABS` from `js/nav-history-core.js`, plus deep links
into the seeded task, drawing board, process run and template. It fails on a
page error, a console error, a failed request, a page with almost no content,
or an empty state. A page added later is toured automatically. It fails until
the fixture fills that page or the tour's `ALLOW` list says why it stays empty.
Every allow entry carries its reason.

With `--peer` the tour also opens the map, Fleet config → Peers and
Harnesses, and the peer's Groups page and task. Each must show the peer's
seeded names. The allow entries a live peer makes wrong (`peer: false`) no
longer apply there: the map card's "Unreachable.", and the empty Peers,
Invites, Profiles and Audit pages. A restricted peer refuses the views it did
not grant, so its 403s are allowed, and only under `--peer`.

The tests use `@playwright/test` with the system Chrome (`channel: 'chrome'`),
so nothing is downloaded. Set `SANDBOX_CHROME` to use another binary. The
package is kept out of `go.mod` and runs on demand only. Under an agent sandbox
Chrome needs writable XDG dirs; `ui.sh` points them into the throwaway
directory. Screenshots of every route land in `sandbox-out/results-<run>/`.
