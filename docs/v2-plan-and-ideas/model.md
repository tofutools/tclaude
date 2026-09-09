# The user-facing model

The model should explain the product without requiring users to understand a
harness's internal process or identifier scheme.

| Concept | What it means to the user |
|---|---|
| Agent | A persistent identity to configure, contact and work with |
| Group | Agents collaborating, with memberships, owners and shared defaults |
| Profile | Named reusable settings, including harness-specific choices |
| Running session | The agent's current native execution; it may stop or be replaced |
| Conversation | History the user can find and continue working with |
| Message | Communication with a recorded delivery outcome |
| Team, process or automation | Saved instructions for coordinating or repeating work |
| Activity | What is known about ongoing work, including usage and failures |

These are conceptual responsibilities, not a replacement database schema.
Standalone sessions and later registration as an agent must remain possible.
A terminal is a view into execution, not the identity of the agent.

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

Old-session reports must remain attributable to the old session. They must not
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
