import {
  loadProfiles,
  createProfile,
  updateProfile,
  deleteProfile,
  exportProfiles,
  inspectProfileImport,
  importProfiles,
} from './profiles.js';
import { loadRoles, createRole, updateRole, deleteRole } from './roles.js';
import { BREAK_GLASS_ACK_CODE } from './sandbox-break-glass.js';
import {
  loadSandboxProfiles,
  loadSandboxReadExclusionCatalog,
  previewSandboxProfile,
  saveSandboxProfile,
  deleteSandboxProfile,
  exportSandboxProfiles,
  inspectSandboxImport,
  importSandboxProfiles,
  inspectSandboxDirectories,
  createSandboxDirectories,
} from './sandbox-profiles-data.js';

const MAX_SANDBOX_PROFILE_NAME_BYTES = 200;

function utf8Length(value) {
  return new TextEncoder().encode(value).length;
}

function sandboxCloneName(sourceName, existingNames) {
  const names = new Set(existingNames);
  for (let suffix = 1; ; suffix += 1) {
    const tail = suffix === 1 ? '-copy' : `-copy-${suffix}`;
    const budget = MAX_SANDBOX_PROFILE_NAME_BYTES - utf8Length(tail);
    let prefix = '';
    for (const char of sourceName) {
      if (utf8Length(prefix + char) > budget) break;
      prefix += char;
    }
    const candidate = prefix + tail;
    if (!names.has(candidate)) return candidate;
  }
}

