import { loadProfiles, profileChoices, setDashDefaultProfile } from './profiles.js';
import { loadSandboxProfiles, openSandboxProfileEditor } from './sandbox-profiles.js';
import { openProfileEditor } from './modal-profiles.js';
import { renderDashDefaultProfile, renderDashSandboxProfile } from './toolbar-profile-renderers.js';
import { refreshAgentSpawnSandboxPolicy } from './agent-spawn-controller.js';

function choices(items) {
  return profileChoices(items).map(({ value, label }) => Object.freeze({ value, label }));
}

export function createToolbarProfilePickerActions({
  fetchImpl = fetch,
  notify = () => {},
  refresh = () => {},
} = {}) {
  const pending = new Set();
  const actions = {
    async load(kind) {
      const items = kind === 'sandbox' ? await loadSandboxProfiles() : await loadProfiles();
      return Object.freeze({ choices: Object.freeze(choices(items)) });
    },

    async commit(kind, name) {
      if (pending.has(kind)) return false;
      pending.add(kind);
      try {
        if (kind === 'profile') {
          const canonical = await setDashDefaultProfile(name);
          notify(canonical ? `dashboard default profile → ${canonical}` : 'dashboard default profile cleared');
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
        if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
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
        notify(`set ${kind === 'sandbox' ? 'global sandbox' : 'dashboard default'} profile failed: ${cause?.message || cause}`, true);
        return false;
      }
    },
  };
  return Object.freeze(actions);
}
