// agent-spawn-remote.js — the regular spawn dialog aimed at a peer's group.
//
// Spawning into another node's group is a remote spawn request
// (POST /api/peer/{node}/groups/{g}/spawn, which this node forwards to the
// peer's POST /api/groups/{g}/spawn). The peer takes a brief, a name, a role, a profile from
// its selectable list, and — only where its grant says so — a directory,
// harness, model and effort. Everything else is the peer's own launch policy.
//
// So the dialog keeps every control in place and disables the ones this
// request cannot carry, each with the reason on hover. The lock is
// fail-closed: a control the remote mode does not name stays disabled, so a
// control added to the dialog later never silently sends local-only settings.
// GET spawn-options/{g} says what the peer allows; a peer without that route
// (an older tclaude) gets the request it understood before: brief, name and
// role.

const SPAWN_POLL_MS = 2000;
const SPAWN_POLL_LIMIT_MS = 30 * 60 * 1000;
export const REMOTE_BRIEF_MAX = 8 * 1024;
const SAFE_NODE = /^inst_[a-z0-9]{4,64}$/;
const OVERRIDES = Object.freeze(['cwd', 'harness', 'model', 'effort']);

// FIELD_CONTROLS maps the dialog's control ids to the request field they
// feed. Controls of fields that are always carried stay live; the override
// fields and the profile picker follow the peer's answer.
const FIELD_CONTROLS = Object.freeze({
  'agent-spawn-name': 'always',
  'agent-spawn-role': 'always',
  'agent-spawn-init-msg': 'always',
  'agent-spawn-cancel': 'always',
  'agent-spawn-submit': 'always',
  'agent-spawn-load-profile': 'profile',
  'agent-spawn-clear': 'profile',
  'agent-spawn-cwd': 'cwd',
  'agent-spawn-harness': 'harness',
  'agent-spawn-model': 'model',
  'agent-spawn-model-codex': 'model',
  'agent-spawn-model-custom': 'model',
  'agent-spawn-effort': 'effort',
});

function text(value) { return typeof value === 'string' ? value : ''; }

