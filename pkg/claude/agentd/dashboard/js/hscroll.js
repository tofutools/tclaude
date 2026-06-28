// hscroll.js — keep the full-bleed chrome bars from looking ragged when
// the dashboard is wide enough to need a horizontal page scrollbar
// (JOH-313).
//
// header / nav / #slop-marquee are viewport-wide block bars with a filled
// background. When something in <main> (typically a wide member table)
// overflows, the page scrolls sideways but those bars stay viewport-wide,
// so scrolling right exposes the darker page background past their right
// edge. We fix that by widening the bars to the document's scrollWidth.
//
// Their content can't just widen with them — the header's right-aligned
// controls (usage readout + windows / power / shutdown buttons, pushed
// right by margin-left:auto) would fly off to the far-right edge, off
// screen at rest. So each bar's content lives in a .bar-inner that we pin
// to the viewport width; the bar's background fills the rest.
//
// Two modes, toggled live (so both can be compared) and persisted:
//   follow (default) — .bar-inner is sticky-left (CSS, body.hscroll-follow),
//                      so the controls + tab strip stay put and usable as
//                      the content scrolls under them.
//   static          — .bar-inner scrolls off with the page; the bar
//                      background still fills the width, so it's never
//                      ragged, but the controls aren't reachable while
//                      scrolled right.
//
// We use the live, laid-out document.scrollWidth (not a CSS
// min-width:max-content) so wrappable content — long descriptions, audit
// lines — can't force a permanent horizontal scrollbar on a wide screen.

import { dashPrefs } from './prefs.js';

const FOLLOW_KEY = 'tclaude.dash.hscroll.follow';

// syncFullBleedBars widens header/nav/marquee to the scrollable content
// width and pins their .bar-inner to the viewport width — but only while
// the page actually overflows sideways; otherwise everything is reset to
// its natural (viewport-wide) layout. Also flags body.hscroll-overflow so
// the mode toggle reveals itself only when it's relevant.
function syncFullBleedBars() {
  const bars = [
    document.querySelector('header'),
    document.getElementById('slop-marquee'),
    document.querySelector('nav'),
  ].filter(Boolean);
  const inners = document.querySelectorAll('.bar-inner');
  const doc = document.documentElement;

  // Reset first so our own sizing can't pollute the overflow measurement,
  // then read scrollWidth (this forces one reflow — fine at rAF cadence).
  for (const bar of bars) bar.style.minWidth = '';
  for (const inner of inners) inner.style.width = '';

  const overflow = doc.scrollWidth > doc.clientWidth;
  document.body.classList.toggle('hscroll-overflow', overflow);
  if (!overflow) return;

  // Widen the bars to the full scrollable width; keep their content one
  // viewport wide so the right-aligned controls stay on-screen.
  const contentWidth = doc.scrollWidth + 'px';
  const viewportWidth = doc.clientWidth + 'px';
  for (const bar of bars) bar.style.minWidth = contentWidth;
  for (const inner of inners) inner.style.width = viewportWidth;
}

// rAF-coalesced wrapper — a single render can fire many mutations; we only
// need to re-measure once per frame.
let scheduled = false;
function scheduleSync() {
  if (scheduled) return;
  scheduled = true;
  requestAnimationFrame(() => {
    scheduled = false;
    syncFullBleedBars();
  });
}

function paintToggle(btn) {
  const on = document.body.classList.contains('hscroll-follow');
  btn.setAttribute('aria-pressed', on ? 'true' : 'false');
  btn.textContent = on ? '⇆ follow' : '⇆ static';
  btn.title = on
    ? 'Horizontal scroll: FOLLOW — the header bar & tabs stay pinned to the view (controls usable while scrolled). Click for static mode.'
    : 'Horizontal scroll: STATIC — the bars fill the width but their controls scroll off with the page. Click for follow mode.';
}

// bindHScroll wires up the mode (from the saved pref, default follow), the
// ⇆ toggle button, and the resync triggers. Call AFTER initDashPrefs() so
// the saved mode is available.
function bindHScroll() {
  const follow = (dashPrefs.getItem(FOLLOW_KEY) ?? '1') === '1';
  document.body.classList.toggle('hscroll-follow', follow);

  const btn = document.getElementById('hscroll-toggle');
  if (btn) {
    paintToggle(btn);
    btn.addEventListener('click', () => {
      const next = !document.body.classList.contains('hscroll-follow');
      document.body.classList.toggle('hscroll-follow', next);
      dashPrefs.setItem(FOLLOW_KEY, next ? '1' : '0');
      paintToggle(btn);
      scheduleSync();
    });
  }

  // Re-measure whenever the content could have changed width: every
  // re-render / tab switch / group expand (DOM churn under <main>), and on
  // window resize. syncFullBleedBars only touches the bars + body class,
  // never <main>, so the observer can't loop on its own work. (childList
  // only — a width change that adds/removes no nodes, e.g. an inline-style
  // column resize, waits for the next 2s re-render or a resize; that's a
  // rare case and the lag is at most one poll.)
  const main = document.querySelector('main');
  if (main) {
    new MutationObserver(scheduleSync).observe(main, { childList: true, subtree: true });
  }
  window.addEventListener('resize', scheduleSync);
  scheduleSync();
}

export { bindHScroll, syncFullBleedBars };
