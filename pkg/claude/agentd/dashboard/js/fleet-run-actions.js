import { FleetAdminError } from './fleet-admin-actions.js';

// Limits the daemon enforces on a script request (noderun.Request.Validate).
export const SCRIPT_MAX_BYTES = 16 * 1024;
export const TIMEOUT_DEFAULT_S = 3600;
export const TIMEOUT_MAX_S = 86400;

// LOG_MAX_BYTES caps one full-log read in the browser; the daemon keeps at
// most 4 MiB per stream.
const LOG_MAX_BYTES = 4 * 1024 * 1024;

// runBase addresses a node's script routes: this node directly, a peer
// through the peer proxy (which needs node.exec there plus its local accept
// switch).
export function runBase(node) {
  return node?.local ? '/api/node/run' : `/api/peer/${encodeURIComponent(node.id)}/node/run`;
}

export function runActive(job) {
  return job?.state === 'running';
}

// runOK is a finished job that exited 0; anything else is re-run material.
export function runOK(job) {
  return job?.state === 'completed' && job.exit_code === 0;
}

export function scriptBytes(text) {
  return new TextEncoder().encode(text || '').length;
}

function decodeBase64(data) {
  if (!data) return new Uint8Array(0);
  const bin = globalThis.atob(data);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// createRunActions wraps /api/node/run (and its peer-proxied form) plus the
// local-only settings route. Errors are FleetAdminError with the daemon code.
export function createRunActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(method, url, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetchImpl(url, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) throw new FleetAdminError(res.status, data);
    return data;
  }
  return Object.freeze({
    // status is the node's receiving settings: whether it accepts remote
    // scripts and its limits. A granted peer can read it while closed.
    status: (node) => call('GET', runBase(node)),
    start: (node, script, timeoutSeconds) => call('POST', runBase(node), timeoutSeconds ? { script, timeout_seconds: timeoutSeconds } : { script }),
    job: (node, id) => call('GET', `${runBase(node)}/jobs/${encodeURIComponent(id)}`),
    // fullLog reads one stream to its end in chunks and decodes it as UTF-8.
    fullLog: async (node, id, stream) => {
      const parts = [];
      let offset = 0; let total = 0;
      for (;;) {
        const chunk = await call('GET', `${runBase(node)}/jobs/${encodeURIComponent(id)}/logs?stream=${stream}&offset=${offset}`);
        const bytes = decodeBase64(chunk?.data);
        parts.push(bytes); total += bytes.length;
        if (chunk?.eof || !bytes.length || chunk?.next_offset <= offset || total >= LOG_MAX_BYTES) break;
        offset = chunk.next_offset;
      }
      const all = new Uint8Array(total);
      let at = 0;
      for (const p of parts) { all.set(p, at); at += p.length; }
      return new TextDecoder().decode(all);
    },
    settings: () => call('GET', '/api/node/run/settings'),
    saveSettings: (body) => call('PUT', '/api/node/run/settings', body),
  });
}
