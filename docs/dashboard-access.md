# Access tab

The **Access** tab shows and changes what agents may do. For the model behind it,
see [Permissions and audit](permissions-and-audit.md).

Waiting permission requests aren't here. They open in the
[approval panel](companion.md#answering-permission-requests), and they're always
in [Messages › 🔐 Access requests](dashboard-messages.md#access-requests).

## Permissions

The top of the tab lists the defaults every agent gets, from `config.json`. Below
are permanent grants and denies per agent.

Each permission has three states: **Default**, **Grant** and **Deny**. A deny is a
veto. The dialog shows where each effective permission comes from, with 👑 marking
what ownership gives and 👥 what membership gives. Some permissions can be narrowed
with a scope.

A granted permission that takes a scope has a ▸ that opens its scope drawer, one line
per dimension. A short list of values is a select. A long one, and the `resource`
dimension of `resources.read` and `resources.write`, is a search box: type to filter,
↑↓ to move, Enter to add, and Tab to complete the highlighted name. The resource
catalogue (every board and process template, with its groups) loads when the drawer
opens. Results are grouped by kind and then group, and at most 8 show, with the total
and a hint to refine. Type `board:` or `process_template:` to narrow by kind, or
`group:<name>` by group. Opened with nothing typed, the box shows the groups with
counts. A value the list doesn't have can be typed and added with Enter, and the daemon
checks it on save. Past six values, the chips collapse into **+N more**, which opens a
filterable, paged list of the rest.

A permission at Default that an umbrella covers says so: `boards.read` under a granted
`resources.read` reads "covered by resources.read, scoped to 2 groups and 41 resources".
The reach is given in counts. A Deny on the row, or on a step of the chain, removes the
hint, because the deny wins.

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
`groups.admin` still apply. The row says so. **Max uses** under the list limits every
picked permission that names a countable action, per permission, and a row's own **max**
overrides it: the grant ends at whichever comes first, the time or the count, and a
scope works as usual. A row that cannot have a limit says **no limit**, with the reason
on hover. That includes a grant scoped to a peer, because the count stops at this node,
and a permission the agent already holds, because that access is never counted. See
[what counts](permissions-and-audit.md#use-limits). A grant stored for all future
Stewards keeps its scope.
**Active now** lists what the agent already holds, with scope, time and uses left,
**+duration** to extend and **Revoke**.

Limits are the server's: the default is 5 minutes, the maximum 1 hour counted from now
(extensions included), and `permissions.grant` and `permissions.revoke` can't be given
through sudo. `agent.sudo` in config changes them.
