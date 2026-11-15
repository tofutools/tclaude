// phone-history.js -- Back on a phone does what the screen suggests.
//
// The router (nav-history.js) records tabs and their routed views. On a phone
// three more things sit over a tab and look like places a Back should leave:
//   - a bottom sheet (phone-sheet.js), such as More or an agent's actions;
//   - a page a tab view pushes over its list (phone-page.js) without a route
//     of its own, such as an automation or a process run;
//   - the shell's own views, Now and Board, which cover whatever tab is under
//     them.
// Each is an overlay of the router (overlay-routes.js), so it is a history
// step: opening one records an entry, Back closes it, and closing it from the
// screen (‹, the scrim, another tab) steps back off its entry.
//
//   ?phone=<n>   how many layers are up: a pushed page, a sheet over it. A
//                layer cannot be reopened from a link, so Forward onto one
//                leaves it closed.
//   ?own=now|board   the shell's own view.
//
// A page with a route of its own (Mail's reader, a Settings section) says
// `routed: true` and is left to the router.

import { effect } from '@preact/signals';
import { announceOverlay, registerOverlayRoute } from './overlay-routes.js';
import { phonePage } from './phone-page.js';
import { phoneSheet, requestClosePhoneSheet } from './phone-sheet.js';

export const OWN_VIEW_VALUES = Object.freeze(['now', 'board']);

// layerDepth is how many layers are up: an unrouted page, then a sheet.
export function layerDepth(page = phonePage.peek(), sheet = phoneSheet.peek()) {
  return (page && !page.routed ? 1 : 0) + (sheet ? 1 : 0);
}

function booting(doc) {
  return !!doc?.body?.classList?.contains('dashboard-booting');
}

// whenBooted runs `run` once the dashboard has finished booting (the router
// is up then). Returns a cancel function.
function whenBooted(doc, run) {
  if (!booting(doc)) { const timer = setTimeout(run, 0); return () => clearTimeout(timer); }
  const Observer = doc.defaultView?.MutationObserver;
  if (!Observer) return () => {};
  const watch = new Observer(() => { if (!booting(doc)) { watch.disconnect(); run(); } });
  watch.observe(doc.body, { attributes: true, attributeFilter: ['class'] });
  return () => watch.disconnect();
}

// kept is the own view Back or Forward restored, and the tab it was
// restored over: the shell clears its view on a tab change except on that
// one (Back onto Now over Mail shows Now, not Mail).
let kept = null;

// keptView is the own view Back restored over `tab`, or null; asking spends
// it, so a later tab change clears the view as usual.
export function keptView(tab) {
  const was = kept;
  kept = null;
  return was && was.tab === tab ? was.view : null;
}

// installPhoneHistory wires the layers and `view` (the shell's own-view
// signal) to the router; `tab` reads the active tab. Returns the uninstall
// function.
export function installPhoneHistory({ view, tab = () => null, doc = globalThis.document }) {
  let restoring = false;
  const unLayers = registerOverlayRoute('phone', {
    value: () => { const depth = layerDepth(); return depth ? String(depth) : ''; },
    restore: (want) => {
      const depth = Number(want) || 0;
      restoring = true;
      let held = false;
      try {
        // Back is a close the operator asked for: a sheet's guard may hold
        // it (a draft to ask about, a send in flight).
        if (layerDepth() > depth && phoneSheet.peek()) {
          requestClosePhoneSheet();
          held = !!phoneSheet.peek();
        }
        if (!held && layerDepth() > depth) phonePage.peek()?.onBack?.();
      } finally {
        restoring = false;
      }
      // A held sheet stays on screen, so it takes its entry back, once this
      // restore is done: the next Back asks again.
      if (held) setTimeout(() => announceOverlay({ doc }), 0);
    },
  });
  const unView = registerOverlayRoute('own', {
    value: () => view.peek() || '',
    restore: (want) => {
      // The boot's restore of an address without a view: the shell already
      // chose (a bare / lands on Now), and the address is corrected to match
      // once booted. One with a view (a reload of /?own=board) shows it.
      if (booting(doc) && !want) return;
      const next = OWN_VIEW_VALUES.includes(want) ? want : null;
      kept = next ? { view: next, tab: tab() } : null;
      if (view.peek() === next) return;
      restoring = true;
      try {
        view.value = next;
      } finally {
        restoring = false;
      }
    },
  });

  // Changes made on screen are history steps; ones Back made are not.
  // A layer closed on screen steps back off its own entry.
  let depth = layerDepth();
  const stopLayers = effect(() => {
    const next = layerDepth(phonePage.value, phoneSheet.value);
    if (next === depth) return;
    const closed = next < depth;
    depth = next;
    if (!restoring) announceOverlay({ closed, doc });
  });
  // An own view coming up or going is a step forward either way: leaving Now
  // for Mail must leave Now behind it for Back. It is announced a moment
  // later, so a tab the same tap shows records the step once, as that tab.
  let shown = view.peek() || '';
  let pending = null;
  const stopView = effect(() => {
    const next = view.value || '';
    if (next === shown) return;
    shown = next;
    if (restoring) return;
    clearTimeout(pending);
    pending = setTimeout(() => announceOverlay({ doc }), 0);
  });
  // Put what is on screen at boot (Now on a bare /) into the address.
  const cancelBoot = whenBooted(doc, () => announceOverlay({ replace: true, doc }));
  return () => {
    cancelBoot();
    clearTimeout(pending);
    stopLayers();
    stopView();
    unLayers();
    unView();
    kept = null;
  };
}
