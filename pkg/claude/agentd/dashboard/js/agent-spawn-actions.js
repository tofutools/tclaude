import { composeSandboxProfilePolicy } from './sandbox-profile-preview.js';
import { BREAK_GLASS_WARNING, describeBreakGlassEntries } from './sandbox-break-glass.js';
import { WT_NEW } from './agent-spawn-model.js';

const EFFORT_KEY = 'tclaude.dash.spawn.modelEffort';
const AUTOFOCUS_KEY = 'tclaude.dash.spawn.autofocus';

async function responseText(response) {
  try { return await response.text(); } catch (_) { return ''; }
}

// Failures carry the daemon's structured {"error", "code"} body; status and
// typed code stay on the thrown Error so submit-side recovery can key off
// break_glass_acknowledgement_required rather than message text.
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

async function jsonRequest(fetchImpl, path) {
  const response = await fetchImpl(path, { credentials: 'same-origin' });
  if (!response.ok) throw await responseError(response);
  return response.json().catch(() => ({}));
}

function readEffortMap(prefs) {
  try {
    const value = JSON.parse(prefs.getItem(EFFORT_KEY));
    return value && typeof value === 'object' ? value : {};
  } catch (_) {
    return {};
  }
}

export function createAgentSpawnActions({
  fetchImpl = fetch,
  prefs,
  loadProfiles,
  loadSandboxProfiles,
  getDashboardDefaultProfile = () => '',
  pickDirectory,
  openProfileEditor,
  openPermissions,
  confirm,
  notify = () => {},
  refresh = () => {},
  openTerminal = () => {},
  celebrateSlop = () => {},
  celebrateWizard = () => {},
  recordInteraction = () => {},
  shortID = (value) => value,
} = {}) {
  return Object.freeze({
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

    async resolveWorktree(draft, worktrees) {
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
      const response = await fetchImpl('/api/worktrees', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          repo: worktrees?.repoRoot || String(draft.wtRepo || '').trim(),
          branch,
          from_branch: draft.worktreeBase || '',
        }),
      });
      if (!response.ok) throw new Error((await responseText(response)) || `HTTP ${response.status}`);
      const payload = await response.json();
      return { path: payload.path || '', branch: payload.branch || branch };
    },

    async loadSandboxPolicy(groupName, selected = '') {
      const profiles = await loadSandboxProfiles();
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
      return {
        profiles,
        selected: byName[selected] ? selected : '',
        preview: policy.text,
        // Break-glass can arrive from ANY layer (global or group assignment,
        // not just the explicit pick), so the spawn gate keys off the resolved
        // composition, mirroring the daemon's own acknowledgement rule.
        breakGlass: policy.breakGlass,
      };
    },

    async uploadAttachments(attachments) {
      if (!attachments?.length) return [];
      const form = new FormData();
      for (const attachment of attachments) {
        form.append('file', attachment.file, attachment.name);
      }
      const response = await fetchImpl('/api/spawn-attachments', {
        method: 'POST', credentials: 'same-origin', body: form,
      });
      if (!response.ok) {
        throw new Error(`attachment upload failed: ${(await responseText(response)) || `HTTP ${response.status}`}`);
      }
      const payload = await response.json();
      return (payload.files || []).map((file) => file.path);
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

    openProfileEditor(seed, onSaved) {
      return openProfileEditor(seed, { editExisting: false, onSaved });
    },

    openPermissions(options) {
      return openPermissions(options);
    },

    confirmAutoName(name) {
      return confirm({
        title: 'Auto-name this agent?',
        body: 'No name or description was given, so the agent will be auto-named from the first words of your initial message:',
        meta: `“${name}”`,
        okLabel: 'Auto-name & spawn',
      });
    },

    confirmBreakGlassSpawn(entries) {
      return confirm({
        title: '\u{1f6a8} Spawn with break-glass protected access?',
        body: `The resolved sandbox policy for this launch carries break-glass protected-path access: ${describeBreakGlassEntries(entries)}. ${BREAK_GLASS_WARNING}`,
        okLabel: 'I understand the risk — spawn',
      });
    },

    complete(payload, draft) {
      const label = draft.name || (payload.conv_id ? shortID(payload.conv_id) : 'agent');
      if (draft.autoFocus && payload.focus_mode === 'browser' && payload.focus_ws) {
        openTerminal({
          wsPath: payload.focus_ws,
          label: payload.label || label,
          hideConv: payload.conv_id || null,
        });
        notify(`spawned ${label} → ${draft.group} — opened in-browser terminal`);
      } else {
        notify(`spawned ${label} → ${draft.group}${draft.autoFocus ? ' — opening terminal' : ''}`);
      }
      celebrateSlop();
      celebrateWizard();
      try { prefs?.setItem(`tclaude.dash.group.${draft.group}`, '1'); } catch (_) {}
      recordInteraction(draft.group);
      refresh();
    },
  });
}
