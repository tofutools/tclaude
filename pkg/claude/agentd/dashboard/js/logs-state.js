import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { keyedLogRows, pageCount } from './logs-model.js';
import { createRequestLifecycle } from './request-lifecycle.js';

export function createLogsState({ activeTab = dashboardState.activeTab } = {}) {
  const query = signal('');
  const level = signal('');
  const rangeMs = signal(0);
  const includeRotated = signal(false);
  const hideRaw = signal(false);
  const stream = signal(false);
  const page = signal(1);
  const pageSize = signal(100);
  const response = signal(null);
  const lifecycle = createRequestLifecycle({
    payload: response,
    retainPayloadOnRefresh: true,
    retainPayloadOnError: false,
    onCommit(data) {
      if (typeof data.page === 'number') page.value = data.page;
      if (typeof data.page_size === 'number') pageSize.value = data.page_size;
    },
  });
  const { request, beginRequest, commitRequest, failRequest } = lifecycle;

  const view = computed(() => {
    const data = response.value;
    const pages = pageCount(data?.total || 0, pageSize.value);
    return {
      active: activeTab.value === 'logs', query: query.value, level: level.value,
      rangeMs: rangeMs.value, includeRotated: includeRotated.value, hideRaw: hideRaw.value,
      stream: stream.value, page: page.value, pageSize: pageSize.value,
      response: data, rows: keyedLogRows(data?.entries || []), pages,
      total: data?.total || 0, totalUnfiltered: data?.total_unfiltered || 0,
      request: request.value,
    };
  });

  function resetPage() { page.value = 1; }
  function setFilter(name, value) {
    if (!({ query, level, rangeMs, includeRotated, hideRaw })[name]) return false;
    ({ query, level, rangeMs, includeRotated, hideRaw })[name].value = value;
    resetPage();
    return true;
  }
  function setPage(value) { page.value = Math.max(1, Math.min(view.value.pages, Number(value) || 1)); }
  function setPageSize(value) { pageSize.value = Number(value) || 100; resetPage(); }
  function setStream(value) { stream.value = Boolean(value); if (value) resetPage(); }
  return Object.freeze({
    query, level, rangeMs, includeRotated, hideRaw, stream, page, pageSize,
    response, request, view, setFilter, setPage, setPageSize, setStream,
    beginRequest, commitRequest, failRequest,
  });
}

export const logsState = createLogsState();
