// tooltip.js — fast global hover-tooltip layer.
//
// The dashboard annotates hundreds of elements with the native `title=`
// attribute, but the browser's built-in tooltip has a fixed ~0.5–1.5s show
// delay that no CSS/JS/HTML knob can shorten (there is no web standard for
// it). This module replaces that native tooltip *everywhere* with a single
// shared floating element that appears after a short, tunable delay
// (TOOLTIP_DELAY_MS) — "considerably sooner", as asked — while leaving every
// existing `title=` call site (~250 of them) untouched.
//
// How it stays global with zero per-site changes: one delegated `mouseover`
// listener on document finds the nearest ancestor carrying a real `title`,
// stashes that text and REMOVES the attribute (so the native tooltip is
// suppressed) and, after the delay, shows our styled element anchored to that
// node. On mouseout / scroll / wheel / mousedown / Escape / window blur / the
// 2s snapshot re-render we hide it and restore the original `title` — so the
// value survives for screen readers and for the next hover, and is never lost.
//
// Safety: the (often user-derived) title text is written with textContent,
// never innerHTML, so this is not an injection sink. This layer only reads
// `title`; the cost tab's separate `data-tip` CSS tooltip is unaffected.

// The show delay, in ms. The browser's native delay is ~500ms+; 120ms feels
// near-instant without flickering as the cursor sweeps across a dense table.
// One knob — tune here.
const TOOLTIP_DELAY_MS = 120;

let tip = null; // the shared floating element (created lazily on first show)
let anchor = null; // element that currently owns the tooltip
let stashed = ''; // its original title text, restored on hide
let showTimer = 0;

// ensureTip lazily creates the one shared tooltip element. Kept out of the
// document until first needed so a session that never hovers pays nothing.
function ensureTip() {
  if (tip) return tip;
  tip = document.createElement('div');
  tip.id = 'tt';
  tip.setAttribute('role', 'tooltip');
  tip.setAttribute('aria-hidden', 'true');
  document.body.appendChild(tip);
  return tip;
}

// hide tears down any visible or pending tooltip and — crucially — restores
// the stashed `title` onto its anchor, so the attribute is never lost (a
// re-render may already have detached the node, in which case the restore is a
// harmless no-op on a soon-to-be-GC'd element).
function hide() {
  if (showTimer) {
    clearTimeout(showTimer);
    showTimer = 0;
  }
  if (anchor) {
    if (stashed && !anchor.hasAttribute('title')) anchor.setAttribute('title', stashed);
    anchor.removeAttribute('data-tt'); // drop the closest()-visibility marker
    anchor = null;
    stashed = '';
  }
  if (tip) {
    tip.classList.remove('show');
    tip.setAttribute('aria-hidden', 'true');
  }
}

// position anchors the tooltip above its target, centered; flips below when it
// would clip the top of the viewport, and clamps horizontally so it never
// overflows either edge. Called with content already set so the measured rect
// is the real one.
function position() {
  const r = anchor.getBoundingClientRect();
  const t = tip.getBoundingClientRect();
  const gap = 6; // space between target and tooltip
  const pad = 4; // minimum viewport inset
  let top = r.top - t.height - gap;
  if (top < pad) top = r.bottom + gap; // no room above → drop below
  // Keep it fully on-screen vertically even when the target hugs an edge.
  top = Math.max(pad, Math.min(top, window.innerHeight - t.height - pad));
  let left = r.left + r.width / 2 - t.width / 2; // centered on the target
  left = Math.max(pad, Math.min(left, window.innerWidth - t.width - pad));
  tip.style.top = `${Math.round(top)}px`;
  tip.style.left = `${Math.round(left)}px`;
}

// show is the delayed callback: fill, position, then fade in. Bails if the
// anchor was detached during the delay (e.g. a re-render landed).
function show() {
  showTimer = 0;
  if (!anchor || !anchor.isConnected) {
    hide();
    return;
  }
  const el = ensureTip();
  el.textContent = stashed;
  position(); // measure + place while still transparent, so no positional flash
  el.classList.add('show');
  el.setAttribute('aria-hidden', 'false');
}

// bindTooltips installs the delegated listeners once, at boot. Delegation on
// document means it needs no re-binding across the 2s innerHTML re-renders.
function bindTooltips() {
  document.addEventListener('mouseover', (e) => {
    if (!e.target || !e.target.closest) return;
    // Match BOTH a live `title` and our own `data-tt` marker. Once an anchor's
    // `title` is stripped below (to kill the native tooltip), `[title]` alone
    // would no longer see it — so a mouseover bubbling up from a *non-titled
    // descendant* of the anchor (e.g. the inner `.qo-text` span of a titled
    // group-header chip) would walk straight past the anchor and mis-match an
    // outer titled ancestor (the `<summary>`'s "Drag to reorder" title),
    // flipping the tooltip. The marker keeps the anchor findable so we detect
    // "still inside it" and keep it; a genuinely titled *inner* element still
    // wins via closest()'s nearest-match, so nested distinct titles switch
    // correctly.
    const el = e.target.closest('[title], [data-tt]');
    if (!el) return;
    if (el === anchor) return; // still within the current anchor: keep it
    const text = el.getAttribute('title');
    if (!text) return; // empty title (or a stray marker) carries nothing to show
    hide(); // switching anchors: restore the old one's title first
    anchor = el;
    stashed = text;
    el.setAttribute('data-tt', ''); // keep it findable by closest() post-strip
    el.removeAttribute('title'); // suppress the native (slow) tooltip
    showTimer = setTimeout(show, TOOLTIP_DELAY_MS);
  });
  document.addEventListener('mouseout', (e) => {
    if (!anchor) return;
    // Moving to a descendant of the anchor is still "inside" it — keep the
    // tooltip. relatedTarget null means the pointer left the window entirely.
    const to = e.relatedTarget;
    if (to && anchor.contains(to)) return;
    hide();
  });
  // Anything that moves the page or shifts focus makes a shown tooltip stale.
  window.addEventListener('scroll', hide, true); // capture: also catch inner scrollers
  window.addEventListener('wheel', hide, { passive: true });
  document.addEventListener('mousedown', hide);
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') hide();
  });
  window.addEventListener('blur', hide);
  // The 2s poll re-renders innerHTML, which can detach our anchor out from
  // under a stationary cursor. Drop the tooltip so it never lingers orphaned;
  // the next mouse move re-triggers it against the fresh node.
  document.addEventListener('tclaude:snapshot', hide);
}

export { bindTooltips, TOOLTIP_DELAY_MS };
