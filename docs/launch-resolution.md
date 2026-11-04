# Launch resolution

Every agent launch, whether a first spawn or a later resume, reincarnate,
clone or morph, is built from the same set of launch fields. This page lists
each field, where its value comes from, and what a relaunch does with it.

How a first spawn picks each value (explicit flag, named profile, group
default profile, global default profile, harness default) is described in
[Default resolution](spawning-and-lifecycle.md#default-resolution). This page
covers what happens to those values after the agent exists.

## The rules

**A spawn profile is copied at first spawn.** The named profile, the group's
default profile and the global default profile each contribute values to the
first launch. Those contributions are copied into the agent, and later edits
to the profile, or a change of a group's default profile, apply to new spawns
only. Running and stopped agents keep their copy.

Saving an edit to a profile in the dashboard says how many running agents
keep their copy. The profile editor's **Running agents** row lists them, with
what re-applying the edited profile would change for each:

- a field the edit changes and the agent took from its copy is marked
  **changes**;
- harness, model, effort and directory are **kept (fixed)**;
- a value the spawn request set is **kept (explicit)**;
- a field a higher tier sets is **kept (set by** that profile**)**: a launch
  takes each field from the highest tier that sets it, so editing the global
  default changes nothing where the agent's named profile sets the same field;
- spawn-only values, such as the agent name or the initial message, are
  **not reapplied**: they were used once, at the first spawn.
- roles and the model proxy are **not reapplied yet**: a re-apply does not
  carry them so far, and the agent keeps its copy. Roles will be re-applied
  together with their grants.

A re-apply also changes the values a first spawn stores outside the launch
settings:

- the **group context switch** and the **profile startup context**, which the
  agent is briefed with again after a compaction or `/clear`. Only the
  profile's own text is replaced; the role briefs after it stay;
- the **profile environment**: the profile's entries are recomputed, and the
  entries the spawn set explicitly stay on top of them;
- the **usage-profile links**: the agent gets the links the profile has now,
  unless its spawn named its own usage profiles. The links are read when you
  apply, so applying an unchanged profile picks up a link edit too.

A value the spawn request set explicitly is kept in each case.

Grant changes (permission overrides, group owner and role grants) are listed,
but a re-apply does not apply them yet.

tclaude records which spawn profile, and which of its roles, each birth
grant or deny came from, for agents spawned from now on. A grant has no such
record when it came from the spawn request, from a role the request named, an
inline profile, a group template or federation worker defaults, or when anyone
has changed it since: an operator, a `permissions` command, a policy generator
or a scope edit. A re-apply will only ever change grants that still carry the
profile's record. Sudo grants are kept apart and are never involved. Agents
spawned before this change have no records, so a re-apply will leave all of
their grants alone. Group ownership (`is_owner`) is not recorded this way yet.

**Apply to selected** re-applies the edited profile to the agents you tick,
one at a time, after a confirm that names the count. What happens depends on
the agent:

- a **stopped** agent gets the new copy; it takes effect on its next resume,
  reincarnation or clone;
- a **running, fully idle** agent is stopped, given the copy and resumed, as a
  restart does;
- a **busy** agent (working, or with background agents, shells or monitors
  running) is **busy, skipped**: nothing is stopped or changed. Apply again
  once it is idle.

Each agent's result shows in a **Result** column, and **Refresh** reads the
preview again. The values marked kept or not reapplied stay as they were: the
agent's copy keeps its old roles, grants and the other not-yet fields, so the
preview keeps listing them as differences. A copy whose launch posture
(sandbox, approval) the agent's spawner could not give it, or that would add
grants the person applying it could not confer, is refused with the reason; a
running agent refused that way is resumed with its old settings. When the
profile is a group or global default, re-applying it reaches every agent
spawned under that default, and the preview says so.

The preview and Apply need `profiles.manage`. An agent applying it also needs
`agent.morph` over each agent it re-applies to, cannot use an operator-only
profile, and cannot give an agent a launch posture it could not give a child
it spawned itself. A disabled profile cannot be re-applied. They are also available as
`GET /v1/spawn-profiles/{name}/running-agents` and
`POST /v1/spawn-profiles/{name}/reapply` with `{"agent_id": "..."}`; each apply
is its own audit entry. Retired agents are not listed.

**What an agent references is read again.** A spawn can name a sandbox
profile, a spawn profile can link usage profiles, and an agent can have roles.
Which ones is kept. Their contents are not: an edit to a sandbox profile or a
role takes effect on the agent's next relaunch, and usage rules are evaluated
live.

**Group and workspace settings are kept current.** The group's environment,
remote-control policy, startup context and mission, and the group or workspace
sandbox-profile assignment are read again on every relaunch. This holds for
agents spawned before this table existed too.

**Explicit values stay explicit.** A value given as a flag at spawn wins over
every profile and stays the agent's value on every relaunch. A flag given at
resume becomes the agent's new explicit value from then on.

**Harness, model, effort and directory are fixed.** They are chosen at first
spawn and only change by an explicit action: a morph, an explicit resume flag,
or moving the agent.

**A resume always works.** If a value that is read again is no longer valid
(a profile was deleted, or no longer suits the harness), the relaunch falls
back to the value stored at the last launch and says so.

**A relaunch says what changed.** A resume or reincarnation lists, under
"Changed since last launch", what its sandbox differs in from the one stored
at the last launch: which sandbox profiles apply and whether one was edited,
filesystem rules added or removed, environment and group environment
variables added, removed or changed, network access and rules, the
filesystem root, the harness-config posture, and whether temporary
filesystems, unix sockets, resource limits, pre-launch blocks or the macOS
flags changed. It names variables, never their values. Only a relaunch that
happened reports it. The dashboard's wake toast shows the list too, in error
style only when the relaunch fell back to the stored policy.

**Group permissions and owners are not launch inputs.** They apply to
members at once, without a relaunch.

## Relaunch policy

Five rows can be switched between *re-resolve* (read the current value, the
default) and *reuse* (keep the value stored at the last launch): group
environment, group remote-control policy, group startup context and mission,
sandbox profile contents, and role definitions.

The harness-config posture has three settings, because an explicit
`--harness-config` choice can disagree with the posture the re-read sandbox
profiles give:

- *explicit wins* (the default) keeps the explicit posture, as every other
  explicit flag is kept. It is chosen for least surprise, not maximum
  security.
- *tighten-only* is the security option. It differs from explicit wins in
  exactly one case: an agent spawned with `--harness-config write` whose
  sandbox profiles keep the read-only floor gets the floor back on its next
  relaunch. An explicit `read` pin is already the strictest posture, so it
  always stays.
- *chain wins* always takes the profiles' posture, so a `read` pin over
  profiles that allow `write` is lost on the next relaunch.

For an agent with a recorded spawn request, the explicit posture is not
forgotten while another setting overrides it: switching back to explicit wins
restores it on the agent's next relaunch. A clone carries its source's record.
Agents from before launch layers were recorded, and agents created by the
pending-spawn sweeper or a group template, have no such record, so a relaunch
under chain wins drops their explicit posture for good.
The harness-config repair (see [Sandboxing](sandboxing.md)) also follows the
setting: it does not list an agent whose posture the setting gave up on
purpose.

The answer is one global setting per row. Every other row follows its layer
and has no setting. Show and change it with:

```bash
tclaude agent relaunch-policy show
tclaude agent relaunch-policy set harness_config tighten-only
```

`show` prints each row's mode, its default and why. Reading is open; `set`
needs `relaunch-policy.manage`, which is not granted by default. Treat it like
`sandbox-profiles.manage`: one answer can widen what a relaunch gives every
agent. A change takes effect on each agent's next resume, reincarnate or
clone. The API is `GET` and `PUT /v1/relaunch-policy` (`{"field", "mode"}`).
The five re-resolve or reuse rows are listed but cannot be set yet: no
relaunch reads them, so `set` refuses rather than store an answer that would
do nothing.

## Fields

"Spawn flag" says whether a spawn request can set the field explicitly.

<!-- launch-fields:begin -->

### Fixed at first spawn

| Field | Key | On relaunch | Spawn flag | Why |
|---|---|---|---|---|
| Harness | `harness` | kept | yes | The conversation belongs to one harness; switching it is a morph. |
| Model | `model` | kept | yes | Kept as launched; a morph or an explicit resume flag changes it. |
| Reasoning effort | `effort` | kept | yes | Kept as launched; a morph or an explicit resume flag changes it. |
| Directory and worktree | `directory` | kept | yes | The harness keys the conversation to its directory; only an explicit move changes it. |

### Copied from the spawn profile at first spawn

| Field | Key | On relaunch | Spawn flag | Why |
|---|---|---|---|---|
| Spawn profiles used (named, group default, global default) | `spawn_profile` | kept (copy) | yes | A spawn profile is copied at spawn; editing it, or changing a group's default, affects new spawns only. |
| Model proxy | `model_proxy` | kept (copy) | yes | Part of the spawn profile copy. |
| Sandbox mode | `sandbox_mode` | kept (copy) | yes | Part of the spawn profile copy. |
| Sandbox implementation | `sandbox_implementation` | kept (copy) | yes | Part of the spawn profile copy; the host must still support it. |
| Approval policy | `approval` | kept (copy) | yes | Part of the spawn profile copy. |
| Tool governance | `tools` | kept (copy) | yes | Part of the spawn profile copy. |
| Ask-user timeout | `ask_timeout` | kept (copy) | yes | Part of the spawn profile copy. |
| Auto-compact window and context window cap | `context_window` | kept (copy) | yes | Part of the spawn profile copy. |
| Context features (trims) | `context_features` | kept (copy) | yes | Part of the spawn profile copy; features the harness lacks are dropped. |
| Harness drives (Copilot API, Codex app-server, pi drive) | `harness_drives` | kept (copy) | yes | Part of the spawn profile copy; only harnesses with that drive accept it. |
| Fast mode | `fast_mode` | kept (copy) | yes | Part of the spawn profile copy. |
| Auto-review | `auto_review` | kept (copy) | yes | Part of the spawn profile copy. |
| Remote control (profile default) | `remote_control` | kept (copy) | yes | Part of the spawn profile copy; the group policy can still force it. |
| Auto memory | `auto_memory` | kept (copy) | yes | Part of the spawn profile copy. |
| Peer messaging | `peer_messaging` | kept (copy) | yes | Part of the spawn profile copy. |
| Include group startup context | `include_group_context` | kept (copy) | yes | Part of the spawn profile copy; the group context itself is a group setting. |
| SSH workaround | `ssh_workaround` | kept (copy) | yes | Part of the spawn profile copy. |
| Profile and explicit environment | `profile_env` | kept (copy) | yes | Part of the spawn profile copy; explicit entries stay explicit. |
| Profile startup context | `profile_startup_context` | kept (copy) | no | Part of the spawn profile copy; re-injection after compaction uses it. |
| Roles | `roles` | kept (copy) | yes | Which roles an agent has is part of the copy; their text is a referenced definition. |
| Sandbox profile named at spawn | `explicit_sandbox_profile` | kept (copy) | yes | An explicit choice stays; the named profile's contents are re-read. |
| Usage profiles named by the spawn or linked from its spawn profile | `usage_profile_links` | kept (copy) | yes | Which profiles is part of the copy; their rules are read live. |

### Used by the first spawn only

| Field | Key | On relaunch | Spawn flag | Why |
|---|---|---|---|---|
| Agent name and description | `identity` | not reapplied | yes | Set at spawn; a rename acts on the agent itself. |
| Initial message, saved prompts, attachments | `initial_brief` | not reapplied | yes | Delivered once, as the opening brief. |
| Task link | `task_link` | not reapplied | yes | Set at spawn; later changes act on the agent itself. |
| Directory trust seeding | `trust_dir` | not reapplied | yes | Seeded once for the launch directory, which does not move. |
| Worktree sync and fetch | `worktree_setup` | not reapplied | no | Prepares the worktree before the first launch. |
| Auto-focus | `focus` | not reapplied | yes | Where the new agent is shown when it starts. |
| Group owner and permission overrides | `access` | not reapplied | yes | Granted to the agent at spawn; after that they are the agent's own and edited directly. |
| One-shot run, timeouts and queueing | `run_mode` | not reapplied | yes | Shape of the spawn call itself. |
| Unenforced-sandbox and break-glass acknowledgements | `launch_acknowledgements` | not reapplied | yes | A human's consent for one fresh spawn; never replayed. |

### Group and workspace settings

| Field | Key | On relaunch | Spawn flag | Why |
|---|---|---|---|---|
| Group environment | `group_env` | re-resolved, configurable | no | Group settings are kept current, for agents spawned before this table too (operator decision). |
| Group remote-control policy | `group_remote_control` | re-resolved, configurable | no | Group settings are kept current; still clamped to what the harness supports. |
| Group startup context and mission | `group_startup_context` | re-resolved, configurable | no | Group settings are kept current; re-injection after compaction reads them live. |
| Group and workspace sandbox-profile assignment | `group_sandbox_assignment` | re-resolved | no | Which profile a group or the workspace assigns is a setting, kept current. |

### Contents of referenced profiles and roles

| Field | Key | On relaunch | Spawn flag | Why |
|---|---|---|---|---|
| Sandbox profile contents | `sandbox_profile_contents` | re-read, configurable | no | Edits to a sandbox profile take effect on the next relaunch (operator decision). |
| Role definitions (text) | `role_contents` | re-read, configurable | no | A role is a named entry in the shared registry; edits to it take effect on relaunch and re-injection. |
| Harness config posture | `harness_config` | configurable: explicit-wins (default), tighten-only, chain-wins | yes | Default chosen for least surprise, not maximum security: an explicit posture stays like any explicit flag. Tighten-only, where a narrower sandbox-profile posture always applies, is the security option. |
| Usage profile rules | `usage_profile_contents` | re-read | no | Usage rules are evaluated live, every few minutes. |

### Live, not a launch input

| Field | Key | On relaunch | Spawn flag | Why |
|---|---|---|---|---|
| Group and workspace usage-profile assignment | `usage_assignment` | live | no | Usage rules are evaluated live against the current assignments. |
| Group permissions and owners | `group_access` | live | no | Applied to members at once; no relaunch needed. |
| Member cap, notifications, re-injection after compaction | `group_runtime` | live | no | Read when used; not part of a launch. |

<!-- launch-fields:end -->

## Where the code still differs

The rules above are the specification. These rows do not follow it yet:

<!-- launch-gaps:begin -->

- **Model** (`model`): No resume path takes a model flag yet; only a morph changes it.
- **Reasoning effort** (`effort`): No resume path takes an effort flag yet; only a morph changes it.
- **Model proxy** (`model_proxy`): Reincarnate and clone drop the model proxy. A re-applied spawn copy does not change it yet; the agent keeps the value from its first spawn.
- **Roles** (`roles`): A re-applied spawn copy does not change it yet; the agent keeps the value from its first spawn.
- **Group environment** (`group_env`): Watch mode and conv resume reuse the stored group environment.
- **Group remote-control policy** (`group_remote_control`): Resume reuses the remote-control setting stored at the last launch.
- **Group and workspace sandbox-profile assignment** (`group_sandbox_assignment`): Watch mode and conv resume reuse the stored sandbox policy.
- **Sandbox profile contents** (`sandbox_profile_contents`): Watch mode and conv resume reuse the stored contents.
- **Role definitions (text)** (`role_contents`): Re-injection after compaction still uses the role text resolved at spawn.
- **Harness config posture** (`harness_config`): Watch mode and conv resume keep the stored posture, as does a resume that falls back to the stored policy when the profiles cannot be read at all; the setting applies on the agent's next ordinary resume.

<!-- launch-gaps:end -->
