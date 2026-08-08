import { readReviewer, reviewerValue } from './approval-controls.js';
import { CODEX_BUILTIN_FILTERED_NETWORK_DETAIL } from './sandbox-network-disclosure.js';

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
    const status = profile.disabled
      ? ` [🚫 disabled: ${text(profile.disabled_reason).replace(/\s+/g, ' ').trim()}]`
      : profile.operator_only ? ' [👤 operator only]' : '';
    choices.push({ value: profile.name, label: profile.name + status });
    for (const alias of profile.aliases || []) {
      choices.push({ value: alias, label: `${alias} → ${profile.name}${status}` });
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

// SANDBOX_IMPL_DEFAULT mirrors sandboxpolicy.ImplementationHarnessBuiltin. It
// is the historical normalized default, but a blank dialog value remains
// unpinned so profile resolution and harness-specific behavior stay intact.
export const SANDBOX_IMPL_DEFAULT = 'harness-builtin';
export const SANDBOX_IMPL_TCLAUDE_LAYER = 'tclaude-layer';
export const SANDBOX_IMPL_STACKED = 'stacked';
export const SANDBOX_IMPL_OFF = 'off';
export const SANDBOX_IMPL_RESOURCE_ONLY = 'resource-only';

export function harnessBuiltinModeIsOff(harnessName, mode) {
  const offModes = {
    claude: 'off',
    codex: 'danger-full-access',
    opencode: 'off',
  };
  const offMode = offModes[text(harnessName)] || '';
  return !!offMode && text(mode) === offMode;
}

// SANDBOX_APPARMOR_DOC is the operator guide a hint links to when this host
// most likely denies the nested bwrap stacked needs. The docs own the
// explanation, the workaround, and its host-wide trade-off; the hint stays one
// sentence and points here rather than growing its own copy of them.
export const SANDBOX_APPARMOR_DOC = {
  href: 'https://github.com/tofutools/tclaude/blob/main/docs/sandboxing.md'
    + '#stacked-refuses-on-apparmor-restricted-hosts',
  label: 'why, and how to allow it',
};

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
export function harnessBuiltinModeHelpForImplementation(help, implementation, harness) {
  const harnessName = text(harness);
  if (harnessName === 'opencode') return text(help);
  if (!text(implementation)) {
    return 'The sandbox implementation comes from the resolved defaults at launch, '
      + "so this mode's effect is not known yet.";
  }
  if (text(implementation) === SANDBOX_IMPL_TCLAUDE_LAYER
    && (harnessName === 'claude' || harnessName === 'codex')) {
    return "The harness's own sandbox is off by design. tclaude's built-in OS sandbox enforces "
      + 'sandbox-profile filesystem rules as OS mounts; any environment entries also apply.';
  }
  if (text(implementation) === SANDBOX_IMPL_STACKED
    && (harnessName === 'claude' || harnessName === 'codex')) {
    return "The tclaude outer mounts and the harness's real nested OS sandbox both enforce "
      + 'the launch policy. A fresh engine round-trip must succeed before launch; '
      + 'environment entries also apply.';
  }
  return text(help);
}

// HARNESS_PLACEHOLDER_FALLBACK stands in when no harness is selected. The
// implementation rows are gated on a selected harness, so this is defensive
// rather than a state the dialogs render.
const HARNESS_PLACEHOLDER_FALLBACK = 'the harness';

// fillHarnessPlaceholder substitutes the catalog's "{harness}" placeholder with
// the display name of the selected harness, so "Harness built-in" reads
// "Claude Code built-in" instead — the generic word is easily read as tclaude
// itself being the harness. The copy stays in the Go catalog; only the name is
// filled here, because the catalog is host-wide while the harness selection
// changes in the browser without a refetch.
function fillHarnessPlaceholder(template, displayName) {
  const raw = text(template);
  if (!raw.includes('{harness}')) return raw;
  const name = text(displayName) || HARNESS_PLACEHOLDER_FALLBACK;
  const filled = raw.replaceAll('{harness}', name);
  // A sentence-initial placeholder must not start the string lowercase when the
  // fallback (or an all-lowercase display name) lands there.
  return raw.startsWith('{harness}')
    ? filled.charAt(0).toUpperCase() + filled.slice(1)
    : filled;
}

// sandboxImplOptionsFor names the harness-owned option after the actual harness.
// Both the spawn dialog and the profile editor render the same catalog, so both
// call this rather than each rewriting the copy.
export function sandboxImplOptionsFor(options, displayName, canBuiltinOSSandbox = true) {
  return (Array.isArray(options) ? options : [])
    .filter((option) => canBuiltinOSSandbox
      || text(option?.value) !== SANDBOX_IMPL_DEFAULT)
    .map((option) => ({
      ...option,
      label: fillHarnessPlaceholder(option?.label, displayName),
      descr: fillHarnessPlaceholder(option?.descr, displayName),
    }));
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
  const serverBoundary = !!harness?.tclaude_layer_server_boundary;
  const harnessLabel = text(harness?.display_name) || text(harness?.name);
  // Missing means an older snapshot that predates this capability bit; retain
  // its old catalog rather than hiding a valid option for every harness.
  const canBuiltinOSSandbox = harness?.can_builtin_os_sandbox !== false;
  return {
    showSandboxImpl: !!harness,
    sandboxImplOptions: sandboxImplOptionsFor(catalog.options, harnessLabel, canBuiltinOSSandbox),
    sandboxImplDefault: text(catalog.default) || SANDBOX_IMPL_DEFAULT,
    sandboxImplCanBuiltin: canBuiltinOSSandbox,
    sandboxImplHarness: harnessLabel,
    sandboxImplHarnessName: text(harness?.name),
    sandboxImplCanStacked: !!harness?.can_stacked,
    sandboxImplStackedAvailability: catalog.stacked?.[text(harness?.name)] || {},
    sandboxImplStackedAppArmorLikely: !!catalog.stacked_apparmor_nested_bwrap_likely,
    sandboxImplHostAvailable: serverBoundary
      ? catalog.server_host_available !== false
      : catalog.host_available !== false,
    sandboxImplHostReason: text(serverBoundary
      ? catalog.server_host_unavailable_reason
      : catalog.host_unavailable_reason),
  };
}

// sandboxImplResolvedLabel names the implementation the daemon says a blank row
// would resolve to, using the SAME label the concrete option carries — the
// operator should be able to see that the resolved default is one of the
// choices below it, not a fourth thing.
//
// An answer that matches no offered option returns '': the row then says
// "Resolved default" without naming one. That is not a corner case to paper
// over. A harness with no built-in OS sandbox (OpenCode) resolves a blank field
// to harness-builtin, which its own option list deliberately omits, so naming
// it would advertise a sandbox that harness cannot run. The hint row beneath
// already explains that case in full.
export function sandboxImplResolvedLabel(options, implementation) {
  const value = text(implementation);
  if (!value) return '';
  const match = (Array.isArray(options) ? options : [])
    .find((option) => text(option?.value) === value);
  return match ? text(match.label) : '';
}

// sandboxImplClearedNoticeFor renders the notice shown after a harness switch
// discarded a sandbox-implementation selection.
//
// It exists because the row itself DISAPPEARS in that moment: the row is gated
// on the new harness's can_tclaude_layer, so the one control that could have
// shown the loss is gone at exactly the instant there is something to show. The
// selection is dropped, the launch proceeds with the harness's historical
// default behavior, and nothing anywhere says so.
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
      + "the selection was cleared, so this agent launches with the harness's historical default behavior.",
  };
}

