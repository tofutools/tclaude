// resource-moves.js -- moving a board, a page or a saved process to another
// group (tcl-eq8pq9), and copying a board or a process into one: which
// groups it may go to, and the move or copy itself. The Groups
// tab's ⋯ → Move to group, its drag of a resource row onto a group box
// (resource-dnd.js) and the Collab panel's drag onto a group section
// (collab-rail-actions.js) all go through here, so the three agree.
//
// A board may go to any group, or out of every group (''). A page always
// belongs to a group: an agent's page only to a group its agent is in, and a
// group's own blank page only while it is closed. The server enforces the
// same; this keeps the views from offering what it would refuse. A process
// is pinned to groups: moving it pins it in the other group and unpins it
// here, and copying it only pins it there (the one template is shared).
// Copying a board makes a new board with the same drawing in the group. A
// page is its agent's, and a shell its directory's: neither copies.

// RESOURCE_DRAG_MIME carries a dragged resource: { kind, id, label, group,
// owner, closed } (JSON).
import { freeBoardId } from './board-manage.js';

export const RESOURCE_DRAG_MIME = 'application/x-tclaude-resource';

// MOVABLE_KINDS are the resources that change group. A shell is its group's
// own.
export const MOVABLE_KINDS = Object.freeze(['board', 'page', 'process']);

// COPYABLE_KINDS are the resources a Ctrl/Cmd-drag copies into a group.
export const COPYABLE_KINDS = Object.freeze(['board', 'process']);

const realGroup = (snapshotValue, name) => (snapshotValue?.groups || []).find((g) => g?.id > 0 && g.name === name) || null;

// pinnedIn is whether group (a snapshot group) has process id saved to it.
// A group also lists a process it only has a run of; that one is not pinned.
const pinnedIn = (snapshotValue, group, id) => (snapshotValue?.group_resources || [])
  .some((r) => r?.kind === 'process' && r.id === id && r.group_id === group.id && r.pinned);

// moveTargets are the groups a resource can move to: the others, and for an
// agent's page only those its agent is a member of.
export function moveTargets(snapshotValue, groupId, agentId = '') {
  return (snapshotValue?.groups || [])
    .filter((g) => g?.id > 0 && g.id !== groupId && g.name)
    .filter((g) => !agentId || (g.members || []).some((m) => m.agent_id === agentId))
    .map((g) => g.name);
}

// canMoveResource says whether resource ({ kind, group, owner, closed }) may
// move to the group named to ('' for none).
export function canMoveResource(resource, to, snapshotValue) {
  if (!resource || !MOVABLE_KINDS.includes(resource.kind)) return false;
  const from = resource.group || '';
  if (to === from) return false;
  if (resource.kind === 'board') return !to || !!realGroup(snapshotValue, to);
  if (!to) return false;
  if (resource.kind === 'process') {
    // Only a process saved to its group moves out of it; one the group only
    // runs is copied (pinned) instead.
    const group = realGroup(snapshotValue, to);
    const source = realGroup(snapshotValue, from);
    return !!group && !!source && pinnedIn(snapshotValue, source, resource.id) && !pinnedIn(snapshotValue, group, resource.id);
  }
  // A group's own page (no agent owner) moves only while closed.
  if (!resource.owner && !resource.closed) return false;
  const fromId = (snapshotValue?.groups || []).find((g) => g?.name === from)?.id ?? 0;
  return moveTargets(snapshotValue, fromId, resource.owner || '').includes(to);
}

// moveResource moves resource to the group named to ('' takes a board out of
// every group), says how it went, and refreshes. api is the process run API
// (process-context.js): moveBoard, moveGroupPage.
export async function moveResource(resource, to, { api, notify, refresh = () => {}, confirm = null }) {
  const label = resource.label || resource.id;
  if (confirm && !(await confirm(dropConfirm(resource, to, false)))) return false;
  try {
    if (resource.kind === 'board') await api.moveBoard(resource.id, to);
    else if (resource.kind === 'process') {
      // Pinned there first, so a failed unpin leaves it in both, not neither.
      await api.pinProcess(to, resource.id);
      // Already gone here (unpinned meanwhile) is still a move.
      await api.unpinProcess(resource.group, resource.id).catch((cause) => { if (cause?.status !== 404) throw cause; });
    } else await api.moveGroupPage(resource.id, to);
    notify(to ? `Moved ${label} to ${to}` : `Moved ${label} out of its group`);
    void refresh();
    return true;
  } catch (cause) {
    notify(`Could not move ${label}: ${cause.message}`, true);
    return false;
  }
}

// canCopyResource says whether a Ctrl/Cmd-drop copies resource into the
// group named to: a board into any group (its own too, as a second board),
// a process into a group that does not list it yet.
export function canCopyResource(resource, to, snapshotValue) {
  if (!resource || !COPYABLE_KINDS.includes(resource.kind) || !to) return false;
  const group = realGroup(snapshotValue, to);
  if (!group) return false;
  return resource.kind === 'board' || !pinnedIn(snapshotValue, group, resource.id);
}

// copyResource copies resource into the group named to, says how it went,
// and refreshes. api: copyBoard and boardIds (a board), pinProcess (a
// process).
export async function copyResource(resource, to, { api, notify, refresh = () => {}, confirm = null }) {
  const label = resource.label || resource.id;
  if (confirm && !(await confirm(dropConfirm(resource, to, true)))) return false;
  try {
    if (resource.kind === 'board') {
      const title = `${label} (copy)`;
      const board = await api.copyBoard(resource.id, to, title, freeBoardId(title, await api.boardIds()));
      notify(`Copied ${label} to ${to} as ${board?.title || title}`);
    } else {
      await api.pinProcess(to, resource.id);
      notify(`Added ${label} to ${to}`);
    }
    void refresh();
    return true;
  } catch (cause) {
    notify(`Could not copy ${label}: ${cause.message}`, true);
    return false;
  }
}

const KIND_NOUN = Object.freeze({ board: 'drawing board', page: 'page', process: 'process' });

// dropConfirm is the confirm a drag's drop asks before it moves or copies
// resource into the group named to, as an agent's drop does (dnd-ops.js):
// { title, body, meta, okLabel } for the shell's confirm modal.
export function dropConfirm(resource, to, copy) {
  const label = resource.label || resource.id;
  const from = resource.group || '';
  const noun = KIND_NOUN[resource.kind] || resource.kind;
  if (copy) {
    return resource.kind === 'process'
      ? { title: 'Add process to another group?', meta: label, okLabel: 'Add',
        body: `Save "${label}" to group "${to}" as well. It stays in "${from}"; both use the one template.` }
      : { title: from === to ? `Copy ${noun}?` : `Copy ${noun} to another group?`, meta: label, okLabel: 'Copy',
        body: `Make a copy of "${label}" in group "${to}", with the same drawing. The original stays ${from ? `in "${from}"` : 'where it is'}.` };
  }
  if (!to) {
    return { title: `Take ${noun} out of its group?`, meta: label, okLabel: 'Move',
      body: `Take "${label}" out of group "${from}". It is then in no group.` };
  }
  const note = resource.kind === 'page' ? ' The marks sent before are dropped.'
    : resource.kind === 'process' ? ` It is saved to "${to}" and removed from "${from}"; the template itself stays.` : '';
  return { title: `Move ${noun} to another group?`, meta: label, okLabel: 'Move',
    body: `Move "${label}" ${from ? `from group "${from}" ` : ''}to group "${to}".${note}` };
}
