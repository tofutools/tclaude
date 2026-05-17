# Issue tracker for developing the tclaude repo

## What this is — and is NOT

This is a **workflow gap in the tclaude-dev agent group**, not a tclaude
product feature. **tclaude itself will not build an issue tracker** —
work-tracking semantics (tickets, states, sub-tasks, dependencies) are out of
tclaude's scope by design; tclaude orchestrates agents (lifecycle / comms /
permissions) and leaves work-tracking to an external engine.

Every project that uses tclaude picks **its own** issue tracker. Agents reach
whichever tracker a project chose over **MCP, a skill, or similar** — so the
tracker is pluggable and swappable, and tclaude stays tracker-agnostic.

This file is only about **one project's choice: the tclaude repo's own
development.** Building features for tclaude itself is getting messy without a
tracker — work-item state (in-dev / in-review / merge-ready / PO-approved /
back-in-dev), sub-tasks, and follow-ups are tracked informally in the PO's
conversation and relayed on request. The tclaude-dev group should adopt a
real tracker.

## Constraint — why not the GitHub-native options

`tofutools/tclaude` is a **public** repo, so:

- **GitHub Issues** — anyone can open an issue; if agents treat the tracker
  as a work queue, that is an external injection / poisoning / noise vector.
- **PR comments** — same: anyone can comment on a public PR.
- **GitHub Projects** — publicly viewable; a board is only a view over
  issues/PRs, so it inherits the same exposure.

PR **labels** are the exception — only repo-write-access users can set them —
so they remain a usable, non-poisonable partial signal (a queryable
merge-gate state).

## Likely direction

An **access-controlled tracker**, reached by agents via MCP / a skill so it
stays swappable:

- A **private** repo's GitHub Issues (issue creation gated to collaborators).
- An external tracker (Linear / Jira / private-repo Issues) via an MCP server.
- Not an agentd-local tracker — tclaude does not own work-tracking semantics.

## Open questions

- Which tracker for the tclaude repo specifically (private GH repo vs an
  external engine).
- The agent access path — an MCP server vs a `tclaude` skill vs a CLI wrapper.
- Trust model: who or what may create a ticket and transition its state.
- Cross-machine survival of tickets.

## Interim

The PO tracks work state informally and relays it on request. PR labels
(write-access-gated, non-poisonable) can serve as a cheap partial merge-gate
signal in the meantime.
