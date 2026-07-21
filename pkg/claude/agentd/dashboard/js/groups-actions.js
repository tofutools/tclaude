import { cycleSort } from './sort.js';
import {
  listPagerNav,
  setListPageSize,
} from './list-paging.js';
import { dashPrefs } from './prefs.js';
import { loadProfiles, profileChoices } from './profiles.js';
import { openProfileEditor } from './modal-profiles.js';
import { loadSandboxProfiles, openSandboxProfileEditor } from './sandbox-profiles.js';
import { refreshAgentSpawnSandboxPolicy } from './agent-spawn-controller.js';
import { pickDirectory } from './helpers.js';
import { saveMemberEditorRequests } from './member-editor-actions.js';
import {
  addExistingMemberRequest,
  loadAddMemberPromotionPool,
} from './add-member-actions.js';
import { openSpawnHarnessPolicy } from './spawn-harness-policy-controller.js';
import { shellConfirm } from './shell-state.js';
import { assignedBreakGlass, breakGlassAssignmentPrompt } from './sandbox-break-glass.js';

async function responseError(response, fallback) {
  return (await response.text()) || fallback || `HTTP ${response.status}`;
}

export function createGroupsActions({
  state, refresh, notify = () => {}, fetchImpl = fetch,
  confirmDanger = shellConfirm,
  openMemberPermissions = () => { throw new Error('permissions editor is not ready'); },
}) {
  if (!state) throw new TypeError('groups actions require state');
  if (typeof refresh !== 'function') throw new TypeError('groups actions require refresh');

  return Object.freeze({
    refresh,
    reportError(error) {
      notify((error && error.message) || String(error), true);
    },
    openMemberEditor(member, group, focus = 'title') {
      return state.openMemberEditor(member, group, focus);
    },
    closeMemberEditor() {
      state.closeMemberEditor();
    },
    openAddMember(group) {
      return state.openAddMember(group);
    },
    openSpawnHarnessPolicy(group) {
      return openSpawnHarnessPolicy(group?.name || group || '');
    },
    closeAddMember() {
      state.closeAddMember();
    },
    loadAddMemberPromotionPool() {
      return loadAddMemberPromotionPool({ fetchImpl });
    },
    async addExistingMember(descriptor, candidate) {
      await addExistingMemberRequest({
        group: descriptor.group,
        candidate,
        fetchImpl,
      });
      state.optimisticAddMember(descriptor.group, candidate);
      notify(`added ${candidate.title || candidate.conv_id} to ${descriptor.group}`);
      return true;
    },
    openMemberPermissions(descriptor) {
      return openMemberPermissions(descriptor.conv, descriptor.label);
    },
    noMemberChanges() {
      notify('no changes');
    },
    async saveMemberEditor(descriptor, changes) {
      return saveMemberEditorRequests({ descriptor, changes, fetchImpl, notify, refresh });
    },
    sort(table, column) {
      cycleSort(table, column);
      state.rerender();
    },
    page(kind, action, total) {
      if (!listPagerNav(kind, action, total)) return false;
      void refresh();
      return true;
    },
    setPageSize(kind, value) {
      setListPageSize(kind, Number(value) || 50);
      void refresh();
    },
    toggleQuickPin(group) {
      const key = `tclaude.dash.quickpin.${group.name}`;
      if (dashPrefs.getItem(key) === '1') dashPrefs.removeItem(key);
      else dashPrefs.setItem(key, '1');
      state.rerender();
    },
    toggleForceFold(group) {
      const key = `tclaude.dash.forcefold.${group.name}`;
      if (dashPrefs.getItem(key) === '1') dashPrefs.removeItem(key);
      else dashPrefs.setItem(key, '1');
      state.rerender();
    },
    async renameAgent(member, rawTitle) {
      const oldTitle = member.title || '';
      const title = String(rawTitle || '').trim();
      if (!title || title === oldTitle) return false;
      const selector = member.agent_id || member.conv_id;
      const response = await fetch(`/api/agents/${encodeURIComponent(selector)}/rename`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title }),
      });
      if (!response.ok) throw new Error(`rename failed: ${await responseError(response)}`);
      notify(`renaming ${member.title || member.conv_id} → ${title}`);
      void refresh();
      return true;
    },
    async renameGroup(group, rawName) {
      const oldName = group.name;
      const newName = String(rawName || '').trim();
      if (!newName || newName === oldName) return false;
      const response = await fetch(`/api/groups/${encodeURIComponent(oldName)}/rename`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ new_name: newName }),
      });
      if (!response.ok) throw new Error(`rename failed: ${await responseError(response)}`);
      const disclosure = dashPrefs.getItem(`tclaude.dash.group.${oldName}`);
      dashPrefs.removeItem(`tclaude.dash.group.${oldName}`);
      if (disclosure !== null) dashPrefs.setItem(`tclaude.dash.group.${newName}`, disclosure);
      notify(`renamed: ${oldName} → ${newName}`);
      void refresh();
      return true;
    },
    async patchGroup(group, field, value, message) {
      const response = await fetch(`/api/groups/${encodeURIComponent(group.name)}`, {
        method: 'PATCH', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ [field]: value }),
      });
      if (!response.ok) throw new Error(await responseError(response, `could not update ${field}`));
      if (message) notify(message(value));
      void refresh();
      return true;
    },
    async pickGroupDirectory(group) {
      const result = await pickDirectory({
        startDir: group.default_cwd || '',
        title: `Default spawn directory for "${group.name}"`,
      });
      if (result.canceled) return false;
      if (result.error) throw new Error(`pick dir failed: ${result.error}`);
      return this.patchGroup(group, 'default_cwd', result.path,
        (value) => `${group.name}: default dir → ${value}`);
    },
    async groupProfileChoices(kind) {
      if (kind === 'sandbox') {
        const profiles = await loadSandboxProfiles();
        return profiles.map((profile) => ({ value: profile.name, label: profile.name }));
      }
      return profileChoices(await loadProfiles());
    },
    openNewGroupProfile(kind, onSaved) {
      if (kind === 'sandbox') {
        openSandboxProfileEditor(null, { onCreate: onSaved });
      } else {
        openProfileEditor(null, { onSaved });
      }
    },
    async setGroupProfile(group, kind, name) {
      if (kind === 'sandbox') {
        // Group assignment is a persistent break-glass surface: every future
        // launch into the group inherits it, so it needs the prominent
        // warning and the explicit acknowledgement.
        let breakGlassAcknowledged = false;
        if (name) {
          const entries = assignedBreakGlass(name, await loadSandboxProfiles(), `group:${group.name}`);
          if (entries.length) {
            const proceed = await confirmDanger(breakGlassAssignmentPrompt({
              scopeLabel: `This assigns the default sandbox profile for group "${group.name}": every agent launched into the group inherits it until the assignment is removed.`,
              name, entries,
            }));
            if (!proceed) return false;
            breakGlassAcknowledged = true;
          }
        }
        const response = await fetch(`/api/groups/${encodeURIComponent(group.name)}/sandbox-profile`, {
          method: name ? 'PUT' : 'DELETE', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: name ? JSON.stringify({ name, ...(breakGlassAcknowledged ? { break_glass_acknowledged: true } : {}) }) : undefined,
        });
        if (!response.ok) throw new Error(`set sandbox profile failed: ${await responseError(response)}`);
        notify(name ? `${group.name} sandbox profile: ${name}` : `${group.name} sandbox profile cleared`);
        refreshAgentSpawnSandboxPolicy();
      } else {
        const response = await fetch(`/api/groups/${encodeURIComponent(group.name)}`, {
          method: 'PATCH', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ default_profile: name }),
        });
        if (!response.ok) throw new Error(`set default profile failed: ${await responseError(response)}`);
        notify(name ? `${group.name}: default profile → ${name}` : `${group.name}: default profile cleared`);
      }
      void refresh();
      return true;
    },
  });
}
