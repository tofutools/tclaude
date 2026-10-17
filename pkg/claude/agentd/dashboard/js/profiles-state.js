import { batch, computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { activitySubtab as currentActivitySubtab, activityViewActive } from './activity-state.js';

function errorMessage(error) {
  if (!error) return null;
  return String(error.message || error);
}

export const PROFILE_DEFAULTS = Object.freeze({ seconds: 30, trace_seconds: 5, max_seconds: 600 });

// Daemon profiling (Activity › Performance): the capture list served by
// /api/profiles plus the "Profile daemon…" dialog draft.
export function createProfilesState({ activeTab = dashboardState.activeTab, activitySubtab = currentActivitySubtab } = {}) {
  const response = signal(null);
  const request = signal({ phase: 'idle', token: 0, error: null });
  const modal = signal(null);
  const notice = signal(null);
  const deleting = signal('');
  let latestToken = 0;
  let pendingToken = null;

  const view = computed(() => {
    const data = response.value;
    return {
      active: activityViewActive(activeTab, activitySubtab, 'performance'),
      captures: Array.isArray(data?.captures) ? data.captures : [],
      running: data?.running || '',
      defaults: {
        seconds: data?.defaults?.seconds || PROFILE_DEFAULTS.seconds,
        trace_seconds: data?.defaults?.trace_seconds ?? PROFILE_DEFAULTS.trace_seconds,
        max_seconds: data?.max_seconds || PROFILE_DEFAULTS.max_seconds,
      },
      loaded: data !== null,
      request: request.value,
      modal: modal.value,
      notice: notice.value,
      deleting: deleting.value,
    };
  });

  function beginRequest() {
    const token = ++latestToken;
    pendingToken = token;
    request.value = { phase: response.value === null ? 'loading' : 'refreshing', token, error: null };
    return token;
  }

  function acceptsRequest(token) {
    return token === latestToken && token === pendingToken;
  }

  function commitRequest(token, data) {
    if (!acceptsRequest(token)) return false;
    pendingToken = null;
    batch(() => {
      response.value = data;
      request.value = { phase: 'ready', token, error: null };
    });
    return true;
  }

  function failRequest(token, error) {
    if (!acceptsRequest(token)) return false;
    pendingToken = null;
    request.value = { phase: 'error', token, error: errorMessage(error) };
    return true;
  }

  function cancelRequest(token = pendingToken) {
    if (token === null || !acceptsRequest(token)) return false;
    pendingToken = null;
    request.value = { phase: response.value === null ? 'idle' : 'ready', token, error: null };
    return true;
  }

  function openModal() {
    const { defaults } = view.value;
    modal.value = {
      seconds: String(defaults.seconds),
      trace_seconds: String(defaults.trace_seconds),
      label: '',
      submitting: false,
      error: '',
    };
  }

  function updateModal(patch) {
    if (modal.value) modal.value = { ...modal.value, ...patch };
  }

  function closeModal() {
    modal.value = null;
  }

  function setNotice(text) {
    notice.value = text || null;
  }

  function setDeleting(id) {
    deleting.value = id || '';
  }

  return Object.freeze({
    response,
    request,
    modal,
    view,
    beginRequest,
    acceptsRequest,
    commitRequest,
    failRequest,
    cancelRequest,
    openModal,
    updateModal,
    closeModal,
    setNotice,
    setDeleting,
  });
}

export const profilesState = createProfilesState();
