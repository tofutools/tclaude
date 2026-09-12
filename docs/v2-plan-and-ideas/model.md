# The user-facing model: current and future

The current vocabulary grew over time; it is not a clean version of the proposed
model. In particular, **Claude Code's session and tclaude's session are different
concepts**. The history below comes from the operator's account; the current
implementation already includes later additions such as stable agent IDs.

## Current

| Concept | Current meaning and history |
|---|---|
| Agent | Originally identified by the conversation ID taken from Claude Code. Main now also has a durable `agt_…` identity and links to conversation generations; that later addition does not make all surrounding concepts consistent. |
| Conversation | Originally Claude Code's own session ID, used as the unique agent identifier. Claude Code can stop and resume that session. tclaude's conversation records and history handling grew around that native identity; this is not yet a clean, independent history abstraction. |
| tclaude session | A tclaude-owned concept identifying an actual running window, with its own ID, tmux/process state and a conversation association. It is **not** a harness session. |
| Harness session or conversation | Native terminology and lifecycle: Claude Code calls its resumable unit a session; other harnesses expose their own identifiers and transitions. |
| Group | Existing collaboration and membership records, with owners and defaults. |
| Profile | Existing reusable launch or sandbox settings, including harness-specific fields. |
| Message | Existing mailbox communication and delivery handling. |
| Team, process or automation | Existing templates and mechanisms for coordinating or repeating work. |
| Activity | Existing readings and status assembled from native observations and tclaude runtime state. |

Main evidence: [agent and conversation records](../../pkg/claude/common/db/agents.go),
[session records](../../pkg/claude/common/db/sessions.go), and
[window/session state](../../pkg/claude/session/session.go). These show current
structures, not proof that their vocabulary or lifetimes are already settled.

## Future (proposed)

