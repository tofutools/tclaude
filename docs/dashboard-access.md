# Access tab

The **Access** tab shows and changes what agents may do. For the model behind it,
see [Permissions and audit](permissions-and-audit.md).

Waiting permission requests aren't here. With Collab on they open in the
[approval panel](companion.md#answering-permission-requests), and they're always
in [Messages › 🔐 Access requests](dashboard-messages.md#access-requests).

## Permissions

The top of the tab lists the defaults every agent gets, from `config.json`. Below
are permanent grants and denies per agent.

Each permission has three states: **Default**, **Grant** and **Deny**. A deny is a
veto. The dialog shows where each effective permission comes from, with 👑 marking
what ownership gives and 👥 what membership gives. Some permissions can be narrowed
with a scope.

Similar dialogs edit group permissions, which apply right away (an agent-level deny
still wins), and role permissions, which only grant.

## Slug registry

A read-only list of every permission, with its name, owner and description, and a
filter.

## Sudo

Active time-limited grants, with a countdown, reason, who granted it, and revoke.
**+ Grant sudo** adds one, 5 minutes by default. `permissions.grant` and
`permissions.revoke` can't be given through sudo.
