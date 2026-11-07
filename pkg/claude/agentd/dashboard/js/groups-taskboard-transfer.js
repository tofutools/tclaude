// groups-taskboard-transfer.js -- dragging a task to another group's board
// (tcl-0r0obl, docs/taskboard.md): drop it on another group's Task board
// section (list or lanes) or on its header row to move it there; hold
// Ctrl/Cmd, as the Groups view's other clones do, to clone it instead. Both
// ask first, with the Groups view's confirmation, then run
//
//   tclaude agent taskboard move-to-group <task> <group> [--clone] [--stage next]
//
// A drop on a lanes cell in Next lands the task in next; anywhere else it
// lands in the ideas. The drag is the operator's, so nothing gates it here
// but the routes.
//
// In the fleet view a group on another node is a target too (this node's or
// a linked one's, either way round): the task is created there first
// (transfer-in, on the target node), and only once that node has it is the
// original dropped with a "moved to node/group" note (moved-away, on its own
// node). A failed create changes nothing; a failed drop is retried once,
// then left as a warning that names both copies, never a silent duplicate.

import { dashboardState } from './snapshot-store.js';
import { markLanded } from './drop-landing.js';
import { shellToast } from './shell-state.js';

const OVER = 'tb-transfer-over';
// TASK_DRAG_TYPE is a task drag's data type (groups-taskboard.js, the
// lanes): only such a drag is moved, so a listener left behind by a missed
// dragend never takes another drag (an agent onto a group).
const TASK_DRAG_TYPE = 'application/x-tclaude-task';

function isTaskDrag(event) {
  const types = event.dataTransfer?.types;
  return !types || Array.from(types).includes(TASK_DRAG_TYPE);
}

// transferDeps are the page's confirmation and toast, swappable in tests.
export const transferDeps = {
  confirm: async (options) => (await import('./refresh.js')).confirmModal(options),
  toast: (text, error) => shellToast(text, error),
  // nodeApi is the write client for the boards of a node (by instance id).
  nodeApi: async (node) => (await import('./groups-taskboard.js')).nodeTaskboard(node).api,
};

// cloneGesture is the Groups view's clone modifier: Ctrl or Cmd held, or a
// drag the browser already turned into a copy.
export function cloneGesture(event) {
  return !!(event.ctrlKey || event.metaKey) || event.dataTransfer?.dropEffect === 'copy';
}

// realGroupBox is the <details> of a real group (not a virtual one) that
// holds el.
function realGroupBox(el) {
  const box = el?.closest?.('details[data-group-key]');
  if (!box || !(box.hasAttribute('data-dnd-target-group') || box.hasAttribute('data-fleet-node'))) return null;
  return box;
}

// ownName is a group's name on its own node: the fused view lists a linked
// node's group as name@node.
function ownName(shown, box) {
  const node = box?.getAttribute('data-fleet-node-name');
  const at = node ? `@${node}` : '';
  return at && shown.endsWith(at) ? shown.slice(0, -at.length) : shown;
}

// transferTarget is where a drag over el would move a task from the board
// shown as `fromShown` (in box `fromBox`): another group's Task board
// section or header row, or null. node and nodeName name the target's node,
// and cross says it is not the source's.
export function transferTarget(el, fromShown, fromBox) {
  const section = el?.closest?.('[data-fused-taskboard]');
  let box;
  let shown;
  let into = el;
  if (section) {
    shown = section.dataset.fusedTaskboard;
    box = realGroupBox(section) || null;
    into = section;
  } else {
    const summary = el?.closest?.('summary');
    box = summary && realGroupBox(summary);
    if (!box || summary.parentElement !== box) return null;
    shown = box.dataset.groupKey;
    into = summary;
  }
  if (!shown || shown === fromShown) return null;
  const node = box?.getAttribute('data-fleet-node') || '';
  const cross = node !== (fromBox?.getAttribute('data-fleet-node') || '');
  const column = section ? el.closest?.('.tb-cell')?.dataset.column : '';
  return {
    el: into, shown, group: ownName(shown, box), stage: column === 'next' ? 'next' : 'idea',
    node, nodeName: box?.getAttribute('data-fleet-node-name') || '', cross,
  };
}

function clearOver(state) {
  state.over?.classList.remove(OVER);
  state.over?.removeAttribute('data-tb-transfer');
  state.over = null;
}

function stageLabel(stage) {
  return stage === 'next' ? 'Next' : 'Ideas';
}

