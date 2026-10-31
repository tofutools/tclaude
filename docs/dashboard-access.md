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
**+ Grant sudo** opens the sudo variant of the permission editor, the same one as
**+ sudo** on an agent row and **sudo** on a Steward. It groups permissions by area,
keeps its search above the list and the duration and Grant button below it, marks
risky permissions, and says why a slug is refused. Picked permissions can be narrowed
with scopes like any grant. On one the agent already holds, a scoped sudo replaces its
own, group and default access until the grant ends, while ownership, membership and
`groups.admin` still apply. The row says so. A permission with a countable action, today
`agent.spawn` and `groups.members.spawn`, also takes a **max** number of uses: the
grant ends at whichever comes first, the time or the count, and a scope works as usual.
A grant scoped to a peer takes no use limit, because the count stops at this node, and
neither does a permission the agent already holds, because that access is never counted.
A grant stored for all future Stewards keeps its scope.
**Active now** lists what the agent already holds, with scope, time and uses left,
**+duration** to extend and **Revoke**.

Limits are the server's: the default is 5 minutes, the maximum 1 hour counted from now
(extensions included), and `permissions.grant` and `permissions.revoke` can't be given
through sudo. `agent.sudo` in config changes them.
