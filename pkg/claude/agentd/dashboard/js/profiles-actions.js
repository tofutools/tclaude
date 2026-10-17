async function responseError(response) {
  let detail = '';
  try { detail = await response.text(); } catch (_) {}
  return new Error(detail.trim() || `HTTP ${response.status}`);
}

function isAbort(error) {
  return error?.name === 'AbortError';
}

// parseDraftNumber turns a dialog field into an integer, or null when it is
// not a whole number.
function parseDraftNumber(value) {
  const text = String(value ?? '').trim();
  if (!/^\d+$/.test(text)) return null;
  return Number(text);
}

export function validateProfileDraft(draft, maxSeconds) {
  const seconds = parseDraftNumber(draft.seconds);
  if (seconds === null || seconds < 1 || seconds > maxSeconds) {
    return { error: `Duration must be a whole number of seconds between 1 and ${maxSeconds}.` };
  }
  const traceSeconds = parseDraftNumber(draft.trace_seconds);
  if (traceSeconds === null) {
    return { error: 'Trace must be a whole number of seconds (0 skips the trace).' };
  }
  return {
    body: {
      seconds,
      trace_seconds: Math.min(traceSeconds, seconds),
      label: String(draft.label || '').trim(),
    },
  };
}

export function createProfilesActions({
  state,
  fetchImpl = globalThis.fetch,
  AbortControllerImpl = globalThis.AbortController,
  confirm = async () => true,
  pickAgent = async () => '',
} = {}) {
  if (!state?.beginRequest) throw new TypeError('profiles actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('profiles actions require fetch');

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

  async function load() {
    if (disposed) return false;
    cancel();
    const request = { token: state.beginRequest(), controller: new AbortControllerImpl() };
    current = request;
    try {
      const response = await fetchImpl('/api/profiles', {
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
      if (current === request) current = null;
    }
  }

  async function start() {
    const draft = state.modal.value;
    if (disposed || !draft || draft.submitting) return false;
    const { body, error } = validateProfileDraft(draft, state.view.value.defaults.max_seconds);
    if (error) {
      state.updateModal({ error });
      return false;
    }
    state.updateModal({ submitting: true, error: '' });
    try {
      const response = await fetchImpl('/api/profiles', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!response.ok) throw await responseError(response);
      state.closeModal();
      state.setNotice(`Profiling the daemon for ${body.seconds}s — the capture appears below when it finishes.`);
      await load();
      return true;
    } catch (err) {
      state.updateModal({ submitting: false, error: String(err.message || err) });
      return false;
    }
  }

  async function remove(capture) {
    if (disposed || !capture?.id) return false;
    const approved = await confirm({
      title: 'Delete profile capture?',
      body: `Deletes every file of capture ${capture.id}${capture.label ? ` (“${capture.label}”)` : ''} from ~/.tclaude/data/profiles. This cannot be undone.`,
      okLabel: 'Delete',
    });
    if (!approved) return false;
    state.setDeleting(capture.id);
    try {
      const response = await fetchImpl(`/api/profiles/${encodeURIComponent(capture.id)}`, {
        method: 'DELETE',
        credentials: 'same-origin',
      });
      if (!response.ok) throw await responseError(response);
      state.setNotice(null);
      await load();
      return true;
    } catch (err) {
      state.setNotice(`Could not delete ${capture.id}: ${String(err.message || err)}`);
      return false;
    } finally {
      state.setDeleting('');
    }
  }

  // share mails a finished capture's files to an agent picked from the
  // shared agent picker; they arrive as message attachments.
  async function share(capture) {
    if (disposed || !capture?.id) return false;
    const to = await pickAgent({ title: `Share capture ${capture.id} with`, identity: 'agent' });
    if (!to || disposed) return false;
    state.setNotice(`Sharing ${capture.id}…`);
    try {
      const response = await fetchImpl(`/api/profiles/${encodeURIComponent(capture.id)}/share`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ to }),
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(payload.error || `HTTP ${response.status}`);
      const skipped = Array.isArray(payload.skipped) && payload.skipped.length
        ? ` (too large to attach: ${payload.skipped.join(', ')})` : '';
      state.setNotice(`Sent ${capture.id} to ${payload.title || to} as ${payload.files} attached files${skipped}.`);
      return true;
    } catch (err) {
      state.setNotice(`Could not share ${capture.id}: ${String(err.message || err)}`);
      return false;
    }
  }

  function dispose() {
    if (disposed) return;
    disposed = true;
    cancel();
  }

  return Object.freeze({ load, start, remove, share, cancel, dispose });
}
