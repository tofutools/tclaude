import { readReviewer, reviewerValue } from './approval-controls.js';

export const MODEL_CUSTOM_VALUE = '__custom__';
export const WT_NEW = '__new__';
// Local-only select sentinel. Sandbox-profile names cannot contain "/", so it
// cannot collide with an operator-authored profile. Never send it as
// sandbox_profile: the request builder translates it to the explicit
// omit_sandbox_profiles wire flag.
export const SANDBOX_PROFILE_NONE = '/omit-all-tclaude-sandbox-profiles';
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

// SANDBOX_IMPL_DEFAULT mirrors sandboxpolicy.ImplementationHarnessBuiltin. It is
// the value a launch resolves to when nothing pins one, and the only value this
// dialog ever sends when the row is hidden.
export const SANDBOX_IMPL_DEFAULT = 'harness-builtin';
export const SANDBOX_IMPL_TCLAUDE_LAYER = 'tclaude-layer';

// The per-harness mode help describes the harness-owned sandbox. Under the
// tclaude layer that sandbox is deliberately off while the outer OS wall
// renders the same sandbox-profile policy, so reusing (for example) Claude
// Code's `off` help would call a confined launch unconfined. OpenCode is
// deliberately excluded: its soft rules remain enabled as defence in depth
// and its dedicated mode help describes that distinct topology.
//
// Blank is not harness-builtin: it leaves the group/global profile chain in
// charge, and the browser does not have the fully resolved launch. Stay
// neutral rather than guessing a branch whose copy may say the opposite.
export function sandboxModeHelpForImplementation(help, implementation, harness) {
  const harnessName = text(harness);
  if (!text(implementation)) {
    return 'Sandbox implementation is inherited from the profile chain at launch, '
      + "so this mode's effect is not known yet.";
  }
  if (text(implementation) === SANDBOX_IMPL_TCLAUDE_LAYER
    && (harnessName === 'claude' || harnessName === 'codex')) {
    return "The harness's own sandbox is off by design. The tclaude layer enforces "
      + 'sandbox-profile filesystem rules as OS mounts; any environment entries also apply.';
  }
  return text(help);
}

// sandboxImplView answers the two halves of "can this launch use the tclaude
// layer?" from the two places that own them: the HARNESS half from the harness
// catalog entry (never a harness-name switch here), and the HOST half from the
// snapshot-level catalog.
//
// The row is shown only when the selected harness can host the layer, because
// with one implementation left there is no choice to render. Host availability
// deliberately does NOT hide or disable anything: it only adds a warning. The
// launch-time refusal is the authority on what may run, and a dialog that
// quietly removed the option would have replaced that authority with itself —
// besides making it impossible to author a profile that pins the layer on a
// machine where bwrap is not installed yet.
function sandboxImplView(harness, context) {
  const catalog = context?.sandboxImpl || {};
  const options = Array.isArray(catalog.options) ? catalog.options : [];
  const serverBoundary = !!harness?.tclaude_layer_server_boundary;
  return {
    showSandboxImpl: harness ? !!harness.can_tclaude_layer : false,
    sandboxImplOptions: options,
    sandboxImplDefault: text(catalog.default) || SANDBOX_IMPL_DEFAULT,
    sandboxImplHostAvailable: serverBoundary
      ? catalog.server_host_available !== false
      : catalog.host_available !== false,
    sandboxImplHostReason: text(serverBoundary
      ? catalog.server_host_unavailable_reason
      : catalog.host_unavailable_reason),
  };
}

// sandboxImplClearedNoticeFor renders the notice shown after a harness switch
// discarded a sandbox-implementation selection.
//
// It exists because the row itself DISAPPEARS in that moment: the row is gated
// on the new harness's can_tclaude_layer, so the one control that could have
// shown the loss is gone at exactly the instant there is something to show. The
// selection is dropped, the launch proceeds on harness-builtin, and nothing
// anywhere says so.
//
// That is the dialog deciding by erasure. The server's loud refusal for an
// explicitly incompatible request is unreachable precisely BECAUSE the dialog
// cleared the value before submit, so the discloses-never-decides rule needs
// this line to hold. It names both the implementation that was dropped and the
// harness that dropped it, so the operator can act on it rather than wonder.
//
// It survives until the state it describes stops being true — an explicit
// re-pick, or a switch back to a harness that can host the layer.
export function sandboxImplClearedNoticeFor(draft) {
  const cleared = draft?.sandboxImplCleared;
  if (!cleared || !text(cleared.implementation)) return null;
  const harnessLabel = text(cleared.harness) || 'the selected harness';
  return {
    warn: true,
    text: `${text(cleared.implementation)} is not available for ${harnessLabel} — `
      + 'the selection was cleared, so this agent launches with the harness-builtin sandbox.',
  };
}

