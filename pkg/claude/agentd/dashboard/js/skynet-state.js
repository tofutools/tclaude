import { computed, effect, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { normalizeFleet } from './skynet-model.js';

// createSkynetState holds the chip row's fleet list and the map's per-node
// summaries. It owns no timers or fetches; skynet-actions.js loads, and the
// island's effects decide when (only while the map is on screen).
export function createSkynetState({ activeTab = dashboardState.activeTab, now = () => Date.now() } = {}) {
  const fleet = signal(null);
  // summaries maps instance ID -> { summary, etag, receivedAt, failure, failures }.
  const summaries = signal({});
  const focused = signal('');
  // statusLoaded flips once the first federation status read settles, so the
  // map can tell "no linked nodes" from "not loaded yet".
  const statusLoaded = signal(false);
  // lastLocal remembers the per-node tab the operator left for the map, so the
  // ⌂ chip and "Open dashboard" return there rather than always to Groups.
  let lastLocal = activeTab.value && activeTab.value !== 'map' ? activeTab.value : 'groups';
  const stopTracking = effect(() => { const tab = activeTab.value; if (tab && tab !== 'map') lastLocal = tab; });
  const view = computed(() => ({
    mapActive: activeTab.value === 'map',
    activeTab: activeTab.value,
    fleet: fleet.value,
    summaries: summaries.value,
    focused: focused.value,
    statusLoaded: statusLoaded.value,
  }));
  function markStatusLoaded() { statusLoaded.value = true; }
  function setStatus(status) {
    fleet.value = normalizeFleet(status);
    return fleet.value;
  }
  // clearFleet hides the chip row, e.g. when the status route is absent on an
  // older daemon or federation is disabled.
  function clearFleet() { fleet.value = null; }
  function entry(id) { return summaries.value[id] || null; }
  function commitSummary(id, summary, etag) {
    const prev = entry(id);
    summaries.value = { ...summaries.value, [id]: { summary: summary ?? prev?.summary ?? null, etag: etag || prev?.etag || '', receivedAt: now(), failure: null, failures: 0 } };
  }
  function failSummary(id, failure) {
    const prev = entry(id);
    summaries.value = { ...summaries.value, [id]: { summary: prev?.summary ?? null, etag: prev?.etag || '', receivedAt: prev?.receivedAt ?? null, failure, failures: (prev?.failures || 0) + 1 } };
  }
  function setFocused(id) { focused.value = id || ''; }
  function lastLocalTab() { return lastLocal; }
  return Object.freeze({ fleet, summaries, focused, view, statusLoaded, markStatusLoaded, setStatus, clearFleet, entry, commitSummary, failSummary, setFocused, lastLocalTab, dispose: stopTracking });
}

export const skynetState = createSkynetState();
