import { readReviewer, reviewerValue } from './approval-controls.js';

export const MODEL_CUSTOM_VALUE = '__custom__';
export const WT_NEW = '__new__';
export const MAX_SPAWN_NAME_LEN = 64;
export const SPAWN_NAME_VALID = /^[A-Za-z0-9_-]{1,64}$/;

const DEFAULT_EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'];

function text(value) {
  return value == null ? '' : String(value);
}

export function findSpawnGroup(groups, name) {
  return (groups || []).find((group) => group?.name === name) || null;
}

export function findSpawnHarness(harnesses, name) {
  return (harnesses || []).find((harness) => harness?.name === name) || null;
}

export function findSpawnProfile(profiles, handle) {
  const name = text(handle).trim();
  return (profiles || []).find((profile) =>
    profile?.name === name || (profile?.aliases || []).includes(name)) || null;
}

export function spawnProfileChoices(profiles) {
  const choices = [];
  for (const profile of profiles || []) {
    const disabled = profile.disabled
      ? ` [🚫 disabled: ${text(profile.disabled_reason).replace(/\s+/g, ' ').trim()}]`
      : '';
    choices.push({ value: profile.name, label: profile.name + disabled });
    for (const alias of profile.aliases || []) {
      choices.push({ value: alias, label: `${alias} → ${profile.name}${disabled}` });
    }
  }
  return choices;
}

export function defaultSpawnHarness(harnesses) {
  return findSpawnHarness(harnesses, 'claude') || (harnesses || [])[0] || null;
}

export function normalizeSpawnName(name) {
  let out = '';
  let previousSeparator = false;
  for (const character of text(name)) {
    if (/[A-Za-z0-9_-]/.test(character)) {
      out += character;
      previousSeparator = false;
    } else if (!previousSeparator) {
      out += '-';
      previousSeparator = true;
    }
  }
  out = out.replace(/^-+/, '').replace(/-+$/, '');
  if (out.length > MAX_SPAWN_NAME_LEN) {
    out = out.slice(0, MAX_SPAWN_NAME_LEN).replace(/-+$/, '');
  }
  return out;
}

export function deriveSpawnNameFromMessage(message) {
  const words = [];
  for (const raw of text(message).trim().split(/\s+/)) {
    const word = normalizeSpawnName(raw);
    if (word) words.push(word);
    if (words.length >= 4) break;
  }
  return normalizeSpawnName(words.join('-'));
}

export function spawnNameHint(name, normalizeEnabled = true) {
  const raw = text(name).trim();
  if (!raw || SPAWN_NAME_VALID.test(raw)) return { text: '', warn: false };
  if (!normalizeEnabled) {
    return {
      text: 'invalid — use only letters, digits, underscore and dash (max 64 chars)',
      warn: true,
    };
  }
  const normalized = normalizeSpawnName(raw);
  return {
    text: normalized
      ? `will be created as “${normalized}”`
      : 'no usable characters — the agent will get an auto-generated name',
    warn: false,
  };
}

export function groupDefaultProfileName(groups, groupName) {
  return text(findSpawnGroup(groups, groupName)?.default_profile);
}

export function selectedDefaultProfile({ groups, groupName, dashboardDefault = '', override = '' }) {
  return text(override) || groupDefaultProfileName(groups, groupName) || text(dashboardDefault);
}

export function groupRemoteControlDefault(group, profile = null) {
  const policy = text(group?.remote_control_policy);
  if (policy === 'optin') return true;
  if (policy === 'deny') return false;
  return profile?.remote_control != null ? !!profile.remote_control : false;
}

