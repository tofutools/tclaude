import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { keyedLogRows, pageCount } from './logs-model.js';

function errorMessage(error) {
  let value = error?.message || String(error);
  if (error?.body) value += `: ${typeof error.body === 'string' ? error.body : (error.body.error || JSON.stringify(error.body))}`;
  return value;
}

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
  const request = signal({ phase: 'idle', token: 0, error: null });

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
  function beginRequest() {
    const token = request.value.token + 1;
    request.value = { phase: response.value ? 'refreshing' : 'loading', token, error: null };
    return token;
  }
  function commitRequest(token, data) {
    if (request.value.token !== token) return false;
    response.value = data;
    if (typeof data.page === 'number') page.value = data.page;
    if (typeof data.page_size === 'number') pageSize.value = data.page_size;
    request.value = { phase: 'ready', token, error: null };
    return true;
  }
  function failRequest(token, error) {
    if (request.value.token !== token) return false;
    response.value = null;
    request.value = { phase: 'error', token, error: errorMessage(error) };
    return true;
  }

  return Object.freeze({
    query, level, rangeMs, includeRotated, hideRaw, stream, page, pageSize,
    response, request, view, setFilter, setPage, setPageSize, setStream,
    beginRequest, commitRequest, failRequest,
  });
}

export const logsState = createLogsState();
