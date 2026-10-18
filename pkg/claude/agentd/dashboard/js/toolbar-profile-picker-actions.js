import { loadProfiles, profileChoices, setDashDefaultProfile } from './profiles.js';
import { loadSandboxProfiles, openSandboxProfileEditor } from './sandbox-profiles.js';
import { openProfileEditor } from './modal-profiles.js';
import { renderDashDefaultProfile, renderDashSandboxProfile } from './toolbar-profile-renderers.js';
import { refreshAgentSpawnSandboxPolicy } from './agent-spawn-controller.js';
import { announceNodeChanged, nodeFetch } from './node-api.js';

// Assignment failures carry the daemon's structured {"error", "code"} body.
// Keep the typed code on the thrown Error so callers can key off it rather
// than pattern-matching message text.
async function assignmentError(response) {
  const raw = await response.text();
  let body = null;
  try { body = JSON.parse(raw); } catch (_) { body = null; }
  const error = new Error(body?.message || body?.error || raw || `HTTP ${response.status}`);
  error.status = response.status;
  if (body?.code) error.code = body.code;
  return error;
}

const KIND_API = Object.freeze({
  profile: { list: '/api/spawn-profiles', default: '/api/spawn-profile-default', label: 'global default spawn profile' },
  sandbox: { list: '/api/sandbox-profiles', default: '/api/sandbox-profile-default', label: 'global sandbox profile' },
});

function choices(items) {
  return profileChoices(items).map(({ value, label }) => Object.freeze({ value, label }));
}

export function createToolbarProfilePickerActions({
  fetchImpl = fetch,
  notify = () => {},
  refresh = () => {},
} = {}) {
  const pending = new Set();
  // node is another node (the toolbar following a fused Groups view's node,
  // toolbar-node.js): its catalog and defaults through nodeFetch, and that
  // node polled again after a change. Blank is the page's own node.
  async function loadNode(kind, node) {
    const response = await nodeFetch(node, { fetchImpl })(KIND_API[kind].list, { credentials: 'same-origin' });
    if (!response.ok) throw await assignmentError(response);
    const items = await response.json();
    return Object.freeze({ choices: Object.freeze(choices(Array.isArray(items) ? items : [])) });
  }

  async function commitNode(kind, name, node, nodeName) {
    const api = KIND_API[kind];
    const response = await nodeFetch(node, { fetchImpl })(api.default, {
      method: name ? 'PUT' : 'DELETE',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: name ? JSON.stringify({ name }) : undefined,
    });
    if (!response.ok) throw await assignmentError(response);
    const canonical = (await response.json().catch(() => ({})))?.name || name;
    notify(name ? `${nodeName || node}: ${api.label} → ${canonical}` : `${nodeName || node}: ${api.label} cleared`);
    announceNodeChanged(node);
    return true;
  }

  const actions = {
    async load(kind, node = '') {
      if (node) return loadNode(kind === 'sandbox' ? 'sandbox' : 'profile', node);
      const items = kind === 'sandbox' ? await loadSandboxProfiles() : await loadProfiles();
      return Object.freeze({ choices: Object.freeze(choices(items)) });
    },

    async commit(kind, name, node = '', nodeName = '') {
      if (pending.has(kind)) return false;
      pending.add(kind);
      try {
        if (node) return await commitNode(kind === 'sandbox' ? 'sandbox' : 'profile', name, node, nodeName);
        if (kind === 'profile') {
          const canonical = await setDashDefaultProfile(name);
          notify(canonical ? `global default spawn profile → ${canonical}` : 'global default spawn profile cleared');
          // Begin and await a newer dashboard request before repainting. Its
          // request generation invalidates any poll that captured the prior
          // server-backed default before this mutation completed.
          await refresh();
          renderDashDefaultProfile();
          return true;
        }
        const response = await fetchImpl('/api/sandbox-profile-default', {
          method: name ? 'PUT' : 'DELETE',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: name ? JSON.stringify({ name }) : undefined,
        });
        if (!response.ok) throw await assignmentError(response);
        notify(name ? `global sandbox profile: ${name}` : 'global sandbox profile cleared');
        await refresh();
        renderDashSandboxProfile();
        refreshAgentSpawnSandboxPolicy();
        return true;
      } finally {
        pending.delete(kind);
      }
    },

    openNew(kind, onSaved) {
      if (kind === 'sandbox') {
        openSandboxProfileEditor(null, { onCreate: onSaved });
      } else {
        openProfileEditor(null, { onSaved });
      }
    },

    async commitFromEditor(kind, name) {
      try {
        return await actions.commit(kind, name);
      } catch (cause) {
        notify(`set ${kind === 'sandbox' ? 'global sandbox profile' : 'global default spawn profile'} failed: ${cause?.message || cause}`, true);
        return false;
      }
    },
  };
  return Object.freeze(actions);
}
