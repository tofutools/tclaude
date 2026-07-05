// dock-dnd.js — drag a PALETTE DOCK card (a spawn profile, a role, or a group
// template) onto a group to open a prefilled dialog (JOH-375 2/4 + JOH-377 4/4).
//
// The dock (dock.js) renders profile/role/template cards as drag sources
// (draggable="true" + data-dock-kind/name). Dropping a PROFILE or ROLE onto a
// real group's box (its whole <details>, header or expanded body) opens the
// existing spawn modal (modal-spawn.js) with the target group pinned and the
// profile/role prefilled — no new spawn semantics, just a shortcut into the
// same dialog. Dropping a TEMPLATE onto a group opens the unified summon dialog
// (modal-templates.js) with a drop-mode chooser: reinforce the group in place,
// or spawn a NEW group in its image (JOH-377). Dropping onto the groups-list
// background (empty space, no group box) — OR onto the virtual UNGROUPED box,
// which literally means "no group" and is the discoverable target for the same
// gesture — opens the relevant dialog with the item prefilled but no group
// pinned (a plain spawn with the group picker left open / a plain "new party
// from circle" open). Every OTHER virtual group box (Retired / Conversations /
// Generations / Pending) stays inert — "spawn into Retired" is meaningless — so
// the drop targets are real groups, the Ungrouped box, and genuine empty space.
//
// Isolation from the two OTHER document-level DnD features is deliberate and
// total, mirroring how group-reorder.js coexists with dnd.js:
//   - This drag sets ONLY a custom MIME ('application/x-tclaude-dock-item'),
//     never 'text/plain'. dnd.js's member-drop handler bails on that exact MIME
//     up front (like it already does for the group-reorder MIME), so a dock
//     drop never reaches the member-move path.
//   - Each of the three modules gates its dragover/dragenter on its OWN active
//     flag (dockDragActive here, dndDragActive in dnd.js, groupReorderActive in
//     group-reorder.js) and each dragstart only fires for its own source
//     element — the flags are never set together. So the three feature modules'
//     document-level listeners coexist without stepping on each other.
//   - The hover highlight uses a DISTINCT class (.dock-drop-over), so dnd.js's
//     own dragleave/dragend — which strip only .dnd-drop-over — never fight it.
//
// Survives the 2s poll: dockDragActive is what refreshSuspended() reads to keep
// auto-refresh from rebuilding the Groups tab / dock mid-drag (which would
// detach the drag source or the drop target and lose the drag's own dragend
// cleanup). Set in dragstart, cleared in dragend (fires on drop AND on
// Escape-cancel), so the suspension covers the whole gesture.

import { $, $$ } from './helpers.js';
import { wizWord } from './slop.js';
import { openAgentSpawnModal } from './modal-spawn.js';
import { openSummonForDrop } from './modal-templates.js';

// Custom drag payload MIME. Intentionally NOT 'text/plain' — see the module
// header: withholding text/plain is what makes dnd.js's member-row drop handler
// bail on a dock drop (it also has an explicit guard on this MIME).
const DOCK_DRAG_MIME = 'application/x-tclaude-dock-item';
// The dock kinds that are drag sources: profiles + roles (JOH-375 2/4) and now
// templates (JOH-377 4/4). This set is the belt to dock.js's braces (its
// SECTIONS `drag` flag) — a card whose kind isn't here is ignored on dragstart
// even if some other code marked it draggable.
const DRAGGABLE_KINDS = new Set(['profiles', 'roles', 'templates']);

// dockDragActive mirrors dnd.js's dndDragActive / group-reorder's
// groupReorderActive: a live-binding flag refreshSuspended() reads so a 2s
// auto-refresh can't rebuild the DOM mid-drag. Exported as a `let` so importers
// see the updated value.
let dockDragActive = false;
// The payload of the card currently being dragged ({kind, name}), or null when
// idle. dragover/dragenter can't read the DataTransfer payload (browsers gate
// getData to the drop event), so we stash it here for the hover pill.
let dockDragItem = null;

// Real-group drop target: a group's <details> box (header or expanded body).
// Same boxes dnd.js's member drag targets, but only the REAL groups — a profile
// can't be spawned into the virtual Ungrouped / Retired groups.
const GROUP_TARGET_SEL = 'details[data-dnd-target-group]';
// Empty-space drop target: the groups-list container itself (a drop NOT over
// any group box → a plain spawn with the profile prefilled, no group).
const EMPTY_TARGET_SEL = '#groups-list';

