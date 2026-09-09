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

Start with **Agent**, its settings and selected harness. The integration manages
the link needed to continue the agent's native work. Conversation and Execution
were names in the stopped v2 attempt, but we are not adopting them as core
entities. Their usefulness has not been established by a user need.

| Concept | Meaning to the user | What tclaude controls, and its limit |
|---|---|---|
| Agent | Who I am working with | Owns identity, settings, harness selection and membership. The integration manages native continuation, including changes of native IDs. |
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

## Native continuation belongs to the integration

An Agent selects a harness and is associated with integration-managed continuation
state. The core does not interpret its contents. An integration may need one
native ID, several references, or other persisted state. Those details may change
without changing the Agent's identity.

As the operator describes it, Claude Code calls its resumable chat a session. It
can preserve an ID across stop/resume and replace or clone IDs on clear or other
transitions. The integration handles that behavior. The platform must not infer
what happened merely by comparing native IDs.

The core model does **not** contain chat history, a history-access entity, or a
Conversation entity. Any native history reading or manipulation needed by an
operation belongs to the harness or its integration. This is not a proposal to
remove existing history-related UI features; it defines where their native
mechanics belong, without requiring a platform history model or store.

## Clone and reincarnate are operations, not history models

Cloning an agent must carry out the requested native duplication as well as the
appropriate tclaude changes. Copying an Agent record or sharing a continuation
reference is not enough. The integration performs the native work and supplies
an independent association when that is required by the clone contract.

Reincarnation likewise delegates its native continuation or replacement steps.
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