// targetMembers are the target group's members, from the page's snapshot.
function targetMembers(name) {
  const group = (dashboardState.snapshot.value?.groups || []).find((g) => g.name === name);
  return group?.members || [];
}

// crossMessage is the confirmation's text for a move (or clone) to a group
// on another node.
export function crossMessage({ task, from, fromNode, to, toNode, stage, clone }) {
  const title = task.title || task.id;
  const there = `group "${to}" on ${toNode}`;
  if (clone) {
    return {
      title: 'Clone task to another node?',
      body: `Clone "${title}" into ${there} as a new task in ${stageLabel(stage)}, with the same title, brief and ref. `
        + `The original stays in "${from}"${fromNode ? ` on ${fromNode}` : ''}.`,
      okLabel: 'Clone',
    };
  }
  return {
    title: 'Move task to another node?',
    body: `Move "${title}" to ${there}: it is created there in ${stageLabel(stage)}, with the same title, brief and ref `
      + `and a note where it came from, and only then closed here as dropped ("moved to ${toNode}/${to}"). `
      + 'Its history, assignee and dependencies stay behind; an assignee is told.',
    okLabel: 'Move',
  };
}

// transferMessage is the confirmation's text for moving (or cloning) task
// from group `from` to `to`, naming what a move undoes.
export function transferMessage({ task, from, to, stage, clone, tasks = [], members = [] }) {
  const title = task.title || task.id;
  if (clone) {
    return {
      title: 'Clone task to another group?',
      body: `Clone "${title}" into group "${to}" as a new task in ${stageLabel(stage)}, with the same title, brief and ref. `
        + `The clone has no assignee and no dependencies; the original stays in "${from}".`,
      okLabel: 'Clone',
    };
  }
  const name = (id) => {
    const t = tasks.find((x) => x.id === id);
    return t ? `"${t.title}"` : id;
  };
  const deps = (task.depends_on || []).map(name);
  const dependents = tasks.filter((t) => (t.depends_on || []).includes(task.id)).map((t) => `"${t.title}"`);
  const parts = [`Move "${title}" from group "${from}" to group "${to}". It keeps its history.`];
  const assignee = task.assignee?.agent_id;
  const done = task.status === 'done';
  // members is null where the target's members are not known (a linked
  // node's group): the server decides, and the dialog does not guess.
  const known = Array.isArray(members);
  const stays = known && assignee && members.some((m) => m.agent_id === assignee);
  if (done) parts.push(stays ? 'It stays done and assigned.' : 'It stays done.');
  else if (stays) parts.push('It stays assigned and lands in Next.');
  else parts.push(`It lands in ${stageLabel(stage)}.`);
  if (assignee && known && !stays) parts.push(`Its assignee is not a member of "${to}", so it is unassigned${done ? '' : ' (and told)'}.`);
  if (assignee && !known) parts.push(`If its assignee is not a member of "${to}", it is unassigned.`);
  if (task.status === 'blocked') parts.push('Its block is lifted.');
  if (deps.length) parts.push(`It no longer waits for ${deps.join(', ')} (dependencies never cross groups).`);
  if (dependents.length) parts.push(`${dependents.join(', ')} no longer wait${dependents.length === 1 ? 's' : ''} for it.`);
  return { title: 'Move task to another group?', body: parts.join(' '), okLabel: 'Move' };
}

