// chrome-inset.js — the content-area top inset, published as --dock-top.
//
// The dashboard's chrome (header + slop marquee + nav) is not sticky: making it
// sticky would spin up a stacking context that re-scopes the header popovers, a
// documented no-go. So a right-side panel that must span ONLY the content area
// cannot pin its top to a constant — it tracks nav's live viewport-bottom,
// clamped at 0, which at rest sits just under the nav and rises to fill the
// height as the page scrolls the chrome away.
//
// Two panels ride that inset now — the palette dock (js/dock.js, which owns the
// scroll/resize/ResizeObserver listeners that keep it fresh) and the
// human-notification reader — so the measurement lives in its own leaf module
// instead of inside either of them. The variable keeps its original --dock-top
// name: it is referenced across dashboard.css and renaming it would be churn.
//
// applyChromeTopInset is the immediate write, for a panel that is about to
// appear and must not paint one frame at a stale inset. syncChromeTopInset is
// the rAF-coalesced form the continuous listeners use.

export function applyChromeTopInset(documentRef = document) {
  const nav = documentRef.querySelector('nav');
  const navBottom = nav?.getBoundingClientRect ? nav.getBoundingClientRect().bottom : 0;
  documentRef.documentElement?.style?.setProperty('--dock-top', `${Math.max(0, navBottom)}px`);
}

let scheduled = false;

export function syncChromeTopInset(documentRef = document) {
  if (scheduled) return;
  scheduled = true;
  const raf = globalThis.requestAnimationFrame || ((fn) => setTimeout(fn, 16));
  raf(() => {
    scheduled = false;
    applyChromeTopInset(documentRef);
  });
}
