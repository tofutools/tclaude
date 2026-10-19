// agent-spawn-remote.js — the regular spawn dialog aimed at a peer's group.
//
// Spawning into another node's group is a remote spawn request
// (POST /api/peer/{node}/groups/{g}/spawn, which this node forwards to the
// peer's POST /api/groups/{g}/spawn). The peer takes a brief, a name, a role, a profile from
// its selectable list, and — only where its grants say so — every other
// launch setting of the dialog: the directory, harness, model and effort,
// roles and sandbox profile, and the rest as launch keys (LAUNCH_KEYS).
// Unrestricted trust opens all of them, so the dialog works as a local one.
// What a grant does not open is the peer's own launch policy.
//
// So the dialog keeps every control in place and disables the ones this
// request cannot carry, each with the reason on hover. The lock is
// fail-closed: a control the remote mode does not name stays disabled, so a
// control added to the dialog later never silently sends local-only settings.
// GET spawn-options/{g} says what the peer allows; a peer without that route
// (an older tclaude) gets the request it understood before: brief, name and
// role.
//
// When the peer lends its catalogs (node.spawn.read; unrestricted trust holds
// it), the dialog reads them through the peer view and pre-fills exactly what
// the peer's own dialog would: its default profile, roles, prompts, sandbox
// and launch defaults, and the grant's pinned settings. Locked controls show
// those values. Profiles and saved prompts come from both libraries — the
// peer's and this node's, never a third node's — under one source filter. A
// peer profile rides by name when the grant lets this operator pick it; a
// local profile fills only the fields the request carries. Saved prompts the
// peer applies itself (its profile and group defaults) stay locked; any
// other prompt is written into the brief.

import { createAgentSpawnActions } from './agent-spawn-actions.js';

const SPAWN_POLL_MS = 2000;
const SPAWN_POLL_LIMIT_MS = 30 * 60 * 1000;
export const REMOTE_BRIEF_MAX = 8 * 1024;
const SAFE_NODE = /^inst_[a-z0-9]{4,64}$/;
const OVERRIDES = Object.freeze(['cwd', 'harness', 'model', 'effort']);
// FIELDS are every launch setting a peer's grants may open (its
// groups.members.spawn.* grants): the string overrides above, the profile
// pick, the role set and an explicit sandbox profile, then the fields whose
// settings ride as launch keys.
const LAUNCH_FIELDS = Object.freeze(['permissions', 'environment', 'owner', 'features', 'prompts', 'worktree']);
const FIELDS = Object.freeze([...OVERRIDES, 'profile', 'roles', 'sandbox', ...LAUNCH_FIELDS]);
// LAUNCH_KEYS are the spawn request keys a remote spawn carries as launch
// settings (agentd's fedSpawnLaunchKeys): the field that opens each, and the
// dialog draft keys whose change by the operator sends it. Prompts and
// worktrees are mapped on their own (remoteSpawnBody).
const LAUNCH_KEYS = Object.freeze({
  approval: ['permissions', ['approval']],
  tools: ['permissions', ['tools']],
  auto_review: ['permissions', ['approvalReviewer']],
  ask_user_question_timeout: ['permissions', ['askTimeout']],
  trust_dir: ['permissions', ['trustDir', 'trustDirSpecified']],
  permission_overrides: ['permissions', ['permissionOverrides']],
  sandbox: ['permissions', ['sandbox']],
  sandbox_implementation: ['permissions', ['sandboxImpl']],
  allow_unenforced_sandbox: ['permissions', ['allowUnenforcedSandbox']],
  omit_sandbox_profiles: ['permissions', ['sandboxProfile', 'sandboxImpl', 'sandbox']],
  environment: ['environment', ['environment']],
  is_owner: ['owner', ['owner']],
  include_group_context: ['features', ['includeGroupContext']],
  context_features: ['features', ['contextFeatures']],
  remote_control: ['features', ['remoteControl']],
  auto_memory: ['features', ['autoMemory']],
  peer_messaging: ['features', ['peerMessaging']],
  copilot_api: ['features', ['copilotAPI']],
  codex_app_server: ['features', ['codexAppServer']],
  fast_mode: ['features', ['fastMode']],
  ssh_workaround: ['features', ['sshWorkaround']],
  auto_compact_window: ['features', ['autoCompactWindow']],
  context_window_max: ['features', ['contextWindowMax']],
  descr: ['features', ['descr']],
  task_ref_url: ['features', ['task']],
});
// The dialog's draft key the operator sets for each list-like field.
const FIELD_DRAFT_KEYS = Object.freeze({ roles: 'roleRefs', sandbox: 'sandboxProfile' });
const SANDBOX_PROFILE_NONE = '/omit-all-tclaude-sandbox-profiles';
// A ref or profile handle with this prefix names this node's library item
// rather than the target's.
export const LOCAL_REF = 'local:';

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
  'agent-spawn-role-add': 'roles',
  'agent-spawn-sandbox-profile': 'sandbox',
  'agent-spawn-approval': 'permissions',
  'agent-spawn-approval-reviewer': 'permissions',
  'agent-spawn-tools': 'permissions',
  'agent-spawn-ask-timeout': 'permissions',
  'agent-spawn-trust-dir': 'permissions',
  'agent-spawn-perms': 'permissions',
  'agent-spawn-sandbox': 'permissions',
  'agent-spawn-sandbox-impl': 'permissions',
  'agent-spawn-allow-unenforced-sandbox': 'permissions',
  'agent-spawn-owner': 'owner',
  'agent-spawn-context-features': 'features',
  'agent-spawn-descr': 'features',
  'agent-spawn-task': 'features',
  'agent-spawn-remote-control': 'features',
  'agent-spawn-auto-memory': 'features',
  'agent-spawn-peer-messaging': 'features',
  'agent-spawn-copilot-api': 'features',
  'agent-spawn-codex-app-server': 'features',
  'agent-spawn-fast-mode': 'features',
  'agent-spawn-ssh-workaround': 'features',
  'agent-spawn-auto-compact-window': 'features',
  'agent-spawn-context-window-max': 'features',
  'agent-spawn-group-context': 'features',
  'agent-spawn-wt-repo': 'worktree',
  'agent-spawn-worktree': 'worktree',
  'agent-spawn-wt-sync': 'worktree',
  'agent-spawn-wt-fetch': 'worktree',
  'agent-spawn-wt-branch': 'worktree',
  'agent-spawn-wt-base': 'worktree',
});

