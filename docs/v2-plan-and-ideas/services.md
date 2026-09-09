# Services and harness integration

In the proposed architecture, a service owns a focused mechanism and its rules. An operation coordinates
those mechanisms for a user request. A service may itself expose substantial
behavior; the distinction is responsibility, not function size.

## Current

These are implementation areas, not services already extracted on main.

| Responsibility | Current location or form |
|---|---|
| Configuration | Shared resolution helpers and flow-specific handling in daemon lifecycle and team/template code |
| Permissions | Existing access checks used by daemon actions |
| Native execution | Existing harness abstractions, spawners and lifecycle handling |
| Terminal and workspace | Existing tclaude session/window, tmux and worktree code |
| Identity and conversation tracking | Agent/conversation mappings, tclaude session records and native event handling |
| Messaging | Existing daemon mailbox and harness delivery paths |
| Persistence | Existing SQLite operations, used by application paths |

Examples: [daemon lifecycle](../../pkg/claude/agentd/lifecycle.go),
[team templates](../../pkg/claude/agentd/templates.go),
[harness boundary](../../pkg/claude/harness/harness.go), and
[database code](../../pkg/claude/common/db/).

## Future (proposed)

| Service responsibility | Intended boundary |
|---|---|
| Configuration | Resolve explicit choices, profiles and defaults with their meaning intact |
| Permissions | Decide whether an actor may perform an action under current rules |
| Native execution | Launch, resume or communicate with native execution where supported; do not call this tclaude's session/window model |
| Terminal and workspace | Manage tclaude windows, terminal views and working resources with explicit lifetimes |
| Identity and conversation tracking | Associate agents, native conversations, running windows and observations without conflating their IDs |
| Messaging | Store, address and track message delivery |
| Persistence | Commit related application state changes consistently |

This is a vocabulary for discussion, not a mandatory package or interface for
every row. Main already has harness abstractions and shared configuration
helpers; reuse them rather than introducing a parallel framework.

## Proposed harness integration: more than a function adapter

It may own native configuration validation, command construction, protocol
handling, lifecycle state and asynchronous observation processing. Some work
happens in response to an operation; other work happens because the native
harness reports a change.

For example, a context observation can arrive independently of any user action:

```mermaid
flowchart LR
    Native[Native status payload or event] --> Interpret[Harness-specific interpretation]
    Interpret --> Correlate[Identify execution and conversation]
    Correlate --> Reading[Record meaningful observation]
    Reading --> UI[Display value, age and uncertainty]
```

The integration understands what the native payload means. Shared tracking
services provide the associations and update rules. Unknown correlations must
not be guessed into the agent's current native execution. Depending on the native protocol,
handling may require waiting for identity information or exposing unavailable
state; that behavior belongs in the integration's contract.

## Where a strategy belongs

An operation selects the harness behavior it needs. The harness-specific strategy
can live alongside its integration and use that integration's mechanisms. The
application owns the common operation contract; the harness owns its native
sequence. Keep the dependency direction explicit when choosing packages.

Not every harness must implement every capability. Use focused contracts with
meaningful unsupported outcomes, rather than one enormous interface padded
with no-op methods.

## Keep the boundaries useful

A good service contract states what it accepts, what it can change and what its
result means. It should not expose a harness's incidental sequence as a rule
for all callers. Conversely, it should not erase a difference the operation
needs to handle.

Prefer direct calls and explicit dependencies within the single app. Event
handling does not require a new event bus. Test the operation through existing
public flows, and test native sequences and event interpretation at the harness
boundary. A successful extraction makes behavior easier to locate and change;
more layers alone are not progress.
