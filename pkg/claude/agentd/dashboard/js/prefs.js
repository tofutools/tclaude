// prefs.js — DB-backed store for the dashboard's "sticky" view/config
// preferences (group expand/collapse, per-tab filters and toggles, the
// sort state, the spawn-modal auto-focus checkbox, the per-model spawn
// effort memory).
//
// Why not localStorage? The dashboard is served on a RANDOM loopback
// port each daemon start, and localStorage is partitioned by origin
// (scheme + host + port) — so a value written on one run is invisible to
// the next, and every "sticky" dashboard setting silently reset on
// restart. This module persists those prefs server-side in SQLite
// (dashboard_prefs) over /api/dashboard/prefs, so they survive restarts,
// browser profiles and multiple tabs — the same reasoning slop-volume.js
// already used for the volume sliders.
//
// `dashPrefs` mirrors the three localStorage methods the call sites used
// (getItem / setItem / removeItem) so migrating a call site is a
// one-word swap. Reads are synchronous off an in-memory cache; writes go
// through to the daemon, debounced per key. `initDashPrefs()` loads the
// cache and MUST be awaited before the first render reads any pref.
//
// The slop master-mute (tclaude.slop.sound, slop-audio.js) deliberately
// stays in localStorage — on/off is a per-browser whim, not shared
// config (its own comment makes that call); everything under the
// tclaude.dash.* namespace lives here.

const API = '/api/dashboard/prefs';

// The authoritative session copy of every pref. Object.create(null) so a
// pref literally named "toString"/"constructor" can't collide with
// Object.prototype. Replaced wholesale by initDashPrefs once loaded.
let cache = Object.create(null);

// initDashPrefs loads the whole pref map from the daemon into the cache.
// Best-effort: on any failure the cache stays empty and every getItem
// falls back to its caller's default, so the dashboard still renders — it
// just won't persist this session. Awaited once at boot before any bind
// or render runs.
async function initDashPrefs() {
  // Cap the load so a wedged daemon can't leave the dashboard blank
  // forever (boot awaits this before binding anything). 5s is generous
  // for a same-host loopback call that just served the page.
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 5000);
  try {
    const r = await fetch(API, { credentials: 'same-origin', signal: ctrl.signal });
    if (!r.ok) return;
    const obj = await r.json();
    if (obj && typeof obj === 'object') {
      cache = Object.assign(Object.create(null), obj);
    }
  } catch (_) {
    // Timeout, network error, bad JSON — leave the cache empty and let
    // the UI degrade to defaults rather than hang.
  } finally {
    clearTimeout(timer);
  }
}

// ---- write-through, debounced per key ----------------------------------
//
// The cache updates synchronously (reads stay correct immediately); only
// the network write is debounced, so a flurry of setItem calls — e.g. a
// filter box firing on every keystroke — collapses into one POST per key.
const WRITE_DEBOUNCE_MS = 400;

// key → { value: string | null }  (null = delete). Holds the latest
// desired state per key until the next flush.
const pending = new Map();
let flushTimer = null;

function scheduleFlush() {
  if (flushTimer) return;
  flushTimer = setTimeout(flush, WRITE_DEBOUNCE_MS);
}

function flush() {
  flushTimer = null;
  const batch = [...pending.entries()];
  pending.clear();
  // Dispatch every queued write SYNCHRONOUSLY — fire-and-forget, no
  // inter-request await. The writes are independent and best-effort, so
  // serialising them buys nothing; worse, awaiting between them breaks
  // the page-unload path: flushNow() calls this from pagehide /
  // visibilitychange→hidden, where the event loop won't resume past an
  // await, so an `await fetch` loop would dispatch only the FIRST request
  // of a multi-key batch and silently drop the rest. Calling fetch() up
  // front commits each keepalive request before the page tears down.
  for (const [key, { value }] of batch) {
    try {
      // value:null tells the daemon to delete (mirrors removeItem).
      fetch(API, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ key, value }),
        // keepalive lets the write survive a tab close / navigation that
        // races the debounce (also how flushNow's unload writes land).
        keepalive: true,
      }).catch(() => {
        // Best-effort persistence; the cache already reflects the value.
      });
    } catch (_) {
      // fetch() itself can throw synchronously (e.g. the keepalive total
      // body-size quota is exceeded). Best-effort — swallow and move on.
    }
  }
}

// flushNow fires every pending write immediately — wired to page-hide so
// a value set right before navigating away isn't lost to the debounce.
function flushNow() {
  if (flushTimer) {
    clearTimeout(flushTimer);
    flushTimer = null;
  }
  if (pending.size) flush();
}

const dashPrefs = {
  // getItem returns the cached string or null — the same contract as
  // localStorage.getItem, so callers' `=== '1'` / `?? default` checks
  // are unchanged.
  getItem(key) {
    const v = cache[key];
    return v === undefined ? null : v;
  },
  // setItem stringifies (localStorage does too), updates the cache, and
  // queues a debounced write-through.
  setItem(key, value) {
    const s = String(value);
    cache[key] = s;
    pending.set(key, { value: s });
    scheduleFlush();
  },
  // removeItem drops the cached value and queues a delete.
  removeItem(key) {
    delete cache[key];
    pending.set(key, { value: null });
    scheduleFlush();
  },
};

// pagehide covers tab close / navigation / bfcache; visibilitychange→
// hidden is the more reliable signal on mobile and some desktops.
window.addEventListener('pagehide', flushNow);
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'hidden') flushNow();
});

export { dashPrefs, initDashPrefs };
