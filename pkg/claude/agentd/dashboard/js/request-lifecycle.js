import { batch, signal } from '@preact/signals';

export function requestErrorMessage(error) {
  let value = error?.message || String(error);
  if (error?.body) {
    value += `: ${typeof error.body === 'string' ? error.body : (error.body.error || JSON.stringify(error.body))}`;
  }
  return value;
}

// Shared query lifecycle for bounded islands. Payload retention is deliberately
// selected by every consumer: list metadata is often unsafe after a failed
// filtered request, while other features may prefer stale-but-visible data.
// Mutations stay feature-owned because their concurrency and rollback semantics
// are not equivalent to an idempotent query.
export function createRequestLifecycle({
  payload,
  retainPayloadOnRefresh,
  retainPayloadOnError,
  onBegin = () => {},
  onCommit = () => {},
} = {}) {
  if (!payload || !('value' in payload)) throw new TypeError('request lifecycle requires a payload signal');
  if (typeof retainPayloadOnRefresh !== 'boolean') {
    throw new TypeError('request lifecycle requires an explicit retainPayloadOnRefresh policy');
  }
  if (typeof retainPayloadOnError !== 'boolean') {
    throw new TypeError('request lifecycle requires an explicit retainPayloadOnError policy');
  }
  const request = signal({ phase: 'idle', token: 0, error: null });

  function beginRequest() {
    const token = request.value.token + 1;
    onBegin(token);
    const refreshing = retainPayloadOnRefresh && payload.value !== null;
    batch(() => {
      if (!retainPayloadOnRefresh) payload.value = null;
      request.value = { phase: refreshing ? 'refreshing' : 'loading', token, error: null };
    });
    return token;
  }

  function acceptsRequest(token) { return request.value.token === token; }

  function commitRequest(token, data) {
    if (!acceptsRequest(token)) return false;
    batch(() => {
      payload.value = data;
      onCommit(data, token);
      request.value = { phase: 'ready', token, error: null };
    });
    return true;
  }

  function failRequest(token, error) {
    if (!acceptsRequest(token)) return false;
    batch(() => {
      if (!retainPayloadOnError) payload.value = null;
      request.value = { phase: 'error', token, error: requestErrorMessage(error) };
    });
    return true;
  }

  return Object.freeze({ request, beginRequest, acceptsRequest, commitRequest, failRequest });
}