function text(value) { return typeof value === 'string' ? value : ''; }
function list(value) { return Array.isArray(value) ? value : []; }

export function isLocalRef(value) { return text(value).startsWith(LOCAL_REF); }

// localFetchImpl reaches this node's own API, also from a peer's page.
function localFetchImpl() {
  const local = globalThis.__tclaudeRemoteNode?.localFetch;
  return typeof local === 'function' ? local : (...a) => globalThis.fetch(...a);
}

// peerFetchFor addresses the target's /api/ the way remoteSpawnApi does:
// through /api/peer/{node}/, or on a per-node peer view (nodeId '') through
// the page's own rerouting.
export function peerFetchFor(nodeId, fetchImpl = (...a) => globalThis.fetch(...a)) {
  if (!nodeId) return fetchImpl;
  const prefix = `/api/peer/${encodeURIComponent(nodeId)}/`;
  return (path, init) => fetchImpl(String(path).replace(/^\/api\//, prefix), init);
}

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
  for (const field of FIELDS) overrides[field] = raw?.overrides?.[field] === true;
  // The fields the peer answered about at all: an older one lacks the later.
  const reported = FIELDS.filter((field) => typeof raw?.overrides?.[field] === 'boolean');
  const profiles = (Array.isArray(raw?.profiles) ? raw.profiles : [])
    .filter((p) => typeof p?.name === 'string' && p.name)
    .map((p) => Object.freeze({ name: p.name, harness: text(p.harness), model: text(p.model), effort: text(p.effort) }));
  return Object.freeze({
    known: !!raw,
    approval: raw?.approval === 'automatic' ? 'automatic' : raw?.approval === 'operator' ? 'operator' : '',
    overrides: Object.freeze(overrides),
    reported: Object.freeze(reported),
    profiles: Object.freeze(profiles),
    harnesses: Object.freeze((Array.isArray(raw?.harnesses) ? raw.harnesses : []).filter((h) => typeof h === 'string' && h)),
    defaults: Object.freeze({ ...(raw?.defaults && typeof raw.defaults === 'object' ? raw.defaults : {}) }),
    catalog: raw?.catalog === true,
    defaultPrompts: Object.freeze(list(raw?.default_prompts).filter((p) => typeof p === 'string' && p)),
  });
}

// loadRemoteSpawnOptions reads what the peer lets this operator choose. An
// older peer has no such route and answers it as a local-only path; any other
// failure (including this group being invisible: 404) is reported.
export async function loadRemoteSpawnOptions(api, group) {
  try {
    return normalizeRemoteSpawnOptions(await api.options(group));
  } catch (error) {
    if (error?.status === 403 && error?.code === 'peer_view_local_only') return normalizeRemoteSpawnOptions(null);
    throw error;
  }
}

async function readJSON(fetchImpl, path) {
  const res = await fetchImpl(path, { credentials: 'same-origin' });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

const settle = (promise, fallback) => promise.then((value) => value, () => fallback);

// loadRemoteSpawnCatalogs reads the two libraries the dialog offers: the
// target's (only when it lends them) and this node's. A library that fails to
// load is empty rather than fatal: the spawn request itself needs neither.
export async function loadRemoteSpawnCatalogs({ options, peerFetch, localFetch = localFetchImpl() }) {
  const local = Promise.all([
    settle(readJSON(localFetch, '/api/spawn-profiles').then(list), []),
    settle(readJSON(localFetch, '/api/saved-prompts').then(list), []),
  ]);
  const target = options?.catalog ? Promise.all([
    settle(readJSON(peerFetch, '/api/spawn-profiles').then(list), []),
    settle(readJSON(peerFetch, '/api/spawn-profile-default').then((v) => text(v?.name)), ''),
    settle(readJSON(peerFetch, '/api/saved-prompts').then(list), []),
    settle(readJSON(peerFetch, '/api/roles').then(list), []),
    settle(readJSON(peerFetch, '/api/sandbox-profiles').then(list), []),
    settle(readJSON(peerFetch, '/api/claude-settings/default-model').then((v) => text(v?.model)), ''),
    settle(readJSON(peerFetch, '/api/spawn-launch-catalog'), null),
  ]) : Promise.resolve(null);
  const [[localProfiles, localPrompts], peer] = await Promise.all([local, target]);
  return Object.freeze({
    target: peer ? Object.freeze({
      profiles: peer[0], defaultProfile: peer[1], prompts: peer[2], roles: peer[3], sandboxProfiles: peer[4], defaultModel: peer[5],
      // The peer's harnesses (models, modes) and sandbox implementations; an
      // older peer without the route leaves the snapshot's names.
      harnesses: list(peer[6]?.harnesses), sandboxImpl: peer[6]?.sandbox_impl && typeof peer[6].sandbox_impl === 'object' ? peer[6].sandbox_impl : null,
    }) : null,
    local: Object.freeze({ profiles: localProfiles.filter((p) => p?.name && !p.disabled), prompts: localPrompts }),
  });
}

// remoteSpawnProfiles is the profile picker's list: the target's profiles
// (full, so pre-fill shows what the peer applies; only the grant's selectable
// ones may be picked), then this node's under local: handles.
export function remoteSpawnProfiles(remote) {
  const selectable = new Set((remote?.options?.profiles || []).map((p) => p.name));
  const target = remote?.catalogs?.target
    ? remote.catalogs.target.profiles
    : remote?.options?.profiles || [];
  const out = target.filter((p) => p?.name).map((p) => ({
    ...p, remoteSource: 'target', remoteSelectable: selectable.has(p.name),
  }));
  for (const p of remote?.catalogs?.local?.profiles || []) {
    out.push({ ...p, name: LOCAL_REF + p.name, aliases: [], localName: p.name, remoteSource: 'local', remoteSelectable: true });
  }
  return out;
}

// remoteSpawnPrompts lists both prompt libraries, the local one under
// local: refs.
export function remoteSpawnPrompts(remote) {
  const target = (remote?.catalogs?.target?.prompts || []).filter((p) => p?.name)
    .map((p) => ({ ...p, source: 'target' }));
  const local = (remote?.catalogs?.local?.prompts || []).filter((p) => p?.name)
    .map((p) => ({ ...p, name: LOCAL_REF + p.name, label: p.name, source: 'local' }));
  return [...target, ...local];
}

// remotePromptSetOpen says whether the request may name the whole prompt set
// (groups.members.spawn.prompts). It also needs the peer's prompt library:
// without it the dialog cannot show the peer's own defaults, and sending a
// set would silently drop them.
export function remotePromptSetOpen(remote) {
  return remote?.options?.overrides?.prompts === true && !!remote?.catalogs?.target;
}

// remotePromptBodies is the text of the prompts the brief carries: every
// selected prompt the peer does not already apply itself.
export function remotePromptBodies(refs, peerApplied, remote) {
  const applied = new Set(peerApplied || []);
  const byName = new Map(remoteSpawnPrompts(remote).map((p) => [p.name, p]));
  const out = [];
  for (const ref of refs || []) {
    if (applied.has(ref)) continue;
    const prompt = byName.get(ref);
    if (!prompt) throw new Error(`saved prompt ${ref} is not in a library this dialog can read`);
    if (text(prompt.body).trim()) out.push(text(prompt.body).trim());
  }
  return out;
}

// remoteFieldAllowed says whether a control's field can ride on the request.
// The profile picker is open when a peer profile can be picked, or a local
// profile can fill some field the request carries.
export function remoteFieldAllowed(field, options, catalogs = null) {
  // Saved prompt chips always work: this node's prompts go into the brief,
  // and the peer's ride by name under groups.members.spawn.prompts.
  if (field === 'always' || field === 'prompts') return true;
  // A worktree launches in its own directory, so it needs the directory too.
  if (field === 'worktree') return options?.overrides?.worktree === true && options?.overrides?.cwd === true;
  if (LAUNCH_FIELDS.includes(field)) return options?.overrides?.[field] === true;
  // Roles and sandbox profiles are picked from the peer's own lists, which
  // only its catalogs (node.spawn.read) show.
  if (field === 'roles' || field === 'sandbox') return options?.overrides?.[field] === true && !!catalogs?.target;
  if (field === 'profile') {
    return (options?.profiles?.length || 0) > 0
      || ((catalogs?.local?.profiles?.length || 0) > 0 && OVERRIDES.some((f) => options?.overrides?.[f] === true));
  }
  return OVERRIDES.includes(field) && options?.overrides?.[field] === true;
}

// remoteSpawnPins lays the grant's pinned settings (the peer's defaults for
// this requester) over a pre-filled draft, for the fields the operator has
// not set: what the peer launches unless the request overrides it.
export function remoteSpawnPins(draft, options, operatorSet = new Set()) {
  const pins = options?.defaults || {};
  const next = { ...draft };
  if (text(pins.cwd) && !operatorSet.has('cwd') && !text(draft.cwd).trim()) next.cwd = pins.cwd;
  if (text(pins.harness) && !operatorSet.has('harness')) next.harness = pins.harness;
  if (text(pins.model) && !operatorSet.has('model')) { next.model = pins.model; next.customModel = false; }
  return next;
}

// remoteLaunchFields groups a request's launch settings by the field that
// opens them, in FIELDS order: [{ field, label, keys }].
export function remoteLaunchFields(launch) {
  const keys = Object.keys(launch && typeof launch === 'object' ? launch : {});
  const fieldOf = (key) => (key === 'prompt_refs' ? 'prompts' : key.startsWith('worktree_') ? 'worktree' : LAUNCH_KEYS[key]?.[0] || '');
  return LAUNCH_FIELDS.map((field) => ({ field, label: LAUNCH_WORDS[field], keys: keys.filter((key) => fieldOf(key) === field).sort() }))
    .filter((group) => group.keys.length);
}

const FIELD_WORDS = Object.freeze({ cwd: 'directory', harness: 'harness', model: 'model', effort: 'effort' });
const LAUNCH_WORDS = Object.freeze({
  permissions: 'permission and sandbox modes', environment: 'environment', owner: 'group ownership',
  features: 'harness features, context and description', prompts: 'saved prompts', worktree: 'worktree',
});

// remoteLockReason is the hover text of a disabled control.
export function remoteLockReason(field, remote) {
  const node = remote?.node || 'the peer';
  const options = remote?.options;
  if (field === 'profile') {
    return options?.known
      ? `${node} lists no spawn profiles you may pick for this group — its own default profile applies`
      : `${node} runs an older tclaude that does not say which profiles you may pick — its own default profile applies`;
  }
  if (field === 'roles' || field === 'sandbox') {
    const word = field === 'roles' ? 'roles' : 'sandbox profile';
    if (!options?.known) return `${node} runs an older tclaude that does not take the ${word} from a remote spawn — its own defaults apply`;
    if (options.approval !== 'automatic') return `${node} has not given you a spawn grant for this group — its operator decides the ${word} when approving`;
    if (options.overrides?.[field] === true) return `${node} lets you choose the ${word}, but does not share its list with you (node.spawn.read) — its profile and group defaults apply`;
    return `${node} does not let you choose the ${word} (groups.members.spawn.${field}) — its profile and group defaults apply`;
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
  if (LAUNCH_FIELDS.includes(field)) {
    const word = LAUNCH_WORDS[field];
    if (!options?.known) return `${node} runs an older tclaude that does not take the ${word} from a remote spawn — its own launch policy applies`;
    if (options.approval !== 'automatic') return `${node} has not given you a spawn grant for this group — its operator decides the ${word} when approving`;
    if (!options.reported?.includes(field)) return `${node} runs an older tclaude that does not take the ${word} from a remote spawn — its own launch policy applies`;
    if (options.overrides?.[field] === true && field === 'worktree') return `${node} lets you launch in a worktree, but not choose the directory (groups.members.spawn.cwd)`;
    return `${node} does not let you choose the ${word} (groups.members.spawn.${field}) — its profile and group defaults apply`;
  }
  if (field === 'focus') return `The new agent runs on ${node}; there is no pane on this node to focus`;
  if (field === 'attachments') return `Attachments cannot be sent to ${node} with a spawn request`;
  if (field === 'queue') return `Queued spawns wait on this node; a spawn on ${node} is sent as a request now`;
  if (field === 'browse') return `Browsing reads this machine's folders, not ${node}'s — type a path on ${node} instead`;
  return options?.catalog
    ? `${node} applies this setting itself (shown: what it will use) — a remote spawn request does not carry it`
    : `${node} applies its own setting here (its group and profile defaults) — a remote spawn request does not carry it`;
}

function controlField(el) {
  if (el.id === 'agent-spawn-cwd-browse' || el.id === 'agent-spawn-wt-repo-browse') return 'browse';
  if (el.id === 'agent-spawn-queue') return 'queue';
  if (el.id === 'agent-spawn-focus') return 'focus';
  if (el.id === 'agent-spawn-attach-btn' || el.id === 'agent-spawn-attach-input') return 'attachments';
  return FIELD_CONTROLS[el.id] || el.closest?.('[data-remote-spawn-field]')?.dataset?.remoteSpawnField || '';
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
    if (field && field !== 'browse' && remoteFieldAllowed(field, remote.options, remote.catalogs)) continue;
    if (!el.disabled) el.disabled = true;
    const reason = remoteLockReason(field, remote);
    if (el.title !== reason) el.title = reason;
    el.closest('label, .cron-create-row')?.classList.add('remote-spawn-locked');
  }
}

// remoteSpawnBody maps the dialog's regular request onto a remote spawn
// request. An allowed override is sent only when the operator set it in this
// dialog (explicit), so an untouched prefill leaves the peer's policy in
// charge; a field the peer does not allow is never sent. The same holds for
// the profile: a pre-filled default is the peer's to apply, and only a peer
// profile the operator picked and may pick is named. Prompts (bodies) open
// the brief, as saved prompts do in a local spawn.
export function remoteSpawnBody(request, explicit, options, prompts = [], { promptRefs = null, worktree = null } = {}) {
  const body = request?.body || {};
  const said = new Set(explicit || []);
  const brief = [...prompts, text(body.initial_message).trim()].filter(Boolean).join('\n\n');
  if (!brief) throw new Error('write a brief in the initial message — the peer starts the agent with it');
  if (new TextEncoder().encode(brief).length > REMOTE_BRIEF_MAX) throw new Error(`the brief is over ${REMOTE_BRIEF_MAX} bytes, the remote spawn limit`);
  const out = { brief };
  if (text(body.name).trim()) out.name = text(body.name).trim();
  if (text(body.role).trim()) out.role = text(body.role).trim();
  const profile = text(body.profile);
  if (profile && !isLocalRef(profile) && said.has('profile') && (options?.profiles || []).some((p) => p.name === profile)) out.profile = profile;
  for (const field of OVERRIDES) {
    const value = text(body[field]).trim();
    if (value && said.has(field) && remoteFieldAllowed(field, options)) out[field] = value;
  }
  const roles = list(body.role_refs).filter((r) => typeof r === 'string' && r);
  if (said.has(FIELD_DRAFT_KEYS.roles) && options?.overrides?.roles === true) {
    // There is no "no roles" on the wire: the peer would apply its profile
    // and group roles, not the empty set shown here.
    if (!roles.length) throw new Error('a remote spawn cannot remove every role — keep one, or Clear to the peer\'s defaults');
    out.roles = roles;
  }
  const sandbox = text(body.sandbox_profile);
  if (sandbox && sandbox !== SANDBOX_PROFILE_NONE && said.has(FIELD_DRAFT_KEYS.sandbox) && options?.overrides?.sandbox === true) out.sandbox_profile = sandbox;
  const launch = {};
  for (const [key, [field, draftKeys]] of Object.entries(LAUNCH_KEYS)) {
    if (body[key] === undefined || !draftKeys.some((k) => said.has(k)) || !remoteFieldAllowed(field, options)) continue;
    if (typeof body[key] === 'string' && !body[key].trim() && key !== 'descr') continue;
    launch[key] = body[key];
  }
  // The peer's saved prompts ride by name when the grant opens the prompt set
  // (it then replaces the peer's profile and group defaults); this node's are
  // in the brief either way.
  if (promptRefs && said.has('promptRefs') && options?.overrides?.prompts === true) launch.prompt_refs = [...promptRefs];
  // A worktree the dialog created on the peer: the launch runs in it.
  if (worktree?.path && remoteFieldAllowed('worktree', options)) {
    out.cwd = text(body.cwd).trim();
    if (text(body.worktree_path)) {
      launch.worktree_path = body.worktree_path;
      launch.worktree_branch = text(body.worktree_branch);
    }
  }
  // The choice only: this node's peer proxy adds the token itself.
  if (body.claude_subscription === true) out.claude_subscription = true;
  if (Object.keys(launch).length) out.launch = launch;
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
// own groups, harnesses and (when it lends its catalogs) roles, sandbox
// profiles and default model, never this node's.
export function remoteSpawnContext(snapshot, options, catalogs = null, group = '') {
  const target = catalogs?.target || null;
  // The peer-view snapshot may withhold a group's defaults from a restricted
  // peer; spawn-options names them to a catalog holder, so the pre-fill
  // starts from the profile and prompts the peer really applies.
  const withDefaults = (g) => (options?.catalog && g?.name === group
    ? { ...g, default_profile: text(options.defaults?.group_profile), default_prompts: [...(options.defaultPrompts || [])] }
    : g);
  let harnesses = target?.harnesses?.length ? target.harnesses : Array.isArray(snapshot?.harnesses) ? snapshot.harnesses : [];
  if (!harnesses.length && options?.harnesses?.length) harnesses = options.harnesses.map((name) => ({ name }));
  return {
    groups: Array.isArray(snapshot?.groups) ? snapshot.groups.map(withDefaults) : [],
    harnesses,
    roles: target ? target.roles : [],
    sandbox_impl: target?.sandboxImpl || {},
    sandbox_profiles: target ? target.sandboxProfiles : [],
    user_default_model: target ? target.defaultModel : '',
    default_terminal: 'native',
    spawn_name_normalize: snapshot?.spawn_name_normalize !== false,
  };
}

function finalStatus(status) {
  return status && status !== 'pending' && status !== 'launching';
}

// createRemoteSpawnActions wraps the dialog's local actions for a remote
// spawn: nothing reads or writes this node's state (worktrees, uploads,
// directory picker, queues), previews read the peer's own when it lends its
// catalogs, worktrees are the peer's under its worktree grant, and submit
// sends the remote request.
export function createRemoteSpawnActions(base, remote, { notify = () => {}, timers = globalThis, now = () => Date.now(), peerFetch = remote.peerFetch || peerFetchFor(remote.nodeId) } = {}) {
  const node = remote.node || 'the peer';
  const api = remote.api;
  const target = remote.catalogs?.target || null;
  const peer = target ? createAgentSpawnActions({ fetchImpl: peerFetch, loadSandboxProfiles: async () => target.sandboxProfiles }) : null;
  // Worktrees are listed and created on the peer, as its own dialog does,
  // when the grant opens them.
  const worktrees = remoteFieldAllowed('worktree', remote.options) ? createAgentSpawnActions({ fetchImpl: peerFetch }) : null;
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
  const notQueued = async () => { throw new Error(`Queued spawns wait on this node; a spawn on ${node} is sent as a request now`); };
  return Object.freeze({
    ...base,
    // Queues are this node's: a remote dialog never saves into or removes
    // from one, whatever path reaches these.
    saveQueued: notQueued,
    deleteQueued: notQueued,
    autoFocusDefault: () => false,
    rememberedEffort: () => '',
    rememberLaunchPreferences() {},
    dashboardDefaultProfile: () => target?.defaultProfile || '',
    async loadProfiles() { return remoteSpawnProfiles(remote); },
    async loadWorktrees(repo) {
      if (worktrees) return worktrees.loadWorktrees(repo);
      return { repo: text(repo).trim(), isRepo: false, empty: true, hasCommits: true, repoRoot: '', worktrees: [], branches: [], defaultBranch: '', subRepos: [] };
    },
    async resolveWorktree(...args) {
      return worktrees ? worktrees.resolveWorktree(...args) : { path: '', branch: '' };
    },
    async loadLaunchDefaults(...args) { return peer ? peer.loadLaunchDefaults(...args) : {}; },
    async loadSandboxPolicy(...args) {
      if (!peer) return { profiles: [], selected: '', preview: `${node} applies its own sandbox policy.` };
      const policy = await peer.loadSandboxPolicy(...args);
      return { ...policy, preview: `${node} applies: ${policy.preview}` };
    },
    async loadUnsandboxedAutonomy(input) {
      return peer ? peer.loadUnsandboxedAutonomy(input) : { info: [], warnings: [], sandboxState: '', sandboxSource: '' };
    },
    async uploadAttachments(attachments) {
      if (attachments?.length) throw new Error(`attachments cannot be sent to ${node} with a spawn request`);
      return [];
    },
    async reportTiming() {},
    async pickDirectory() { return {}; },
    async spawn(request) {
      // The worktree the dialog resolved for this submit (a retry reuses it).
      const used = request.worktree || null;
      // With the prompt set open, the peer's prompts ride by name and only
      // this node's are written into the brief.
      if (remotePromptSetOpen(remote) && Array.isArray(request.promptRefs)) {
        const refs = request.promptRefs;
        const prompts = remotePromptBodies(refs.filter(isLocalRef), [], remote);
        return api.spawn(remote.group, remoteSpawnBody(request, request.explicit, remote.options, prompts, { promptRefs: refs.filter((r) => !isLocalRef(r)), worktree: used }));
      }
      return api.spawn(remote.group, remoteSpawnBody(request, request.explicit, remote.options, request.prompts, { worktree: used }));
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
// group: the peer's answer about allowances, its snapshot, and the libraries
// the dialog offers.
export async function prepareRemoteSpawn({ nodeId = '', node = '', group, snapshot = null, fetchImpl, localFetch } = {}) {
  if (nodeId && !SAFE_NODE.test(nodeId)) throw new Error('unknown node');
  const api = remoteSpawnApi(nodeId, fetchImpl ? { fetchImpl } : {});
  const options = await loadRemoteSpawnOptions(api, group);
  const peerFetch = peerFetchFor(nodeId, ...(fetchImpl ? [fetchImpl] : []));
  const catalogs = await loadRemoteSpawnCatalogs({ options, peerFetch, ...(localFetch ? { localFetch } : {}) });
  return Object.freeze({ nodeId, node: node || (nodeId ? nodeId.slice(0, 13) : 'the peer'), group, api, peerFetch, options, catalogs, snapshot });
}
