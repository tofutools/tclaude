# The Making of tclaude

*An origin story, reconstructed from the git log and the pile of
shipped-feature notes in `docs/plans/DONE/`. Roughly 437 commits and
two months span this tale.*

---

## Chapter 0 — A tool carved out of a bigger tool

On **7 March 2026** a commit landed with the unceremonious name
`Initial commit`. The *second* commit told the real story:

> Extract Claude Code extensions from GiGurra/tofu

tclaude wasn't born from nothing. It was carved out of a larger
personal toolbox — the genuinely useful Claude Code bits given their
own home: their own module path (`github.com/tofutools/tclaude`),
their own CI, their own GitHub Pages site. A few commits later came
badges, a supported-platforms table, and the quiet confidence of a
project that intends to stick around.

What it was on day one: a **utility belt**.

- `session` — wrap Claude Code in tmux so you can detach and reattach.
- `conv` — search, resume, copy, prune Claude's `.jsonl` conversations.
- `worktree` — parallel Claude sessions on parallel git branches.
- `stats`, `usage`, `statusbar` — know how much Claude you've used,
  and show it in your prompt.
- `web` — a browser terminal, back when Claude Code didn't have one.

Useful! Tidy! Finite. If the story ended here, tclaude would be a
perfectly respectable CLI that a few hundred people quietly love.

The story does not end here.

## Chapter 1 — The day one Claude wasn't enough

Somewhere in the spring, a question turned up that the utility belt
couldn't answer: *what if you want more than one Claude at once?*

Not "more than one session" — tclaude already had that. The question
was sharper. What if two Claude agents need to **talk to each other**?
Hand off work? Form a team with a name?

The answer shipped as **Agent Coordination v1** (PR #47). It is the
hinge the whole project swings on. Suddenly tclaude had:

- `tclaude agent` — a cross-session messaging CLI.
- **Groups** — named bags of agents that can find and message peers.
- An **inbox** — so a message survives until its recipient looks.
- A **daemon transport** — because agents come and go, but the
  postbox has to stay up.

tclaude had stopped being a belt of tools *you* wear. It had become
the thing standing between a roomful of Claudes, routing the mail.

## Chapter 2 — The daemon grows a spine

A message router is only as trustworthy as its idea of *who is
asking*. Tokens get copied, leaked, screenshotted. So `tclaude agentd`
— the HTTP-over-Unix-socket daemon — was given a stricter answer:
**identity comes from socket peer credentials**. The kernel tells the
daemon which process is on the other end of the pipe (`SO_PEERCRED`,
`LOCAL_PEERPID`). You can't forge that with a string.

With a daemon that *knows who's calling*, the lifecycle verbs got
interesting:

- **clone** — fork an agent into a sibling that inherits its identity,
  optionally its whole conversation history. The original keeps going.
- **reincarnate** — replace a context-bloated agent with a fresh one
  that inherits identity but starts clean. (It demands a follow-up
  task, so the new instance never wakes up idle and confused.)
- **self-lifecycle** — an agent that can `compact`, `clone`, or
  `reincarnate` *itself* when it feels its context window filling up.
  No human babysitting required.

A succession scheme had to be invented just so messages addressed to
"the agent formerly known as r-2" still found their way home. The
postbox now does *forwarding addresses*.

## Chapter 3 — Giving it a face

A daemon with no UI is a daemon only its author can love. So came the
**web dashboard** — a single-page app served on the same loopback
port as the approval popup, gated behind a peer-cred-minted init
token.

The dashboard did not arrive finished. It arrived *and then kept
arriving* — the `DONE/` folder holds something like seventeen
dashboard notes, each a small honest improvement:

- clickable column headers, an offline filter, a cwd column;
- drag-and-drop to **move** an agent between groups, drag-and-drop to
  **clone** one;
- a spawn modal that learned to tell "initial message" apart from
  "description";
- agents that report themselves *offline* instead of lying about
  being idle;
- a checkbox that pops a terminal already attached to your new agent.

This is what a tool looks like when someone actually uses it every
day: not big-bang rewrites, but a steady drip of "oh, that annoyed me,
fixed it."

## Chapter 4 — With great fleets come great responsibility

Once you can spawn a fleet of agents, a colder question arrives:
*what are they allowed to do?*

The **permissions framework** answered with a graduated trust model —
defaults in config, per-conversation overrides in SQLite, slugs like
`self.rename` and `agent.schedule` deciding who may do what to whom.

Then the **sudo-elevation** system — temporary, audited elevation of
an agent's powers. It is, by a wide margin, the most-iterated feature
in the whole repo: a v1, then dashboard API, dashboard UI, proactive
grants, config defaults, audit annotations, periodic cleanup, and an
*orange tray icon* so a human can see at a glance that someone,
somewhere, is currently running with the safety off.

The lesson encoded in those nine files: orchestrating power is easy;
orchestrating it *safely* is the actual project.

## Chapter 5 — Tests that act like a daemon

A system this twitchy — spawns, renames, reincarnations, message
forwarding — needed tests that exercise *coordination*, not just
functions. So **testharness v2** was built.

The trick is restraint. Only **two** boundaries are ever mocked:
`clcommon.Default` (the tmux command builder) and `agentd.Spawn` (the
session spawner). Everything else — the daemon, the HTTP mux, the conv
index, the SQLite — runs as the real production code. The `*_flow_test.go`
files drive whole stories through it: *spawn → /rename → resume*,
*reincarnate-of-r-N*, *clone alias derivation*, *delete cleanup*.

And there's a rule with a nice attitude to it: when a real-world
Claude or tmux quirk bites you in production, you don't just patch the
bug — you **teach the simulator the quirk**, so the flow test would
have caught it. Over time the simulators accumulate the institutional
memory of every surprise the project ever had.

## Where the story stands

As of the latest commit (**15 May 2026**):

- `docs/plans/TODO/high-prio/` is **empty**. Nothing is on fire.
- `med-prio/` holds the next dozen ideas — realtime dashboard push, an
  fsnotify-based monitor, cross-agent lifecycle, group links.
- `future/` keeps the daydreams: cross-machine agents, human-in-the-
  loop approval, and something called `agent-seance` that this
  document is too responsible to speculate about.

tclaude began as a belt of tools for working with **one** Claude. It
is now the daemon, the dashboard, the postbox, the permission desk and
the safety inspector for working with **a whole fleet** of them.

Silly and useful, exactly as planned.

---

*Want to add a chapter? Ship a feature, move its note into
`docs/plans/DONE/`, and the next storyteller will find it.*