export function launchSetting(harness, key) {
  const specs = {
    sandbox: ['can_sandbox', 'sandbox_modes', 'default_sandbox', 'sandbox_mode_help'],
    approval: ['can_approval', 'approval_modes', 'default_approval', 'approval_mode_help'],
    tools: ['can_tools', 'tools_modes', 'default_tools', 'tools_mode_help'],
    askTimeout: ['can_ask_timeout', 'ask_timeout_modes', 'default_ask_timeout', 'ask_timeout_mode_help'],
  };
  const [capability, modesKey, defaultKey, helpKey] = specs[key];
  const modes = Array.isArray(harness?.[modesKey]) ? harness[modesKey] : [];
  const visible = !!(harness?.[capability] && modes.length);
  const value = visible && modes.includes(harness?.[defaultKey])
    ? harness[defaultKey]
    : visible ? modes[0] : '';
  return {
    visible,
    modes,
    recommended: text(harness?.[defaultKey]),
    value,
    help: harness?.[helpKey] || {},
  };
}

export function spawnCapabilityView(draft, context) {
  const harness = findSpawnHarness(context.harnesses, draft.harness);
  const models = Array.isArray(harness?.models) ? harness.models : [];
  const hasModelList = !harness || models.length > 0;
  const sandbox = launchSetting(harness, 'sandbox');
  const approval = launchSetting(harness, 'approval');
  const tools = launchSetting(harness, 'tools');
  const askTimeout = launchSetting(harness, 'askTimeout');
  const sandboxProfilesDisabled = draft.harness === 'codex'
    && draft.sandbox === 'danger-full-access';
  return {
    harness,
    models,
    hasModelList,
    efforts: Array.isArray(harness?.effort_levels) && harness.effort_levels.length
      ? harness.effort_levels : DEFAULT_EFFORTS,
    sandbox,
    approval,
    tools,
    askTimeout,
    showApprovalReviewer: !!harness?.can_auto_review,
    showTrustDir: draft.harness === 'codex',
    showRemoteControl: harness ? !!harness.can_remote_control : draft.harness === 'claude',
    showAutoMemory: harness ? !!harness.can_auto_memory : draft.harness === 'claude',
    sandboxProfilesDisabled,
  };
}

export function modelSelectValue(draft, context) {
  const view = spawnCapabilityView(draft, context);
  if (!view.hasModelList) return draft.model;
  if (draft.customModel) return MODEL_CUSTOM_VALUE;
  if (!draft.model) return '';
  return view.models.includes(draft.model) ? draft.model : MODEL_CUSTOM_VALUE;
}

export function spawnModelDefaultLabel(draft, context, profiles = []) {
  if (draft.harness !== 'claude') return 'Default (harness\'s own)';
  const group = findSpawnGroup(context.groups, draft.group);
  const profile = findSpawnProfile(profiles, group?.default_profile);
  if (profile?.model && (!profile.harness || profile.harness === 'claude')) {
    return `Default (${profile.model} — group default)`;
  }
  return context.userDefaultModel
    ? `Default (${context.userDefaultModel} — user settings)`
    : "Default (claude's own)";
}

function harnessDefaults(harness, rememberedEffort = () => '') {
  const sandbox = launchSetting(harness, 'sandbox').value;
  const approval = launchSetting(harness, 'approval').value;
  const tools = launchSetting(harness, 'tools').value;
  const askTimeout = launchSetting(harness, 'askTimeout').value;
  return {
    harness: text(harness?.name),
    model: '',
    customModel: false,
    effort: rememberedEffort('') || '',
    sandbox,
    approval,
    tools,
    approvalReviewer: '',
    askTimeout,
    trustDir: false,
    trustDirSpecified: false,
    remoteControl: false,
    // Off is tclaude's recommended posture: agents sharing a repo would
    // otherwise cross-pollute one Claude Code project memory store.
    autoMemory: false,
    sandboxProfile: '',
  };
}