function downloadJSON(name, value) {
  const blob = new Blob([JSON.stringify(value, null, 2) + '\n'], {
    type: 'application/json',
  });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = name;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

export function createManagementActions({
  state,
  confirm,
  notify,
  download = downloadJSON,
  getSnapshot = () => null,
  refresh = async () => {},
  onGroupImported = () => {},
  onGroupDeployed = () => {},
  refreshSandboxSpawn = async () => {},
  summonSandboxScribe = async () => {},
  summonTemplateScribe = async () => {},
  profileAPI = {},
  roleAPI = {},
  sandboxAPI = {},
  templateAPI = {},
  groupAPI = {},
}) {
  let starterRequest = 0;
  const profiles = {
    loadProfiles,
    createProfile,
    updateProfile,
    deleteProfile,
    exportProfiles,
    inspectProfileImport,
    importProfiles,
    ...profileAPI,
  };
  const roles = { loadRoles, createRole, updateRole, deleteRole, ...roleAPI };
  const sandbox = {
    loadSandboxProfiles,
    loadSandboxReadExclusionCatalog,
    previewSandboxProfile,
    saveSandboxProfile,
    deleteSandboxProfile,
    exportSandboxProfiles,
    inspectSandboxImport,
    importSandboxProfiles,
    inspectSandboxDirectories,
    createSandboxDirectories,
    ...sandboxAPI,
  };
  const templates = {
    async saveTemplate(originalName, payload) {
      const response = await fetch(
        originalName
          ? `/api/templates/${encodeURIComponent(originalName)}`
          : '/api/templates',
        {
          method: originalName ? 'PATCH' : 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        },
      );
      if (!response.ok)
        throw new Error((await response.text()) || `HTTP ${response.status}`);
      return response.json().catch(() => ({}));
    },
    async deleteTemplate(name) {
      const response = await fetch(
        `/api/templates/${encodeURIComponent(name)}`,
        { method: 'DELETE', credentials: 'same-origin' },
      );
      if (!response.ok && response.status !== 204)
        throw new Error((await response.text()) || `HTTP ${response.status}`);
    },
    async importTemplate(raw, { as = '', update = false } = {}) {
      const query = new URLSearchParams();
      if (as) query.set('as', as);
      if (update) query.set('update', 'true');
      const response = await fetch(
        `/api/templates/import${query.size ? `?${query}` : ''}`,
        {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: raw,
        },
      );
      const text = await response.text();
      if (!response.ok) {
        let detail = text;
        try {
          detail = JSON.parse(text)?.error || text;
        } catch (_) {}
        throw new Error(detail || `HTTP ${response.status}`);
      }
      try {
        return JSON.parse(text);
      } catch (_) {
        return {};
      }
    },
    async fromGroup(body) {
      const response = await fetch('/api/templates/from-group', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      const text = await response.text();
      if (!response.ok) throw new Error(text || `HTTP ${response.status}`);
      try {
        return JSON.parse(text);
      } catch (_) {
        return {};
      }
    },
    async loadStarters() {
      const response = await fetch('/api/starters', {
        credentials: 'same-origin',
      });
      const text = await response.text();
      if (!response.ok) {
        let detail = text;
        try {
          detail = JSON.parse(text)?.error || text;
        } catch (_) {}
        throw new Error(detail || `HTTP ${response.status}`);
      }
      try {
        return JSON.parse(text) || [];
      } catch (_) {
        return [];
      }
    },
    async installStarter(name) {
      const response = await fetch(
        `/api/starters/${encodeURIComponent(name)}/install`,
        { method: 'POST', credentials: 'same-origin' },
      );
      const text = await response.text();
      if (!response.ok) {
        let detail = text;
        try {
          detail = JSON.parse(text)?.error || text;
        } catch (_) {}
        throw new Error(detail || `HTTP ${response.status}`);
      }
      try {
        return JSON.parse(text);
      } catch (_) {
        return {};
      }
    },
    async deployTemplate(name, kind, payload) {
      const suffix =
        kind === 'reinforce'
          ? 'reinforce'
          : kind === 'instantiate'
            ? 'instantiate'
            : 'deploy';
      const response = await fetch(
        `/api/templates/${encodeURIComponent(name)}/${suffix}`,
        {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        },
      );
      const text = await response.text();
      if (!response.ok) throw new Error(text || `HTTP ${response.status}`);
      try {
        return JSON.parse(text);
      } catch (_) {
        return {};
      }
    },
    async loadWorktrees(repo) {
      const response = await fetch(
        `/api/worktrees?repo=${encodeURIComponent(repo)}`,
        { credentials: 'same-origin' },
      );
      return response.json().catch(() => ({}));
    },
    async createWorktree(body) {
      const response = await fetch('/api/worktrees', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!response.ok)
        throw new Error((await response.text()) || `HTTP ${response.status}`);
      return response.json();
    },
    ...templateAPI,
  };
  const groups = {
    async inspectImport(file, as = '') {
      const form = new FormData();
      form.append('archive', file);
      if (as) form.append('as', as);
      const response = await fetch('/api/groups/import/inspect', {
        method: 'POST',
        credentials: 'same-origin',
        body: form,
      });
      const body = await response.json().catch(() => null);
      if (!response.ok)
        throw new Error(body?.error || `HTTP ${response.status}`);
      return body;
    },
    async importGroup(file, into, as = '') {
      const form = new FormData();
      form.append('archive', file);
      form.append('into', into);
      if (as) form.append('as', as);
      const response = await fetch('/api/groups/import', {
        method: 'POST',
        credentials: 'same-origin',
        body: form,
      });
      const body = await response.json().catch(() => null);
      if (!response.ok)
        throw new Error(body?.error || `HTTP ${response.status}`);
      return body;
    },
    async saveContext(name, context) {
      const response = await fetch(`/api/groups/${encodeURIComponent(name)}`, {
        method: 'PATCH',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ default_context: context }),
      });
      if (!response.ok)
        throw new Error((await response.text()) || `HTTP ${response.status}`);
    },
    async cloneGroup(name, body) {
      const response = await fetch(
        `/api/groups/${encodeURIComponent(name)}/clone`,
        {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        },
      );
      if (!response.ok)
        throw new Error((await response.text()) || `HTTP ${response.status}`);
      return response.json().catch(() => ({}));
    },
    ...groupAPI,
  };

  async function load(kind) {
    const lifecycle =
      kind === 'profiles'
        ? state.profilesRequest
        : kind === 'roles'
          ? state.rolesRequest
          : state.sandboxRequest;
    const token = lifecycle.beginRequest();
    try {
      const data =
        kind === 'profiles'
          ? await profiles.loadProfiles(true)
          : kind === 'roles'
            ? await roles.loadRoles(true)
            : await sandbox.loadSandboxProfiles();
      const committed = lifecycle.commitRequest(token, data);
      // Any successful authoritative sandbox-registry load makes the cached
      // registry trustworthy again for break-glass composition.
      if (committed && kind === 'sandbox' && state.sandboxRegistryRecoveryRequired) {
        state.sandboxRegistryRecoveryRequired.value = false;
      }
      return committed;
    } catch (error) {
      lifecycle.failRequest(token, error);
      return false;
    }
  }

  async function openManager(kind) {
    state.openManager(kind);
    await load(kind);
  }
  function openProfileEditor(seed = null, options = {}) {
    state.openDialog({
      kind: 'profile-editor',
      seed,
      options,
      catalog: getSnapshot()?.harnesses || [],
    });
  }
  function openRoleEditor(seed = null) {
    void load('profiles');
    state.openDialog({
      kind: 'role-editor',
      seed,
      catalog: getSnapshot()?.harnesses || [],
      slugs: getSnapshot()?.slugs || [],
    });
  }
  function openSandboxEditor(seed = null, options = {}) {
    state.openDialog({ kind: 'sandbox-editor', seed, options });
    if (options.notice) state.error.value = options.notice;
  }
  function openSandboxClone(source) {
    if (!source?.name) {
      notify('sandbox profile not found', true);
      return false;
    }
    const name = sandboxCloneName(
      source.name,
      (state.sandboxProfiles.value || []).map((profile) => profile.name),
    );
    // Keep clone creation in the normal sandbox editor. Besides making the
    // copy reviewable, this preserves its normalized diff and the mandatory
    // fresh acknowledgement when break-glass authority is carried over.
    state.openDialog({
      kind: 'sandbox-editor',
      seed: { ...source, name },
      options: { editExisting: false, cloneSourceName: source.name },
    });
    return true;
  }
  function openTemplateEditor(seed = null, options = {}) {
    state.openTemplateDialog({ kind: 'template-editor', seed, options });
    void load('profiles');
    void load('roles');
  }

  async function saveTemplate({ draft, originalName, payload }) {
    state.busy.value = 'template-save';
    state.error.value = '';
    try {
      await templates.saveTemplate(originalName, payload);
      notify(
        originalName
          ? `template updated: ${draft.name.trim()}`
          : `template created: ${draft.name.trim()}`,
      );
      state.closeTemplateDialog();
      await refresh();
      return true;
    } catch (error) {
      state.error.value = error.message || String(error);
      return false;
    } finally {
      state.busy.value = '';
    }
  }

  async function removeTemplate(name) {
    if (
      !(await confirm({
        title: 'Delete template?',
        body: `Delete the template "${name}"? This removes the blueprint only — any groups already instantiated from it are left untouched.`,
        meta: name,
        okLabel: 'Delete template',
      }))
    )
      return false;
    try {
      await templates.deleteTemplate(name);
      notify(`template deleted: ${name}`);
      await refresh();
      return true;
    } catch (error) {
      notify(error.message || String(error), true);
      return false;
    }
  }

  function updateTemplates(value, groups) {
    state.updateTemplates(value, groups);
  }
  function openTemplateManager(options = {}) {
    const snapshot = getSnapshot() || {};
    state.updateTemplates(snapshot.templates || [], snapshot.groups || []);
    state.openTemplateManager(options);
  }
  function openTemplateDeploy(name, dropGroup = '') {
    const snapshot = getSnapshot() || {};
    const available = snapshot.templates || state.templates.value;
    if (!available.length) {
      notify(
        'no templates yet — define one via the Groups cog → templates first',
        true,
      );
      return;
    }
    state.updateTemplates(
      available,
      snapshot.groups || state.templateGroups.value,
    );
    state.openDialog({
      kind: 'template-deploy',
      presetName: available.some((template) => template.name === name)
        ? name
        : available[0].name,
      dropGroup: dropGroup.trim(),
    });
    void load('profiles');
  }
  function openTemplateDuplicate(name) {
    const source = state.templates.value.find(
      (template) => template.name === name,
    );
    if (!source) {
      notify('template not found', true);
      return;
    }
    state.openDialog({ kind: 'template-duplicate', source });
  }
  function exportTemplate(name) {
    const anchor = document.createElement('a');
    anchor.href = `/api/templates/${encodeURIComponent(name)}/export`;
    anchor.download = `${name}.task-force.json`;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
  }
  function openTemplateFromGroup(presetGroup = '') {
    const groups = (getSnapshot()?.groups || []).map((group) => group.name);
    if (!groups.length) {
      notify('no groups to snapshot', true);
      return;
    }
    state.openDialog({
      kind: 'template-from-group',
      presetGroup: groups.includes(presetGroup) ? presetGroup : '',
      groups,
    });
  }
  function openTemplateImport() {
    state.openDialog({ kind: 'template-import' });
  }
  async function openTemplateStarters() {
    const token = ++starterRequest;
    state.openDialog({
      kind: 'template-starters',
      request: { phase: 'loading', data: [], error: '' },
    });
    try {
      const data = await templates.loadStarters();
      if (
        token === starterRequest &&
        state.dialog.value?.kind === 'template-starters'
      )
        state.openDialog({
          kind: 'template-starters',
          request: { phase: 'ready', data, error: '' },
        });
    } catch (error) {
      if (
        token === starterRequest &&
        state.dialog.value?.kind === 'template-starters'
      )
        state.openDialog({
          kind: 'template-starters',
          request: {
            phase: 'error',
            data: [],
            error: error.message || String(error),
          },
        });
    }
  }
  async function duplicateTemplate(source, name) {
    const payload = { ...source, name: name.trim() };
    delete payload.created_at;
    delete payload.updated_at;
    await templates.saveTemplate('', payload);
    notify(`template duplicated: ${payload.name}`);
    state.closeDialog();
    await refresh();
    return payload;
  }
  async function importTemplate(raw, options) {
    const result = await templates.importTemplate(raw, options);
    const name = result.imported || options.as || 'template';
    const warnings = result.warnings || [];
    notify(
      `${result.updated ? 'template overwritten' : 'template imported'}: ${name}${warnings.length ? ` — ${warnings.length} warning${warnings.length === 1 ? '' : 's'}: ${warnings.join('; ')}` : ''}`,
    );
    state.closeDialog();
    await refresh();
    return result;
  }
  async function snapshotTemplateFromGroup(group, name, update) {
    const result = await templates.fromGroup({
      group,
      template_name: name,
      update,
    });
    state.closeDialog();
    const blank = result.blank_briefs || 0;
    const blankNote = blank
      ? ` — ⚠ ${blank} agent brief(s) blank; edit the template before deploying`
      : '';
    if (result.updated)
      notify(
        `template updated from ${group}: ${name} (briefs kept: ${(result.briefs_kept || []).length}, added: ${(result.added || []).length}, removed: ${(result.removed || []).length})${blankNote}`,
      );
    else notify(`template created from ${group}: ${name}${blankNote}`);
    await refresh();
    if (result.name || result.agents) openTemplateEditor(result);
    return result;
  }
  async function installTemplateStarter(name) {
    const result = await templates.installStarter(name);
    if (result.skipped)
      notify(
        result.message ||
          `${name} is already in your templates — nothing copied`,
      );
    else {
      const finalName = result.name || name;
      const warnings = result.warnings || [];
      notify(
        `added to your templates: ${finalName} — deploy or edit it from the list (nothing spawned yet)${warnings.length ? ` — ${warnings.length} warning${warnings.length === 1 ? '' : 's'}: ${warnings.join('; ')}` : ''}`,
      );
    }
    await refresh();
    return result;
  }
  function openGroupImport() {
    state.openDialog({ kind: 'group-import' });
  }
  function openGroupContext(name) {
    const group = (getSnapshot()?.groups || []).find(
      (item) => item.name === name,
    );
    state.openDialog({
      kind: 'group-context',
      group: name,
      context: group?.default_context || '',
    });
  }
  function openGroupClone(name) {
    const snapshotGroups = getSnapshot()?.groups || [];
    const source = snapshotGroups.find((item) => item.name === name) || null;
    const match = /^(.*?)-(?:c|clone)-\d+$/.exec(name);
    const base = match ? match[1] : name;
    const prefix = `${base}-c-`;
    const used = new Set(
      snapshotGroups
        .filter((item) => item.name?.startsWith(prefix))
        .map((item) => Number.parseInt(item.name.slice(prefix.length), 10))
        .filter(Number.isInteger),
    );
    let suffix = 1;
    while (used.has(suffix)) suffix += 1;
    state.openDialog({
      kind: 'group-clone',
      group: name,
      source,
      defaultName: `${prefix}${suffix}`,
    });
  }
  async function inspectGroupImport(file, as) {
    return groups.inspectImport(file, as);
  }
  async function importGroup(file, into, as) {
    const result = await groups.importGroup(file, into, as);
    state.closeDialog();
    let summary = `Imported group "${result.group}" — ${result.agent_count} agent(s), ${result.message_count} message(s)`;
    const remaps = Object.keys(result.conv_remaps || {}).length;
    if (remaps) summary += ` (${remaps} conv-id(s) remapped to fresh copies)`;
    notify(
      result.file_warnings?.length
        ? `${summary} — ${result.file_warnings.length} file warning(s); see the daemon log`
        : summary,
      !!result.file_warnings?.length,
    );
    onGroupImported(result.group);
    await refresh();
    return result;
  }
  async function saveGroupContext(name, context) {
    await groups.saveContext(name, context.trim());
    state.closeDialog();
    notify(
      context.trim()
        ? `${name}: startup context updated`
        : `${name}: startup context cleared`,
    );
    await refresh();
  }
  async function cloneGroup(
    name,
    defaultName,
    requestedName,
    withAgents,
    copyOwners,
  ) {
    const body = { no_clone_members: !withAgents, copy_owners: copyOwners };
    if (requestedName && requestedName !== defaultName)
      body.new_name = requestedName;
    const result = await groups.cloneGroup(name, body);
    state.closeDialog();
    const created = result.group ? `"${result.group}"` : 'new group';
    const failed = (result.members || []).filter(
      (member) => member?.error,
    ).length;
    const bits = [];
    if (!withAgents) bits.push('no member agents');
    if (!copyOwners) bits.push('no owners');
    notify(
      withAgents
        ? failed
          ? `Cloned ${name} → ${created} (${failed} member(s) skipped — see CLI for detail${bits.length ? `; ${bits.join(', ')}` : ''})`
          : `Cloned ${name} → ${created}${bits.length ? ` (${bits.join(', ')})` : ''}`
        : `Cloned ${name} → ${created} (settings only${copyOwners ? ' + owners' : ''})`,
      failed > 0,
    );
    await refresh();
    return result;
  }
  async function loadDeployWorktrees(repo) {
    return templates.loadWorktrees(repo);
  }
  async function createDeployWorktree(body) {
    return templates.createWorktree(body);
  }
  async function deployTemplate(name, kind, payload, mode = '') {
    const result = await templates.deployTemplate(name, kind, payload);
    state.closeDialog();
    state.closeTemplateManager();
    const group = result.group || payload.group_name;
    const failed = result.failed || 0;
    const spawned = result.spawned || 0;
    if (kind === 'reinforce')
      notify(
        failed
          ? `${group} reinforced: ${spawned} spawned, ${failed} failed — check the group`
          : `${group} reinforced: ${spawned} agent${spawned === 1 ? '' : 's'} added`,
        failed > 0,
      );
    else if (kind === 'instantiate')
      notify(
        failed
          ? `${group}: ${spawned} spawned, ${failed} failed — check the group`
          : mode === 'subgroup'
            ? `subgroup ${group} created — ${spawned} spawned`
            : `top-level group ${group} created — ${spawned} spawned`,
        failed > 0,
      );
    else
      notify(
        failed
          ? `task force ${group}: ${spawned} spawned, ${failed} failed — check the group`
          : payload.mission
            ? `task force ${group} deployed — ${spawned} spawned`
            : `group ${group}: spawned ${spawned} agent${spawned === 1 ? '' : 's'}`,
        failed > 0,
      );
    if (result.owner_note) notify(result.owner_note);
    const patternErrors = result.pattern_errors || [];
    if (patternErrors.length)
      notify(
        `⚠ work pattern: ${patternErrors.length} step${patternErrors.length === 1 ? '' : 's'} not sent — ${patternErrors[0]}`,
        true,
      );
    else if (result.pattern_delivered)
      notify(
        `work pattern: ${result.pattern_delivered} briefing${result.pattern_delivered === 1 ? '' : 's'} sent`,
      );
    if (result.pending_waves)
      notify(
        `staged deploy: ${result.pending_waves} more wave(s) will spawn as each settles`,
      );
    onGroupDeployed(group);
    await refresh();
    return result;
  }
  function editTemplatesWithAgent() {
    return summonTemplateScribe({ scope: 'library' });
  }
  function editTemplateWithAgent(name) {
    return summonTemplateScribe({ scope: 'template', name });
  }

  async function saveProfile({ draft, original, options, payload }) {
    state.busy.value = 'profile-save';
    state.error.value = '';
    try {
      if (options?.local) {
        options.local.onSave?.(payload);
        options.onSaved?.(draft.name.trim());
      } else if (original && options?.editExisting !== false) {
        await profiles.updateProfile(original.name, payload);
        notify(`profile updated: ${draft.name.trim()}`);
      } else {
        await profiles.createProfile(payload);
        notify(`profile created: ${draft.name.trim()}`);
      }
      const saved = draft.name.trim();
      state.closeDialog();
      if (!options?.local) await load('profiles');
      options?.onSaved?.(saved);
      return true;
    } catch (error) {
      state.error.value = error.message || String(error);
      return false;
    } finally {
      state.busy.value = '';
    }
  }

  async function saveRole({ draft, original, payload }) {
    state.busy.value = 'role-save';
    state.error.value = '';
    try {
      if (original) await roles.updateRole(original.name, payload);
      else await roles.createRole(payload);
      notify(
        original
          ? `role updated: ${draft.name.trim()}`
          : `role created: ${draft.name.trim()}`,
      );
      state.closeDialog();
      await load('roles');
      return true;
    } catch (error) {
      state.error.value = error.message || String(error);
      return false;
    } finally {
      state.busy.value = '';
    }
  }

  async function removeProfile(name) {
    if (
      !(await confirm({
        title: 'Delete profile?',
        body: `Delete the spawn profile "${name}"? Any group or the dashboard that names it as a default will fall back to blank spawn fields until re-pointed. Agents already spawned are untouched.`,
        meta: name,
        okLabel: 'Delete profile',
      }))
    )
      return false;
    try {
      await profiles.deleteProfile(name);
      notify(`profile deleted: ${name}`);
      await load('profiles');
      return true;
    } catch (error) {
      notify(error.message || String(error), true);
      return false;
    }
  }

  async function removeRole(name) {
    if (
      !(await confirm({
        title: 'Delete role?',
        body: `Delete the role "${name}"? Roles resolve at deploy time, so this is refused while any template still references it.`,
        meta: name,
        okLabel: 'Delete role',
      }))
    )
      return false;
    try {
      await roles.deleteRole(name);
      notify(`role deleted: ${name}`);
      await load('roles');
      return true;
    } catch (error) {
      notify(error.message || String(error), true);
      return false;
    }
  }

  async function saveSandbox({ draft, original, options = {}, breakGlassAcknowledged = false }) {
    state.busy.value = 'sandbox-save';
    state.error.value = '';
    try {
      const body = {
        name: draft.name.trim(),
        filesystem: draft.filesystem,
        environment: draft.environment,
        includes: draft.includes,
        agent_directories: draft.agent_directories,
        network_access: draft.network_access || '',
        read_baseline: draft.read_baseline || '',
        read_baseline_exclusions: draft.read_baseline_exclusions || [],
        break_glass_filesystem: draft.break_glass_filesystem || [],
      };
      if (!body.name) throw new Error('name is required');
      const targetName = options.editExisting === false
        ? ''
        : options.targetName || original?.name || '';
      const preview = await sandbox.previewSandboxProfile(targetName, body);
      if (
        preview.before &&
        JSON.stringify(preview.before) === JSON.stringify(preview.after)
      ) {
        notify('No sandbox profile changes to save');
        return false;
      }
      const ok = await state.confirmSandboxDiff(
        preview.before || null,
        preview.after,
      );
      if (!ok) {
        notify('Sandbox profile save cancelled');
        return false;
      }
      await sandbox.saveSandboxProfile(
        targetName,
        breakGlassAcknowledged
          ? { ...preview.after, break_glass_acknowledged: true }
          : preview.after,
        preview.revision || '',
      );
      state.closeDialog();
      notify(`sandbox profile saved: ${preview.after.name}`);
      await load('sandbox');
      await refreshSandboxSpawn();
      await options.onCreate?.(preview.after.name);
      return true;
    } catch (error) {
      if (error?.code === BREAK_GLASS_ACK_CODE) {
        // The daemon's authoritative state changed after the preview (e.g.
        // an included profile gained break-glass). Reload the registry so
        // the editor re-resolves its includes against current reality and
        // shows the exact current rules, then demand a fresh explicit
        // acknowledgement — never resend automatically. load() reports
        // failure as `false` rather than throwing; a failed reload means
        // the editor still cannot see the real rules, so it must stay
        // blocked from saving until an authoritative reload succeeds.
        let recovered = false;
        try { recovered = (await load('sandbox')) === true; } catch (_) { recovered = false; }
        state.error.value = recovered
          ? `${error.message || String(error)} The sandbox-profile registry changed since this preview — review the current break-glass rules above and re-acknowledge before saving again.`
          : `${error.message || String(error)} Reloading the sandbox-profile registry failed, so the current rules are unknown — saving stays blocked until an authoritative reload succeeds.`;
        return { breakGlassAckRequired: true, recovered };
      }
      state.error.value = error.message || String(error);
      return false;
    } finally {
      state.busy.value = '';
    }
  }

  async function removeSandbox(name) {
    if (
      !(await confirm({
        title: 'Delete sandbox profile?',
        body: `Delete “${name}”? Global and group assignments to it are cleared. Running agents keep their frozen launch snapshot.`,
        meta: name,
        okLabel: 'Delete sandbox profile',
      }))
    )
      return false;
    try {
      await sandbox.deleteSandboxProfile(name);
      notify(`sandbox profile deleted: ${name}`);
      await load('sandbox');
      await refreshSandboxSpawn();
      return true;
    } catch (error) {
      notify(error.message || String(error), true);
      return false;
    }
  }

  async function inspectProfiles(envelope) {
    return profiles.inspectProfileImport(envelope);
  }
  async function importProfileBundle(envelope, decisions) {
    const result = await profiles.importProfiles(envelope, decisions);
    await load('profiles');
    const imported = result?.imported || [];
    const skipped = result?.skipped || [];
    const warnings = result?.warnings || [];
    notify(
      `${imported.length} profile${imported.length === 1 ? '' : 's'} imported${skipped.length ? `, ${skipped.length} skipped` : ''}${warnings.length ? ` — ${warnings.join('; ')}` : ''}`,
    );
    return result;
  }
  async function exportProfileBundle(names) {
    const bundle = await profiles.exportProfiles(names);
    download('spawn-profiles.json', bundle);
    notify(`${names.length} profile${names.length === 1 ? '' : 's'} exported`);
    return bundle;
  }
  async function exportSandboxBundle(names) {
    const bundle = await sandbox.exportSandboxProfiles(names);
    download('sandbox-profiles.json', bundle);
    notify(
      `${names.length} sandbox profile${names.length === 1 ? '' : 's'} exported`,
    );
    return bundle;
  }
  async function inspectSandboxBundle(envelope) {
    return sandbox.inspectSandboxImport(envelope);
  }
  async function importSandboxBundle(envelope, conflict, breakGlassAcknowledged = false) {
    const result = await sandbox.importSandboxProfiles(envelope, conflict, breakGlassAcknowledged);
    await load('sandbox');
    await refreshSandboxSpawn();
    const imported = result?.imported || [];
    const skipped = result?.skipped || [];
    notify(
      `${imported.length} sandbox profile${imported.length === 1 ? '' : 's'} imported${skipped.length ? `, ${skipped.length} skipped` : ''}`,
    );
    return result;
  }
  async function configureSandboxWithAgent(seed, options = {}) {
    const editExisting = options.editExisting !== false
      && !!(options.targetName || seed?.name);
    await summonSandboxScribe(
      seed,
      editExisting ? options.targetName || seed?.name || '' : '',
      options.onCreate || null,
      { editExisting },
    );
  }
  function inspectDirectories(filesystem) {
    return sandbox.inspectSandboxDirectories({
      name: 'directory-preview',
      filesystem,
      environment: [],
    });
  }
  function createDirectories(filesystem) {
    return sandbox.createSandboxDirectories({
      name: 'directory-preview',
      filesystem,
      environment: [],
    });
  }

  function loadReadExclusionCatalog() {
    return sandbox.loadSandboxReadExclusionCatalog();
  }

  return Object.freeze({
    load,
    openManager,
    openProfileEditor,
    openRoleEditor,
    openSandboxEditor,
    openSandboxClone,
    openTemplateManager,
    openTemplateEditor,
    updateTemplates,
    saveProfile,
    saveRole,
    saveSandbox,
    saveTemplate,
    removeProfile,
    removeRole,
    removeSandbox,
    removeTemplate,
    openTemplateDeploy,
    deployTemplate,
    loadDeployWorktrees,
    createDeployWorktree,
    openTemplateDuplicate,
    duplicateTemplate,
    exportTemplate,
    openTemplateFromGroup,
    snapshotTemplateFromGroup,
    openTemplateImport,
    importTemplate,
    openTemplateStarters,
    installTemplateStarter,
    editTemplatesWithAgent,
    editTemplateWithAgent,
    openGroupImport,
    inspectGroupImport,
    importGroup,
    openGroupContext,
    saveGroupContext,
    openGroupClone,
    cloneGroup,
    inspectProfiles,
    importProfileBundle,
    exportProfileBundle,
    exportSandboxBundle,
    inspectSandboxBundle,
    importSandboxBundle,
    configureSandboxWithAgent,
    inspectDirectories,
    createDirectories,
    loadReadExclusionCatalog,
  });
}
// dashboard-imperative-boundary: browser-io
