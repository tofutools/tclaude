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

| Concept | Intended meaning and change |
|---|---|
| Agent | The identity users configure, contact and keep working with. Build on the existing durable agent ID; stop treating native identity as interchangeable with it. |
| Conversation/history | Make the relationship between user-visible history and native resumable conversations explicit. Whether these need separate named objects is still open; do not silently redefine the current conversation record. |
| tclaude session / running window | Keep this distinct from native resumable state. Define the window's lifetime and associations explicitly; any rename is undecided. |
| Harness session or conversation | Preserve the native unit and its semantics inside the integration, with explicit associations to the agent, history and running window. Do not assume one shared lifetime or a permanent one-to-one mapping. |
| Group | Keep collaboration, membership, owners and defaults explicit; no new product concept proposed here. |
| Profile | Keep named reusable settings and harness-specific choices. Separate saved choices from settings used by a particular execution. |
| Message | Keep user-visible communication, with clear ownership of addressing and delivery outcomes. |
| Team, process or automation | Keep existing user workflows; coordinate shared operations where behavior matches. |
| Activity | Present attributable readings with their meaning, age and uncertainty, rather than treating all native values as equivalent. |

This is a proposed separation of responsibilities, not a replacement schema or
an approved naming scheme. Starting an unregistered tclaude session and later
registering an agent must remain possible. The relation between a running
window and native execution needs explicit treatment, not the earlier draft's
assumption that “session” simply means native execution.

The following sections describe proposed rules, not claims that main already
implements a uniform model.

## Common concepts do not require identical harness settings

A profile can expose different approval modes, context settings or other options
for different harnesses. Preserve their meaning through authoring, storage and
execution. Capabilities explain what can be selected; permission checks decide
whether the caller may do it. Unsupported choices need a useful explanation.

Saved profile settings, settings used for an execution, and observed native
state are different facts. Operators must remain able to edit profiles while
agents run. How a later operation resolves those settings belongs in that
operation's explicit contract, not a blanket rule that freezes every object.

## Stable identity, changing native identity

An agent can keep its tclaude identity while a harness resumes a conversation,
replaces its native ID, or announces a new ID later. Keep the association and
history needed to explain that transition. Do not make a single native ID the
universal identity of the agent.

Reports from an old native execution must remain attributable to that execution. They must not
silently become the current agent's status after a restart.

## Observations describe what is known

Context information might be a token count, a percentage, an estimate or absent.
A useful reading records:

- The value and its meaning, including capacity if known.
- Which execution or conversation it describes.
- Its source and observation time.

Do not derive a precise token count from an unknown capacity or treat missing
information as zero. Show differences and uncertainty when necessary. The same
principle applies to activity, cost and native completion reports.

The shared model captures meaning useful to the operator. Harness-specific
protocol details stay with the integration unless the user needs to see them.