export function createSpawnDraft({
  groups = [], harnesses = [], groupName = '', defaultGroup = '',
  autoFocus = true, rememberedEffort = () => '',
} = {}) {
  const liveDefault = findSpawnGroup(groups, defaultGroup)?.name || '';
  const group = findSpawnGroup(groups, groupName)
    || (groupName ? { name: groupName } : null)
    || findSpawnGroup(groups, liveDefault)
    || groups.find((entry) => entry?.name);
  const harness = defaultSpawnHarness(harnesses);
  const cwd = text(group?.default_cwd);
  return {
    group: text(group?.name),
    fixedGroup: !!groupName,
    profile: '',
    name: '', role: '', descr: '', task: '', initialMessage: '',
    ...harnessDefaults(harness, rememberedEffort),
    owner: false,
    permissionOverrides: {},
    cwd,
    cwdOrigin: cwd ? 'group' : '',
    wtRepo: cwd,
    wtRepoEdited: false,
    worktree: '',
    worktreeBranch: '',
    worktreeBase: '',
    syncWorktree: true,
    autoFocus: !!autoFocus,
    includeGroupContext: true,
    remoteControl: groupRemoteControlDefault(group),
    autoMemory: false,
  };
}

export function selectSpawnGroup(draft, groupName, context) {
  const group = findSpawnGroup(context.groups, groupName);
  const nextCwd = text(group?.default_cwd);
  const replaceCwd = !draft.cwd.trim() || draft.cwdOrigin === 'group';
  const cwd = replaceCwd ? nextCwd : draft.cwd;
  return {
    ...draft,
    group: text(groupName),
    cwd,
    cwdOrigin: replaceCwd && nextCwd ? 'group' : replaceCwd ? '' : draft.cwdOrigin,
    wtRepo: draft.wtRepoEdited ? draft.wtRepo : cwd,
    worktree: '',
    worktreeBranch: '',
    worktreeBase: '',
    includeGroupContext: true,
    remoteControl: groupRemoteControlDefault(group),
    sandboxProfile: '',
  };
}

export function selectSpawnHarness(draft, harnessName, context, rememberedEffort = () => '') {
  const harness = findSpawnHarness(context.harnesses, harnessName)
    || defaultSpawnHarness(context.harnesses);
  const defaults = harnessDefaults(harness, rememberedEffort);
  const group = findSpawnGroup(context.groups, draft.group);
  return {
    ...draft,
    ...defaults,
    remoteControl: harness?.can_remote_control
      ? groupRemoteControlDefault(group) : false,
    autoMemory: harness?.can_auto_memory ? draft.autoMemory : false,
  };
}

function compatibleValue(value, modes, fallback) {
  return value && modes.includes(value) ? value : fallback;
}

