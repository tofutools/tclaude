// group-reorder.js — drag-to-reorder AND drag-to-nest the REAL groups in the
// Groups tab (n-level groups-in-groups, JOH-392).
//
// A real group's HEADER (its <summary>, carrying data-group-reorder +
// draggable, see groups-list.js) is the drag handle: press the bare header and drag.
// Where you DROP on a target group decides the gesture (see dropZone):
//   - onto the BULK of the target box (most of the header + its whole expanded
//     body — a big, forgiving zone like the member-row DnD's whole-box target)
//     → NEST the dragged group inside it
//   - onto the thin top/bottom EDGE strip of the box → place it as a SIBLING
//     before/after the target, adopting the target's OWN parent — so dropping a
//     nested group onto a TOP-LEVEL group's edge pulls it back out to top level.
// Sibling order is persisted as a JSON array of group names in dashPrefs under
// GROUP_ORDER_KEY (a pure browser-view concern, every depth ordered by this one
// flat list). The parent edge is server state (agent_groups.parent_id): a nest/
// un-nest drop PUTs /api/groups/{name}/parent, the only server round-trip here.
//
// The header also holds CLICK targets — the title (.group-name, click to
// fold/unfold), the click-to-edit chips (descr / cwd / cap / profile, all
// data-act) and the link chips. Native DnD starts the drag on the draggable
// ANCESTOR, so a press-with-wobble on any of those would otherwise start a
// reorder drag and eat the click (and once a native drag begins, the click
// is gone for good — there's no minimum-distance threshold to lean on). So a
// pointerdown over an interactive descendant turns the summary's draggable
// OFF for that gesture (the click lands); a press on the bare header leaves
// it ON, making that empty space the drag handle. This mirrors the
// gesture-scoped suppression dnd.js's bindDnd does for member rows; the
// implementation here is the pointerdown handler in bindGroupReorder below.
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
// mirroring groups-list.js/dashboard.js: neither module reads the other's
// export at evaluation time — renderGroupsTab is called on drop and
// sortGroupsByPref on render, both long after every module finishes
// loading.

import { $, $$ } from './helpers.js';
import { setGroupOrderPref, sortGroupsByPref } from './group-order.js';
import { renderGroupsTab } from './tabs.js';
import { lastSnapshot } from './dashboard.js';
import { refresh, toast } from './refresh.js';
import { openDeleteGroupModal } from './dashboard-operations.js';
import { isWizardActive } from './slop.js';

// Custom drag payload type. Intentionally NOT 'text/plain' — see the
// module header: withholding text/plain is what makes dnd.js's member-row
// drop handler bail on a group-reorder drop.
const GROUP_DRAG_MIME = 'application/x-tclaude-group';

// Live routing state shared with reverse dock capture. Keyed Preact group
// nodes survive snapshot publishes during the gesture.
let groupReorderActive = false;
let groupDragHandle = null;
// The name of the group currently being dragged (null when idle). Read by
// the dragover pill + drop handler; dragover can't read the DataTransfer
// payload (browsers gate getData to the drop event), so we stash it here.
let groupDragName = null;


// reorderTarget resolves the real-group <details> under the drag cursor,
// or null when the cursor is over a virtual group (fixed position — not a
// reorder target) or outside any group.
function reorderTarget(e) {
  const details = e.target.closest('details[data-group-key]');
  if (!details || details.classList.contains('group-virtual')) return null;
  return details;
}

function groupTrashTarget(e) {
  return e.target.closest('#dnd-trash');
}

// clearDropMarkers strips the insertion-line + nest-target classes from every
// group.
function clearDropMarkers() {
  $$('.group-drop-before, .group-drop-after, .group-drop-into').forEach(d =>
    d.classList.remove('group-drop-before', 'group-drop-after', 'group-drop-into'));
}

// snapshotGroupsByName returns a name→group map of the current snapshot's real
// groups, or null when the snapshot isn't ready.
function snapshotGroupsByName() {
  if (!lastSnapshot || !Array.isArray(lastSnapshot.groups)) return null;
  return new Map(lastSnapshot.groups.map(g => [g.name, g]));
}