// remoteSpawnApi addresses one peer's spawn routes. nodeId '' is the per-node
// peer view, whose /api/* the page already reroutes to the peer.
export function remoteSpawnApi(nodeId, { fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  const api = nodeId ? `/api/peer/${encodeURIComponent(nodeId)}/` : '/api/';
  async function call(method, path, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetchImpl(api + path, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) {
      const error = new Error(data?.error || `HTTP ${res.status}`);
      error.status = res.status;
      error.code = data?.code || '';
      throw error;
    }
    return data;
  }
  return Object.freeze({
    options: (group) => call('GET', `spawn-options/${encodeURIComponent(group)}`),
    spawn: (group, body) => call('POST', `groups/${encodeURIComponent(group)}/spawn`, body),
    status: (id) => call('GET', `spawn-requests/${encodeURIComponent(id)}`),
  });
}

// normalizeRemoteSpawnOptions turns the peer's spawn-options answer into the
// dialog's allowances. null (no route on an older peer) allows only the
// always-carried fields.
export function normalizeRemoteSpawnOptions(raw) {
  const overrides = {};
  for (const field of OVERRIDES) overrides[field] = raw?.overrides?.[field] === true;
  const profiles = (Array.isArray(raw?.profiles) ? raw.profiles : [])
    .filter((p) => typeof p?.name === 'string' && p.name)
    .map((p) => Object.freeze({ name: p.name, harness: text(p.harness), model: text(p.model), effort: text(p.effort) }));
  return Object.freeze({
    known: !!raw,
    approval: raw?.approval === 'automatic' ? 'automatic' : raw?.approval === 'operator' ? 'operator' : '',
    overrides: Object.freeze(overrides),
    profiles: Object.freeze(profiles),
    harnesses: Object.freeze((Array.isArray(raw?.harnesses) ? raw.harnesses : []).filter((h) => typeof h === 'string' && h)),
    defaults: Object.freeze({ ...(raw?.defaults && typeof raw.defaults === 'object' ? raw.defaults : {}) }),
  });
}

// loadRemoteSpawnOptions reads what the peer lets this operator choose. A
// 404 is an older peer; any other failure is reported.
export async function loadRemoteSpawnOptions(api, group) {
  try {
    return normalizeRemoteSpawnOptions(await api.options(group));
  } catch (error) {
    if (error?.status === 404) return normalizeRemoteSpawnOptions(null);
    throw error;
  }
}

// remoteFieldAllowed says whether a control's field can ride on the request.
export function remoteFieldAllowed(field, options) {
  if (field === 'always') return true;
  if (field === 'profile') return (options?.profiles?.length || 0) > 0;
  return OVERRIDES.includes(field) && options?.overrides?.[field] === true;
}

const FIELD_WORDS = Object.freeze({ cwd: 'directory', harness: 'harness', model: 'model', effort: 'effort' });

// remoteLockReason is the hover text of a disabled control.
export function remoteLockReason(field, remote) {
  const node = remote?.node || 'the peer';
  const options = remote?.options;
  if (field === 'profile') {
    return options?.known
      ? `${node} lists no spawn profiles you may pick for this group — its own default profile applies`
      : `${node} runs an older tclaude that does not say which profiles you may pick — its own default profile applies`;
  }
  if (OVERRIDES.includes(field)) {
    const word = FIELD_WORDS[field];
    const fixed = text(options?.defaults?.[field]);
    if (!options?.known) return `${node} runs an older tclaude that does not take a ${word} from a remote spawn — its own launch policy applies`;
    if (options.approval !== 'automatic') return `${node} has not given you a spawn grant for this group — its operator decides the ${word} when approving`;
    return fixed
      ? `${node}'s grant does not let you choose the ${word} — it uses ${fixed}`
      : `${node} does not let you choose the ${word} — its grant or group default applies`;
  }
  if (field === 'browse') return `Browsing reads this machine's folders, not ${node}'s — type a path on ${node} instead`;
  return `${node} applies its own setting here (its group and profile defaults) — a remote spawn request does not carry it`;
}

function controlField(el) {
  if (el.id === 'agent-spawn-cwd-browse' || el.id === 'agent-spawn-wt-repo-browse') return 'browse';
  return FIELD_CONTROLS[el.id] || '';
}

// watchRemoteSpawnLocks keeps the lock on while the dialog lives: Preact
// rewrites `disabled` when a control's own prop changes, and child components
// (saved-prompt chips, help fields) re-render without the dialog, so any DOM
// change re-applies it. Applying is idempotent, so its own writes settle.
export function watchRemoteSpawnLocks(root, remote, { MutationObserverImpl = globalThis.MutationObserver } = {}) {
  if (!root || !remote) return () => {};
  applyRemoteSpawnLocks(root, remote);
  if (typeof MutationObserverImpl !== 'function') return () => {};
  const observer = new MutationObserverImpl(() => applyRemoteSpawnLocks(root, remote));
  observer.observe(root, { subtree: true, childList: true, attributes: true, attributeFilter: ['disabled', 'title'] });
  return () => observer.disconnect();
}

// applyRemoteSpawnLocks disables every control of the dialog that the remote
// request cannot carry and explains why. Help toggles stay usable.
export function applyRemoteSpawnLocks(root, remote) {
  if (!root || !remote) return;
  for (const el of root.querySelectorAll('input, select, textarea, button')) {
    if (el.classList.contains('spawn-field-help-trigger') || el.closest('.help-disclosure, [data-remote-spawn-free]')) continue;
    const field = controlField(el);
    if (field && field !== 'browse' && remoteFieldAllowed(field, remote.options)) continue;
    if (!el.disabled) el.disabled = true;
    const reason = remoteLockReason(field, remote);
    if (el.title !== reason) el.title = reason;
    el.closest('label, .cron-create-row')?.classList.add('remote-spawn-locked');
  }
}

// remoteSpawnBody maps the dialog's regular request onto a remote spawn
// request. An allowed override is sent only when the operator set it in this
// dialog (explicit), so an untouched prefill leaves the peer's policy in
// charge; a field the peer does not allow is never sent.
export function remoteSpawnBody(request, explicit, options) {
  const body = request?.body || {};
  const brief = text(body.initial_message).trim();
  if (!brief) throw new Error('write a brief in the initial message — the peer starts the agent with it');
  if (new TextEncoder().encode(brief).length > REMOTE_BRIEF_MAX) throw new Error(`the brief is over ${REMOTE_BRIEF_MAX} bytes, the remote spawn limit`);
  const out = { brief };
  if (text(body.name).trim()) out.name = text(body.name).trim();
  if (text(body.role).trim()) out.role = text(body.role).trim();
  if (text(body.profile) && remoteFieldAllowed('profile', options)) out.profile = body.profile;
  const said = new Set(explicit || []);
  for (const field of OVERRIDES) {
    const value = text(body[field]).trim();
    if (value && said.has(field) && remoteFieldAllowed(field, options)) out[field] = value;
  }
  return out;
}

// remoteSpawnIntro is the dialog's line about where the agent starts.
export function remoteSpawnIntro(remote, group) {
  const node = remote?.node || 'the peer';
  if (remote?.options?.approval === 'automatic') return `Spawns in ${group} on ${node}: ${node} launches it under the grant it gave you; disabled fields are ${node}'s to decide (hover for why).`;
  if (remote?.options?.approval === 'operator') return `Asks ${node} for an agent in ${group}: no spawn grant covers it, so ${node}'s operator approves it; disabled fields are ${node}'s to decide (hover for why).`;
  return `Asks ${node} for an agent in ${group}; disabled fields are ${node}'s to decide (hover for why).`;
}

// remoteSpawnContext is the launch context the dialog opens with: the peer's
// own groups and harnesses, never this node's.
export function remoteSpawnContext(snapshot, options) {
  let harnesses = Array.isArray(snapshot?.harnesses) ? snapshot.harnesses : [];
  if (!harnesses.length && options?.harnesses?.length) harnesses = options.harnesses.map((name) => ({ name }));
  return {
    groups: Array.isArray(snapshot?.groups) ? snapshot.groups : [],
    harnesses,
    roles: [],
    sandbox_impl: {},
    sandbox_profiles: [],
    user_default_model: '',
    default_terminal: 'native',
    spawn_name_normalize: snapshot?.spawn_name_normalize !== false,
  };
}

function finalStatus(status) {
  return status && status !== 'pending' && status !== 'launching';
}

// createRemoteSpawnActions wraps the dialog's local actions for a remote
// spawn: nothing reads or writes this node's state (worktrees, sandbox
// previews, uploads, directory picker), and submit sends the remote request.
export function createRemoteSpawnActions(base, remote, { notify = () => {}, timers = globalThis, now = () => Date.now() } = {}) {
  const node = remote.node || 'the peer';
  const api = remote.api;
  const watch = (request, startedAt) => {
    timers.setTimeout(() => {
      api.status(request.id).then((next) => {
        if (finalStatus(next?.status)) {
          const failed = next.status !== 'approved';
          notify(`${node} spawn request #${next.id}: ${next.status}${next.result_agent ? ` — agent ${next.result_agent}` : ''}${next.reason ? ` — ${next.reason}` : ''}`, failed);
        } else if (now() - startedAt < SPAWN_POLL_LIMIT_MS) {
          watch(next || request, startedAt);
        }
      }).catch(() => { if (now() - startedAt < SPAWN_POLL_LIMIT_MS) watch(request, startedAt); });
    }, SPAWN_POLL_MS);
  };
  return Object.freeze({
    ...base,
    autoFocusDefault: () => false,
    rememberedEffort: () => '',
    rememberLaunchPreferences() {},
    dashboardDefaultProfile: () => '',
    async loadProfiles() { return [...remote.options.profiles]; },
    async loadWorktrees(repo) {
      return { repo: text(repo).trim(), isRepo: false, empty: true, hasCommits: true, repoRoot: '', worktrees: [], branches: [], defaultBranch: '', subRepos: [] };
    },
    async resolveWorktree() { return { path: '', branch: '' }; },
    async loadLaunchDefaults() { return {}; },
    async loadSandboxPolicy() { return { profiles: [], selected: '', preview: `${node} applies its own sandbox policy.` }; },
    async loadUnsandboxedAutonomy() { return { info: [], warnings: [], sandboxState: '', sandboxSource: '' }; },
    async uploadAttachments(attachments) {
      if (attachments?.length) throw new Error(`attachments cannot be sent to ${node} with a spawn request`);
      return [];
    },
    async reportTiming() {},
    async pickDirectory() { return {}; },
    async spawn(request) {
      return api.spawn(remote.group, remoteSpawnBody(request, request.explicit, remote.options));
    },
    async complete(payload) {
      const id = payload?.id;
      notify(payload?.status === 'pending' && remote.options.approval !== 'automatic'
        ? `Spawn request #${id} sent to ${node} — waiting for its operator`
        : `Spawn request #${id} sent to ${node}`);
      if (id && !finalStatus(payload?.status)) watch(payload, now());
    },
  });
}

// prepareRemoteSpawn gathers what the dialog needs to open against a peer's
// group: the peer's answer about allowances and its snapshot.
export async function prepareRemoteSpawn({ nodeId = '', node = '', group, snapshot = null, fetchImpl } = {}) {
  if (nodeId && !SAFE_NODE.test(nodeId)) throw new Error('unknown node');
  const api = remoteSpawnApi(nodeId, fetchImpl ? { fetchImpl } : {});
  const options = await loadRemoteSpawnOptions(api, group);
  return Object.freeze({ nodeId, node: node || (nodeId ? nodeId.slice(0, 13) : 'the peer'), group, api, options, snapshot });
}
