import { composeSandboxProfilePolicy } from './sandbox-profile-preview.js';
import { sandboxPredictionWarnings, sandboxProfileForWire } from './sandbox-profiles-data.js';
import { SANDBOX_PROFILE_NONE, WT_NEW } from './agent-spawn-model.js';
import { revealGroupPath, setFusedSectionOpen } from './groups-fused-sections.js';
import { fetchUnsandboxedAutonomy } from './unsandboxed-autonomy.js';
import { announceNodeChanged, nodeFetch, nodeSnapshot, nodeUpload } from './node-api.js';
import { trackedUpload } from './transfers.js';

const EFFORT_KEY = 'tclaude.dash.spawn.modelEffort';
const AUTOFOCUS_KEY = 'tclaude.dash.spawn.autofocus';

async function responseText(response) {
  try { return await response.text(); } catch (_) { return ''; }
}

// Failures carry the daemon's structured {"error", "code"} body; status and
// typed code stay on the thrown Error so submit-side recovery can key off the
// code rather than message text.
async function responseError(response, prefix = '') {
  const raw = await responseText(response);
  let body = null;
  try { body = JSON.parse(raw); } catch (_) { body = null; }
  const message = body?.message || body?.error || raw || `HTTP ${response.status}`;
  const error = new Error(prefix ? `${prefix}${message}` : message);
  error.status = response.status;
  if (body?.code) error.code = body.code;
  return error;
}

async function jsonRequest(fetchImpl, path, options = {}) {
  const response = await fetchImpl(path, { credentials: 'same-origin', ...options });
  if (!response.ok) throw await responseError(response);
  return response.json().catch(() => ({}));
}

const worktreeProgressDelay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function worktreeProgressID() {
  return `wt-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 14)}`;
}

function readEffortMap(prefs) {
  try {
    const value = JSON.parse(prefs.getItem(EFFORT_KEY));
    return value && typeof value === 'object' ? value : {};
  } catch (_) {
    return {};
  }
}