// isDescendantOrSelf reports whether `name` is `ancestor` itself or sits
// anywhere in the subtree BELOW `ancestor` — walking UP from `name` via the
// snapshot's parent edges until a root. A visited set bounds the walk so a
// pre-existing corrupt loop still terminates. Used to reject a drop that would
// nest a group under itself or one of its own descendants (a cycle).
function isDescendantOrSelf(name, ancestor, byName) {
  let cur = name;
  const seen = new Set();
  while (cur && !seen.has(cur)) {
    if (cur === ancestor) return true;
    seen.add(cur);
    cur = (byName.get(cur) || {}).parent || '';
  }
  return false;
}

// REORDER_EDGE is the height (px) of the thin strip at the very top / bottom of
// a group box reserved for sibling REORDER; everything between is a generous
// NEST zone. Capped at a third of the box so even a short collapsed header
// still yields all three zones.
const REORDER_EDGE = 12;

// dropZone classifies a drag over a target group's box into one of three
// gestures, measured against the WHOLE <details> box (like the member-row DnD's
// whole-box target — a big, forgiving drop area):
//   'before' — cursor in the box's top edge strip → sibling, before target
//   'after'  — cursor in the box's bottom edge strip → sibling, after target
//   'nest'   — anywhere in between (the bulk of the box, header + expanded body)
//              → child OF target
// The big middle makes "drag INTO a group" easy to hit; the thin edge strips
// keep sibling reorder — and give "drag back OUT" (dropping a nested group onto
// a top-level group's edge re-parents it to top level). When a group is
// expanded, `closest` resolves to the INNERMOST group box under the cursor, so
// hovering a child's area nests into the child, not the parent — the natural
// tree behaviour.
function dropZone(e, details) {
  const r = details.getBoundingClientRect();
  const edge = Math.min(REORDER_EDGE, r.height / 3);
  if (e.clientY < r.top + edge) return 'before';
  if (e.clientY > r.bottom - edge) return 'after';
  return 'nest';
}

// resolveDrop turns a (dragName, targetName, zone) gesture into the concrete
// mutation: the desired parent (nest → target; edge → target's OWN parent, so
// dropping beside a top-level group lands the dragged group at top level) and
// the reorder side. Returns null when the drop is a no-op or would form a cycle.
function resolveDrop(dragName, targetName, zone, byName) {
  if (!dragName || !targetName || dragName === targetName) return null;
  const target = byName.get(targetName);
  if (!target) return null;
  const desiredParent = zone === 'nest' ? targetName : (target.parent || '');
  // Cycle guard: the new parent must not be the dragged group itself nor any
  // group inside the dragged group's own subtree.
  if (desiredParent && isDescendantOrSelf(desiredParent, dragName, byName)) return null;
  return { desiredParent, before: zone === 'before' };
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

// persistSiblingOrder mutates the persisted flat order so `dragName` lands
// before/after `targetName`. It computes the new order from the FULL snapshot's
// effective order (not the possibly-filtered DOM), so reordering while a text
// filter is active never drops the hidden groups out of the saved order. The
// flat order governs sibling order WITHIN each parent scope at render time
// (siblings compare by flat index), so this one list serves every depth.
function persistSiblingOrder(dragName, targetName, before) {
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
}

// applyGroupDrop is the single drop resolver for the group tree. It positions
// `dragName` relative to `targetName` in the flat order (client dashPref) AND,
// when the gesture changes the dragged group's parent (nest into a group, or
// drop beside a top-level group to pull it back out), PUTs the new parent to
// the server. The parent write is the only server round-trip; sibling order
// stays a pure browser-view concern.
function applyGroupDrop(dragName, targetName, zone) {
  const byName = snapshotGroupsByName();
  if (!byName) return;
  const plan = resolveDrop(dragName, targetName, zone, byName);
  if (!plan) return; // no-op or would-be cycle
  const curParent = (byName.get(dragName) || {}).parent || '';
  const parentChanged = curParent !== plan.desiredParent;

  // Sibling order first (synchronous, client-only) so a subsequent server
  // refresh honours it.
  persistSiblingOrder(dragName, targetName, plan.before);

  if (!parentChanged) {
    renderGroupsTab();
    return;
  }
  // Re-parent on the server; the response-driven refresh re-lays-out the tree.
  fetch(`/api/groups/${encodeURIComponent(dragName)}/parent`, {
    method: 'PUT', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ parent: plan.desiredParent }),
  }).then(async (r) => {
    if (!r.ok) {
      toast(`nest failed: ${await r.text()}`, true);
      renderGroupsTab(); // roll the view back to server truth
      return;
    }
    toast(plan.desiredParent ? `${dragName}: nested under ${plan.desiredParent}` : `${dragName}: moved to top level`);
    refresh();
  }).catch((err) => {
    toast(`nest failed: ${(err && err.message) || err}`, true);
    renderGroupsTab();
  });
}

