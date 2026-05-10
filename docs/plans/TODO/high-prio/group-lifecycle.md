# Group lifecycle (spawn / stop / resume entire teams)

A **group is a persistent team** the human (or a trusted agent) can
spawn on demand, suspend, and resume. Load-bearing UX for "delegate
this batch of work to a code-reviewer + test-runner + integration-
runner team, then come back later."

Membership table exists. Operations that act on members in bulk are
the missing piece.

## Shipped

- `groups stop` (soft `/exit`, `--force` kill-session) — slug
  `groups.stop`.
- `groups resume` (spawn detached `tclaude session new -r <conv>
  -d --global` for offline members) — slug `groups.resume`.
- `groups archive` / `groups unarchive` (schema v16) — soft-delete a
  group; default-hidden listings; mutating ops refused with 409.
- `tclaude agent spawn <group>` — fresh CC + auto-join (slug
  `groups.spawn`, default human-only).
- `tclaude --join-group <group>` — top-level + `session new` flag
  reusing the spawn endpoint, foreground attach.

## Open

### `groups create --team <template>` bootstrap

Bootstrap a group + initial members in one call. Template is JSON or
a flag bundle:

```
tclaude agent groups create reviewer-team \
  --member alias=lead,role=tech-lead,descr="...",cwd=. \
  --member alias=tester,role=test-runner,descr="..."
```

Each member starts in the `no-conv-yet` placeholder state until
`groups spawn` is called. Or could spawn-and-join in the same
`--team` flag.

### Phase B: groups spawn handles no-conv-yet placeholders

Today `groups spawn` only handles "members with no conv yet" if it
can derive that state — but the model has been "spawn always creates
a fresh conv-id". Open: do we let the human pre-fill `member.role`
/ `member.descr` and pass them as a bootstrap prompt the spawning
agent receives on first turn?

### Permission slugs to add

All gated by default — consistent with the existing `groups.*` /
`member.*` model:

- `agent.spawn` — generic "spawn a fresh CC session by some
  identifier (not tied to a group)" slug. Today an agent can call
  `tclaude session new` directly (it doesn't route through the
  daemon), so there's nothing for the daemon to gate yet. Routing
  `session new` through the daemon would make this enforceable —
  bigger refactor, deferred. (See also
  `agent-self-service-permissions.md`.)

### Open questions

- **Member-row survival across spawn cycles.** If we want conv-id
  stability (so `permissions grant <conv> ...` keeps working across
  spawns), we have to track a "logical member id" separately from
  the conv-id, or accept that re-spawning a no-conv-yet member
  produces a brand-new conv. Probably the latter: members are
  templates; conv-ids are runtime state.
- **Stop semantics.** Should `stop` be reversible (`resume` brings
  the same conv-ids back) or "kill and recreate"? Reversible is
  much nicer for the human ("suspend this team for an hour");
  recreate is simpler.
- **Where do we store team templates?** If `--member alias=...,role=...`
  flags get cumbersome, a `~/.tclaude/teams/<name>.toml` directory
  would feel natural — same shape as docker-compose / k8s manifests.
- **Bootstrap prompts.** The message a freshly-spawned member sees
  as its first `[system: ...]` nudge needs a home. Probably a per-
  member optional `bootstrap_prompt` column that gets injected on
  first `agent.spawn`.

### Recommended UX progression

1. Manage teams from the CLI: `groups create --team`, `groups
   spawn`, `groups stop`. Reads like docker-compose for agents.
2. Eventually do the same from the dashboard (one-click spawn /
   stop a team, pending-approval queue inline).
3. Grant a *coordinator agent* `groups.spawn`/`groups.stop` so it
   can manage subordinate teams without bothering the human (with
   `--ask-human` as the off-ramp for one-off escalations).

## Files
- `pkg/claude/agent/groups.go` — CLI
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn`
- `pkg/claude/agentd/handlers.go` — group POST endpoint
