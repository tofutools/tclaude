// layout-boot.js — decides, once per page load and before the first paint,
// whether the dashboard uses its phone layout or its desktop layout.
//
// A classic script loaded synchronously in <head>, right after the last
// stylesheet link and before <body>, so the decision is in place before the
// first paint and before the module graph runs; the phone sheets it links land
// after ui.css and so wins its cascade ties. js/layout.js is the module-side
// reader of the result.
//
// The decision is by device class, not by layout width: a phone held sideways
// is ~850px wide, the soft keyboard and pinch-zoom change the layout width,
// and a narrow desktop window must keep the desktop layout. A device is a
// phone when it says so (UA Client Hints: navigator.userAgentData.mobile,
// Chromium only), or when it is touch-first (coarse pointer, no hover) with a
// short screen side of at most 600 CSS px — phones are 360-430, the smallest
// tablet (iPad mini) is 744, so no tablet qualifies in either orientation.
//
// An override wins over detection: ?layout=phone or ?layout=desktop picks a
// layout and remembers it in this browser's localStorage, ?layout=auto forgets
// it. Per browser on purpose: dashboard prefs are server-side and shared by
// every device that opens this dashboard. The parameter is then removed from
// the address bar so a copied link does not carry it.
//
// Phone mode marks <html data-layout="phone">, adds the viewport meta and
// links the phone sheets (PHONE_SHEETS; their every rule is scoped under that
// attribute). Desktop
// mode changes nothing at all: no attribute, no meta, no stylesheet, so the
// desktop page is the page it was before this script existed. "Desktop site"
// on a phone is therefore today's zoomed-out desktop page.
//
// dashboard-imperative-boundary: platform-layout
(() => {
  const STORAGE_KEY = 'tclaude.layout';
  // The phone layout's sheets, in cascade order: phone.css (tokens, shared
  // controls, the shell and its own views, pushed pages, dialogs), then one
  // sheet per view, one per line, alphabetical, so parallel changes to
  // different views touch different files. jstest/phone-css.test.mjs holds
  // this list and the files in step.
  const PHONE_SHEETS = [
    'phone.css',
    'phone-access.css',
    'phone-agents.css',
    'phone-automations.css',
    'phone-board.css',
    'phone-config.css',
    'phone-diagnostics.css',
    'phone-fleet.css',
    'phone-mail.css',
    'phone-plugins.css',
    'phone-terminal.css',
    'phone-usage.css',
  ];
  const LAYOUTS = ['phone', 'desktop'];
  const SHORT_SIDE_MAX = 600;

  const isLayout = (value) => LAYOUTS.includes(value);

  const phoneDevice = () => {
    try {
      if (navigator.userAgentData?.mobile === true) return true;
      const touchFirst = !!window.matchMedia?.('(pointer: coarse) and (hover: none)')?.matches;
      const shortSide = Math.min(Number(screen?.width) || Infinity, Number(screen?.height) || Infinity);
      return touchFirst && shortSide <= SHORT_SIDE_MAX;
    } catch {
      return false;
    }
  };

  // readOverride applies ?layout= (remembering or forgetting it) and returns
  // the override in force. Storage can be absent or throw (private windows,
  // blocked site data): a ?layout= then still holds for this page load.
  const readOverride = (asked) => {
    let storage = null;
    try { storage = window.localStorage || null; } catch { storage = null; }
    try {
      if (asked === 'auto') storage?.removeItem(STORAGE_KEY);
      else if (isLayout(asked)) storage?.setItem(STORAGE_KEY, asked);
    } catch { /* not remembered; this load still honours it */ }
    if (isLayout(asked)) return asked;
    if (asked === 'auto') return null;
    try {
      const stored = storage?.getItem(STORAGE_KEY);
      return isLayout(stored) ? stored : null;
    } catch {
      return null;
    }
  };

  let url = null;
  let asked = null;
  try {
    url = new URL(window.location.href);
    asked = url.searchParams.get('layout');
  } catch { url = null; }

  const override = readOverride(asked);
  const detected = phoneDevice() ? 'phone' : 'desktop';
  const layout = override || detected;

  // The address keeps neither ?layout= nor what only the phone layout can
  // show: its layers (?phone=, which no address can reopen) and, off the
  // phone, its own view (?own=). Left in, the router would carry them onto
  // every entry ("Desktop site" from the More sheet over Now).
  if (url) {
    const drop = ['layout', 'phone', ...(layout === 'phone' ? [] : ['own'])];
    const had = drop.filter((key) => url.searchParams.has(key));
    if (had.length) {
      for (const key of had) url.searchParams.delete(key);
      try {
        window.history?.replaceState?.(window.history.state, '', url.pathname + url.search + url.hash);
      } catch { /* the address keeps them; nothing else depends on it */ }
    }
  }
  window.__tclaudeLayout = Object.freeze({ layout, detected, override });
  if (layout !== 'phone') return;

  document.documentElement.dataset.layout = 'phone';
  // A desktop browser showing the phone layout (?layout=phone) is a preview:
  // phone.css keeps the shell in a phone-wide column instead of stretching it
  // across the window.
  if (detected !== 'phone') document.documentElement.dataset.phonePreview = '';
  const head = document.head || document.documentElement;
  const meta = document.createElement('meta');
  meta.name = 'viewport';
  // interactive-widget: Android Chrome shrinks the page for the on-screen
  // keyboard, so what sits at the bottom stays above it (phone-keyboard.js).
  meta.content = 'width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content';
  head.appendChild(meta);
  for (const name of PHONE_SHEETS) {
    const sheet = document.createElement('link');
    sheet.rel = 'stylesheet';
    sheet.href = `/static/${name}`;
    head.appendChild(sheet);
  }
})();