// dockTarget resolves what the cursor is over during a dock drag:
//   { group: '<name>', box } when over a real group's box, or
//   { group: '', box }       when over the Ungrouped box or the empty space, or
//   null                     when over neither / an inert box (no drop here).
// A group box wins over the surrounding groups-list (closest() walks up from
// the cursor's element, hitting the inner <details> first).
function dockTarget(e) {
  const box = e.target.closest(GROUP_TARGET_SEL);
  if (box) return { group: box.getAttribute('data-dnd-target-group') || '', box };
  // The virtual UNGROUPED box means "no group", so it's a natural, discoverable
  // target for a no-group spawn — same result as an empty-space drop (group '')
  // but on an obvious surface rather than the background gaps. Light the box
  // itself so the affordance reads. Checked BEFORE the generic .group-virtual
  // inert guard below (the Ungrouped box carries both classes/attrs).
  const ungrouped = e.target.closest('details[data-dnd-target-ungrouped]');
  if (ungrouped) return { group: '', box: ungrouped };
  // Every OTHER virtual group box (Retired / Conversations / Generations /
  // Pending — all carry .group-virtual) is not a spawn target: "spawn into
  // Retired" is meaningless. Treat it as inert so hovering it neither flashes
  // the whole groups-list nor drops.
  if (e.target.closest('details.group-virtual')) return null;
  const list = e.target.closest(EMPTY_TARGET_SEL);
  if (list) return { group: '', box: list };
  return null;
}