export function createAgentSpawnActions(dependencies = {}) {
  const {
  fetchImpl = fetch,
  prefs,
  loadProfiles,
  loadSandboxProfiles,
  loadUsageShares = null,
  getDashboardDefaultProfile = () => '',
  pickDirectory,
  openProfileEditor,
  openPermissions,
  openContextFeatures,
  confirm,
  notify = () => {},
  refresh = () => {},
  openTerminalPane = () => {},
  celebrateSlop = () => {},
  celebrateWizard = () => {},
  recordInteraction = () => {},
  shortID = (value) => value,
  getSnapshot = () => null,
  // uploadTransport posts the bytes with progress (transfers.js).
  uploadTransport = trackedUpload,
  // uploadImpl posts a file upload to the dialog's node; another node's
  // goes on in chunks (nodeUpload).
  uploadImpl = (path, init) => nodeUpload('', path, init, { fetchImpl, upload: uploadTransport }),
  } = dependencies;
  return Object.freeze({
    // forNode is the same dialog's actions aimed at another node's group
    // (target: {node, nodeId, name}, agent-spawn-state.js): every request
    // goes through nodeFetch(node) to that node's own routes — its profiles,
    // libraries, worktrees, queue and spawn — as its own dashboard would send
    // them. What works on the browser's own machine stays with it: the
    // directory picker. The new agent's terminal opens in the browser
    // terminal, not as a window on the node's desktop. The profile editor
    // saves where its "Save on" says.
    forNode(target) {
      return createNodeSpawnActions(dependencies, target);
    },

    autoFocusDefault() {
      try {
        const value = prefs?.getItem(AUTOFOCUS_KEY);
        return value == null ? true : value === '1';
      } catch (_) {
        return true;
      }
    },

    rememberedEffort(model) {
      return readEffortMap(prefs)[model] || '';
    },

    rememberLaunchPreferences(draft) {
      try {
        prefs?.setItem(AUTOFOCUS_KEY, draft.autoFocus ? '1' : '0');
        const map = readEffortMap(prefs);
        if (draft.effort) map[draft.model || ''] = draft.effort;
        else delete map[draft.model || ''];
        prefs?.setItem(EFFORT_KEY, JSON.stringify(map));
      } catch (_) {}
    },

    dashboardDefaultProfile() {
      return getDashboardDefaultProfile() || '';
    },

    async loadProfiles(force = false) {
      return loadProfiles(force);
    },

    // loadUsageShares lists the peers' model gateways this node may bill
    // to (model_shares.go handleModelSharesReceived).
    async loadUsageShares() {
      if (loadUsageShares) return loadUsageShares();
      const value = await jsonRequest(fetchImpl, '/api/federation/models/shares/received');
      return Array.isArray(value?.shares) ? value.shares : [];
    },

    async loadWorktrees(repo) {
      const value = String(repo || '').trim();
      if (!value) {
        return {
          repo: value, isRepo: false, empty: true, hasCommits: true,
          repoRoot: '', worktrees: [], branches: [], defaultBranch: '', subRepos: [],
        };
      }
      let data = {};
      try {
        data = await jsonRequest(fetchImpl, `/api/worktrees?repo=${encodeURIComponent(value)}`);
      } catch (_) {
        data = {};
      }
      return {
        repo: value,
        isRepo: !!data.is_repo,
        empty: false,
        hasCommits: data.has_commits !== false,
        repoRoot: data.repo_root || '',
        worktrees: Array.isArray(data.worktrees) ? data.worktrees : [],
        branches: Array.isArray(data.branches) ? data.branches : [],
        defaultBranch: data.default_branch || '',
        subRepos: Array.isArray(data.sub_repos) ? data.sub_repos : [],
      };
    },

    async resolveWorktree(draft, worktrees, onProgress = () => {}, onTiming = () => {}) {
      const selected = String(draft.worktree || '');
      if (!selected) return { path: '', branch: '' };
      const expectedRepo = String(draft.wtRepo || '').trim();
      if (worktrees?.phase !== 'ready' || String(worktrees.repo || '').trim() !== expectedRepo) {
        throw new Error('wait for the worktree repository to finish loading');
      }
      if (selected.startsWith('wt:')) {
        const path = selected.slice(3);
        const entry = (worktrees?.worktrees || []).find((item) => item.path === path);
        return { path, branch: entry?.branch || '' };
      }
      if (selected !== WT_NEW) return { path: '', branch: '' };
      const branch = String(draft.worktreeBranch || '').trim();
      if (!branch) throw new Error('enter a branch name for the new worktree');
      onProgress('Creating worktree…');
      const progressID = worktreeProgressID();
      const startedAt = performance.now();
      let finished = false;
      const pollProgress = (async () => {
        while (!finished) {
          await worktreeProgressDelay(200);
          if (finished) break;
          try {
            const response = await fetchImpl(`/api/worktrees/progress?id=${encodeURIComponent(progressID)}`, {
              credentials: 'same-origin',
            });
            if (!response.ok) continue;
            const status = await response.json();
            if (status.retrying) {
              onProgress(`Git config busy — retrying upstream setup (${status.attempt}/${status.max})…`);
            }
          } catch (_) {
            // Progress is advisory; the authoritative POST below still owns
            // success/failure and must not be cancelled by a polling blip.
          }
        }
      })();
      try {
        const response = await fetchImpl('/api/worktrees', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            repo: worktrees?.repoRoot || String(draft.wtRepo || '').trim(),
            branch,
            from_branch: draft.worktreeBase || '',
            fetch_latest: !!draft.fetchLatestWorktree,
            progress_id: progressID,
          }),
        });
        if (!response.ok) throw new Error((await responseText(response)) || `HTTP ${response.status}`);
        const payload = await response.json();
        if (payload.note) notify(payload.note);
        if (payload.tracking_fallback) onProgress('Worktree created without tracking; spawning agent…');
        else onProgress('Worktree ready; spawning agent…');
        return { path: payload.path || '', branch: payload.branch || branch };
      } finally {
        const responseAt = performance.now();
        finished = true;
        await pollProgress;
        const completedAt = performance.now();
        onTiming({
          worktree_progress_id: progressID,
          worktree_http_ms: responseAt - startedAt,
          worktree_progress_cleanup_ms: completedAt - responseAt,
          worktree_total_ms: completedAt - startedAt,
        });
      }
    },

    /* Ask the daemon what a launch would resolve for the fields this dialog
       leaves blank. The browser cannot work this out: clearing the profile row
       blanks the dialog but not the tiers beneath it. A selected spawn profile
       still rides on the spawn request and outranks the group/global defaults,
       and the daemon applies all three at launch. */
    async loadLaunchDefaults(groupName, profileHandle, harnessName) {
      const query = new URLSearchParams();
      if (groupName) query.set('group', groupName);
      if (profileHandle) query.set('profile', profileHandle);
      if (harnessName) query.set('harness', harnessName);
      return jsonRequest(fetchImpl, `/api/spawn-launch-defaults?${query.toString()}`);
    },

    async loadSandboxPolicy(groupName, selected = '') {
      const profiles = await loadSandboxProfiles();
      if (selected === SANDBOX_PROFILE_NONE) {
        return {
          profiles,
          selected,
          preview: 'No tclaude sandbox-profile values will be applied for this launch. Global, group, and explicit profile tiers are all omitted.',
        };
      }
      const [globalDefault, groupDefault] = await Promise.all([
        jsonRequest(fetchImpl, '/api/sandbox-profile-default'),
        groupName
          ? jsonRequest(fetchImpl, `/api/groups/${encodeURIComponent(groupName)}/sandbox-profile`)
          : Promise.resolve({ name: '' }),
      ]);
      const byName = Object.fromEntries((profiles || []).map((profile) => [profile.name, profile]));
      const applied = [];
      if (globalDefault.name && byName[globalDefault.name]) {
        applied.push({ scope: 'global', profile: byName[globalDefault.name] });
      }
      if (groupDefault.name && byName[groupDefault.name]) {
        applied.push({ scope: 'group', profile: byName[groupDefault.name] });
      }
      if (selected && byName[selected]) {
        applied.push({ scope: 'explicit', profile: byName[selected] });
      }
      const policy = composeSandboxProfilePolicy(applied, byName);
      let accessPreview = '';
      const predictionRoot = byName[selected] || byName[groupDefault.name] || byName[globalDefault.name];
      if (predictionRoot) {
        try {
          const prediction = await jsonRequest(fetchImpl, '/api/sandbox-profile-enforcement', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              draft: sandboxProfileForWire(predictionRoot),
              targets: [],
              context: { group: groupName || '' },
            }),
          });
          const warnings = sandboxPredictionWarnings(prediction);
          const details = [
            ...warnings.composition.map((notice) => notice.detail),
            ...warnings.capability,
          ];
          if (details.length) accessPreview = ` Warnings: ${[...new Set(details)].join(' · ')}`;
        } catch (error) {
          accessPreview = ` Access-axis preview unavailable: ${error?.message || String(error)}`;
        }
      }
      return {
        profiles,
        selected: byName[selected] ? selected : '',
        preview: policy.text + accessPreview,
      };
    },

    /* Ask the daemon whether this draft would actually be confined (TCL-586).
       The spawn dialog passes the launch CWD so project-level settings count.
       Shared with the profile/role editors via fetchUnsandboxedAutonomy. */
    async loadUnsandboxedAutonomy(input) {
      return fetchUnsandboxedAutonomy(fetchImpl, input);
    },

    async uploadAttachments(attachments) {
      if (!attachments?.length) return [];
      const form = new FormData();
      for (const attachment of attachments) {
        form.append('file', attachment.file, attachment.name);
      }
      const files = attachments.map((attachment) => ({ name: attachment.name, size: attachment.file.size }));
      const response = await uploadImpl('/api/spawn-attachments', { body: form, files, title: 'Uploading attachments' });
      if (!response.ok) {
        throw new Error(`attachment upload failed: ${(await responseText(response)) || `HTTP ${response.status}`}`);
      }
      const payload = await response.json();
      return (payload.files || []).map((file) => file.path);
    },

    // Best-effort telemetry after completion: never hold the dialog open or
    // turn a successful spawn into an error if diagnostic reporting fails.
    async reportTiming(timing) {
      try {
        await fetchImpl('/api/spawn-timing', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(timing),
        });
      } catch (_) {}
    },

    // Queued spawns store the dialog draft only. Nothing a launch does —
    // worktree creation, base-branch fetch, attachment upload, the spawn
    // itself — runs until the queued draft is launched through submit.
    async saveQueued(id, draft) {
      const response = await fetchImpl(id ? `/api/queued-spawns/${encodeURIComponent(id)}` : '/api/queued-spawns', {
        method: id ? 'PUT' : 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ draft }),
      });
      if (!response.ok) throw await responseError(response);
      return response.json().catch(() => ({}));
    },

    async deleteQueued(id) {
      const response = await fetchImpl(`/api/queued-spawns/${encodeURIComponent(id)}`, {
        method: 'DELETE', credentials: 'same-origin',
      });
      // Already gone (launched or discarded elsewhere) is the wanted end state.
      if (!response.ok && response.status !== 404) throw await responseError(response);
    },

    confirmDiscardQueued(label) {
      return confirm({
        title: 'Discard this queued agent?',
        body: 'The prepared spawn is removed from the queue. Nothing was launched, so there is nothing else to clean up.',
        meta: label,
        okLabel: 'Discard',
      });
    },

    queued(draft, edited) {
      notify(`${edited ? 'saved queued' : 'queued'} ${draft.name || draft.descr || 'agent'} → ${draft.group} — launch it from ⏸ queued`);
      refresh();
    },

    dequeued(draft) {
      notify(`discarded queued ${draft.name || draft.descr || 'agent'}`);
      refresh();
    },

    notifyError(message) {
      notify(message, true);
    },

    async spawn(request) {
      const response = await fetchImpl(request.url, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(request.body),
      });
      if (!response.ok) throw await responseError(response);
      return response.json().catch(() => ({}));
    },

    pickDirectory(options) {
      return pickDirectory(options);
    },

    // nodes (profile-save-nodes.js) is the editor's "Save on" choice, for a
    // spawn on another node; without it the profile saves on the page's.
    openProfileEditor(seed, onSaved, nodes = null) {
      return openProfileEditor(seed, { editExisting: false, onSaved, ...(nodes ? { nodes } : {}) });
    },

    openPermissions(options) {
      return openPermissions(options);
    },

    // Twin of openPermissions for the Role row's "Context…" button (TCL-597).
    // Both are pure launchers for a buffered editor the spawn draft owns; the
    // dialog hands the trim map back through the caller's onSave.
    openContextFeatures(options) {
      return openContextFeatures(options);
    },

    confirmAutoName(name) {
      return confirm({
        title: 'Auto-name this agent?',
        body: 'No name or description was given, so the agent will be auto-named from the first words of your initial message:',
        meta: `“${name}”`,
        okLabel: 'Auto-name & spawn',
      });
    },

    async complete(payload, draft) {
      const label = draft.name || (payload.conv_id ? shortID(payload.conv_id) : 'agent');
      if (draft.autoFocus && payload.focus_mode === 'browser' && payload.focus_ws) {
        const agent = payload.agent_id || payload.conv_id || payload.label || label;
        try {
          const pane = await openTerminalPane({
            ws: payload.focus_ws,
            label: payload.label || label,
            key: `window:${agent}`,
            hideConv: payload.conv_id || agent,
            agent,
            harness: draft.harness || '',
          });
          if (pane) notify(`spawned ${label} → ${draft.group} — opened in Terminals tab`);
          else notify(`spawned ${label} → ${draft.group} — terminal pane did not open`, true);
        } catch (cause) {
          notify(`spawned ${label} → ${draft.group} — terminal pane failed: ${cause?.message || cause}`, true);
        }
      } else {
        notify(`spawned ${label} → ${draft.group}${draft.autoFocus ? ' — opening terminal' : ''}`);
      }
      celebrateSlop();
      celebrateWizard();
      try {
        prefs?.setItem(`tclaude.dash.group.${draft.group}`, '1');
        if (prefs) revealGroupPath(draft.group, getSnapshot()?.groups, prefs);
        // The new agent must not land in the group's folded Agents (Both).
        if (prefs) setFusedSectionOpen(draft.group, 'agents', true, prefs);
      } catch (_) {}
      recordInteraction(draft.group);
      refresh();
    },
  });
}

