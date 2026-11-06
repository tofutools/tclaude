// resource-moves.js -- moving a board or a page to another group
// (tcl-eq8pq9): which groups it may go to, and the move itself. The Groups
// tab's ⋯ → Move to group, its drag of a resource row onto a group box
// (resource-dnd.js) and the Collab panel's drag onto a group section
// (collab-rail-actions.js) all go through here, so the three agree.
//
// A board may go to any group, or out of every group (''). A page always
// belongs to a group: an agent's page only to a group its agent is in, and a
// group's own blank page only while it is closed. The server enforces the
// same; this keeps the views from offering what it would refuse.

// RESOURCE_DRAG_MIME carries a dragged resource: { kind, id, label, group,
// owner, closed } (JSON).
export const RESOURCE_DRAG_MIME = 'application/x-tclaude-resource';

// MOVABLE_KINDS are the resources that change group. A shell is its group's
// own; a process is pinned, not moved.
export const MOVABLE_KINDS = Object.freeze(['board', 'page']);

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
  if (resource.kind === 'board') return !to || (snapshotValue?.groups || []).some((g) => g?.id > 0 && g.name === to);
  if (!to) return false;
  // A group's own page (no agent owner) moves only while closed.
  if (!resource.owner && !resource.closed) return false;
  const fromId = (snapshotValue?.groups || []).find((g) => g?.name === from)?.id ?? 0;
  return moveTargets(snapshotValue, fromId, resource.owner || '').includes(to);
}

// moveResource moves resource to the group named to ('' takes a board out of
// every group), says how it went, and refreshes. api is the process run API
// (process-context.js): moveBoard, moveGroupPage.
export async function moveResource(resource, to, { api, notify, refresh = () => {} }) {
  const label = resource.label || resource.id;
  try {
    if (resource.kind === 'board') await api.moveBoard(resource.id, to);
    else await api.moveGroupPage(resource.id, to);
    notify(to ? `Moved ${label} to ${to}` : `Moved ${label} out of its group`);
    void refresh();
    return true;
  } catch (cause) {
    notify(`Could not move ${label}: ${cause.message}`, true);
    return false;
  }
}