// dockPill reuses the shared #dnd-pill chip (the member-row DnD's hint) to
// track the cursor with a spawn label. `text` null hides it.
function dockPill(e, text) {
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

// dockPillText composes the hover hint for a target. A template "deploys" a
// whole roster onto a group (or seeds a new party from empty space); a profile
// "spawns" and a role "summons as a class". Onto a group names the group, onto
// empty space says "(no group)".
function dockPillText(target) {
  if (!dockDragItem) return '';
  const { kind, name } = dockDragItem;
  if (kind === 'templates') {
    return target.group
      ? `→ ${wizWord('deploy', 'summon')} ${name} → ${target.group}`
      : `→ ${wizWord('new group from', 'new party from')} ${name}`;
  }
  const verb = kind === 'roles'
    ? wizWord('spawn with role', 'summon as class')
    : wizWord('spawn from', 'summon from');
  return target.group
    ? `→ ${verb} ${name} → ${target.group}`
    : `→ ${verb} ${name} ${wizWord('(no group)', '(no party)')}`;
}

// clearDockHighlights strips the hover class from every box that carries it.
function clearDockHighlights() {
  $$('.dock-drop-over').forEach(el => el.classList.remove('dock-drop-over'));
}

// endDockDrag is the single teardown for a dock drag: clear the active flag,
// the dragged-source dimming, the hover highlights and the pill. Idempotent, so
// calling it from both the drop handler and dragend is safe.
function endDockDrag() {
  // Clear the flag FIRST (mirrors dnd.js / group-reorder) so auto-refresh
  // always resumes even if a DOM call below were to throw.
  dockDragActive = false;
  dockDragItem = null;
  $$('.dock-card.dock-drag-source').forEach(c => c.classList.remove('dock-drag-source'));
  clearDockHighlights();
  dockPill(null, null);
}

function bindDockDnd() {
  // Gesture-scoped draggable suppression (mirrors dnd.js's row handling and
  // group-reorder's summary handling). The whole card is draggable, but a press
  // that lands on its ⚙ manage button (or any interactive child) must produce
  // that button's CLICK, not a drag. So disable the card's draggable for the
  // duration of such a gesture; a press on the bare card leaves it on.
  // pointerdown targets the actual element under the cursor and fires BEFORE the
  // drag is initiated. pointerup/pointercancel restore it immediately, so the
  // card is never left un-draggable between gestures.
  let suppressedCard = null;
  const restoreCardDraggable = () => {
    if (!suppressedCard) return;
    suppressedCard.draggable = true;
    suppressedCard = null;
  };
  document.addEventListener('pointerdown', (e) => {
    const card = e.target.closest('.dock-card[draggable="true"]');
    if (!card) return;
    const ctl = e.target.closest('button, a, input, select, textarea, label, [data-dock-act]');
    if (ctl && card.contains(ctl)) {
      card.draggable = false;
      suppressedCard = card;
    }
  });
  document.addEventListener('pointerup', restoreCardDraggable);
  document.addEventListener('pointercancel', restoreCardDraggable);

  document.addEventListener('dragstart', (e) => {
    const card = e.target.closest('.dock-card[draggable="true"]');
    if (!card) return;
    const kind = card.getAttribute('data-dock-kind');
    const name = card.getAttribute('data-dock-name');
    if (!kind || !name || !DRAGGABLE_KINDS.has(kind)) return;
    dockDragActive = true;
    dockDragItem = { kind, name };
    // Custom MIME ONLY — see the module header for why text/plain is withheld.
    // effectAllowed/dropEffect stay 'copy' (a drop spawns a new agent; it never
    // moves the palette card).
    e.dataTransfer.setData(DOCK_DRAG_MIME, JSON.stringify(dockDragItem));
    e.dataTransfer.effectAllowed = 'copy';
    card.classList.add('dock-drag-source');
  });

  // dragend is the guaranteed reset for EVERY drag-end outcome — a successful
  // drop, an Escape-cancel, or a release over nothing. The drop handler does
  // NOT re-render before it opens the modal, so (unlike group-reorder) the card
  // stays attached and this dragend always fires; it is the primary teardown.
  document.addEventListener('dragend', endDockDrag);

  document.addEventListener('dragover', (e) => {
    if (!dockDragActive) return;
    const target = dockTarget(e);
    // Repaint highlights from scratch each move so a box we've left goes dark
    // even if its dragleave was swallowed (Firefox occasionally drops the final
    // dragleave). Cheap: at most a handful of group boxes carry the class.
    clearDockHighlights();
    if (!target) {
      dockPill(e, null);
      return;
    }
    e.preventDefault(); // required for `drop` to fire on this element
    e.dataTransfer.dropEffect = 'copy';
    target.box.classList.add('dock-drop-over');
    dockPill(e, dockPillText(target));
  });

  // dragenter/dragleave are handled implicitly: dragover fully owns the
  // highlight (it repaints every move), so no separate enter/leave bookkeeping
  // — this sidesteps the classic child-element dragleave flicker entirely.

  document.addEventListener('drop', (e) => {
    if (!dockDragActive) return;
    const target = dockTarget(e);
    if (!target) return;
    e.preventDefault();
    // Read the payload from the DataTransfer (authoritative) with the stashed
    // item as a fallback; a browser that dropped the custom MIME still spawns.
    let item = dockDragItem;
    const raw = e.dataTransfer.getData(DOCK_DRAG_MIME);
    if (raw) {
      try { item = JSON.parse(raw); } catch (_) { /* keep the stashed item */ }
    }
    const group = target.group;
    // Tear down BEFORE opening the modal: the modal is a .modal-overlay, so once
    // it's up refreshSuspended() keeps auto-refresh parked on the modal instead
    // of the drag — but the drag flag must still be cleared so it doesn't wedge
    // if the modal is dismissed. endDockDrag is idempotent, so the dragend that
    // still fires afterwards is a harmless no-op.
    endDockDrag();
    if (!item || !item.name) return;
    // A TEMPLATE opens the unified summon dialog (JOH-377). Onto a group it
    // shows the drop-mode chooser (reinforce in place / new group in its image);
    // onto empty space it opens the plain "new party from circle" flow (the same
    // as the templates-manage 🚀 button). No new endpoint — the dialog dispatches
    // to the existing reinforce / instantiate / deploy paths by mode.
    if (item.kind === 'templates') {
      openSummonForDrop(item.name, group);
      return;
    }
    // Prefill grammar: a profile preselects the spawn Profile (JOH-350/210), a
    // role presets the Role field. A group pins the target; empty space leaves
    // the group picker (a plain spawn). No new spawn semantics — this is the
    // existing dialog opened with arguments.
    const opts = group ? { groupName: group } : {};
    if (item.kind === 'roles') opts.role = item.name;
    else opts.profileName = item.name;
    openAgentSpawnModal(opts);
  });
}

export { bindDockDnd, dockDragActive };
