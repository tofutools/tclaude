// dock-actions.js -- the Groups toolbar's global controls live in the side
// panel's head while the Groups panel is fully open (JOH-390 item 4,
// tcl-2k2om1): the "+ new group" primary, the ⚙ more-actions cog (+ its
// .action-menu), the queued-spawns control, and the 🧠 dashboard-default-profile
// chip and its 🛡 sandbox-profile peer. Row 1 of the dock head is new-group +
// cog; row 2 the two profile controls. Folded, they go back to their exact
// toolbar slots so the filter bar renders as before.
//
// "Fully open" is pinned open (body.dock-open) or floated in by the edge
// reveal (body.dock-revealed, side-panel-reveal.js); a peek (body.dock-peeking)
// shows only the panel's leading edge and moves nothing.
//
// We MOVE the live DOM nodes (not clones), so every listener rides along:
// id-bound ones (#group-create-open's click), the cog's direct menu listener,
// and the profile chips' cross-feature action route all remain valid. The
// toolbar filter bar + the dock head are both STATIC markup (Preact owns
// #dock-body and #groups-list), so nothing re-creates or clobbers the moved
// nodes. A focused control keeps its focus across the move. One being typed
// in or picked from (the profile chips' <select> editor, which closes on blur)
// is not moved from under the user: the move waits for focus to leave it.
//
// The cog's .action-menu still anchors to .filter-bar-cog (position:relative
// rides along) and opens downward INTO the dock body; at the header's top it
// stays within #agent-dock's box, so .dock-inner's overflow:hidden never clips
// it.

const DOCK_ACTION_SPECS = [
  { sel: '#group-create-open', dock: '#dock-actions-primary' },
  { sel: '.filter-bar-cog', dock: '#dock-actions-primary' },
  { sel: '#queued-spawns-control', dock: '#dock-actions-primary' },
  { sel: '#dashboard-default-profile-control', dock: '#dock-actions-profile' },
  { sel: '#dashboard-default-sandbox-profile-control', dock: '#dock-actions-profile' },
];

// dockActionsHomed is whether the controls belong in the dock head: the Groups
// panel is the one showing, pinned open or revealed.
export function dockActionsHomed(doc = globalThis.document) {
  const body = doc?.body;
  if (!body || body.dataset.dockPanel !== 'groups') return false;
  return body.classList.contains('dock-open') || body.classList.contains('dock-revealed');
}

const state = new WeakMap(); // doc -> { homes, last }

// entry is a control the user types in or picks from.
function entry(el) {
  const tag = el.tagName;
  if (el.isContentEditable || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return tag === 'INPUT' && !/^(button|checkbox|color|file|hidden|image|radio|range|reset|submit)$/.test(el.type || '');
}

// syncDockActions moves the controls to match dockActionsHomed. It acts only
// when that flipped: the class observer fires on every body-class mutation
// (hscroll flags, dock-anim, wizard, …), so the guard keeps those to a cheap
// no-op and prevents a redundant re-append that could reorder the primary row.
export function syncDockActions(doc = globalThis.document) {
  if (!doc?.body) return;
  let st = state.get(doc);
  if (!st) state.set(doc, st = { homes: null, last: null });
  const homed = dockActionsHomed(doc);
  if (homed === st.last) return;
  // Capture each control's ORIGINAL toolbar slot once, before the first move.
  // The nextSibling anchors are the inter-control whitespace text nodes, which
  // never move, so insertBefore restores the exact slot regardless of the
  // order we process the controls in.
  if (!st.homes) {
    st.homes = DOCK_ACTION_SPECS.map((s) => {
      const el = doc.querySelector(s.sel);
      if (!el) return null;
      return { el, dock: doc.querySelector(s.dock), home: el.parentNode, next: el.nextSibling };
    });
  }
  // Moving a node drops its focus; put it back where it went. A text entry or
  // select would close or lose its draft, so wait for focusout instead.
  const focused = doc.activeElement;
  if (focused && entry(focused) && st.homes.some((h) => h?.el?.contains(focused))) return;
  st.last = homed;
  let refocus = null;
  for (const h of st.homes) {
    if (!h?.el) continue;
    const before = h.el.parentNode;
    // Append in spec order → primary row becomes [+ new group][⚙ cog]. Guarded
    // so a repeat call (idempotent re-apply) doesn't re-append and reorder.
    if (homed) {
      if (h.dock && before !== h.dock) h.dock.appendChild(h.el);
    } else if (h.home && before !== h.home) {
      h.home.insertBefore(h.el, h.next?.parentNode === h.home ? h.next : null);
    }
    if (h.el.parentNode !== before && focused && h.el.contains(focused)) refocus = focused;
  }
  if (refocus && doc.activeElement !== refocus) refocus.focus?.({ preventScroll: true });
}

// bindDockActions follows the body's classes, so the move can never desync
// from the visible open state, whichever path changed it.
export function bindDockActions(doc = globalThis.document) {
  const MO = (doc.defaultView || globalThis).MutationObserver;
  if (!doc?.body || !MO) return;
  new MO(() => syncDockActions(doc)).observe(doc.body, { attributes: true, attributeFilter: ['class', 'data-dock-panel'] });
  // A move that waited on a focused entry happens once focus has left it.
  doc.addEventListener('focusout', () => queueMicrotask(() => syncDockActions(doc)));
  syncDockActions(doc);
}
