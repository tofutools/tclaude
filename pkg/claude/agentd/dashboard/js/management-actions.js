import {
  loadProfiles,
  createProfile,
  updateProfile,
  deleteProfile,
  exportProfiles,
  inspectProfileImport,
  importProfiles,
  profilesVia,
} from './profiles.js';
import { loadRoles, createRole, updateRole, deleteRole, rolesVia } from './roles.js';
import { signal } from '@preact/signals';
import { createRequestLifecycle } from './request-lifecycle.js';
import { ambientLaunchDecisions } from './management-model.js';
import { fetchUnsandboxedAutonomy } from './unsandboxed-autonomy.js';
import { announceNodeChanged, isPageNode, nodeDownload, nodeDownloadPath, nodeFetch, nodeSnapshot, selfNodeID } from './node-api.js';
import { unrestrictedOn } from './node-trust.js';
import { exportParams, landingParams, queryString } from './paf-model.js';
import {
  loadSandboxProfiles,
  loadSandboxCommonRules,
  predictSandboxProfile,
  sandboxProfileForWire,
  previewSandboxProfile,
  saveSandboxProfile,
  deleteSandboxProfile,
  exportSandboxProfiles,
  inspectSandboxImport,
  importSandboxProfiles,
  inspectSandboxDirectories,
  createSandboxDirectories,
  sandboxVia,
} from './sandbox-profiles-data.js';

const MAX_SANDBOX_PROFILE_NAME_BYTES = 200;

function utf8Length(value) {
  return new TextEncoder().encode(value).length;
}

