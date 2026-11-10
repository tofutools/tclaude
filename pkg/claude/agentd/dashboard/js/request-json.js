// request-json.js — the dashboard's one JSON request helper for the action
// modules (peer, fleet, mail, spawn): send a request with the session cookie,
// read the JSON answer, and turn a failed status into a RequestError.
//
// Every failure carries the same shape, whatever module sent it:
//   status  — the HTTP status
//   code    — the daemon's machine-readable code (body.code), or ''
//   message — the daemon's text (body.error), or "HTTP <status>"
//   body    — the parsed error body, or null (a refused apply returns its plan)
// A body that is not JSON (or is empty, as a 204) reads as null.

export class RequestError extends Error {
  constructor(status, body) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code || '';
    this.body = body || null;
  }
}

// requestJSON sends body, when given, as JSON. ErrorClass lets a module keep
// its own error name (a RequestError subclass taking (status, body)).
export async function requestJSON(url, {
  method = 'GET', body, cache, fetchImpl = (...a) => globalThis.fetch(...a), ErrorClass = RequestError,
} = {}) {
  const init = { method, credentials: 'same-origin', headers: {} };
  if (cache) init.cache = cache;
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetchImpl(url, init);
  let data = null;
  try { data = await res.json(); } catch (_) { data = null; }
  if (!res.ok) throw new ErrorClass(res.status, data);
  return data;
}
