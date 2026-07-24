import { batch, computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { dashPrefs } from './prefs.js';
import { usageSeriesSort } from './usage-history-model.js';

// Per-series (provider × quota window) spans live under one JSON pref key.
// The legacy global keys seed the default span so an operator's last global
// choice carries over to series without an explicit per-series entry.
const SERIES_SPANS_KEY = 'tclaude.dash.usage.seriesSpans';
const LEGACY_HISTORY_HOURS_KEY = 'tclaude.dash.usage.historyHours';
const LEGACY_LOOKAHEAD_HOURS_KEY = 'tclaude.dash.usage.lookaheadHours';
const HISTORY_HOURS = [24, 168, 720, 2160];
const LOOKAHEAD_HOURS = [5, 24, 168, 720];
const DEFAULT_HISTORY_HOURS = 168;
const DEFAULT_LOOKAHEAD_HOURS = 168;

function errorMessage(error) { return String(error?.message || error); }

// A key must match the server's spans grammar (provider:window, both
// colon/comma-free) and the entry count stays under the server's override
// cap — otherwise one poisoned persisted entry would make every
// /api/usage-history request 400 until the preference is cleared by hand.
const SERIES_KEY_PATTERN = /^[^:,]+:[^:,]+$/;
const MAX_SERIES_SPAN_ENTRIES = 100;

function parseStoredSpans(raw) {
  if (!raw) return {};
  try {
    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const out = {};
    for (const [key, value] of Object.entries(parsed)) {
      if (!SERIES_KEY_PATTERN.test(key)) continue;
      if (Object.keys(out).length >= MAX_SERIES_SPAN_ENTRIES) break;
      const entry = {};
      if (HISTORY_HOURS.includes(Number(value?.hours))) entry.hours = Number(value.hours);
      if (LOOKAHEAD_HOURS.includes(Number(value?.lookaheadHours))) entry.lookaheadHours = Number(value.lookaheadHours);
      if (Object.keys(entry).length) out[key] = entry;
    }
    return out;
  } catch {
    return {};
  }
}

export function createUsageHistoryState({
  snapshot = dashboardState.snapshot,
  activeTab = dashboardState.activeTab,
  prefs = dashPrefs,
} = {}) {
  const seriesSpans = signal({});
  const defaultSpan = signal({ hours: DEFAULT_HISTORY_HOURS, lookaheadHours: DEFAULT_LOOKAHEAD_HOURS });
  const payload = signal(null);
  const request = signal({ phase: 'idle', requestId: 0, hasLoaded: false, error: null });
  let initialized = false;
  const spanFor = (spans, fallback, key) => ({
    hours: spans[key]?.hours ?? fallback.hours,
    lookaheadHours: spans[key]?.lookaheadHours ?? fallback.lookaheadHours,
  });
  const view = computed(() => {
    const snap = snapshot.value;
    const spans = seriesSpans.value;
    const fallback = defaultSpan.value;
    return {
      defaultHours: fallback.hours,
      payload: payload.value,
      series: [...(payload.value?.series || [])].sort(usageSeriesSort),
      spanFor: (key) => spanFor(spans, fallback, key),
      spanOverrides: Object.fromEntries(Object.entries(spans)
        .filter(([, entry]) => entry.hours !== undefined && entry.hours !== fallback.hours)
        .map(([key, entry]) => [key, entry.hours])),
      request: request.value,
      active: activeTab.value === 'usage',
      activeTab: activeTab.value,
      snapshotLoaded: snap !== null,
      visible: !!snap?.usage_tab_visible,
    };
  });
  function initialize() {
    if (initialized) return false;
    initialized = true;
    const legacyHours = Number(prefs.getItem(LEGACY_HISTORY_HOURS_KEY));
    const legacyLookahead = Number(prefs.getItem(LEGACY_LOOKAHEAD_HOURS_KEY));
    batch(() => {
      defaultSpan.value = {
        hours: HISTORY_HOURS.includes(legacyHours) ? legacyHours : DEFAULT_HISTORY_HOURS,
        lookaheadHours: LOOKAHEAD_HOURS.includes(legacyLookahead) ? legacyLookahead : DEFAULT_LOOKAHEAD_HOURS,
      };
      seriesSpans.value = parseStoredSpans(prefs.getItem(SERIES_SPANS_KEY));
    });
    return true;
  }
  function persistSpan(key, patch) {
    const next = { ...seriesSpans.value, [key]: { ...seriesSpans.value[key], ...patch } };
    seriesSpans.value = next;
    prefs.setItem(SERIES_SPANS_KEY, JSON.stringify(next));
  }
  function setSeriesHours(key, value) {
    const parsed = Number(value);
    if (typeof key !== 'string' || !SERIES_KEY_PATTERN.test(key) || !HISTORY_HOURS.includes(parsed)) return false;
    persistSpan(key, { hours: parsed });
    return true;
  }
  function setSeriesLookaheadHours(key, value) {
    const parsed = Number(value);
    if (typeof key !== 'string' || !SERIES_KEY_PATTERN.test(key) || !LOOKAHEAD_HOURS.includes(parsed)) return false;
    persistSpan(key, { lookaheadHours: parsed });
    return true;
  }
  function setDefaultHours(value) {
    const parsed = Number(value);
    if (!HISTORY_HOURS.includes(parsed)) return false;
    defaultSpan.value = { ...defaultSpan.value, hours: parsed };
    prefs.setItem(LEGACY_HISTORY_HOURS_KEY, String(parsed));
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
  function failMutation(error) {
    request.value = {
      ...request.value, phase: 'error', hasLoaded: payload.value !== null, error: errorMessage(error),
    };
  }
  return Object.freeze({
    seriesSpans, defaultSpan, payload, request, view,
    initialize, setDefaultHours, setSeriesHours, setSeriesLookaheadHours,
    beginRequest, commitRequest, failRequest, failMutation,
  });
}

export const usageHistoryState = createUsageHistoryState();