// endGroupDrag is the single teardown for a reorder drag: clear the active
// flag, the dragged-source dimming, the drop-line markers and the pill.
// Idempotent, so calling it from both the drop handler and dragend is safe.
function endGroupDrag() {
  // Clear the flag FIRST (mirrors dnd.js) so later document events cannot be
  // misrouted even if a DOM call below were to throw.
  groupReorderActive = false;
  groupDragName = null;
  groupDragHandle?.removeEventListener('dragend', endGroupDrag);
  groupDragHandle = null;
  $$('.group-reorder-source').forEach(d => d.classList.remove('group-reorder-source'));
  clearDropMarkers();
  const trash = $('#dnd-trash');
  if (trash) trash.classList.remove('show', 'dnd-drop-over');
  reorderPill(null, null);
}

function bindGroupReorder() {
  const removers = [];
  const listen = (target, type, listener, options) => {
    target.addEventListener(type, listener, options);
    removers.push(() => target.removeEventListener(type, listener, options));
  };
  // Gesture-scoped draggable suppression (mirrors dnd.js's row handling).
  // The whole group <summary> is draggable, but a press that lands on an
  // interactive child — the title, a click-to-edit chip, a link chip, any
  // button — must produce that child's CLICK, not a reorder drag. So disable
  // the summary's draggable for the duration of such a gesture; a press on
  // the bare header leaves it on. pointerdown targets the actual element
  // under the cursor and fires BEFORE the drag is initiated, so it's the
  // right place to decide. pointerup/pointercancel restore it immediately, so
  // the header is never left un-draggable between gestures.
  let suppressedSummary = null;
  const restoreSummaryDraggable = () => {
    if (!suppressedSummary) return;
    suppressedSummary.draggable = true;
    suppressedSummary = null;
  };
  listen(document, 'pointerdown', (e) => {
    const summary = e.target.closest('summary[data-group-reorder]');
    if (!summary) return;
    const ctl = e.target.closest('button, a, input, select, textarea, label, [data-act], [contenteditable], .group-name');
    if (ctl && summary.contains(ctl)) {
      summary.draggable = false;
      suppressedSummary = summary;
    }
  });
  listen(document, 'pointerup', restoreSummaryDraggable);
  listen(document, 'pointercancel', restoreSummaryDraggable);

  listen(document, 'dragstart', (e) => {
    // The drag handle is the group header (<summary> with data-group-reorder);
    // match on the attribute so the source element can change without touching
    // this code.
    const handle = e.target.closest('[data-group-reorder]');
    if (!handle) return;
    const name = handle.getAttribute('data-group-reorder');
    if (!name) return;
    groupReorderActive = true;
    groupDragName = name;
    groupDragHandle?.removeEventListener('dragend', endGroupDrag);
    groupDragHandle = handle;
    handle.addEventListener('dragend', endGroupDrag, { once: true });
    // Custom MIME ONLY — see the module header for why text/plain is
    // withheld. effectAllowed/dropEffect stay 'move' (reorder, never copy).
    e.dataTransfer.setData(GROUP_DRAG_MIME, name);
    e.dataTransfer.effectAllowed = 'move';
    const details = handle.closest('details[data-group-key]');
    if (details) details.classList.add('group-reorder-source');
    const trash = $('#dnd-trash');
    if (trash) trash.classList.add('show');
  });

  // dragend is the guaranteed reset for a CANCELLED or no-target drag
  // (Escape, or a release over nothing). A SUCCESSFUL drop tears down in the
  // drop handler instead (see there) before keyed reconciliation moves the
  // source or, for a re-parent, may replace its header. Browser dragend
  // delivery after that synchronous update is not a reliable primary path.
  listen(document, 'dragend', endGroupDrag);

  listen(document, 'dragover', (e) => {
    if (!groupReorderActive) return;
    const trash = groupTrashTarget(e);
    if (trash) {
      clearDropMarkers();
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      trash.classList.add('dnd-drop-over');
      reorderPill(e, isWizardActive() ? `↓ disband party ${groupDragName}` : `↓ delete group ${groupDragName}`);
      return;
    }
    const details = reorderTarget(e);
    clearDropMarkers();
    if (!details) {
      reorderPill(e, null);
      return;
    }
    const targetName = details.getAttribute('data-group-key');
    const byName = snapshotGroupsByName();
    const zone = dropZone(e, details);
    // Reject up front (no preventDefault ⇒ no drop) a gesture that resolves to
    // nothing or a cycle — e.g. dropping a group onto itself, or nesting under
    // one of its own descendants. The marker/pill only show for a valid drop.
    const plan = byName ? resolveDrop(groupDragName, targetName, zone, byName) : null;
    if (!plan) {
      reorderPill(e, null);
      return;
    }
    e.preventDefault(); // required for `drop` to fire on this element
    e.dataTransfer.dropEffect = 'move';
    if (zone === 'nest') {
      details.classList.add('group-drop-into');
      reorderPill(e, `⤵ nest ${groupDragName} in ${targetName}`);
    } else {
      details.classList.add(plan.before ? 'group-drop-before' : 'group-drop-after');
      const curParent = (byName.get(groupDragName) || {}).parent || '';
      // Signal a re-parent (drag OUT of / across subtrees) distinctly from a
      // plain same-scope reorder.
      reorderPill(e, curParent !== plan.desiredParent
        ? (plan.desiredParent ? `↕ move ${groupDragName} beside ${targetName}` : `⤴ ${groupDragName} → top level`)
        : `↕ reorder ${groupDragName}`);
    }
  });

  listen(document, 'dragleave', (e) => {
    if (!groupReorderActive) return;
    const trash = groupTrashTarget(e);
    if (!trash) return;
    if (trash.contains(e.relatedTarget)) return;
    trash.classList.remove('dnd-drop-over');
  });

  listen(document, 'drop', (e) => {
    if (!groupReorderActive) return;
    const trash = groupTrashTarget(e);
    if (trash) {
      e.preventDefault();
      const dragName = groupDragName;
      endGroupDrag();
      openDeleteGroupModal(dragName);
      return;
    }
    const details = reorderTarget(e);
    if (!details) return;
    e.preventDefault();
    // Snapshot everything we need from the live DOM BEFORE teardown and
    // reconciliation: the target name and the drop zone (measured against the
    // current target box).
    const dragName = groupDragName;
    const targetName = details.getAttribute('data-group-key');
    const zone = dropZone(e, details);
    // Tear down NOW, before applyGroupDrop reconciles the keyed tree and moves
    // or replaces the dragged header. Leaving cleanup to browser dragend could
    // strand the pill and route later document events through stale drag state.
    // endGroupDrag is idempotent, so a dragend that does fire is harmless.
    endGroupDrag();
    applyGroupDrop(dragName, targetName, zone);
  });

  let cleaned = false;
  return () => {
    if (cleaned) return;
    cleaned = true;
    for (const remove of removers.splice(0).reverse()) remove();
    restoreSummaryDraggable();
    endGroupDrag();
  };
}

export { bindGroupReorder, sortGroupsByPref, groupReorderActive };