export function applySpawnProfile(
  draft, profile, context, rememberedEffort = () => '', pickerUsable = false,
) {
  if (!profile) return draft;
  let next = { ...draft };
  if (profile.harness && findSpawnHarness(context.harnesses, profile.harness)) {
    const keepModel = profile.harness === next.harness ? next.model : '';
    const keepCustomModel = profile.harness === next.harness && next.customModel;
    next = selectSpawnHarness(next, profile.harness, context, rememberedEffort);
    if (keepModel) next.model = keepModel;
    if (keepCustomModel) next.customModel = true;
  }
  const view = spawnCapabilityView(next, context);
  if (profile.model) {
    next.model = text(profile.model);
    next.customModel = view.hasModelList && !view.models.includes(next.model);
  }
  if (profile.effort && view.efforts.includes(profile.effort)) next.effort = profile.effort;
  else if (!profile.effort && profile.model) next.effort = rememberedEffort(next.model) || '';
  if (profile.sandbox) {
    next.sandbox = compatibleValue(profile.sandbox, view.sandbox.modes, next.sandbox);
  }
  if (profile.approval) {
    next.approval = compatibleValue(profile.approval, view.approval.modes, next.approval);
  }
  if (profile.tools) {
    next.tools = compatibleValue(profile.tools, view.tools.modes, next.tools);
  }
  if (view.showApprovalReviewer) {
    // A sparse profile means "inherit", not "keep the last selected profile's
    // reviewer". Clear the prior selection so switching from an auto-review
    // profile cannot accidentally send an explicit true for the new profile.
    next.approvalReviewer = reviewerValue(profile.auto_review);
  } else {
    next.approvalReviewer = '';
  }
  if (profile.ask_user_question_timeout) {
    next.askTimeout = compatibleValue(
      profile.ask_user_question_timeout, view.askTimeout.modes, next.askTimeout,
    );
  }
  if (next.harness === 'codex' && profile.trust_dir != null) {
    next.trustDir = !!profile.trust_dir;
    next.trustDirSpecified = true;
  } else if (next.harness !== 'codex') {
    next.trustDir = false;
    next.trustDirSpecified = false;
  }
  const group = findSpawnGroup(context.groups, next.group);
  next.remoteControl = view.showRemoteControl
    ? groupRemoteControlDefault(group, profile) : false;
  // A profile's auto_memory speaks only when it explicitly set one; unset keeps
  // the dialog's own default, which is off.
  next.autoMemory = view.showAutoMemory && profile.auto_memory != null
    ? !!profile.auto_memory : false;
  if (profile.agent_name) next.name = text(profile.agent_name);
  if (profile.role) next.role = text(profile.role);
  if (profile.descr) next.descr = text(profile.descr);
  if (profile.initial_message) next.initialMessage = text(profile.initial_message);
  if (profile.auto_focus != null) next.autoFocus = !!profile.auto_focus;
  if (profile.sync_worktree != null) next.syncWorktree = !!profile.sync_worktree;
  if (profile.include_group_default_context != null) {
    next.includeGroupContext = !!profile.include_group_default_context;
  }
  if (profile.is_owner != null) next.owner = !!profile.is_owner;
  if (profile.permission_overrides) {
    next.permissionOverrides = { ...profile.permission_overrides };
  }
  return syncSpawnWorktree(next, pickerUsable);
}

export function clearSpawnProfileFields(draft, context, {
  autoFocus = true, rememberedEffort = () => '',
} = {}) {
  const defaults = createSpawnDraft({
    groups: context.groups,
    harnesses: context.harnesses,
    groupName: draft.fixedGroup ? draft.group : '',
    defaultGroup: draft.group,
    autoFocus,
    rememberedEffort,
  });
  return syncSpawnWorktree({
    ...draft,
    profile: '',
    name: '', role: '', descr: '', task: '', initialMessage: '',
    harness: defaults.harness,
    model: defaults.model,
    customModel: defaults.customModel,
    effort: defaults.effort,
    sandbox: defaults.sandbox,
    approval: defaults.approval,
    tools: defaults.tools,
    approvalReviewer: defaults.approvalReviewer,
    askTimeout: defaults.askTimeout,
    trustDir: false,
    trustDirSpecified: false,
    remoteControl: defaults.remoteControl,
    autoMemory: false,
    owner: false,
    permissionOverrides: {},
    syncWorktree: defaults.syncWorktree,
    autoFocus: defaults.autoFocus,
    includeGroupContext: true,
  }, false);
}

export function setSpawnCwd(draft, cwd) {
  return {
    ...draft,
    cwd: text(cwd),
    cwdOrigin: 'user',
    wtRepo: draft.wtRepoEdited ? draft.wtRepo : text(cwd),
    worktree: '',
    worktreeBranch: '',
    worktreeBase: '',
  };
}

export function setSpawnWorktreeRepo(draft, repo) {
  return {
    ...draft,
    wtRepo: text(repo),
    wtRepoEdited: true,
    worktree: '',
    worktreeBranch: '',
    worktreeBase: '',
  };
}

export function syncSpawnWorktree(draft, pickerUsable = true) {
  if (!draft.syncWorktree) return draft;
  if (!pickerUsable) {
    return draft.worktree === WT_NEW
      ? { ...draft, worktree: '', worktreeBranch: '' }
      : draft;
  }
  const name = text(draft.name).trim();
  if (name) return { ...draft, worktree: WT_NEW, worktreeBranch: name };
  if (draft.worktree === WT_NEW) {
    return { ...draft, worktree: '', worktreeBranch: '' };
  }
  return draft;
}

