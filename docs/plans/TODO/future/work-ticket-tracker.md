# Work ticket tracker for the tclaude-dev agent group

## What's open

The tclaude-dev multi-agent group (human + PO + worker agents) has no
structured work-state tracker. Work-item state — in-dev / in-review /
merge-ready / PO-approved / back-in-dev — is tracked informally in the PO's
conversation and relayed to the human on request. The human wants a real
ticket system: dynamic tickets with sub-tasks / checkboxes, explicit states a
ticket moves between, updated independently by the PO and by worker agents.

## Why not the obvious GitHub-native options

Evaluated 2026-05-17 and rejected for now:

- **GitHub Issues** — `tofutools/tclaude` is a *public* repo, so anyone can
  open an issue. If agents treat a ticket tracker as a work queue, public
  issue creation is an injection / poisoning / noise vector — an external
  party could file a "ticket" an agent then picks up and acts on.
- **PR comments** — same problem: anyone can comment on a public PR.
- **GitHub Projects** — publicly viewable; and a Project board is only a view
  over issues/PRs, so it inherits the same writability exposure.

The disqualifier is **public writability** (and visibility): the tracker must
not be a surface arbitrary external parties can write to, or inject through.

Note: PR **labels** are *not* poisonable — only repo-write-access users can
set them — so they remain a possible non-poisonable partial measure (a
queryable merge-gate state). Worth reconsidering if a fuller tracker stalls.

## Likely direction

A **separate, access-controlled ticket tracker** — not public-GitHub-native.
Candidates to weigh when this is picked up:

- A **private** repo's GitHub Issues (issue creation gated to collaborators).
- An external tracker (Linear / Jira / private-repo GitHub Issues) reached by
  agents via **MCP** — consistent with tclaude's design stance that
  work-tracking semantics belong in an external engine, not in tclaude itself.
- An agentd-local tracker — least preferred: tclaude's scope is to
  orchestrate agents (lifecycle / comms / permissions), explicitly NOT to own
  work-tracking semantics, so building a tracker into the daemon cuts against
  the project's own scope decision.

## Open questions

- Private GH repo vs a dedicated external tracker (Linear / Jira) vs other.
- How agents read/write it — an MCP server, a CLI wrapper, or a `tclaude`
  skill.
- Trust model: who or what may create a ticket and transition its state.
- Whether tickets need to survive across machines (a cross-machine concern).

## Interim

The PO tracks work state informally and relays it on request. PR labels
(write-access-gated, non-poisonable) could be adopted as a cheap partial
merge-gate signal in the meantime if desired.
