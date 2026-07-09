---
name: agent-circles
description: >-
  Author and edit tclaude group templates (a.k.a. summoning circles / task
  forces) by talking to a scribe agent, via `tclaude agent templates`. Covers
  the template JSON wire shape, the safe show-json → edit → edit-file → show
  round-trip, which permission slugs a scribe needs (templates.manage, and
  roles.manage / profiles.manage only when touching the shared registries), and
  the wizard-mode vocabulary (circle/party/rite/quest/drumbeats) a human may
  speak. Use when asked to create, edit, inspect, rename, delete, snapshot,
  export/import, or reason about a group template / summoning circle — or when a
  human describes a team blueprint in wizard words.
---

# Editing summoning circles (group templates)

A **group template** is a reusable blueprint for a whole working group: a name,
a shared context, an ordered roster of agent specs, and optional choreography
(work pattern, process, rhythms, staged-spawn waves). It holds **no conv-ids** —
it describes a team that does not exist yet. *Instantiating* / *deploying* one
creates a fresh group and spawns one agent per roster spec.

You edit templates through `tclaude agent templates …`, a thin client over the
`tclaude agentd` daemon (the human starts it; you don't). Everything here also
drives the dashboard's Templates tab — same endpoints, same JSON.

> **Wizard mode.** A human may speak in Dungeons-&-Dragons costume: a template
> is a *summoning circle*, a deployed group is a *hero party*, and so on. That
> vocabulary is **display-layer only** (`wizWord` in the dashboard JS) — the DB,
> the wire JSON, and every CLI verb use the plain words. Translate the human's
> wizard words back to the plain nouns before you act; see the table at the end.

## Permissions: what a scribe agent needs

Reads are **open** — `ls`, `show`, `export`, and `starters ls/show` need no
grant, so discovery always works. Mutations are gated:

| You want to… | Verb(s) | Slug required |
|---|---|---|
| Create / edit / delete a template, snapshot a group, import one | `create` `edit` `rm` `from-group` `import` | `templates.manage` |
| Spawn a whole team from a template | `instantiate` `deploy` | `templates.instantiate` |
| Also create/edit roles the template references | `roles create/edit/rm` | `roles.manage` |
| Also create/edit spawn profiles the template references | `profiles create/edit/rm` | `profiles.manage` |

A **scribe** — an agent whose job is authoring circles by chat — needs
`templates.manage`. Grant `roles.manage` / `profiles.manage` **only** when the
edit must also touch those shared registries (a template merely *referencing* an
existing role/profile by name needs neither — just `templates.manage`). Grant
`templates.instantiate` only if the scribe should also be able to spawn teams;
it is strictly more powerful (a whole team at once) and is usually left to the
human.

If a mutation is refused you'll get a 403 naming the missing slug — ask the
human to grant it (`tclaude agent permissions grant <conv> templates.manage`,
itself human-only), then retry. Don't loop on a refusal. (The template verbs
have no `--ask-human` one-shot-approval flag; that escalation lives on the
lifecycle/spawn/message verbs, not here.)

## The safe edit loop

`edit` is a **full replace**, not a field merge — you must post the template's
*complete desired state*. So never hand-write a partial body; always start from
the current JSON, mutate it, and send the whole thing back:

```bash
tclaude agent templates show my-circle --json > circle.json   # 1. dump current state
$EDITOR circle.json                                           # 2. mutate the JSON
tclaude agent templates edit my-circle --file circle.json     # 3. full-replace
tclaude agent templates show my-circle                        # 4. verify (human view)
```

`show --json` emits exactly the shape `create`/`edit` accept via `--file`
(`--file -` reads stdin), so the round-trip is lossless. To rename a template,
change the `name` field in the body and `edit` under the old name — the CLI
reports the rename. To create from scratch, write the JSON and
`templates create --file circle.json`.

**Cautions**
- Full replace: any field you drop from the JSON is dropped from the template.
  Re-`show --json` first so you're editing live state, not a stale copy.
- Per-agent `permissions` are validated against the slug registry — an unknown
  slug is rejected with the list of known slugs. See them with
  `tclaude agent permissions slugs`.
- `role_ref` / `spawn_profile` are validated to **exist** at save; a dangling
  reference is rejected and the error lists the known names.
- Validation errors are actionable: each names the offending field and says
  what's allowed. Read the error, fix that field, re-send.

## The template JSON wire shape

Top level (`templateJSON`):

| Field | Type | Notes |
|---|---|---|
| `name` | string, **required** | Group-name rules: non-empty, no slashes, no control chars, no leading/trailing space. It's the route key and the prefix of every spawned agent's name (`<name>-<agent>`). |
| `descr` | string | One-line description. |
| `default_context` | string | Shared context folded into every spawned agent's briefing. CRLF-normalised, capped at 16 KiB. |
| `agents` | array | The roster — see below. Order is the spawn order (within a wave). |
| `work_pattern` | array | Ordered routed briefings, delivered once after the roster spawns. |
| `process` | array | Advisory phase plan (tracked, never enforced). |
| `rhythms` | array | Recurring nudges → group cron jobs at deploy. |
| `wave_max_wait` | int (seconds) | Cap on how long each staged-spawn wave waits for the prior wave to go idle. `0` = built-in default. |
| `created_at` / `updated_at` | string | **Response-only** — ignored on input. |

Each roster agent (`templateAgentJSON`):

