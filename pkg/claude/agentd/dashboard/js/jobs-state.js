import { batch, computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { dashPrefs } from './prefs.js';
import {
  applySortState, JOBS_ACCESSORS, persistedTableSort, persistTableSort,
} from './sort.js';
import { cronJobToPrefill, standingOrderToPrefill } from './jobs-dialog-model.js';

const FILTER_KEY = 'tclaude.dash.filter.jobs';
const KIND_KEY = 'tclaude.dash.jobs.kind';
const PAGE_SIZE_KEY = 'tclaude.dash.list.jobs.pagesize';
const SORT_KEY = 'tclaude.dash.sort';
export const JOBS_PAGE_SIZES = [25, 50, 100, 200];
export const JOBS_KINDS = ['all', 'export', 'cron', 'standing-order'];
export const JOBS_KIND_SUBTABS = Object.freeze({
  all: '',
  'export': 'exports',
  cron: 'cron-jobs',
  'standing-order': 'standing-orders',
});
const DEFAULT_PAGE_SIZE = 50;

export function jobsLocation(kind) {
  const normalized = JOBS_KINDS.includes(kind) ? kind : 'all';
  const subtab = JOBS_KIND_SUBTABS[normalized];
  return subtab ? { tab: 'jobs', subtab } : { tab: 'jobs' };
}

export function jobsKindForLocation(loc) {
  if (!loc || loc.tab !== 'jobs' || !loc.subtab) return 'all';
  return JOBS_KINDS.find((kind) => JOBS_KIND_SUBTABS[kind] === loc.subtab) || 'all';
}

function errorMessage(error) {
  return error ? String(error.message || error) : null;
}

function nextSort(current, col) {
  if (!current || current.col !== col) return { col, dir: 'asc' };
  if (current.dir === 'asc') return { col, dir: 'desc' };
  return null;
}

export function createJobsState({ snapshot = dashboardState.snapshot, prefs = dashPrefs } = {}) {
  const query = signal('');
  const kind = signal('all');
  const offset = signal(0);
  const limit = signal(DEFAULT_PAGE_SIZE);
  const sort = signal(null);
  const request = signal({
    phase: 'idle',
    requestId: 0,
    hasLoaded: false,
    error: null,
  });
  const dialog = signal(null);
  const orderDialog = signal(null);
  let initialized = false;
  let nextLaunchID = 0;

  const params = computed(() => {
    const search = new URLSearchParams({
      offset: String(offset.value),
      limit: String(limit.value),
    });
    if (kind.value !== 'all') search.set('kind', kind.value);
    const value = query.value.trim();
    if (value) search.set('q', value);
    return search.toString();
  });
  const location = computed(() => jobsLocation(kind.value));

  const view = computed(() => {
    const value = snapshot.value;
    const rows = value?.jobs || [];
    const paging = value?.paging?.jobs || {
      offset: offset.value,
      limit: limit.value,
      total: rows.length,
      total_unfiltered: rows.length,
    };
    return {
      rows: applySortState(rows, JOBS_ACCESSORS, sort.value),
      paging,
      activeExports: value?.export_jobs_active || 0,
      query: query.value,
      kind: kind.value,
      sort: sort.value,
      request: request.value,
      dialog: dialog.value,
      orderDialog: orderDialog.value,
      dashboard: value,
    };
  });

  function initialize() {
    if (initialized) return false;
    initialized = true;
    query.value = prefs.getItem(FILTER_KEY) || '';
    const savedKind = prefs.getItem(KIND_KEY) || '';
    kind.value = JOBS_KINDS.includes(savedKind) ? savedKind : 'all';
    const savedLimit = Number.parseInt(prefs.getItem(PAGE_SIZE_KEY) || '', 10);
    limit.value = JOBS_PAGE_SIZES.includes(savedLimit) ? savedLimit : DEFAULT_PAGE_SIZE;
    const savedSort = prefs === dashPrefs
      ? persistedTableSort('jobs')
      : (() => {
          try { return JSON.parse(prefs.getItem(SORT_KEY) || '{}')?.jobs; }
          catch { return null; }
        })();
    if (savedSort && JOBS_ACCESSORS[savedSort.col] &&
        (savedSort.dir === 'asc' || savedSort.dir === 'desc')) {
      sort.value = savedSort;
    }
    return true;
  }

  function setQuery(value) {
    const next = String(value ?? '');
    batch(() => {
      query.value = next;
      offset.value = 0;
    });
    if (next) prefs.setItem(FILTER_KEY, next);
    else prefs.removeItem(FILTER_KEY);
    invalidateRequest();
  }

  function setKind(value) {
    const next = JOBS_KINDS.includes(value) ? value : 'all';
    if (kind.value === next) return false;
    batch(() => {
      kind.value = next;
      offset.value = 0;
    });
    if (next === 'all') prefs.removeItem(KIND_KEY);
    else prefs.setItem(KIND_KEY, next);
    invalidateRequest();
    return true;
  }

  function applyLocation(loc) {
    return setKind(jobsKindForLocation(loc));
  }

  function cycleSort(col) {
    const next = nextSort(sort.value, col);
    sort.value = next;
    if (prefs === dashPrefs) persistTableSort('jobs', next);
    else {
      let all = {};
      try { all = JSON.parse(prefs.getItem(SORT_KEY) || '{}') || {}; } catch { /* replace malformed value */ }
      if (next) all.jobs = next;
      else delete all.jobs;
      prefs.setItem(SORT_KEY, JSON.stringify(all));
    }
  }

  function page(action, total) {
    const current = offset.value;
    const size = limit.value;
    const last = total > 0 ? Math.floor((total - 1) / size) * size : 0;
    const destinations = {
      first: 0,
      prev: Math.max(0, current - size),
      next: Math.min(last, current + size),
      last,
    };
    if (!(action in destinations) || destinations[action] === current) return false;
    offset.value = destinations[action];
    invalidateRequest();
    return true;
  }

  function setPageSize(value) {
    const size = JOBS_PAGE_SIZES.includes(Number(value)) ? Number(value) : DEFAULT_PAGE_SIZE;
    batch(() => {
      limit.value = size;
      offset.value = 0;
    });
    prefs.setItem(PAGE_SIZE_KEY, String(size));
    invalidateRequest();
  }

  function syncServedOffset(value) {
    if (typeof value === 'number' && value >= 0) offset.value = value;
  }

  function beginRequest(requestId) {
    request.value = { ...request.value, phase: 'loading', requestId, error: null };
  }

  function acceptsRequest(requestId) {
    return request.value.requestId === requestId;
  }

  function invalidateRequest() {
    if (!request.value.requestId) return false;
    request.value = {
      ...request.value,
      phase: request.value.hasLoaded ? 'ready' : 'idle',
      requestId: 0,
      error: null,
    };
    return true;
  }

  function commitRequest(requestId) {
    if (request.value.requestId !== requestId) return false;
    request.value = { phase: 'ready', requestId, hasLoaded: true, error: null };
    return true;
  }

  function failRequest(requestId, error) {
    if (request.value.requestId !== requestId) return false;
    request.value = { ...request.value, phase: 'error', error: errorMessage(error) };
    return true;
  }

  function discardRequest(requestId) {
    if (request.value.requestId !== requestId) return false;
    request.value = {
      ...request.value,
      phase: request.value.hasLoaded ? 'ready' : 'idle',
      error: null,
    };
    return true;
  }

  // A successful cron create/edit response is canonical server state. Publish
  // that row immediately; the next authoritative poll replaces the full page.
  function upsertCron(cron) {
    const value = snapshot.value;
    if (!value || !cron) return false;
    const rows = [...(value.jobs || [])];
    const index = rows.findIndex(row => row.kind === 'cron' && row.cron?.id === cron.id);
    const next = { kind: 'cron', cron };
    // Replace only a row already present in this server-selected window. A new
    // row may not match the active query/page; the Jobs mutation action triggers a refetch.
    if (index < 0) return false;
    rows[index] = next;
    snapshot.value = { ...value, jobs: rows };
    return true;
  }

  function openCronDialog(descriptor) {
    if (dialog.value || orderDialog.value) return false;
    dialog.value = { ...descriptor, launchID: ++nextLaunchID };
    return true;
  }

  function openCronCreate(prefill = {}) {
    return openCronDialog({ kind: 'create', prefill: { ...prefill }, originalExpr: '', originalTarget: '' });
  }

  function openCronEdit(job = {}) {
    return openCronDialog({
      kind: 'edit', id: job.id, job: { ...job }, prefill: cronJobToPrefill(job),
      originalExpr: job.cron_expr || '',
      originalTarget: job.target_agent || job.target_conv || '',
    });
  }

  function openCronDuplicate(job = {}) {
    return openCronDialog({
      kind: 'duplicate', sourceID: job.id, job: { ...job },
      prefill: cronJobToPrefill(job, { duplicate: true }),
      originalExpr: '', originalTarget: '',
    });
  }

  function closeCronDialog() {
    dialog.value = null;
  }

  function openStandingOrderDialog(descriptor) {
    if (dialog.value || orderDialog.value) return false;
    orderDialog.value = { ...descriptor, launchID: ++nextLaunchID };
    return true;
  }

  function openStandingOrderCreate(prefill = {}, { onCancel = null } = {}) {
    return openStandingOrderDialog({ kind: 'create', prefill: { ...prefill }, onCancel });
  }

  function openStandingOrderEdit(order = {}) {
    return openStandingOrderDialog({
      kind: 'edit', id: order.id, order: { ...order },
      prefill: standingOrderToPrefill(order),
    });
  }

  function closeStandingOrderDialog({ cancelled = false } = {}) {
    const closing = orderDialog.value;
    orderDialog.value = null;
    if (cancelled && typeof closing?.onCancel === 'function') closing.onCancel();
  }

  return Object.freeze({
    query, kind, offset, limit, sort, request, dialog, orderDialog, params, view, location,
    initialize, setQuery, setKind, applyLocation, cycleSort, page, setPageSize, syncServedOffset,
    beginRequest, acceptsRequest, invalidateRequest,
    commitRequest, failRequest, discardRequest, upsertCron,
    openCronCreate, openCronEdit, openCronDuplicate, closeCronDialog,
    openStandingOrderCreate, openStandingOrderEdit, closeStandingOrderDialog,
  });
}

export const jobsState = createJobsState();
