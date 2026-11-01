# Portable Agent Format (PAF)

The Portable Agent Format is an open, versioned file format for the state of a
coding agent. A `.paf` file holds what an agent needs to resume work: its
conversation history in a harness-neutral form, its definition and identity,
and the environment it ran in. It can also hold a second copy of the history
in the original harness's own native format, so the same harness can resume it
without any loss. You can resume that agent on another machine or in another
harness, possibly long after the original harness or model has gone.

This page is both the specification and the user guide; the
[Walkthrough](#walkthrough) is a hands-on start. It covers why the
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
carrier: the [agent bundle](federation-moving.md#portable-local-agent-bundles)
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
  are provenance. They become grants on the receiver only when that receiver's
  own policy approves them, or a human here keeps them from the import's confirm
  list (see [Keeping what a file asks for](#keeping-what-a-file-asks-for)). A deny
  stays, since it only takes away. Scheduled jobs land disabled unless kept.
  Members and their roles stay. Sudo is temporary, so an export never holds it,
  and an import always drops it from an older file with a warning, and never
  offers it. An agent's own ownership never travels, since its source group means
  nothing here; a group's owners are confirmed like the rest of the list.
- **Posture is decided by the receiver.** The recorded sandbox and approval
  posture is shown at import, but the landing profile and the receiver's
  policy decide how the new agent actually runs. A file can make its sandbox
  tighter than a spawn here would resolve, never wider, and no confirmation
  changes that. Approval, tools and auto-review may tighten too; a wider value
  is dropped unless a human keeps it, and the preview and the import result say
  which. A directory trust, remote control or environment variables it asks
  for are dropped the same way, unless a spawn here gives them, as are peer
  messaging, auto memory, the Copilot API drive, a question timeout, a model
  proxy, a context feature a tier here trims, and a trim of the repo's or
  organization's instructions. Other trims add to the receiver's. The warning
  names variables, not their values, and a group file's environment goes the
  same way. The dashboard keeps the import dialog open on the result when an
  import has warnings, so you can read them. A teleport or direct move keeps all of
  this, so a moved agent does not stall on prompts, when it lands automatically
  or when its peer holds `agents.teleport.receive` for the receiving group, and
  no human accepts it by hand. A human accepting a teleport or move by hand gets
  the [same list](#keeping-what-a-file-asks-for), with every item ticked when the
  sending peer has unrestricted trust and unticked otherwise; a request with no
  selection, such as `tclaude federation offers import` without `--keep`, takes
  those ticks. Without that grant, and with no human, the offer's teleport or
  move label counts for nothing and it lands as a file. A received
  history is
  untrusted input: it may contain instructions written by anyone whose text
  reached the original agent.

### Keeping what a file asks for

A file import drops what it asks for beyond a spawn here. The preview lists those
items, grouped by kind, each with what it gives, so a human can keep the ones they
want. Items a spawn here would give anyway, or that only tighten, are not listed.

| In the list | Starts |
|---|---|
| Group owners | ticked |
| Permissions and per-conversation grants | ticked |
| Wider approval, tools and auto-review | ticked |
| `trust_dir`, `remote_control`, `model_proxy`, `peer_messaging` | ticked |
| `auto_memory`, `copilot_api`, `ask_user_question_timeout` | ticked |
| `context_features` trims of instructions or policy, and features a tier here trims | ticked |
| Environment variables | unticked |
| Enabled scheduled jobs | unticked |

The sandbox and sudo are never in the list. Anything unticked is dropped with a
warning, and the result lists what was kept and what was dropped. A kept scheduled
job lands enabled, and it still lands without operator attribution until you set
that. A grant that cannot land here, such as an unknown permission or a scope naming
a specific agent, is not offered and only warns.

The list names environment variables and never holds their values. Reveal value in
the dashboard, or `tclaude paf reveal <file> <name>`, asks for one value in a request
of its own. Keeping a variable means you could see its value.

The server decides. The apply request names what was kept, with the digest of the
file it was read from, and the server checks both: a different file is refused, and
so is an item this file does not list. Only a human on this node can keep anything.
An agent caller keeps nothing even if it names items, and it gets a warning. So does
a request a federation peer forwards, including a peer with unrestricted trust.

On the command line, the preview or `--dry-run` prints the list and the digest:

```bash
tclaude agent groups import team.paf --dry-run --into "$PWD"
tclaude agent groups import team.paf --into "$PWD" --keep-defaults --keep-digest <digest>
tclaude agent groups import team.paf --into "$PWD" --keep owner:<conv> --keep cron:nightly --keep-digest <digest>
tclaude paf import --file scout.paf --group team --apply --keep-all --keep-digest <digest>
```

`--keep-defaults` keeps what the dashboard starts with ticked, `--keep <id>` names
items, and `--keep-all` takes every listed item. `--keep-none` drops everything,
owners included. An offer's environment values cannot be revealed, and its
permissions stay under the carry policy rather than this list. A request that names no selection has confirmed nothing, so only a
group's owners land, for a human caller. Snapshot spawns and board imports are such
requests.

## How tclaude uses PAF

| Operation | What PAF carries | Carrier in this release |
|---|---|---|
| **Export / import** of an agent | One agent PAF file. Import previews first, then lands the agent on a harness you choose. | PAF by default, from [`tclaude paf export`](#exporting-and-importing-an-agent) or `agent bundle export`; [agent bundle v1](federation-moving.md#portable-local-agent-bundles) with `--format v1` |
| **Group export / import** | A group PAF: group configuration plus every member. | A [group PAF](#exporting-and-importing-a-group) by default; group export v5 with `--format zip` |
| **Snapshots** | A named PAF in a local library. Instantiate one or many agents from it, or share it. | PAF in the [snapshot library](#snapshots) |
| **Morph** (model or harness change in place) | The neutral context, losses and full transcript the switch produced. The identity stays in place, so no new agent is created. | Morph artifact directory |
| **Clone** | A local sibling. Clone copies native history directly and shares PAF's history model. | Native copy (no file) |
| **Move, teleport, recall home** | The agent's state in transit to another node, with provenance hops. Peers that do not advertise PAF receive the legacy agent bundle. | PAF to peers that advertise it, otherwise agent bundle v1 |
| **Sharing** with a peer or on a [Board](federation-boards.md#content-board-membership) | An encrypted PAF addressed to named recipients, published only by a human. | Unencrypted PAF to peers that advertise it, otherwise agent bundle v1; [snapshots on Boards](#sharing-a-snapshot-on-a-board) as an encrypted PAF |
| **Publishing** | A redacted, pseudonymous, signed PAF file for public sharing. tclaude provides no hosting; you choose where the file goes. | PAF with [`tclaude paf publish` / `agent snapshot publish`](#publishing) |
| **Resilience** | Opt-in periodic encrypted snapshots to another node or Board, from which an agent can be recovered if its host is lost. | Encrypted PAF to a [federation peer or a Board](#resilience-snapshots), [recovered](#recovering-an-agent) under its agent id |
| **Archival** | Any PAF can be inspected, verified and rendered to readable text without the original harness or model. | [`tclaude paf`](#reading-a-paf) |

Any of the file operations above can also carry workflows beside the agents;
see [Carrying workflows](#carrying-workflows).

The last column says what each operation uses in this release. tclaude keeps
importing every legacy carrier after an operation switches to PAF; see
[Legacy formats](#legacy-formats).

## Walkthrough

This hands-on walkthrough exercises the main operations on one agent. You
save it to a file, read it with no harness, land it on another harness or a
smaller model, fork it from a snapshot, and move it to another machine. Each
step links the section that explains it in full.

**You need:** the daemon running and `TCLAUDE_HUMAN_TOKEN` exported, as in
[Get started](learn/get-started.md). The last step also needs two
linked machines. All the other steps work on one.

### 1. Give an agent something worth saving

```bash
cd ~/code/my-project
tclaude agent groups create lab --descr "PAF walkthrough"
tclaude agent spawn lab --name scout \
  --initial-message "Read this project and write a short list of its main packages and what each does."
```

Wait until `scout` is idle in the dashboard or in `tclaude session watch`.

### 2. Export it to a file

```bash
tclaude paf export scout --history --file scout.paf
```

`--history` includes the conversation twice:
- as the harness's own native history, for a lossless resume on the same
  harness;
- as a harness-neutral copy that any harness can land.

If the conversation contains text that looks like a credential, the export
refuses and says where. Check the finding. Then export without `--history`,
or with `--allow-flagged` if you are sure the file stays with you. See
[Exporting and importing an agent](#exporting-and-importing-an-agent).

### 3. Read it without any harness

```bash
tclaude paf inspect scout.paf
tclaude paf render scout.paf --out scout.html
```

`inspect` shows what the file holds: harness, model, working directory,
signatures and privacy findings. It does not show the conversation. `render`
writes the conversation as one self-contained HTML page, or as Markdown when
you leave out `--out`. See [Reading a PAF](#reading-a-paf).

### 4. Land it on another harness or a smaller model

Import creates a **new** agent with a new identity. It previews first and
changes nothing until you add `--apply`. Preview a landing on Codex, and name
the model so tclaude can fit the conversation to its context window:

```bash
tclaude paf import --file scout.paf --cwd "$PWD" --harness codex --model gpt-5.5
```

`history_landing` in the preview has three possible values:
- **native** when the history fits as it is: same harness, and a window
  large enough;
- **translated** when it moves to another harness or must be cut to fit,
  listing what does not carry over;
- **none**, meaning the agent would import as configuration only.

A model with a smaller window works the same way:

```bash
tclaude paf import --file scout.paf --cwd "$PWD" --model sonnet
```

The preview says how much of the conversation would be cut, if any. From a
25% cut, `--apply` refuses until you choose one of three options:
- `--confirm-history-loss`;
- a model with a larger window;
- `--skip-history`.

Then land it for real:

```bash
tclaude paf import --file scout.paf --cwd "$PWD" --harness codex --model gpt-5.5 \
  --group lab --name scout-codex --apply
```

See [Landing a PAF](#landing-a-paf) and
[Landing on a smaller context window](#landing-on-a-smaller-context-window).

### 5. Fork agents from a snapshot

A snapshot is a named save point in this machine's library. The agent carries
on, and the snapshot stays as it was:

```bash
tclaude agent snapshot create scout --name scout-v1 --note "after the package survey"
tclaude agent spawn lab --from-snapshot scout-v1 --count 2 --name trial
```

`trial-1` and `trial-2` each start with their own identity and a copy of the
conversation. See [Snapshots](#snapshots) and
[Spawning from a snapshot](#spawning-from-a-snapshot).

### 6. Move an agent to another machine

This needs two instances linked through a hub
([Federation setup](federation-setup.md)). Below, this machine is `desktop`,
and the trusted peer `laptop` has a group `helpers`.

1. With restricted trust, first allow the move on `laptop`:

    ```bash
    tclaude federation grant desktop agents.receive --scope group=helpers
    ```

2. Move `scout` from `desktop`:

    ```bash
    tclaude federation move-agent scout laptop --group helpers
    ```

3. Accept the offer on `laptop`, previewing it first:

    ```bash
    tclaude federation offers
    tclaude federation offers import OFFER_ID --cwd ~/code/my-project
    tclaude federation offers import OFFER_ID --cwd ~/code/my-project --apply
    ```

The source keeps running until the agent runs on `laptop`, then retires
`scout`. `tclaude federation moves ls` shows the progress.

An agent can also move itself. Grant `trial-1` teleport rights scoped to the
destination, then ask it to run `tclaude agent teleport laptop --group helpers`:

```bash
tclaude agent permissions grant trial-1 self.teleport --scope peer=laptop
```

With only the `agents.receive` grant, the teleport arrives on `laptop` as an
ordinary offer. Accept it with `federation offers import` as in step 3.
Automatic landing needs extra setup on the receiver.

See [Moving an agent](federation-moving.md#moving-an-agent) and
[Teleport yourself to another node](federation-moving.md#teleport-yourself-to-another-node).

!!! note "Sharing snapshots on a Board"
    `tclaude agent snapshot share` shares a snapshot, encrypted to named
    members, on a federation Board. Update `tclaude-hub` first: an older hub
    without board snapshot support is refused before anything is uploaded.
    See [Sharing a snapshot on a Board](#sharing-a-snapshot-on-a-board).

### 7. Clean up

```bash
tclaude agent groups retire lab
tclaude agent groups archive lab
tclaude agent snapshot rm scout-v1
```

On `laptop`, retire the moved agents too if you no longer need them.

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

## Carrying workflows

A PAF is also the shared envelope: one archive that holds agents and
[PWF](pwf.md) workflows side by side, and can say which agents perform which
workflows. There is no separate file type. A `.pwf` stays a workflow on its
own, and a `.paf` that carries workflows opens wherever a `.pwf` does.

Carrying is opt-in. Nothing below happens unless you ask for it.

```bash
# An agent, with the templates of its active runs or a named template.
tclaude paf export reviewer --file reviewer.paf --workflows
tclaude paf export reviewer --file reviewer.paf --workflow ship-feature

# A group with its saved processes; a snapshot likewise.
tclaude agent groups export team --out team.paf --workflows
tclaude agent snapshot create before-release --workflow ship-feature

# A workflow with an agent for one of its roles, pre-briefed.
tclaude agent process-templates export ship-feature \
  --performer reviewer=alice --performer-history
```

The last form writes `ship-feature.paf`, of the [kind `bundle`](#manifestjson):
workflows and the agents that perform them, with no group.

Each side keeps its own gates. Carrying a workflow needs
`process.templates.read` and processes turned on. Shipping a performer needs
`agent.bundle.export` on that agent, and is refused on suspected credentials
in the conversation until you pass `--allow-flagged`. Without
`--performer-history` a performer travels as its definition only.

### Importing what a file carries

Importing never takes both sides at once. The file is reviewed one thing at a
time, with the review that thing always gets:

- `paf import` and `agent groups import` land the agents, store no workflows,
  and tell you what else the file holds. `--dry-run` and the preview list it.
- `process-templates import` stores the workflow after the
  [workflow review](pwf.md#sharing-a-workflow), and lands no agents. Its review
  lists the performers the file ships. `--workflow <key>` picks one when the
  file holds several.
- A performer lands through the agent import, by its key:

```bash
tclaude paf import --file ship-feature.paf --agent reviewer --role reviewer \
  --cwd "$PWD" --group team --apply
```

`--role` is the role you land it under. The file proposes a role and cannot
assign one: a role given by the importer is the only way a performer gets it.

In the dashboard, *import workflow* accepts a `.paf`. Its review lists the
agents the file ships, each with *Land it…*, and the agent import dialog offers
*Review workflows…* for a file that carries them. Both export dialogs offer
*Carry its workflows*. Shipping performers is on the command line for now. All of
this is hidden when processes are turned off.

### What sharing does to a carried workflow

A carried workflow's templates are addressed by hash, so they cannot be edited
in place. [Redaction](#redacting-a-paf) keeps or drops each workflow whole, and
records a dropped one as `drop_unredactable`:

| Redaction | Carried workflow |
| --- | --- |
| Conversation-only (`--stub-tool-results`, `--drop-reasoning`, `--keep-turns`, …) | Kept |
| `mask_flagged` | Kept if its scan is clean, otherwise dropped |
| Paths to placeholders, `strip_identity` (so every [published](#publishing) PAF) | Dropped |

A `bundle` that loses all its workflows this way becomes an `agent` PAF if it
has one agent; with several, the redaction is refused. The credential scan
covers workflow entries like any other. Federation transfers do not carry
workflows; only files do. A bundle offered as a federation transfer is refused.

## Exporting and importing an agent

```bash
tclaude paf export reviewer --file reviewer.paf --history
tclaude paf import --file reviewer.paf --cwd "$PWD"                      # preview
tclaude paf import --file reviewer.paf --cwd "$PWD" --group team --apply
tclaude paf import --file reviewer.paf --cwd "$PWD" --group team --apply --no-start
tclaude paf import --file reviewer.paf --cwd "$PWD" --harness codex --model gpt-5.5
```

`paf export` is `agent bundle export`, which writes a PAF by default, with the same
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

`--apply` starts the imported agent. With `--no-start` it lands offline
instead: it joins the group with its conversation installed and its
briefing queued, and `tclaude agent resume` starts it later on its landing
harness. Landing offline needs a conversation that lands (not
`--skip-history` or a configuration-only import). On OpenCode the
conversation goes in the store its landing posture reads, so a
tclaude-sandboxed agent lands in private state, and its server starts at
resume. OpenCode takes a model only from the launch prompt, so an offline
landing with its conversation intact keeps the conversation's last model, even
when the import names another; the import warns when it does.

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
  format and, when it lands on a smaller context window than it ran in, it
  fits that window's budget. A native history that does not fit, and that
  no trimmed translation fits either, still lands as it is, with a warning;
- **translated** otherwise: the neutral context, or the native history
  decoded by tclaude's reader for its harness, is translated for the landing
  harness and budgeted to the landing model's context window, with the
  expected losses;
- **none** when the conversation cannot land, for example on a harness
  without history import, or when it does not fit the context budget even
  after trimming. The agent then imports as configuration only, with a
  warning; `tclaude paf render` still reads the whole conversation.

The receiving group's sandbox posture applies, and a file may tighten it but
never widen it. An agent that ran under tclaude's sandbox keeps it, and the import
is refused where this node cannot run it. The exception is a landing on Codex `read-only`
or `workspace-write`, which replaces tclaude's sandbox because that sandbox would
allow more. A harness's own mode stays only when it is
stricter than the landing posture, such as Codex `read-only` landing on
`workspace-write`; anything looser is dropped, as is `off`. A kept mode is refused
like any other launch if it cannot enforce the receiver's sandbox profiles. A
landing under tclaude's sandbox keeps tclaude's sandbox. This holds for moves and teleports too,
so an agent does not land with wider access than it had. The preview states the
result. Without `--group` it shows the posture with no group's settings.

On another harness none of the source's launch posture applies, tclaude's sandbox
included; the receiver's defaults and normal
spawn checks apply, and the preview says so. Imports through federation
offers, moves and teleports keep landing on the source harness.

### Landing on a smaller context window

Take an agent that ran on a model with a 1M-token window and had used half
of it, about 500k tokens, imported onto a model with a 200k window:

```bash
tclaude paf import --file big.paf --cwd "$PWD" --model sonnet --group team --apply
```

- **The budget.** The carried conversation may fill 60% of the landing
  window (`agent.morph.history_budget_percent`, as for a morph), here about
  120k tokens; the rest is headroom so the agent does not compact on its
  first turn. A native history landing on a smaller window than it ran in
  (here 1M onto 200k; `opus[1m]` onto `opus` counts too) is installed as it
  is only when it fits that budget. This one does not, so it lands
  translated and trimmed instead, and the preview says so. A fork or recover
  onto an equal or larger window keeps its native history unchecked.
- **What is kept.** The original task and the latest instructions are
  pinned, as are the compaction summaries the harness wrote when it
  compacted. Then come the newest complete turns that fit. Long tool
  inputs and results are clipped, and media the landing harness cannot
  carry is left out. The rest, here roughly three quarters of the
  conversation, is left out, and markers say where.
- **Confirmation.** The preview reports how much is cut. From 10% it warns.
  From 25% `--apply` refuses until you choose:
  - `--confirm-history-loss` to accept the cut;
  - `--model` for a model with a larger window;
  - `--skip-history` to import the agent without its conversation.

  `agent spawn --from-snapshot`, `snapshot board import` and `agent
  recover` take `--confirm-history-loss` too; the dashboard's import and
  snapshot spawn have a checkbox for it. Group import takes the same flag and
  its dry run shows the cut per member. The dashboard's group import has the
  checkbox too, and without it the whole import is refused, naming each member
  and its cut.
- **Where the full history is.** It is never lost. It stays in the PAF,
  which `tclaude paf render` reads whole. When an agent or group import cuts it, it
  is also saved beside the landed agent as `transcript.md` and `transcript.jsonl` (a PAF context file), in
  its morph artifact directory, and the agent gets an inbox message naming
  both, so it can read what was cut instead of guessing.
- **A better result.** A model with a larger window keeps more. Or export
  with a summary:

  ```bash
  tclaude paf export big-agent --history --with-summary --file big.paf
  tclaude agent snapshot create big-agent --with-summary
  ```

  `--with-summary` first asks the agent to write a summary of its earlier
  work: the goal, decisions, completed work, open issues and next steps. The
  agent submits it with `tclaude agent morph-handoff`, as a morph's source
  does. The PAF carries it as `context/handoff.md`. A landing that has to cut
  then keeps the pinned start, that summary and the newest turns that fit
  beside it; a summary too large for the budget is left out, with a
  warning, and stays in the PAF. Asking
  costs the agent a turn, so it is opt-in and never happens on its own. The
  export waits up to `--summary-wait` (default 10 minutes) and is not made
  without the summary. An export's summary is scanned for credentials like
  the conversation; a snapshot's is scanned with the rest of the archive
  when it is shared or imported. An agent cannot ask for its own.

## Snapshots

A snapshot is a named, point-in-time PAF of an agent, kept in this node's
library. It is a fork point: the agent carries on, and the snapshot stays as
it was captured.

```bash
tclaude agent snapshot create reviewer --name before-refactor --note "known good" --expires 30d
tclaude agent snapshot ls
tclaude agent snapshot show before-refactor
tclaude agent snapshot export before-refactor -f before-refactor.paf
tclaude agent snapshot rm before-refactor
tclaude agent snapshot prune
```

- **What it holds.** The agent's definition, environment and conversation:
  the native history plus a harness-neutral context, as `tclaude paf export
  --history` writes them. `--no-history` captures the definition only.
  `agent.json` carries `snapshot` (`name`, `note`, `taken_at`, `state`) and a
  `snapshot` lineage step that names the source agent and conversation.
- **Consistency.** An idle agent is captured whole (`state: idle`). A busy
  agent is captured up to its last complete turn (`state:
  last_complete_turn`): the history export writes only finished records.
- **Names.** 1 to 64 characters of `a-z`, `0-9`, `.`, `_` and `-`, starting
  with a letter or digit. The default is `<agent>-<UTC timestamp>`.
- **Storage.** Each snapshot is a directory under `~/.tclaude/data/snapshots/`
  (mode 0700) holding `snapshot.paf` and a `snapshot.json` listing record
  (files 0600). The PAF is the source of truth. A snapshot is written to a
  hidden temporary directory, synced, and renamed into place, so a
  half-written one never appears.
- **Retention.** `--expires` takes a duration (`72h`, `30d`), an RFC 3339
  time or `never`. Without it, the config key `snapshots.default_retention`
  applies; when that is unset, snapshots are kept until removed. Expired
  snapshots are removed when the library is listed, when a snapshot is
  created and by `snapshot prune`. Removal and pruning are ordered with
  creation under one lock, so retention never races a create.
- **Who may.** Creating a snapshot needs `agent.bundle.export` on the agent,
  as an export does. Because only the human removes snapshots, an agent's
  snapshots expire within 30 days, and an agent cannot add to a library of
  200 or more. Listing and showing need only an identified caller; agents
  see neither the note nor where in the history findings are.
  Exporting a snapshot's PAF needs the human or `agent.bundle.export` on the
  snapshotted agent. Removing and pruning are human-only.
- **Credentials.** A snapshot stays on this node, so scan findings are
  recorded rather than refused. Redact and consent before sharing an exported
  snapshot.

### Spawning from a snapshot

```bash
tclaude agent spawn team --from-snapshot before-refactor
tclaude agent spawn team --from-snapshot before-refactor --count 3 --name trial --cwd "$PWD" --harness codex
```

Each spawned agent is an import of the snapshot's PAF, landed the way
`tclaude paf import --apply` lands one: the same landing harness and model,
history translation, path remapping and spawn checks. Each gets a new
identity and continues from the snapshot's conversation; the original agent
is untouched.

- `--count` (1 to 20) spawns several agents. With a count above one, each
  name gets a `-1`, `-2`, ... suffix. The name defaults to the snapshotted
  agent's name.
- Without `--cwd`, agents land at the recorded working directory.
- Only the group argument, `--count`, `--name`, `--cwd`, `--harness` and
  `--model` combine with `--from-snapshot`; other spawn flags are refused.
- **Who may.** The caller needs access to the snapshot's content (the human,
  or `agent.bundle.export` on the snapshotted agent) plus what an import with
  `--apply` needs in the receiving group.
- **Provenance.** Each new agent's arrival briefing names the snapshot and
  when it was taken, and the audit log records a `snapshot.spawn` entry with
  `from_snapshot <name>`. The link is provenance only: it grants no authority
  over or from the original agent.
- If a later agent in a `--count` run fails, the agents already spawned are
  kept and reported with the error.

### Sharing a snapshot on a Board

```bash
tclaude agent snapshot share before-refactor --board BOARD --to lab --to inst_7c21… --expires 3d
tclaude agent snapshot share --file reviewer.redacted.paf --board BOARD --to lab
tclaude agent snapshot board ls --board BOARD
tclaude agent snapshot board fetch --board BOARD --item ITEM --version VERSION
tclaude agent snapshot board preview --board BOARD --item ITEM --version VERSION --group team
tclaude agent snapshot board import --board BOARD --item ITEM --version VERSION --group team --preview-token TOKEN
tclaude agent snapshot board rm --board BOARD --item ITEM --version VERSION [--local]
```

A snapshot goes on a [Board](federation-boards.md#content-board-membership) as an
`agent-snapshot` item, encrypted to named members only.

- **Recipients.** `--to` names board members by instance id or by the label
  of a trusted peer; names the hub reports are never used. This node is
  always a recipient. The snapshot is sealed with
  [age](#encryption) to those members' instance keys, then stored under the
  board's own encryption. Every member sees that it exists, who published it
  and for whom; only the recipients can open it. Joining the board or a key
  rotation later never adds a recipient.
- **Confirmation.** Only the human operator can share. `share` shows the size,
  turn count, time range, credential scan, redactions, recipients and expiry,
  and publishes only after you type `share`. Non-interactively, pass
  `--confirm-digest` with the content digest a previous run showed. The
  confirmation is recorded as a consent record for those recipients
  ([Consenting to share](#consenting-to-share)).
- **Credentials.** A snapshot with suspected credentials is refused. Export
  it, run `tclaude paf redact`, and share the redacted file with `--file`. A shared file is named after its agent unless you pass `--name`.
- **Size.** At most 64 MiB once encrypted, stored as up to four Board blobs.
  Each blob counts toward the board's blob and byte quotas, and snapshots
  together may use at most half of the board's byte quota.
- **Expiry.** Every shared snapshot expires: after 7 days by default, at most
  30 (`--expires`). At expiry the hub stops serving it and deletes it, even
  if someone pinned it.
- **Removal.** `board rm` deletes the snapshot and its stored ciphertext from
  the hub; its publisher or a board owner may do it. Members who already
  fetched it keep their copy, and removing a member does not take back what
  they fetched.
- **Hubs.** Sharing needs a hub that supports board snapshots. An older hub
  is refused before anything is uploaded. An older tclaude shows a snapshot as
  an invalid item and cannot fetch it.

Receiving:

- `board ls` lists the board's snapshots and marks those addressed to this
  node.
- `board fetch` downloads, verifies and decrypts a snapshot addressed to this
  node, validates the PAF and scans it for credentials again. A snapshot with
  findings is not kept. A member who is not a recipient is told so and gets
  nothing to import.
- A fetched snapshot is kept decrypted under
  `~/.tclaude/data/federation/board-snapshots/` (mode 0700) until it is
  imported, it expires, or you remove it with `board rm --local` (which
  leaves the snapshot on the board). `board rm` without `--local` removes
  the local copy too.
- `board preview` shows the agent the import would create and prints a
  single-use preview token, valid for ten minutes. `board import` needs that
  token with the same options. The PAF is scanned again just before it lands.
- The new agent lands like `tclaude paf import --apply`: a new identity in
  the group you name, the receiver's profile and policy, and no permissions
  from the source. Its arrival briefing marks the received conversation as
  untrusted: context to read, not instructions to follow.
- These routes are human-only and are not served through peer view.

### Resilience snapshots

```bash
tclaude agent snapshot policy set reviewer --every 1h --to peer:bob --keep 5
tclaude agent snapshot policy set group:team --every 6h --to peer:bob --keep 3 --max-age-days 14
tclaude agent snapshot policy set reviewer --every 6h --to board:BOARD --recipient bob --keep 2
tclaude agent snapshot policy ls
tclaude agent snapshot policy run POLICY
tclaude agent snapshot policy rm POLICY
tclaude agent snapshot held            # on the peer: what it keeps for others
tclaude agent snapshot held --rm-peer alice
```

A resilience policy sends snapshots of an agent, or of every agent in a
group, to a trusted [federation peer](federation.md) that keeps them for
you, or puts them on a [Board](#sharing-a-snapshot-on-a-board), so a copy
survives if this node is lost.

- **Consent.** Only the human operator sets, runs or removes a policy.
  Setting one is your consent to send those agents' conversations to that
  peer, encrypted.
- **Encryption.** Each snapshot is sealed with [age](#encryption) to the
  peer's and this node's instance keys, so that it can be recovered when
  this node is lost. The peer's operator can therefore decrypt it, as with
  any recipient: choose a peer you would trust with the conversations.
  tclaude on the peer stores the ciphertext and never previews or imports
  it on its own; the generic offer routes refuse it. Only its operator can
  open it, by [recovering the agent](#recovering-an-agent) when this node is
  lost.
- **When.** At most every `--every` (at least 10 minutes), only while the
  agent is idle, and only when its conversation changed since the last
  snapshot the peer kept, or that snapshot is halfway to its maximum age, so
  an unchanged agent still has a copy that has not expired. A busy agent is
  tried again on the next check. tclaude does not learn when the peer drops
  a copy early (`held --rm-peer`, its size cap, a lost disk).
- **Credentials.** Suspected credentials are not removed or refused. Their
  count shows in `policy ls`, and you are told once per policy.
- **Failures.** A failed snapshot is retried with growing delays, up to six
  hours apart. `policy ls` shows the error; after three failures in a row
  you are told once, and again only after a snapshot succeeds and fails again.
- **The peer.** It must run a tclaude that advertises resilience snapshots
  and grant this node `snapshots.receive`, even when it trusts this node as
  unrestricted:

  ```bash
  tclaude federation grant alice snapshots.receive
  ```

  It keeps the newest `--keep` snapshots (1 to 20, default 5) of each agent
  for at most `--max-age-days` (default 30, at most 365), and at most 2 GiB
  for one sender, dropping the oldest first, and snapshots of at most 200
  agents per sender. A snapshot is at most 65 MiB. Revoking the grant stops
  new custody; `tclaude agent snapshot held --rm-peer alice` deletes what
  is already kept. Removing a policy stops the schedule; the peer's copies
  stay until its retention removes them.

- **A Board.** `--to board:BOARD` puts each snapshot on the board as a
  [shared snapshot](#sharing-a-snapshot-on-a-board), sealed to the members
  named with `--recipient` (at least one besides this node, which is always
  added) and published under the policy's consent. The board's rules hold:
  a snapshot with suspected credentials is not put on it (the policy reports
  a failure until they are gone from the conversation), it is at most 64 MiB
  once encrypted, and it expires after `--max-age-days`, at most 30 (the
  default for a board). After each snapshot, this agent's older snapshots
  beyond `--keep` are deleted from the board. Recipients fetch and import
  them like any shared snapshot.

#### Recovering an agent

```bash
tclaude agent snapshot held
tclaude agent recover --held OFFER --from peer:alice --group team [--cwd DIR]
tclaude agent recover --held OFFER --from peer:alice --group team [--cwd DIR] --preview-token TOKEN
tclaude agent recover --held OFFER --from peer:alice --group team --new-id
```

When a node that sent you resilience snapshots is lost, recover one of its
agents from a snapshot you hold. A board recipient recovers with
[`snapshot board fetch`, `preview` and `import`](#sharing-a-snapshot-on-a-board).

- **Human-only.** Only the operator previews or recovers.
- **Checks.** The held copy is decrypted with this node's key, must be a
  valid one-agent PAF signed by the instance that sent it, and is
  summarized again; suspected credentials are shown in the preview.
- **Preview first.** Without `--preview-token`, `recover` shows the agent
  that would land and prints a single-use token, valid for ten minutes and
  only for the same snapshot and options.
- **The agent keeps its id.** As with a teleport, the recovered agent keeps
  its agent id and this node becomes its home. It gets the group's policy and
  profile, no permissions from the source, and an arrival briefing that marks
  the conversation as received context, not instructions. `--new-id`
  recovers it as a new agent with a fresh id instead.
- **Conflicts are your call.** When keeping the id conflicts with this
  node's records, the preview lists why and recovers nothing until you
  choose `--new-id` or `--force-keep-id`. Conflicts are:
  - the agent exists here, or was retired here;
  - this node's records place it elsewhere (away, or with another home);
  - a newer snapshot of it is held.

  With `--force-keep-id` an existing agent's id heads the recovered
  conversation, and its earlier conversations become past generations. A
  running agent is never taken over: stop it first, or use `--new-id`.
  tclaude cannot tell whether the original still runs on a node it cannot
  reach. If that node comes back, two agents share the id until you retire
  one.
- The held copy stays until its retention removes it.

**Recalling snapshots to a rebuilt node.** A node rebuilt with its old
federation identity (for example from a backup of `~/.tclaude`) can ask a
peer for the snapshots it holds of this node's agents:

```bash
tclaude agent recover --recall agt_… --from peer:bob [--offer OFFER]
tclaude agent snapshot recalls
tclaude agent recover --recall-id RECALL --group team [--new-id | --force-keep-id] [--preview-token TOKEN]
```

- The peer returns a snapshot only to the instance it holds it for, and only
  one it holds: the newest of that agent, or the one `--offer` names. The
  peer must run a tclaude that can return snapshots.
- A recall is bound to the peer asked, the agent named, the snapshot asked
  for and an expiry of an hour. The first matching return binds it to that
  snapshot's digest. Any other return is refused, and a recall recovers at
  most one agent.
- The returned snapshot must be signed by this node and be of the agent
  recalled. Recovery then works as above: preview first, the id kept by
  default, and conflicts left to you. On a node whose records survived, an
  agent that still exists is a conflict, as is a newer snapshot this node
  sent.

## Exporting and importing a group

```bash
tclaude agent groups export team --out team.paf
tclaude agent groups import team.paf --dry-run --into "$PWD"
tclaude agent groups import team.paf --into "$PWD" --as team-copy
tclaude agent groups import team.paf --into "$PWD" --harness codex --model gpt-5.5 \
  --member-harness reviewer=claude --member-model reviewer=claude-opus-5-5
```

`groups export` writes the group as a group PAF.
- `group.json` holds the group's name, description and members with their
  roles.
- Each member is an agent carrying its own harness's native history.
- tclaude's own group state (settings, owners, permissions, enrollment, cron
  jobs, messages and audit trail) travels in the `tclaude` extension.

`--format zip` writes the legacy group export v5 instead, for importing into
an older tclaude. It carries Claude Code conversations only; members on other
harnesses export without history. The export is signed when this node has a
federation identity. Both formats need the same permission
(`groups.export`).

`groups import` and the dashboard import accept either file, with the same
permission (`groups.import`), preview, `--into`, `--as` and collision
handling. A group PAF is verified before anything is written. One written by
another tool imports the group's name, description and members, each with
its native history or neutral context.

Each member of a group PAF lands like an
[imported agent](#exporting-and-importing-an-agent):
- **Harness:** it lands on its own harness, unless `--harness` (every member)
  or `--member-harness <member>=<harness>` names another. A member is named
  by its title or agent key.
- **Model:** `--model` and `--member-model` set the landing model the
  conversation is budgeted for.
- **History:** it lands natively when the landing harness reads its format,
  and is translated otherwise, with the losses shown. A member whose
  conversation cannot land imports as configuration, with a warning. Its
  record still names its landing harness, so it resumes there.
- **OpenCode:** a member landing on OpenCode installs its conversation into
  this host's OpenCode store through the `opencode` CLI, which is where its
  first resume reads it, which depends on its [landing posture](#landing-posture).
  Without `opencode` installed it imports as configuration, with a warning.

A Claude Code member landing on Claude Code takes the group import's own
path, which rewrites embedded paths and keeps its conversation id unless it
collides. Every other landing goes through the harness's own history import,
which mints a new conversation id that the group's rows then follow. The
dry run shows each member's landing harness, model, rung and posture. Imported
members land offline and resume on their landing harness.

### Landing posture

An imported member launches under the sandbox a spawn into the landing group
would get: the spawn profile tiers, then the install default, composed with
the group and global sandbox profiles. A group import always creates its
group, so at import that means the global tiers and the install default.
Nothing in the file sets it, so a source that ran without a sandbox cannot
bring that here. A sandbox profile assigned to the group later binds the
member's next launch, like any other member's; its sandbox implementation
stays what the import resolved until `sandbox-impl set` changes it.

`--dry-run`, the import result and the dashboard preview state each member's
implementation, sandbox profiles and the tiers that chose them. A posture this
node cannot meet, such as tclaude's sandbox on a host without `bwrap`, is shown
as refused by the dry run and refuses the whole import, with the same reason a
spawn would give and nothing written. It never lands unconfined instead.

An OpenCode member's conversation goes in the store its server reads. Under
tclaude's sandbox that is the member's private per-agent state; under any other
posture it is the shared store, shared with other unsandboxed OpenCode agents.

Members imported by an earlier version are left as they are, running or not:
they keep the baseline posture, with no tclaude sandbox. Move one with
[`sandbox-impl set`](sandboxing.md#reassigning-an-existing-agent). An OpenCode
member moved onto tclaude's sandbox takes its conversation with it.

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
  context, native blob, text attachments and extension data for suspected
  credentials. Findings are counts and locations, never the matched text.
  - **Detectors:** API and GitHub tokens, private keys, keychain references,
    `token=`/`password=`-style assignments, AWS access key ids, and long
    random-looking tokens (`high-entropy-string`).
  - **Advisory kind:** `high-entropy-string` is reported but does not block
    anything on its own. Identifiers, hex digests, UUIDs and encoded media are
    not counted.
  - **Export:** findings of any other kind refuse the export unless you
    explicitly allow them (`--allow-flagged`). The allowance is recorded as
    `scan.allowed`. A group PAF export scans every member's history and the
    group's messages and settings too.
  - **Import:** import never trusts the producer's recorded scan. It scans
    again and shows its own findings in the preview.
  - **Inspect:** `tclaude paf inspect --scan` runs the same scan on any file.
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
- **Consent before it leaves the node.** Posting a snapshot to a Board,
  publishing, consenting with `tclaude paf consent` and setting a resilience
  policy are human-only, after an explicit confirmation. The confirmation
  shows size, time range, turn count, findings and redactions, and is
  recorded against the content digest. An agent can never post history to a
  Board or publish it on its own authority. Federation share, move and
  teleport keep their own [authority model](federation.md): the human, or an
  agent holding the grant (`agent.share`, `agent.move`, `self.teleport`),
  with the same credential scan and `--allow-flagged`.
- **Encrypted when stored off-node.** Snapshots stored on a Board or with a
  resilience peer are [encrypted](#encryption) to named recipients. A share,
  move or teleport hands the agent to a trusted peer over the authenticated
  federation link and is not separately encrypted; the receiving node holds
  it as it would any agent it imports.
- **Retention.** A PAF can carry an expiry (`retention.expires_at`), which a
  receiving tclaude honours for the copies it keeps. Shared snapshots default
  to a short retention. Copies that recipients already fetched cannot be
  recalled, and an expiry cannot force another implementation to delete its
  copy.

### Redacting a PAF

`tclaude paf redact` writes a redacted copy of a PAF, or a legacy agent
bundle or group export, and `--preview` shows what it would change without
writing anything:

```bash
tclaude paf redact reviewer.paf --mask-flagged --paths-to-placeholders --preview
tclaude paf redact reviewer.paf --mask-flagged --stub-tool-results -o reviewer.redacted.paf
```

| Flag | Redaction | What it does |
|---|---|---|
| `--mask-flagged` | `mask_spans` | Replaces every span the scan flags with `[REDACTED:<kind>]`. A PEM private key is masked from its header to its `END` line. |
| `--stub-tool-results` | `stub_tool_results` | Replaces tool results older than the last `--keep-turns` human turns (default 3) with a stub, and drops their media. |
| `--drop-media` | `drop_media` | Replaces images and documents with a stub. |
| `--drop-reasoning` | `drop_reasoning` | Removes thinking blocks and provider reasoning state. |
| `--drop-native` | `drop_native` | Drops the native blob only. |
| `--paths-to-placeholders` | `paths_to_placeholders` | Replaces recorded placeholder paths, each working directory and worktree, the home directory above them and the user name it is named after with `${PROJECT}`, `${HOME}` and `${USER}`. A path matches only as a whole path, the user name only as a whole word, and a user name shorter than four characters is left alone. Agent keys are identifiers and are kept as they are. |

The copy is a new file:

- Every redaction drops the native blob. An agent whose history exists only as
  a native blob first gets a neutral context decoded from it, so its history
  survives as the redacted context.
- Extension data other than tclaude's own, unknown context line types, and
  files that are neither JSON nor text are dropped and recorded as
  `drop_unredactable`.
- A thinking block whose text a redaction changed loses its reasoning
  signature, which no longer matches the text.
- The scan is redone on the output and recorded in `privacy.json` with the
  applied redactions. Each agent's loss report gets a `redacted` item per
  redaction applied to it, with the redaction kind as `detail`.
- Sensitivity, audience and retention carry over. Consent records and
  signatures do not: the copy is signed by this node when it has a
  federation identity.
- Rewriting needs every required feature and extension, as import does.

### Consenting to share

`tclaude paf consent` records your confirmation that a PAF may go to an
audience:

```bash
tclaude paf consent reviewer.paf --audience recipients --recipient inst_7c21 -o reviewer.paf
```

It first shows what would be shared, then asks you to confirm:

- the size and number of agents;
- the conversation's human turn count and time range;
- this node's own credential scan, not the producer's recorded one;
- the redactions already applied;
- the content digest.

Without a terminal, review the summary and pass `--yes`. The audience is
`self`, `recipients` (with one or more `--recipient` instance ids), `board`
(with `--board`) or `public`.

The consent record (`by`, `at`, `audience`, `content_digest`) is added to
`privacy.json`, and the audience and sensitivity are set to match. Every
consent record in a file is for its `privacy.audience`. The content entries
are copied byte for byte and the file is re-signed by this node. Any later
change to the content makes the consent stale. Redacting produces new
content, so consent never carries over to a redacted copy.

- **Bound to what you reviewed.** The daemon refuses the request when the
  file's content digest is not the one the summary showed you, for example
  because it was rewritten while you were reading.
- **Earlier records are kept only when this node vouches for them.** The
  summary lists any consent the file already carries. Those records are kept
  only when this node signed the file and they are for the same audience.
  Records anyone else wrote are dropped rather than re-signed.
- **Only the human operator can consent.** The daemon refuses the request
  from an agent, so an agent can never consent on its own authority.
  tclaude records the confirmer as `operator`.

A consent record is only as trustworthy as the signature over the manifest.
`tclaude paf inspect` shows each consent as current, as stale, or as
unverified when no signature from this node or a known peer covers it.

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

```bash
tclaude agent snapshot publish before-refactor --drop-reasoning -o reviewer.public.paf
tclaude paf publish reviewer.paf --stub-tool-results --keep-turns 5 -o reviewer.public.paf
```

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

How tclaude publishes:

- Only the human operator can publish; agents are refused. You see the
  redactions, the placeholders and the summary of what remains (size, turns,
  time range, scan), then type `publish`. Non-interactively, pass
  `--confirm-digest` with the content digest a previous run showed. Your
  consent is recorded for a `public` audience, naming `publisher` rather
  than you.
- Credentials the scan flags are masked. If the scan still finds any in the
  redacted copy, nothing is published.
- Identity goes in two steps. First the identifying fields are cleared: the
  agent's name becomes `agent` (tclaude names an agent after its
  conversation's title) and so does its key in entry paths; ids and titles,
  the instance, groups, lineage sources, the task (`definition.task`) and
  the workspace repository, branch, commit and worktree go. Then the ids,
  the original name and titles, the instance name and the group names are
  replaced with `[REDACTED:identity]` wherever they appear in text. The
  source must hold exactly one agent.
- Optional extra redactions: `--stub-tool-results` (with `--keep-turns`),
  `--drop-media` and `--drop-reasoning`.
- The publisher key is created on first use under
  `~/.tclaude/data/paf/publisher.key`, separate from the federation key.
  Every publication from this node is signed with it (`signer.kind`:
  `publisher`, `key_id` `pub_…`), so readers can link your publications to
  each other but not to your node. Delete the file to start a new
  pseudonym.
- What is not removed: the conversation text itself, apart from the
  credentials, paths, ids and names above. Ids of other agents and nodes
  mentioned in it stay. Names, hostnames, URLs, customer details
  or anything else you discussed are published as written. Read the
  redacted copy with `tclaude paf render` before you share it.

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
- Sizes are bounded. Every reader **must** accept at least 1 MiB for each
  JSON metadata entry, 16 MiB for a single JSON Lines record, 10,000
  entries and 1 GiB of uncompressed content; a writer that wants its file
  read everywhere stays within these. A reader may accept more: tclaude
  defaults to 1 MiB, 256 MiB, 100,000 entries and 2 GiB (configurable per
  node). Readers check both compressed and uncompressed sizes.
- ZIP64 is allowed and readers must support it: an archive with more than
  65,535 entries needs it. Readers bound the entry count the central
  directory declares by what it can actually hold before allocating
  anything, and refuse a mismatch.
- An entry is data. A reader never writes an entry to a path derived from its
  name.

### Layout

```text
manifest.json                         format, version, kind, entry hashes
privacy.json                          scan, redactions, consent, retention   (optional)
group.json                            group configuration                    (kind "group")
workflows/<key>/workflow.json         a carried PWF workflow                 (optional)
workflows/<key>/README.md             its readme                             (optional)
workflows/<key>/templates/<id>/<hash> its templates, addressed by hash       (with workflow.json)
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
has exactly one agent; a group PAF has one per member; a bundle PAF has one per
performer. Attachments are shared by every agent in the archive. A
`workflows/<key>/` tree has the [PWF layout](pwf.md#layout) and passes the same
checks and limits as a `.pwf`.

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

- `kind` is `agent`, `group` or `bundle` (workflows and the agents that perform
  them, with no group). A reader that opens a `bundle` as one agent must be told
  which.
- `agents` lists the agent keys present, in order.
- `workflows` lists each carried workflow as `{key, root, requires, digest}`:
  its key under `workflows/`, its root template ref, what it requires, and a
  digest over its entries.
- `links` says how entries relate, as
  `{type, workflow, agent, role?, digest}`. `performer` means the agent performs
  that role of the workflow, and `carries` means the agent's file carries the
  workflow. The links are flat manifest data rather than fields inside agents
  and workflows, so that they cannot create a hash cycle. A reader skips a link
  `type` it does not know.
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

- `harness`: `name` (`claude`, `codex`, `opencode`, `copilot`, `gemini`, `pi`,
  or another harness's name) and `version`.
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
- `placeholders`: named path placeholders the importer remaps. Each has a
  `name`, the source-side `original` value (omitted when paths were
  redacted) and an optional `description`. Substitution follows these
  rules:
  - A token is `${NAME}`, where `NAME` is one or more of `A-Z`, `a-z`, `0-9`
    and `_`.
  - Tokens are substituted only inside JSON string values (never keys or
    JSON syntax) of `agent.json` and `environment.json`, including their
    extension data.
  - `HOME` always resolves to the importing user's home directory. Any other
    name resolves only when `placeholders` declares it; an undeclared token
    is reported to the importer as unresolved rather than guessed.
  - There is no escape sequence in 0.1, so writers must not put literal
    `${NAME}` text in those fields.
  - Transcript and native content is history and is never rewritten on
    import, so a `${NAME}` there is either redaction output (recorded in
    `privacy.json`) or literal text.
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

- `role` is `user` or `assistant`.
- `origin` says who produced the entry:
  - `human`: text the operator typed, and nothing else;
  - `agent`: the agent itself;
  - `tool`: tool results, which travel as `user` entries as in every native
    format;
  - `system`: text a harness or orchestrator injected, such as compaction
    summaries and tclaude notices;
  - `peer`: another agent's message, with `author` naming it.

  `origin` is an open set. A reader treats an origin it does not know as
  untrusted input that is not the operator's, and when it lands the
  conversation it labels such text so the model does not take it as the
  operator's instructions. tclaude does the same for `peer` text.
- `model` names the model that produced an assistant entry. `author` names a
  non-primary speaker, such as another agent or a sub-agent.
- `blocks` have a `kind`:

| Kind | Fields |
|---|---|
| `text` | `text` |
| `thinking` | `text` (readable reasoning, may be empty), `opaque` (`vendor`, `data`: provider-bound reasoning only that vendor can use) |
| `tool_call` | `call_id`, `name`, `input` (JSON arguments) or `text` with `freeform: true` |
| `tool_result` | `call_id`, `text`, `is_error`, `media` |
| `media` | `media` |
| `summary` | `text`: a compaction summary standing in for earlier history |

A media item has exactly one of `attachment` (`sha256:<hex>`, naming
`attachments/<hex>`), `url` or `path`. A `path` is a hint about where the
file lived on the source machine, not a reference into the archive.
`media_type` is optional but writers should set it; a reader that needs it
and finds none sniffs the bytes or treats the item as an opaque file. A
`name` is optional. Writers should embed media as attachments rather than
`url` or `path`: a URL may stop resolving and a path names another machine,
so neither keeps the conversation readable on its own.

`scope` is `effective` for `transcript.jsonl`. `full.jsonl` uses the same
line schema with scope `full`. It may also contain `boundary` lines
(`kind`: `compaction`, `clear`, `rewind` or `branch`) marking where the
effective context diverges from the full history. A boundary may name a
`parent`: the `id` of the entry the history continues from after it, so a
reader can rebuild the branches. Without `parent`, the history continues
from the entry just before the boundary.

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
- an `items` list of `{kind, severity, count, detail}`;
- `source` and `target`: the harness the conversation was decoded from and,
  when the report describes a landing or translation, the harness it was
  written for;
- `dropped_percent` and `kept_percent`: the share of source entries left out
  and carried. They are whole percentages that sum to 100 when both are
  present, with `dropped_percent` rounded up so that any loss shows;
- `prior`: the reports of earlier translations of the same conversation,
  oldest first, so successive morphs and imports add a report rather than
  overwrite the last one. Each item has the fields of `losses.json` itself,
  without its own `prior`. A writer that replaces a context with a new
  translation moves the old report into `prior`.

tclaude stores the report of the conversion that produced a conversation: a
cross-harness morph, or an import that lands it. A clone with its history
keeps the source's report, and so does a group clone. A federation offer, move
or teleport carries the report in the PAF, with only harnesses, kinds,
severities, landing labels and numbers: `detail`, `context_window_source` and
extensions stay on the sending node, and the receiver drops them too. Snapshots
are written the same way, since they can reach another node. A peer on bundle
v1 has no `losses.json`, so the record stops there. The next morph,
import landing or export then puts that report and its `prior` into the new
report's `prior`, keeping the latest 32. A conversation that lands unchanged
keeps the report it came with. An export with no neutral context, which
includes every group PAF, still writes `losses.json` for a conversation with a
stored report, as that report unchanged. tclaude also keeps `prior` when it
rewrites a PAF, for example when redacting. `tclaude paf inspect` shows how many
earlier reports a file carries.

A clone does not take its source's report, and neither does a federation
move, teleport or offer landing, which hands over a decoded bundle.

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
- `redactions`: applied redactions as `{kind, count, detail}`. Kinds are an
  open set; tclaude writes `mask_spans`, `stub_tool_results`, `drop_media`,
  `drop_reasoning`, `drop_native`, `paths_to_placeholders`, `strip_identity`
  and `drop_unredactable` (content dropped because it could not be rewritten
  safely).
- `consent`: human confirmations, each with `by`, `at`, `audience` and
  `content_digest`.
- `retention`: `expires_at`.

The `content_digest` binds consent to content without a circular hash. It is
the lowercase hex SHA-256 of the lines `<sha256>  <path>\n` for every manifest
entry except `privacy.json`, sorted by path in byte order. It covers the
content of every entry, and so all extension data inside entries and every
`ext/` entry. It does not cover `manifest.json` itself, so the manifest's own
`extensions` object and other manifest fields are outside it; a signature
covers those. Content that consent must bind belongs in an entry, not in
the manifest's extensions.

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
by the lowercase hex SHA-256 of `manifest.json`. `public_key` and `signature`
are standard base64 with padding (RFC 4648 section 4, not the URL-safe
alphabet). `key_id` is a label of 1 to 128 characters from `A-Z`, `a-z`,
`0-9`, `.`, `_` and `-`, chosen by the signer and not derived from the key:
tclaude uses the instance id for an instance key and `pub_` plus the first
16 hex digits of the SHA-256 of the public key for a publisher key. tclaude signs with the
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

```bash
tclaude paf encrypt reviewer.paf --to lab -o reviewer.paf.age   # a trusted peer by id, name or label
tclaude paf encrypt reviewer.paf --to age1… -o reviewer.paf.age  # any age recipient
tclaude paf decrypt reviewer.paf.age -o reviewer.paf
```

- **`encrypt`** encrypts a valid PAF to each `--to` recipient and to this
  node. It needs this node's federation identity.
- **`decrypt`** decrypts with this node's identity and verifies the result.
  A file this node is not a recipient of is refused.
- `validate`, `inspect`, `render` and `redact` read an encrypted PAF
  addressed to this node directly. Decrypt a file before importing it. Their
  output (a rendered document, a redacted PAF) is plaintext; encrypt a
  redacted copy again before it leaves the node.
- `--to` names a trusted peer by instance id or the label you gave it, never
  by the name the peer advertises, so a peer cannot claim a name meant for
  another.

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

PAF does not carry, by design:

- tool definitions and their schemas: the landing harness brings its own
  tools, and a tool is named only by reference;
- system prompts and instruction files (`CLAUDE.md`, `AGENTS.md` and the
  like): they belong to the workspace or the harness, not to the agent, and
  the landing harness loads its own. The agent's own role guidance and
  startup context travel in `agent.json`.

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
  extensions documented on this page, and a writer must not mint any other
  single-label id. Anyone else uses a reverse-DNS id they control, such as
  `com.example.agentlab`. A reader treats an undocumented single-label id
  like any unknown extension: it ignores it, and refuses the file only when
  `extensions_required` lists it.
- **`version`:** the extension's own format version, `MAJOR.MINOR`, with the
  same rules as `paf_version`: minor versions only add. A reader that knows
  the extension but not its major version treats it as unknown.
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
| `group.json` members | `owner` | The member owned the group. A group import keeps it as member structure; elsewhere it is advisory. |
| `ext/tclaude/mail-deliveries.json` | entry | The mail delivery ledger a continuation carries. |
| `ext/tclaude/group-state.json` | entry | A group's tclaude state in the group export's shape: members, owners, permissions, enrollment, cron jobs, messages, audit trail and per-conversation metadata. Each conversation's harness and history live in its agent. |
| `manifest.json` | `converted_from` | The legacy carrier a PAF was converted from: `format`, `format_version`, `tclaude_version`, `created_at`. |

The extension is `copy: unsafe`, version `0.1`.

## Versioning and compatibility

`paf_version` is `MAJOR.MINOR`, compared the way semantic versioning
compares versions.

### Compatibility policy

PAF aims to stay backward and forward compatible from 0.1 on. Until 1.0 this
is best effort rather than a guarantee, and any break is marked clearly.

- **Minor versions only add**, from 0.1 on: new optional fields, new
  optional entries, new values in enumerations documented as open, or new
  `requires` features. A reader reads every minor version of its major
  version, older or newer, ignoring what it does not know and still
  honouring `requires` and `extensions_required`.
- **A deliberate break is marked with `min_reader_version`** in the
  manifest: the oldest PAF version whose reader can import the file. A
  writer sets it only when the file cannot be read correctly by an older
  reader. A reader older than that refuses to import or resume the file,
  with a message that names the version it needs ("this file needs PAF >=
  0.3"). Inspection and rendering still read it best effort and warn that
  parts may be missing or misread.
- **A major version** may change anything. A reader refuses to import a
  file of another major version. Inspection and rendering read it best
  effort, with the same warning, and always show `format`, `paf_version` and
  `producer` so the user knows what to upgrade.
- **Read retention.** Once a version is published, later tclaude releases
  keep reading it: natively, or by converting it to the current model on
  read. A newer tclaude never drops the ability to open a file an older one
  wrote.
- **Federation** sends PAF only in a version both sides read: each instance
  advertises the list of PAF versions it reads (`paf_versions`), the sender
  writes the highest version both share, and falls back to the legacy
  carrier when they share none.

| Version | Breaking? | Changes |
| --- | --- | --- |
| 0.1 | (first version) | The initial format. |
| 0.2 | Only for files that carry workflows | Adds the [envelope](#carrying-workflows): `workflows`, `links`, `workflows/` entries and the kind `bundle`. Such a file sets `min_reader_version` to 0.2, because an older reader would silently lose its workflows. A file with agents only is still written as 0.1. |

- Readers **ignore unknown fields** and any listed entry they do not
  understand, wherever it sits. Writers must not rely on readers understanding
  a field or entry unless it is listed in `requires`.
- **Open enumerations** may gain values in a minor version, and readers
  accept unknown values. These are block `kind`, JSON Lines `type`, loss
  `kind` and `severity`, lineage `op`, boundary `kind`, redaction `kind`,
  audience `kind`, `sensitivity`, snapshot `state`, tool `kind`, entry
  `origin` (with the reading rule above), `signer.kind`
  and signature `alg`. The schemas list the known values without restricting
  them. A reader renders an unknown block kind or line type as a visible
  "unsupported content" placeholder that keeps its `text` when present, and
  never drops it silently. A writer that needs a reader to understand a new
  value lists a feature in `requires`.
- **Closed enumerations** change only in a major version, or in a minor
  version that sets `min_reader_version`: `format`, `kind`
  (`agent`, `group`, `bundle`), entry `role` (`user`, `assistant`), and the
  transcript `scope`.
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

### requires and extensions_required

`requires` names base-format features a reader must support, and
`extensions_required` names vendor extensions that change the meaning of
base data. Either belongs on a file only when a reader that ignored it would
silently produce a wrong result, not merely a poorer one. For example:

- the context is stored as a delta against an earlier snapshot (a `requires`
  feature): read on its own, it looks like a short conversation;
- the context is encoded or encrypted inside an extension, and the base
  context is a placeholder;
- the real history lives in an extension, and the base holds only a stub.

Writers should almost never set either. tclaude's own extension never does:
everything in it is additional, and a reader that ignores it loses only
tclaude-specific state. A reader refuses to import, resume or rewrite a file
that lists something it does not know, but can still inspect and render it,
with a warning.

The schemas for version 0.1 live under `docs/paf/schema/v0.1/`, and their
`$id`s include `v0.1`. They allow unknown properties, so they validate later
0.y files too; a later minor version that needs new schemas adds a
directory. From 1.0, one directory per major version (`v1/`)
validates every file of that major version, because unknown properties are
allowed. A few rules are semantic rather than structural, and
a reader checks them in code:

- entry hashes and sizes match the manifest;
- no entry name contains a `.` or `..` segment;
- every agent key listed in the manifest has an `agent.json`;
- `agent.native.path` lies under that agent's own `agents/<key>/native/`;
- every `attachment` reference resolves to an entry;
- every extension id used in an `extensions` object, an `ext/` entry or
  `extensions_required` is declared in `extensions_used`, found by decoding
  the JSON (a key spelled with escapes counts) rather than by searching its
  bytes;
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

- [Federation: portable local agent bundles](federation-moving.md#portable-local-agent-bundles)
  and [sharing an agent with a peer](federation-moving.md#sharing-an-agent-with-a-peer)
- [Harnesses: cross-harness history translation](harnesses.md#cross-harness-history-translation)
- [Spawning and lifecycle: morph and clone](spawning-and-lifecycle.md#morph)
- [Federation: content boards](federation-boards.md#content-board-membership)

### Managed instructions

`tclaude` carries its explicitly managed agent skills and rules as hashed
`attachments/<sha256>` entries. A version 1 resource index on `agent.json`, under
`extensions.org.tclaude.managed-instructions`, lists each item's kind, name,
content digest and byte count. The required extension is version `1.0`; readers
that do not support it refuse import. Ordinary PAFs do not require it.
`environment.skills` lists the carried skill names and attachment digests.
The content remains agent-scoped and confers no permissions or library writes.
Receiving code-bearing skills requires a separate human decision bound to the
verified resource digests; trusted-peer or automatic landing policy is insufficient.