// cloneName suggests the free "<source>-copy" handle a clone opens with,
// bumping the suffix until it misses everything in `existingNames`. `maxBytes`
// caps the result for registries that enforce a name length (sandbox profiles);
// the prefix is trimmed by whole code points so a multi-byte name never gets
// cut mid-character. Spawn profiles have no such cap and pass none.
function cloneName(sourceName, existingNames, maxBytes = Infinity) {
  const names = new Set(existingNames);
  for (let suffix = 1; ; suffix += 1) {
    const tail = suffix === 1 ? '-copy' : `-copy-${suffix}`;
    const budget = maxBytes - utf8Length(tail);
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

export function createManagementActions(deps) {
  const {
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
  // fetchImpl reaches the node whose catalogs these actions edit: the page's
  // own, or another node's for catalogOn.
  fetchImpl = (...args) => fetch(...args),
  } = deps;
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
    loadSandboxCommonRules,
    predictSandboxProfile,
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
      const response = await fetchImpl(
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
      const response = await fetchImpl(
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
    async fromGroup(body, node = '') {
      const response = await nodeFetch(node)('/api/templates/from-group', {
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
    async inspectImport(file, as = '', landing = {}) {
      const form = new FormData();
      form.append('archive', file);
      if (as) form.append('as', as);
      const response = await fetch(`/api/groups/import/inspect${queryString(landingParams(landing))}`, {
        method: 'POST',
        credentials: 'same-origin',
        body: form,
      });
      const body = await response.json().catch(() => null);
      if (!response.ok)
        throw new Error(body?.error || `HTTP ${response.status}`);
      return body;
    },
    async importGroup(file, into, as = '', landing = {}, keep = {}) {
      const form = new FormData();
      form.append('archive', file);
      form.append('into', into);
      if (as) form.append('as', as);
      const response = await fetch(`/api/groups/import${queryString({ ...landingParams(landing), ...keep })}`, {
        method: 'POST',
        credentials: 'same-origin',
        body: form,
      });
      const body = await response.json().catch(() => null);
      if (!response.ok)
        throw new Error(body?.error || `HTTP ${response.status}`);
      return body;
    },
    async saveContext(name, context, node = '') {
      const response = await nodeFetch(node)(`/api/groups/${encodeURIComponent(name)}`, {
        method: 'PATCH',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ default_context: context }),
      });
      if (!response.ok)
        throw new Error((await response.text()) || `HTTP ${response.status}`);
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
      return lifecycle.commitRequest(token, data);
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
    void load('roles');
    state.openDialog({
      kind: 'profile-editor',
      seed,
      options,
      catalog: getSnapshot()?.harnesses || [],
      // Host-wide sandbox-implementation catalog, so the editor can label the
      // tclaude layer and disclose whether this host can run it.
      sandboxImpl: getSnapshot()?.sandbox_impl || {},
    });
  }
  // openProfileClone seeds the normal profile editor with a copy of `source`,
  // so the duplicate is reviewed and saved through the same validated path as
  // any hand-written profile rather than being POSTed behind the operator's
  // back. Mirrors openSandboxClone.
  function openProfileClone(source) {
    if (!source?.name) {
      notify('spawn profile not found', true);
      return false;
    }
    // Primary names and aliases live in ONE server-side namespace, so the
    // suggested handle has to dodge both — landing on an existing alias is a
    // 400 at save time, not a rename prompt.
    const taken = (state.profiles.value || []).flatMap((profile) => [
      profile.name,
      ...(profile.aliases || []),
    ]);
    // The copy starts with no aliases for the same reason: an alias is a
    // single-holder handle, so carrying the source's list over would guarantee
    // that 400.
    openProfileEditor(
      { ...source, aliases: [], name: cloneName(source.name, taken) },
      { editExisting: false, cloneSourceName: source.name },
    );
    return true;
  }
  function openRoleEditor(seed = null) {
    state.openDialog({
      kind: 'role-editor',
      seed,
      slugs: getSnapshot()?.slugs || [],
    });
  }
  function openSandboxEditor(seed = null, options = {}) {
    state.openDialog({
      kind: 'sandbox-editor', seed, options,
      catalog: getSnapshot()?.harnesses || [],
      sandboxImpl: getSnapshot()?.sandbox_impl || {},
    });
    if (options.notice) state.error.value = options.notice;
  }
  function openSandboxClone(source) {
    if (!source?.name) {
      notify('sandbox profile not found', true);
      return false;
    }
    const name = cloneName(
      source.name,
      (state.sandboxProfiles.value || []).map((profile) => profile.name),
      MAX_SANDBOX_PROFILE_NAME_BYTES,
    );
    // Keep clone creation in the normal sandbox editor, so the copy is
    // reviewable through the same normalized diff as any other edit.
    state.openDialog({
      kind: 'sandbox-editor',
      seed: { ...source, name },
      options: { editExisting: false, cloneSourceName: source.name },
      catalog: getSnapshot()?.harnesses || [],
      sandboxImpl: getSnapshot()?.sandbox_impl || {},
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
    if (refuseOnRestrictedPeer('Template export', '', notify)) return;
    const anchor = document.createElement('a');
    anchor.href = nodeDownloadPath('', `/api/templates/${encodeURIComponent(name)}/export`);
    anchor.download = `${name}.task-force.json`;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
  }
  function openTemplateFromGroup(presetGroup = '', node = '') {
    // Another node's group snapshots into that node's templates.
    const snap = (node && nodeSnapshot(node)) || getSnapshot();
    const groups = (snap?.groups || []).map((group) => group.name);
    if (!groups.length) {
      notify('no groups to snapshot', true);
      return;
    }
    state.openDialog({
      kind: 'template-from-group',
      presetGroup: groups.includes(presetGroup) ? presetGroup : '',
      groups,
      ...(node ? { node, templates: snap?.templates || [] } : {}),
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
  async function snapshotTemplateFromGroup(group, name, update, node = '') {
    const result = await templates.fromGroup({
      group,
      template_name: name,
      update,
    }, node);
    state.closeDialog();
    const blank = result.blank_briefs || 0;
    const blankNote = blank
      ? ` — ⚠ ${blank} agent brief(s) blank; edit the template before deploying`
      : '';
    // Launch fields the re-snapshot refused to carry forward (TCL-1083). Nothing
    // else tells the operator: the template saves cleanly and the loss would
    // otherwise surface as a failed deploy, or never.
    const droppedNote = (result.dropped || [])
      .map((d) => ` — ⚠ ${d.agent}: dropped ${(d.fields || []).join(', ')} (${d.reason})`)
      .join('');
    if (result.updated)
      notify(
        `template updated from ${group}: ${name} (briefs kept: ${(result.briefs_kept || []).length}, added: ${(result.added || []).length}, removed: ${(result.removed || []).length})${blankNote}${droppedNote}`,
      );
    // A create drops things too — not curated fields (it has none to lose) but
    // values observed rather than chosen, which it declines to pin (TCL-1090).
    // That is invisible in the template it just opened, so it is appended here
    // as well; this used to hang off the `updated` branch only.
    else notify(`template created from ${group}: ${name}${blankNote}${droppedNote}`);
    // Another node's template is edited on its own dashboard: this node's
    // editor saves here.
    if (node) {
      notify(`saved in that node's templates: edit ${name} on its own dashboard`);
      announceNodeChanged(node);
      return result;
    }
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
  // openGroupImport opens the one ⤒ import… dialog; file preloads a
  // dropped .paf or .zip.
  let importKey = 0;
  // landing names one agent of the file and the role it lands under.
  function openGroupImport(file = null, landing = null) {
    state.openDialog({ kind: 'group-import', file, landing, key: ++importKey });
  }
  // node names another node's group (the fused Groups view): its snapshot
  // holds the context, and the save goes to it.
  function openGroupContext(name, node = '') {
    const group = ((nodeSnapshot(node) || getSnapshot())?.groups || []).find(
      (item) => item.name === name,
    );
    state.openDialog({
      kind: 'group-context',
      group: name,
      node,
      context: group?.default_context || '',
    });
  }
  async function inspectGroupImport(file, as, landing) {
    return groups.inspectImport(file, as, landing);
  }
  async function importGroup(file, into, as, landing, keep) {
    const result = await groups.importGroup(file, into, as, landing, keep);
    let summary = `Imported group "${result.group}" — ${result.agent_count} agent(s), ${result.message_count} message(s)`;
    const remaps = Object.keys(result.conv_remaps || {}).length;
    if (remaps) summary += ` (${remaps} conv-id(s) remapped to fresh copies)`;
    const warnings = [...(result.file_warnings || []), ...(result.prompt_warnings || [])];
    if (!warnings.length) {
      state.closeDialog();
      notify(summary);
    }
    onGroupImported(result.group);
    await refresh();
    return warnings.length || result.kept?.length || result.dropped?.length
      ? { summary, warnings, kept: result.kept, dropped: result.dropped } : null;
  }
  // revealImportValue fetches one environment value from the picked file, for
  // the click that asked. It is shown in that row and kept nowhere else.
  async function revealImportValue(file, item) {
    const form = new FormData();
    form.append('archive', file);
    const response = await nodeFetch('')(`/api/import/reveal${queryString({ item })}`, {
      method: 'POST',
      credentials: 'same-origin',
      body: form,
    });
    const body = await response.json().catch(() => null);
    if (!response.ok) throw new Error(body?.error || `HTTP ${response.status}`);
    return body;
  }
  // PAF export / redact / agent import (tcl-jkmitn). Each goes through the
  // dashboard's /api twin of the human-only /v1 route.
  async function pafError(response) {
    const body = await response.json().catch(() => null);
    const error = new Error(body?.error || `HTTP ${response.status}`);
    error.status = response.status;
    error.body = body;
    return error;
  }
  // exportAgentPAF fetches the agent's PAF. A 422 with code
  // flagged_credentials carries the findings, for the dialog to show.
  // An export downloads from the node it names (blank: the page's), another
  // node's in chunks (nodeDownload); a file in hand is redacted or published
  // here, on this node.
  async function exportAgentPAF(choices, node = '') {
    const response = await nodeDownload(node, `/api/agent-bundle/export${queryString(exportParams(choices))}`);
    if (!response.ok) throw await pafError(response);
    return response.blob();
  }
  async function exportGroupArchive(group, format, allowFlagged = false, node = '', workflows = false) {
    const response = await nodeDownload(node, `/api/groups/${encodeURIComponent(group)}/export${queryString({ format, allow_flagged: allowFlagged, workflows: workflows ? 'own' : '' })}`);
    if (!response.ok) throw await pafError(response);
    return { blob: await response.blob(), filename: response.headers.get('X-Tclaude-Export-Filename') || dispositionName(response) || `group-${group}.${format}` };
  }
  async function redactPAF(blob, options, preview) {
    const response = await thisNodeFetch(`/api/paf/redact${queryString({ ...options, preview })}`, {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/vnd.tclaude.paf+zip' }, body: blob,
    });
    if (!response.ok) throw await pafError(response);
    return preview ? response.json() : response.blob();
  }
  // importAgentPAF previews (apply false) or lands the agent, started or
  // offline (start=false). A landing with warnings resolves to them for the
  // dialog to show, as importGroup does: too much to read in a passing toast.
  async function importAgentPAF(file, params) {
    const response = await fetch(`/api/agent-bundle/import${queryString(params)}`, {
      method: 'POST', credentials: 'same-origin', body: file,
    });
    if (!response.ok) throw await pafError(response);
    const result = await response.json();
    if (params.apply) {
      const summary = `Imported agent "${result.agent?.name || params.name || 'agent'}" into ${params.group}${result.offline ? ' offline; wake it to start' : ' and started it'}`;
      const warnings = result.warnings || [];
      const outcome = warnings.length || result.kept?.length || result.dropped?.length;
      if (!outcome) {
        state.closeDialog();
        notify(summary);
      }
      await refresh();
      return outcome ? { summary, warnings, kept: result.kept, dropped: result.dropped } : null;
    }
    return result;
  }
  // Snapshot library (4d, tcl-3hjdm1) through the dashboard /api twins, on
  // the node the dialog names (blank: the page's).
  async function snapshotCall(method, path, body, node = '') {
    const response = await nodeFetch(node)(`/api/${path}`, {
      method, credentials: 'same-origin',
      ...(body !== undefined ? { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) } : {}),
    });
    if (!response.ok) throw await pafError(response);
    return response.json();
  }
  // snapshotsOn is the library of node (blank: the page's). Another node's
  // snapshot is shared on a board from here, as the file it downloads.
  function snapshotsOn(node = '') {
    const files = new Map();
    const lib = {
      list: async () => (await snapshotCall('GET', 'snapshots', undefined, node)).snapshots || [],
      create: (body) => { files.clear(); return snapshotCall('POST', 'snapshots', body, node); },
      remove: (name) => { files.delete(name); return snapshotCall('DELETE', `snapshots/${encodeURIComponent(name)}`, undefined, node); },
      prune: () => snapshotCall('POST', 'snapshots/prune', {}, node),
      async spawn(name, body) {
        const result = await snapshotCall('POST', `snapshots/${encodeURIComponent(name)}/spawn`, body, node);
        announceNodeChanged(node);
        return result;
      },
      policies: async () => (await snapshotCall('GET', 'resilience/policies', undefined, node)).policies || [],
      async exportBlob(name) {
        const response = await nodeDownload(node, `/api/snapshots/${encodeURIComponent(name)}/export`);
        if (!response.ok) throw await pafError(response);
        return response.blob();
      },
      // share previews (no digest) or publishes (digest) a snapshot on a
      // board; blob, when given, shares that file (a redacted copy) instead.
      async share(board, { snapshot = '', name = '', to = [], expires = '', digest = '', blob = null }) {
        if (!blob && !onThisNode(node)) {
          if (!files.has(snapshot)) files.set(snapshot, await lib.exportBlob(snapshot));
          blob = files.get(snapshot);
        }
        const query = queryString({ snapshot: blob ? '' : snapshot, name, to, expires, content_digest: digest });
        const response = await thisNodeFetch(`/api/federation/boards/${encodeURIComponent(board)}/snapshots${query}`, {
          method: 'POST', credentials: 'same-origin',
          ...(blob ? { headers: { 'Content-Type': 'application/vnd.tclaude.paf+zip' }, body: blob } : {}),
        });
        if (!response.ok) throw await pafError(response);
        return response.json();
      },
      // publish previews (no digest) or returns the signed public PAF.
      async publish(blob, options, digest = '') {
        const response = await thisNodeFetch(`/api/paf/publish${queryString({ ...options, content_digest: digest })}`, {
          method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/vnd.tclaude.paf+zip' }, body: blob,
        });
        if (!response.ok) throw await pafError(response);
        return digest ? response.blob() : response.json();
      },
    };
    return lib;
  }
  const snapshots = snapshotsOn();
  // catalogOn is these actions over another node's catalogs (profiles,
  // roles, sandbox profiles, templates), for editing its items in place:
  // reads and writes go through that node's fetch, its snapshot supplies the
  // harness and permission catalogs, and the lists are its own. Dialogs still
  // open in this page's slots, stamped with the node so they render with
  // these actions (management-island.js catalogFor). A blank node is the
  // page's (these very actions).
  const catalogs = new Map();
  function catalogOn(node = '') {
    if (!node || isPageNode(node)) return controller;
    if (!catalogs.has(node)) catalogs.set(node, nodeCatalog(node));
    const child = catalogs.get(node);
    const snap = nodeSnapshot(node) || {};
    child.catalogState.updateTemplates(snap.templates || [], snap.groups || []);
    return child;
  }
  function nodeCatalog(node) {
    const via = nodeFetch(node);
    const lists = { profiles: signal([]), roles: signal([]), sandboxProfiles: signal([]), templates: signal([]), templateGroups: signal([]) };
    const nodeState = Object.freeze({
      ...state,
      ...lists,
      profilesRequest: createRequestLifecycle({ payload: lists.profiles, retainPayloadOnRefresh: true, retainPayloadOnError: true }),
      rolesRequest: createRequestLifecycle({ payload: lists.roles, retainPayloadOnRefresh: true, retainPayloadOnError: true }),
      sandboxRequest: createRequestLifecycle({ payload: lists.sandboxProfiles, retainPayloadOnRefresh: true, retainPayloadOnError: true }),
      openDialog: (value) => state.openDialog({ ...value, node }),
      openTemplateDialog: (value) => state.openTemplateDialog({ ...value, node }),
      updateTemplates(value, groups = []) {
        lists.templates.value = Array.isArray(value) ? value : [];
        lists.templateGroups.value = Array.isArray(groups) ? groups : [];
      },
    });
    const changed = async () => { announceNodeChanged(node); };
    const base = createManagementActions({
      ...deps,
      state: nodeState,
      getSnapshot: () => nodeSnapshot(node) || {},
      refresh: changed,
      refreshSandboxSpawn: changed,
      profileAPI: profilesVia(via),
      roleAPI: rolesVia(via),
      sandboxAPI: sandboxVia(via),
      fetchImpl: via,
    });
    // A saved item shows on that node's dock card once its snapshot is
    // re-read.
    const announced = (fn) => async (...args) => {
      const result = await fn(...args);
      if (result) announceNodeChanged(node);
      return result;
    };
    return Object.freeze({
      ...base,
      saveProfile: announced(base.saveProfile),
      saveRole: announced(base.saveRole),
      saveSandbox: announced(base.saveSandbox),
      catalogNode: node,
      catalogState: nodeState,
    });
  }
  // actionsOn binds the snapshot and export actions a dialog takes to the
  // node it names; a blank node is the page's (these very actions).
  const nodeActions = new Map();
  function actionsOn(node = '') {
    if (!node || isPageNode(node)) return controller;
    if (!nodeActions.has(node)) {
      nodeActions.set(node, Object.freeze({
        ...controller,
        snapshots: snapshotsOn(node),
        exportAgentPAF: (choices) => exportAgentPAF(choices, node),
        exportGroupArchive: (group, format, allowFlagged = false, _node = '', workflows = false) => exportGroupArchive(group, format, allowFlagged, node, workflows),
      }));
    }
    return nodeActions.get(node);
  }
  // The snapshot and export dialogs take the node they act on (blank: the
  // page's): this node, or one that trusts it unrestricted.
  function openSnapshots(preset = {}) {
    if (refuseOnRestrictedPeer('Snapshots', preset.node, notify)) return;
    state.openDialog({ kind: 'snapshots', ...preset });
  }

  function openAgentExport({ agent, label, node = '' }) {
    if (refuseOnRestrictedPeer('Agent export', node, notify)) return;
    state.openDialog({ kind: 'paf-export', agent, label, node });
  }
  function openGroupExport(group, node = '') {
    if (refuseOnRestrictedPeer('Group export', node, notify)) return;
    state.openDialog({ kind: 'group-export', group, node });
  }

  async function saveGroupContext(name, context, node = '') {
    await groups.saveContext(name, context.trim(), node);
    state.closeDialog();
    notify(
      context.trim()
        ? `${name}: startup context updated`
        : `${name}: startup context cleared`,
    );
    announceNodeChanged(node);
    await refresh();
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
    // Per-member launch provenance. A deploy resolves the group and global
    // default profiles INSIDE the spawn, so a member can land on a harness,
    // model, containment or Copilot drive that nobody picked in this dialog —
    // and until TCL-1097 the only trace of that was a daemon log line. Only
    // ambient decisions are announced: a value the deployer typed needs no
    // telling, and a roster that inherited nothing produces no toasts at all.
    for (const member of result.agents || []) {
      const decisions = ambientLaunchDecisions(member.resolved);
      if (!decisions.length) continue;
      notify(`${member.final_name || member.name}: ${decisions.join(', ')}`);
    }
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
  async function editTemplatesWithAgent() {
    const result = await summonTemplateScribe({ scope: 'library' });
    if (result?.focus_mode === 'browser' && result.focus_ws) state.closeTemplateManager();
    return result;
  }
  async function editTemplateWithAgent(name) {
    const result = await summonTemplateScribe({ scope: 'template', name });
    if (result?.focus_mode === 'browser' && result.focus_ws) state.closeTemplateManager();
    return result;
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
      } else if (options?.node && !isPageNode(options.node)) {
        // The spawn dialog's "Save on" another node (profile-save-nodes.js).
        const response = await nodeFetch(options.node)('/api/spawn-profiles', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
        if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
        announceNodeChanged(options.node);
        notify(`profile created on ${options.nodeName || 'that node'}: ${draft.name.trim()}`);
      } else {
        await profiles.createProfile(payload);
        notify(`profile created: ${draft.name.trim()}`);
      }
      const saved = draft.name.trim();
      state.closeDialog();
      if (!options?.local) await load('profiles');
      options?.onSaved?.(saved, options?.node || '');
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

  async function saveSandbox({ draft, original, options = {} }) {
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
      };
      // Send pre_launch only when the draft actually carries it. The daemon
      // treats an absent key as "leave the blocks alone" and an explicit []
      // as "delete them", so an editor that always sent the field would wipe
      // an operator's setup script the moment it loaded a profile it cannot
      // yet display.
      if (draft.pre_launch !== undefined) body.pre_launch = draft.pre_launch;
      // Same tri-state as pre_launch, and it matters for the same reason: the
      // daemon reads an absent tmpfs key as "leave the mounts alone" and an
      // explicit [] as "unmount them", so a body that always carried the field
      // would silently drop an agent's scratch space the moment this editor
      // loaded a profile it could not yet display.
      if (draft.tmpfs !== undefined) body.tmpfs = draft.tmpfs;
      if (draft.id) body.id = draft.id;
      if (draft.filesystem_spellings !== undefined) body.filesystem_spellings = draft.filesystem_spellings;
      if (draft.network !== undefined) body.network = draft.network;
      if (draft.unix_sockets !== undefined) body.unix_sockets = draft.unix_sockets;
      if (draft.resource_limits !== undefined) body.resource_limits = draft.resource_limits;
      if (draft.darwin_disable_keychain_write !== undefined) {
        body.darwin_disable_keychain_write = !!draft.darwin_disable_keychain_write;
      }
      if (draft.darwin_allow_mach_register !== undefined) {
        body.darwin_allow_mach_register = !!draft.darwin_allow_mach_register;
      }
      Object.assign(body, sandboxProfileForWire(body));
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
        preview.notices || [],
      );
      if (!ok) {
        notify('Sandbox profile save cancelled');
        return false;
      }
      // The commit sends the server's own rendering, but pre_launch carries
      // omitempty — so a deliberate "delete every block" ([]) comes back from
      // the preview with the key absent, and absence means "keep them". Carry
      // the intent across explicitly, or clearing a block would silently do
      // nothing.
      const committed = {
        ...preview.after,
        ...(body.pre_launch === undefined ? {} : { pre_launch: body.pre_launch }),
        // preview.after carries tmpfs with omitempty too, so a deliberate
        // "remove every mount" ([]) comes back with the key absent — which
        // means "keep them". Carry the intent across explicitly or clearing
        // the last mount would silently do nothing.
        ...(body.tmpfs === undefined ? {} : { tmpfs: body.tmpfs }),
      };
      await sandbox.saveSandboxProfile(targetName, committed, preview.revision || '');
      state.closeDialog();
      notify(`sandbox profile saved: ${preview.after.name}`);
      await load('sandbox');
      await refreshSandboxSpawn();
      await options.onCreate?.(preview.after.name);
      return true;
    } catch (error) {
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
  async function importSandboxBundle(envelope, conflict) {
    const result = await sandbox.importSandboxProfiles(envelope, conflict);
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
    const result = await summonSandboxScribe(
      seed,
      editExisting ? options.targetName || seed?.name || '' : '',
      options.onCreate || null,
      { editExisting, cloneSourceName: options.cloneSourceName || '' },
    );
    if (result?.focus_mode === 'browser' && result.focus_ws) state.closeManager();
    return result;
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

  function loadCommonRuleCatalog() {
    return sandbox.loadSandboxCommonRules();
  }

  // Profile/role editors probe with NO dir: a saved profile is portable, so
  // only the machine-global settings tiers are knowable at edit time — which is
  // exactly what the daemon reads when dir is empty. (The spawn dialog passes a
  // real CWD; both share fetchUnsandboxedAutonomy.)
  function loadUnsandboxedAutonomy({
    harness = '', sandbox: harnessBuiltinMode = '', sandboxImplementation = '', approval = '',
  } = {}) {
    return fetchUnsandboxedAutonomy(fetchImpl, {
      harness, sandbox: harnessBuiltinMode, sandboxImplementation, approval, dir: '',
    });
  }

  const controller = Object.freeze({
    load,
    openManager,
    openProfileEditor,
    openProfileClone,
    openRoleEditor,
    openSandboxEditor,
    openSandboxClone,
    openTemplateManager,
    openTemplateEditor,
    updateTemplates,
    saveProfile,
    saveRole,
    saveSandbox,
    predictSandbox: sandbox.predictSandboxProfile,
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
    revealImportValue,
    exportAgentPAF,
    exportGroupArchive,
    redactPAF,
    importAgentPAF,
    openAgentExport,
    openGroupExport,
    snapshots,
    openSnapshots,
    openGroupContext,
    saveGroupContext,
    inspectProfiles,
    importProfileBundle,
    exportProfileBundle,
    exportSandboxBundle,
    inspectSandboxBundle,
    importSandboxBundle,
    configureSandboxWithAgent,
    inspectDirectories,
    createDirectories,
    loadCommonRuleCatalog,
    loadUnsandboxedAutonomy,
    actionsOn,
    catalogOn,
    catalogNode: '',
    catalogState: state,
  });
  return controller;
}
// thisNodeFetch reaches this node's own daemon, on a peer's page too: a file
// in hand is redacted, published or shared on a board from here.
function thisNodeFetch(...args) {
  return (globalThis.__tclaudeRemoteNode?.localFetch || globalThis.fetch)(...args);
}

function onThisNode(node) {
  return node ? node === selfNodeID() : !globalThis.__tclaudeRemoteNode?.id;
}

// refuseOnRestrictedPeer stops a snapshot or export control on a node that
// does not let this one act there as its operator does.
function refuseOnRestrictedPeer(what, node, toast) {
  if (onThisNode(node) || unrestrictedOn(node)) return false;
  toast(`${what} on another node takes its unrestricted trust`, true);
  return true;
}

function dispositionName(response) {
  const match = /filename="?([^";]+)"?/.exec(response.headers.get('Content-Disposition') || '');
  return match ? match[1] : '';
}

// dashboard-imperative-boundary: browser-io
