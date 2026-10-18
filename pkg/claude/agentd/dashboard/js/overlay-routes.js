// overlay-routes.js — dialogs and drawers that open over whatever tab is
// showing and are part of its location: ?manage=<kind> (a library manager)
// and ?notification=<sender>:<message-id> (the human-notification reader).
//
// Dependency-free (bar the pure routing core) so the history router and the
// lazily mounted islands that own each overlay meet here without importing
// each other. An island registers its overlay's current value and how to
// show one; the router reads every value into the URL, and on Back/Forward or
// a deep link asks each overlay to match the location. A value asked for
// before its island registers waits here and is applied on registration.

import { OVERLAY_PARAMS } from './nav-history-core.js';

export const OVERLAY_EVENT = 'tclaude:overlay';

const routes = new Map();
const wanted = new Map();

// registerOverlayRoute installs an overlay: value() is what it shows ('' when
// closed), restore(value) shows that value or closes it. Returns the
// unregister function.
export function registerOverlayRoute(key, route) {
  routes.set(key, route);
  if (wanted.has(key)) {
    const want = wanted.get(key);
    wanted.delete(key);
    if ((route.value() || '') !== want) route.restore(want);
  }
  return () => { if (routes.get(key) === route) routes.delete(key); };
}

// overlayParams is every overlay's value, as location params. One still
// waiting for its island keeps the value asked for.
export function overlayParams() {
  const out = {};
  for (const key of OVERLAY_PARAMS) {
    const route = routes.get(key);
    out[key] = route ? route.value() || '' : wanted.get(key) || '';
  }
  return out;
}

// restoreOverlays brings every overlay in line with a location's params.
export function restoreOverlays(params) {
  for (const key of OVERLAY_PARAMS) {
    const want = params?.[key] || '';
    const route = routes.get(key);
    if (!route) {
      if (want) wanted.set(key, want); else wanted.delete(key);
      continue;
    }
    if ((route.value() || '') !== want) route.restore(want);
  }
}

// announceOverlay tells the router an overlay opened, changed or closed
// (closed: true), so it records or rewinds a history entry.
export function announceOverlay({ closed = false, doc = globalThis.document } = {}) {
  doc?.dispatchEvent?.(new CustomEvent(OVERLAY_EVENT, { detail: { closed } }));
}
