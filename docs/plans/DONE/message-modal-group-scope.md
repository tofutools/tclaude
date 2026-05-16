# Group-scoped one-shot message modal — per-member subset selection

Shipped 2026-05.

The dashboard's one-shot message modal
([`dashboard-oneshot-message`](dashboard-oneshot-message.md)) was
reachable from two buttons: a per-agent ✉ (solo send) and a per-group
✉ message. The per-group button only ever did one thing — blast the
ENTIRE group. But the button sits next to a specific group, so the
natural expectation is that opening it scopes the modal to that group
and lets you pick who, exactly, receives the message.

This slice ships that: opened from a group's ✉ button, the modal locks
to that group and shows a checkbox list of its members — all ticked by
default, so "message the whole group" stays one click; untick any to
send to a subset.

## What shipped

### Modal — two shapes

`openMessageCreateModal(prefill)` now picks a shape from the prefill:

- **Solo mode** (per-agent ✉, prefill `{targetMode:'solo', …}`):
  unchanged. The shared solo/group target picker
  (`bindTargetPicker('message-create')`) is shown as before.
- **Group-scoped mode** (per-group ✉ message, prefill
  `{targetMode:'group', groupName}`): the target picker row is hidden
  and replaced by a member checkbox list. The modal title/description
  name the group; the picker is never reachable, so the modal cannot
  drift off-group.

`messageScopedGroup` (a module-level `{name, members:[{conv_id, title,
online, checked}]}` or `null`) holds the group-scoped state.
`setupMessageGroupScope` snapshots the group's members from
`lastSnapshot` with every box ticked; `renderMessageMembers` draws the
list (reusing the `cleanup-*` CSS the cleanup / bulk-window-focus
modals use, so the three selection UIs look consistent);
`select all` / `select none` shortcuts and an "N of M selected" count
round it out. No agent dropdown across unrelated agents is ever shown
in group context — the only recipients offered are that group's
members.

### Submit — whole group vs subset

`submitMessageForm` branches on `messageScopedGroup`:

- **Every member ticked** → a plain `to:"group:NAME"` multicast, no
  `members` field. This tracks the LIVE roster: a member who joined
  after the modal opened is still reached.
- **A subset ticked** → `to:"group:NAME"` plus an explicit
  `members:[conv-id,…]` list.

### Backend — the `members` narrowing filter

`sendReq` gained `Members []string`. It is the recipient-set twin of
`Role`:

- `dispatchSend` rejects `members` on a non-`group:` target with a 400
  (`members is only valid with a 'group:' multicast target`), exactly
  as it rejects `role`.
- `fanOutToGroup` gained a `memberFilter []string` parameter. When
  non-empty it is applied AFTER the roster is read alongside the role
  filter — so, like `role`, it can only SHRINK the recipient set,
  never widen it. A conv-id in the list that is not a current member
  of the group simply matches nothing; there is no way to message an
  unrelated agent through it. Each filter entry is resolved to its
  live successor (`db.ResolveLatestConv`) and matched against the
  likewise succession-walked roster id, so a subset that named a
  member who has since reincarnated still reaches the live agent — the
  dashboard's member list is a point-in-time snapshot that can lag the
  roster.
- The cron caller (`fireCronJob`) passes `nil` — group-targeted cron
  jobs still fan out to the whole group.

A subset multicast is recorded and attributed identically to a full
multicast: each recipient gets its own `agent_messages` row stamped
with the group's id, `from_conv` = the picked From conv. The recipient
cannot tell a subset send from a full one — it is just a group-routed
message. No new endpoint, no DB migration.

The dashboard's `POST /api/message` (`handleDashboardMessageCreate`)
threads the new `members` field straight into the `sendReq` it hands
to the shared `dispatchSend` core — the cookie-auth front door gains
one field and no new logic.

## Files

- `pkg/claude/agentd/handlers.go` — `sendReq.Members`, the
  `dispatchSend` 1:1-target guard, `fanOutToGroup`'s `memberFilter`.
- `pkg/claude/agentd/cron.go` — `fanOutToGroup` call passes `nil`.
- `pkg/claude/agentd/dashboard_message.go` — `members` in the request
  body struct, passed through to `dispatchSend`.
- `pkg/claude/agentd/dashboard.html` — the group-scoped member
  selector markup + JS (`messageScopedGroup`, `setupMessageGroupScope`,
  `renderMessageMembers`, the `submitMessageForm` branch).

## Tests

`pkg/claude/agentd/dashboard_message_subset_flow_test.go`:

- `…_ReachesOnlySelectedMembers` — `members:[A,C]` of a 3-member group
  reaches exactly A and C; B gets no row; the alive recipient is
  nudged.
- `…_ExplicitFullListReachesEveryMember` — a `members` list naming
  every member behaves identically to a bare multicast.
- `…_NonMemberIDsAreIgnored` — an outside conv-id in `members` matches
  nothing; the filter cannot widen reach.
- `…_ExcludesSenderEvenIfListed` — the From conv is skipped even when
  ticked in the subset.
- `…_RejectedOnSoloTarget` — `members` on a 1:1 send is a 400, no row
  written.
- `…_FollowsSuccessionToReincarnatedMember` — a subset naming a member
  who reincarnated after the snapshot still reaches the live
  successor.
- `…_BlankMembersListReachesNobody` — a `members` list whose entries
  all trim away narrows to nobody, never a full-group fallback (the
  filter can only shrink reach).
- `TestMulticast_MembersSubset_OnAgentMessagesEndpoint` — the `members`
  narrowing works on the agent-facing `POST /v1/messages` too (shared
  `sendReq`); the sender stays gated by `handleMulticast`.
