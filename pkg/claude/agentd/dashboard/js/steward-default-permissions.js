import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { readJSON } from './message-access-dialog-actions.js';
import { dashboardState } from './snapshot-store.js';

const html = htm.bind(h);
const ROUTE = '/api/steward/default-permissions';

// stewardPermissionSummary says what is stored, in the terms of the levels.
export function stewardPermissionSummary(access) {
  const levels = new Set(access?.config || []);
  const effects = Object.entries(access?.carried || {});
  const added = effects.filter(([slug, effect]) => effect === 'grant' && !levels.has(slug)).length;
  const denied = effects.filter(([, effect]) => effect === 'deny').length;
  const parts = [added && `${added} added`, denied && `${denied} denied`].filter(Boolean);
  return parts.length ? parts.join(', ') : 'Reads and Writes only';
}

// StewardDefaultPermissions is the Config row for what is stored for future
// Stewards. It shows a count and hands the editing to the common permission
// editor, which saves straight away: this is not part of the config form. The
// daemon sends steward_access only with Collab on, and without it the routes
// behind this are closed.
export function StewardDefaultPermissions({ fetchImpl = (...args) => globalThis.fetch(...args), openEditor = null }) {
  const [error, setError] = useState('');
  const access = dashboardState.snapshot.value?.steward_access;
  const open = async () => {
    setError('');
    try {
      const { stored, levels } = await readJSON(await fetchImpl(ROUTE, { credentials: 'same-origin' }));
      const editor = openEditor || (await import('./message-access-dialog-controller.js')).openSpawnPermEditor;
      editor({
        stewardLevels: levels, overrides: stored,
        onSave: async (overrides) => {
          await readJSON(await fetchImpl(ROUTE, {
            method: 'PUT', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ overrides }),
          }));
        },
      });
    } catch (cause) { setError(`Could not open the editor: ${cause.message}`); }
  };
  if (!access) return html`<span class="cfg-hint" id="cfg-steward-permissions-summary">Needs Collab</span>`;
  return html`<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;justify-content:flex-end">
    <span class="cfg-hint" id="cfg-steward-permissions-summary">${stewardPermissionSummary(access)}</span>
    <button type="button" class="cfg-inline" id="cfg-steward-permissions-edit" onClick=${open}>Edit…</button>
    ${error && html`<span class="cfg-hint" role="alert">${error}</span>`}
  </div>`;
}
