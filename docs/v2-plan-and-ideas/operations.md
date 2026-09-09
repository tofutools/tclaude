# Operations coordinate user requests

In the proposed architecture, an operation answers a request such as “restart this agent” or “send this
message.” It owns the sequence, the shared application rules and the outcome
presented to the caller. Dashboard, CLI and automation reuse the same operation
where they request the same behavior, retaining their actual caller identity.

## Current

| Part of a restart/lifecycle operation | Current organization |
|---|---|
| Application rules | Existing daemon lifecycle paths coordinate checks and effects |
| Harness differences | Existing harness lifecycle implementations and daemon/native handling participate in the sequence |
| Identity | Agent IDs, native conversation IDs and tclaude session/window IDs are distinct records, with historically accumulated associations |
| Later observations | Native event handling updates state after the initiating call; this is not a single synchronous function result |

See [daemon lifecycle](../../pkg/claude/agentd/lifecycle.go),
[harness lifecycle](../../pkg/claude/harness/lifecycle.go), and
[Codex native lifecycle](../../pkg/claude/agentd/codex_native_registry_lifecycle.go).
This describes existing organization, not a verified step-by-step restart trace
for every harness. That trace is still needed before choosing an extraction.

## Future (proposed)

| Part of a restart/lifecycle operation | Intended organization |
|---|---|
| Application rules | Common operation owns authorization, configuration resolution and user-visible outcome |
| Harness differences | An explicit strategy owns a coherent native sequence, calling focused services |
| Identity | Track the agent, running window and native resumable unit separately throughout the transition |
| Later observations | Define how native events correlate to the operation attempt and complete or update its outcome |

### Example: restart an agent

A possible common outline is:

1. Identify the agent and authorize the caller.
2. Resolve the requested configuration and current execution state.
3. Ask the selected harness strategy to carry out the native transition.
4. Record the outcome and expose progress or failure.

Inside step 3, one strategy could stop execution, resume the existing native
conversation and confirm readiness. Another could request native replacement,
await an event carrying a new conversation ID, associate it and then confirm
readiness. These are hypothetical sequences, not current behavior attributed to
named harnesses. They need not share an identical internal step list.

The common contract describes the user-visible result: preserve the agent's
identity, associate its history correctly, and expose starting/ready/failed
states honestly. It must not require a final native conversation ID immediately
if a supported harness cannot provide one.

## Explicit strategies, not arbitrary hooks

Use a shared implementation when behavior matches. Select a different strategy
for a coherent part of the operation when sequencing or lifecycle differs.
Strategies can call services and reuse small helpers; they need not copy the
whole operation.

Prefer a readable native-transition implementation to scattered flags such as
`skipStop`, `beforeResume`, `afterResume` and `replaceIdLater`. Add a variation
point only when a real harness demonstrates the need. A strategy is code with a
clear contract, not necessarily a configurable workflow or a new interface.

Shared authorization and state protections are not optional strategy hooks.
The operation owns application admission; strategies cannot bypass it. Long
operations must define what is rechecked before later effects under existing
rules. Capability support alone never grants permission.

## Operations can outlive requests

A returned “starting” result is not proof of successful launch. The initiating
operation and later harness events must agree on which attempt they describe
and who records its completion. An old event must not complete a newer attempt.

Each selected operation needs explicit answers for failure, cancellation,
concurrent requests and retry. Database changes and native process effects are
not one atomic transaction. Preserve existing guarantees, and report uncertain
outcomes rather than blindly repeating a possibly completed effect.

These requirements do not imply one durable workflow engine for every action.
Use the simplest existing mechanism appropriate to the operation.

## First design exercise

Choose one operation already implemented on main. Trace it through two harnesses
with genuinely different lifecycles. Write their normal and failure sequences
side by side. Identify shared rules, services and the smallest useful strategy
boundary. Try that extraction without changing the public behavior, then assess
whether it actually reduced duplication and made the sequence easier to read.
