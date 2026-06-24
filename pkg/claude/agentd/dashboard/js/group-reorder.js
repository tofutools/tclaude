// group-reorder.js — drag-to-reorder the REAL groups in the Groups tab.
//
// A real group's title (.group-name, carrying data-group-reorder +
// draggable, see render.js) IS the reorder drag handle. Dragging it
// reorders that group relative to the other real groups; a clean click on
// the same title still folds/unfolds the group (bindGroupTitleToggle in
// refresh.js) — the browser only starts a drag on movement, so the two
// gestures don't collide. The order is persisted as a JSON array of group
// names in dashPrefs under GROUP_ORDER_KEY.
//
// Why a dashPref and not a server column? Group display order is a
// dashboard *presentation* concern — the same kind as the per-group
// expand/collapse state, which already lives in dashPrefs
// ('tclaude.dash.group.<name>'). It is the human's own browser-view
// layout, no business of an agent, so it stays client-side: no /v1 twin,
// no schema migration, no change to the CLI's alphabetical `groups ls`.
// dashPrefs is SQLite-backed (prefs.js → /api/dashboard/prefs), so the
// order survives daemon restarts, browser profiles and multiple tabs.
//
// Isolation from the member-row drag-and-drop in dnd.js is deliberate and
// total. This drag sets ONLY a custom MIME ('application/x-tclaude-group')
// — never 'text/plain'. dnd.js's drop handler checks for that exact MIME
// up front and returns, so a reorder drop never reaches the member-move
// path. The two modules also use separate active flags (groupReorderActive
// here, dndDragActive there) which are never set together — each module's
// dragstart only fires for its own source element — and dnd.js's
// dragover/dragenter self-gate on dndDragActive. So the two feature
// modules' document-level listeners coexist without stepping on each other.
//
// Deliberate, benign import cycle with tabs.js (group-reorder ↔ tabs),
// mirroring render.js/dashboard.js: neither module reads the other's
// export at evaluation time — renderGroupsTab is called on drop and
// sortGroupsByPref on render, both long after every module finishes
// loading.

import { $, $$ } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { renderGroupsTab } from './tabs.js';
import { lastSnapshot } from './dashboard.js';

const GROUP_ORDER_KEY = 'tclaude.dash.groupOrder';
// Custom drag payload type. Intentionally NOT 'text/plain' — see the
// module header: withholding text/plain is what makes dnd.js's member-row
// drop handler bail on a group-reorder drop.
const GROUP_DRAG_MIME = 'application/x-tclaude-group';

// groupReorderActive mirrors dnd.js's dndDragActive: a live-binding flag
// refreshSuspended() reads so a 2s auto-refresh can't rebuild the Groups
// tab DOM mid-drag (which would detach the dragged grip and lose the
// drag's own dragend cleanup). Exported as a `let` so importers see the
// updated value.
let groupReorderActive = false;
// The name of the group currently being dragged (null when idle). Read by
// the dragover pill + drop handler; dragover can't read the DataTransfer
// payload (browsers gate getData to the drop event), so we stash it here.
let groupDragName = null;

// groupOrderPref returns the saved order as an array of group-name
// strings, or null when nothing is saved / the value is malformed.
function groupOrderPref() {
  const raw = dashPrefs.getItem(GROUP_ORDER_KEY);
  if (!raw) return null;
  try {
    const arr = JSON.parse(raw);
    if (!Array.isArray(arr)) return null;
    return arr.filter(n => typeof n === 'string');
  } catch (_) {
    return null;
  }
}

// setGroupOrderPref persists the given ordered name list. dashPrefs guards
// unchanged values, so re-saving an identical order is a no-op write.
function setGroupOrderPref(names) {
  dashPrefs.setItem(GROUP_ORDER_KEY, JSON.stringify(names));
}

// sortGroupsByPref returns a NEW array of the given real groups ordered by
// the saved preference. Groups named in the saved order sort by their
// saved index; groups absent from it (created since the last reorder, or
// before the human ever reordered) keep their incoming relative order,
// placed AFTER every saved one — so a brand-new group lands at the bottom
// of the custom order rather than jumping to the top. A saved name with no
// matching group is simply ignored. Stable: equal ranks fall back to the
// incoming index. Exported so renderGroupsTab applies the very order the
// drop handler persists.
function sortGroupsByPref(groups) {
  const order = groupOrderPref();
  if (!order || !order.length) return groups;
  const rank = new Map();
  order.forEach((name, i) => { if (!rank.has(name)) rank.set(name, i); });
  return groups
    .map((g, i) => ({ g, i }))
    .sort((a, b) => {
      const ra = rank.has(a.g.name) ? rank.get(a.g.name) : Infinity;
      const rb = rank.has(b.g.name) ? rank.get(b.g.name) : Infinity;
      return ra === rb ? a.i - b.i : ra - rb;
    })
    .map(x => x.g);
}

// reorderTarget resolves the real-group <details> under the drag cursor,
// or null when the cursor is over a virtual group (fixed position — not a
// reorder target) or outside any group.
function reorderTarget(e) {
  const details = e.target.closest('details[data-group-key]');
  if (!details || details.classList.contains('group-virtual')) return null;
  return details;
}

// dropsBefore reports whether a drop on `details` should land the dragged
// group BEFORE it (cursor in the top half) or AFTER it (bottom half).
// Measured against the WHOLE <details> box, not just its <summary> header,
// so an EXPANDED group's full area counts: hovering low over its open body
// drops AFTER the entire group, and the after-indicator (drawn on the
// <details>, see the CSS) sits below the expanded body — not tucked under
// the header. A collapsed group's box is just its header, so it behaves the
// same as before.
function dropsBefore(e, details) {
  const rect = details.getBoundingClientRect();
  return e.clientY < rect.top + rect.height / 2;
}

