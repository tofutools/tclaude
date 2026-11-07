// resource-dnd.js -- dragging a board, page or saved-process row of a
// group's Resources
// (groups-resources.js, [data-dnd-resource]) onto another group's box in the
// Groups tab moves it there (tcl-eq8pq9); a board dropped on Ungrouped
// leaves every group. With Ctrl/Cmd held, as for an agent's clone, a board
// or a process is copied there instead (resource-moves.js copyResource).
// The move is the ⚙ → Move to group one
// (resource-moves.js), so the drag offers only what that menu would. A row
// with a 🗑 (a board, a page, a running shell) shows the shared #dnd-trash
// bin while it is dragged, and a drop there is that 🗑: the row registered
// its remover (registerResourceRemover), which confirms first. It rides its
// own MIME type, which the member drag (dnd.js) ignores.

import { RESOURCE_DRAG_MIME, canCopyResource, canMoveResource, copyResource, moveResource } from './resource-moves.js';
import { clearDropBox, markDropBox, targetGroupOf } from './dnd-ops.js';
import { dashboardState } from './snapshot-store.js';
import { processContext, processRunApi } from './process-context.js';
import { defaultNotify } from './groups-quickstart.js';
import { shellConfirm } from './shell-state.js';

// removers maps a resource key (<kind>:<id>) to its row's 🗑, while the row
// is mounted.
const removers = new Map();

// registerResourceRemover routes a trash-bin drop of key's row to remove; it
// returns the unregister.
export function registerResourceRemover(key, remove) {
  removers.set(key, remove);
  return () => { if (removers.get(key) === remove) removers.delete(key); };
}

const trashOf = (e) => e.target.closest?.('#dnd-trash') || null;
const binOf = (doc) => doc.getElementById?.('dnd-trash') || null;
const removerOf = (resource) => (resource ? removers.get(`${resource.kind}:${resource.id}`) : null);

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
  // A drop asks first, as an agent's drop does.
  const confirm = deps.confirm || shellConfirm;
  let dragged = null;
  let over = null;
  // binShown is whether this drag showed the bin, so its end hides it even
  // when a move to a group box already let go of dragged.
  let binShown = false;
  // painted is the copy tint the last dragover showed: macOS Chrome clears
  // metaKey on the drop itself, as the agents' clone drag (dnd.js) found.
  let painted = false;
  let overCopy = false;
  const mark = (box, copy = false) => {
    if (over === box && overCopy === copy) return;
    clearDropBox(over);
    over = box;
    overCopy = copy;
    if (box) markDropBox(box, copy ? 'clone' : 'move');
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
    if (e.dataTransfer) e.dataTransfer.effectAllowed = 'copyMove';
    painted = false;
    row.classList.add('dnd-source-row');
    if (removerOf(dragged)) {
      // The bin says Delete (its template mode), not the agents' Retire.
      binOf(doc)?.classList.add('show', 'dnd-trash-template-mode');
      binShown = true;
    }
    // dragend goes to the source, which a re-render may have detached.
    row.addEventListener('dragend', end, { once: true });
  };
  const ours = (e) => !!e.dataTransfer?.types && Array.from(e.dataTransfer.types).includes(RESOURCE_DRAG_MIME);
  // target is what a drop here would do: { box, to, copy }. Ctrl/Cmd held
  // copies (a board or a process into a real group), as it clones an agent;
  // otherwise it moves.
  const target = (e, onDrop = false) => {
    if (!dragged || !ours(e)) return null;
    const box = e.target.closest?.(TARGET_SEL) || null;
    const to = dropGroupOf(box);
    if (to === null) return null;
    const copy = !!e.ctrlKey || !!e.metaKey || (onDrop && (painted || e.dataTransfer?.dropEffect === 'copy'));
    if (copy) return canCopyResource(dragged, to, snapshot()) ? { box, to, copy: true } : null;
    return canMoveResource(dragged, to, snapshot()) ? { box, to, copy: false } : null;
  };
  const dragover = (e) => {
    const trash = dragged && ours(e) && removerOf(dragged) ? trashOf(e) : null;
    if (dragged) binOf(doc)?.classList.toggle('dnd-drop-over', !!trash);
    if (trash) {
      mark(null);
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
      return;
    }
    const hit = target(e);
    mark(hit?.box || null, hit?.copy);
    painted = !!hit?.copy;
    if (!hit) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = hit.copy ? 'copy' : 'move';
  };
  const drop = (e) => {
    const remove = dragged && ours(e) && trashOf(e) ? removerOf(dragged) : null;
    if (remove) {
      e.preventDefault();
      e.stopPropagation();
      end();
      void remove();
      return;
    }
    const hit = target(e, true);
    const resource = dragged;
    mark(null);
    painted = false;
    if (!hit) return;
    e.preventDefault();
    e.stopPropagation();
    dragged = null;
    void (hit.copy ? copyResource : moveResource)(resource, hit.to, { api, notify, refresh, confirm });
  };
  const end = () => {
    if (binShown) binOf(doc)?.classList.remove('show', 'dnd-drop-over', 'dnd-trash-template-mode');
    binShown = false;
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