| Field | Type | Notes |
|---|---|---|
| `name` | string, **required** | Non-empty, no slashes, no control chars, unique in the template, not the reserved `"all"`. |
| `role` | string | Free-text display label (e.g. `"product-owner"`). Not the role-library reference — that's `role_ref`. |
| `descr` | string | One-line description of the agent. |
| `initial_message` | string | The agent's task brief. ≤ 16384 bytes; newlines and tabs allowed, other control chars not. |
| `is_owner` | bool | Marks this agent a group owner (may be several). |
| `permissions` | array of slug strings | Per-agent grants; each validated against the slug registry. |
| `role_ref` | string | By-name reference into the role library; the agent inherits that role's defaults beneath its own fields. Must exist at save. |
| `spawn_profile` | string | By-name reference to a saved spawn profile (launch shape + birth-time permissions/owner). Must exist at save. |
| `profile_inline` | object | A template-LOCAL spawn profile: the spawn-profile shape (`harness`/`model`/`effort`/`sandbox`/`approval`/`ask_user_question_timeout`/`auto_review`/`trust_dir`/`remote_control`/`is_owner`/`permission_overrides`) embedded in the template — a bespoke per-agent launch config with no registry entry, carried along on export/import. No `name`, no identity fields (`agent_name`/`role`/`descr`/`initial_message` — those live on the roster agent), no dialog-only toggles (`sync_worktree`/`auto_focus`/`include_group_default_context`); all rejected at save. Sits between the legacy inline fields and `spawn_profile` in launch resolution. |
| `harness` `model` `effort` `sandbox` `approval` | string | Legacy inline launch overrides that win over every profile tier. Validated against the resolved harness catalog. Leave blank to inherit — prefer `profile_inline` for new configs. |
| `wave` | int (0–64) | Staged-spawn wave. All-zero (default) = one synchronous spawn pass; higher waves spawn later, in ascending order. |

`work_pattern` entry (`workPatternEntryJSON`):
- `send_to` — a roster agent's `name`, or `"all"` (broadcast to every member).
- `value` — the message; may contain `{{task}}`, replaced with the
  per-instantiation task at delivery. Same ≤16384-byte charset rule as a brief.

`process` phase (`processPhaseJSON`):
- `name` — phase handle, unique case-insensitively.
- `roles` — role labels active in the phase (matched case-insensitively against
  a member's `role`; `"all"` = everyone).
- `criteria` — free prose (entry / exit / handoff in words; no DSL).

`rhythm` entry (`rhythmJSON`):
- `name` — unique case-insensitively; becomes the `<group>-<name>` cron handle.
- `target_role` — filter to matching members; `""` or `"all"` = whole group.
- `interval` **xor** `cron_expr` — exactly one. `interval` is a Go duration
  (`"10m"`, `"1h"`, `"30s"`), must be ≥ 30s. `cron_expr` is a cron expression.
- `subject` — optional.
- `body` — **required**, the message the nudge sends.

A **minimal** template is just `name` + `agents` (each with a `name` and usually
an `initial_message`). Everything else is optional advanced choreography — add
it only when asked. Example minimal body:

```json
{
  "name": "feature-team",
  "descr": "a PO and two devs",
  "default_context": "Use worktrees and open PRs.",
  "agents": [
    { "name": "PO",   "role": "product-owner", "is_owner": true,
      "initial_message": "Coordinate the team.", "permissions": ["groups.spawn"] },
    { "name": "dev1", "role": "dev", "initial_message": "Build feature A." },
    { "name": "dev2", "role": "dev", "initial_message": "Build feature B." }
  ]
}
```

## Other verbs

```bash
tclaude agent templates ls                                   # list (open)
tclaude agent templates from-group <group> <template-name>   # snapshot a live group (templates.manage)
tclaude agent templates export <name> [--file f.json]        # portable envelope (open)
tclaude agent templates import --file f.json [--as N|--update]  # read one back (templates.manage)
tclaude agent templates starters ls|show|install <name>      # bundled ready-to-run templates
tclaude agent templates instantiate <name> --group <g> [--task T]   # spawn a team (templates.instantiate)
```

`from-group` bootstraps a template from a running group but leaves per-agent
briefs blank (a live group stores none) — fill them in with `edit`. `export`
embeds the full definitions of every role and spawn profile the template
references, so the file is portable; `import` re-creates only the ones missing
locally (never overwriting a same-named local one).

## Wizard-mode ↔ plain vocabulary

The human may speak wizard; the system never does. Translate before acting:

| Wizard word | Plain word (DB / wire / CLI) |
|---|---|
| summoning circle | template |
| hero party / party (deployed) | group / task force |
| familiar | agent |
| rite of command | work pattern |
| quest plan (chapters) | process (phases) |
| drumbeats | rhythms |
| summon | deploy |
| cast (a circle) | instantiate |
| conjure a preset party | install a starter |
| chalk / inscribe a circle | create a template |

So "revise the drumbeats on my summoning circle" = edit the `rhythms` on that
template; "cast the circle into a party" = `instantiate`. Always emit the plain
JSON field names — the wire shape has no wizard keys.

## Installing this skill

The agent skills (this one, `agent-coord`, `agent-rename`, …) are bundled into
the `tclaude` binary and materialised into the harness skill directories with:

```bash
tclaude setup --install-agent-skills
```

Idempotent — re-running overwrites the local copies with whatever the current
binary embeds.