export function selectSpawnWorktree(draft, value) {
  return {
    ...draft,
    worktree: text(value),
    syncWorktree: value === WT_NEW ? draft.syncWorktree : false,
  };
}

export function spawnPermissionIndicator(overrides) {
  let grants = 0;
  let denies = 0;
  for (const effect of Object.values(overrides || {})) {
    if (effect === 'deny') denies += 1;
    else grants += 1;
  }
  const parts = [];
  if (grants) parts.push(`${grants} grant${grants === 1 ? '' : 's'}`);
  if (denies) parts.push(`${denies} den${denies === 1 ? 'y' : 'ies'}`);
  return parts.join(' · ');
}

export function spawnProfileSeed(draft, context) {
  const view = spawnCapabilityView(draft, context);
  const seed = {
    harness: draft.harness,
    model: text(draft.model).trim(),
    effort: draft.effort,
    agent_name: text(draft.name).trim(),
    role: text(draft.role).trim(),
    descr: text(draft.descr).trim(),
    initial_message: draft.initialMessage,
    auto_focus: !!draft.autoFocus,
    sync_worktree: !!draft.syncWorktree,
    include_group_default_context: !!draft.includeGroupContext,
    is_owner: !!draft.owner,
  };
  if (Object.keys(draft.permissionOverrides || {}).length) {
    seed.permission_overrides = { ...draft.permissionOverrides };
  }
  if (view.sandbox.visible) seed.sandbox = draft.sandbox;
  if (view.approval.visible) seed.approval = draft.approval;
  if (view.tools.visible) seed.tools = draft.tools;
  const reviewer = view.showApprovalReviewer ? readReviewer(draft.approvalReviewer) : null;
  if (reviewer != null) seed.auto_review = reviewer;
  if (view.askTimeout.visible) seed.ask_user_question_timeout = draft.askTimeout;
  if (draft.harness === 'codex') seed.trust_dir = !!draft.trustDir;
  // Seed only an explicit opt-IN. Off is what an unset profile already
  // resolves to, so pinning false would give the operator an "auto-memory off"
  // chip on a field they never touched — indistinguishable from a deliberate
  // pin, and it would opt the profile out of any future default change.
  if (view.showAutoMemory && draft.autoMemory) seed.auto_memory = true;
  return seed;
}

const DIRTY_FIELDS = [
  'group', 'profile', 'name', 'role', 'descr', 'task', 'initialMessage',
  'harness', 'model', 'customModel', 'effort', 'sandbox', 'sandboxProfile', 'approval',
  'approvalReviewer', 'tools', 'askTimeout', 'trustDir', 'trustDirSpecified', 'remoteControl', 'autoMemory', 'owner',
  'cwd', 'wtRepo', 'worktree', 'worktreeBranch', 'worktreeBase',
  'syncWorktree', 'autoFocus', 'includeGroupContext',
];

export function spawnDraftIsDirty(draft, baseline, attachmentCount = 0) {
  if (attachmentCount) return true;
  if (DIRTY_FIELDS.some((key) => draft[key] !== baseline[key])) return true;
  return JSON.stringify(draft.permissionOverrides || {})
    !== JSON.stringify(baseline.permissionOverrides || {});
}

export function validateSpawnDraft(draft, context) {
  if (!text(draft.group)) return 'group is required';
  const rawName = text(draft.name).trim();
  if (rawName && !SPAWN_NAME_VALID.test(rawName) && !context.normalizeNames) {
    return 'name may use only letters, digits, underscore and dash (max 64 chars)';
  }
  const usableName = context.normalizeNames ? normalizeSpawnName(rawName) : rawName;
  if (!usableName && !text(draft.descr).trim() && !deriveSpawnNameFromMessage(draft.initialMessage)) {
    return 'give the agent a name or an initial description';
  }
  if (draft.worktree === WT_NEW && !text(draft.worktreeBranch).trim()) {
    return 'enter a branch name for the new worktree';
  }
  return '';
}

