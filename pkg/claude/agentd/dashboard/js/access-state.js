import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { dashPrefs } from './prefs.js';
import {
  ACCESS_SUBTABS, matchesSudo, permissionRows, remainingSeconds, sortSudo,
} from './access-model.js';
import { persistedTableSort, persistTableSort } from './sort.js';

const SUDO_FILTER_KEY = 'tclaude.dash.filter.sudo';

function message(error) {
  let detail = error?.message || String(error);
  if (error?.body != null) {
    const body = typeof error.body === 'string' ? error.body : (error.body.error || error.body.message || JSON.stringify(error.body));
    if (body) detail += `: ${body}`;
  }
  return detail;
}

export function createAccessState({
  snapshot = dashboardState.snapshot,
  prefs = dashPrefs,
  now = () => Date.now(),
} = {}) {
  const subtab = signal('permissions');
  const sudoQuery = signal('');
  const sudoSort = signal(null);
  const clock = signal(now());
  const mutation = signal({ busy: new Set(), error: null });
  let initialized = false;

  const view = computed(() => {
    const data = snapshot.value;
    const permissions = data?.permissions ?? null;
    const slugs = data?.slugs ?? null;
    const snapshotMs = Date.parse(data?.generated_at || '');
    const allSudo = (data?.sudo || []).map((row) => ({
      ...row,
      remaining_seconds: remainingSeconds(row, clock.value, snapshotMs),
    })).filter((row) => row.remaining_seconds > 0);
    const filteredSudo = sortSudo(allSudo.filter((row) => matchesSudo(row, sudoQuery.value)), sudoSort.value);
    return {
      snapshotLoaded: data !== null,
      subtab: subtab.value,
      permissions,
      defaults: permissions?.defaults || [],
      permissionRows: permissionRows(permissions, data?.agents || []),
      slugs,
      sudoAvailable: data !== null && Array.isArray(data?.sudo),
      sudo: filteredSudo,
      sudoTotal: allSudo.length,
      sudoQuery: sudoQuery.value,
      sudoSort: sudoSort.value,
      mutation: mutation.value,
    };
  });

  function initialize() {
    if (initialized) return false;
    initialized = true;
    sudoQuery.value = prefs.getItem(SUDO_FILTER_KEY) || '';
    const saved = persistedTableSort('sudo');
    if (saved && SUDO_COLUMNS_KEYS.has(saved.col) && (saved.dir === 'asc' || saved.dir === 'desc')) {
      sudoSort.value = { key: saved.col, dir: saved.dir };
    }
    return true;
  }

  function setSubtab(value) {
    if (!ACCESS_SUBTABS.includes(value) || subtab.value === value) return false;
    subtab.value = value;
    return true;
  }
  function setSudoQuery(value) {
    const next = String(value ?? '');
    sudoQuery.value = next;
    if (next) prefs.setItem(SUDO_FILTER_KEY, next); else prefs.removeItem(SUDO_FILTER_KEY);
  }
  function cycleSudoSort(key) {
    if (!SUDO_COLUMNS_KEYS.has(key)) return false;
    const current = sudoSort.value;
    let next;
    if (!current || current.key !== key) next = { key, dir: 'asc' };
    else if (current.dir === 'asc') next = { key, dir: 'desc' };
    else next = null;
    sudoSort.value = next;
    persistTableSort('sudo', next ? { col: next.key, dir: next.dir } : null);
    return true;
  }
  function tick(value = now()) { clock.value = value; }
  function beginMutation(key) {
    if (mutation.value.busy.has(key)) return false;
    const busy = new Set(mutation.value.busy);
    busy.add(key);
    mutation.value = { busy, error: null };
    return true;
  }
  function endMutation(key) {
    const busy = new Set(mutation.value.busy);
    busy.delete(key);
    mutation.value = { ...mutation.value, busy };
  }
  function failMutation(key, error) {
    const busy = new Set(mutation.value.busy);
    busy.delete(key);
    mutation.value = { busy, error: message(error) };
  }

  return Object.freeze({
    subtab, sudoQuery, sudoSort, clock, mutation, view, initialize, setSubtab,
    setSudoQuery, cycleSudoSort, tick, beginMutation, endMutation, failMutation,
  });
}

const SUDO_COLUMNS_KEYS = new Set(['conv', 'slug', 'granted', 'expires', 'reason', 'by']);
export const accessState = createAccessState();