// setSpawnSandboxImpl records an EXPLICIT pick and retires any cleared-notice
// with it: once the operator has spoken for this field again, the notice is
// describing a state that no longer stands.
export function setSpawnSandboxImpl(draft, value) {
  return { ...draft, sandboxImpl: text(value), sandboxImplCleared: null };
}

// sandboxImplHintFor renders the note under the sandbox-implementation row.
// Two truths, in the order an operator needs them: what the selected
// implementation actually is, and — when they have selected the experimental
// one on a host that cannot run it — that the launch will REFUSE rather than
// quietly fall back. Saying "will refuse" is the whole point: an operator who
// picks it anyway is choosing a failed launch, not an unnoticed downgrade.
export function sandboxImplHintFor(draft, view) {
  if (!view.showSandboxImpl) return null;
  const value = text(draft.sandboxImpl) || view.sandboxImplDefault;
  if (value !== SANDBOX_IMPL_TCLAUDE_LAYER) return null;
  if (view.sandboxImplHostAvailable) {
    return {
      warn: false,
      text: 'Experimental. Wraps the authoritative tool executor in a tclaude-owned '
        + 'bubblewrap namespace.',
    };
  }
  const reason = view.sandboxImplHostReason || 'this host cannot create the namespace';
  return {
    warn: true,
    text: `Not available on this host: ${reason}. Selecting it will refuse the launch, not fall back.`,
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
  const showSSHWorkaround = !!harness?.can_ssh_workaround;
  const sshWorkaroundAvailable = showSSHWorkaround
    && draft.sandbox === 'tclaude-agent'
    && !sandboxProfilesDisabled
    && draft.sandboxProfile !== SANDBOX_PROFILE_NONE;
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
    showTrustDir: harness ? !!harness.can_dir_trust
      : (draft.harness === 'codex' || draft.harness === 'claude'),
    // The config file the opt-in edits, so the checkbox names the side effect.
    // Blank until the harness catalog loads; the copy degrades to "the
    // harness's config" rather than naming the wrong file.
    trustDirStore: harness?.dir_trust_store || '',
    showRemoteControl: harness ? !!harness.can_remote_control : draft.harness === 'claude',
    showAutoMemory: harness ? !!harness.can_auto_memory : draft.harness === 'claude',
    showSSHWorkaround,
    sshWorkaroundAvailable,
    showContextFeatures: harness ? !!harness.can_context_features : draft.harness === 'claude',
    showAutoCompactWindow: harness ? !!harness.can_auto_compact_window : draft.harness === 'claude',
    ...sandboxImplView(harness, context),
    autoCompactWindowMin: Number(harness?.auto_compact_window_min) || 0,
    autoCompactWindowMax: Number(harness?.auto_compact_window_max) || 0,
    contextFeatureCatalog: Array.isArray(harness?.context_features) ? harness.context_features : [],
    sandboxProfilesDisabled,
  };
}

// AUTO_COMPACT_WINDOW_PATTERN is the client-side shape check, applied to the
// value AFTER the same normalization harness.ParseAutoCompactWindow performs:
// surrounding whitespace trimmed, and `_` / interior spaces dropped as digit
// separators. It must not be STRICTER than the Go parser — this gate blocks
// submit (see validateSpawnDraft), so a spelling the daemon would accept must
// not be rejected here. A leading `.` is allowed for the same reason: Go reads
// ".5M" as 500000.
//
// The daemon remains the authority; this exists only so an obvious typo is
// caught before the round trip.
const AUTO_COMPACT_WINDOW_PATTERN = /^(\d+(\.\d+)?|\.\d+)[kKmM]?$/;

// normalizeAutoCompactWindowInput strips the separators the Go parser strips, so
// the pattern and the arithmetic below both see the same digits.
function normalizeAutoCompactWindowInput(raw) {
  return text(raw).trim().replace(/[_ ]/g, '');
}

