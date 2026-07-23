import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';

export const NOTIFY_TYPES = Object.freeze([
  'idle',
  'awaiting_permission',
  'awaiting_input',
  'error',
  'exited',
]);

export const NOTIFY_DELIVERIES = Object.freeze(['os', 'browser', 'both']);

export function normalizeNotifySettings(value) {
  const source = value && typeof value === 'object' ? value : {};
  const types = source.types && typeof source.types === 'object' ? source.types : {};
  return Object.freeze({
    enabled: !!source.enabled,
    types: Object.freeze(Object.fromEntries(NOTIFY_TYPES.map((type) => [type, !!types[type]]))),
    // The daemon's compatibility default is on: only an explicit false disables
    // desktop banners for messages sent through human.notify.
    humanMessages: source.human_messages !== false,
    accessRequests: !!source.access_requests,
    // Where a decided notification is raised. Absent / unrecognised → 'os',
    // the historical desktop-only behaviour.
    delivery: NOTIFY_DELIVERIES.includes(source.delivery) ? source.delivery : 'os',
  });
}

function errorMessage(error) {
  return error ? String(error.message || error) : null;
}

export function createNotifyState({ snapshot = dashboardState.snapshot } = {}) {
  const open = signal(false);
  const settings = signal(normalizeNotifySettings(null));
  const request = signal({
    phase: 'idle',
    requestId: 0,
    hasLoaded: false,
    error: null,
  });
  let nextRequestId = 0;

  const view = computed(() => {
    const snap = snapshot.value;
    return {
      open: open.value,
      // Keep the bell's historical source of truth: the accepted dashboard
      // snapshot. GET/POST responses authoritatively repaint the popover fields;
      // the next snapshot reconciles the global bell.
      bellReady: snap !== null && snap !== undefined,
      bellEnabled: !!snap?.notifications_enabled,
      settings: settings.value,
    };
  });

  function setOpen(value) {
    const next = !!value;
    if (open.value === next) return false;
    open.value = next;
    return true;
  }

  function beginRequest() {
    const requestId = ++nextRequestId;
    request.value = {
      ...request.value,
      phase: 'loading',
      requestId,
      error: null,
    };
    return requestId;
  }

  function acceptsRequest(requestId) {
    return request.value.requestId === requestId;
  }

  function commitRequest(requestId, value) {
    if (!acceptsRequest(requestId)) return false;
    settings.value = normalizeNotifySettings(value);
    request.value = {
      phase: 'ready',
      requestId,
      hasLoaded: true,
      error: null,
    };
    return true;
  }

  function failRequest(requestId, error) {
    if (!acceptsRequest(requestId)) return false;
    request.value = {
      ...request.value,
      phase: 'error',
      error: errorMessage(error),
    };
    return true;
  }

  return Object.freeze({
    open,
    settings,
    request,
    view,
    setOpen,
    beginRequest,
    acceptsRequest,
    commitRequest,
    failRequest,
  });
}

export const notifyState = createNotifyState();
