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

Names such as **Agent**, **Conversation**, **Execution** and **Operation** appeared
in the stopped v2 implementation. They are useful vocabulary to reconsider,
not a commitment to that implementation's schema, policies or guarantees.
The source inspected was `internal/backend/model/model.go`, `ids.go` and
`context.go` at v2 commit `0cc125fa221855ccde0341bb3c50278768df3691`.

| Concept | Meaning to the user | What tclaude controls, and its limit |
|---|---|---|
| Agent | Who I am working with | Owns identity, configuration and membership; does not control every native lifecycle event. |
| Conversation | A thread of interaction I can revisit and continue | Owns the thread's identity and organization of available history. Continuing the thread does not guarantee the harness can restore its previous context. |
| Execution | One attempt to run an agent or other workload | Owns attempt identity and requested lifecycle; actual readiness, exit and failure must be observed. A native ID change alone does not define a new execution. |
| Terminal/window | Where I interact with running work | Owns the view and its association with execution. It is neither the conversation nor the native resumable unit. Exact replacement terminology for today's tclaude session remains open. |
| Operation | A request to do something, with progress and an outcome | Owns coordination and records the outcome; cannot promise a requested external effect has happened before it is confirmed. |
| Group | Who works together | Owns membership, owners and defaults. |
| Profile | Settings I want to reuse | Owns saved intent, including explicit harness-specific options; the selected harness validates and applies supported choices. |
| Message | Communication I send or receive | Owns recorded communication and delivery tracking; distinguishes acceptance from confirmed native delivery. |
| Team, process or automation | How I arrange or repeat work | Owns coordination of operations, subject to each operation's capabilities and permissions. |
| Activity | What is known about the work | Owns presentation and attribution; availability and precision depend on observations. |

The future table deliberately has no native-session entity. Harness IDs,
native history formats and replacement rules belong behind the integration
boundary. The platform model refers to tclaude identities, not native ones.
An execution may exist without a registered agent, preserving standalone work
and later registration.

These are proposals, not current structures. Existing concepts must be mapped
and migrated deliberately; neither current conversation records nor session
records can simply be renamed into this model.

## What stays behind the harness boundary

Each integration keeps the bindings it needs between tclaude identities and
native resources. Those bindings may change, be discovered later, or involve
several native references. They may need durable storage for recovery, but they
are integration state, not additional concepts in the user-facing model.

For example, as the operator describes it, Claude Code calls its resumable chat
a session. It can retain an ID across stop/resume and replace or clone IDs on
clear or other native transitions. The integration must understand those events.
Other harnesses may need entirely different tracking.

The integration translates the result into meanings tclaude needs: execution
ready or stopped, continuation available or unavailable, context reset, history
available or incomplete. Common operations consume those meanings, not native
ID comparisons or harness-specific event names.

A native ID changing does not automatically create a new tclaude Conversation.
Nor does an unchanged ID prove context was retained. Whether a user action such
as clear continues the thread with a reset marker or creates a new thread is a
separate product decision, still open here. Do not derive it from native naming.

## A stable thread does not promise identical context

The user can continue working with the same tclaude conversation even when the
native mechanism changes, provided the operation can do so under its declared
behavior. If native continuation is unavailable, report that limit. Do not
silently start fresh and claim a successful resume, or replay history into the
harness as if it were equivalent. Any alternative requires an explicit product
rule or user choice.

Best effort means adapting accurately and reporting what could not be achieved.
It does not weaken permission checks, retry protections or the meaning of a
successful operation. Available history is not a promise of a complete archive.

## Harness-specific settings are an extension, not the core model

Users can still choose a harness and its specific settings. The integration
provides their definitions and validation; tclaude stores the authored choices
and displays suitable controls. These explicit options do not make native
session IDs or lifecycle quirks part of Agent, Conversation or Execution.

Saved profile settings, execution settings and observed native state remain
different facts. Operators must be able to update profiles while agents run.
Later operations apply their documented resolution rules; this proposal does
not introduce a blanket freeze of saved objects.

## Observations describe what is known

The integration translates context information into meaningful readings: counts,
percentages or estimates, with known capacity, age and quality where available.
Shared code need not know which native payload produced them. Unknown capacity
must not become a fabricated token count; missing information is not zero.

Correlating late reports with old native resources is the integration's job.
Only correctly attributed platform observations should update an execution or
conversation. Diagnostic native details can remain available for troubleshooting
without becoming required knowledge for ordinary users or common operations.
