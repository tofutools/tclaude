# Per-group export / import — phase 1

Ships the ability to export one agent group to a single portable `.zip`
archive and import it back — on the same machine or a different one — to
recreate the group, its agents, their permissions and their full
conversation history. The use cases: checking a group into source
control, moving it to another worker machine, and backups. Per-group
(not whole-DB) is deliberate: the DB holds many groups and some data is
sensitive, so the human transfers one group at a time.

Follow-up shipped after phase 1: the dashboard import surface — the
**⤒ import** button, an upload modal, and a dry-run preview that shows
the manifest summary + collision report before anything is written.
Import is no longer CLI-only. See the Dashboard section.

## CLI surface

- `tclaude agent groups export <name> [-o <file>]` — download the group
  as a `.zip`. Default filename `group-<name>-<timestamp>.zip` in the
  cwd; `-o -` streams to stdout.
- `tclaude agent groups import <file> --into <dir> [--as <name>]
  [--dry-run]` — recreate the group from an archive. `--into` (required
  unless `--dry-run`) is the local working directory the imported agents
  are bound to. `--as` imports under a different group name. `--dry-run`
  inspects the archive and prints what *would* be imported — manifest
  summary plus group-name and conv-id collisions — without writing
  anything.
- `tclaude agent groups transfers [--limit N] [--json]` — the
  export/import audit log.

## Dashboard

The Groups page carries two top-bar controls and a per-group button:

- Per group: an **⤓ export** button (`GET /api/groups/{name}/export`,
  cookie-authed) downloads the `.zip`.
- Top-right, next to **🧹 clean up**: an **⤒ import** button opens the
  import modal — a `<input type="file">` picker for the `.zip`, an
  "Into dir" text field (the `--into` value; a browser cannot browse the
  server filesystem) and an optional "Import as" field (`--as`).
- The moment a `.zip` is picked the modal POSTs it to the dry-run
  endpoint and renders a **preview panel**: manifest summary (source
  group, agent/message counts, source machine, format version) plus a
  collision report — whether the group name is already taken here, and
  which conv-ids will be remapped to `-i-N` copies. The **Import** button
  stays disabled until the preview is clean; a malformed / corrupt /
  unsupported-version archive shows its error in the preview and blocks
  the confirm outright. On a failed commit the modal surfaces that the
  transactional import wrote nothing.

## Daemon endpoints

- `GET  /v1/groups/{name}/export` — slug `groups.export` (human-only).
- `POST /v1/groups/import?into=<path>&as=<name>` — slug `groups.import`
  (human-only). Request body is the raw `.zip`.
- `POST /v1/groups/import/inspect?as=<name>` — slug `groups.import`. Raw
  `.zip` body; returns the dry-run analysis (manifest summary + collision
  report) and writes nothing.
- `GET  /v1/groups/transfers` — read-only, open to any caller.
- `GET  /api/groups/{name}/export` — dashboard export.
- `POST /api/groups/import` — dashboard import; a `multipart/form-data`
  upload (an `archive` file part + `into` / `as` form fields), since a
  browser cannot stream a raw body with query params.
- `POST /api/groups/import/inspect` — dashboard dry-run; same multipart
  upload shape.

The dashboard import / inspect routes wrap the cookie-authed request
with `asDashboardHumanPeer` and call the **same** permission-checked
`handleGroupImport` / `handleGroupImportInspect` the `/v1` routes use, so
the `groups.import` slug is structurally enforced on every path (the
fix-pattern established for the export route in commit `6a1ade5`). A
single `readImportUpload` helper transparently handles both the raw-body
(CLI) and multipart (dashboard) request shapes; uploads are capped at
512 MiB so a realistically large conversation-bearing archive is
accepted.

## On-disk format — a zip archive

A group export is a single `.zip` (no base64 anywhere):

- `manifest.json` at the root — one combined JSON document holding every
  DB row plus the format metadata (`format_version`, `source_home`,
  `source_os`, `exported_at`, `schema_version`, and a per-conv
  `source_cwd`).
- `projects/<conv-id>.jsonl` — each agent's conversation as a real,
  deflate-compressed file entry (flat by conv-id, a unique UUID).

The format is versioned: `FormatVersion = 1`. An archive whose manifest
declares a newer version is **refused** on import — the forward-compat
guard the source-control use case relies on.

The serialization is isolated behind `pkg/claude/common/groupexport`
(`model.go` = the format-agnostic in-memory `Export`, `container.go` =
the zip marshal/unmarshal). Swapping the container is a localized change.

Accepted tradeoff: a zip is opaque in source control (no line diffs).
The compactness (deflate — conversations get large) and single-artifact
portability win for the backup / machine-move use cases.

## Schema enumeration — what is carried

Every table keyed to the group or to a member's conv-id is exported:

- Group-scoped: `agent_groups`, `agent_group_members`,
  `agent_group_owners`, `agent_group_audit`, `agent_cron_jobs`,
  `agent_cron_runs`, `agent_messages`.
- Conv-scoped (per member): `agent_permissions` (grant **and** deny —
  the `effect` column), `agent_enrollment`, `agent_workdir`,
  `agent_sudo_grants`, `agent_head_aliases`, `agent_conv_succession`,
  `agent_spawn_history`, `agent_clone_history`.
- Each member's conversation `.jsonl`.

Deliberately **not** carried, with reasons:

