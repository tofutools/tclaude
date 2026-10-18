import { computed, effect, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { mapFleet, normalizeFleet, normalizeNode, normalizeStrangers } from './fleet-model.js';
import { parseFused } from './fleet-scope.js';
import { dashPrefs } from './prefs.js';
import { setSelfNode } from './node-api.js';

// SETUP_SEEN_PREF remembers that the operator has opened Fleet's setup guide,
// which retires the "set up" hint on the Fleet chip.
export const SETUP_SEEN_PREF = 'tclaude.dash.fleet.setupSeen';

// LAST_VIEW_PREF remembers the fleet view (map or Fleet config) the operator
// used last, which the ⌘ Fleet chip returns to.
export const LAST_VIEW_PREF = 'tclaude.dash.fleet.lastView';

// SHOW_UNTRUSTED_PREF remembers the map's "show untrusted" checkbox.
export const SHOW_UNTRUSTED_PREF = 'tclaude.dash.fleet.showUntrusted';

// SHOW_HUB_PREF remembers the map's "show hub" checkbox (shown by default).
export const SHOW_HUB_PREF = 'tclaude.dash.fleet.showHub';

function readLastView(prefs) {
  try { return prefs.getItem(LAST_VIEW_PREF) === 'fleet-admin' ? 'fleet-admin' : 'map'; } catch { return 'map'; }
}

function readSetupSeen(prefs) {
  try { return !!prefs.getItem(SETUP_SEEN_PREF); } catch { return false; }
}

// TOP_LEVEL_TABS are the multi-node views (the map and fleet administration): they hide the per-node tabs and are never a per-node tab to return to.
export const TOP_LEVEL_TABS = new Set(['map', 'fleet-admin']);

// createFleetState holds the chip row's fleet list and the map's per-node
// summaries. It owns no timers or fetches; fleet-actions.js loads, and the
// island's effects decide when (only while the map is on screen).
export function createFleetState({ activeTab = dashboardState.activeTab, now = () => Date.now(), search = globalThis.location?.search || '', pathname = globalThis.location?.pathname || '', prefs = dashPrefs } = {}) {
  const fleet = signal(null);
  // node is this node and its hub connection, known with or without peers:
  // the chip row renders from it and is greyed out until the hub is up.
  const node = signal(null);
  // fused is the node scope's fused set (fleet-scope.js): null shows one
  // node, else 'all' or the ticked instance IDs. It starts from ?nodes=, or
  // all nodes on the old /fleet address of the fused Groups view.
  const fused = signal(parseFused(search) || (/^\/fleet\/?$/.test(pathname) ? 'all' : null));
  // noFleet is set once a status read says there is no fleet (federation off,
  // or no trusted peer), as opposed to one that failed or has not settled.
  const noFleet = signal(false);
  // summaries maps instance ID -> { summary, etag, receivedAt, failure, failures }.
  const summaries = signal({});
  // strangers are the hub-visible instances not trusted here; the status read
  // asks for them all only while the map's "show untrusted" box is ticked,
  // and always returns the ones that asked to be trusted.
  const strangers = signal([]);
  // showUntrusted is this page's choice; null defers to the pref, read
  // lazily for the same reason as lastFleet below.
  const showUntrusted = signal(null);
  const showHub = signal(null);
  const focused = signal('');
  // statusLoaded flips once the first federation status read settles, so the
  // map can tell "no linked nodes" from "not loaded yet".
  const statusLoaded = signal(false);
  // setupSeen flips when the setup guide is opened here; the pref carries it
  // across reloads (read on each recompute, as prefs load after startup).
  const setupSeen = signal(false);
  // lastLocal remembers the per-node tab the operator left for the map, so the
  // ⌂ chip and "Open dashboard" return there rather than always to Groups.
  let lastLocal = activeTab.value && !TOP_LEVEL_TABS.has(activeTab.value) ? activeTab.value : 'groups';
  // lastFleet is this page's own choice; until there is one the saved pref
  // answers, read lazily because this module loads before initDashPrefs()
  // has filled the pref cache.
  let lastFleet = null;
  const stopTracking = effect(() => {
    const tab = activeTab.value;
    if (tab && !TOP_LEVEL_TABS.has(tab)) lastLocal = tab;
    if (TOP_LEVEL_TABS.has(tab) && tab !== lastFleet) {
      lastFleet = tab;
      try { prefs.setItem(LAST_VIEW_PREF, tab); } catch { /* best effort */ }
    }
  });
  const view = computed(() => ({
    mapActive: activeTab.value === 'map',
    adminActive: activeTab.value === 'fleet-admin',
    topLevel: TOP_LEVEL_TABS.has(activeTab.value),
    activeTab: activeTab.value,
    fleet: fleet.value,
    node: node.value,
    // mapFleet is the fleet, or this node alone while it is on a hub.
    mapFleet: mapFleet(fleet.value, node.value),
    summaries: summaries.value,
    focused: focused.value,
    statusLoaded: statusLoaded.value,
    fused: fused.value,
    noFleet: noFleet.value,
    setupSeen: setupSeen.value || readSetupSeen(prefs),
    showUntrusted: untrustedShown(),
    showHub: hubShown(),
    strangers: untrustedShown() ? strangers.value : strangers.value.filter((s) => s.request),
  }));
  function untrustedShown() {
    if (showUntrusted.value != null) return showUntrusted.value;
    try { return prefs.getItem(SHOW_UNTRUSTED_PREF) === '1'; } catch { return false; }
  }
  function setShowUntrusted(on) {
    showUntrusted.value = !!on;
    if (!on) strangers.value = strangers.value.filter((s) => s.request);
    try { prefs.setItem(SHOW_UNTRUSTED_PREF, on ? '1' : ''); } catch { /* best effort */ }
  }
  function hubShown() {
    if (showHub.value != null) return showHub.value;
    try { return prefs.getItem(SHOW_HUB_PREF) !== '0'; } catch { return true; }
  }
  function setShowHub(on) {
    showHub.value = !!on;
    try { prefs.setItem(SHOW_HUB_PREF, on ? '' : '0'); } catch { /* best effort */ }
  }
  function markStatusLoaded() { statusLoaded.value = true; }
  function setStatus(status) {
    node.value = normalizeNode(status);
    fleet.value = normalizeFleet(status);
    setSelfNode(fleet.value?.self?.id || '');
    strangers.value = normalizeStrangers(status);
    noFleet.value = !fleet.value;
    return fleet.value;
  }
  // clearFleet drops the fleet and this node's hub state, e.g. when the status
  // route is absent on an older daemon: the chip row stays, greyed out.
  function clearFleet() { fleet.value = null; node.value = null; strangers.value = []; noFleet.value = true; }
  function entry(id) { return summaries.value[id] || null; }
  function commitSummary(id, summary, etag) {
    const prev = entry(id);
    summaries.value = { ...summaries.value, [id]: { summary: summary ?? prev?.summary ?? null, etag: etag || prev?.etag || '', receivedAt: now(), failure: null, failures: 0 } };
  }
  function failSummary(id, failure) {
    const prev = entry(id);
    summaries.value = { ...summaries.value, [id]: { summary: prev?.summary ?? null, etag: prev?.etag || '', receivedAt: prev?.receivedAt ?? null, failure, failures: (prev?.failures || 0) + 1 } };
  }
  function markSetupSeen() {
    if (setupSeen.value) return;
    setupSeen.value = true;
    try { prefs.setItem(SETUP_SEEN_PREF, '1'); } catch { /* best effort */ }
  }
  function setFused(value) { fused.value = value || null; }
  function setFocused(id) { focused.value = id || ''; }
  function lastLocalTab() { return lastLocal; }
  function lastFleetTab() { return lastFleet || readLastView(prefs); }
  return Object.freeze({ fleet, node, fused, setFused, strangers, untrustedShown, setShowUntrusted, hubShown, setShowHub, summaries, focused, view, statusLoaded, markStatusLoaded, setStatus, clearFleet, markSetupSeen, entry, commitSummary, failSummary, setFocused, lastLocalTab, lastFleetTab, dispose: stopTracking });
}

export const fleetState = createFleetState();
