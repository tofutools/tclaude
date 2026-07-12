import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { AUDIT_SORT_KEYS, auditPageCount } from './audit-model.js';
import { createRequestLifecycle } from './request-lifecycle.js';

export function createAuditState({ activeTab = dashboardState.activeTab, now = () => Date.now() } = {}) {
  const query = signal(''); const outcome = signal(''); const source = signal('');
  const page = signal(1); const pageSize = signal(100); const sort = signal('time'); const dir = signal('desc');
  const response = signal(null);
  let lastFetchedAt = 0;
  const lifecycle = createRequestLifecycle({
    payload: response,
    retainPayloadOnRefresh: true,
    retainPayloadOnError: false,
    onBegin() { lastFetchedAt = now(); },
    onCommit(data) {
      if (typeof data.page === 'number') page.value = data.page;
      if (typeof data.page_size === 'number') pageSize.value = data.page_size;
      if (data.sort) sort.value = data.sort;
      if (data.dir) dir.value = data.dir;
    },
  });
  const { request, beginRequest, commitRequest, failRequest } = lifecycle;
  const view = computed(() => {
    const data = response.value;
    return {
      active: activeTab.value === 'audit', query: query.value, outcome: outcome.value, source: source.value,
      page: page.value, pageSize: pageSize.value, sort: sort.value, dir: dir.value,
      response: data, rows: data?.entries || [], total: data?.total || 0,
      totalUnfiltered: data?.total_unfiltered || 0, pages: auditPageCount(data?.total || 0, pageSize.value),
      request: request.value,
    };
  });
  function resetPage() { page.value = 1; }
  function setFilter(name, value) {
    const target = ({ query, outcome, source })[name]; if (!target) return false;
    target.value = value; resetPage(); return true;
  }
  function setPage(value) { page.value = Math.max(1, Math.min(view.value.pages, Number(value) || 1)); }
  function setPageSize(value) { pageSize.value = Number(value) || 100; resetPage(); }
  function cycleSort(key) {
    if (!AUDIT_SORT_KEYS.has(key)) return false;
    if (sort.value === key) dir.value = dir.value === 'asc' ? 'desc' : 'asc';
    else { sort.value = key; dir.value = (key === 'actor' || key === 'verb' || key === 'target') ? 'asc' : 'desc'; }
    resetPage(); return true;
  }
  function refreshDue(interval, value = now()) { return value - lastFetchedAt > interval; }
  return Object.freeze({ query, outcome, source, page, pageSize, sort, dir, response, request, view,
    setFilter, setPage, setPageSize, cycleSort, beginRequest, commitRequest, failRequest, refreshDue });
}
export const auditState = createAuditState();
