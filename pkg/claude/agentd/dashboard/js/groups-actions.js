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
import { openGroupAttachmentDialog } from './action-dialog-controller.js';
import { openStandingOrderCreateModal } from './jobs-controller.js';

// Claude Code applies /rename in-pane, then the conversation monitor indexes
// its JSONL write after a 500ms quiet window. The mutation response therefore
// confirms delivery before an immediate snapshot is guaranteed to carry the
// new title. One retry just beyond that window closes the ordinary race without
// turning a rare rename into a standing poll loop. Direct-store harnesses such
// as Codex normally match on the first refresh and never wait.
const RENAME_REFRESH_RETRY_MS = 600;

async function responseError(response, fallback) {
  return (await response.text()) || fallback || `HTTP ${response.status}`;
}

function snapshotHasAgentTitle(snapshot, selector, title) {
  const matches = (row) => row &&
    (row.agent_id === selector || row.conv_id === selector) &&
    row.title === title;
  for (const group of snapshot?.groups || []) {
    if ((group.members || []).some(matches)) return true;
  }
  return ['ungrouped', 'retired', 'conversations', 'replaced', 'agents']
    .some((key) => (snapshot?.[key] || []).some(matches));
}

async function refreshAfterAgentRename({ state, refresh, selector, title, wait }) {
  await refresh();
  if (snapshotHasAgentTitle(state.snapshot?.value, selector, title)) return;
  await wait(RENAME_REFRESH_RETRY_MS);
  await refresh();
}

// Group sandbox-assignment failures carry the daemon's structured
// {"error", "code"} body. Keep the typed code on the thrown Error so callers
// can key off it rather than pattern-matching message text.
async function sandboxAssignmentError(response) {
  const raw = await response.text();
  let body = null;
  try { body = JSON.parse(raw); } catch (_) { body = null; }
  const message = body?.message || body?.error || raw || `HTTP ${response.status}`;
  const error = new Error(`set sandbox profile failed: ${message}`);
  error.status = response.status;
  if (body?.code) error.code = body.code;
  return error;
}

export function createGroupsActions({
  state, refresh, notify = () => {}, fetchImpl = fetch,
  openMemberPermissions = () => { throw new Error('permissions editor is not ready'); },
  wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
}) {
  if (!state) throw new TypeError('groups actions require state');
  if (typeof refresh !== 'function') throw new TypeError('groups actions require refresh');
  if (typeof wait !== 'function') throw new TypeError('groups actions require wait');

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
    openStandingOrders(group) {
      return state.openStandingOrders(group);
    },
    closeStandingOrders() {
      state.closeStandingOrders();
    },
    openStandingOrderCreate(group) {
      const name = group?.name || group || '';
      const opened = openStandingOrderCreateModal({
        targetMode: 'group', groupName: name, scopeGroup: name,
      }, {
        onCancel: () => state.openStandingOrders({ name }),
      });
      if (opened) state.closeStandingOrders();
      return opened;
    },
    async loadStandingOrders(group) {
      const response = await fetchImpl(
        `/api/groups/${encodeURIComponent(group)}/standing-orders`,
        { credentials: 'same-origin' },
      );
      if (!response.ok) {
        throw new Error(`load standing orders failed: ${await responseError(response)}`);
      }
      return response.json();
    },
    async loadTriggers() {
      const response = await fetchImpl('/api/triggers', { credentials: 'same-origin' });
      if (!response.ok) {
        throw new Error(`load triggers failed: ${await responseError(response)}`);
      }
      const body = await response.json();
      const rules = Array.isArray(body?.triggers) ? body.triggers : [];
      return Promise.all(rules.map(async (rule) => {
        const history = await fetchImpl(
          `/api/triggers/${encodeURIComponent(rule.id)}/firings?limit=1`,
          { credentials: 'same-origin' },
        );
        if (!history.ok) throw new Error(`load trigger state failed: ${await responseError(history)}`);
        const ledger = await history.json();
        return { ...rule, firings: Array.isArray(ledger?.firings) ? ledger.firings : [] };
      }));
    },
    async setStandingOrderScope(group, row, assigned) {
      const order = row?.order || {};
      const response = await fetchImpl(
        `/api/groups/${encodeURIComponent(group)}/standing-orders/${encodeURIComponent(order.id)}?row_version=${encodeURIComponent(order.row_version)}`,
        {
          method: assigned ? 'POST' : 'DELETE',
          credentials: 'same-origin',
        },
      );
      if (!response.ok) {
        throw new Error(`set standing-order scope failed: ${await responseError(response)}`);
      }
      const updated = await response.json();
      notify(`${order.name}: ${assigned ? 'enabled for' : 'removed from'} ${group}`);
      return updated;
    },
    openSpawnHarnessPolicy(group) {
      return openSpawnHarnessPolicy(group?.name || group || '');
    },
    openGroupAttachment(group) {
      return openGroupAttachmentDialog({
        group: group.name,
        url: group.attachment_url || '',
        attachmentLabel: group.attachment_label_override || '',
      });
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
      const response = await fetchImpl(`/api/agents/${encodeURIComponent(selector)}/rename`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title }),
      });
      if (!response.ok) throw new Error(`rename failed: ${await responseError(response)}`);
      notify(`renaming ${member.title || member.conv_id} → ${title}`);
      await refreshAfterAgentRename({ state, refresh, selector, title, wait });
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
        const response = await fetch(`/api/groups/${encodeURIComponent(group.name)}/sandbox-profile`, {
          method: name ? 'PUT' : 'DELETE', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: name ? JSON.stringify({ name }) : undefined,
        });
        if (!response.ok) throw await sandboxAssignmentError(response);
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
