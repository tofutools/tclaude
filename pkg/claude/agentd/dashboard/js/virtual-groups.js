// virtual-groups.js — the dashboard synthetic groups.
//
// Ungrouped / Conversations / Retired are not real server-side groups;
// they are built client-side from the snapshot so groupless agents,
// raw conversations, and retired agents each surface as a drag target
// in the Groups tab. Extracted from dashboard.js as part of the
// Stage 2 module split.

import { $ } from './helpers.js';

// The virtual "Ungrouped" group. UNGROUPED_LABEL is the display
// name; UNGROUPED_VKEY is its identity for localStorage / DOM keying.
// The key has a leading space, which validateGroupName (server-side)
// rejects — so it can never collide with a real group's name, even
// if a human creates a real group literally called "Ungrouped".
const UNGROUPED_LABEL = 'Ungrouped';
const UNGROUPED_VKEY = ' ungrouped-virtual';

// virtualUngroupedGroup builds the synthetic group object from the
// snapshot's groupless agents. Always returns the object (with an
// empty members[] when there are none) — once the human has ticked
// "show ungrouped" the group stays visible even while empty, so it
// reads as a stable, discoverable drop target rather than something
// that blinks in and out as agents come and go. The `virtual` flag
// is the discriminator every group-affecting code path keys off to
// suppress rename / delete / multicast / cron / add-member.
//
// Online AND offline agents are kept: ungrouped[] carries both (a
// freshly-promoted offline conversation belongs here so it can be
// dragged into a group). renderVirtualGroup applies the "show
// offline" filter at render time, like a real group.
function virtualUngroupedGroup(agents) {
  const rows = (agents || []).slice();
  return {
    name: UNGROUPED_LABEL,
    key: UNGROUPED_VKEY,
    virtual: true,
    descr: 'agents not in any group',
    members: rows,
    online: rows.filter(a => a.online).length,
  };
}

// ungroupedVisible reports the "show ungrouped" checkbox state.
// Defaults to true when the checkbox isn't in the DOM yet.
function ungroupedVisible() {
  const el = $('#filter-groups-ungrouped');
  return el ? el.checked : true;
}

// The virtual "Conversations" group — non-agent conversations,
// surfaced in the Groups tab so a raw conversation can be dragged
// into a group (which promotes it) without leaving the tab.
const CONVERSATIONS_LABEL = 'Conversations';
const CONVERSATIONS_VKEY = ' conversations-virtual';

// virtualConversationsGroup builds the synthetic group from the
// snapshot's conversations[] (recent non-enrolled convs). The
// `conversations` flag is the discriminator renderGroups keys off to
// pick the lighter conversation-row renderer.
function virtualConversationsGroup(convs) {
  const rows = (convs || []).slice();
  return {
    name: CONVERSATIONS_LABEL,
    key: CONVERSATIONS_VKEY,
    virtual: true,
    conversations: true,
    descr: 'recent conversations that are not agents',
    members: rows,
    online: rows.filter(c => c.online).length,
  };
}

// conversationsVisible reports the "show conversations" checkbox
// state. Defaults to false — there can be a lot of conversations, so
// the virtual group is opt-in (unlike "show ungrouped").
function conversationsVisible() {
  const el = $('#filter-groups-conversations');
  return el ? el.checked : false;
}

// The virtual "Retired" group — agents that were demoted back to
// plain conversations (retire). Surfaced in the Groups tab so a
// retired agent doesn't silently vanish off the tab: it lands here
// and can be reinstated in place.
const RETIRED_LABEL = 'Retired';
const RETIRED_VKEY = ' retired-virtual';

// virtualRetiredGroup builds the synthetic group from the snapshot's
// retired[] rows. The `retired` flag is the discriminator renderGroups
// keys off to pick the retired-row renderer.
function virtualRetiredGroup(retired) {
  const rows = (retired || []).slice();
  return {
    name: RETIRED_LABEL,
    key: RETIRED_VKEY,
    virtual: true,
    retired: true,
    descr: 'agents demoted back to plain conversations',
    members: rows,
    online: rows.filter(r => r.online).length,
  };
}

// retiredVisible reports the "show retired" checkbox state. Defaults
// to true when the checkbox isn't in the DOM yet — a retired agent
// must not silently disappear from the Groups tab.
function retiredVisible() {
  const el = $('#filter-groups-retired');
  return el ? el.checked : true;
}

// The virtual "Pending" group — dashboard spawns whose conv-id has not
// materialised yet (the pending_spawns table — JOH-205 inc2). A pending
// Codex agent has a live tmux pane but is stuck behind a startup gate
// (untrusted dir, new-hooks-config prompt, OpenAI auth modal), so it
// never took the first turn that exposes its conv-id and is not an
// enrolled agent yet. Surfaced so the operator can SEE it and focus its
// pane to clear the gate; the pending_spawn sweeper then promotes it into
// a real agent.
//
// Unlike the other virtual groups this is an actionable ALERT, not a drag
// target: the caller (tabs.js) only builds it when there are pending
// spawns, so there is no persistent empty box, and it has no opt-out
// checkbox — a gated spawn must always surface.
const PENDING_LABEL = 'Pending';
const PENDING_VKEY = ' pending-virtual';

// virtualPendingGroup builds the synthetic group from the snapshot's
// pending[] rows. The `pending` flag is the discriminator renderGroups
// keys off to pick the pending-row renderer (focus button only).
function virtualPendingGroup(pending) {
  const rows = (pending || []).slice();
  return {
    name: PENDING_LABEL,
    key: PENDING_VKEY,
    virtual: true,
    pending: true,
    descr: 'spawns waiting to clear a startup gate',
    members: rows,
    online: rows.filter(p => p.online).length,
  };
}

export {
  virtualUngroupedGroup, ungroupedVisible,
  virtualConversationsGroup, conversationsVisible,
  virtualRetiredGroup, retiredVisible,
  virtualPendingGroup,
};
