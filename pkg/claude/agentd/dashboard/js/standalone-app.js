// standalone-app.js -- the dashboard installed as an app (the web app
// manifest's display: standalone, dashboard_pwa.go): a window of its own with
// no address bar. Two things a browser tab gave for free need standing in
// for there, and only there (a tab keeps the browser's own):
//
//   - Leaving the app. A link to another site (a GitHub PR, a claude.ai deep
//     link) would load inside the app's window; installExternalLinks opens it
//     in the browser (or the site's own app) instead.
//   - Reloading. Chrome's pull-to-refresh is off in a standalone window, so
//     installHeaderPull makes a pull down on the phone's header bar reload,
//     as the pull did in the browser.

// isStandalone says whether the page runs as the installed app: the
// display-mode media feature (Chrome, Edge, Safari 16.4+), or iOS's own flag
// for a home-screen web app.
export function isStandalone(win = globalThis) {
  try {
    if (win?.matchMedia?.('(display-mode: standalone)')?.matches) return true;
  } catch { /* no matchMedia */ }
  return win?.navigator?.standalone === true;
}

// externalHref is the URL a click on target would leave the app for, or ''
// when it stays (same origin, no link, a modified click the browser handles,
// a download, or a link already opening a window of its own).
export function externalHref(event, origin) {
  if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return '';
  const link = event.target?.closest?.('a[href]');
  if (!link || link.hasAttribute('download') || (link.target && link.target !== '_self')) return '';
  let url;
  try { url = new URL(link.href, origin); } catch { return ''; }
  if (!/^https?:$/.test(url.protocol) || url.origin === origin) return '';
  return url.href;
}

// installExternalLinks opens a link to another site in the browser while
// running as the app. Returns the uninstall function.
export function installExternalLinks(win = globalThis) {
  const doc = win?.document;
  if (!doc || !isStandalone(win)) return () => {};
  const onClick = (event) => {
    const href = externalHref(event, win.location.origin);
    if (!href) return;
    event.preventDefault();
    win.open(href, '_blank', 'noopener');
  };
  doc.addEventListener('click', onClick, true);
  return () => doc.removeEventListener('click', onClick, true);
}

// PULL_RELOAD_PX is how far down the header must be pulled to reload.
export const PULL_RELOAD_PX = 80;

// installHeaderPull makes a pull down that starts on the phone's header bar
// (bar) reload the page (reload), while running as the app: a hint follows
// the finger, says when letting go reloads, and a pull that turns sideways or
// goes back up does nothing. Returns the uninstall function.
export function installHeaderPull({ win = globalThis, bar = '.phone-bar', reload = () => win.location.reload() } = {}) {
  const doc = win?.document;
  if (!doc || !isStandalone(win)) return () => {};
  let start = null;
  let hint = null;
  const show = (dy) => {
    if (!hint) {
      hint = doc.createElement('div');
      hint.className = 'phone-pull';
      hint.setAttribute('role', 'status');
      hint.setAttribute('aria-live', 'polite');
      doc.body.append(hint);
    }
    const ready = dy >= PULL_RELOAD_PX;
    const text = ready ? '↻ Release to reload' : '↓ Pull to reload';
    if (hint.textContent !== text) hint.textContent = text;
    hint.classList.toggle('ready', ready);
    hint.style.setProperty('--phone-pull', `${Math.min(dy, PULL_RELOAD_PX * 1.5)}px`);
  };
  const clear = () => {
    start = null;
    hint?.remove();
    hint = null;
  };
  const onStart = (event) => {
    // A second finger makes it a pinch or the like, not a pull.
    if (event.touches.length !== 1) { clear(); return; }
    if (!event.target?.closest?.(bar)) return;
    const touch = event.touches[0];
    start = { x: touch.clientX, y: touch.clientY, dy: 0 };
  };
  const onMove = (event) => {
    if (!start) return;
    const touch = event.touches[0];
    const dy = touch.clientY - start.y;
    const dx = Math.abs(touch.clientX - start.x);
    if (dx > Math.max(24, dy)) { clear(); return; }
    start.dy = dy;
    if (dy > 8) show(dy);
    else if (hint) { hint.remove(); hint = null; }
  };
  const onEnd = (event) => {
    if (event.touches?.length) return;
    const pulled = start?.dy ?? 0;
    clear();
    if (pulled >= PULL_RELOAD_PX) reload();
  };
  doc.addEventListener('touchstart', onStart, { passive: true });
  doc.addEventListener('touchmove', onMove, { passive: true });
  doc.addEventListener('touchend', onEnd);
  doc.addEventListener('touchcancel', clear);
  return () => {
    doc.removeEventListener('touchstart', onStart);
    doc.removeEventListener('touchmove', onMove);
    doc.removeEventListener('touchend', onEnd);
    doc.removeEventListener('touchcancel', clear);
    clear();
  };
}