// listenForTransfer takes, for the length of one task drag, a drop on
// another group's board or header. from is { task, group (the board's own
// group, with shownName for a linked node's), tasks, actions }. Returns the
// function that stops listening.
export function listenForTransfer({ task, group, tasks = [], actions, onDrop = () => {} }) {
  if (!actions || typeof actions.api?.moveToGroup !== 'function') return () => {};
  const fromShown = group.shownName || group.name;
  const state = { over: null };
  const fromBox = () => [...document.querySelectorAll('[data-fused-taskboard]')]
    .find((el) => el.dataset.fusedTaskboard === fromShown)?.closest('details[data-group-key]') || null;
  const onOver = (event) => {
    if (!isTaskDrag(event)) { clearOver(state); return; }
    const hit = transferTarget(event.target, fromShown, fromBox());
    if (!hit) { clearOver(state); return; }
    event.preventDefault();
    event.stopPropagation();
    const clone = cloneGesture(event);
    if (event.dataTransfer) event.dataTransfer.dropEffect = clone ? 'copy' : 'move';
    if (state.over !== hit.el) { clearOver(state); state.over = hit.el; hit.el.classList.add(OVER); }
    hit.el.setAttribute('data-tb-transfer', `${clone ? 'clone' : 'move'} to ${hit.group} · ${stageLabel(hit.stage)}`);
  };
  const off = () => {
    clearOver(state);
    document.removeEventListener('dragover', onOver, true);
    document.removeEventListener('drop', onDropEvent, true);
  };
  const onDropEvent = (event) => {
    if (!isTaskDrag(event)) { off(); return; }
    const hit = transferTarget(event.target, fromShown, fromBox());
    if (!hit) return;
    event.preventDefault();
    event.stopPropagation();
    const clone = cloneGesture(event);
    off();
    onDrop();
    if (hit.cross) {
      void runCrossTransfer({ task, group, actions, to: hit.group, node: hit.node, nodeName: hit.nodeName,
        fromNodeName: fromBox()?.getAttribute('data-fleet-node-name') || '', stage: hit.stage, clone });
    } else {
      void runTransfer({ task, group, tasks, actions, to: hit.group, stage: hit.stage, clone });
    }
  };
  document.addEventListener('dragover', onOver, true);
  document.addEventListener('drop', onDropEvent, true);
  return off;
}

// runTransfer asks, then moves or clones the task.
export async function runTransfer({ task, group, tasks = [], actions, to, stage, clone }) {
  // A linked node's groups are not in this page's snapshot by their own name.
  const linked = group.fleet_node && !group.fleet_node.local;
  const text = transferMessage({ task, from: group.name, to, stage, clone, tasks, members: linked ? null : targetMembers(to) });
  const ok = await transferDeps.confirm({ ...text, meta: task.title || task.id });
  if (!ok) return null;
  const out = await actions.run(`${task.id}:transfer`, clone ? 'Clone' : 'Move',
    (api) => api.moveToGroup(task.id, to, { clone, stage }));
  if (!out || out === true) return null;
  const done = clone ? `Cloned "${task.title}" into ${to}` : `Moved "${task.title}" to ${to}`;
  const notes = [];
  if (out.unassigned) notes.push('unassigned');
  const dropped = (out.dropped_deps?.length || 0) + (out.dropped_dependents?.length || 0);
  if (dropped) notes.push(`${dropped} dependenc${dropped === 1 ? 'y' : 'ies'} dropped`);
  transferDeps.toast(notes.length ? `${done} (${notes.join(', ')})` : done, false);
  markLanded({ group: to, node: group.fleet_node?.id || '', section: 'taskboard' });
  return out;
}

// runCrossTransfer asks, then moves or clones the task to group `to` on
// node `node`: create there first, then drop the original here.
export async function runCrossTransfer({ task, group, actions, to, node, nodeName, fromNodeName = '', stage, clone }) {
  const toNode = nodeName || node;
  const text = crossMessage({ task, from: group.name, fromNode: fromNodeName, to, toNode, stage, clone });
  const ok = await transferDeps.confirm({ ...text, meta: task.title || task.id });
  if (!ok) return null;
  const target = await transferDeps.nodeApi(node);
  let warning = '';
  const out = await actions.run(`${task.id}:transfer`, clone ? 'Clone' : 'Move', async (api) => {
    const created = await target.transferIn(to, {
      title: task.title, body: task.body || '', ref: task.ref?.url || task.ref?.label || '',
      refLabel: task.ref?.url ? (task.ref?.label || '') : '', stage, clone,
      origin: { node: fromNodeName, group: group.name, task: task.id },
    });
    if (clone) return { created };
    const away = { node: toNode, group: to, task: created?.id || '' };
    let last;
    for (let attempt = 0; attempt < 2; attempt++) {
      try { await api.movedAway(task.id, away); return { created }; } catch (cause) { last = cause; }
    }
    warning = `Moved "${task.title}" to ${toNode}/${to} as ${created?.id || 'a new task'}, but the original `
      + `${task.id} could not be closed here (${last?.message || last}): both copies are live, drop this one by hand.`;
    return { created, warning };
  });
  if (!out || out === true) return null;
  if (warning) {
    transferDeps.toast(warning, true);
  } else {
    transferDeps.toast(clone ? `Cloned "${task.title}" into ${to} on ${toNode}` : `Moved "${task.title}" to ${to} on ${toNode}`, false);
  }
  markLanded({ group: to, node, section: 'taskboard' });
  return out;
}