// parseAutoCompactWindow returns the token count a field value denotes, or null
// when it is blank or malformed. Float math is fine here: this is a hint, and
// the daemon does the exact arithmetic on the digit string.
export function parseAutoCompactWindow(raw) {
  const value = normalizeAutoCompactWindowInput(raw);
  if (!value || !AUTO_COMPACT_WINDOW_PATTERN.test(value)) return null;
  const suffix = value.slice(-1);
  const scale = /[kK]/.test(suffix) ? 1000 : /[mM]/.test(suffix) ? 1000000 : 1;
  const digits = scale === 1 ? value : value.slice(0, -1);
  const tokens = Number(digits) * scale;
  return Number.isFinite(tokens) && Number.isInteger(tokens) ? tokens : null;
}

// autoCompactWindowHintFor renders the one-line note under the window field:
// what the pin means in practice, or why the current text will be rejected.
// Returns null when the field is blank (nothing useful to say about "unset").
export function autoCompactWindowHintFor(draft, view) {
  const raw = text(draft.autoCompactWindow);
  if (!raw) return null;
  const tokens = parseAutoCompactWindow(raw);
  if (tokens == null) {
    return { warn: true, text: `"${raw}" is not a token count — try 450000, 450k or 0.5M.` };
  }
  const min = view.autoCompactWindowMin || 0;
  const max = view.autoCompactWindowMax || 0;
  if ((min && tokens < min) || (max && tokens > max)) {
    return {
      warn: true,
      text: `${formatTokenWindow(tokens)} is outside the accepted range `
        + `(${formatTokenWindow(min)}–${formatTokenWindow(max)}).`,
    };
  }
  // The cap is the thing operators trip over: on a 200K model a 450K pin is a
  // no-op, and nothing in the UI would otherwise say so.
  return {
    warn: false,
    text: `Auto-compacts at ${formatTokenWindow(tokens)} of context, `
      + "or at the model's own window if that is smaller.",
  };
}