function createNodeSpawnActions(dependencies, { node = '', nodeId = '', name = '' } = {}) {
  const {
    fetchImpl = fetch, notify = () => {}, refresh = () => {}, getSnapshot = () => null,
    openNodeTerminal = (options) => import('./remote-terminal.js').then((m) => m.openRemoteTerminal(options)),
  } = dependencies;
  const api = nodeFetch(node, { fetchImpl });
  const where = name || 'that node';
  let catalog = null;
  const loadCatalog = (force) => {
    if (!catalog || force) {
      catalog = Promise.all([
        jsonRequest(api, '/api/spawn-profiles'),
        jsonRequest(api, '/api/spawn-profile-default').catch(() => ({})),
      ]).then(([profiles, fallback]) => ({
        profiles: Array.isArray(profiles) ? profiles : [],
        fallback: typeof fallback?.name === 'string' ? fallback.name : '',
      }));
      catalog.catch(() => { catalog = null; });
    }
    return catalog;
  };
  let fallbackProfile = '';
  const base = createAgentSpawnActions({
    ...dependencies,
    fetchImpl: api,
    uploadImpl: (path, init) => nodeUpload(node, path, init, { fetchImpl, upload: dependencies.uploadTransport || trackedUpload }),
    async loadProfiles(force = false) {
      const { profiles, fallback } = await loadCatalog(force);
      fallbackProfile = fallback;
      return profiles;
    },
    async loadSandboxProfiles() {
      const value = await jsonRequest(api, '/api/sandbox-profiles');
      return Array.isArray(value) ? value : [];
    },
    getDashboardDefaultProfile: () => fallbackProfile,
    // Shares are this node's: an agent spawned on another node bills there.
    loadUsageShares: async () => [],
    pickDirectory: async () => ({ error: `Browse ${where}'s folders from its own dashboard, or type the path` }),
    refresh: () => { announceNodeChanged(node); refresh(); },
    getSnapshot: () => nodeSnapshot(node) || getSnapshot(),
  });
  return Object.freeze({
    ...base,
    async spawn(request) {
      // The node would open the new agent's window on its own desktop.
      const { auto_focus: _focus, auto_focus_web: _web, ...body } = request.body || {};
      // ?as=local: the node serves its regular spawn route to an
      // unrestricted peer (peer_actions.go servePeerSpawn).
      const url = `${request.url}${String(request.url).includes('?') ? '&' : '?'}as=local`;
      return base.spawn({ ...request, url, body: { ...body, auto_focus: false } });
    },
    async complete(payload, draft) {
      await base.complete(payload, { ...draft, autoFocus: false });
      const agent = payload?.agent_id || '';
      if (draft.autoFocus && agent && nodeId) {
        // The node lists a new agent's session within a few seconds.
        void openNodeTerminal({ instance: nodeId, agent, peerLabel: name, wait: 20000 });
      }
    },
  });
}
