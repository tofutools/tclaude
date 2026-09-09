# Operations coordinate user requests

An operation answers a request such as “restart this agent” or “send this
message.” It owns the sequence, the shared application rules and the outcome
presented to the caller. Dashboard, CLI and automation reuse the same operation
where they request the same behavior, retaining their actual caller identity.

## Example: restart an agent

A possible common outline is:

1. Identify the agent and authorize the caller.
2. Resolve the requested configuration and current execution state.
3. Ask the selected harness strategy to carry out the native transition.
4. Record the outcome and expose progress or failure.

Inside step 3, the sequences can be fundamentally different:

| Illustrative harness A | Illustrative harness B |
|---|---|
| Stop the old execution | Request a native replacement |
| Resume the existing native conversation | Await a native lifecycle notification |
| Confirm readiness | Associate the newly reported conversation ID |
| Report the resumed execution | Confirm readiness and report the replacement |

These examples explain variation; they do not prescribe the behavior of a named
harness. Do not force both into a fixed list of identical internal steps.

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
