# Portable Agent Format (PAF)

The Portable Agent Format is an open, versioned file format for the state of a
coding agent. A `.paf` file holds what an agent needs to resume work: its
conversation history in a harness-neutral form, its definition and identity,
and the environment it ran in. It can also hold a second copy of the history
in the original harness's own native format, so the same harness can resume it
without any loss. You can resume that agent on another machine or in another
harness, possibly long after the original harness or model has gone.

This page is both the specification and the user guide. It covers why the
format exists, how tclaude uses it, how to share agent state safely, and the
exact file layout. Every PAF wire structure also has a JSON Schema
under [`docs/paf/schema/v0.1/`](https://github.com/tofutools/tclaude/tree/main/docs/paf/schema/v0.1), and [`docs/paf/examples/`](https://github.com/tofutools/tclaude/tree/main/docs/paf/examples)
holds complete example archives in unpacked form: zip a directory's contents
to get a `.paf` file. The examples are hashed and signed byte for byte; their
signature uses a published example key (an Ed25519 seed of 32 `0x01` bytes)
that must never be trusted for real content.

**Format version:** PAF 0.1. While the major version is 0 the format is
still settling, and a new 0.y version may change it incompatibly; see
[Versioning and compatibility](#versioning-and-compatibility).

## Background

A running agent is more than a prompt. A useful coding agent has built up
context: the task, what it has tried, tool output it has read, decisions the
operator made with it, and a working directory in a particular state. Losing
that context means re-explaining the work. Keeping it locked inside one vendor's
native log means it can only ever continue in that vendor's tool.

tclaude already moves running agents around: morph switches model or
harness in place, clone forks a sibling, move and teleport send an agent to
another node, and recall brings it home. Each of these grew its own
carrier: the [agent bundle](federation.md#portable-local-agent-bundles)
(native history only), the [group export](agents-and-groups.md) archive, the
neutral transcript used by
[cross-harness translation](harnesses.md#cross-harness-history-translation),
and the morph artifact directory. PAF unifies them behind one documented format
that:

- **is harness-neutral by default.** A PAF carries the conversation in a
  neutral form that any harness can be fed, and the native log only as an
  optional extra for a lossless same-harness resume;
- **survives its harness.** A reader with no harness installed can still open,
  verify and read an archived agent;
- **says what it lost.** Every translation records a loss report: counts and
  categories, never content;
- **is private by default.** Scanning, redaction, consent and recipient
  encryption are part of the format, not bolted on afterwards;
- **is verifiable.** Every entry is hashed in the manifest, and the manifest is
  signed by the producing instance.

PAF is about *running state*. Static persona or instruction files, such as
Agent Skills `SKILL.md`, Claude Code subagent files or registry formats like
Agent Shelf, describe an agent before it starts. PAF carries those only as a
definition inside the larger state, and an importer can map such a file onto a
PAF definition.

## Concepts

- **Agent PAF:** one agent: definition, provenance, environment and context.
- **Group PAF:** a group's configuration plus several agent PAFs that share one
  manifest and signature.
- **Snapshot:** a named, point-in-time PAF of an agent. A snapshot is a
  *fork point*, not the agent itself. Instantiating it creates a new agent with
  a new identity and a lineage link back to the snapshot. Instantiating it N
  times creates N independent agents.
- **Context:** the effective model context, meaning what the model actually
  sees on its next turn. That is the active branch from the last compaction on,
  with the compaction summary standing in for what came before.
- **Full history:** optionally, every readable entry including pre-compaction
  turns and abandoned branches, for archival and audit.
- **Native blob:** the harness's own history file, byte for byte, for a
  lossless resume in the same harness.
- **Landing:** turning a PAF back into a running agent (or a readable archive)
  on the receiving side.

## Identity and authority

A PAF records who an agent *was*. It never grants who the agent *is* on the
receiving side.

- **Imports fork.** Importing or instantiating a PAF always creates a fresh
  agent with a new `agt_…` id and a lineage entry pointing back at the source.
  The only way an agent can retake its original identity is a fenced
  recovery after host loss, which also prevents the old host from coming back
  as a second copy.
- **Authority is advisory.** Groups, roles, permissions and ownerships in a PAF
  are provenance. They never become grants on the receiver unless that
  receiver's own policy approves them explicitly. Ownership never travels.
- **Posture is decided by the receiver.** The recorded sandbox and approval
  posture is shown at import, but the landing profile and the receiver's
  policy decide how the new agent actually runs. A received history is
  untrusted input: it may contain instructions written by anyone whose text
  reached the original agent.

## How tclaude uses PAF

| Operation | What PAF carries | Carrier in this release |
|---|---|---|
| **Export / import** of an agent | One agent PAF file. Import previews first, then lands the agent on a harness you choose. | PAF with [`tclaude paf export` / `import`](#exporting-and-importing-an-agent); `agent bundle export` still writes [agent bundle v1](federation.md#portable-local-agent-bundles) by default |
| **Group export / import** | A group PAF: group configuration plus every member. | Group export v5 by default; a group PAF with [`groups export --format paf`](#exporting-and-importing-a-group) |
| **Snapshots** | A named PAF in a local library. Instantiate one or many agents from it, or share it. | Not available yet |
| **Morph** (model or harness change in place) | The neutral context, losses and full transcript the switch produced. The identity stays in place, so no new agent is created. | Morph artifact directory |
| **Clone** | A local sibling. Clone copies native history directly and shares PAF's history model. | Native copy (no file) |
| **Move, teleport, recall home** | The agent's state in transit to another node, with provenance hops. Peers that do not advertise PAF receive the legacy agent bundle. | PAF to peers that advertise it, otherwise agent bundle v1 |
| **Sharing** with a peer or on a [Board](federation.md#content-board-membership) | An encrypted PAF addressed to named recipients, published only by a human. | Unencrypted PAF to peers that advertise it, otherwise agent bundle v1; Boards carry configuration and skills only |
| **Publishing** | A redacted, pseudonymous, signed PAF file for public sharing. tclaude provides no hosting; you choose where the file goes. | Not available yet |
| **Resilience** | Opt-in periodic encrypted snapshots to another node or Board, from which an agent can be recovered if its host is lost. | Not available yet |
| **Archival** | Any PAF can be inspected, verified and rendered to readable text without the original harness or model. | [`tclaude paf`](#reading-a-paf) |

The last column says what each operation uses in this release. tclaude keeps
importing every legacy carrier after an operation switches to PAF; see
[Legacy formats](#legacy-formats).

## Landing a PAF

A consumer lands a PAF on the first rung that works and records which rung it
used. The rungs run from most faithful to most robust:

1. **Native.** The PAF carries a native blob in a format the target harness
   accepts, and that harness is available. The native history is installed
   through that harness's own import path, which only remints the
   conversation id and remaps working-directory metadata, and resumed.
   Message and tool content is not changed, so nothing is lost.
2. **Translated.** The neutral context is encoded into the target harness's
   native history, budgeted to the target model's context window, as in
   [cross-harness translation](harnesses.md#cross-harness-history-translation).
   This works for any harness that supports transcripts, including when the
   original harness or model is unavailable. The loss report says what did
   not carry over, such as provider-bound reasoning or older tool output, which
   is reduced to stubs that point at the full transcript.
3. **Context file.** The agent starts a fresh conversation whose first turn
   points it at the carried history saved as files (a budgeted briefing, the
   full transcript and any handoff). This works for any harness that can read
   files.
4. **Archive (read-only).** No harness runs. The PAF is verified and rendered
   as Markdown or HTML, including the definition, environment, loss report and
   transcript, for a person or another tool to read. This rung needs nothing
   but a PAF reader.

Rung 4 is the format's durability guarantee: **every valid PAF can always be
opened and read**, even with no harness, model or network. Rungs 1 to 3
depend on harnesses that exist at import time; rung 4 does not.

When the original harness or model is unavailable, the importer must name a
landing harness and model explicitly (or a spawn profile). The preview shows
the original environment next to the landing one, the expected losses and the
security posture before anything starts.

## Exporting and importing an agent

```bash
tclaude paf export reviewer --file reviewer.paf --history
tclaude paf import --file reviewer.paf --cwd "$PWD"                      # preview
tclaude paf import --file reviewer.paf --cwd "$PWD" --group team --apply
tclaude paf import --file reviewer.paf --cwd "$PWD" --harness codex --model gpt-5.5
```

`paf export` is `agent bundle export --format paf`, with the same
permission (`agent.bundle.export`), credential scan and `--allow-flagged`.
With `--history` the conversation travels twice: as the native history and
as the harness-neutral context, when tclaude can decode that harness.
`--no-context` leaves the neutral copy out. `--no-native` leaves the native
history out, so the conversation travels only in neutral form.

`paf import` is `agent bundle import`, with the same permission
(`agent.bundle.import`), preview, `--apply`, `--group`, path remapping and
permission handling. It accepts PAF files and legacy agent bundles. The
import is a fork: the new agent gets a new identity, and the source's
permissions and ownership are advisory only.

The agent lands on its own harness unless `--harness` names another. When
the source harness is not available here, the import says so and asks for
`--harness`. `--model` picks the landing model. Without it, the landing
model is the profile's, or on the same harness the model the source last
ran. A translated conversation is budgeted to that model's context window;
when no model is known at all, the preview asks for `--model` rather than
guess. The preview's
`history_landing` shows the source and landing harness and model and the rung
the conversation takes:

- **native** when the PAF carries a native history in the landing harness's
  format;
- **translated** otherwise: the neutral context, or the native history
  decoded by tclaude's reader for its harness, is translated for the landing
  harness and budgeted to the landing model's context window, with the
  expected losses;
- **none** when the conversation cannot land, for example on a harness
  without history import, or when it does not fit the context budget even
  after trimming. The agent then imports as configuration only, with a
  warning; `tclaude paf render` still reads the whole conversation.

On another harness the source's launch posture (approval, sandbox and the
other profile settings) is not applied; the receiver's defaults and normal
spawn checks apply, and the preview says so. Imports through federation
offers, moves and teleports keep landing on the source harness.

## Exporting and importing a group

```bash
tclaude agent groups export team --format paf --out team.paf
tclaude agent groups import team.paf --dry-run
tclaude agent groups import team.paf --into "$PWD" --as team-copy
```

`groups export --format paf` writes the same data as the default `.zip` as a
group PAF. `group.json` holds the group's name, description and members with
their roles. Each member is an agent with its Claude Code history as the
native blob. tclaude's own group state (settings, owners, permissions,
enrollment, cron jobs, messages and audit trail) travels in the `tclaude`
extension. The export is signed when this node has a federation identity.
The permission (`groups.export`) is the same as for the zip.

`groups import` and the dashboard import accept either file, with the same
permission (`groups.import`), preview, `--into`, `--as` and collision
handling. A group PAF is verified before anything is written. A group PAF
tclaude wrote imports exactly like its zip. One written by another tool
imports the group's name, description and members, each with its Claude Code
history. Members in other harnesses are refused for now.

`tclaude paf inspect`, `validate` and `render` also read group export zips,
converting them on read.

## Reading a PAF

`tclaude paf` verifies and reads a PAF on the local machine. It needs no
daemon, harness, model or network, and never changes the file. Every
subcommand also accepts a legacy agent bundle, which it converts to PAF in
memory first.

```bash
tclaude paf validate reviewer.paf          # exit 1 when invalid
tclaude paf inspect reviewer.paf           # what it holds, without the conversation
tclaude paf inspect reviewer.paf --json
tclaude paf render reviewer.paf > reviewer.md
tclaude paf render reviewer.paf --out reviewer.html
```

- **`validate`** runs every check a reader makes: ZIP layout, manifest,
  schemas, entry hashes and sizes, semantic rules and signatures. A file
  that requires a feature or extension this tclaude does not understand is
  valid for reading, with a warning, because an import would refuse it.
- **`inspect`** shows the format, producer, signatures, extensions and privacy
  findings, then each agent's harness, model, working directory, sandbox,
  lineage, native history, neutral context and recorded losses.
- **`render`** writes a Markdown or HTML document with the same facts, the
  definition's text and the whole conversation. This is landing rung 4. It
  shows the full context when the file has one, then the effective context,
  and otherwise decodes the native history with tclaude's built-in reader for
  that harness, which does not need the harness installed. `--scope` picks a
  context. The HTML is one self-contained file: it loads nothing, runs no
  script, escapes all archive text and embeds only PNG, JPEG, GIF and WebP
  images. In Markdown, raw HTML in the archive's prose is escaped and lines
  that would start a heading are escaped, so the text cannot load anything in
  a previewer or pose as another entry. Tool input and output stay in code
  fences. Terminal output from `validate` and `inspect` replaces control
  characters, so archive text cannot move the cursor or rewrite a line.

For each valid signature, tclaude says who holds the key: **this node**, a
**known peer** by name, or an **unknown key**. It compares the public key
itself, never the `key_id` the file claims. The signature does not cover
`key_id` or `signer`, so tclaude prints them quoted, as claims, after the
verdict, and treats a signature whose `key_id` is not a plain identifier as
invalid. A file whose only signatures use algorithms tclaude does not
support counts as unsigned. A valid signature proves the file is unchanged
since that key signed it; whether to trust the signer is your decision.

## Sharing and publishing

A transcript is the most sensitive thing tclaude handles. It can hold:

- tool output: environment dumps, `.env` reads, tokens in `curl` commands;
- private source code and customer data;
- internal URLs;
- operator messages;
- paths that reveal user and project names;
- opaque provider reasoning blobs.

Treat every PAF that has context as confidential unless you have deliberately
prepared it for a wider audience.

### Defaults

- **Scan on export and on import.** tclaude scans the definition, environment,
  context, native blob and text attachments for suspected credentials. Findings
  are counts and locations, never the matched text. Export refuses findings
  unless you explicitly allow them. Import scans again and shows the findings
  in the preview.
- **No silent rewriting.** History is carried verbatim unless you ask for
  redaction.
- **Redaction is opt-in, previewed and recorded.** The available redactions
  are:
  - masking flagged spans;
  - stubbing old tool results;
  - dropping media, provider reasoning or the native blob;
  - replacing paths and usernames with placeholders;
  - stripping identity.

  The preview shows what will change. The applied redactions are recorded in
  [`privacy.json`](#privacy) and the loss report, so a receiver knows the
  history is incomplete.

  A redaction applies to **every copy** of the content it touches: the
  context, the full history, text attachments, the definition and the
  environment. A copy that cannot be rewritten safely is dropped instead and
  the drop is recorded. tclaude never edits a native blob, so any redaction
  of conversation content or paths also drops the native blob
  (`drop_native`), and the PAF then lands through the translated rung.
  `paths_to_placeholders` records placeholder names without their `original`
  values.
- **Consent before it leaves the node.** Sharing with a peer, posting to a
  Board or publishing requires an explicit human confirmation. The confirmation
  shows size, time range, turn count, findings and redactions, and is recorded
  against the content digest. An agent can never share or publish history on
  its own authority.
- **Encrypted when stored off-node.** A PAF with context that is stored
  anywhere other than its own node (another node, a Board, a backup target)
  is [encrypted](#encryption) to named recipients.
- **Retention.** A PAF can carry an expiry (`retention.expires_at`), which a
  receiving tclaude honours for the copies it keeps. Shared snapshots default
  to a short retention. Copies that recipients already fetched cannot be
  recalled, and an expiry cannot force another implementation to delete its
  copy.

### Checklist before sharing an agent

1. Ask whether the receiver needs the history at all. A definition-only PAF
   (no context) shares a configured agent without its past.
2. Read the scan findings. A finding usually means a real token is in the
   transcript. Rotate the credential if it has already left your machine.
3. Consider the redactions: stub old tool results, drop media, and replace paths
   with placeholders.
4. Share with named recipients rather than a whole Board when you can.
5. Remember that the receiver sees what the agent saw, including private code
   and anything the operator typed.

### Publishing

Publishing produces a PAF meant for anyone to read. Beyond the sharing
defaults, a published PAF:

- requires the scan, the redaction preview and an explicit consent step;
- applies `strip_identity`: it removes the agent and instance ids, the
  conversation title, conversation ids (transcript headers,
  `native.source_conv_id`, `lineage[].from`), consent `by` names and media
  `path` hints, and signs with a pseudonymous publisher key;
- replaces absolute paths with placeholders without their original values;
- carries no native blob, group memberships or mail state;
- carries extension data only for extensions the publishing tool understands
  and has applied every redaction above to, including identity, paths and
  stubbed content. All other extension data is dropped, whatever its `copy`
  flag;
- is signed, so readers can tell whether two published PAFs come from the same
  publisher.

The output is a file. tclaude runs no public index or registry; publish it
wherever you publish other artifacts, with a licence of your choosing (see
`definition.license`).

### Receiving a PAF

- Verify the signature and check who signed it. `tclaude paf validate` shows
  whether the signer is this node, a known peer or unknown.
- Land it under a conservative profile. The recorded posture is a hint, not a
  requirement.
- Read the loss report and the redaction record. A redacted or translated
  history is incomplete by design.
- Do not expect the working directory to come with it. A PAF records the
  repository, branch, commit and dirty flag as hints. No repository or
  worktree is copied.

## Design rationale

- **Neutral context first, native optional.** Native logs are lossless but
  only readable by one harness, and they change shape between releases. The
  neutral transcript is lossy but stable and harness-independent. Carrying
  both lets a same-harness resume stay lossless while every other path still
  works.
- **A wire schema separate from code.** The neutral context schema is
  defined here, not by tclaude's internal structures, so refactoring tclaude
  cannot change the format by accident.
- **A plain ZIP of data.** ZIP is universally readable, streams, and allows
  random access for large archives. Entries are data only: a reader never
  extracts paths onto a filesystem. JSON keeps metadata inspectable by hand;
  JSON Lines keeps large transcripts streamable.
- **Sign the manifest bytes.** The manifest pins every other entry by hash, so
  one signature over its exact bytes covers the whole archive without a JSON
  canonicalization step.
- **Encryption is a wrapper.** Encryption wraps the whole file in a standard
  format (age), so any age implementation can decrypt a PAF and the inner
  format stays the same whether or not it was encrypted.
- **Counts, not content, in reports.** Loss reports and scan findings are safe
  to show in previews, audit logs and notifications because they never
  include transcript text.
- **Extensions are declared, namespaced and versioned.** This follows
  glTF's `extensionsUsed`/`extensionsRequired`, PDF's declared developer
  extensions, PNG's critical/ancillary and safe-to-copy chunk rules, and
  OOXML markup compatibility's ignorable vs must-understand namespaces.
  Bare `x-` prefixes, as in OpenAPI, say nothing about which system wrote a
  field or which version of it, so PAF does not use them.
- **Forks, not copies.** Treating every import as a fork keeps identity, mail
  and authority unambiguous: one agent id is only ever live in one place.

## File format

### Container

A PAF file is a ZIP archive (extension `.paf`, media type
`application/vnd.tclaude.paf+zip`). Entries are stored or deflated. Readers
must enforce these rules and refuse an archive that breaks them:

- `manifest.json` exists at the root.
- Every other entry, except files under `signatures/`, is listed in the
  manifest's `entries` with a matching byte length and SHA-256. Unlisted
  entries and duplicate names are refused.
- Entry names are relative, `/`-separated, at most 255 bytes, and match
  `[A-Za-z0-9._-]` per path segment. Names may not contain `.` or `..`
  segments, a leading `/`, backslashes or empty segments.
- Entries are regular files. Directory entries are ignored, and symlinks or
  other special files are refused.
- Sizes are bounded: tclaude defaults to 1 MiB for each JSON metadata entry,
  256 MiB for a single JSON Lines record and 2 GiB for the archive (configurable
  per node). Readers check both compressed and uncompressed sizes.
- An entry is data. A reader never writes an entry to a path derived from its
  name.

### Layout

```text
manifest.json                         format, version, kind, entry hashes
privacy.json                          scan, redactions, consent, retention   (optional)
group.json                            group configuration                    (kind "group")
signatures/<key-id>.json              detached signatures over manifest.json (optional)
attachments/<sha256>                  content-addressed media and files      (optional)
agents/<key>/agent.json               identity, definition, provenance, lineage
agents/<key>/environment.json         the original environment
agents/<key>/context/transcript.jsonl the effective model context          (optional)
agents/<key>/context/losses.json      what the context did not carry         (with context)
agents/<key>/context/full.jsonl       the full readable history              (optional)
agents/<key>/context/handoff.md       an agent-written handoff               (optional)
agents/<key>/native/<format>/<file>   the native history, byte for byte     (optional)
ext/<extension-id>/...                extension entries                      (optional)
```

`<key>` names the agent inside the archive (`[a-z0-9-]{1,64}`). An agent PAF
has exactly one agent; a group PAF has one per member. Attachments are shared
by every agent in the archive.

A PAF without `context/` is a *definition-only* PAF: a configured agent with
no history.

### manifest.json

Schema: [`manifest.schema.json`](paf/schema/v0.1/manifest.schema.json).

```json
{
  "format": "paf",
  "paf_version": "0.1",
  "kind": "agent",
  "created_at": "2026-10-11T19:40:00Z",
  "producer": {"name": "tclaude", "version": "v0.0.2212"},
  "requires": [],
  "extensions_used": {
    "tclaude": {"version": "0.1", "system": "tclaude", "system_version": "v0.0.2212", "spec": "https://github.com/tofutools/tclaude/blob/main/docs/paf.md#the-tclaude-extension"}
  },
  "extensions_required": [],
  "agents": ["reviewer"],
  "entries": [
    {"path": "agents/reviewer/agent.json", "bytes": 1234, "sha256": "…", "media_type": "application/json"}
  ]
}
```

- `kind` is `agent` or `group`.
- `agents` lists the agent keys present, in order.
- `requires` lists [features](#versioning-and-compatibility) a reader must
  understand to resume or import faithfully.
- `extensions_used` declares every [extension](#extensions) the file uses.
  `extensions_required` lists the ones a reader must understand to import or
  resume faithfully.
- `entries` lists every entry other than `manifest.json` and `signatures/*`.

### agent.json

Schema: [`agent.schema.json`](paf/schema/v0.1/agent.schema.json). This file holds
who the agent was and how it was defined:

- `id`, `name`, `title`: the source identity. The id is informational only;
  see [Identity and authority](#identity-and-authority).
- `instance`: the producing node (`id`, `name`), absent when stripped for
  publishing.
- `conversation`: the source harness conversation id and title.
- `definition`: the persona and task: `role`, `role_guidance`,
  `startup_context`, `initial_message`, `task` (`url`, `label`),
  `description`, `license`. A system's own launch configuration, such as
  tclaude's spawn profile, lives in that system's extension.
- `groups`: the teams the agent belonged to, by `name` and `role`. These are
  advisory provenance.
- `lineage`: ordered operations that produced this state. Each entry has an
  `op`:
  - `spawn`, `clone`, `morph`, `reincarnate`;
  - `move`, `teleport`, `recall`;
  - `snapshot`, `import`, `recover`;
  - `publish`.

  It also carries a time, a `from` reference (instance, agent, conversation)
  and an optional short detail.
- `snapshot`: present on snapshots: `name`, `note`, `taken_at` and the capture
  `state` (`idle` or `last_complete_turn`).
- `native`: describes the native blob, if any: `format`, `path`, `harness`,
  `harness_version` and `source_conv_id`.

### environment.json

Schema: [`environment.schema.json`](paf/schema/v0.1/environment.schema.json). This
file holds where and how the agent ran. Every field is a hint for the
receiver, never an instruction:

- `harness`: `name` (`claude`, `codex`, `opencode`, `copilot`, `gemini`, or
  another harness's name) and `version`.
- `model`:
  - the model: `id`, `provider`, `effort`;
  - the window: `context_window_tokens` and its `context_window_source`;
  - `usage`: context tokens in use at capture, and cumulative input, output
    and cache tokens.
- `approval` and `modes`: the harness approval posture and other modes, such
  as plan mode or fast mode.
- `sandbox`: `layers`, the confinement layers the agent ran under, from the
  outermost. Each has a `provider` (for example `tclaude` or the harness
  name) and the `mode` in that provider's own vocabulary. Provider-specific
  details go in an extension on the `environment.json` object itself, not on
  the layer (tclaude writes `extensions.tclaude.sandbox`).
- `workspace`: `cwd`, `repo_url`, `branch`, `commit`, `dirty`, `worktree`.
- `placeholders`: named path placeholders the importer remaps. Fields
  elsewhere in the environment and definition may contain `${NAME}`. Each
  placeholder has a `name`, the source-side `original` value (omitted when
  paths were redacted) and an optional `description`. Placeholders are
  substituted only in the definition and environment. Transcript and native
  content is history and is never rewritten on import, so a `${NAME}` there
  is either redaction output (recorded in `privacy.json`) or literal text.
- `skills`: skills the agent knew, by `name`, `source` and optional `version`
  or `sha256`. These are references, never payloads.
- `tools`: tools available to the agent: `name`, `kind` (`builtin`, `mcp`,
  `custom`) and, for MCP tools, the `server`.
- `mcp_servers`: MCP servers by `name`, `transport` and optional `version`.
  Commands, environment variables, headers and credentials are never recorded.
Details that only one harness understands go in an extension. tclaude
writes them under `extensions.tclaude.harness.<name>`.

### Context: transcript.jsonl

Schema: [`transcript-line.schema.json`](paf/schema/v0.1/transcript-line.schema.json),
one JSON object per line. The first line is a header:

```json
{"type": "header", "scope": "effective", "source": {"harness": "codex", "format": "codex-rollout-jsonl", "conv_id": "…", "model": "gpt-5.5", "cwd": "/home/me/project", "title": "Review PR 123"}}
```

Every following line is an entry, in conversation order:

```json
{"type": "entry", "id": "e1", "role": "user", "origin": "human", "time": "2026-10-11T19:00:00Z", "blocks": [{"kind": "text", "text": "Review PR 123."}]}
```

- `role` is `user` or `assistant`. `origin` is `human`, `agent` or `tool`.
  Tool results travel as `user` entries with origin `tool`, as in every native
  format.
- `model` names the model that produced an assistant entry. `author` names a
  non-primary speaker, such as a sub-agent.
- `blocks` have a `kind`:

| Kind | Fields |
|---|---|
| `text` | `text` |
| `thinking` | `text` (readable reasoning, may be empty), `opaque` (`vendor`, `data`: provider-bound reasoning only that vendor can use) |
| `tool_call` | `call_id`, `name`, `input` (JSON arguments) or `text` with `freeform: true` |
| `tool_result` | `call_id`, `text`, `is_error`, `media` |
| `media` | `media` |
| `summary` | `text`: a compaction summary standing in for earlier history |

A media item has a `media_type` and exactly one of `attachment`
(`sha256:<hex>`, naming `attachments/<hex>`), `url` or `path`. A `path` is a
hint about where the file lived on the source machine, not a reference into
the archive. A `name` is optional.

`scope` is `effective` for `transcript.jsonl`. `full.jsonl` uses the same
line schema with scope `full`. It may also contain `boundary` lines
(`kind`: `compaction`, `clear`, `rewind` or `branch`) marking where the
effective context diverges from the full history.

### Context: losses.json

Schema: [`losses.schema.json`](paf/schema/v0.1/losses.schema.json). This is the
loss report of whatever produced the context. It records:

- source and carried entry counts and estimated tokens, and the budget;
- `landing_rung`: `native`, `translated`, `context_file` or `archive`, as
  [above](#landing-a-paf), when the report describes a landing;
- `fallback`: the legacy morph delivery name, kept for compatibility. Its
  `native` means the history was translated into a native conversation
  (rung `translated`), *not* a byte-for-byte install.
  `context_file` maps to `context_file`, and `handoff` maps to
  `context_file` with an agent-written handoff;
- an `items` list of `{kind, severity, count, detail}`.

Loss kinds include:

- `reasoning_dropped`, `tool_records_stubbed`, `tool_calls_flattened`,
  `tool_output_clipped`;
- `media_externalized`, `media_dropped`;
- `pre_compaction_history`, `encrypted_compaction_summary`,
  `inactive_branch`, `sidechain_dropped`;
- `truncated_for_budget`, `unknown_records`, `redacted`.

Readers must accept kinds they do not know. A loss report never contains
transcript content.

### Native blob

The native history is stored under `native/<format>/` exactly as the
harness's history transfer produces it. Formats tclaude writes today:

| Format | Harness |
|---|---|
| `claude-jsonl` | Claude Code |
| `codex-rollout-jsonl` | Codex CLI |
| `opencode-session-jsonl` | OpenCode |
| `copilot-events-jsonl` | Copilot CLI |
| `gemini-session-jsonl` | Gemini CLI |

A native blob is only ever installed through that harness's reviewed import
path. Sidecar databases, hooks, credentials and caches never travel.

### Attachments

`attachments/<sha256>` holds media and file attachments referenced from the
context, named by the lowercase hex SHA-256 of their bytes. Identical
attachments are stored once.

### group.json

Schema: [`group.schema.json`](paf/schema/v0.1/group.schema.json). The group's
`name` and `description`, and its `members`, each pointing at an agent key
with an advisory `role`. System-specific group settings, such as tclaude's
standing orders, routes and ownership, live in that system's extension and
are advisory, like everything else about authority.

### Privacy

Schema: [`privacy.schema.json`](paf/schema/v0.1/privacy.schema.json). The optional
root `privacy.json` records:

- `sensitivity`: `private`, `shared` or `public`. A missing file means
  `private`.
- `audience`: who the PAF was prepared for (`self`, `recipients`, `board`,
  `public`) and, for recipients, their key ids.
- `scan`: the scanner name and version, when it ran, and `findings`
  (`kind`, `count`, up to three `locations`), plus `allowed: true` when a
  human explicitly allowed findings.
- `redactions`: applied redactions as `{kind, count, detail}`.
- `consent`: human confirmations, each with `by`, `at`, `audience` and
  `content_digest`.
- `retention`: `expires_at`.

The `content_digest` binds consent to content without a circular hash. It is
the lowercase hex SHA-256 of the lines `<sha256>  <path>\n` for every manifest
entry except `privacy.json`, sorted by path in byte order.

### Signatures

Each file under `signatures/` is a detached signature over the exact bytes of
`manifest.json`. Schema: [`signature.schema.json`](paf/schema/v0.1/signature.schema.json).

```json
{
  "alg": "ed25519",
  "public_key": "<base64 32-byte key>",
  "key_id": "<signer id>",
  "signer": {"kind": "instance", "name": "laptop"},
  "manifest_sha256": "<hex>",
  "signature": "<base64>"
}
```

The signed message is the ASCII string `paf-manifest-signature-v1\n` followed
by the lowercase hex SHA-256 of `manifest.json`. tclaude signs with the
producing instance's federation key; a published PAF uses a pseudonymous
publisher key instead (`signer.kind`: `publisher`). Verifying a signature
proves the manifest, and so every hashed entry, is unchanged since signing.

Rules for verifiers:

- **Trust is decided by `public_key` alone.** `key_id`, `signer` and the
  file name are self-asserted labels. A reader shows a signer as this node or
  a known peer only when the public key matches that node's key.
- **Recompute the manifest hash.** `manifest_sha256` is informational; a
  verifier hashes the manifest bytes itself.
- **Judge each signature on its own.** A signature with an unknown `alg` is
  skipped, and an invalid one is reported as invalid. Neither invalidates
  the archive or the other signatures. An unsigned PAF is valid but
  unverified.

So that every reader parses the same signed content, readers also refuse:

- JSON objects with duplicate keys in any PAF metadata file;
- ZIP local file headers whose name, sizes or method disagree with the
  central directory;
- entries whose actual decompressed size differs from the declared one.

Readers locate entries through the central directory only.

### Encryption

An encrypted PAF is the whole `.paf` file encrypted with
[age](https://age-encryption.org/v1) to one or more recipients, with
extension `.paf.age`, using age's standard X25519 recipient type. The
decrypted bytes are an ordinary PAF.

A tclaude instance's federation identity is an Ed25519 key. Its age recipient
is the X25519 public key obtained by the standard birational map from that
Ed25519 public key. Its age identity is the X25519 scalar derived the way
Ed25519 derives its signing scalar (the low 32 bytes of SHA-512 of the seed,
clamped). This is the same derivation tclaude's federation already uses for
sealed payloads. Any age implementation can then decrypt a PAF addressed to a
tclaude instance, given that instance's identity in age's `AGE-SECRET-KEY-1`
encoding. tclaude always includes the sender's own key as a recipient, so the
sender can read back what it sent.

## Extensions

The base format holds what any tool that reads or writes agent state could
understand. Everything specific to one system goes in an extension: a
harness, an orchestrator such as tclaude, or a product built on PAF. An
extension is easy to parse, and a reader can always tell which system wrote
it and in which version.

### What belongs in the base

A field belongs in the base when it means the same thing whichever system
produced it, and a reader on another system could act on it or show it. It
belongs in an extension when its meaning depends on one system's
mechanisms.

| Base | Extension |
|---|---|
| The conversation: entries, blocks, summaries, losses | Raw native records (use the native blob) |
| Name, role guidance, startup context, task | tclaude's inline spawn profile and its profile name |
| Harness and model name, effort, approval and modes in the harness's own words | Harness-only settings (auto-compact window, private state roots) |
| Sandbox layers as provider plus mode | A provider's sandbox profile, mounts and grants |
| Workspace hints and path placeholders | Worktree or repository tooling state |
| Skill, tool and MCP server references | Installation sources and registries |
| Teams the agent belonged to, by name and role | tclaude permission slugs, ownership, standing orders, routes |
| Lineage operations, with time and source | Teleport chains, move offers, mail custody |
| Privacy record, signatures | — |

When in doubt, a new field starts in an extension. Promoting it to the base
later is a minor version change; demoting a base field is a major one.

### Declaring an extension

Every extension a file uses is declared once in `manifest.extensions_used`,
keyed by its id:

```json
"extensions_used": {
  "tclaude": {
    "version": "0.1",
    "system": "tclaude",
    "system_version": "v0.0.2212",
    "spec": "https://github.com/tofutools/tclaude/blob/main/docs/paf.md#the-tclaude-extension",
    "schema": "https://raw.githubusercontent.com/tofutools/tclaude/main/docs/paf/schema/v0.1/ext/tclaude.schema.json",
    "copy": "unsafe"
  }
}
```

- **id:** lowercase. Ids with a single label (`tclaude`) are reserved for
  extensions documented on this page, and a reader refuses any other
  single-label id. Anyone else uses a reverse-DNS id they control, such as
  `com.example.agentlab`.
- **`version`:** the extension's own format version, `MAJOR.MINOR`, with the
  same rules as `paf_version`. A reader that knows the extension but not that
  version (another major, or while the major is 0 any other minor) treats it
  as unknown.
- **`system` and `system_version`:** which software wrote the data, and which
  version of it.
- **`spec`:** where the extension is documented. `schema` (optional) points
  at a JSON Schema for its payloads.
- **`copy`:** whether the data may survive edits by a tool that does not
  understand it, after the PNG rule. With `safe`, the data does not depend on
  the rest of the file, and such a tool may keep it unchanged. With `unsafe`
  (the default), the data may describe other content, and a tool that
  changes the PAF without understanding the extension must drop it. Changes
  include redacting, translating, re-signing for publishing, and merging
  agents into a group. An unknown `copy` value means `unsafe`. `copy`
  governs ordinary edits only; privacy operations follow the stricter rule
  below.

Dropping an extension removes all of it: its data on every object, its
`ext/<id>/` entries (and their manifest rows), its `extensions_used`
declaration and its id in `extensions_required`. Re-signing is then needed,
because the manifest changed.

`manifest.extensions_required` lists ids without which the content would be
misunderstood. Every id in it must also be declared in `extensions_used`. It
follows the same refuse-or-degrade rules as `requires`.

### Using an extension

- **In JSON:** any object in PAF metadata or in a transcript line may carry
  an `extensions` property: an object keyed by extension id, whose values are
  that extension's objects. The property name `extensions` is reserved for
  this on every object, including the manifest root. Signature files are the
  exception: they are not covered by the manifest hash, so they carry no
  extension data.

  ```json
  {"name": "fixer", "definition": {}, "extensions": {"tclaude": {"spawn_profile": {"name": "worker-opus", "profile": {}}}}}
  ```

- **As files:** an extension's own entries live under `ext/<id>/`, are listed
  in the manifest like any entry, and are hashed and signed with the rest.

Extension data is scanned for credentials like everything else.
Redaction, `strip_identity` and publishing keep an extension only when the
tool understands it and applies the same redactions to its data. Otherwise
they drop it, whatever its `copy` flag. `copy: safe` never exempts data from
privacy rules.

### The tclaude extension

tclaude writes the `tclaude` extension (schema:
[`ext/tclaude.schema.json`](paf/schema/v0.1/ext/tclaude.schema.json)) for its
own state that other systems are not expected to share:

| Where | Field | Meaning |
|---|---|---|
| `agent.json` | `spawn_profile` | `name` (informational) and `profile`: tclaude's inline spawn profile, including launch posture. Receivers never look a profile up by name. |
| `agent.json` | `permissions` | Permission rows (`slug`, `effect`, `scope`, `source`) as advisory provenance. They never become grants unless the receiver's own policy approves them. |
| `agent.json` | `teleport` | The teleport chain id and hops (`from_instance`, `from_agent`, `to_instance`, `to_group`, `at`). |
| `agent.json` | `transfer` | Continuation state for move and teleport: the stable federation `identity`, the sender's `carry_permissions` opt-in, the `trigger` and source `warnings`. |
| `environment.json` | `sandbox` | tclaude sandbox details: the profile name and settings behind the `tclaude` layer. |
| `environment.json` placeholders | `item`, `field` | The configuration item and field a placeholder stands in. |
| `environment.json` | `harness.<name>` | Settings only one harness has, such as `harness.claude.auto_compact_window`. |
| `group.json` | `group` | Group settings: description, standing orders, environment, group permissions, member limit and similar. |
| `group.json` members | `owner` | The member owned the group (advisory; ownership never travels). |
| `ext/tclaude/mail-deliveries.json` | entry | The mail delivery ledger a continuation carries. |
| `ext/tclaude/group-state.json` | entry | A group's tclaude state in the group export's shape: members, owners, permissions, enrollment, cron jobs, messages, audit trail and per-conversation metadata. |
| `manifest.json` | `converted_from` | The legacy carrier a PAF was converted from: `format`, `format_version`, `tclaude_version`, `created_at`. |

The extension is `copy: unsafe`, version `0.1`.

## Versioning and compatibility

`paf_version` is `MAJOR.MINOR`, compared the way semantic versioning
compares versions.

- **While the major version is 0**, the format is still settling and any
  minor version may change it incompatibly. A 0.x reader reads exactly the
  0.y versions it implements and refuses every other one, older or newer,
  the same way it refuses an unknown major version. The rule below that a
  minor version only adds applies from 1.0.
- From 1.0, a **minor** version only adds: new optional fields, new optional
  entries, new enum values in fields documented as open (loss kinds, lineage
  ops, redaction kinds), or new `requires` features. A 1.0 reader reads every
  1.x file.
- A **major** version may change anything. A reader refuses an unknown major
  version, but still shows `format`, `paf_version` and `producer` so the user
  knows what to upgrade.
- Readers **ignore unknown fields** and any listed entry they do not
  understand, wherever it sits. Writers must not rely on readers understanding
  a field or entry unless it is listed in `requires`.
- **Open enumerations** may gain values in a minor version, and readers
  accept unknown values. These are block `kind`, JSON Lines `type`, loss
  `kind` and `severity`, lineage `op`, boundary `kind`, redaction `kind`,
  audience `kind`, `sensitivity`, snapshot `state`, tool `kind`, `signer.kind`
  and signature `alg`. The schemas list the known values without restricting
  them. A reader renders an unknown block kind or line type as a visible
  "unsupported content" placeholder that keeps its `text` when present, and
  never drops it silently. A writer that needs a reader to understand a new
  value lists a feature in `requires`.
- **Closed enumerations** change only in a major version (or, while the
  major version is 0, in any minor version): `format`, `kind`
  (`agent`, `group`), entry `role` (`user`, `assistant`) and `origin`
  (`human`, `agent`, `tool`), and the transcript `scope`.
- **`requires`** names features without which the content would be
  misunderstood, not merely incomplete (for example, a context stored as a
  delta against an earlier snapshot). A reader that does not know a required
  feature:
  - **refuses** to import, resume or rewrite the PAF (landing rungs 1 to 3);
  - **degrades** for read-only inspection and rendering (rung 4). It shows what
    it understands and warns which features it could not interpret.
- **Extensions** carry everything outside the base; see
  [Extensions](#extensions). An unknown extension in `extensions_required`
  is handled like an unknown `requires` feature. Any other unknown extension
  is ignored.
- Writers stamp the lowest version that expresses the content.

The schemas for version 0.1 live under `docs/paf/schema/v0.1/`, and their
`$id`s include `v0.1`. While the major version is 0, each minor version gets
its own schema directory. From 1.0, one directory per major version (`v1/`)
validates every file of that major version, because unknown properties are
allowed. A few rules are semantic rather than structural, and
a reader checks them in code:

- entry hashes and sizes match the manifest;
- no entry name contains a `.` or `..` segment;
- every agent key listed in the manifest has an `agent.json`;
- `agent.native.path` lies under that agent's own `agents/<key>/native/`;
- every `attachment` reference resolves to an entry;
- every extension id used in an `extensions` object, an `ext/` entry or
  `extensions_required` is declared in `extensions_used`;
- no single-label extension id appears other than those this page documents;
- JSON metadata has no duplicate keys.

## Legacy formats

tclaude keeps importing the formats PAF supersedes, and keeps writing the
agent bundle for peers that do not advertise PAF support.

| Legacy | Maps to |
|---|---|
| Agent bundle v1 (`tclaude-agent-bundle`, `manifest.json` + `history/transcript.jsonl`) | One agent; `agent.definition` from `manifest.agent`; the native blob from `history`; findings into `privacy.scan`; the inline profile and permissions into the `tclaude` extension; the mail ledger into `ext/tclaude/`. There is no neutral context, so landing in another harness decodes the native blob first. |
| Group export v5 (`manifest.json` + `projects/<conv>.jsonl`) | A group; one agent per conversation with a native Claude Code blob; the group row into `group.json`'s `tclaude` extension and the other tables into `ext/tclaude/group-state.json`. |
| Morph artifacts (`transcript.md`, `transcript.jsonl`, `loss-report.json`) | Readable as a context-file landing. `loss-report.json` has the fields of `losses.json`; readers treat `"items": null` as an empty list and map `fallback` to `landing_rung`. |

## Related pages

- [Federation: portable local agent bundles](federation.md#portable-local-agent-bundles)
  and [sharing an agent with a peer](federation.md#sharing-an-agent-with-a-peer)
- [Harnesses: cross-harness history translation](harnesses.md#cross-harness-history-translation)
- [Spawning and lifecycle: morph and clone](spawning-and-lifecycle.md#morph)
- [Federation: content boards](federation.md#content-board-membership)
