import { batch, computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { usageSeriesSort } from './usage-history-model.js';

function errorMessage(error) { return String(error?.message || error); }

export function createUsageHistoryState({ snapshot = dashboardState.snapshot, activeTab = dashboardState.activeTab } = {}) {
  const hours = signal(168);
  const lookaheadHours = signal(168);
  const payload = signal(null);
  const request = signal({ phase: 'idle', requestId: 0, hasLoaded: false, error: null });
  const view = computed(() => {
    const snap = snapshot.value;
    return {
      hours: hours.value,
      lookaheadHours: lookaheadHours.value,
      payload: payload.value,
      series: [...(payload.value?.series || [])].sort(usageSeriesSort),
      request: request.value,
      active: activeTab.value === 'usage',
      activeTab: activeTab.value,
      snapshotLoaded: snap !== null,
      visible: !!snap?.usage_tab_visible,
    };
  });
  function setHours(value) {
    const parsed = Number(value);
    if (![24, 168, 720, 2160].includes(parsed)) return false;
    hours.value = parsed;
    return true;
  }
  function setLookaheadHours(value) {
    const parsed = Number(value);
    if (![5, 24, 168, 720].includes(parsed)) return false;
    lookaheadHours.value = parsed;
    return true;
  }
  function beginRequest(requestId) { request.value = { ...request.value, phase: 'loading', requestId, error: null }; }
  function commitRequest(requestId, data) {
    if (request.value.requestId !== requestId) return false;
    batch(() => {
      payload.value = data;
      request.value = { phase: 'ready', requestId, hasLoaded: true, error: null };
    });
    return true;
  }
  function failRequest(requestId, error) {
    if (request.value.requestId !== requestId) return false;
    payload.value = null;
    request.value = { phase: 'error', requestId, hasLoaded: false, error: errorMessage(error) };
    return true;
  }
  return Object.freeze({
    hours, lookaheadHours, payload, request, view,
    setHours, setLookaheadHours, beginRequest, commitRequest, failRequest,
  });
}

export const usageHistoryState = createUsageHistoryState();
