import { batch, computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';

function errorMessage(error) {
  if (!error) return null;
  return String(error.message || error);
}

function sortedEndpoints(response) {
  const endpoints = Array.isArray(response?.endpoints) ? response.endpoints.slice() : [];
  endpoints.sort((a, b) =>
    (b.endpoint === '/api/snapshot') - (a.endpoint === '/api/snapshot')
    || String(a.endpoint || '').localeCompare(String(b.endpoint || '')));
  return endpoints;
}

export function createDebugState({ activeTab = dashboardState.activeTab } = {}) {
  const response = signal(null);
  const request = signal({ phase: 'idle', token: 0, operation: null, error: null });
  const resetting = signal(false);
  let latestToken = 0;
  let pendingToken = null;

  const view = computed(() => ({
    active: activeTab.value === 'debug',
    response: response.value,
    endpoints: sortedEndpoints(response.value),
    request: request.value,
    resetting: resetting.value,
  }));

  function beginRequest(operation = 'load') {
    const token = ++latestToken;
    pendingToken = token;
    request.value = {
      phase: response.value === null ? 'loading' : 'refreshing',
      token,
      operation,
      error: null,
    };
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
      request.value = { phase: 'ready', token, operation: null, error: null };
    });
    return true;
  }

  function failRequest(token, error) {
    if (!acceptsRequest(token)) return false;
    pendingToken = null;
    batch(() => {
      response.value = null;
      request.value = { phase: 'error', token, operation: request.value.operation, error: errorMessage(error) };
    });
    return true;
  }

  function cancelRequest(token = pendingToken) {
    if (token === null || !acceptsRequest(token)) return false;
    pendingToken = null;
    request.value = {
      phase: response.value === null ? 'idle' : 'ready',
      token,
      operation: null,
      error: null,
    };
    return true;
  }

  function setResetting(value) {
    resetting.value = !!value;
  }

  return Object.freeze({
    response,
    request,
    resetting,
    view,
    beginRequest,
    acceptsRequest,
    commitRequest,
    failRequest,
    cancelRequest,
    setResetting,
  });
}

export const debugState = createDebugState();