// clearDropMarkers strips the insertion-line classes from every group.
function clearDropMarkers() {
  $$('.group-drop-before, .group-drop-after').forEach(d =>
    d.classList.remove('group-drop-before', 'group-drop-after'));
}

// reorderPill reuses the shared #dnd-pill chip (the member-row DnD's hint)
// to track the cursor with a reorder label. `text` null hides it.
function reorderPill(e, text) {
  const pill = $('#dnd-pill');
  if (!pill) return;
  if (!text) {
    pill.classList.remove('show', 'clone');
    return;
  }
  pill.textContent = text;
  pill.classList.remove('clone');
  pill.classList.add('show');
  pill.style.transform = `translate(${e.clientX + 12}px, ${e.clientY + 12}px)`;
}

// applyReorder mutates the persisted order so `dragName` lands before/after
// `targetName`, then re-renders the tab. It computes the new order from the
// FULL snapshot's effective order (not the possibly-filtered DOM), so
// reordering while a text filter is active never drops the hidden groups
// out of the saved order.
function applyReorder(dragName, targetName, before) {
  if (!dragName || !targetName || dragName === targetName) return;
  if (!lastSnapshot || !Array.isArray(lastSnapshot.groups)) return;
  const names = sortGroupsByPref(lastSnapshot.groups.slice()).map(g => g.name);
  const from = names.indexOf(dragName);
  if (from < 0) return;
  names.splice(from, 1);
  let to = names.indexOf(targetName);
  if (to < 0) return; // target vanished between drag and drop
  if (!before) to += 1;
  names.splice(to, 0, dragName);
  setGroupOrderPref(names);
  renderGroupsTab();
}

// endGroupDrag is the single teardown for a reorder drag: clear the active
// flag, the dragged-source dimming, the drop-line markers and the pill.
// Idempotent, so calling it from both the drop handler and dragend is safe.
function endGroupDrag() {
  // Clear the flag FIRST (mirrors dnd.js) so auto-refresh always resumes
  // even if a DOM call below were to throw.
  groupReorderActive = false;
  groupDragName = null;
  $$('.group-reorder-source').forEach(d => d.classList.remove('group-reorder-source'));
  clearDropMarkers();
  reorderPill(null, null);
}

function bindGroupReorder() {
  document.addEventListener('dragstart', (e) => {
    // The drag handle is the group title (.group-name with
    // data-group-reorder); match on the attribute so the source element can
    // change without touching this code.
    const handle = e.target.closest('[data-group-reorder]');
    if (!handle) return;
    const name = handle.getAttribute('data-group-reorder');
    if (!name) return;
    groupReorderActive = true;
    groupDragName = name;
    // Custom MIME ONLY — see the module header for why text/plain is
    // withheld. effectAllowed/dropEffect stay 'move' (reorder, never copy).
    e.dataTransfer.setData(GROUP_DRAG_MIME, name);
    e.dataTransfer.effectAllowed = 'move';
    const details = handle.closest('details[data-group-key]');
    if (details) details.classList.add('group-reorder-source');
  });

  // dragend is the guaranteed reset for a CANCELLED or no-target drag
  // (Escape, or a release over nothing). A SUCCESSFUL drop tears down in the
  // drop handler instead (see there) — it must, because that handler
  // re-renders #groups-list and detaches the dragged grip, after which a
  // dragend dispatched on the now-detached node never bubbles to this
  // document-level listener. So this is a fallback, not the primary path.
  document.addEventListener('dragend', endGroupDrag);

  document.addEventListener('dragover', (e) => {
    if (!groupReorderActive) return;
    const details = reorderTarget(e);
    clearDropMarkers();
    if (!details) {
      reorderPill(e, null);
      return;
    }
    e.preventDefault(); // required for `drop` to fire on this element
    e.dataTransfer.dropEffect = 'move';
    const before = dropsBefore(e, details);
    // No indicator when the result wouldn't move anything (dropping a
    // group onto the gap it already occupies).
    const targetName = details.getAttribute('data-group-key');
    if (targetName === groupDragName) {
      reorderPill(e, null);
      return;
    }
    details.classList.add(before ? 'group-drop-before' : 'group-drop-after');
    reorderPill(e, `↕ reorder ${groupDragName}`);
  });

  document.addEventListener('drop', (e) => {
    if (!groupReorderActive) return;
    const details = reorderTarget(e);
    if (!details) return;
    e.preventDefault();
    // Snapshot everything we need from the live DOM BEFORE tearing down /
    // re-rendering: the target name, and the drop side (measured against the
    // still-attached summary rect).
    const dragName = groupDragName;
    const targetName = details.getAttribute('data-group-key');
    const before = dropsBefore(e, details);
    // Tear down NOW, before applyReorder re-renders #groups-list and detaches
    // the dragged grip. If we left teardown to dragend, that event — fired on
    // the detached grip — would never bubble here, so the pill would stay
    // stuck and groupReorderActive would wedge auto-refresh on. endGroupDrag
    // is idempotent, so a dragend that does still fire is a harmless no-op.
    endGroupDrag();
    applyReorder(dragName, targetName, before);
  });
}

export { bindGroupReorder, sortGroupsByPref, groupReorderActive };
