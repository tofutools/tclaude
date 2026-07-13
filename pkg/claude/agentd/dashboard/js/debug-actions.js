async function responseError(response) {
  let detail = '';
  try { detail = await response.text(); } catch (_) {}
  return new Error(detail || `HTTP ${response.status}`);
}

function isAbort(error) {
  return error?.name === 'AbortError';
}

export function createDebugActions({
  state,
  fetchImpl = globalThis.fetch,
  AbortControllerImpl = globalThis.AbortController,
} = {}) {
  if (!state?.beginRequest) throw new TypeError('debug actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('debug actions require fetch');
  if (typeof AbortControllerImpl !== 'function') {
    throw new TypeError('debug actions require AbortController');
  }

  let current = null;
  let disposed = false;

  function cancel() {
    if (!current) return false;
    const request = current;
    current = null;
    request.controller.abort();
    state.cancelRequest(request.token);
    return true;
  }

  function begin(kind) {
    cancel();
    const request = {
      kind,
      token: state.beginRequest(kind),
      controller: new AbortControllerImpl(),
    };
    current = request;
    return request;
  }

  function finish(request) {
    if (current === request) current = null;
  }

  async function load() {
    if (disposed || state.resetting.value) return false;
    const request = begin('load');
    try {
      const response = await fetchImpl('/api/perf?limit=240', {
        credentials: 'same-origin',
        signal: request.controller.signal,
      });
      if (!response.ok) throw await responseError(response);
      return state.commitRequest(request.token, await response.json());
    } catch (error) {
      if (isAbort(error)) state.cancelRequest(request.token);
      else state.failRequest(request.token, error);
      return false;
    } finally {
      finish(request);
    }
  }

  async function reset() {
    if (disposed || state.resetting.value) return false;
    state.setResetting(true);
    const request = begin('reset');
    try {
      const response = await fetchImpl('/api/perf/reset', {
        method: 'POST',
        credentials: 'same-origin',
        signal: request.controller.signal,
      });
      if (!response.ok) throw await responseError(response);
      if (!state.acceptsRequest(request.token)) return false;
      state.cancelRequest(request.token);
      finish(request);
      state.setResetting(false);
      return load();
    } catch (error) {
      if (isAbort(error)) state.cancelRequest(request.token);
      else state.failRequest(request.token, error);
      return false;
    } finally {
      finish(request);
      state.setResetting(false);
    }
  }

  function dispose() {
    if (disposed) return;
    disposed = true;
    cancel();
    state.setResetting(false);
  }

  return Object.freeze({ load, reset, cancel, dispose });
}
