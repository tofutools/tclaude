// mail-resize.js — draggable column widths for the Messages tab.
//
// The mail client (mail.js) lays its three panes out as a CSS grid with two
// drag-handle tracks between them:
//
//     sidebar | gutter | list | gutter | reader
//
// Dragging the LEFT gutter resizes the sidebar — a fixed-px track, so it
// keeps its width as the window grows. Dragging the RIGHT gutter shifts the
// split between the list and reader, which are fr tracks sharing the
// remaining space (so they reflow proportionally with the window). The
// chosen layout is persisted server-side via dashPrefs
// (tclaude.dash.mail.cols) — the same store every other sticky dashboard
// pref uses — so it survives reload, daemon restart, browser profile, and
// the random loopback port that would otherwise partition localStorage away.
//
// Double-clicking a gutter resets the layout to the CSS default (and drops
// the pref). The drag math measures the panes' live pixel widths at gesture
// start, so it stays exact whatever the current window size is.

import { $, $$ } from './helpers.js';
import { dashPrefs } from './prefs.js';

const COLS_KEY = 'tclaude.dash.mail.cols';

// Track floors / sidebar ceiling. MIN_LIST / MIN_READER double as the
// grid's minmax() mins (content-overflow guards, matching the CSS defaults);
// the sidebar gets an explicit px clamp, further bounded at drag time by the
// space the list + reader floors leave.
const MIN_SIDEBAR = 160, MAX_SIDEBAR = 640;
const MIN_LIST = 220, MIN_READER = 260;
const GUTTER = '10px';

// Default layout = the CSS default ratio (240px sidebar, 1fr list, 1.4fr
// reader). listFr / readerFr are unitless weights; only their ratio matters,
// so a drag may store them as pixel counts — the fr tracks normalise them.
const DEFAULTS = { sidebar: 240, listFr: 1, readerFr: 1.4 };

let cols = { ...DEFAULTS };

function clamp(v, lo, hi) { return Math.min(hi, Math.max(lo, v)); }

function loadCols() {
  try {
    const s = JSON.parse(dashPrefs.getItem(COLS_KEY));
    if (s && typeof s === 'object') {
      return {
        sidebar: clamp(+s.sidebar || DEFAULTS.sidebar, MIN_SIDEBAR, MAX_SIDEBAR),
        listFr: +s.listFr > 0 ? +s.listFr : DEFAULTS.listFr,
        readerFr: +s.readerFr > 0 ? +s.readerFr : DEFAULTS.readerFr,
      };
    }
  } catch (_) { /* missing / corrupt — fall back to the CSS default layout */ }
  return { ...DEFAULTS };
}

function template(c) {
  return `${c.sidebar}px ${GUTTER} minmax(${MIN_LIST}px, ${c.listFr}fr) `
    + `${GUTTER} minmax(${MIN_READER}px, ${c.readerFr}fr)`;
}

function apply(client) {
  client.style.gridTemplateColumns = template(cols);
}

function persist() {
  try { dashPrefs.setItem(COLS_KEY, JSON.stringify(cols)); } catch (_) {}
}

// initMailResize restores the saved layout and wires each gutter's drag +
// double-click-to-reset. Safe to call once at boot: the .mail-client markup
// is static, and dashPrefs is already loaded (boot awaits initDashPrefs).
function initMailResize(client = $('.mail-client')) {
  if (!client) return () => {};
  const controller = new AbortController();
  const { signal } = controller;
  const removeListeners = [];
  cols = loadCols();
  apply(client);

  $$('.mail-gutter', client).forEach(g => {
    const boundary = g.getAttribute('data-boundary');
    const onPointerDown = e => startDrag(e, client, g, boundary, signal);
    const onDoubleClick = () => {
      cols = { ...DEFAULTS };
      apply(client);
      try { dashPrefs.removeItem(COLS_KEY); } catch (_) {}
    };
    g.addEventListener('pointerdown', onPointerDown);
    g.addEventListener('dblclick', onDoubleClick);
    removeListeners.push(() => {
      g.removeEventListener('pointerdown', onPointerDown);
      g.removeEventListener('dblclick', onDoubleClick);
    });
  });
  return () => {
    controller.abort();
    removeListeners.forEach(remove => remove());
  };
}

function startDrag(e, client, gutter, boundary, signal) {
  if (e.button !== 0) return; // left button only
  e.preventDefault();
  gutter.setPointerCapture(e.pointerId);
  gutter.classList.add('dragging');
  document.body.classList.add('mail-col-resizing');

  const startX = e.clientX;
  // Live pixel widths at gesture start make the fr-ratio drag exact
  // regardless of the current window size.
  const sidebarPx = $('.mail-sidebar-col', client).offsetWidth;
  const listPx = $('.mail-list-col', client).offsetWidth;
  const readerPx = $('.mail-reader', client).offsetWidth;
  // Cap the sidebar so growing it can't squeeze list + reader below their
  // floors (the two 10px gutters cost 20px of track).
  const maxSidebar = clamp(client.clientWidth - 20 - MIN_LIST - MIN_READER,
    MIN_SIDEBAR, MAX_SIDEBAR);
  let moved = false;

  function onMove(ev) {
    const dx = ev.clientX - startX;
    if (boundary === 'sidebar-list') {
      // Left gutter: resize the sidebar (px track); the fr tracks absorb it.
      cols.sidebar = clamp(sidebarPx + dx, MIN_SIDEBAR, maxSidebar);
    } else {
      // Right gutter: move the list/reader split, conserving their total.
      // Clamp the delta so neither pane drops below its floor.
      const d = clamp(dx, MIN_LIST - listPx, readerPx - MIN_READER);
      cols.listFr = listPx + d;
      cols.readerFr = readerPx - d;
    }
    moved = true;
    apply(client);
  }

  function onUp() {
    if (gutter.hasPointerCapture(e.pointerId)) gutter.releasePointerCapture(e.pointerId);
    gutter.classList.remove('dragging');
    document.body.classList.remove('mail-col-resizing');
    gutter.removeEventListener('pointermove', onMove);
    gutter.removeEventListener('pointerup', onUp);
    gutter.removeEventListener('pointercancel', onUp);
    signal?.removeEventListener('abort', onUp);
    if (moved) persist(); // a bare click (no drag) leaves the pref untouched
  }

  gutter.addEventListener('pointermove', onMove);
  gutter.addEventListener('pointerup', onUp);
  gutter.addEventListener('pointercancel', onUp);
  signal?.addEventListener('abort', onUp, { once: true });
}

export { initMailResize };