export function prepareSpawnDraft(
  draft, context, confirmedDerivedName = '', pickerUsable = false,
) {
  let name = text(draft.name).trim();
  if (name && !SPAWN_NAME_VALID.test(name) && context.normalizeNames) {
    name = normalizeSpawnName(name);
  }
  if (!name && !text(draft.descr).trim() && confirmedDerivedName) {
    name = confirmedDerivedName;
  }
  return syncSpawnWorktree({ ...draft, name }, pickerUsable);
}

export function buildSpawnRequest(draft, context, worktreeSelection, attachmentPaths = []) {
  const view = spawnCapabilityView(draft, context);
  const body = {
    name: text(draft.name).trim(),
    role: text(draft.role).trim(),
    descr: text(draft.descr).trim(),
    initial_message: draft.initialMessage,
    auto_focus: !!draft.autoFocus,
    include_group_context: !!draft.includeGroupContext,
  };
  if (draft.profile) body.profile = draft.profile;
  if (attachmentPaths.length) body.attachments = [...attachmentPaths];
  if (draft.effort) body.effort = draft.effort;
  if (text(draft.model).trim()) body.model = text(draft.model).trim();
  if (text(draft.task).trim()) body.task_ref_url = text(draft.task).trim();
  if (draft.harness) body.harness = draft.harness;
  if (view.sandbox.visible && draft.sandbox) body.sandbox = draft.sandbox;
  if (!view.sandboxProfilesDisabled && draft.sandboxProfile) {
    body.sandbox_profile = draft.sandboxProfile;
  }
  if (view.approval.visible && draft.approval) body.approval = draft.approval;
  if (view.tools.visible && draft.tools) body.tools = draft.tools;
  const reviewer = view.showApprovalReviewer ? readReviewer(draft.approvalReviewer) : null;
  if (reviewer != null) body.auto_review = reviewer;
  if (view.askTimeout.visible && draft.askTimeout) {
    body.ask_user_question_timeout = draft.askTimeout;
  }
  if (draft.harness === 'codex' && draft.trustDirSpecified) {
    body.trust_dir = !!draft.trustDir;
  }
  if (view.showRemoteControl) body.remote_control = !!draft.remoteControl;
  if (view.showAutoMemory) body.auto_memory = !!draft.autoMemory;
  if (draft.owner) body.is_owner = true;
  if (Object.keys(draft.permissionOverrides || {}).length) {
    body.permission_overrides = { ...draft.permissionOverrides };
  }
  const cwd = text(draft.cwd).trim();
  const repo = text(draft.wtRepo).trim();
  if (worktreeSelection?.path && repo && repo !== cwd) {
    body.cwd = cwd;
    body.worktree_path = worktreeSelection.path;
    body.worktree_branch = worktreeSelection.branch || '';
  } else if (worktreeSelection?.path) {
    body.cwd = worktreeSelection.path;
  } else {
    body.cwd = cwd;
  }
  return {
    url: `/api/groups/${encodeURIComponent(draft.group)}/spawn`,
    body,
  };
}

export function groupHasContext(groups, groupName) {
  return text(findSpawnGroup(groups, groupName)?.default_context).trim() !== '';
}

export function attachKey(file) {
  return `${file?.name || ''}|${file?.size || 0}|${file?.type || ''}`;
}

export function formatAttachmentSize(size) {
  const value = Number(size) || 0;
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) {
    return `${(value / 1024).toFixed(value < 10 * 1024 ? 1 : 0)} KB`;
  }
  return `${(value / (1024 * 1024)).toFixed(1)} MB`;
}