- `conv_index`, `conv_embeddings` — machine-specific path caches, rebuilt
  from the `.jsonl` on scan. The importer re-runs that scan.
- `sessions`, `notify_state` — live tmux/pid runtime; meaningless at
  rest. Imported agents land **offline** by design.
- `usage_cache`, `git_cache`, `schema_version` — DB-global, not
  group-scoped.
- `agent_group_links` — a link references *two* groups; a single-group
  export structurally cannot carry the peer group. Deferred to a future
  multi-group export.

## Conv-id collisions — remap on import

Conv-ids are preserved when they do not collide. When an imported
conv-id already exists locally (an enrolled agent, a `conv_index` row, a
group membership, or a `.jsonl` on disk), it is **remapped** to a freshly
minted UUID and the agent's title is suffixed `-i-N` (the import sibling
of the `-r-N` reincarnate / `-c-N` clone conventions) so the copy is
distinguishable. The remap is applied across every conv-referencing
column — members, owners, permissions, enrollment, workdir, sudo grants,
head aliases, succession, spawn/clone history, and `agent_messages` from
**and** to — plus the embedded `sessionId` inside the `.jsonl`.

A **group-name** collision is *not* auto-resolved: a group name is a
human-meaningful identity, so the import is refused with an error telling
the human to pass `--as`. Conv-ids are mechanical; names are not.

## Cross-OS / cross-user portability

A group exported on Linux as user A imports cleanly on macOS as user B.
No source-absolute path is baked into an importable DB field — every
path column (`agent_workdir.dir`, `agent_groups.default_cwd`, …) is set
to the `--into` target. The only source paths recorded are `source_home`
and the per-conv `source_cwd`, kept so the importer can rewrite paths
embedded inside the `.jsonl` content: a boundary-anchored prefix rewrite
(`source_cwd → --into`, then `source_home → local home`; cwd first as
the more specific prefix). The boundary anchor stops `/home/A` from
corrupting `/home/Alice`.

Known limitation (documented at the rewrite site in
`agentd/groups_export.go`): the `.jsonl`-internal path rewrite is a POSIX
prefix substitution. Linux↔macOS is fully handled. A Windows↔POSIX
transfer does not get its `.jsonl`-internal backslash paths translated —
the structural import (DB rows, projects-dir placement) still works
cross-OS regardless, because those always use the local encoding.

## Atomicity

Import is all-or-nothing. The transformed `.jsonl` files are written to a
staging directory; then `db.ImportGroup` runs every row insert **plus**
the audit-log row in one transaction. Only after that commits are the
staged files moved into `~/.claude/projects/`. A failure before or
during the transaction wipes the staging directory and leaves the system
exactly as it was — no group, no rows, no files, no log entry.

## Audit log

Migration **v40** adds `agent_transfer_log` — every export and import is
recorded (kind, time, format version, source group + machine bases,
result group + target dir, the conv-id remaps applied, agent/message
counts). Import rows are written inside the import transaction, so a
rolled-back import logs nothing. Surfaced via `groups transfers`.

## Source files

- `pkg/claude/common/groupexport/{model,container}.go` — format + zip.
- `pkg/claude/common/db/group_export.go` — `CollectGroupExport`,
  `ImportGroup` (the transaction).
- `pkg/claude/common/db/transfer_log.go` + `migrate.go` (v40).
- `pkg/claude/agentd/groups_export.go` — handlers, `.jsonl` I/O,
  conv-id remap, path rewrite, staging. `readImportUpload`
  (raw-body / multipart), `inspectGroupImport` (dry-run analysis),
  `handleGroupImport` / `handleGroupImportInspect` (shared,
  permission-checked, behind both `/v1` and `/api`).
- `pkg/claude/agentd/dashboard_edit.go` — `handleDashboardGroupImport`
  / `handleDashboardGroupImportInspect` and their `/api/groups/import`
  route registration.
- `pkg/claude/agentd/dashboard.html` — the **⤒ import** button, the
  import modal (file picker + into / as fields), the dry-run preview
  panel + collision report, and the confirm flow.
- `pkg/claude/agent/groups_export.go` — the CLI subcommands, including
  `import --dry-run`.
- Tests: `groupexport/container_test.go`, `db/migrate_v40_test.go`,
  `agentd/groups_export_flow_test.go` (round-trip, same-machine re-import
  remap, name-collision refusal, malformed/unsupported-version
  rejection, cross-home path rewrite, failed-import-leaves-nothing),
  `agentd/dashboard_group_import_flow_test.go` (dashboard upload
  recreates the group; dry-run reports group-name + conv-id collisions
  without writing; malformed upload rejected at preview and commit).

## Deferred — explicit follow-ups

Out of scope for phase 1; recorded here, not built:

- **Merge-on-import / overwrite-existing semantics.** Phase 1 refuses a
  group-name collision and remaps conv-id collisions; it never merges
  into or overwrites an existing group.
- **Recreating live sessions on import.** Imported agents land offline;
  the human wakes them with the normal resume tooling.
- **Whole-DB or multi-group export**, and the cross-group
  `agent_group_links` table that belongs with it.
- **Selective / partial export** within a group.
- **Encryption of the export file.** The archive holds full conversation
  content and is sensitive; phase 1 does not encrypt it.
- **Windows↔POSIX `.jsonl`-internal path rewriting** (see Known
  limitation above).
