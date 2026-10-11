import { FleetAdminError } from './fleet-admin-actions.js';
import { nodeBase } from './fleet-harness-model.js';

// createHarnessActions wraps a node's harness routes. Every call names the
// node; a peer's go through the local peer proxy, which enforces its grants
// and, for credential pushes, captures this operator's own login files.
export function createHarnessActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(node, method, path, body) {
    const init = { method, credentials: 'same-origin', cache: 'no-store', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetchImpl(`${nodeBase(node)}/harnesses/${path}`, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) throw new FleetAdminError(res.status, data);
    return data;
  }
  return Object.freeze({
    availability: (node, { refresh = false } = {}) => call(node, 'GET', `availability${refresh ? '?refresh=1' : ''}`),
    operations: (node) => call(node, 'GET', 'operations'),
    // req: {action: install|update, harness?|all, mode?, copy_credentials?, overwrite_credentials?}
    start: (node, req) => call(node, 'POST', 'operations', req),
    job: (node, id) => call(node, 'GET', `operations/jobs/${encodeURIComponent(id)}`),
    backups: async (node, harness) => (await call(node, 'GET', `credentials/backups?harness=${encodeURIComponent(harness)}`))?.backups || [],
    backup: (node, harness) => call(node, 'POST', 'credentials/backup', { harness }),
    restore: (node, harness, backup = '') => call(node, 'POST', 'credentials/restore', backup ? { harness, backup } : { harness }),
    // Only a peer can receive a push; confirm_share records the operator's
    // consent that agents there act as them.
    push: (node, harness) => call(node, 'POST', 'credentials/push', { harness, confirm_share: true }),
  });
}
