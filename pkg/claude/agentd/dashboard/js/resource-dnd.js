// resource-dnd.js -- dragging a board or a page row of a group's Resources
// (groups-resources.js, [data-dnd-resource]) onto another group's box in the
// Groups tab moves it there (tcl-eq8pq9); a board dropped on Ungrouped
// leaves every group. The move is the ⋯ → Move to group one
// (resource-moves.js), so the drag offers only what that menu would. It
// rides its own MIME type, which the member drag (dnd.js) ignores.

import { RESOURCE_DRAG_MIME, canMoveResource, moveResource } from './resource-moves.js';
import { clearDropBox, markDropBox, targetGroupOf } from './dnd-ops.js';
import { dashboardState } from './snapshot-store.js';
import { processContext, processRunApi } from './process-context.js';
import { defaultNotify } from './groups-quickstart.js';

// A box takes a resource when it is one of this node's real groups or the
// Ungrouped box; another node's (the fused view's group@node) does not.
const TARGET_SEL = 'details[data-dnd-target-group], details[data-dnd-target-ungrouped]';

// dropGroupOf is the group a box stands for: '' for Ungrouped, null for none.
export function dropGroupOf(box) {
  if (!box) return null;
  if (box.hasAttribute('data-dnd-target-ungrouped')) return '';
  return targetGroupOf(box) || null;
}

function readResource(row) {
  try {
    const value = JSON.parse(row.getAttribute('data-dnd-resource') || '');
    return value && value.kind && value.id ? value : null;
  } catch {
    return null;
  }
}

// bindResourceDnd wires the drag on doc; it returns the unbind.
//   deps: { api, notify, snapshot(), refresh() } (tests pass their own).
export function bindResourceDnd(doc = globalThis.document, deps = {}) {
  const api = deps.api || processRunApi;
  const notify = deps.notify || defaultNotify;
  const snapshot = deps.snapshot || (() => dashboardState.snapshot.value);
  const refresh = deps.refresh || (() => processContext.refresh());
  let dragged = null;
  let over = null;
  const mark = (box) => {
    if (over === box) return;
    clearDropBox(over);
    over = box;
    if (box) markDropBox(box, 'move');
  };
  // A press on a control in the row (its buttons, the rename field) is a
  // click, not a drag: the row stops being draggable until the press ends.
  let suppressed = null;
  const restore = () => { if (suppressed) suppressed.draggable = true; suppressed = null; };
  const down = (e) => {
    const row = e.target.closest?.('[data-dnd-resource]');
    if (!row || !e.target.closest('button, a, input, select, textarea, label, [contenteditable]')) return;
    restore();
    row.draggable = false;
    suppressed = row;
  };
  // Every drag starts clean: a resource drag whose row a re-render detached
  // never told the document it ended, and must not take a later drag's drop.
  const start = (e) => {
    dragged = null;
    mark(null);
    const row = e.target.closest?.('[data-dnd-resource]');
    if (!row) return;
    dragged = readResource(row);
    if (!dragged) return;
    e.dataTransfer?.setData(RESOURCE_DRAG_MIME, JSON.stringify(dragged));
    if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move';
    row.classList.add('dnd-source-row');
    // dragend goes to the source, which a re-render may have detached.
    row.addEventListener('dragend', end, { once: true });
  };
  const ours = (e) => !!e.dataTransfer?.types && Array.from(e.dataTransfer.types).includes(RESOURCE_DRAG_MIME);
  const target = (e) => {
    if (!dragged || !ours(e)) return null;
    const box = e.target.closest?.(TARGET_SEL) || null;
    const to = dropGroupOf(box);
    return to !== null && canMoveResource(dragged, to, snapshot()) ? { box, to } : null;
  };
  const dragover = (e) => {
    const hit = target(e);
    mark(hit?.box || null);
    if (!hit) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
  };
  const drop = (e) => {
    const hit = target(e);
    const resource = dragged;
    mark(null);
    if (!hit) return;
    e.preventDefault();
    e.stopPropagation();
    dragged = null;
    void moveResource(resource, hit.to, { api, notify, refresh });
  };
  const end = () => {
    dragged = null;
    mark(null);
    restore();
    doc.querySelectorAll?.('[data-dnd-resource].dnd-source-row').forEach((row) => row.classList.remove('dnd-source-row'));
  };
  const listeners = [['pointerdown', down], ['pointerup', restore], ['pointercancel', restore],
    ['dragstart', start], ['dragover', dragover], ['drop', drop], ['dragend', end]];
  for (const [type, fn] of listeners) doc.addEventListener(type, fn);
  return () => { for (const [type, fn] of listeners) doc.removeEventListener(type, fn); };
}
