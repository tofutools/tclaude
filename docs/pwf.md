# Portable Workflow Format (PWF)

The Portable Workflow Format is a versioned file format for sharing a
[process](processes.md): a workflow of agent, human and command steps. A
`.pwf` file holds a process template together with every stored template it
references through subprocess steps, so a workflow arrives complete. It is a
sibling of the [Portable Agent Format](paf.md): the same ZIP container rules,
content hashes and Ed25519 signatures, with its own manifest.

This page is both the user guide and the specification.

**Format version:** PWF 0.1. While the major version is 0 the format is still
settling, and a new 0.y version may change it incompatibly, as with PAF.

## Why

A good process takes a while to get right: which steps, in what order, who
performs each, what the reviewer checks and when a human signs off. Sharing it
as a file lets a team, or anyone, reuse that work. A shared workflow is also
untrusted input that can contain commands, so the format makes it easy to see
exactly what a workflow would do before you take it in.

## Sharing a workflow

Export a stored template. Its subprocess templates come with it:

```bash
tclaude agent process-templates export ship-feature            # writes ship-feature.pwf
tclaude agent process-templates export ship-feature -o - > x.pwf
tclaude agent process-templates export ship-feature --version sha256:<hex> --title "Ship a feature"
```

Exporting an older version (`--version`) makes that version the current one
wherever the file is imported. The file is signed with this node's federation
key when the node has one.
Exporting never creates a key. A step that names one specific agent (an
`assignee`) is exported as written, but it is not portable: the importing node
has different agents, so export points those steps out and they wait there
until someone reassigns them.

Check a file without a daemon and without changing anything:

```bash
tclaude agent process-templates inspect ship-feature.pwf
```

`inspect` verifies the file and prints what the workflow needs: its inputs,
the agent roles its steps ask for, the steps that ask you something, every
command it would run, and the steps that name a specific agent.

Import it on another node:

```bash
tclaude agent process-templates import ship-feature.pwf          # review only
tclaude agent process-templates import ship-feature.pwf --yes    # import
```

Without `--yes`, import stops at the review. An import applies exactly what
was reviewed: if a template changes here between the review and the import,
the import is refused and you review again. The review adds what only the
receiving node knows: whether the signing key is this node, a known federation
peer or unknown, and what importing would change for each template:

| Status | Meaning |
|---|---|
| `new` | Not stored here; importing stores it (as the current version when it is a head). |
| `identical` | This exact version is already here; nothing changes. |
| `update` | The template exists here with different content; importing makes the imported version current. Needs `--replace`, or `--rename` to import it under a new id instead. |
| `version` | A pinned older version a subprocess step refers to; stored without changing the current version. |

Three choices change what an import stores. They apply to the review and the
import alike, so the review shows their effect and the import applies exactly
that:

```bash
tclaude agent process-templates import ship-feature.pwf --map-role reviewer=qa --yes
tclaude agent process-templates import ship-feature.pwf --rename --yes
tclaude agent process-templates import ship-feature.pwf --group webshop --yes
```

- `--map-role <workflow role>=<local role>` (repeatable): the review lists
  every role name the agent steps use (the role a step asks for, and the
  profile its fallback spawns) and flags the ones no member of a group you can
  see holds and no spawn profile here is named. A mapped role is rewritten in
  every imported template that uses it. Unmapped roles keep their names.
- `--rename`: a template whose id this node already uses with different
  content is imported as `<id>-2` (the first free number) instead of
  replacing yours. References to it inside the workflow follow, so a template
  that refers to a renamed one changes too and may be renamed in turn.
- `--group <name>`: pins the imported process to the group's saved
  processes (you must be the operator, or an agent in or owning that group).
  It also stays in the library.

Mapping and renaming rewrite templates, so the stored versions have new refs;
the review lists each one as `<ref> as <new ref>`. A subprocess step pinned to
a rewritten version is re-pinned to the rewritten one. In the dashboard, the
import dialog offers the same choices.

