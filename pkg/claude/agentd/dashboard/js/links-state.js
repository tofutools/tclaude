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
  const managerOpen = signal(false);
  const editor = signal(null);
  let initialized = false;
  let editorKey = 0;

  const view = computed(() => {
    const all = snapshot.value?.links || [];
    const filtered = filterLinks(all, query.value);
    return {
      rows: applySortState(filtered, LINK_ACCESSORS, sort.value),
      groups: (snapshot.value?.groups || []).map((group) => group.name),
      filtered: filtered.length,
      total: all.length,
      query: query.value,
      sort: sort.value,
      managerOpen: managerOpen.value,
      editor: editor.value,
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

  function openManager() {
    if (managerOpen.value) return false;
    managerOpen.value = true;
    return true;
  }

  function closeManager() {
    // A child editor owns the visual stack while it is open. Refuse a
    // programmatic attempt to close the listing underneath it, just as the
    // topmost-overlay keyboard/backdrop guard refuses the same gesture.
    if (editor.value) return false;
    managerOpen.value = false;
    return true;
  }

  function openCreate({ preset = {} } = {}) {
    // Preserve an in-progress draft if a delegated launcher fires twice.
    if (editor.value) return false;
    editor.value = {
      kind: 'create', key: ++editorKey,
      preset: {
        from: preset.from || '',
        to: preset.to || '',
        linkMode: preset.linkMode || 'members->members',
      },
    };
    return true;
  }

  function openEdit({ id, from = '', to = '', mode = '' }) {
    if (editor.value) return false;
    editor.value = {
      kind: 'edit', key: ++editorKey, id: String(id || ''),
      from, to, linkMode: mode || 'members->members',
    };
    return true;
  }

  function closeEditor() {
    editor.value = null;
  }

  return Object.freeze({
    snapshot, query, sort, managerOpen, editor, view,
    initialize, publish, setQuery, cycleSort,
    openManager, closeManager, openCreate, openEdit, closeEditor,
  });
}

export const linksState = createLinksState();
