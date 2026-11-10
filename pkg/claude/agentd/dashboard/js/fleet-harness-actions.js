import { FleetAdminError } from './fleet-admin-actions.js';
import { nodeBase } from './fleet-harness-model.js';
import { requestJSON } from './request-json.js';

// createHarnessActions wraps a node's harness routes. Every call names the
// node; a peer's go through the local peer proxy, which enforces its grants
// and, for credential pushes, captures this operator's own login files.
export function createHarnessActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(node, method, path, body) { return callAt(node, method, `harnesses/${path}`, body); }
  function callAt(node, method, path, body) {
    return requestJSON(`${nodeBase(node)}/${path}`, { method, body, cache: 'no-store', fetchImpl, ErrorClass: FleetAdminError });
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
    // The node's own update scripts (config updates.scripts), under node.update.
    updateScripts: (node) => callAt(node, 'GET', 'node/update-scripts'),
    checkUpdateScripts: (node, name = '') => callAt(node, 'POST', 'node/update-scripts', name ? { action: 'check', name } : { action: 'check' }),
    runUpdateScript: (node, name) => callAt(node, 'POST', 'node/update-scripts', { action: 'update', name }),
  });
}