// setSpawnSandboxImpl records an EXPLICIT pick and retires any cleared-notice
// with it: once the operator has spoken for this field again, the notice is
// describing a state that no longer stands.
export function setSpawnSandboxImpl(draft, value) {
  return { ...draft, sandboxImpl: text(value), sandboxImplCleared: null };
}

// Codex approval requests are boundary-crossing requests from its own sandbox.
// When another implementation owns the only wall (or every wall is off), the
// policy and reviewer controls describe prompts Codex cannot produce. Claude
// Code's permission modes are independent of sandboxing, so this gate is
// deliberately Codex-only.
export function approvalControlsVisibleFor(draft, resolvedImplementation = '') {
  if (text(draft?.harness) !== 'codex') return true;
  const explicit = text(draft?.sandboxImpl ?? draft?.sandbox_implementation);
  const implementation = explicit || text(resolvedImplementation);
  const codexOwnsSandbox = implementation === SANDBOX_IMPL_DEFAULT
    || implementation === SANDBOX_IMPL_STACKED;
  return codexOwnsSandbox && text(draft?.sandbox) !== 'danger-full-access';
}

// sandboxImplCaveatFor answers "does the implementation this row selects carry
// a caveat the operator should be able to read?" — the copy the [!] trigger
// beside the selector discloses, NOT copy any dialog prints inline.
//
// Only Codex's built-in sandbox has one today. It is a caveat rather than a
// hint because a blank row reading "Codex CLI built-in" would otherwise
// withhold that this particular implementation cannot enforce a profile's
// TCP/UDP rules; the warning-coloured trigger says a caveat exists, and the
// disclosure says what it is. Keeping the paragraph out of the row is the
// point: it applies to one harness and used to push every control below it off
// the first screen.
//
// resolvedImplementation is the DAEMON's answer for a blank row. Passing
// nothing keeps a blank row silent, which is what the profile editor wants —
// there, the field really is unresolved until spawn.
export function sandboxImplCaveatFor(draft, view, resolvedImplementation = '') {
  if (!view?.showSandboxImpl) return '';
  // A descriptor that says Codex has no built-in OS sandbox at all outranks a
  // caveat about that sandbox's network half: sandboxImplHintFor's "no built-in
  // OS sandbox" branch fires there, and an [!] reading "the built-in filesystem
  // sandbox remains available" beside it would flatly contradict it.
  if (view.sandboxImplCanBuiltin === false) return '';
  const explicit = text(draft?.sandboxImpl);
  const builtinCodex = view.sandboxImplHarnessName === 'codex'
    && (explicit === SANDBOX_IMPL_DEFAULT
      || (!explicit && text(resolvedImplementation) === SANDBOX_IMPL_DEFAULT));
  return builtinCodex ? CODEX_BUILTIN_FILTERED_NETWORK_DETAIL : '';
}

