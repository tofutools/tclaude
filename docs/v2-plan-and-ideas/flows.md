# A user flow through the proposed system

Consider: **“Add a reviewer to this group using my review profile.”**

## What the operator should experience

1. Choose the group and profile, adjust settings if needed, and enter the task.
2. See the new member and whether it started successfully.
3. Open its terminal or send it a message.
4. Stop/restart it later without losing its identity or previous conversation.

If a setting is unsupported or access is refused, explain the actual issue.
Do not create a different agent configuration silently to make launch succeed.

## What the implementation does

```mermaid
sequenceDiagram
    actor Operator
    participant UI as Dashboard
    participant Action as Group-member action
    participant Settings as Settings and permissions
    participant DB as Storage
    participant Harness as Harness and runtime
    Operator->>UI: Add reviewer using profile
    UI->>Action: Group, profile, overrides, task and caller
    Action->>Settings: Resolve choices and check permission
    Settings-->>Action: Settings or actionable refusal
    Action->>DB: Record the required agent and membership state
    Action->>Harness: Start with the selected settings
    Harness-->>Action: Native result
    Action->>DB: Record outcome
    Action-->>UI: Member and launch status
    UI-->>Operator: Show reviewer; allow terminal/message actions
```

This is a responsibility sketch, not a new transaction protocol. Preserve the
current flow's admission, cleanup and retry guarantees. In particular, SQL and
native process creation cannot be treated as one atomic operation. Record a
failure or uncertain outcome honestly rather than replaying the effect blindly.

## Reusing the flow

A team deployment builds member requests from its template and applicable
overrides. A trigger decides when to issue a request. Both call the same shared
member/launch behavior where their semantics match. They retain their own
progress, capacity and policy rules; there is no requirement to merge their engines.

Manual spawn and team launch already share parts of resolution. Preserve that
code and make any remaining differences explicit rather than assuming all
callers should have identical precedence.

## Why a restart is different from creating a member

The agent and membership already exist. A restart starts another running session
for that agent and applies the appropriate saved/current settings. It must not
recreate memberships, duplicate messages or reapply removed permissions merely
because creation once did those things.

This distinction is a reason for separate application actions with shared small
helpers, rather than one giant “do everything” launch function.

## How to refactor this without replacing the product

Pick one existing path. Record its normal result and the important refusal,
retry and stop/restart cases. Extract a shared function behind the current entry
point, move a second appropriate caller, and remove obsolete glue. Existing
public flow tests should still exercise real application/storage behavior.

Useful checks include explicit false/clear/inherit, profile edits while running,
lost replies, permission changes, membership removal, and late old-session
reports—but only when the selected change affects them. Stop at a useful small
boundary and reassess before broadening it.
