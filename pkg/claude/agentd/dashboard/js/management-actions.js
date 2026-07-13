import {
  loadProfiles, createProfile, updateProfile, deleteProfile, exportProfiles, inspectProfileImport, importProfiles,
} from './profiles.js';
import { loadRoles, createRole, updateRole, deleteRole } from './roles.js';
import { loadSandboxProfiles, previewSandboxProfile, saveSandboxProfile, deleteSandboxProfile, exportSandboxProfiles, inspectSandboxImport, importSandboxProfiles, inspectSandboxDirectories, createSandboxDirectories } from './sandbox-profiles-data.js';

function downloadJSON(name, value) {
  const blob = new Blob([JSON.stringify(value, null, 2) + '\n'], { type: 'application/json' });
  const url = URL.createObjectURL(blob); const anchor = document.createElement('a');
  anchor.href = url; anchor.download = name; document.body.appendChild(anchor); anchor.click(); anchor.remove(); URL.revokeObjectURL(url);
}

export function createManagementActions({ state, confirm, notify, download = downloadJSON, getSnapshot = () => null, refreshSandboxSpawn = async () => {}, summonSandboxScribe = async () => {}, profileAPI = {}, roleAPI = {}, sandboxAPI = {} }) {
  const profiles = { loadProfiles, createProfile, updateProfile, deleteProfile, exportProfiles, inspectProfileImport, importProfiles, ...profileAPI };
  const roles = { loadRoles, createRole, updateRole, deleteRole, ...roleAPI };
  const sandbox = { loadSandboxProfiles, previewSandboxProfile, saveSandboxProfile, deleteSandboxProfile, exportSandboxProfiles, inspectSandboxImport, importSandboxProfiles, inspectSandboxDirectories, createSandboxDirectories, ...sandboxAPI };

  async function load(kind) {
    const lifecycle = kind === 'profiles' ? state.profilesRequest : kind === 'roles' ? state.rolesRequest : state.sandboxRequest;
    const token = lifecycle.beginRequest();
    try {
      const data = kind === 'profiles' ? await profiles.loadProfiles(true) : kind === 'roles' ? await roles.loadRoles(true) : await sandbox.loadSandboxProfiles();
      return lifecycle.commitRequest(token, data);
    } catch (error) {
      lifecycle.failRequest(token, error); return false;
    }
  }

  async function openManager(kind) { state.openManager(kind); await load(kind); }
  function openProfileEditor(seed = null, options = {}) { state.openDialog({ kind: 'profile-editor', seed, options, catalog: getSnapshot()?.harnesses || [] }); }
  function openRoleEditor(seed = null) { void load('profiles'); state.openDialog({ kind: 'role-editor', seed, catalog: getSnapshot()?.harnesses || [], slugs: getSnapshot()?.slugs || [] }); }
  function openSandboxEditor(seed = null, options = {}) { state.openDialog({ kind: 'sandbox-editor', seed, options }); if (options.notice) state.error.value = options.notice; }

  async function saveProfile({ draft, original, options, payload }) {
    state.busy.value = 'profile-save'; state.error.value = '';
    try {
      if (options?.local) {
        options.local.onSave?.(payload); options.onSaved?.(draft.name.trim());
      } else if (original && options?.editExisting !== false) {
        await profiles.updateProfile(original.name, payload); notify(`profile updated: ${draft.name.trim()}`);
      } else {
        await profiles.createProfile(payload); notify(`profile created: ${draft.name.trim()}`);
      }
      const saved = draft.name.trim(); state.closeDialog();
      if (!options?.local) await load('profiles');
      options?.onSaved?.(saved); return true;
    } catch (error) { state.error.value = error.message || String(error); return false; }
    finally { state.busy.value = ''; }
  }

  async function saveRole({ draft, original, payload }) {
    state.busy.value = 'role-save'; state.error.value = '';
    try {
      if (original) await roles.updateRole(original.name, payload); else await roles.createRole(payload);
      notify(original ? `role updated: ${draft.name.trim()}` : `role created: ${draft.name.trim()}`);
      state.closeDialog(); await load('roles'); return true;
    } catch (error) { state.error.value = error.message || String(error); return false; }
    finally { state.busy.value = ''; }
  }

  async function removeProfile(name) {
    if (!(await confirm({ title: 'Delete profile?', body: `Delete the spawn profile "${name}"? Any group or the dashboard that names it as a default will fall back to blank spawn fields until re-pointed. Agents already spawned are untouched.`, meta: name, okLabel: 'Delete profile' }))) return false;
    try { await profiles.deleteProfile(name); notify(`profile deleted: ${name}`); await load('profiles'); return true; }
    catch (error) { notify(error.message || String(error), true); return false; }
  }

  async function removeRole(name) {
    if (!(await confirm({ title: 'Delete role?', body: `Delete the role "${name}"? Roles resolve at deploy time, so this is refused while any template still references it.`, meta: name, okLabel: 'Delete role' }))) return false;
    try { await roles.deleteRole(name); notify(`role deleted: ${name}`); await load('roles'); return true; }
    catch (error) { notify(error.message || String(error), true); return false; }
  }

  async function saveSandbox({ draft, original, options = {} }) {
    state.busy.value = 'sandbox-save'; state.error.value = '';
    try {
      const body = { name: draft.name.trim(), filesystem: draft.filesystem, environment: draft.environment, includes: draft.includes, agent_directories: draft.agent_directories };
      if (!body.name) throw new Error('name is required');
      const targetName = options.targetName || original?.name || '';
      const preview = await sandbox.previewSandboxProfile(targetName, body);
      if (preview.before && JSON.stringify(preview.before) === JSON.stringify(preview.after)) { notify('No sandbox profile changes to save'); return false; }
      const ok = await state.confirmSandboxDiff(preview.before || null, preview.after);
      if (!ok) { notify('Sandbox profile save cancelled'); return false; }
      await sandbox.saveSandboxProfile(targetName, preview.after, preview.revision || '');
      state.closeDialog(); notify(`sandbox profile saved: ${preview.after.name}`); await load('sandbox'); await refreshSandboxSpawn(); await options.onCreate?.(preview.after.name); return true;
    } catch (error) { state.error.value = error.message || String(error); return false; }
    finally { state.busy.value = ''; }
  }

  async function removeSandbox(name) {
    if (!(await confirm({ title: 'Delete sandbox profile?', body: `Delete “${name}”? Global and group assignments to it are cleared. Running agents keep their frozen launch snapshot.`, meta: name, okLabel: 'Delete sandbox profile' }))) return false;
    try { await sandbox.deleteSandboxProfile(name); notify(`sandbox profile deleted: ${name}`); await load('sandbox'); await refreshSandboxSpawn(); return true; }
    catch (error) { notify(error.message || String(error), true); return false; }
  }

  async function inspectProfiles(envelope) { return profiles.inspectProfileImport(envelope); }
  async function importProfileBundle(envelope, decisions) { const result = await profiles.importProfiles(envelope, decisions); await load('profiles'); const imported = result?.imported || []; const skipped = result?.skipped || []; notify(`${imported.length} profile${imported.length === 1 ? '' : 's'} imported${skipped.length ? `, ${skipped.length} skipped` : ''}`); return result; }
  async function exportProfileBundle(names) { const bundle = await profiles.exportProfiles(names); download('spawn-profiles.json', bundle); notify(`${names.length} profile${names.length === 1 ? '' : 's'} exported`); return bundle; }
  async function exportSandboxBundle(names) { const bundle = await sandbox.exportSandboxProfiles(names); download('sandbox-profiles.json', bundle); notify(`${names.length} sandbox profile${names.length === 1 ? '' : 's'} exported`); return bundle; }
  async function inspectSandboxBundle(envelope) { return sandbox.inspectSandboxImport(envelope); }
  async function importSandboxBundle(envelope, conflict) { const result = await sandbox.importSandboxProfiles(envelope, conflict); await load('sandbox'); await refreshSandboxSpawn(); const imported = result?.imported || []; const skipped = result?.skipped || []; notify(`${imported.length} sandbox profile${imported.length === 1 ? '' : 's'} imported${skipped.length ? `, ${skipped.length} skipped` : ''}`); return result; }
  async function configureSandboxWithAgent(seed, options = {}) { await summonSandboxScribe(seed, options.targetName || seed?.name || '', options.onCreate || null); }
  function inspectDirectories(filesystem) { return sandbox.inspectSandboxDirectories({ name: 'directory-preview', filesystem, environment: [] }); }
  function createDirectories(filesystem) { return sandbox.createSandboxDirectories({ name: 'directory-preview', filesystem, environment: [] }); }

  return Object.freeze({ load, openManager, openProfileEditor, openRoleEditor, openSandboxEditor, saveProfile, saveRole, saveSandbox, removeProfile, removeRole, removeSandbox, inspectProfiles, importProfileBundle, exportProfileBundle, exportSandboxBundle, inspectSandboxBundle, importSandboxBundle, configureSandboxWithAgent, inspectDirectories, createDirectories });
}