// sandboxImplHintFor renders the note under the sandbox-implementation row.
// Two truths, in the order an operator needs them: what the selected
// implementation actually is, and — when they have selected the experimental
// one on a host that cannot run it — that the launch will REFUSE rather than
// quietly fall back. Saying "will refuse" is the whole point: an operator who
// picks it anyway is choosing a failed launch, not an unnoticed downgrade.
//
// What it deliberately does NOT carry is the Codex built-in caveat; that is
// sandboxImplCaveatFor's, and it rides on the row's disclosure trigger.
//
// resolvedImplementation is the DAEMON's answer for a blank row, used by the
// legacy-off notices below for the same reason the caveat uses it.
export function sandboxImplHintFor(draft, view, resolvedImplementation = '') {
  if (!view.showSandboxImpl) return null;
  const explicit = text(draft.sandboxImpl);
  if (!explicit && harnessBuiltinModeIsOff(draft.harness, draft.sandbox)) {
    const harnessLabel = view.sandboxImplHarness || 'Harness';
    return {
      warn: true,
      text: `Legacy ${harnessLabel} sandbox mode Off is preserved while Sandbox follows `
        + `resolved defaults. Choose Off above to disable every OS sandbox, or choose `
        + `${harnessLabel} built-in to replace the legacy mode.`,
    };
  }
  if (explicit === SANDBOX_IMPL_DEFAULT
    && view.sandboxImplCanBuiltin
    && harnessBuiltinModeIsOff(draft.harness, draft.sandbox)) {
    const harnessLabel = view.sandboxImplHarness || 'Harness';
    return {
      warn: true,
      text: `Legacy ${harnessLabel} built-in + native Off is preserved. Choose a confined `
        + `${harnessLabel} sandbox mode below, or choose Off above to disable every OS `
        + 'sandbox layer explicitly.',
    };
  }
  const value = explicit || view.sandboxImplDefault;
  if (value === SANDBOX_IMPL_OFF) {
    return {
      warn: true,
      text: 'Sandbox OFF. The agent runs without OS-level confinement.',
    };
  }
  /* The other implementation with no confinement. It must warn as loudly as
     Off does about what it does NOT do, while naming the one thing it does. */
  if (value === SANDBOX_IMPL_RESOURCE_ONLY) {
    return {
      warn: true,
      text: 'Sandbox OFF. The agent runs without OS-level confinement; only the '
        + "profile's CPU/memory limits are enforced, in a per-agent cgroup. Any "
        + 'filesystem or network rules in the resolved profile are recorded but '
        + 'NOT enforced. Linux only.',
    };
  }
  if (value === SANDBOX_IMPL_DEFAULT && !view.sandboxImplCanBuiltin) {
    const explicit = text(draft.sandboxImpl) === SANDBOX_IMPL_DEFAULT;
    const harnessLabel = view.sandboxImplHarness || 'this harness';
    return {
      warn: true,
      text: explicit
        ? `harness-builtin is invalid for ${harnessLabel}: ${harnessLabel} has no built-in OS sandbox; `
          + 'its access-control mode is a command filter, not confinement; '
          + "use tclaude's built-in OS sandbox or spawn with the sandbox off."
        : 'No built-in OS sandbox; access-control is a command filter, not confinement.',
    };
  }
  if (value === SANDBOX_IMPL_STACKED) {
    if (!view.sandboxImplCanStacked) {
      return {
        warn: true,
        text: `Stacked is not available for ${view.sandboxImplHarness || 'this harness'}: `
          + 'there is no reviewed nested OS-sandbox contract. Apply or launch will refuse; the selection is preserved.',
      };
    }
    const availability = view.sandboxImplStackedAvailability || {};
    const appArmor = view.sandboxImplStackedAppArmorLikely
      ? { doc: SANDBOX_APPARMOR_DOC }
      : null;
    if (view.sandboxImplHostAvailable && availability.available !== false) {
      // The availability answer above only resolved the engine, so on a host
      // carrying Ubuntu's enforcing bwrap policy it says "available" while the
      // launch probe will refuse. Say so here rather than let the operator
      // discover it from a failed launch — and say "likely", because reading
      // the policy file is not the same as watching the deny.
      if (appArmor) {
        return {
          warn: true,
          ...appArmor,
          text: 'Experimental, and likely blocked on this host: an enforcing '
            + 'bwrap-userns-restrict AppArmor policy denies the nested bwrap, so the launch '
            + 'round-trip will probably refuse. Availability above resolves the engine only.',
        };
      }
      return {
        warn: false,
        text: 'Experimental. Launch performs a fresh model-free allowed/denied round-trip through '
          + "the harness's real nested engine inside the exact tclaude outer boundary.",
      };
    }
    const reason = text(availability.unavailable_reason)
      || view.sandboxImplHostReason
      || 'this host cannot create both sandbox walls';
    return {
      warn: true,
      ...(appArmor || {}),
      // The concrete reason keeps the lead — it is what the probe actually
      // said. The policy is named as a SECOND wall to clear, not as an
      // explanation of this one, because it is neither.
      text: `Not available on this host: ${reason}. Selecting it will refuse the launch, not fall back.`
        + (appArmor
          ? ' An enforcing bwrap-userns-restrict AppArmor policy on this host will likely block'
            + ' the nested sandbox too, once that reason is fixed.'
          : ''),
    };
  }
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

export function spawnCapabilityView(draft, context, resolvedSandboxImpl = '') {
  const harness = findSpawnHarness(context.harnesses, draft.harness);
  const models = Array.isArray(harness?.models) ? harness.models : [];
  const hasModelList = !harness || models.length > 0;
  const sandbox = launchSetting(harness, 'sandbox');
  const approval = launchSetting(harness, 'approval');
  const tools = launchSetting(harness, 'tools');
  const askTimeout = launchSetting(harness, 'askTimeout');
  const selectedSandboxImpl = text(draft.sandboxImpl);
  const resolvedBuiltinSandbox = !selectedSandboxImpl
    && text(resolvedSandboxImpl) === SANDBOX_IMPL_DEFAULT;
  /* Mirrors agentd's sandboxProfilesDisabled, INCLUDING the order in which it
     asks and the VALUE it asks about. Two things make this easy to get wrong:

     - resource-only must be answered before the harness/mode clause, because it
       resolves Codex's no-confinement mode, danger-full-access, which is
       exactly what that clause treats as a profile opt-out.
     - it must be answered about the EFFECTIVE implementation, not the dialog's
       explicit one. The daemon resolves the profile chain into
       body.SandboxImplementation before it reaches its own gate, so a blank
       dialog whose group/global profile pins resource-only is resource-only on
       the server. Asking about draft.sandboxImpl alone would make the client
       send omit_sandbox_profiles where the server would not have omitted — and
       the daemon honours that flag unconditionally, discarding the
       resource_limits on a launch that otherwise succeeds. */
  const effectiveSandboxImpl = selectedSandboxImpl || text(resolvedSandboxImpl);
  const sandboxProfilesDisabled = effectiveSandboxImpl !== SANDBOX_IMPL_RESOURCE_ONLY
    && (effectiveSandboxImpl === SANDBOX_IMPL_OFF
      || (draft.harness === 'codex' && draft.sandbox === 'danger-full-access'));
  const showSSHWorkaround = !!harness?.can_ssh_workaround;
  const sshWorkaroundAvailable = showSSHWorkaround
    && (!draft.sandboxImpl || draft.sandboxImpl === SANDBOX_IMPL_DEFAULT)
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
    showContextWindowMax: harness ? !!harness.can_context_window_max : draft.harness === 'copilot',
    showCopilotAPI: harness ? !!harness.can_copilot_api : draft.harness === 'copilot',
    showFastMode: harness ? !!harness.can_fast_mode : draft.harness === 'codex',
    ...sandboxImplView(harness, context),
    showHarnessBuiltinMode: !!(sandbox.visible && harness?.can_builtin_os_sandbox !== false
      && (selectedSandboxImpl === SANDBOX_IMPL_DEFAULT || resolvedBuiltinSandbox)),
    autoCompactWindowMin: Number(harness?.auto_compact_window_min) || 0,
    autoCompactWindowMax: Number(harness?.auto_compact_window_max) || 0,
    contextWindowMaxMin: Number(harness?.context_window_max_min) || 0,
    contextWindowMaxMax: Number(harness?.context_window_max_max) || 0,
    contextFeatureCatalog: Array.isArray(harness?.context_features) ? harness.context_features : [],
    sandboxProfilesDisabled,
  };
}