// formatTokenWindow renders a token count the way an operator writes one.
export function formatTokenWindow(tokens) {
  if (!tokens) return '';
  if (tokens % 1000000 === 0) return `${tokens / 1000000}M`;
  if (tokens % 1000 === 0) return `${tokens / 1000}k`;
  return String(tokens);
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
    sshWorkaround: !!harness?.can_ssh_workaround,
    autoCompactWindow: '',
    // "" = unset, so the daemon's profile tier stack still speaks. Sending
    // harness-builtin here instead would pin it and silence every lower tier.
    sandboxImpl: '',
    // Set only by a harness switch that discarded a selection; see
    // sandboxImplClearedNoticeFor. Never sent to the daemon.
    sandboxImplCleared: null,
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
    // Startup-context trims, sparse by construction: only features the operator
    // steered appear, so an untouched dialog changes nothing about what the
    // agent loads.
    contextFeatures: {},
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
    sshWorkaround: !!harness?.can_ssh_workaround,
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
    sshWorkaround: !!harness?.can_ssh_workaround,
    // A harness with no steerable startup context cannot carry trims, and keeping
    // them would send a map the daemon rejects with a 400.
    contextFeatures: harness?.can_context_features ? draft.contextFeatures : {},
    // Likewise a harness with no auto-compaction knob: keeping a typed window
    // would send a value the daemon rejects with a 400.
    autoCompactWindow: harness?.can_auto_compact_window ? draft.autoCompactWindow : '',
    // A harness that cannot host the layer must not carry a tclaude-layer
    // selection across the switch: it would be a 400 the operator never typed.
    // But dropping it silently is the dialog deciding by erasure, so record what
    // was discarded and by which harness — the row is about to vanish, and this
    // notice is the only surface left that can say so.
    sandboxImpl: harness?.can_tclaude_layer ? draft.sandboxImpl : '',
    sandboxImplCleared: !harness?.can_tclaude_layer && text(draft.sandboxImpl)
      ? { implementation: text(draft.sandboxImpl), harness: harness?.display_name || text(harnessName) }
      : null,
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
  // Same rule as the reviewer block above: a sparse profile means "let the
  // tier stack decide", NOT "keep whatever the last profile set". Leaving a
  // prior true in place would both pre-trust a directory nobody asked to trust
  // for THIS profile and — because trustDirSpecified pins an explicit value —
  // suppress the group/global default that should have spoken instead.
  if (view.showTrustDir && profile.trust_dir != null) {
    next.trustDir = !!profile.trust_dir;
    next.trustDirSpecified = true;
  } else {
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
  next.sshWorkaround = view.showSSHWorkaround
    ? profile.ssh_workaround !== false : false;
  // Same "a sparse profile means inherit" rule: an unset window clears any value
  // the previously selected profile put in the field, rather than leaving it to
  // silently ride along onto a profile that never asked for it.
  next.autoCompactWindow = view.showAutoCompactWindow && profile.auto_compact_window
    ? text(profile.auto_compact_window) : '';
  // Same rule again: a profile that pins nothing clears whatever the previously
  // selected profile put here, rather than letting an implementation ride along
  // onto a profile that never asked for it.
  next.sandboxImpl = view.showSandboxImpl && profile.sandbox_implementation
    ? text(profile.sandbox_implementation) : '';
  next.sandboxImplCleared = null;
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
  // The profile's trims REPLACE the form's rather than merging, matching the
  // daemon's whole-tier resolution: one profile always tells the whole story of
  // what its agents load. A profile that trims nothing clears the form.
  next.contextFeatures = view.showContextFeatures && profile.context_features
    ? { ...profile.context_features } : {};
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
    sshWorkaround: !!findSpawnHarness(context.harnesses, defaults.harness)?.can_ssh_workaround,
    autoCompactWindow: defaults.autoCompactWindow,
    sandboxImpl: defaults.sandboxImpl,
    sandboxImplCleared: null,
    owner: false,
    permissionOverrides: {},
    contextFeatures: {},
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

// contextFeaturesKey renders a trim map as a stable string for dirty comparison.
// Object key order is insertion order in JS, so two equal maps built in different
// orders would compare unequal under a bare JSON.stringify.
export function contextFeaturesKey(features) {
  return Object.entries(features || {})
    .filter(([, state]) => state === 'on' || state === 'off')
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([slug, state]) => `${slug}=${state}`)
    .join(',');
}

// spawnContextFeatureIndicator summarises a trim map for the button's badge —
// the twin of spawnPermissionIndicator.
export function spawnContextFeatureIndicator(features) {
  let trimmed = 0;
  let kept = 0;
  for (const state of Object.values(features || {})) {
    if (state === 'off') trimmed += 1;
    else if (state === 'on') kept += 1;
  }
  const parts = [];
  if (trimmed) parts.push(`${trimmed} trimmed`);
  if (kept) parts.push(`${kept} kept`);
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
  if (view.showContextFeatures && Object.keys(draft.contextFeatures || {}).length) {
    seed.context_features = { ...draft.contextFeatures };
  }
  if (view.sandbox.visible) seed.sandbox = draft.sandbox;
  if (view.approval.visible) seed.approval = draft.approval;
  if (view.tools.visible) seed.tools = draft.tools;
  const reviewer = view.showApprovalReviewer ? readReviewer(draft.approvalReviewer) : null;
  if (reviewer != null) seed.auto_review = reviewer;
  if (view.askTimeout.visible) seed.ask_user_question_timeout = draft.askTimeout;
  // Seed trust-dir only when the operator actually touched the checkbox. An
  // explicit false is NOT free: it chips the profile as "trust-dir off" on a
  // field nobody set, and its TrustDirSet bit suppresses an inherited
  // group-default trust_dir. Same reasoning as auto-memory just below — it
  // matters more now that this row is shown for Claude Code too, i.e. for most
  // profiles saved from this dialog.
  if (view.showTrustDir && draft.trustDirSpecified) seed.trust_dir = !!draft.trustDir;
  // Seed only an explicit opt-IN. Off is what an unset profile already
  // resolves to, so pinning false would give the operator an "auto-memory off"
  // chip on a field they never touched — indistinguishable from a deliberate
  // pin, and it would opt the profile out of any future default change.
  if (view.showAutoMemory && draft.autoMemory) seed.auto_memory = true;
  if (view.showSSHWorkaround) seed.ssh_workaround = !!draft.sshWorkaround;
  if (view.showAutoCompactWindow && text(draft.autoCompactWindow)) {
    seed.auto_compact_window = text(draft.autoCompactWindow);
  }
  // Seed only an explicit selection. Leaving it unset keeps the profile silent
  // so lower tiers still speak — and a profile that pinned harness-builtin
  // merely because the operator never touched the row would be an override
  // nobody asked for.
  if (view.showSandboxImpl && text(draft.sandboxImpl)) {
    seed.sandbox_implementation = text(draft.sandboxImpl);
  }
  return seed;
}

const DIRTY_FIELDS = [
  'group', 'profile', 'name', 'role', 'descr', 'task', 'initialMessage',
  'harness', 'model', 'customModel', 'effort', 'sandbox', 'sandboxProfile', 'approval',
  'approvalReviewer', 'tools', 'askTimeout', 'autoCompactWindow', 'sandboxImpl', 'trustDir', 'trustDirSpecified', 'remoteControl', 'autoMemory', 'sshWorkaround', 'owner',
  'cwd', 'wtRepo', 'worktree', 'worktreeBranch', 'worktreeBase',
  'syncWorktree', 'autoFocus', 'includeGroupContext',
];

export function spawnDraftIsDirty(draft, baseline, attachmentCount = 0) {
  if (attachmentCount) return true;
  if (DIRTY_FIELDS.some((key) => draft[key] !== baseline[key])) return true;
  if (JSON.stringify(draft.permissionOverrides || {})
    !== JSON.stringify(baseline.permissionOverrides || {})) return true;
  return contextFeaturesKey(draft.contextFeatures) !== contextFeaturesKey(baseline.contextFeatures);
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
  // Catch a malformed or out-of-range window here rather than letting the
  // operator discover it as a 400 after the spawn round trip. The daemon is
  // still the authority; this just moves the same answer earlier.
  const view = spawnCapabilityView(draft, context);
  if (view.showAutoCompactWindow) {
    const windowHint = autoCompactWindowHintFor(draft, view);
    if (windowHint?.warn) return windowHint.text;
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
  if (view.sandboxProfilesDisabled || draft.sandboxProfile === SANDBOX_PROFILE_NONE) {
    body.omit_sandbox_profiles = true;
  } else if (draft.sandboxProfile) {
    body.sandbox_profile = draft.sandboxProfile;
  }
  if (view.approval.visible && draft.approval) body.approval = draft.approval;
  if (view.tools.visible && draft.tools) body.tools = draft.tools;
  const reviewer = view.showApprovalReviewer ? readReviewer(draft.approvalReviewer) : null;
  if (reviewer != null) body.auto_review = reviewer;
  if (view.askTimeout.visible && draft.askTimeout) {
    body.ask_user_question_timeout = draft.askTimeout;
  }
  if (view.showTrustDir && draft.trustDirSpecified) {
    body.trust_dir = !!draft.trustDir;
  }
  if (view.showRemoteControl) body.remote_control = !!draft.remoteControl;
  if (view.showAutoMemory) body.auto_memory = !!draft.autoMemory;
  if (view.showSSHWorkaround) {
    body.ssh_workaround = !!(view.sshWorkaroundAvailable && draft.sshWorkaround);
  }
  // Blank omits the key so the daemon's profile tier stack still speaks; the
  // daemon normalizes "450k" to plain digits, so the raw field text is sent.
  if (view.showAutoCompactWindow && text(draft.autoCompactWindow)) {
    body.auto_compact_window = text(draft.autoCompactWindow);
  }
  // Blank omits the key, so an untouched row leaves the daemon's profile tier
  // stack in charge and the launch stays default-off. An explicit selection —
  // including harness-builtin — is sent, because pinning the legacy layer
  // against a group default that would have flipped it is a real intent.
  if (view.showSandboxImpl && text(draft.sandboxImpl)) {
    body.sandbox_implementation = text(draft.sandboxImpl);
  }
  if (draft.owner) body.is_owner = true;
  if (Object.keys(draft.permissionOverrides || {}).length) {
    body.permission_overrides = { ...draft.permissionOverrides };
  }
  // Sent whenever the harness can take it, INCLUDING as an empty object: the
  // form is the authoritative statement of what this agent loads, so an operator
  // who cleared a profile's trims must not silently get them back from the
  // daemon's profile tier stack. See SpawnRequest.ContextFeatures.
  if (view.showContextFeatures) {
    body.context_features = { ...(draft.contextFeatures || {}) };
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
