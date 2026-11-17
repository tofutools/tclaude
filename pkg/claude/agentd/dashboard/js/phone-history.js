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
import { backLayers, backSeq, closeTopBackLayer, stepLayers } from './back-layers.js';
import { announceOverlay, registerOverlayRoute } from './overlay-routes.js';
import { phonePage } from './phone-page.js';
import { phoneSheet, requestClosePhoneSheet } from './phone-sheet.js';

export const OWN_VIEW_VALUES = Object.freeze(['now', 'board']);

// layerDepth is how many layers are up: an unrouted page (and an unrouted
// page it covers), then a sheet, then the open modal layers over them (a
// dialog, a form, a preview: back-layers.js).
export function layerDepth(page = phonePage.peek(), sheet = phoneSheet.peek(), layers = backLayers.peek()) {
  // A page over another (a task's page over a tab's unrouted page) is a
  // layer on top of that one's.
  const covered = page?.covered && !page.covered.routed ? 1 : 0;
  return (page && !page.routed ? 1 : 0) + covered + (sheet ? 1 : 0) + stepLayers(layers).length;
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

// RETAKE_SETTLE_MS is how long a step Forward (retakeStep) may take to land
// before the step is recorded anew instead.
export const RETAKE_SETTLE_MS = 400;

// installPhoneHistory wires the layers and `view` (the shell's own-view
// signal) to the router; `tab` reads the active tab. Returns the uninstall
// function.
export function installPhoneHistory({ view, tab = () => null, doc = globalThis.document }) {
  let restoring = false;
  // quiet is the layers a Back's close raised while it was on its way (a
  // dialog's Discard? question): they are no step of their own. The form
  // asking takes its entry back at once instead (asked, below), so nothing
  // adds an entry after the Back without a tap (retakeStep).
  const quiet = new Set();
  const counted = (page, sheet, layers = backLayers.peek()) => {
    for (const layer of quiet) if (!layers.includes(layer)) quiet.delete(layer);
    return layerDepth(page, sheet, layers) - stepLayers(layers).filter((layer) => quiet.has(layer)).length;
  };
  // asking is the Back whose close of a layer is on its way, until it lands:
  // { retaken } once a question it raised made the form take its entry back.
  let asking = null;
  let knownLayers = backLayers.peek();
  // depth is the layer count the router was last told of.
  let depth = counted();
  // sheetSeq is when the open sheet opened, against the layers' (backSeq).
  let sheetSeq = 0;
  let sheetShown = phoneSheet.peek();
  const stopSheetSeq = effect(() => {
    const next = phoneSheet.value;
    if (next && next !== sheetShown) sheetSeq = backSeq();
    sheetShown = next;
  });
  // retakeStep gives a layer Back could not close (a draft kept, a form that
  // stayed open) its history entry back: a step Forward, onto the entry Back
  // just left, which is still there. Never a new entry: Chrome marks every
  // entry of a page skippable when the page adds one after a Back without a
  // fresh tap (its history-manipulation intervention), and Android's Back
  // then skips them all and leaves the app (tk_3739e29d). When there is
  // nothing ahead (the Navigation API says so, or the Forward never lands),
  // the step is recorded anew, as before.
  const win = doc?.defaultView;
  let retaking = null;
  const settled = () => { if (retaking) { clearTimeout(retaking); retaking = null; } };
  const retakeStep = () => {
    settled();
    if (!win?.history?.forward || win.navigation?.canGoForward === false) {
      announceOverlay({ doc });
      return;
    }
    retaking = setTimeout(() => {
      retaking = null;
      if (counted() > Number(new URL(win.location.href).searchParams.get('phone') || 0)) announceOverlay({ doc });
    }, RETAKE_SETTLE_MS);
    win.history.forward();
  };
  win?.addEventListener?.('popstate', settled);
  // closing counts the layers Back asked to close whose close has not landed
  // yet: the step back is taken, so they are no longer in the address.
  let closing = 0;
  const unLayers = registerOverlayRoute('phone', {
    value: () => { const shown = counted() - closing; return shown > 0 ? String(shown) : ''; },
    restore: (want) => {
      // Back over an open dialog or form is that overlay's: it closes the
      // topmost one through its own guarded close, which may ask first (a
      // dirty form's discard prompt) and so lands later. The step back is
      // already taken, so the close it lands is no step of its own; one that
      // stayed open takes its entry back, and the next Back asks again.
      // Whichever of a sheet and a layer opened last is on top.
      const top = stepLayers().at(-1);
      const layerOnTop = top && (!phoneSheet.peek() || top.seq > sheetSeq);
      if (layerOnTop && counted() > (Number(want) || 0)) {
        // A question on top (Back while a dialog asks Discard?) is not
        // counted: its answer decides whether the form under it goes (Back
        // confirms a discard) or stays without the entry Back just left.
        // Either way the address has left the form's entry until the close
        // lands (closing).
        const question = quiet.has(top);
        const back = { retaken: false };
        // A question's Back leaves the entry of the form that asked (the
        // Back that raised it): that form's own close settles it.
        if (question && asking) asking.offEntry = true;
        else asking = back;
        depth = counted() - 1;
        closing += 1;
        void closeTopBackLayer().then((closed) => {
          if (question) { closing -= 1; return; }
          if (asking === back) asking = null;
          if (!back.retaken) closing -= 1;
          if (closed) return;
          // Still up: kept, by a tap or a question answered Keep. It takes
          // its entry back, unless it is still on it (a tap's Keep while
          // its entry was there).
          if (back.retaken && !back.offEntry) return;
          depth = counted();
          retakeStep();
        });
        return;
      }
      const depthWanted = Number(want) || 0;
      restoring = true;
      let held = false;
      try {
        // Back is a close the operator asked for: a sheet's guard may hold
        // it (a draft to ask about, a send in flight).
        if (counted() > depthWanted && phoneSheet.peek()) {
          // Until it is known to be held, the sheet is closing: the address
          // has left it. Were it counted, the router would write a held
          // sheet onto the entry Back landed on (the page's), so the sheet
          // would have no step of its own and the next Back would move the
          // page behind it.
          closing += 1;
          try {
            requestClosePhoneSheet({ back: true });
            held = !!phoneSheet.peek();
          } finally {
            if (!held) closing -= 1;
          }
        }
        if (!held && counted() > depthWanted) phonePage.peek()?.onBack?.();
      } finally {
        restoring = false;
      }
      // A held sheet stays on screen, so it takes its entry back once this
      // restore is done (retakeStep). It stops counting as closing as soon as
      // the router has corrected the entry Back landed on (synchronously,
      // after this restore), so a further Back finds it up and asks it again.
      if (held) {
        queueMicrotask(() => { closing -= 1; });
        setTimeout(retakeStep, 0);
      }
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
  const stopLayers = effect(() => {
    const page = phonePage.value;
    const sheet = phoneSheet.value;
    const layers = backLayers.value;
    const raised = layers.filter((layer) => !knownLayers.includes(layer));
    const went = layers.length < knownLayers.length;
    knownLayers = layers;
    // A layer opened while Back's close is on its way is that close's
    // question: no step of its own, and the form takes its entry back now,
    // so a further Back finds it and keeps it.
    if (asking && !asking.retaken && raised.length) {
      for (const layer of raised) quiet.add(layer);
      asking.retaken = true;
      closing -= 1;
      depth = counted(page, sheet, layers);
      retakeStep();
      return;
    }
    const next = counted(page, sheet, layers);
    if (next === depth) return;
    // A layer went, yet more are up than the router knows of: the form a
    // Back is closing, still up while its question is answered. No step.
    if (went && next > depth && closing > 0) return;
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
    settled();
    win?.removeEventListener?.('popstate', settled);
    cancelBoot();
    clearTimeout(pending);
    stopLayers();
    stopSheetSeq();
    stopView();
    unLayers();
    unView();
    kept = null;
  };
}
