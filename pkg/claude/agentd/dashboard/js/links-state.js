import { batch, computed, signal } from '@preact/signals';
import { dashPrefs } from './prefs.js';
import { applySortState, LINK_ACCESSORS, persistTableSort, persistedTableSort } from './sort.js';

const FILTER_KEY = 'tclaude.dash.filter.links';

export function filterLinks(rows, query) {
  const needle = String(query || '').trim().toLowerCase();
  if (!needle) return rows;
  return rows.filter((link) =>
    String(link.from || '').toLowerCase().includes(needle) ||
    String(link.to || '').toLowerCase().includes(needle) ||
    String(link.mode || '').toLowerCase().includes(needle));
}

export function createLinksState({
  prefs = dashPrefs,
  readSort = persistedTableSort,
  writeSort = persistTableSort,
} = {}) {
  const snapshot = signal(null);
  const query = signal('');
  const sort = signal(null);
  let initialized = false;

  const view = computed(() => {
    const all = snapshot.value?.links || [];
    const filtered = filterLinks(all, query.value);
    return {
      rows: applySortState(filtered, LINK_ACCESSORS, sort.value),
      filtered: filtered.length,
      total: all.length,
      query: query.value,
      sort: sort.value,
    };
  });

  function initialize() {
    if (initialized) return false;
    initialized = true;
    batch(() => {
      query.value = prefs.getItem(FILTER_KEY) || '';
      sort.value = readSort('links');
    });
    return true;
  }

  function publish(value) {
    snapshot.value = value ? { ...value } : null;
  }

  function setQuery(value) {
    const next = String(value ?? '');
    query.value = next;
    if (next) prefs.setItem(FILTER_KEY, next);
    else prefs.removeItem(FILTER_KEY);
  }

  function cycleSort(column) {
    const current = sort.value;
    let next;
    if (!current || current.col !== column) next = { col: column, dir: 'asc' };
    else if (current.dir === 'asc') next = { col: column, dir: 'desc' };
    else next = null;
    sort.value = next;
    writeSort('links', next);
  }

  return Object.freeze({
    snapshot, query, sort, view,
    initialize, publish, setQuery, cycleSort,
  });
}

export const linksState = createLinksState();