Importing stores templates only. It never starts a run, and starting one later
asks you to authorize each program profile its command steps use, as for any
run (see [Processes](processes.md#running)). Export needs
`process.templates.read`; import needs `process.templates.manage`.

### Before you share

- Read the commands `inspect` lists. They run on the importing node with that
  node's authorizations.
- Template text (prompts, questions, descriptions) travels as written. Keep
  secrets, internal hostnames and customer details out of it.
- Prefer roles over specific agents, so the workflow runs anywhere.

### Before you import

- Check the signer. An unknown key proves only that the file is unchanged
  since it was signed, not who signed it.
- Read every command and its program profile. You authorize them, per run,
  when a run starts.
- An `update` replaces your current version of a template you already have,
  unless you import it under a new id with `--rename`.
- Map roles the review flags, or those steps wait for a role nobody has.

## File format

### Container

A PWF file is a ZIP archive (extension `.pwf`, media type
`application/vnd.tclaude.pwf+zip`). It follows PAF's
[container rules](paf.md#container) exactly: `manifest.json` at the root,
every other entry except `signatures/*` listed in the manifest with its byte
length and SHA-256, safe relative entry names, regular files only, bounded
sizes, duplicate JSON keys refused, and local headers that agree with the
central directory. Entries outside the layout below are refused. A workflow
is a few small text files, so readers bound it far below PAF: tclaude accepts
at most 32 MiB of uncompressed content and 1,000 entries, and 1 MiB per entry.

### Layout

```text
manifest.json                   format, version, root ref, entry hashes
workflow.json                   title, summary, templates, and what the workflow needs
README.md                       optional human documentation
templates/<id>/<semantic-hash>  one process template source (YAML or JSON) per version carried
signatures/<key-id>.json        detached signatures over manifest.json (optional)
```

A template is addressed by its ref, `<id>@sha256:<semantic-hash>`, the same
identity tclaude's template store and subprocess `version` pins use. The
semantic hash ignores layout and formatting, so the same workflow exported
twice has the same refs.

### manifest.json

Schema: [`manifest.schema.json`](pwf/schema/v0.1/manifest.schema.json).

```json
{
  "format": "pwf",
  "pwf_version": "0.1",
  "kind": "workflow",
  "created_at": "2026-10-15T18:00:00Z",
  "producer": {"name": "tclaude", "version": "v0.0.2300"},
  "requires": [],
  "root": "ship-feature@sha256:…",
  "entries": [
    {"path": "workflow.json", "bytes": 1234, "sha256": "…", "media_type": "application/json"}
  ]
}
```

`requires` lists features a reader must understand; PWF 0.1 defines none, and
a 0.1 reader refuses a file that requires any.

### workflow.json

Schema: [`workflow.schema.json`](pwf/schema/v0.1/workflow.schema.json).

`title` and `summary` are free text from the exporter, without control
characters (the summary may contain newlines and tabs). Everything else is
derived from the templates, and a reader recomputes it and refuses a file
whose `workflow.json` disagrees. That makes it safe to show a review from
`workflow.json` alone.

- `root`: the entry template's ref. It is listed first in `templates`.
- `templates`: every template carried, as `{ref, id, name, head}`. `head`
  marks the version an unpinned subprocess reference to that id resolves to;
  each id has at most one head.
- `params`: the root template's inputs.
- `roles`: the agent roles steps ask for (an agent performer's `profile`),
  each with the steps that use it.
- `programs`: every command step, as `{step, profile, run, args}`.
- `assignees`: steps that name one specific agent.
- `humans`: steps a person performs.
- `loops`: decision outcomes that send work back for another round, as
  `{step, outcome, to, maxIterations, onExhausted}`. Omitted when the
  templates have none.

A step is named `<template-id>:<node-id>`, with a nested subprocess node's id
namespaced as the runtime does, and a compound task's stages suffixed
(`/plan`, `/check1`, `/review`).

### Templates and completeness

Every `templates/` entry must be listed in `workflow.json`, parse as a valid
process template, and hash to the ref it is stored under. Every subprocess
step in every carried template must resolve inside the file: a pinned
reference (`templateRef` plus `version`) to that exact ref, an unpinned one to
the id's head. A file that references a template it does not carry is
refused.

### Signatures

Signature files have PAF's [signature format](paf.md#signatures). The signed
message is the ASCII string `pwf-manifest-signature-v1\n` followed by the
lowercase hex SHA-256 of `manifest.json`. The prefix differs from PAF's, so a
PAF signature never verifies a PWF file, or the reverse. Trust is decided by
the public key alone; an unsigned file is valid but unverified.

## Versioning and compatibility

PWF follows PAF's [compatibility policy](paf.md#versioning-and-compatibility)
while it is 0.x. A reader refuses a version it does not support with a message
that names both versions.

## Related pages

- [Processes](processes.md): authoring and running workflows.
- [Portable Agent Format](paf.md): the agent sibling this format builds on.
