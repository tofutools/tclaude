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
// Two modes, chosen from config (dashboard.hscroll_follow, default follow)
// and applied on every snapshot via setHScrollFollow:
//   follow (default) — .bar-inner is sticky-left (CSS, body.hscroll-follow),
//                      so the controls + tab strip stay put and usable as
//                      the content scrolls under them.
//   static          — .bar-inner scrolls off with the page; the bar
//                      background still fills the width, so it's never
//                      ragged, but the controls aren't reachable while
//                      scrolled right.
//
// The mode used to be a per-browser header toggle button; it now lives in
// ~/.tclaude/config.json (editable on the Config tab) and rides the 2s
// snapshot, so every dashboard for that daemon shares one setting.
//
// We use the live, laid-out document.scrollWidth (not a CSS
// min-width:max-content) so wrappable content — long descriptions, audit
// lines — can't force a permanent horizontal scrollbar on a wide screen.

// syncFullBleedBars widens header/nav/marquee to the scrollable content
// width and pins their .bar-inner to the viewport width — but only while
// the page actually overflows sideways; otherwise everything is reset to
// its natural (viewport-wide) layout. Also flags body.hscroll-overflow so
// the sticky-pin CSS (follow mode's .bar-inner) only engages when the page
// actually overflows sideways.
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

// setHScrollFollow applies the follow/static mode from config
// (dashboard.hscroll_follow) — refresh.js calls it from every snapshot. It
// only touches the DOM (and reschedules a bar resync) when the mode actually
// changes, so the 2s poll doesn't churn. Exported so refresh.js drives it.
function setHScrollFollow(on) {
  on = !!on;
  if (on === document.body.classList.contains('hscroll-follow')) return;
  document.body.classList.toggle('hscroll-follow', on);
  scheduleSync();
}

// bindHScroll seeds the default mode (follow, until the first snapshot
// confirms it from config) and wires the resync triggers. Call once at boot;
// the live mode then arrives on every snapshot via setHScrollFollow.
function bindHScroll() {
  // Default-follow at boot so a wide first paint isn't briefly static before
  // the first snapshot lands; refresh.js corrects it from config thereafter.
  document.body.classList.add('hscroll-follow');

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

export { bindHScroll, setHScrollFollow, syncFullBleedBars };