// harnessBuiltinModeControlLabel names the nested control after the harness that owns
// it. It is shown when an explicit or resolved-default selection uses the
// harness-builtin implementation; the primary Sandbox selector above it
// chooses the implementation (or Off).
export function harnessBuiltinModeControlLabel(harness) {
  const name = text(harness?.name);
  const label = name === 'codex'
    ? 'Codex'
    : text(harness?.display_name) || name || 'Harness';
  return `${label} sandbox mode`;
}

// harnessBuiltinModeOptionsForImplementation removes the native off spelling from
// new nested-mode choices. A legacy built-in + native-off pair keeps its
// current value visible until the operator changes it; hiding a still-submitted
// controlled-select value would misrepresent an existing profile.
export function harnessBuiltinModeOptionsForImplementation(setting, harnessName, currentMode = '') {
  return {
    ...setting,
    modes: (setting?.modes || []).filter((mode) => (
      !harnessBuiltinModeIsOff(harnessName, mode) || text(mode) === text(currentMode)
    )),
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

// Copilot's configured cap uses the same token-count vocabulary as the
// adjacent Claude auto-compact control (for example, 272k or 0.5M), while the
// wire still carries the canonical integer token count.
export function parseContextWindowMax(raw) {
  return parseAutoCompactWindow(raw);
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

export function contextWindowMaxHintFor(draft, view) {
  const raw = text(draft.contextWindowMax);
  if (!raw) return null;
  const value = parseContextWindowMax(raw);
  if (value == null) return { warn: true, text: 'Use a whole number of tokens, 272k or 0.5M.' };
  const min = view.contextWindowMaxMin || 0;
  const max = view.contextWindowMaxMax || 0;
  if (!Number.isSafeInteger(value) || (min && value < min) || (max && value > max)) {
    return { warn: true, text: `${formatTokenWindow(value)} is outside the accepted range (${formatTokenWindow(min)}–${formatTokenWindow(max)}).` };
  }
  return { warn: false, text: 'Used as a configured cap when set; otherwise the observed model gets a static assumed cap.' };
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
    contextWindowMax: '',
    copilotAPI: false,
    fastMode: '',
    // "" = unset, so the daemon's profile tier stack still speaks. Sending
    // harness-builtin here instead would pin it and silence every lower tier.
    sandboxImpl: '',
    // Dashboard-only, per-open escape hatch. It is never profile-backed or
    // remembered; every fresh dialog and harness switch starts unchecked.
    allowUnenforcedSandbox: false,
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
    contextWindowMax: harness?.can_context_window_max ? draft.contextWindowMax : '',
    // A harness with no API-backed drive keeps the checkbox off rather than
    // sending an opt-in the daemon would reject with a 400.
    copilotAPI: harness?.can_copilot_api ? draft.copilotAPI : false,
    fastMode: harness?.can_fast_mode ? draft.fastMode : '',
    // Every harness keeps the operator's selection visible. An incapable
    // switch becomes an inline refusal warning; the browser never decides by
    // erasing the value before the launch/apply authority can reject it.
    sandboxImpl: draft.sandboxImpl,
    sandboxImplCleared: null,
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
  next.contextWindowMax = view.showContextWindowMax && profile.context_window_max
    ? text(profile.context_window_max) : '';
  // Same rule as auto_memory: a profile that says nothing about the drive clears
  // whatever the previously selected profile put here rather than letting an
  // opt-in ride along onto a profile that never asked for it.
  next.copilotAPI = view.showCopilotAPI && profile.copilot_api != null
    ? !!profile.copilot_api : false;
  next.fastMode = view.showFastMode && profile.fast_mode != null
    ? (profile.fast_mode ? '1' : '0') : '';
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
  // Only the SELECTED profile pre-fills the checkbox. The daemon resolves this
  // toggle down the whole tier stack (named > group default > global default),
  // but the form always posts an explicit include_group_context, and explicit
  // outranks every tier — so on this surface a group/global default profile's
  // toggle stays inert by construction. That asymmetry with the CLI and the
  // agentd TUI is deliberate: the checkbox is on screen, so what the operator
  // sees is what the spawn gets, rather than a visible box a profile silently
  // overrides.
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
    contextWindowMax: defaults.contextWindowMax,
    copilotAPI: defaults.copilotAPI,
    fastMode: defaults.fastMode,
    trustDir: false,
    trustDirSpecified: false,
    remoteControl: defaults.remoteControl,
    autoMemory: false,
    sshWorkaround: !!findSpawnHarness(context.harnesses, defaults.harness)?.can_ssh_workaround,
    autoCompactWindow: defaults.autoCompactWindow,
    sandboxImpl: defaults.sandboxImpl,
    allowUnenforcedSandbox: defaults.allowUnenforcedSandbox,
    sandboxImplCleared: null,
    owner: false,
    permissionOverrides: {},
    contextFeatures: {},
    syncWorktree: defaults.syncWorktree,
    autoFocus: defaults.autoFocus,
    includeGroupContext: true,
  }, false);
}

// Selecting a spawn profile is replacement, not a sparse overlay on the
// previously selected profile. Reset every field a profile can own first, then
// apply the new profile and finally restore explicit per-spawn choices the
// operator made in this open dialog. Location fields are deliberately outside
// clearSpawnProfileFields and survive naturally; callers include explicitly
// touched worktree-selection fields in preservedFields because reset-time sync
// can otherwise clear a pending new-worktree selection.
export function replaceSpawnProfile(draft, profile, context, {
  autoFocus = true,
  rememberedEffort = () => '',
  pickerUsable = false,
  preservedFields = [],
} = {}) {
  const reset = clearSpawnProfileFields(draft, context, { autoFocus, rememberedEffort });
  const applied = applySpawnProfile(
    reset, profile, context, rememberedEffort, pickerUsable,
  );
  const next = { ...applied };
  for (const field of preservedFields) {
    if (field !== 'profile' && Object.hasOwn(draft, field)) next[field] = draft[field];
  }
  return syncSpawnWorktree(next, pickerUsable);
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
  if (view.showCopilotAPI && draft.copilotAPI) seed.copilot_api = true;
  if (view.showFastMode && draft.fastMode !== '') seed.fast_mode = draft.fastMode === '1';
  if (view.showSSHWorkaround) seed.ssh_workaround = !!draft.sshWorkaround;
  if (view.showAutoCompactWindow && text(draft.autoCompactWindow)) {
    seed.auto_compact_window = text(draft.autoCompactWindow);
  }
  if (view.showContextWindowMax && text(draft.contextWindowMax)) {
    seed.context_window_max = parseContextWindowMax(draft.contextWindowMax);
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
  'approvalReviewer', 'tools', 'askTimeout', 'autoCompactWindow', 'contextWindowMax', 'copilotAPI', 'fastMode', 'sandboxImpl', 'allowUnenforcedSandbox', 'trustDir', 'trustDirSpecified', 'remoteControl', 'autoMemory', 'sshWorkaround', 'owner',
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
  if (view.showContextWindowMax) {
    const maxHint = contextWindowMaxHintFor(draft, view);
    if (maxHint?.warn) return maxHint.text;
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
  // Tell the daemon not to pop a native window; its ordinary browser-focus
  // response then lets the dashboard attach the new pane in the Terminals tab.
  if (draft.autoFocus && context.defaultTerminal === 'web') body.auto_focus_web = true;
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
  // Sent as an explicit true/false for a Copilot launch — like auto_memory, the
  // modal is authoritative, so an unchecked box overrides a profile's opt-in.
  // Omitted entirely for every other harness, leaving the pointer nil.
  if (view.showCopilotAPI) body.copilot_api = !!draft.copilotAPI;
  if (view.showFastMode) {
    body.fast_mode = draft.fastMode === '1' ? 'on' : draft.fastMode === '0' ? 'off' : 'inherit';
  }
  if (view.showSSHWorkaround) {
    body.ssh_workaround = !!(view.sshWorkaroundAvailable && draft.sshWorkaround);
  }
  // Blank omits the key so the daemon's profile tier stack still speaks; the
  // daemon normalizes "450k" to plain digits, so the raw field text is sent.
  if (view.showAutoCompactWindow && text(draft.autoCompactWindow)) {
    body.auto_compact_window = text(draft.autoCompactWindow);
  }
  if (view.showContextWindowMax && text(draft.contextWindowMax)) {
    body.context_window_max = parseContextWindowMax(draft.contextWindowMax);
  }
  // Blank omits the key, so an untouched row leaves the daemon's profile tier
  // stack in charge and the launch stays default-off. An explicit selection —
  // including harness-builtin — is sent, because pinning the legacy layer
  // against a group default that would have flipped it is a real intent.
  if (view.showSandboxImpl && text(draft.sandboxImpl)) {
    body.sandbox_implementation = text(draft.sandboxImpl);
  }
  // This bit has no profile/config/default representation. Sending true is the
  // one per-open dashboard authorization; false is omitted so an untouched
  // dialog follows the daemon's ordinary fail-closed path exactly.
  if (draft.allowUnenforcedSandbox) {
    body.allow_unenforced_sandbox = true;
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