Start with **Agent**: a stable identity, its harness ID, intentional
generations with a current-generation pointer, a per-generation continuation
association, requested and resolved startup configurations, and last-known
extracted metadata. The integration manages the native meaning of each
association (see [Generations and native continuation](#generations-and-native-continuation)).
Conversation and Execution were names in the stopped v2 attempt, but we are
not adopting them as core entities. Native behavior alone does not justify them.

| Concept | Meaning to the user | What tclaude controls, and its limit |
|---|---|---|
| Agent | Who I am working with | Owns stable identity, harness ID, intentional generations and the current-generation pointer, requested/resolved startup configurations, last-known metadata and membership. The integration owns each generation's native association, including changes of native IDs. |
| Terminal/window | Where I interact with running work | Owns the view and its association with the agent or standalone work. It is not the harness's resumable chat. Naming beyond today's tclaude session remains open. |
| Group | Who works together | Owns membership, owners and defaults. |
| Profile | Settings I want to reuse | Owns saved intent, including harness-specific options; the integration validates and applies them. |
| Message | Coordination mail I send or receive | Owns tclaude mailbox communication and delivery tracking. This is separate from native chat history. |
| Team, process or automation | How I arrange or repeat work | Owns coordination of operations, subject to permissions and reported support. |
| Activity | What is known about the work | Owns presentation and attribution; native observations determine available readings. |

Operations act on these concepts. Tracking operation progress or a running
attempt internally does not require a new user-facing entity for each.
Standalone work and later agent registration must remain possible; the internal
representation is still to be designed.

## Agent details

### Current

| Detail | Current organization |
|---|---|
| Harness selection | Existing harness fields in launch and session state |
| Generations and current pointer | The agent row's current conversation ID plus succession edges between native conversation IDs (`db.RotateAgentConv`). Every recorded native ID rotation, whether intentional reincarnation or Claude Code `/clear`, becomes a new link; a platform generation and a native ID change are not distinguished |
| Continuation association | Native conversation IDs, agent/conversation mappings and lifecycle handling accumulated over time |
| Requested and resolved startup configurations | Existing profiles, launch resolution and persisted launch/session fields; not presented here as one already-clean Agent structure |
| Last-known metadata | Existing session/status fields and native observations, including model, context and usage |

### Future (proposed)

| Agent detail | Intended meaning |
|---|---|
| Harness ID | Which harness integration handles this agent |
| Generations | Intentional stages of the Agent's work, known to tclaude. Not native IDs and not created by native ID changes |
| Current generation | Which generation is current; operations state which generation they intend to address |
| Per-generation harness association | What the integration needs to continue that generation's native work. It may cover several native refs over time; its contents, native ID changes and prior refs remain private to the integration |
| Requested startup configurations | What was requested, across the relevant kinds of settings; preserve explicit choices and inheritance intent |
| Resolved startup configurations | What those requests resolved to for startup, distinct from both the request and later observations |
| Last-known extracted metadata | Information the integration reported about native work: context window, usage, model and other useful readings |

The current configuration row splits into requested and resolved rows to make
that distinction explicit. These are parts of Agent state, not a requirement
that all data live in one database row or one Go struct. Whether configuration
and metadata belong to the Agent, to a generation, or partly to each is still
to be explored.

Requested model, resolved startup model and last-observed model may differ.
Retain those meanings rather than overwriting them with one ambiguous value.
Resolved startup settings do not prove the harness actually applied them;
observations provide separate evidence. Later profile edits must remain allowed.

Metadata is a last-known observation, not necessarily live truth. Retain its
source, observation time and known/unknown status where applicable. Different
readings may have different freshness. The integration handles native parsing
and correlation; the core stores and presents meaningful metadata without
reading or modeling chat history. An unsupported reading is not zero.

## Generations and native continuation

### Current

| Aspect | Current behavior |
|---|---|
| What advances an agent | `db.RotateAgentConv` links a new native conversation ID and advances the agent's pointer. It runs for reincarnation and for Claude Code `/clear` alike |
| Detecting native changes | Shared session hook code interprets Claude hook sources (`clear`, `resume`, `compact`) and, for the unannounced remote-control handoff, scans the transcript head for lineage |
| Prior references | Succession edges and `conv_index` rows keyed by native ID; archived predecessors are marked by title suffix and `conv_index.archived_at` |
| Unrelated native work | Listed from harness stores and `conv_index`; tclaude can archive, title and index it without an Agent |

### Future (proposed)

| Aspect | Intended behavior |
|---|---|
| What advances an agent | Only an intentional platform operation changes generations. Reincarnation creates a new generation and moves the current pointer. Séance, its sister operation, addresses an earlier generation without moving the pointer |
| Detecting native changes | Native replacements such as Claude Code `/clear` are harness-level management: the integration updates the association and references of the generation its binding identifies, which may be an earlier, non-current one. No generation is created and the current pointer does not move |
| Prior references | The integration keeps prior associations durably so that previously managed work is recognised later |
| Unrelated native work | Discovery is an integration result. Recognised references are attributed to their Agent and generation; others are candidates. Conversation archiving is not part of the agent-accessible model |

An integration may need one native ID, several references, or other persisted
state per generation. Those details may change without changing the Agent's
identity or its current generation.

As the operator describes it, Claude Code calls its resumable chat a session. It
can preserve an ID across stop/resume and replace or clone IDs on clear or other
transitions. The integration handles that behavior. The platform must not infer
what happened merely by comparing native IDs. If the integration cannot
establish continuity confidently, for example a copied transcript versus a
native replacement, it reports the ambiguity instead of choosing.

The core model does **not** contain chat history, a history-access entity, or a
Conversation entity. Any native history reading or manipulation needed by an
operation belongs to the harness or its integration. Native IDs remain
available to operators for diagnostics. This does not remove existing
history-related commands or UI on main, and it does not claim that their
future mapping is solved; it defines where native mechanics belong.

## Clone and reincarnate are operations, not history models

Cloning an agent must carry out the requested native duplication as well as the
appropriate tclaude changes. Copying an Agent record or sharing a continuation
reference is not enough. The integration performs the native work and supplies
an independent association when that is required by the clone contract.

Reincarnation is the intentional generational change: it creates a new
generation, moves the current pointer once it commits, and delegates the native
steps. Séance is its agent-level sister operation: it addresses an earlier
generation of the same Agent, leaves the current pointer unchanged, and
likewise delegates reaching that generation's native continuation to the
integration.
Each operation needs its own user-visible contract; do not make clone and
reincarnate synonyms or decide their identity behavior from native ID changes.
The common operation coordinates permissions, settings, platform changes and
outcome. It does not read transcripts to implement these operations.

## Desired behavior first, harness support second

Define what tclaude should do for the user, then map it to each harness. Do not
choose the minimum feature set shared by all harnesses. An integration reports
whether it can fulfil the requested behavior, offers an explicitly described
alternative, or reports it unsupported. No silent downgrade should masquerade
as successful resume, deep clone or reincarnation.

Best effort concerns the mapping to native behavior, not permission checks or
the truthfulness of the outcome. If a native operation may have happened but
cannot be confirmed, report uncertainty rather than blindly repeating it.

## Settings and observations

Users can select harness-specific options through definitions and validation
provided by the integration. tclaude preserves authored choices without making
native lifecycle semantics part of the Agent model. Operators can update
profiles while agents run; later operations apply documented resolution rules.

The integration can translate context usage or activity into meaningful
readings. This is status information, not a chat-history model. Missing values
remain unknown, and late reports from old native work must not overwrite the
agent's current status. Native correlation stays behind the integration boundary.
