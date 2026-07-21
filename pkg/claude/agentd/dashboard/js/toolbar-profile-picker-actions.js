import { loadProfiles, profileChoices, setDashDefaultProfile } from './profiles.js';
import { loadSandboxProfiles, openSandboxProfileEditor } from './sandbox-profiles.js';
import { openProfileEditor } from './modal-profiles.js';
import { renderDashDefaultProfile, renderDashSandboxProfile } from './toolbar-profile-renderers.js';
import { refreshAgentSpawnSandboxPolicy } from './agent-spawn-controller.js';
import { shellConfirm } from './shell-state.js';
import { assignedBreakGlass, breakGlassAssignmentPrompt } from './sandbox-break-glass.js';

function choices(items) {
  return profileChoices(items).map(({ value, label }) => Object.freeze({ value, label }));
}

export function createToolbarProfilePickerActions({
  fetchImpl = fetch,
  notify = () => {},
  refresh = () => {},
  confirmDanger = shellConfirm,
  loadSandboxProfilesImpl = loadSandboxProfiles,
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
        // A global assignment is the most persistent break-glass surface:
        // every future launch inherits it until the assignment is removed, so
        // it needs the prominent warning and the explicit acknowledgement.
        let breakGlassAcknowledged = false;
        if (name) {
          const entries = assignedBreakGlass(name, await loadSandboxProfilesImpl(), 'global');
          if (entries.length) {
            const proceed = await confirmDanger(breakGlassAssignmentPrompt({
              scopeLabel: 'This assigns the GLOBAL default sandbox profile: every newly launched agent inherits it until the assignment is removed.',
              name, entries,
            }));
            if (!proceed) return false;
            breakGlassAcknowledged = true;
          }
        }
        const response = await fetchImpl('/api/sandbox-profile-default', {
          method: name ? 'PUT' : 'DELETE',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: name ? JSON.stringify({ name, ...(breakGlassAcknowledged ? { break_glass_acknowledged: true } : {}) }) : undefined,
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
