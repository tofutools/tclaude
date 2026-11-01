import { claimField } from './form-fill.js';
import { FormFillBar } from './form-fill-bar.js';
import { SavedPromptChips } from './saved-prompts.js';
import { openUsageRulesManager } from './modal-usage-rules.js';
import { dashboardState } from './snapshot-store.js';
import { Fragment, h, render } from 'preact';
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { TransferTray } from './transfer-card.js';
import { LockReason } from './lock-reason.js';
import { isPageNode } from './node-api.js';
import { profileSaveNodes } from './profile-save-nodes.js';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { relTime, shortCwd } from './helpers.js';
import {
  CODEX_NATIVE_REGISTRY_SETUP_DOC,
  MODEL_CUSTOM_VALUE,
  SANDBOX_PROFILE_NONE,
  WT_NEW,
  approvalControlsVisibleFor,
  applySpawnProfile,
  attachKey,
  buildSpawnRequest,
  clearSpawnProfileFields,
  createSpawnDraft,
  deriveSpawnNameFromMessage,
  findSpawnHarness,
  findSpawnProfile,
  formatAttachmentSize,
  groupDefaultProfileName,
  groupDefaultPrompts,
  groupHasContext,
  modelSelectValue,
  nativeCodexRegistryWarningFor,
  prepareSpawnDraft,
  replaceSpawnProfile,
  spawnUserText,
  selectedDefaultProfile,
  selectSpawnGroup,
  selectSpawnHarness,
  selectSpawnWorktree,
  setSpawnCwd,
  setSpawnWorktreeRepo,
  spawnCapabilityView,
  spawnDraftIsDirty,
  spawnModelDefaultLabel,
  spawnNameHint,
  spawnContextFeatureIndicator,
  spawnPermissionIndicator,
  spawnProfileChoices,
  spawnProfileSeed,
  syncSpawnWorktree,
  validateSpawnDraft,
  autoCompactWindowHintFor,
  contextWindowMaxHintFor,
  harnessBuiltinModeHelpForImplementation, approvalCaveatVisibleFor,
  harnessBuiltinModeControlLabel,
  harnessBuiltinModeOptionsForImplementation,
  sandboxImplHintFor,
  sandboxImplCaveatFor,
  sandboxImplClearedNoticeFor,
  sandboxImplResolvedLabel,
  setSpawnSandboxImpl,
} from './agent-spawn-model.js';
import { registerAgentSpawnController } from './agent-spawn-controller.js';
import {
  createRemoteSpawnActions, isLocalRef, remotePromptBodies, remotePromptSetOpen, remoteSpawnIntro, remoteSpawnPins,
  remoteSpawnPrompts, watchRemoteSpawnLocks,
} from './agent-spawn-remote.js';
import { shellToast } from './shell-state.js';
import { approvalPolicyLabel, approvalReviewerHelp, approvalReviewerOptions } from './approval-controls.js';
import { HelpDisclosure, HelpField } from './help-field.js';
import { SandboxImplHint } from './sandbox-impl-hint.js';
import { isWizardActive } from './slop.js';
import { shareHint, shareKey, shareOptions } from './usage-share-model.js';
import {
  RESOLVED_DEFAULTS_CHAIN, SANDBOX_PROFILE_COMPOSITION,
  resolvedDefaultOption, harnessBuiltinModeOptionLabel,
} from './resolved-defaults.js';

const html = htm.bind(h);
// A harness change clears context trims, which no field shows, so a fill of it waits.
const SPAWN_CONTEXT_CLAIM = ['agent-spawn-harness'];
const NO_CLAIMS = [];
const NEW_USAGE_PROFILE = '/new-usage-profile';

// liveUsageProfiles reads the usage profiles off the live snapshot rather
// than the list frozen at open, so one made through "new usage profile…"
// is pickable without reopening the dialog (and the draft stays as it is).
function liveUsageProfiles(frozen) {
  const live = dashboardState.snapshot.value?.usage?.budgets?.profiles;
  return live ? live.map((p) => p.name) : frozen;
}
const PASTE_REPEAT_MS = 1000;
// One title for the auto-compaction window control, shared by the spawn dialog
// and kept next to the hint logic it explains. The two consequences an operator
// has to know are the cap and the status-line decoupling.
const AUTO_COMPACT_WINDOW_TITLE = 'Context capacity in tokens for Claude Code\'s auto-compaction '
  + '(CLAUDE_CODE_AUTO_COMPACT_WINDOW). Accepts 450000, 450k or 0.5M; blank uses the model default. '
  + 'Pin it below a 1M model\'s real window so a long-lived agent compacts while it is still sharp. '
  + 'Capped at the model\'s actual context window.';
const CONTEXT_WINDOW_MAX_TITLE = 'Configured/assumed context cap for the Copilot context meter. '
  + 'Copilot does not report its context limit; a blank value uses the observed model\'s static assumption.';
const COPILOT_API_TITLE = 'EXPERIMENTAL: drive this Copilot agent over its embedded JSON-RPC API '
  + '(copilot --ui-server) instead of typing into its pane with tmux send-keys. Messages, rename and '
  + 'compaction become typed calls, context is read live, and an agent blocked on a permission prompt '
  + 'becomes visible; soft exit still uses keystrokes. The spawn is REFUSED unless the launch directory is already trusted '
  + '(or Pre-trust is checked) and the pane shares host loopback. The endpoint is unauthenticated and '
  + 'loopback-bound. Off by default — the two drives run side by side and agents move over one at a '
  + 'time. Copilot only.';
const ENVIRONMENT_OVERRIDE_HELP = 'Explicit values override matching profile, group, and sandbox-profile variables. '
  + 'The resolved values are frozen for resume, restart, clone, and reincarnate.';
const ENVIRONMENT_OVERRIDE_HELP_WIZARD = 'Explicit runes override matching loadout, party, and ward runes. '
  + 'The resolved spellbook is sealed for awakening, reincanting, mirroring, and reincarnation.';
const EFFECTIVE_ENVIRONMENT_HELP = 'Common configured values shown here; sandbox-profile variables are '
  + 'additionally resolved by the daemon at launch.';
const EFFECTIVE_ENVIRONMENT_HELP_WIZARD = 'Common bound runes are shown here; ward runes are additionally '
  + 'divined by the daemon during summoning.';
// Names WHO enforces the wall, which the Sandbox row above does not: that row
// picks a mode within whatever sandbox is in force, this one picks which
// sandbox that is. The platform requirements are stated rather than implied
// — a launch that cannot meet them refuses.
const SANDBOX_IMPL_TITLE = 'Which sandbox provides OS-level containment for the new agent. '
  + 'harness-builtin is offered only when the selected harness owns a real OS sandbox. '
  + "tclaude’s built-in sandbox runs the whole "
  + "harness process inside a tclaude-owned bubblewrap namespace and turns the harness's own "
  + 'sandbox off inside it. Linux only, and it needs bwrap plus unprivileged user namespaces — '
  + 'a host without them refuses the launch instead of falling back. '
  + `Blank uses the resolved defaults. ${RESOLVED_DEFAULTS_CHAIN}`;
const UNENFORCED_SANDBOX_TITLE = 'Operator-only escape hatch. If closed network access or individual '
  + 'network deny rules cannot be enforced, launch with those rules unenforced. Other enforceable '
  + 'filesystem, network, and Unix-socket rules still apply. This choice is not saved and starts '
  + 'unchecked every time.';
const PROFILE_OWNED_FIELDS = [
  'profile', 'name', 'role', 'roleRefs', 'promptRefs', 'descr', 'task', 'initialMessage',
  'harness', 'model', 'customModel', 'effort', 'sandbox', 'approval', 'approvalReviewer', 'tools', 'askTimeout',
  'trustDir', 'trustDirSpecified', 'remoteControl', 'autoMemory', 'peerMessaging', 'sshWorkaround', 'owner', 'permissionOverrides',
  'contextFeatures', 'autoCompactWindow', 'contextWindowMax', 'copilotAPI', 'codexAppServer', 'fastMode', 'sandboxImpl', 'sandboxImplCleared',
  'syncWorktree', 'fetchLatestWorktree', 'autoFocus', 'includeGroupContext',
];
const WORKTREE_SELECTION_FIELDS = ['worktree', 'worktreeBranch', 'worktreeBase'];

function errorMessage(error) {
  return error?.message || String(error);
}

// Identifies which group/profile selection (and profile-library revision) a
// resolved sandbox policy belongs to, so submit can refuse a policy loaded
// for a different selection.
const STALE_POLICY_ERROR = 'the sandbox policy changed while spawning — review the refreshed preview and submit again';

function sandboxPolicyKey(group, sandboxProfile, revision) {
  return JSON.stringify([group, sandboxProfile, revision]);
}

function Words({ plain, wizard, prefix = 'theme-copy' }) {
  return html`<span class=${`${prefix}-regular`}>${plain}</span><span class=${`${prefix}-wizard`}>${wizard}</span>`;
}

function useWizardTheme() {
  const [wizard, setWizard] = useState(isWizardActive());
  useEffect(() => {
    const update = (event) => setWizard(
      event.detail?.active == null ? isWizardActive() : Boolean(event.detail.active),
    );
    document.addEventListener('tclaude:wizard', update);
    return () => document.removeEventListener('tclaude:wizard', update);
  }, []);
  return wizard;
}

function ErrorBanner({ error, onDismiss }) {
  const ref = useRef(null);
  useEffect(() => {
    const element = ref.current;
    if (!element || !error) return undefined;
    element.classList.remove('flash');
    void element.offsetWidth;
    element.classList.add('flash');
    element.scrollIntoView({ block: 'nearest' });
    const timer = setTimeout(() => element.classList.remove('flash'), 900);
    return () => clearTimeout(timer);
  }, [error]);
  if (!error) return html`<div ref=${ref} class="cron-create-error" id="agent-spawn-error" role="alert"></div>`;
  return html`<div ref=${ref} class="cron-create-error dismissible" id="agent-spawn-error" role="alert">
    <span class="cron-create-error-msg">${error}</span>
    <button type="button" class="cron-create-error-x" aria-label="Dismiss error" onClick=${onDismiss}>×</button>
  </div>`;
}

// optionLabel lets a setting rewrite a mode token that says nothing to a human —
// today only Claude's sandbox `inherit`. It owns the "(recommended)" suffix too,
// so a rewritten label ending in its own parenthetical does not collect a second
// one. Every other setting renders its tokens, which already name their effect.
function SettingOptions({ setting, optionLabel = null }) {
  return setting.modes.map((mode) => ({
    value: mode,
    label: optionLabel
      ? optionLabel(mode, setting.recommended)
      : `${mode}${mode === setting.recommended ? ' (recommended)' : ''}`,
  }));
}

// storedQueuedAttachments lists a reopened queued spawn's stored attachments
// as dialog attachment entries (no File: they stay on the daemon).
function storedQueuedAttachments(queued) {
  const stored = Array.isArray(queued?.draft?.attachments) ? queued.draft.attachments : [];
  return stored.filter((entry) => entry?.name).map((entry, index) => ({
    id: -(index + 1), name: String(entry.name), size: Number(entry.size) || 0, url: '',
  }));
}

function AttachmentList({ attachments, remove, busy }) {
  return html`<ul class="spawn-attachments-list" id="agent-spawn-attachments-list">
    ${attachments.map((attachment) => html`<li key=${attachment.id}>
      ${attachment.url
        ? html`<img class="att-thumb" src=${attachment.url} alt="" />`
        : html`<span class="att-icon">📄</span>`}
      <span class="att-name" title=${attachment.name}>${attachment.name}</span>
      <span class="att-size">${formatAttachmentSize(attachment.size)}</span>
      <button type="button" class="att-remove" title="Remove" disabled=${busy}
        aria-label=${`Remove ${attachment.name}`} onClick=${() => remove(attachment.id)}>✕</button>
    </li>`)}
  </ul>`;
}

function roleGrantSlug(grant) {
  return typeof grant === 'string' ? grant : String(grant?.slug || '');
}

function roleGrantScope(grant) {
  if (!grant || typeof grant !== 'object' || !grant.scope) return '';
  if (typeof grant.scope === 'string') return grant.scope;
  return Object.entries(grant.scope).map(([key, values]) => `${key}=${(values || []).join(',')}`).join(' · ');
}

function roleHoverText(role) {
  const grants = (role?.permissions || []).map((grant) => {
    const scope = roleGrantScope(grant);
    return `${roleGrantSlug(grant)}${scope ? ` (${scope})` : ''}`;
  });
  return [role?.descr, role?.brief, grants.length ? `Grants: ${grants.join(', ')}` : 'Grants: none']
    .filter(Boolean).join('\n\n');
}

// RemoteProfileOptions lists a remote spawn's profiles by library: the
// target's (a profile the grant does not let this operator pick shows, but
// cannot be chosen) and this node's. The selected one always shows.
function RemoteProfileOptions({ profiles, remote, showFrom, selected }) {
  const shown = (source) => profiles.filter((profile) => profile.remoteSource === source
    && (showFrom.includes(source) || profile.name === selected));
  const label = (profile) => `${profile.localName || profile.name}${profile.disabled ? ' [🚫 disabled]' : ''}`;
  const locked = (profile) => (profile.name === selected ? ` (${remote.node}'s default)` : ' (not in your grant)');
  const target = shown('target');
  const local = shown('local');
  return html`
    ${target.length ? html`<optgroup label=${`${remote.node}'s profiles`}>${target.map((profile) => html`<option key=${profile.name} value=${profile.name}
      disabled=${!profile.remoteSelectable}
      title=${profile.remoteSelectable ? '' : `${remote.node}'s grant does not let you pick this profile${profile.name === selected ? `; ${remote.node} applies it by default` : ''}`}>${label(profile)}${profile.remoteSelectable ? '' : locked(profile)}</option>`)}</optgroup>` : null}
    ${local.length ? html`<optgroup label="This node's profiles (fills what the request carries)">${local.map((profile) => html`<option key=${profile.name} value=${profile.name}>${label(profile)}</option>`)}</optgroup>` : null}`;
}

// RemoteSourceFilter picks which libraries a remote spawn's profile and
// prompt pickers list: the target's, this node's, or both.
function RemoteSourceFilter({ remote, showFrom, setShowFrom }) {
  const hasTarget = !!remote.catalogs?.target || (remote.options?.profiles?.length || 0) > 0;
  const toggle = (source) => setShowFrom((value) => (value.includes(source)
    ? (value.length > 1 ? value.filter((entry) => entry !== source) : value)
    : [...value, source]));
  const box = (source, text, title) => html`<label class="agent-spawn-source-toggle" title=${title}>
    <input type="checkbox" id=${`agent-spawn-source-${source}`} checked=${showFrom.includes(source)} onChange=${() => toggle(source)} /> ${text}</label>`;
  return html`<div class="cron-create-row agent-spawn-source-filter" id="agent-spawn-source-filter" data-remote-spawn-free="">
    <span class="cron-create-label">Show from</span>
    <div class="cron-create-target">
      ${box('target', remote.node, hasTarget ? `List ${remote.node}'s profiles and prompts` : `${remote.node} does not share its profiles and prompts with you (node.spawn.read)`)}
      ${box('local', 'this node', "List this node's own profiles and prompts")}
      ${remote.catalogs?.target ? null : html`<span class="muted" title=${`${remote.node} has not granted you node.spawn.read: its own defaults still apply, but this dialog cannot show them or offer its profiles and prompts`}>${remote.node} does not share its libraries with you</span>`}
    </div>
  </div>`;
}

function AgentSpawnDialog({ current, state, actions: localActions, confirmDiscard, notify = shellToast }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  // A spawn into a peer's group (agent-spawn-remote.js) keeps this dialog
  // but sends a remote spawn request and never touches this node's state.
  const remote = current.remote || null;
  // Without a spawn grant the peer's operator decides it: a request.
  const requestOnly = remote?.options?.approval === 'operator';
  // A spawn on another node under unrestricted trust (current.target) runs
  // as that node's own dialog would, through its routes (actions.forNode).
  const target = current.target || null;
  const actions = useMemo(() => (remote ? createRemoteSpawnActions(localActions, remote, { notify })
    : target ? localActions.forNode(target) : localActions), [remote, target, localActions]);
  const away = !!target;
  // "Save as profile…" saves on the spawn's node or the page's, where this
  // operator may create profiles; the profile editor offers the choice.
  const spawnNode = remote ? remote.nodeId || '' : target && !isPageNode(target.node) ? target.node : '';
  const saveNodes = useMemo(() => (spawnNode ? profileSaveNodes(spawnNode, remote ? remote.node : target?.name) : null), [spawnNode, remote, target]);
  const canSaveProfile = !saveNodes || saveNodes.length > 0;
  const awayHint = away ? `${target.name || 'That node'}'s folders are browsed from its own dashboard; type the path` : '';
  // Disable what the remote request cannot carry, for as long as the dialog
  // lives (see watchRemoteSpawnLocks): fail-closed for controls added later too.
  useLayoutEffect(() => (remote
    ? watchRemoteSpawnLocks(globalThis.document?.getElementById('agent-spawn-modal'), remote)
    : undefined), [remote]);
  const context = useMemo(() => ({
    groups: current.groups,
    harnesses: current.harnesses,
    roles: current.roles,
    sandboxImpl: current.sandboxImpl,
    userDefaultModel: current.userDefaultModel,
    usageProfiles: current.usageProfiles || [],
    defaultTerminal: current.defaultTerminal,
    normalizeNames: current.normalizeNames,
    remoteSpawn: !!(current.remote || current.target),
  }), [current]);
  const rememberedEffort = (model) => actions.rememberedEffort(model);
  // A reopened queued spawn restores its stored draft over a fresh one, so a
  // field added to the form since it was queued still gets its default. Its
  // attachments live in the attachment list, not the draft.
  const queued = current.options.queued;
  const initial = useMemo(() => {
    if (queued) {
      const { attachments: _stored, ...queuedDraft } = queued.draft;
      return {
        ...createSpawnDraft({
          groups: context.groups,
          harnesses: context.harnesses,
          groupName: queued.draft.group,
          autoFocus: actions.autoFocusDefault(),
          rememberedEffort,
        }),
        ...queuedDraft,
      };
    }
    const value = createSpawnDraft({
      groups: context.groups,
      harnesses: context.harnesses,
      groupName: current.options.groupName,
      defaultGroup: current.options.defaultGroup,
      autoFocus: actions.autoFocusDefault(),
      rememberedEffort,
    });
    return current.options.role ? { ...value, role: current.options.role } : value;
  }, [current]);
  const [draft, setDraft] = useState(initial);
  const [baseline, setBaseline] = useState(initial);
  const [profiles, setProfiles] = useState([]);
  const [worktrees, setWorktrees] = useState({
    phase: 'loading', repo: initial.wtRepo, isRepo: false, hasCommits: true,
    worktrees: [], branches: [], defaultBranch: '', subRepos: [],
  });
  const [sandboxPolicy, setSandboxPolicy] = useState({ profiles: [], preview: '', error: '', key: '' });
  // What the daemon says a blank sandbox-implementation row would resolve to
  // for this group + harness. The request key makes answers ineligible after
  // their inputs change; the derived value below is null while unknown, which
  // the row renders as an unnamed resolved default rather than a guess.
  const [launchDefaultsAnswer, setLaunchDefaultsAnswer] = useState({
    key: '', value: null,
  });
  // Live mirror for the submit closure: after its awaits, the captured
  // sandboxPolicy binding is stale, but revalidation must see the latest.
  const sandboxPolicyRef = useRef(sandboxPolicy);
  sandboxPolicyRef.current = sandboxPolicy;
  // Messages about the posture this draft would actually launch with: warnings
  // for risky pairings and informational disclosures for safe but non-obvious
  // boundaries. Advisory only: neither kind gates submit, and the daemon repeats
  // them on the spawn response regardless.
  const [autonomyWarnings, setAutonomyWarnings] = useState([]);
  const [sandboxInfo, setSandboxInfo] = useState([]);
  // A local attachment carries the picked File; a queued spawn's stored one
  // only its name and size, and is re-staged from the queue at launch.
  const [attachments, setAttachments] = useState(() => storedQueuedAttachments(queued));
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  // Busy with a queue save rather than a launch: the Spawn button must not
  // read "Spawning…" while nothing is being spawned.
  const [queueing, setQueueing] = useState(false);
  const [progress, setProgress] = useState('');
  const [browseBusy, setBrowseBusy] = useState('');
  const [helpOpen, setHelpOpen] = useState('');
  const [effectiveEnvironmentOpen, setEffectiveEnvironmentOpen] = useState(false);
  const wizard = useWizardTheme();
  const [inspectedRole, setInspectedRole] = useState('');
  const [dragOver, setDragOver] = useState(false);
  const nameRef = useRef(null);
  const fileRef = useRef(null);
  const touched = useRef(new Set());
  // Fields the operator explicitly changed in this open dialog remain
  // per-spawn overrides when they switch profiles. This is separate from
  // `touched`, which also guards derived changes against the initial async
  // profile load (for example a group change recalculating remote control).
  const profileOverrides = useRef(new Set());
  // Launch settings the operator set by hand. A remote spawn sends only
  // these as overrides: `touched` also covers values a harness, model or
  // group change merely pre-fills, which must leave the peer's policy in
  // charge.
  const operatorSet = useRef(new Set());
  // Remote spawn: the saved prompts the peer applies itself (its profile and
  // group defaults, pre-filled and locked), the pre-fill's set to return to
  // on Clear, and which libraries the profile and prompt pickers list.
  const [peerPrompts, setPeerPrompts] = useState([]);
  const basePeerPrompts = useRef([]);
  const [showFrom, setShowFrom] = useState(['target', 'local']);
  const remotePrompts = useMemo(() => (remote ? remoteSpawnPrompts(remote) : null), [remote]);
  // With groups.members.spawn.prompts the prompt set rides by name, so the
  // peer's own prompts can be dropped like any other.
  const remotePromptsOpen = remotePromptSetOpen(remote);
  const submitLock = useRef(false);
  const busyRef = useRef(false);
  const profileRequest = useRef(0);
  const worktreeRequest = useRef(0);
  const sandboxRequest = useRef(0);
  const launchDefaultsRequest = useRef(0);
  const autonomyRequest = useRef(0);
  const directoryRequest = useRef(0);
  const worktreesRef = useRef(worktrees);
  const draftRef = useRef(draft);
  const attachSequence = useRef(0);
  const attachmentsRef = useRef([]);
  const pasteState = useRef({ at: 0, keys: new Set() });
  const dragDepth = useRef(0);
  const uploaded = useRef({ key: '', paths: [] });
  const resolvedWorktree = useRef({ key: '', value: null });

  attachmentsRef.current = attachments;
  worktreesRef.current = worktrees;
  draftRef.current = draft;
  const launchDefaultsKey = JSON.stringify([
    draft.group, draft.profile, draft.harness,
  ]);
  // A draft change renders before its effect can clear the previous answer.
  // Reject the old key during that render so a built-in result for the prior
  // group/profile/harness cannot reveal the wrong mode for one paint.
  const launchDefaults = launchDefaultsAnswer.key === launchDefaultsKey
    ? launchDefaultsAnswer.value
    : null;
  const view = spawnCapabilityView(draft, context, launchDefaults?.implementation);
	const effectiveEnvironment = useMemo(() => {
		const byName = new Map();
		const add = (entries, source) => (entries || []).forEach((entry) => {
			if (entry?.name) byName.set(entry.name, { ...entry, source });
		});
		const group = (context.groups || []).find((entry) => entry.name === draft.group);
		add(group?.environment, `group ${draft.group}`);
		const handles = [actions.dashboardDefaultProfile(), group?.default_profile, draft.profile].filter(Boolean);
		for (const handle of handles) {
			const profile = findSpawnProfile(profiles, handle);
			add(profile?.environment, `profile ${profile?.name || handle}`);
		}
		add(draft.environment, 'this spawn');
		return [...byName.values()].sort((left, right) => left.name.localeCompare(right.name));
	}, [draft.group, draft.profile, draft.environment, context.groups, profiles]);
  const attachmentsChanged = attachments.some((attachment) => attachment.file)
    || attachments.length !== storedQueuedAttachments(queued).length;
  const dirty = spawnDraftIsDirty(draft, baseline, attachmentsChanged ? 1 : 0);
  const nameHint = spawnNameHint(draft.name, context.normalizeNames);
  const permissionsLabel = spawnPermissionIndicator(draft.permissionOverrides);
  const selectedRoles = (draft.roleRefs || []).map((name) =>
    (context.roles || []).find((role) => role.name === name) || { name, missing: true });
  const inspected = selectedRoles.find((role) => role.name === inspectedRole);
  const contextFeaturesLabel = spawnContextFeatureIndicator(draft.contextFeatures);

  useEffect(() => () => {
    profileRequest.current += 1;
    worktreeRequest.current += 1;
    sandboxRequest.current += 1;
    launchDefaultsRequest.current += 1;
    directoryRequest.current += 1;
    for (const attachment of attachmentsRef.current) {
      if (attachment.url) URL.revokeObjectURL(attachment.url);
    }
    attachmentsRef.current = [];
  }, []);

  // Usage sharing: model gateways peers let this node's agents bill their
  // model calls to. Only a spawn on this node can use one (the new agent's
  // gateway bridge runs here), and the row shows only once a peer shares.
  const [usageShares, setUsageShares] = useState([]);
  // Running as yourself elsewhere: an agent spawned on another node may run
  // Claude Code on this operator's own subscription token, stored on this
  // node and sent in the encrypted spawn request only. Asked of this node
  // (localActions), whichever node the dialog spawns on.
  const [subscription, setSubscription] = useState(null);
  useEffect(() => {
    if (!(remote || target) || !localActions.loadClaudeSubscription) return undefined;
    let off = false;
    // A reopened draft keeps its choice only while a token is stored, so a
    // hidden or disabled row never sends it.
    const settle = (value) => {
      if (off) return;
      setSubscription(value);
      if (!value?.set) setDraft((before) => (before.claudeSubscription ? { ...before, claudeSubscription: false } : before));
    };
    localActions.loadClaudeSubscription().then((value) => settle(value || { set: false })).catch(() => settle(null));
    return () => { off = true; };
  }, [remote, target, localActions]);

  useEffect(() => {
    if (remote || target || !actions.loadUsageShares) return undefined;
    let off = false;
    actions.loadUsageShares().then((shares) => {
      if (off) return;
      setUsageShares(shares);
      // A draft (a queued one too) keeps a copy of its share; judge it by
      // what the peer says now, and drop it once the peer stopped sharing.
      setDraft((before) => {
        if (!before.usageFrom) return before;
        const fresh = shares.find((s) => shareKey(s) === before.usageFrom) || null;
        return { ...before, usageFrom: fresh ? before.usageFrom : '', usageShare: fresh };
      });
    }).catch(() => {});
    return () => { off = true; };
  }, [remote, target, actions]);

  useEffect(() => {
    const request = ++profileRequest.current;
    const generation = current.generation;
    actions.loadProfiles().then((nextProfiles) => {
      if (request !== profileRequest.current || !state.isCurrent(generation)) return;
      setProfiles(nextProfiles);
      // A queued draft already carries the profile it was prepared with;
      // re-applying defaults here would overwrite what the operator queued.
      if (queued) return;
      // Legacy parity: a default captured for the group at open must never
      // bleed into a different group selected while the profile request ran.
      if (draftRef.current.group !== initial.group) return;
      const handle = selectedDefaultProfile({
        groups: context.groups,
        groupName: initial.group,
        dashboardDefault: actions.dashboardDefaultProfile(),
        override: current.options.profileName,
      });
      const profile = findSpawnProfile(nextProfiles, handle);
      let nextBaseline = profile
        ? applySpawnProfile(
          { ...initial, profile: handle }, profile, context,
          rememberedEffort, worktreesRef.current.isRepo,
        )
        : initial;
      // Only a named or group profile outranks the group's own default
      // prompts; the dashboard-wide default profile ranks below them, as it
      // does on the daemon.
      const groupPrompts = groupDefaultPrompts(context.groups, initial.group);
      if (profile && groupPrompts.length && !String(current.options.profileName || '').trim()
        && !groupDefaultProfileName(context.groups, initial.group)) {
        nextBaseline = { ...nextBaseline, promptRefs: groupPrompts };
      }
      // Apply the palette's explicit speed choice after the saved profile.
      if (profile && current.options.fastMode && spawnCapabilityView(nextBaseline, context).showFastMode) {
        nextBaseline = { ...nextBaseline, fastMode: '1' };
      }
      if (current.options.role) {
        nextBaseline = { ...nextBaseline, role: current.options.role };
      }
      if (remote) {
        nextBaseline = remoteSpawnPins(nextBaseline, remote.options);
        basePeerPrompts.current = [...(nextBaseline.promptRefs || [])];
        setPeerPrompts(basePeerPrompts.current);
      }
      setBaseline(nextBaseline);
      setDraft((existing) => {
        const merged = { ...nextBaseline };
        for (const key of touched.current) merged[key] = existing[key];
        return syncSpawnWorktree(merged, worktreesRef.current.isRepo);
      });
    }).catch(() => {
      if (request === profileRequest.current && state.isCurrent(generation)) setProfiles([]);
    });
  }, []);

  useEffect(() => {
    const request = ++worktreeRequest.current;
    const generation = current.generation;
    setWorktrees({
      phase: 'loading', repo: draft.wtRepo, isRepo: false, empty: !draft.wtRepo.trim(),
      hasCommits: true, repoRoot: '', worktrees: [], branches: [], defaultBranch: '', subRepos: [],
    });
    const timer = setTimeout(() => {
      actions.loadWorktrees(draft.wtRepo).then((data) => {
        if (request !== worktreeRequest.current || !state.isCurrent(generation)) return;
        setWorktrees({ phase: 'ready', ...data });
        setBaseline((value) => value.wtRepo === data.repo
          ? syncSpawnWorktree(value, data.isRepo) : value);
        setDraft((value) => {
          let next = value;
          if (!value.worktreeBase && data.defaultBranch) {
            next = { ...next, worktreeBase: data.defaultBranch };
          }
          return syncSpawnWorktree(next, data.isRepo);
        });
      });
    }, 350);
    return () => clearTimeout(timer);
  }, [draft.wtRepo]);

  useEffect(() => {
    const request = ++sandboxRequest.current;
    const generation = current.generation;
    if (view.sandboxProfilesDisabled) {
      setSandboxPolicy((value) => ({ ...value, preview: '', error: '', key: '' }));
      return undefined;
    }
    // The stored key records which selection the resolved policy belongs to.
    // Clearing it up front makes a stale policy — one loaded for a previous
    // group/profile pick, or one still in flight — ineligible for submit
    // until the matching load lands. The revision is read LIVE from the
    // dialog signal (not the render prop) so a load triggered before a
    // revision bump has re-rendered this component still stores the key the
    // submit gates will compare against.
    const policyKey = sandboxPolicyKey(draft.group, draft.sandboxProfile, state.dialog.value?.sandboxRevision);
    setSandboxPolicy((value) => ({ ...value, error: '', key: '' }));
    actions.loadSandboxPolicy(draft.group, draft.sandboxProfile).then((value) => {
      if (request !== sandboxRequest.current || !state.isCurrent(generation)) return;
      setSandboxPolicy({ ...value, error: '', key: policyKey });
      if (value.selected !== draft.sandboxProfile) {
        setDraft((before) => ({ ...before, sandboxProfile: value.selected }));
      }
    }).catch((cause) => {
      if (request !== sandboxRequest.current || !state.isCurrent(generation)) return;
      setSandboxPolicy((value) => ({
        ...value,
        preview: `Could not preview sandbox policy: ${errorMessage(cause)}`,
        error: errorMessage(cause),
        key: '',
      }));
    });
    return undefined;
  }, [draft.group, draft.sandboxProfile, view.sandboxProfilesDisabled, current.sandboxRevision]);

  // Ask the daemon what a blank implementation row resolves to. Keyed on the
  // three inputs that change the answer: the selected spawn profile (which the
  // spawn request carries and which outranks the ambient tiers), the group
  // (which selects those tiers), and the harness (which decides what is even
  // valid). The request counter drops an answer that lands after a newer pick,
  // and a failure clears the label rather than leaving a previous selection's
  // answer on screen under a new one.
  useEffect(() => {
    if (!view.showSandboxImpl || typeof actions?.loadLaunchDefaults !== 'function') {
      setLaunchDefaultsAnswer({ key: '', value: null });
      return undefined;
    }
    const request = ++launchDefaultsRequest.current;
    const generation = current.generation;
    const answerKey = launchDefaultsKey;
    setLaunchDefaultsAnswer({ key: '', value: null });
    Promise.resolve(actions.loadLaunchDefaults(
      draft.group, draft.profile, draft.harness,
    )).then((value) => {
      if (request !== launchDefaultsRequest.current || !state.isCurrent(generation)) return;
      setLaunchDefaultsAnswer({ key: answerKey, value: value || null });
    }).catch(() => {
      if (request !== launchDefaultsRequest.current || !state.isCurrent(generation)) return;
      setLaunchDefaultsAnswer({ key: '', value: null });
    });
    return undefined;
  }, [draft.group, draft.profile, draft.harness, view.showSandboxImpl, launchDefaultsKey]);

  // Re-probe the effective sandbox whenever an input to it changes. CWD is a
  // free-text field, so the same 350ms debounce the worktree-repo probe uses
  // keeps a typed path from firing a request per keystroke; the other three are
  // selects, and the debounce is imperceptible there. The request counter drops
  // a stale answer that lands after a newer one.
  useEffect(() => {
    const request = ++autonomyRequest.current;
    const generation = current.generation;
    const timer = setTimeout(() => {
      actions.loadUnsandboxedAutonomy({
        harness: draft.harness, sandbox: draft.sandbox,
        sandboxImplementation: draft.sandboxImpl,
        approval: draft.approval, dir: draft.cwd,
      }).then((value) => {
        if (request !== autonomyRequest.current || !state.isCurrent(generation)) return;
        setSandboxInfo(value.info || []);
        setAutonomyWarnings(value.warnings || []);
      });
    }, 350);
    return () => clearTimeout(timer);
  }, [draft.harness, draft.sandbox, draft.sandboxImpl, draft.approval, draft.cwd]);

  const update = (key, value) => {
    touched.current.add(key);
    operatorSet.current.add(key);
    if (PROFILE_OWNED_FIELDS.includes(key) && key !== 'profile') {
      profileOverrides.current.add(key);
    }
    setDraft((before) => ({ ...before, [key]: value }));
  };
  const updateName = (value) => {
    touched.current.add('name');
    profileOverrides.current.add('name');
    setDraft((before) => syncSpawnWorktree({ ...before, name: value }, worktrees.isRepo));
  };
  const changeGroup = (value) => {
    touched.current.add('group');
    touched.current.add('cwd');
    touched.current.add('wtRepo');
    touched.current.add('remoteControl');
    // The new group's default directory replaces only a blank or
    // group-filled one; a directory the operator typed stays theirs.
    if (!draft.cwd.trim() || draft.cwdOrigin === 'group') operatorSet.current.delete('cwd');
    setDraft((before) => selectSpawnGroup(before, value, context));
  };
  const changeHarness = (value) => {
    touched.current.add('harness');
    profileOverrides.current.add('harness');
    // The new harness pre-fills its own model and effort.
    operatorSet.current.add('harness');
    operatorSet.current.delete('model');
    operatorSet.current.delete('effort');
    const harnessFields = [
      'model', 'customModel', 'effort', 'sandbox', 'approval', 'approvalReviewer',
      'tools', 'askTimeout', 'trustDir', 'trustDirSpecified', 'remoteControl',
      'autoMemory', 'peerMessaging', 'sshWorkaround', 'autoCompactWindow', 'contextWindowMax',
      'copilotAPI', 'codexAppServer', 'fastMode', 'sandboxImpl', 'sandboxImplCleared', 'contextFeatures',
    ];
    for (const key of harnessFields) {
      touched.current.add(key);
      profileOverrides.current.add(key);
    }
    setDraft((before) => selectSpawnHarness(before, value, context, rememberedEffort));
  };
  const changeModel = (value) => {
    operatorSet.current.add('model');
    if (value === MODEL_CUSTOM_VALUE) {
      touched.current.add('model');
      touched.current.add('customModel');
      profileOverrides.current.add('model');
      profileOverrides.current.add('customModel');
      setDraft((before) => ({
        ...before,
        model: before.model && !view.models.includes(before.model) ? before.model : '',
        customModel: true,
      }));
      queueMicrotask(() => document.querySelector('#agent-spawn-model-custom')?.focus());
      return;
    }
    touched.current.add('model');
    touched.current.add('customModel');
    touched.current.add('effort');
    profileOverrides.current.add('model');
    profileOverrides.current.add('customModel');
    profileOverrides.current.add('effort');
    // A listed model pre-fills its remembered effort.
    operatorSet.current.delete('effort');
    setDraft((before) => ({
      ...before, model: value, customModel: false, effort: rememberedEffort(value),
    }));
  };
  const changeProfile = (handle) => (remote ? changeRemoteProfile(handle) : changeLocalProfile(handle));
  // A remote spawn opened with a chosen profile (a palette or dock open on a
  // peer) picks it once the peer's profiles load, as the operator would: the
  // request then names it. One the grant does not let this operator pick
  // stays unpicked; the peer's pre-fill stands.
  const chosenRemote = useRef(remote ? current.options.chosenProfile : '');
  useEffect(() => {
    const handle = chosenRemote.current;
    if (!handle || !profiles.length) return;
    chosenRemote.current = '';
    const profile = findSpawnProfile(profiles, handle);
    if (!profile?.remoteSelectable || isLocalRef(profile.name)) return;
    changeRemoteProfile(profile.name);
    const harness = profile.harness || draftRef.current.harness;
    if (current.options.fastMode && remote.options.overrides?.features
      && spawnCapabilityView({ ...draftRef.current, harness }, context).showFastMode) update('fastMode', '1');
  }, [profiles]);
  // The settings a remote request names: those the operator set, less a role
  // set or sandbox profile they changed and then put back as the peer has it.
  const remoteExplicit = (value) => [...operatorSet.current].filter((key) => !['roleRefs', 'sandboxProfile'].includes(key)
    || JSON.stringify(value[key] ?? null) !== JSON.stringify(baseline[key] ?? null));
  // A remote spawn's profile pick. Clear returns to what the peer applies.
  // A peer profile replaces the pre-fill as it does locally (it rides by
  // name, so the peer applies all of it). A local profile fills only what
  // the request carries: the overrides the grant opens, name, description,
  // brief and display role, and its prompts as local ones written into the
  // brief.
  const changeRemoteProfile = (handle) => {
    if (!handle) {
      operatorSet.current.clear();
      profileOverrides.current.clear();
      setPeerPrompts(basePeerPrompts.current);
      const previous = findSpawnProfile(profiles, draftRef.current.profile);
      setDraft((before) => ({ ...baseline, group: before.group, ...spawnUserText(before, previous, baseline) }));
      return;
    }
    const profile = findSpawnProfile(profiles, handle);
    if (!profile) return;
    const previous = findSpawnProfile(profiles, draftRef.current.profile);
    touched.current.add('profile');
    if (!isLocalRef(handle)) {
      operatorSet.current.add('profile');
      const next = replaceSpawnProfile(draftRef.current, profile, context, {
        autoFocus: actions.autoFocusDefault(), rememberedEffort, pickerUsable: false,
        preservedFields: new Set(profileOverrides.current), previousProfile: previous, prefill: baseline,
      });
      setPeerPrompts([...(next.promptRefs || [])]);
      setDraft({ ...remoteSpawnPins(next, remote.options, operatorSet.current), profile: handle });
      return;
    }
    // The request names no profile now, so the peer launches its default:
    // start from that pre-fill, not from a peer profile picked before.
    for (const field of ['profile', 'harness', 'model', 'effort']) operatorSet.current.delete(field);
    setPeerPrompts(basePeerPrompts.current);
    const filled = replaceSpawnProfile(draftRef.current, { ...profile, name: profile.localName }, context, {
      autoFocus: actions.autoFocusDefault(), rememberedEffort, pickerUsable: false,
      previousProfile: previous, prefill: baseline,
    });
    setDraft((current) => {
      const before = {
        ...baseline, group: current.group,
        ...Object.fromEntries(['descr', 'role', 'cwd'].map((key) => [key, current[key]])),
        ...spawnUserText(current, previous, baseline),
        promptRefs: [...new Set([...basePeerPrompts.current, ...(current.promptRefs || []).filter((ref) => isLocalRef(ref) || !peerPrompts.includes(ref))])],
      };
      const next = { ...before, profile: handle };
      for (const field of ['name', 'descr', 'initialMessage', 'role']) {
        if (String(filled[field] || '').trim() && !profileOverrides.current.has(field)) next[field] = filled[field];
      }
      Object.assign(next, spawnUserText(current, previous, baseline));
      const overrides = { harness: ['harness'], model: ['model', 'customModel'], effort: ['effort'] };
      for (const [field, keys] of Object.entries(overrides)) {
        if (!remote.options.overrides[field] || !String(filled[field] || '').trim()) continue;
        for (const key of keys) next[key] = filled[key];
        operatorSet.current.add(field);
      }
      const local = (profile.prompt_refs || []).map((ref) => `local:${ref}`);
      next.promptRefs = [...new Set([...(before.promptRefs || []), ...local])];
      return next;
    });
  };
  const changeLocalProfile = (handle) => {
    if (!handle) {
      // The worktree choices the operator made survive Clear, as they survive a
      // profile switch; collect them before Clear marks every field touched.
      const keptWorktree = WORKTREE_SELECTION_FIELDS.filter((field) => touched.current.has(field));
      if (profileOverrides.current.has('syncWorktree')) keptWorktree.push('syncWorktree');
      for (const field of PROFILE_OWNED_FIELDS) {
        touched.current.add(field);
        // Clearing resets these to defaults: no longer the operator's values.
        operatorSet.current.delete(field);
      }
      profileOverrides.current.clear();
      // A kept sync choice stays the operator's for the next profile switch.
      if (keptWorktree.includes('syncWorktree')) profileOverrides.current.add('syncWorktree');
      const previous = findSpawnProfile(profiles, draftRef.current.profile);
      // Clear resets with the picker treated as unusable, so a kept name must
      // re-sync its new worktree against the real picker state.
      setDraft((before) => {
        const cleared = clearSpawnProfileFields(before, context, {
          autoFocus: actions.autoFocusDefault(), rememberedEffort, previousProfile: previous,
        });
        for (const field of keptWorktree) cleared[field] = before[field];
        return syncSpawnWorktree(cleared, worktrees.isRepo);
      });
      return;
    }
    touched.current.add('profile');
    const profile = findSpawnProfile(profiles, handle);
    const previous = findSpawnProfile(profiles, draftRef.current.profile);
    if (profile) {
      const preservedFields = new Set(profileOverrides.current);
      for (const field of WORKTREE_SELECTION_FIELDS) {
        if (touched.current.has(field)) preservedFields.add(field);
      }
      setDraft((before) => ({
        ...replaceSpawnProfile(before, profile, context, {
          autoFocus: actions.autoFocusDefault(),
          rememberedEffort,
          pickerUsable: worktrees.isRepo,
          preservedFields,
          previousProfile: previous,
        }),
        profile: handle,
      }));
    }
  };

  const invalidateAttachmentUploads = () => {
    uploaded.current = { key: '', paths: [] };
  };
  const addAttachments = (files) => {
    if (busyRef.current) return;
    const additions = [];
    for (const file of files || []) {
      if (!file) continue;
      let name = file.name;
      if (!name) {
        const extension = (file.type && file.type.split('/')[1]) || 'png';
        name = `pasted-${++attachSequence.current}.${extension}`;
      }
      additions.push({
        id: ++attachSequence.current,
        file,
        name,
        size: file.size,
        url: (file.type || '').startsWith('image/') ? URL.createObjectURL(file) : '',
      });
    }
    if (!additions.length) return;
    invalidateAttachmentUploads();
    setAttachments((before) => [...before, ...additions]);
  };
  const removeAttachment = (id) => {
    if (busyRef.current) return;
    invalidateAttachmentUploads();
    setAttachments((before) => {
      const removed = before.find((attachment) => attachment.id === id);
      if (removed?.url) URL.revokeObjectURL(removed.url);
      return before.filter((attachment) => attachment.id !== id);
    });
  };
  const paste = (event) => {
    if (busyRef.current) return;
    const transfer = event.clipboardData;
    if (!transfer) return;
    const seen = new Set();
    const collected = [];
    const collect = (file) => {
      if (!file) return;
      const key = attachKey(file);
      if (seen.has(key)) return;
      seen.add(key);
      collected.push(file);
    };
    for (let index = 0; index < (transfer.files?.length || 0); index += 1) {
      collect(transfer.files[index]);
    }
    for (let index = 0; index < (transfer.items?.length || 0); index += 1) {
      if (transfer.items[index].kind === 'file') collect(transfer.items[index].getAsFile());
    }
    if (!collected.length) return;
    event.preventDefault();
    const now = globalThis.performance?.now?.() || 0;
    const repeating = now - pasteState.current.at < PASTE_REPEAT_MS;
    const fresh = repeating
      ? collected.filter((file) => !pasteState.current.keys.has(attachKey(file)))
      : collected;
    pasteState.current = { at: now, keys: new Set(collected.map(attachKey)) };
    addAttachments(fresh);
  };
  const hasDraggedFiles = (event) => {
    const types = event.dataTransfer?.types;
    return !!types && Array.prototype.indexOf.call(types, 'Files') !== -1;
  };
  const dragEnter = (event) => {
    if (!hasDraggedFiles(event)) return;
    event.preventDefault();
    if (busyRef.current) return;
    dragDepth.current += 1;
    setDragOver(true);
  };
  const dragOverEvent = (event) => {
    if (!hasDraggedFiles(event)) return;
    event.preventDefault();
    if (busyRef.current) return;
    event.dataTransfer.dropEffect = 'copy';
  };
  const dragLeave = (event) => {
    if (!hasDraggedFiles(event)) return;
    if (busyRef.current) return;
    dragDepth.current = Math.max(0, dragDepth.current - 1);
    if (!dragDepth.current) setDragOver(false);
  };
  const drop = (event) => {
    if (!hasDraggedFiles(event)) return;
    event.preventDefault();
    if (busyRef.current) return;
    dragDepth.current = 0;
    setDragOver(false);
    addAttachments(event.dataTransfer.files);
  };

  const browse = async (kind) => {
    if (browseBusy) return;
    const request = ++directoryRequest.current;
    const generation = current.generation;
    setBrowseBusy(kind);
    try {
      const result = await actions.pickDirectory({
        startDir: (kind === 'cwd' ? draft.cwd : draft.wtRepo).trim(),
        title: kind === 'cwd' ? 'Select the working directory' : 'Select the git repo to worktree',
      });
      if (request !== directoryRequest.current || !state.isCurrent(generation)) return;
      if (result.error) setError(result.error);
      else if (result.path) {
        if (kind === 'cwd') setDraft((before) => setSpawnCwd(before, result.path));
        else setDraft((before) => setSpawnWorktreeRepo(before, result.path));
        touched.current.add(kind === 'cwd' ? 'cwd' : 'wtRepo');
        if (kind === 'cwd') operatorSet.current.add('cwd');
        claimField('spawn', kind === 'cwd' ? 'agent-spawn-cwd' : 'agent-spawn-wt-repo');
        queueMicrotask(() => document.querySelector(
          kind === 'cwd' ? '#agent-spawn-cwd' : '#agent-spawn-wt-repo',
        )?.focus());
      }
    } finally {
      if (request === directoryRequest.current && state.isCurrent(generation)) setBrowseBusy('');
    }
  };

  const openPermissions = () => {
    const generation = current.generation;
    actions.openPermissions({
      overrides: draft.permissionOverrides,
      ownsGroup: draft.owner,
      group: draft.group,
      roleGrants: selectedRoles.filter((role) => !role.missing).flatMap((role) =>
        (role.permissions || []).map((grant) => ({
          slug: roleGrantSlug(grant), scope: roleGrantScope(grant), role: role.name,
        }))),
      label: draft.name.trim(),
      onSave: (kept) => {
        if (!state.isCurrent(generation)) return;
        touched.current.add('permissionOverrides');
        operatorSet.current.add('permissionOverrides');
        profileOverrides.current.add('permissionOverrides');
        setDraft((before) => ({ ...before, permissionOverrides: { ...kept } }));
      },
    });
  };
  const openContextFeatures = () => {
    const generation = current.generation;
    actions.openContextFeatures({
      catalog: view.contextFeatureCatalog,
      selection: draft.contextFeatures,
      label: draft.name.trim(),
      onSave: (kept) => {
        if (!state.isCurrent(generation)) return;
        touched.current.add('contextFeatures');
        operatorSet.current.add('contextFeatures');
        profileOverrides.current.add('contextFeatures');
        setDraft((before) => ({ ...before, contextFeatures: { ...kept } }));
      },
    });
  };
  const saveProfile = () => {
    const generation = current.generation;
    actions.openProfileEditor(spawnProfileSeed(draft, context), (name, node = '') => {
      // Only a profile on the node this dialog lists profiles from is
      // picked; a remote request's catalog is the peer's, read at open.
      if (remote || node !== spawnNode) return;
      actions.loadProfiles(true).then((nextProfiles) => {
        if (!state.isCurrent(generation)) return;
        setProfiles(nextProfiles);
        setDraft((before) => ({ ...before, profile: name }));
      }).catch(() => {});
    }, saveNodes);
  };

  const submit = async () => {
    if (submitLock.current) return;
    submitLock.current = true;
    const clickedAt = performance.now();
    const submitTiming = {};
    const markTiming = (stage) => { submitTiming[stage] = performance.now() - clickedAt; };
    let next = draft;
    const validation = validateSpawnDraft(next, context);
    if (validation) {
      setError(validation);
      if (validation.includes('name') || validation.includes('description')) nameRef.current?.focus();
      submitLock.current = false;
      return;
    }
    const derived = !next.name.trim() && !next.descr.trim()
      ? deriveSpawnNameFromMessage(next.initialMessage) : '';
    if (derived) {
      const proceed = await actions.confirmAutoName(derived);
      if (!state.isCurrent(current.generation)) return;
      if (!proceed) {
        nameRef.current?.focus();
        submitLock.current = false;
        return;
      }
    }
    if (worktrees.phase !== 'ready' && (
      next.worktree || (next.syncWorktree && !!(next.name.trim() || derived))
    )) {
      setError('wait for the worktree repository to finish loading');
      submitLock.current = false;
      return;
    }
    // The preview must belong to the selection being submitted — a stale one
    // could describe profile A while the request selects profile B — so a
    // policy still loading, or loaded for an earlier selection, blocks submit
    // until the matching load lands.
    //
    // The same check repeats after each await below. TCL-791 dropped the
    // break-glass fingerprint the old frozen token also carried, but not the
    // drift check itself: submit resolves a worktree and uploads attachments
    // before spawning, and an operator editing the selected profile in
    // another tab meanwhile bumps sandboxRevision. The daemon resolves the
    // profile by name at spawn time, so without this the agent would launch
    // under an edited policy whose preview the operator never saw.
    const liveSelectionKey = () =>
      sandboxPolicyKey(next.group, next.sandboxProfile, state.dialog.value?.sandboxRevision);
    // A failed preview never lands on its own: say why, and load it again.
    if (!view.sandboxProfilesDisabled && sandboxPolicyRef.current.error) {
      setError(`could not load the sandbox policy preview (${sandboxPolicyRef.current.error}) — reloading it; submit again in a moment`);
      state.refreshSandboxPolicy();
      submitLock.current = false;
      return;
    }
    if (!view.sandboxProfilesDisabled && sandboxPolicyRef.current.key !== liveSelectionKey()) {
      setError('wait for the sandbox policy preview to finish loading');
      submitLock.current = false;
      return;
    }
    const policyKey = view.sandboxProfilesDisabled ? null : sandboxPolicyRef.current.key;
    const policyDrifted = () => policyKey !== null
      && (sandboxPolicyRef.current.key !== policyKey || liveSelectionKey() !== policyKey);
    next = prepareSpawnDraft(next, context, derived, worktrees.isRepo);
    setDraft(next);
    setError('');
    setProgress('Preparing spawn…');
    busyRef.current = true;
    setBusy(true);
    actions.rememberLaunchPreferences(next);
    markTiming('prepared_ms');
    try {
      const worktreeKey = JSON.stringify([
        next.wtRepo, next.worktree, next.worktreeBranch, next.worktreeBase, next.fetchLatestWorktree,
      ]);
      let worktreeSelection = resolvedWorktree.current.key === worktreeKey
        ? resolvedWorktree.current.value : null;
      if (!worktreeSelection) {
        worktreeSelection = await actions.resolveWorktree(next, worktrees, setProgress, (timing) => Object.assign(submitTiming, timing));
        resolvedWorktree.current = { key: worktreeKey, value: worktreeSelection };
      }
      markTiming('worktree_ready_ms');
      if (!state.isCurrent(current.generation)) return;
      if (policyDrifted()) throw new Error(STALE_POLICY_ERROR);
      const uploadKey = attachments.map((attachment) => (attachment.file
        ? `${attachment.id}:${attachKey(attachment.file)}` : `queued:${attachment.name}`)).join('|');
      let attachmentPaths = uploaded.current.key === uploadKey ? uploaded.current.paths : null;
      if (!attachmentPaths) {
        const stored = attachments.filter((attachment) => !attachment.file);
        const stagedPaths = stored.length
          ? await actions.stageQueuedAttachments(queued.id, stored.map((attachment) => attachment.name))
          : [];
        if (!state.isCurrent(current.generation)) return;
        const uploadedPaths = await actions.uploadAttachments(attachments.filter((attachment) => attachment.file));
        attachmentPaths = [...stagedPaths, ...uploadedPaths];
        uploaded.current = { key: uploadKey, paths: attachmentPaths };
      }
      if (!state.isCurrent(current.generation)) return;
      markTiming('attachments_ready_ms');
      const request = buildSpawnRequest(next, context, worktreeSelection, attachmentPaths);
      if (policyDrifted()) throw new Error(STALE_POLICY_ERROR);
      setProgress('Launching agent…');
      markTiming('request_sent_ms');
      // A remote request carries only the overrides the operator set here.
      const payload = await actions.spawn(remote
        ? { ...request, explicit: remoteExplicit(next), promptRefs: [...(next.promptRefs || [])], prompts: remotePromptBodies(next.promptRefs, peerPrompts, remote), worktree: worktreeSelection }
        : request);
      markTiming('response_received_ms');
      if (queued) {
        // The agent exists now; a leftover queue row would offer to launch it
        // twice, so its removal failing is worth telling the operator.
        actions.deleteQueued(queued.id).catch((cause) => {
          actions.notifyError?.(`spawned, but could not remove it from the queue: ${errorMessage(cause)}`);
        });
      }
      if (!state.isCurrent(current.generation)) return;
      state.close();
      markTiming('elapsed_ms');
      void actions.reportTiming?.({ ...submitTiming, label: payload.label || '', closed: true });
      void actions.complete(payload, next);
    } catch (cause) {
      markTiming('elapsed_ms');
      void actions.reportTiming?.({ ...submitTiming, closed: false });
      if (state.isCurrent(current.generation)) {
        setError(errorMessage(cause));
        setProgress('');
        busyRef.current = false;
        setBusy(false);
        submitLock.current = false;
      }
    }
  };

  // Queue (fresh dialog) or Save (reopened queued spawn): store the draft
  // without launching. Only the name derivation a spawn would confirm runs
  // now; worktree creation, base fetch and everything else stays at launch.
  // In remote mode the queue shortcut does nothing rather than fall through
  // to Spawn: queueing never becomes a sent request.
  const noQueueHotkey = () => {};
  const saveQueued = async () => {
    // Queues are this node's; a remote spawn is sent as a request instead.
    if (remote || submitLock.current) return;
    submitLock.current = true;
    const release = () => { submitLock.current = false; };
    let next = draft;
    const validation = validateSpawnDraft(next, context);
    if (validation) {
      setError(validation);
      if (validation.includes('name') || validation.includes('description')) nameRef.current?.focus();
      release();
      return;
    }
    const derived = !next.name.trim() && !next.descr.trim()
      ? deriveSpawnNameFromMessage(next.initialMessage) : '';
    if (derived) {
      const proceed = await actions.confirmAutoName(derived);
      if (!state.isCurrent(current.generation)) return;
      if (!proceed) {
        nameRef.current?.focus();
        release();
        return;
      }
      next = { ...next, name: derived };
    }
    setError('');
    setProgress(queued ? 'Saving…' : 'Queueing…');
    busyRef.current = true;
    setBusy(true);
    setQueueing(true);
    try {
      // New files go up to the ordinary staging endpoint first; the queue
      // keeps its own copy of each, and the stored ones by name.
      const local = attachments.filter((attachment) => attachment.file);
      const stagedPaths = local.length ? await actions.uploadAttachments(local) : [];
      if (!state.isCurrent(current.generation)) return;
      let stagedIndex = 0;
      const queuedAttachments = attachments.map((attachment) => (attachment.file
        ? { name: attachment.name, size: attachment.size, staged: stagedPaths[stagedIndex++] }
        : { name: attachment.name, size: attachment.size }));
      await actions.saveQueued(queued?.id || 0, queuedAttachments.length ? { ...next, attachments: queuedAttachments } : next);
      if (!state.isCurrent(current.generation)) return;
      state.close();
      actions.queued(next, !!queued);
    } catch (cause) {
      if (state.isCurrent(current.generation)) {
        setError(errorMessage(cause));
        setProgress('');
        busyRef.current = false;
        setBusy(false);
        setQueueing(false);
        release();
      }
    }
  };

  const discardQueued = async () => {
    if (!queued || submitLock.current) return;
    const proceed = await actions.confirmDiscardQueued(draft.name || draft.descr || 'unnamed agent');
    if (!proceed || !state.isCurrent(current.generation)) return;
    try {
      await actions.deleteQueued(queued.id);
      if (!state.isCurrent(current.generation)) return;
      state.close();
      actions.dequeued(draft);
    } catch (cause) {
      if (state.isCurrent(current.generation)) setError(errorMessage(cause));
    }
  };

  // ▶ on a queued row opens this dialog with autoSpawn: launch once the same
  // loads submit waits for have landed, so a failure (a taken branch name, a
  // denied permission) stays on screen next to the fields that fix it.
  const autoSpawned = useRef(false);
  const policyReady = view.sandboxProfilesDisabled || !!sandboxPolicy.error
    || sandboxPolicy.key === sandboxPolicyKey(draft.group, draft.sandboxProfile, current.sandboxRevision);
  useEffect(() => {
    if (!current.options.autoSpawn || autoSpawned.current) return;
    if (worktrees.phase !== 'ready' || !policyReady) return;
    autoSpawned.current = true;
    void submit();
  }, [worktrees.phase, policyReady]);

  const selectedModel = modelSelectValue(draft, context);
  const usageChoices = shareOptions(usageShares, findSpawnHarness(context.harnesses, draft.harness), draft.model, Date.now(), draft.permissionOverrides);
  const usageChoice = usageChoices.find((o) => o.value === draft.usageFrom) || null;
  const subscriptionRow = !!subscription && (remote || target) && (draft.harness || 'claude') === 'claude';
  const spawnNodeName = remote ? remote.node || 'the peer' : target?.name || 'that node';
  const sandboxHelp = harnessBuiltinModeHelpForImplementation(
    view.sandbox.help[draft.sandbox],
    draft.sandboxImpl
      || launchDefaults?.implementation
      || (view.showSandboxImpl ? '' : view.sandboxImplDefault),
    draft.harness,
  );
  const approvalHelp = view.approval.help[draft.approval] || '';
  const reviewerHelp = approvalReviewerHelp(draft.approvalReviewer, draft.approval);
  const toolsHelp = view.tools.help[draft.tools] || '';
  const askTimeoutHelp = view.askTimeout.help[draft.askTimeout] || '';
  const autoCompactWindowHint = autoCompactWindowHintFor(draft, view);
  const contextWindowMaxHint = contextWindowMaxHintFor(draft, view);
  // The blank row names the answer the daemon gave, in the same words the
  // concrete option below it uses.
  const resolvedSandboxImplLabel = sandboxImplResolvedLabel(
    view.sandboxImplOptions, launchDefaults?.implementation,
  );
  const sandboxImplHint = sandboxImplHintFor(draft, view, launchDefaults?.implementation);
  // The row's own help and the selected implementation's caveat are one
  // disclosure, not two triggers: an operator opening [!] wants everything this
  // row has to say, and a second button beside the first would only make them
  // guess which one holds the warning.
  const sandboxImplCaveat = sandboxImplCaveatFor(draft, view, launchDefaults?.implementation);
  const sandboxImplHelp = sandboxImplCaveat
    ? `${sandboxImplCaveat} ${SANDBOX_IMPL_TITLE}` : SANDBOX_IMPL_TITLE;
  const showApprovalControls = approvalControlsVisibleFor(
    draft, launchDefaults?.implementation,
  );
  const sandboxImplCleared = sandboxImplClearedNoticeFor(draft);
  const nativeCodexRegistryWarning = nativeCodexRegistryWarningFor(
    draft, view.harness, launchDefaults?.implementation,
  );
  const worktreeUsable = worktrees.phase === 'ready' && worktrees.isRepo;
  let worktreeEmptyLabel = '(no worktree — use CWD above)';
  if (worktrees.phase === 'loading') worktreeEmptyLabel = 'loading…';
  else if (worktrees.empty) worktreeEmptyLabel = '(enter a CWD to enable worktrees)';
  else if (!worktrees.isRepo && worktrees.subRepos?.length) {
    worktreeEmptyLabel = '(not a git repo — pick a sub-repo in "Worktree repo" above)';
  } else if (!worktrees.isRepo) worktreeEmptyLabel = '(not a git repo — worktrees unavailable)';

  return html`<${Overlay}
    id="agent-spawn-modal"
    labelledby="agent-spawn-title"
    onClose=${state.close}
    onSubmitHotkey=${queued ? saveQueued : submit}
    onSubmitShiftHotkey=${remote ? noQueueHotkey : saveQueued}
    dirty=${dirty}
    blocked=${busy}
    confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}
    resizeKey="tclaude.dash.modalSize.agent-spawn"
    persistHeight=${false}
    guardBackdropDrag=${true}
    initialFocusRef=${nameRef}
    dialogClass=${`cron-create-modal${dragOver ? ' spawn-drag-over' : ''}`}
    onDragEnter=${dragEnter}
    onDragOver=${dragOverEvent}
    onDragLeave=${dragLeave}
    onDrop=${drop}
    onPaste=${paste}
  >
    <h3 id="agent-spawn-title">${queued
      ? html`<${Words} prefix="spawn-title" plain="Queued agent" wizard="Dormant summoning" />`
      : requestOnly
        ? html`<${Words} prefix="spawn-title" plain="Request an agent" wizard="Petition for a hero" />`
        : html`<${Words} prefix="spawn-title" plain="Spawn a new agent" wizard="Summon a new hero" />`}${remote ? ` on ${remote.node}` : target?.name ? ` on ${target.name}` : ''}</h3>
    ${queued && html`<div class="agent-spawn-queued-banner" id="agent-spawn-queued-banner">
      ⏸ ${wizard ? 'Prepared' : 'Queued'}${queued.createdAt ? ` ${relTime(queued.createdAt)}` : ''} · not running yet
    </div>`}
    <div class="modal-meta" id="agent-spawn-meta" hidden=${!draft.fixedGroup}>
      ${draft.fixedGroup ? `joining group: ${draft.group}` : ''}
    </div>
    ${remote && html`<div class="modal-meta agent-spawn-remote-intro" id="agent-spawn-remote-intro">${remoteSpawnIntro(remote, draft.group)}</div>`}
    ${remote && html`<${RemoteSourceFilter} remote=${remote} showFrom=${showFrom} setShowFrom=${setShowFrom} />`}
    <label class="cron-create-row" id="agent-spawn-load-profile-row"
      title=${`Pre-fill this dialog from a saved spawn profile — a reusable bundle of the harness / model / effort / sandbox + name / role / descr / initial-message fields (NOT the directory or worktree). Fields left blank here fall through to the resolved defaults. ${RESOLVED_DEFAULTS_CHAIN}`}>
      <span class="cron-create-label"><${Words} prefix="profiles-word" plain="Profile" wizard="Loadout" /></span>
      <div class="cron-create-target"><div class="cron-target-input-row">
        <select id="agent-spawn-load-profile" value=${draft.profile} disabled=${busy}
          onChange=${(event) => changeProfile(event.currentTarget.value)}>
          <option value="">${remote ? `— ${remote.node}'s defaults —` : '— none (blank form) —'}</option>
          ${remote
            ? html`<${RemoteProfileOptions} profiles=${profiles} remote=${remote} showFrom=${showFrom} selected=${draft.profile} />`
            : spawnProfileChoices(profiles).map((choice) => html`<option key=${choice.value} value=${choice.value}>${choice.label}</option>`)}
        </select>
        <button id="agent-spawn-clear" type="button" disabled=${busy}
          title="Reset the profile-filled fields (harness / model / effort / name / role / …) to blank — leaves the group, directory and worktree untouched"
          onClick=${() => changeProfile('')}>Clear</button>
        <button id="agent-spawn-save-profile" type="button" disabled=${busy || !canSaveProfile}
          aria-disabled=${canSaveProfile ? undefined : 'true'} data-remote-spawn-free=${canSaveProfile ? '' : undefined}
          title=${canSaveProfile ? "Save the current dialog fields as a reusable spawn profile — opens the profile editor pre-filled so you can name it and pick the node it's saved on (the directory and worktree aren't stored)" : undefined}
          onClick=${saveProfile}><${Words} prefix="profiles-word" plain="Save as profile…" wizard="Save as loadout…" /></button><${LockReason} id="agent-spawn-save-profile-lock" align="end" why=${canSaveProfile ? '' : `You can't save profiles on ${remote?.node || target?.name || 'that node'} or here: that needs unrestricted trust or the node.spawn.profiles.create grant.`}/>
      </div></div>
    </label>
    <label class="cron-create-row" id="agent-spawn-group-row" hidden=${draft.fixedGroup}>
      <span class="cron-create-label"><${Words} prefix="spawn-group-word" plain="Group" wizard="Party" /></span>
      <select id="agent-spawn-group" value=${draft.group} disabled=${busy}
        onChange=${(event) => changeGroup(event.currentTarget.value)}>
        ${context.groups.filter((group) => group?.name).map((group) => html`<option key=${group.name} value=${group.name}>${group.name}</option>`)}
      </select>
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Name</span>
      <input ref=${nameRef} id="agent-spawn-name" type="text" value=${draft.name} disabled=${busy}
        onInput=${(event) => updateName(event.currentTarget.value)}
        onBlur=${() => {
          const next = prepareSpawnDraft(draft, context, '', worktrees.isRepo);
          if (next.name !== draft.name) setDraft(next);
        }}
        placeholder="optional — sets /rename on the new pane" autocomplete="off" spellcheck="false" />
      <div id="agent-spawn-name-hint" class=${`spawn-field-hint${nameHint.warn ? ' warn' : ''}`}>${nameHint.text}</div>
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Initial msg</span>
      <textarea id="agent-spawn-init-msg" rows="4" value=${draft.initialMessage} disabled=${busy}
        onInput=${(event) => update('initialMessage', event.currentTarget.value)}
        placeholder="optional — the new agent's task brief; delivered to its inbox, newlines preserved (≤16384 bytes)" spellcheck="false"></textarea>
    </label>
    <div class="cron-create-row agent-spawn-prompts-row"
      title="Saved prompts open the task brief, in order, ahead of the initial message. Pre-filled from the profile, else the group's default prompts.">
      <span class="cron-create-label">Saved prompts</span>
      ${remote
        ? html`<${SavedPromptChips} id="agent-spawn-prompt-refs" value=${draft.promptRefs || []}
          onChange=${(refs) => update('promptRefs', refs)} disabled=${busy} confirmDiscard=${confirmDiscard}
          library=${remotePrompts} visible=${showFrom} locked=${remotePromptsOpen ? [] : peerPrompts}
          lockedReason=${`${remote.node} applies this prompt itself (its profile or group default) — dropping it needs groups.members.spawn.prompts`}
          sources=${[{ source: 'target', label: `${remote.node}'s prompts` }, { source: 'local', label: "This node's prompts", chip: 'this node' }]}
          hint=${remotePromptsOpen
            ? `${remote.node}'s prompts are applied by ${remote.node}; this node's are written into the brief.`
            : `Prompts ${remote.node} does not apply itself are written into the brief.`}/>`
        : html`<${SavedPromptChips} id="agent-spawn-prompt-refs" value=${draft.promptRefs || []} node=${target?.node || ''}
          onChange=${(refs) => update('promptRefs', refs)} disabled=${busy} confirmDiscard=${confirmDiscard}/>`}
    </div>
    <div class="cron-create-row" id="agent-spawn-attachments-row"
      title="Attach files or screenshots to hand to the new agent. They're uploaded to a temp dir and listed in the agent's startup briefing.">
      <span class="cron-create-label">Attachments</span>
      <div class="cron-create-target spawn-attachments">
        <div class="spawn-attachments-controls">
          <button type="button" id="agent-spawn-attach-btn" disabled=${busy} onClick=${() => fileRef.current?.click()}>📎 Attach files…</button>
          <input ref=${fileRef} type="file" id="agent-spawn-attach-input" multiple hidden disabled=${busy}
            onChange=${(event) => { addAttachments(event.currentTarget.files); event.currentTarget.value = ''; }} />
          <span class="spawn-attachments-hint">…or drag files here / paste (⌘/Ctrl-V)</span>
        </div>
        <${AttachmentList} attachments=${attachments} remove=${removeAttachment} busy=${busy} />
      </div>
    </div>
		<div class="cron-create-row" id="agent-spawn-environment-row" data-remote-spawn-field="environment"><span class="cron-create-label"><${Words} plain="Environment" wizard="Summoning runes" /></span><div class="cron-create-target"><div class="sbx-rows">${(draft.environment || []).map((row, index) => html`<div key=${index} class="sbx-row sbx-environment-row"><input class="sbx-env-name" value=${row.name || ''} disabled=${busy} placeholder="NAME" onInput=${(event) => update('environment', draft.environment.map((entry, rowIndex) => rowIndex === index ? { ...entry, name: event.currentTarget.value } : entry))}/><input data-capture-mask class="sbx-env-value" value=${row.value || ''} disabled=${busy} placeholder="value" onInput=${(event) => update('environment', draft.environment.map((entry, rowIndex) => rowIndex === index ? { ...entry, value: event.currentTarget.value } : entry))}/><button type="button" disabled=${busy} onClick=${() => update('environment', draft.environment.filter((_, rowIndex) => rowIndex !== index))}>×</button></div>`)}</div><div class="sbx-add-row-help"><button type="button" class="sbx-add-row" disabled=${busy} onClick=${() => update('environment', [...(draft.environment || []), { name: '', value: '' }])}><${Words} plain="＋ add override" wizard="✦ bind override" /></button>${effectiveEnvironment.length ? html`<button type="button" class="sbx-effective-environment-toggle" id="agent-spawn-effective-environment-toggle" disabled=${busy} aria-expanded=${effectiveEnvironmentOpen ? 'true' : 'false'} aria-controls="agent-spawn-environment-preview" aria-label=${`${effectiveEnvironmentOpen ? 'Hide' : 'Show'} effective environment (${effectiveEnvironment.length} ${effectiveEnvironment.length === 1 ? 'variable' : 'variables'})`} onClick=${() => { setHelpOpen(''); setEffectiveEnvironmentOpen((value) => !value); }}>${effectiveEnvironmentOpen ? '▾' : '▸'} <${Words} plain="Effective env" wizard="Runes in force" /> (${effectiveEnvironment.length})</button>` : null}<${HelpDisclosure} id="agent-spawn-environment-help" label=${wizard ? 'Summoning rune overrides' : 'Environment overrides'} help=${wizard ? ENVIRONMENT_OVERRIDE_HELP_WIZARD : ENVIRONMENT_OVERRIDE_HELP} open=${helpOpen === 'agent-spawn-environment-help'} setOpen=${setHelpOpen} /></div>${effectiveEnvironment.length ? html`<div id="agent-spawn-environment-preview" class="sbx-effective-environment-help" hidden=${!effectiveEnvironmentOpen}><div class="sbx-caps">${effectiveEnvironment.map((entry) => html`<div class="sbx-cap"><span class="sbx-cap-tag sbx-cap-env">${entry.name}</span><span data-capture-mask class="sbx-cap-val" title=${entry.value}>${entry.value}</span><span class="muted">${entry.source}</span></div>`)}</div><${HelpDisclosure} id="agent-spawn-effective-environment-help" label=${wizard ? 'Runes in force' : 'Effective environment'} help=${wizard ? EFFECTIVE_ENVIRONMENT_HELP_WIZARD : EFFECTIVE_ENVIRONMENT_HELP} open=${helpOpen === 'agent-spawn-effective-environment-help'} setOpen=${setHelpOpen} /></div>` : null}</div></div>
    <div class="cron-create-row agent-spawn-roles-row">
      <span class="cron-create-label"><${Words} plain="Roles" wizard="Classes" /></span>
      <div class="agent-spawn-role-picker">
        <div class="agent-spawn-role-chips" id="agent-spawn-role-refs">
          ${selectedRoles.map((role) => html`<span key=${role.name} class=${`agent-spawn-role-chip${role.missing ? ' missing' : ''}`}>
            <button type="button" class="role-chip-inspect" disabled=${busy} data-remote-spawn-free="" title=${role.missing ? 'This role is missing from the library' : roleHoverText(role)}
              aria-expanded=${inspectedRole === role.name ? 'true' : 'false'} onClick=${() => setInspectedRole(inspectedRole === role.name ? '' : role.name)}>${role.name}</button>
            <button type="button" class="role-chip-remove" disabled=${busy} data-remote-spawn-field="roles" title=${`Remove ${role.name}`}
              aria-label=${`Remove role ${role.name}`} onClick=${() => {
                setInspectedRole((currentRole) => currentRole === role.name ? '' : currentRole);
                update('roleRefs', (draft.roleRefs || []).filter((name) => name !== role.name));
              }}>×</button>
          </span>`)}
          <select id="agent-spawn-role-add" value="" disabled=${busy || (context.roles || []).every((role) => (draft.roleRefs || []).includes(role.name))}
            aria-label=${wizard ? 'Add class' : 'Add role'} onChange=${(event) => {
              const next = event.currentTarget.value;
              if (!next) return;
              const before = draft.roleRefs || [];
              update('roleRefs', [...before, next]);
              event.currentTarget.value = '';
            }}>
            <option value="">${wizard ? '＋ Add class…' : '＋ Add role…'}</option>
            ${(context.roles || []).filter((role) => !(draft.roleRefs || []).includes(role.name))
              .map((role) => html`<option key=${role.name} value=${role.name}>${role.name}${role.descr ? ` — ${role.descr}` : ''}</option>`)}
          </select>
        </div>
        ${inspected && html`<div class=${`agent-spawn-role-inspect${inspected.missing ? ' missing' : ''}`}>
          ${inspected.missing ? html`⚠ This role is no longer in the library.` : html`
            ${inspected.descr && html`<div class="role-inspect-descr">${inspected.descr}</div>`}
            <div><b>Brief</b> ${inspected.brief || html`<span class="muted">none</span>`}</div>
            <div><b>Grants</b> ${(inspected.permissions || []).length ? html`<span class="role-inspect-grants">${inspected.permissions.map((grant) => html`<code key=${roleGrantSlug(grant)}>${roleGrantSlug(grant)}${roleGrantScope(grant) ? ` · ${roleGrantScope(grant)}` : ''}</code>`)}</span>` : html`<span class="muted">none</span>`}</div>`}
        </div>`}
      </div>
    </div>
    <div class="cron-create-row spawn-role-row">
      <span class="cron-create-label">Display role</span>
      <input id="agent-spawn-role" type="text" value=${draft.role} disabled=${busy}
        onInput=${(event) => update('role', event.currentTarget.value)}
        placeholder="optional — dashboard and routing label; grants no access" autocomplete="off" spellcheck="false" />
      <label class="spawn-owner-toggle" title="Make the new agent a group owner of the destination group at birth.">
        <input id="agent-spawn-owner" type="checkbox" checked=${draft.owner} disabled=${busy}
          onChange=${(event) => update('owner', event.currentTarget.checked)} /> owner
      </label>
      <button type="button" id="agent-spawn-perms" disabled=${busy}
        title="Set the new agent's permanent per-slug permissions (grant / deny / inherit) to apply when it spawns."
        onClick=${openPermissions}>Permissions…</button>
      <span id="agent-spawn-perms-indicator" class="spawn-perms-indicator" hidden=${!permissionsLabel}>${permissionsLabel}</span>
      ${view.showContextFeatures && html`<button type="button" id="agent-spawn-context-features" disabled=${busy}
        title="Choose how much of Claude Code's startup context this agent loads — trim bundled skills, unused tool schemas and system-prompt blocks so more of its window is about your task."
        onClick=${openContextFeatures}>Context…</button>`}
      ${view.showContextFeatures && html`<span id="agent-spawn-context-features-indicator" class="spawn-perms-indicator"
        hidden=${!contextFeaturesLabel}>${contextFeaturesLabel}</span>`}
    </div>
    <label class="cron-create-row">
      <span class="cron-create-label">Descr</span>
      <input id="agent-spawn-descr" type="text" value=${draft.descr} disabled=${busy}
        onInput=${(event) => update('descr', event.currentTarget.value)}
        placeholder="optional — short one-line description shown on the dashboard" autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row" title="Optional task-reference link (http(s)) for the new agent.">
      <span class="cron-create-label">Task link</span>
      <input id="agent-spawn-task" type="url" inputmode="url" value=${draft.task} disabled=${busy}
        onInput=${(event) => update('task', event.currentTarget.value)}
        placeholder="optional — Linear/GitHub/ticket URL (http(s)); shown in the Task link column" autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row" id="agent-spawn-harness-row"
      title="Coding harness for the new agent. The harness switches the Model + Effort menus and launch controls.">
      <span class="cron-create-label">Harness</span>
      <select id="agent-spawn-harness" value=${draft.harness} disabled=${busy}
        onChange=${(event) => changeHarness(event.currentTarget.value)}>
        ${context.harnesses.map((harness) => html`<option key=${harness.name} value=${harness.name}>${harness.display_name || harness.name}</option>`)}
      </select>
    </label>
    <label class="cron-create-row" id="agent-spawn-fast-mode-row" hidden=${!view.showFastMode}
      title="Codex fast mode uses a higher-cost, lower-latency service tier. Harness default leaves ~/.codex/config.toml in charge.">
      <span class="cron-create-label">Fast mode</span>
      <select id="agent-spawn-fast-mode" value=${draft.fastMode} disabled=${busy}
        onChange=${(event) => update('fastMode', event.currentTarget.value)}>
        <option value="">Harness default (config.toml)</option>
        <option value="1">On</option>
        <option value="0">Off</option>
      </select>
    </label>
    <div class="spawn-inline-fields">
      <label class="cron-create-row" id="agent-spawn-model-claude-row" hidden=${!view.hasModelList}
        title="Model suggested by the selected harness. Default passes no --model; Custom model id accepts an out-of-catalog model.">
        <span class="cron-create-label">Model</span>
        <select id="agent-spawn-model" value=${selectedModel} disabled=${busy}
          onChange=${(event) => changeModel(event.currentTarget.value)}>
          <option value="">${spawnModelDefaultLabel(draft, context, profiles)}</option>
          ${view.models.map((model) => html`<option key=${model} value=${model}>${model}</option>`)}
          <option value=${MODEL_CUSTOM_VALUE}>Custom model id…</option>
        </select>
      </label>
      <label class="cron-create-row" id="agent-spawn-model-codex-row" hidden=${view.hasModelList}
        title="Free-text model id for a harness with no curated suggestions. Blank passes no --model.">
        <span class="cron-create-label">Model</span>
        <input id="agent-spawn-model-codex" type="text" value=${draft.model} disabled=${busy}
          onInput=${(event) => {
            const value = event.currentTarget.value;
            touched.current.add('model'); touched.current.add('effort');
            profileOverrides.current.add('model'); profileOverrides.current.add('effort');
            operatorSet.current.add('model'); operatorSet.current.delete('effort');
            setDraft((before) => ({
              ...before, model: value, customModel: false, effort: rememberedEffort(value.trim()),
            }));
          }}
          placeholder="blank = harness default; model id or alias" autocomplete="off" spellcheck="false" />
      </label>
      <label class="cron-create-row" title="Reasoning effort for the new agent.">
        <select id="agent-spawn-effort" aria-label="Effort" value=${draft.effort} disabled=${busy}
          onChange=${(event) => update('effort', event.currentTarget.value)}>
          <option value="">Default (harness's own)</option>
          ${view.efforts.map((effort) => html`<option key=${effort} value=${effort}>${effort}</option>`)}
        </select>
      </label>
      <label class="cron-create-row" id="agent-spawn-auto-compact-window-row" hidden=${!view.showAutoCompactWindow}
        title=${AUTO_COMPACT_WINDOW_TITLE}>
        <input id="agent-spawn-auto-compact-window" type="text" aria-label="Auto-compact window (tokens)"
          value=${draft.autoCompactWindow} disabled=${busy}
          onInput=${(event) => update('autoCompactWindow', event.currentTarget.value)}
          placeholder="ctx limit" autocomplete="off" spellcheck="false" inputmode="numeric" />
      </label>
      <label class="cron-create-row" id="agent-spawn-context-window-max-row" hidden=${!view.showContextWindowMax}
        title=${CONTEXT_WINDOW_MAX_TITLE}>
        <span class="cron-create-label">Context max</span>
        <input id="agent-spawn-context-window-max" type="text" aria-label="Configured Copilot context max (tokens)"
          value=${draft.contextWindowMax} disabled=${busy}
          onInput=${(event) => update('contextWindowMax', event.currentTarget.value)}
          placeholder="blank = assumed; e.g. 272000" autocomplete="off" spellcheck="false" inputmode="numeric" />
      </label>
    </div>
    ${view.showAutoCompactWindow && autoCompactWindowHint && html`
      <div class="cron-create-row" id="agent-spawn-auto-compact-window-hint">
        <span class="cron-create-label"></span>
        <div class="cron-create-target">
          <div class=${`spawn-field-hint${autoCompactWindowHint.warn ? ' warn' : ''}`}>${autoCompactWindowHint.text}</div>
        </div>
      </div>`}
    ${view.showContextWindowMax && contextWindowMaxHint && html`
      <div class="cron-create-row" id="agent-spawn-context-window-max-hint">
        <span class="cron-create-label"></span>
        <div class="cron-create-target">
          <div class=${`spawn-field-hint${contextWindowMaxHint.warn ? ' warn' : ''}`}>${contextWindowMaxHint.text}</div>
        </div>
      </div>`}
    <label class="cron-create-row" id="agent-spawn-model-custom-row" hidden=${selectedModel !== MODEL_CUSTOM_VALUE}
      title="Type any model id or alias accepted by the selected harness. Validated when the agent spawns.">
      <span class="cron-create-label"></span>
      <input id="agent-spawn-model-custom" type="text" aria-label="Custom model id" value=${draft.model} disabled=${busy}
        onInput=${(event) => {
          const value = event.currentTarget.value;
          touched.current.add('model'); touched.current.add('effort');
          profileOverrides.current.add('model'); profileOverrides.current.add('effort');
          operatorSet.current.add('model'); operatorSet.current.delete('effort');
            setDraft((before) => ({
              ...before, model: value, customModel: true, effort: rememberedEffort(value.trim()),
            }));
        }}
        placeholder="model id or alias" autocomplete="off" spellcheck="false" />
    </label>
    ${usageChoices.length > 0 && html`<div class="cron-create-row" id="agent-spawn-usage-row"
      title="Whose provider account pays for this agent's model calls. A peer's share routes them through that peer's model gateway; the agent still runs here.">
      <label class="cron-create-label" for="agent-spawn-usage">Usage from</label>
      <div class="cron-create-target">
        <select id="agent-spawn-usage" value=${usageChoice && !usageChoice.reason ? draft.usageFrom : ''} disabled=${busy}
          onChange=${(event) => {
            const value = event.currentTarget.value;
            const pick = usageChoices.find((o) => o.value === value);
            setDraft((before) => ({ ...before, usageFrom: pick ? value : '', usageShare: pick ? pick.share : null, usageOwn: !pick }));
          }}>
          <option value="">⌂ Your own account</option>
          ${usageChoices.map((o) => html`<option key=${o.value} value=${o.value} disabled=${!!o.reason}>${o.label} — ${o.reason || o.detail}</option>`)}
        </select>
        ${usageChoice && !usageChoice.reason && html`<div class="spawn-field-hint info" id="agent-spawn-usage-hint">${shareHint(usageChoice.share)}</div>`}
        ${usageChoice?.reason && html`<div class="spawn-field-hint warn" id="agent-spawn-usage-hint">${usageChoice.label} cannot pay for this spawn: ${usageChoice.reason}. It will use your own account.</div>`}
      </div>
    </div>`}
    ${subscriptionRow && html`<div class="cron-create-row" id="agent-spawn-claude-login-row" data-remote-spawn-free=""
      title="Which Claude login the agent runs on. Your subscription: the token you stored on this node travels in the encrypted spawn request, stays in memory on the node running the agent, and reaches Claude Code through a pipe — never its disk or environment.">
      <label class="cron-create-label" for="agent-spawn-claude-login">Claude login</label>
      <div class="cron-create-target">
        <select id="agent-spawn-claude-login" value=${draft.claudeSubscription && subscription?.set ? 'mine' : ''} disabled=${busy}
          onChange=${(event) => {
            const mine = event.currentTarget.value === 'mine';
            setDraft((before) => ({ ...before, claudeSubscription: mine }));
          }}>
          <option value="">${spawnNodeName}'s own login</option>
          <option value="mine" disabled=${!subscription?.set}>Your Claude subscription${subscription?.set ? '' : ' (not set up)'}</option>
        </select>
        ${draft.claudeSubscription && subscription?.set
          ? html`<div class="spawn-field-hint info" id="agent-spawn-claude-login-hint">Runs on your subscription and is billed to it. ${spawnNodeName} holds the token in memory only: if its tclaude restarts, the agent cannot be restarted, and you spawn a new one.</div>`
          : !subscription?.set && html`<div class="spawn-field-hint" id="agent-spawn-claude-login-hint">To run it on your own subscription, store a token from <code>claude setup-token</code> under Fleet config → Model gateways → My Claude subscription.</div>`}
      </div>
    </div>`}
    <div class="cron-create-row" id="agent-spawn-sandbox-impl-row" hidden=${!view.showSandboxImpl}
      title=${SANDBOX_IMPL_TITLE}>
      <label class="cron-create-label" for="agent-spawn-sandbox-impl">Sandbox</label>
      <div class="cron-create-target spawn-field-help-column">
        <div class="spawn-field-with-help">
          <select id="agent-spawn-sandbox-impl" value=${draft.sandboxImpl} disabled=${busy}
            aria-describedby="agent-spawn-sandbox-impl-help"
            onChange=${(event) => {
    const value = event.currentTarget.value;
    touched.current.add('sandboxImpl');
    operatorSet.current.add('sandboxImpl');
    profileOverrides.current.add('sandboxImpl');
    setDraft((before) => setSpawnSandboxImpl(before, value));
  }}>
            <option value="">${resolvedDefaultOption(resolvedSandboxImplLabel)}</option>
            ${(view.sandboxImplOptions || []).map((option) => html`
              <option key=${option.value} value=${option.value}>${option.label}</option>`)}
          </select>
          <${HelpDisclosure} id="agent-spawn-sandbox-impl"
            descriptionID="agent-spawn-sandbox-impl-help" label="Sandbox"
            help=${sandboxImplHelp} warn=${!!sandboxImplCaveat}
            open=${helpOpen === 'agent-spawn-sandbox-impl'} setOpen=${setHelpOpen} />
        </div>
      </div>
    </div>
    ${view.showSandboxImpl && sandboxImplHint && html`
      <div class="cron-create-row" id="agent-spawn-sandbox-impl-hint-row">
        <span class="cron-create-label"></span>
        <div class="cron-create-target">
          <${SandboxImplHint} hint=${sandboxImplHint} id="agent-spawn-sandbox-impl-hint" />
        </div>
      </div>`}
    <${HelpField} id="agent-spawn-sandbox"
      label=${harnessBuiltinModeControlLabel(view.harness)}
      title="Harness-native sandbox mode. Available when Sandbox explicitly or by resolved default uses the harness's built-in sandbox."
      value=${draft.sandbox} options=${SettingOptions({
    setting: harnessBuiltinModeOptionsForImplementation(view.sandbox, draft.harness, draft.sandbox),
    optionLabel: (mode, recommended) => harnessBuiltinModeOptionLabel(draft.harness, mode, recommended),
  })}
      onChange=${(event) => {
        const value = event.currentTarget.value;
        touched.current.add('sandbox');
        operatorSet.current.add('sandbox');
        profileOverrides.current.add('sandbox');
        setDraft((before) => ({ ...before, sandbox: value }));
      }} help=${sandboxHelp} open=${helpOpen === 'agent-spawn-sandbox'} setOpen=${setHelpOpen}
      disabled=${!view.showHarnessBuiltinMode} busy=${busy} />
    ${sandboxImplCleared && html`
      <div class="cron-create-row" id="agent-spawn-sandbox-impl-cleared-row" role="alert">
        <span class="cron-create-label"></span>
        <div class="cron-create-target">
          <div class="spawn-field-hint warn">${sandboxImplCleared.text}</div>
        </div>
      </div>`}
    <${HelpField} id="agent-spawn-sandbox-profile" descriptionID="agent-spawn-sandbox-profile-preview" label="Sandbox profile"
      title=${`Sandbox policy, not a launch default: ${SANDBOX_PROFILE_COMPOSITION} Choose the composed layers, omit every tclaude sandbox-profile value, or add an explicit profile on top of the global and group ones.`}
      value=${view.sandboxProfilesDisabled ? SANDBOX_PROFILE_NONE : draft.sandboxProfile}
      options=${view.sandboxProfilesDisabled ? [
        { value: SANDBOX_PROFILE_NONE, label: '— none (required by this sandbox mode) —' },
      ] : [
        { value: '', label: '— composed global + group sandbox profiles —' },
        { value: SANDBOX_PROFILE_NONE, label: '— none (omit all tclaude profile values) —' },
        ...(sandboxPolicy.profiles || []).map((profile) => ({ value: profile.name, label: profile.name })),
      ]}
      onChange=${(event) => update('sandboxProfile', event.currentTarget.value)}
      help=${view.sandboxProfilesDisabled
        ? 'This sandbox mode cannot carry tclaude sandbox-profile filesystem, environment, network, or agent-directory values.'
        : sandboxPolicy.preview}
      open=${helpOpen === 'agent-spawn-sandbox-profile'} setOpen=${setHelpOpen}
      busy=${busy || view.sandboxProfilesDisabled} />
    <div class="cron-create-row" id="agent-spawn-allow-unenforced-sandbox-row"
      hidden=${!view.showSandboxImpl}>
      <span class="cron-create-label"></span>
      <div class="cron-create-target">
        <label class="spawn-unenforced-toggle" title=${UNENFORCED_SANDBOX_TITLE}>
          <input id="agent-spawn-allow-unenforced-sandbox" type="checkbox"
            aria-describedby="agent-spawn-allow-unenforced-sandbox-description"
            checked=${draft.allowUnenforcedSandbox} disabled=${busy}
            onChange=${(event) => update('allowUnenforcedSandbox', event.currentTarget.checked)} />
          Allow launch with unenforced rules
        </label>
        <span id="agent-spawn-allow-unenforced-sandbox-description"
          class="spawn-field-description">${UNENFORCED_SANDBOX_TITLE}</span>
      </div>
    </div>
    ${!context.remoteSpawn ? html`<${HelpField} id="agent-spawn-usage-profile" label="Usage profile"
      title="The budget and alerts this agent runs under. Inherited: the spawn profile's usage profiles on top of the group's (else the global default). A profile picked here replaces them."
      value=${draft.usageProfile || ''}
      options=${[
        { value: '', label: '— inherited (spawn profile + group / global) —' },
        ...liveUsageProfiles(context.usageProfiles).map((name) => ({ value: name, label: name })),
        { value: NEW_USAGE_PROFILE, label: '＋ new usage profile…', noFill: true },
      ]}
      onChange=${(event) => {
        // The new profile is made in the usage manager; the pick stays.
        if (event.currentTarget.value === NEW_USAGE_PROFILE) {
          event.currentTarget.value = draft.usageProfile || '';
          openUsageRulesManager({ tab: 'profiles', newProfile: true });
          return;
        }
        update('usageProfile', event.currentTarget.value);
      }}
      help="Usage profiles budget each agent (see Usage limits & budgets). Leave inherited unless this agent needs its own budget."
      open=${helpOpen === 'agent-spawn-usage-profile'} setOpen=${setHelpOpen}
      busy=${busy} />` : null}
    <${HelpField} id="agent-spawn-approval" label=${draft.harness === 'codex' ? 'Approval policy' : 'Permission mode'}
      title="Controls when the new agent requests approval; it does not change the sandbox."
      value=${draft.approval}
      options=${view.approval.modes.map((mode) => ({
        value: mode, label: approvalPolicyLabel(draft.harness, mode, view.approval.recommended),
      }))}
      onChange=${(event) => update('approval', event.currentTarget.value)}
      showCaveat=${approvalCaveatVisibleFor(draft, launchDefaults?.implementation)}
      help=${approvalHelp} open=${helpOpen === 'agent-spawn-approval'} setOpen=${setHelpOpen}
      disabled=${!view.approval.visible || !showApprovalControls} busy=${busy} />
    <div class=${`cron-create-row${sandboxInfo.length === 0 ? ' sandbox-info-pending' : ''}`}
      id="agent-spawn-sandbox-info">
      <span class="cron-create-label"></span>
      <div class="cron-create-target" role="status">
        ${sandboxInfo.map((message) => html`
          <div class="spawn-field-hint info" key=${message}>ℹ ${message}</div>`)}
      </div>
    </div>
    ${/* Sits between the two selects it is about — the sandbox above and the
          permission mode right here — because the warning is a statement about
          their combination, not about either field alone. role="alert" so a
          screen reader hears it when a mode change introduces it; the per-field
          caveats above are aria-hidden precisely because they duplicate the
          select's own description, while this one has no other voice. */
      autonomyWarnings.length > 0 && html`
      <div class="cron-create-row" id="agent-spawn-autonomy-warning">
        <span class="cron-create-label"></span>
        <div class="cron-create-target" role="alert">
          ${autonomyWarnings.map((warning) => html`
            <div class="spawn-field-hint warn" key=${warning}>${warning}</div>`)}
        </div>
      </div>`}
    <${HelpField} id="agent-spawn-approval-reviewer" label="Approval reviewer"
      title="Controls who decides eligible approval requests; it does not change the approval policy or sandbox."
      value=${draft.approvalReviewer} options=${approvalReviewerOptions(false)}
      onChange=${(event) => update('approvalReviewer', event.currentTarget.value)}
      help=${reviewerHelp} open=${helpOpen === 'agent-spawn-approval-reviewer'} setOpen=${setHelpOpen}
      disabled=${!view.showApprovalReviewer || !showApprovalControls} busy=${busy} />
    <${HelpField} id="agent-spawn-tools" label="Tool governance"
      title="Uniform action for OpenCode's bash, glob, grep, lsp, task, and skill tools."
      value=${draft.tools} options=${SettingOptions({ setting: view.tools })}
      onChange=${(event) => update('tools', event.currentTarget.value)}
      help=${toolsHelp} open=${helpOpen === 'agent-spawn-tools'} setOpen=${setHelpOpen}
      disabled=${!view.tools.visible} busy=${busy} />
    <${HelpField} id="agent-spawn-ask-timeout" label="Question timeout"
      title="AskUserQuestion idle-timeout for the new agent."
      value=${draft.askTimeout} options=${SettingOptions({ setting: view.askTimeout })}
      onChange=${(event) => update('askTimeout', event.currentTarget.value)}
      help=${askTimeoutHelp} open=${helpOpen === 'agent-spawn-ask-timeout'} setOpen=${setHelpOpen}
      disabled=${!view.askTimeout.visible} busy=${busy} />
    <label class="cron-create-enabled cron-check-aligned" id="agent-spawn-trust-dir-row" hidden=${!view.showTrustDir}
      title="Pre-trust the launch directory so the new agent doesn't freeze on the harness's trust-folder dialog.">
      <input id="agent-spawn-trust-dir" type="checkbox" checked=${draft.trustDir} disabled=${busy}
        onChange=${(event) => {
          touched.current.add('trustDir'); touched.current.add('trustDirSpecified');
          profileOverrides.current.add('trustDir'); profileOverrides.current.add('trustDirSpecified');
          operatorSet.current.add('trustDir'); operatorSet.current.add('trustDirSpecified');
          setDraft((before) => ({ ...before, trustDir: event.currentTarget.checked, trustDirSpecified: true }));
        }} /> ${`Pre-trust this directory — skip the trust-folder dialog (edits ${view.trustDirStore || "the harness's config"})`}
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">CWD</span>
      <input id="agent-spawn-cwd" type="text" value=${draft.cwd} disabled=${busy}
        onInput=${(event) => { touched.current.add('cwd'); operatorSet.current.add('cwd'); setDraft((before) => setSpawnCwd(before, event.currentTarget.value)); }}
        placeholder="optional — prefilled from the group's default dir; ~ expands to home" autocomplete="off" spellcheck="false" />
      <button id="agent-spawn-cwd-browse" type="button" class="dir-browse-btn" disabled=${busy || !!browseBusy || away}
        title=${awayHint || 'Browse for a directory'} onClick=${() => { void browse('cwd'); }}>
        ${browseBusy === 'cwd' ? 'Opening…' : 'Browse…'}
      </button>
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Worktree repo</span>
      <input id="agent-spawn-wt-repo" type="text" list="agent-spawn-subrepo-list" value=${draft.wtRepo} disabled=${busy}
        onInput=${(event) => { touched.current.add('wtRepo'); setDraft((before) => setSpawnWorktreeRepo(before, event.currentTarget.value)); }}
        placeholder="git repo to worktree — defaults to CWD; for a monorepo, pick a sub-repo" autocomplete="off" spellcheck="false" />
      <datalist id="agent-spawn-subrepo-list">
        ${(worktrees.subRepos || []).map((repo) => html`<option key=${repo.path} value=${repo.path}>${repo.rel}</option>`)}
      </datalist>
      <button id="agent-spawn-wt-repo-browse" type="button" class="dir-browse-btn" disabled=${busy || !!browseBusy || away}
        title=${awayHint || 'Browse for a directory'} onClick=${() => { void browse('repo'); }}>
        ${browseBusy === 'repo' ? 'Opening…' : 'Browse…'}
      </button>
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Worktree</span>
      <select id="agent-spawn-worktree" value=${draft.worktree} disabled=${busy || !worktreeUsable}
        onChange=${(event) => {
          for (const key of ['worktree', 'worktreeBranch', 'syncWorktree']) touched.current.add(key);
          profileOverrides.current.add('syncWorktree');
          setDraft((before) => selectSpawnWorktree(before, event.currentTarget.value));
        }}>
        <option value="">${worktreeEmptyLabel}</option>
        ${(worktrees.worktrees || []).map((worktree) => {
          const branch = worktree.branch || '(detached)';
          const main = worktree.is_main ? ' [main]' : '';
          return html`<option key=${worktree.path} value=${`wt:${worktree.path}`}
            title=${`${branch}${main} — ${worktree.path}`}>${branch}${main} — ${shortCwd(worktree.path)}</option>`;
        })}
        ${worktreeUsable && html`<option value=${WT_NEW}>+ create new worktree…</option>`}
      </select>
    </label>
    <label class=${`cron-create-enabled cron-check-aligned${worktreeUsable ? '' : ' disabled'}`} id="agent-spawn-wt-sync-row"
      title="Spawn the agent in a fresh git worktree whose branch is named after the name above. Needs a CWD inside a git repo.">
      <input id="agent-spawn-wt-sync" type="checkbox" checked=${draft.syncWorktree} disabled=${busy || !worktreeUsable}
        onChange=${(event) => {
          for (const key of ['syncWorktree', 'worktree', 'worktreeBranch']) touched.current.add(key);
          profileOverrides.current.add('syncWorktree');
          setDraft((before) => syncSpawnWorktree({ ...before, syncWorktree: event.currentTarget.checked }, worktreeUsable));
        }} />
      Sync worktree branch with name
    </label>
    <label class=${`cron-create-enabled cron-check-aligned${worktreeUsable ? '' : ' disabled'}`} id="agent-spawn-wt-fetch-row"
      title="Before cutting a new worktree branch, fetch its base branch and use the freshly updated remote-tracking ref. A repository with no git remote skips the fetch. Any other fetch failure stops the spawn.">
      <input id="agent-spawn-wt-fetch" type="checkbox" checked=${draft.fetchLatestWorktree} disabled=${busy || !worktreeUsable}
        onChange=${(event) => {
          touched.current.add('fetchLatestWorktree');
          profileOverrides.current.add('fetchLatestWorktree');
          setDraft((before) => ({ ...before, fetchLatestWorktree: event.currentTarget.checked }));
        }} />
      Fetch latest worktree base
    </label>
    <label class="cron-create-row" id="agent-spawn-wt-new-row" hidden=${draft.worktree !== WT_NEW}>
      <span class="cron-create-label">New branch</span>
      <input id="agent-spawn-wt-branch" type="text" value=${draft.worktreeBranch} disabled=${busy}
        onInput=${(event) => {
          // Editing the branch also claims the new-worktree selection it belongs
          // to, which name sync may have made, so a profile change keeps both.
          touched.current.add('worktree'); touched.current.add('worktreeBranch'); touched.current.add('syncWorktree');
          profileOverrides.current.add('syncWorktree');
          setDraft((before) => ({ ...before, worktreeBranch: event.currentTarget.value, syncWorktree: false }));
        }}
        placeholder="branch name for the new worktree" autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row" id="agent-spawn-wt-base-row"
      hidden=${draft.worktree !== WT_NEW || !worktrees.hasCommits}>
      <span class="cron-create-label">Base branch</span>
      <select id="agent-spawn-wt-base" value=${draft.worktreeBase} disabled=${busy}
        onChange=${(event) => update('worktreeBase', event.currentTarget.value)}>
        ${(worktrees.branches || []).map((branch) => html`<option key=${branch} value=${branch}>${branch}</option>`)}
      </select>
    </label>
    <p class="wt-orphan-warn" id="agent-spawn-wt-orphan-hint"
      hidden=${draft.worktree !== WT_NEW || worktrees.hasCommits}>
      ⚠ This repo has no commits yet, so the worktree will be cut as an
      <strong>orphan branch</strong> (no base to branch off). That's fine for bootstrapping a fresh repo.
    </p>
    <div class="cron-create-sep" aria-hidden="true"></div>
    <label class="cron-create-enabled" id="agent-spawn-focus-row"
      title="After the agent spawns, open a terminal window attached to its tclaude session.">
      <input id="agent-spawn-focus" type="checkbox" checked=${draft.autoFocus} disabled=${busy}
        onChange=${(event) => update('autoFocus', event.currentTarget.checked)} />
      Auto focus — open a terminal attached to the new agent
    </label>
    <label class="cron-create-enabled" id="agent-spawn-group-context-row"
      hidden=${!groupHasContext(context.groups, draft.group)}
      title="Include this group's shared startup context in the new agent's inbox briefing.">
      <input id="agent-spawn-group-context" type="checkbox" checked=${draft.includeGroupContext} disabled=${busy}
        onChange=${(event) => update('includeGroupContext', event.currentTarget.checked)} />
      Include group default context
    </label>
    <label class="cron-create-enabled" id="agent-spawn-remote-control-row" hidden=${!view.showRemoteControl}
      title="Start the new agent with Claude Code Remote Access ON.">
      <input id="agent-spawn-remote-control" type="checkbox" checked=${draft.remoteControl} disabled=${busy}
        onChange=${(event) => update('remoteControl', event.currentTarget.checked)} />
      Start with remote control — reachable from the Claude app (claude --remote-control)
    </label>
    <label class="cron-create-enabled" id="agent-spawn-auto-memory-row" hidden=${!view.showAutoMemory}
      title="Claude Code's built-in auto memory. tclaude disables it by default: agents sharing a repo all read one per-project memory store and cross-pollute each other's notes. Does not affect CLAUDE.md.">
      <input id="agent-spawn-auto-memory" type="checkbox" checked=${draft.autoMemory} disabled=${busy}
        onChange=${(event) => update('autoMemory', event.currentTarget.checked)} />
      Keep Claude Code auto memory on — off by default to stop agents cross-polluting one project memory
    </label>
    <label class="cron-create-enabled" id="agent-spawn-peer-messaging-row" hidden=${!view.showPeerMessaging}
      title="Claude Code's built-in cross-session messaging (the ListAgents and SendMessage tools over a per-session socket). tclaude closes it by default: it is a second coordination channel outside tclaude's groups, permissions and audit trail — use tclaude agent send instead. In-harness subagents are unaffected; the deny names ListAgents, never SendMessage.">
      <input id="agent-spawn-peer-messaging" type="checkbox" checked=${draft.peerMessaging} disabled=${busy}
        onChange=${(event) => update('peerMessaging', event.currentTarget.checked)} />
      Keep Claude Code cross-session messaging on — off by default so agents coordinate through tclaude
    </label>
    <label class="cron-create-enabled" id="agent-spawn-copilot-api-row" hidden=${!view.showCopilotAPI}
      title=${COPILOT_API_TITLE}>
      <input id="agent-spawn-copilot-api" type="checkbox" checked=${draft.copilotAPI} disabled=${busy}
        onChange=${(event) => update('copilotAPI', event.currentTarget.checked)} />
      Drive over the Copilot API instead of tmux send-keys — experimental, off by default
    </label>
    <label class="cron-create-enabled" id="agent-spawn-codex-app-server-row" hidden=${!view.showCodexAppServer}
      title="EXPERIMENTAL: launch the normal Codex TUI against a private per-agent app-server. The TUI submits the initial prompt exactly once; agentd binds the created thread without replay. Requires Codex 0.147.x and fails closed if the server or verified thread handle is unavailable. Off by default so send-keys remains selectable for A/B testing.">
      <input id="agent-spawn-codex-app-server" type="checkbox" checked=${draft.codexAppServer} disabled=${busy}
        onChange=${(event) => update('codexAppServer', event.currentTarget.checked)} />
      Drive Codex through a private app-server — experimental, off by default
    </label>
    ${nativeCodexRegistryWarning && html`
      <div class="cron-create-row" id="agent-spawn-codex-native-registry-warning">
        <span class="cron-create-label"></span>
        <div class="cron-create-target" role="alert">
          <div class="spawn-field-hint warn">
            ⚠ Native Codex permission-profile integration is not ready; this launch will be refused: ${nativeCodexRegistryWarning}.
            <a href=${CODEX_NATIVE_REGISTRY_SETUP_DOC} target="_blank" rel="noopener">Setup and repair</a>
          </div>
        </div>
      </div>`}
    <label class="cron-create-enabled" id="agent-spawn-ssh-workaround-row" hidden=${!view.showSSHWorkaround}
      title="Use an agent-owned SSH client config when the launch needs it. It activates only for the Codex managed sandbox or tclaude's sandbox layer, and overrides Git core.sshCommand.">
      <input id="agent-spawn-ssh-workaround" type="checkbox"
        checked=${draft.sshWorkaround}
        disabled=${busy}
        onChange=${(event) => update('sshWorkaround', event.currentTarget.checked)} />
      Use SSH compatibility workaround
    </label>
    <${ErrorBanner} error=${error} onDismiss=${() => setError('')} />
    <${FormFillBar} form="spawn" keepValues=${!!queued} fillable=${!current.options.autoSpawn}
      claims=${Object.keys(draft.contextFeatures || {}).length ? SPAWN_CONTEXT_CLAIM : NO_CLAIMS} />
    <div class="modal-buttons">
      ${queued && html`<button id="agent-spawn-queued-discard" class="confirm-danger" type="button" disabled=${busy}
        title="Remove this prepared spawn from the queue without launching it"
        onClick=${() => { void discardQueued(); }}>Discard</button>`}
      <button id="agent-spawn-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>${queued ? 'Close' : 'Cancel'}</button>
      <span id="agent-spawn-progress" class="agent-spawn-progress" role="status" aria-live="polite">
        ${busy ? progress : ''}
      </span>
      <span class="spacer"></span>
      <button id="agent-spawn-queue" class="agent-spawn-queue-btn" type="button" disabled=${busy}
        title=${queued
          ? 'Save changes and keep it queued (Ctrl/⌘+Enter)'
          : 'Prepare this agent without starting it — launch it later from ⏸ queued. Worktree creation, branch fetch and the spawn itself happen at launch. (Ctrl/⌘+Shift+Enter)'}
        onClick=${() => { void saveQueued(); }}>
        ${queued ? 'Save' : '⏸ Add to queue'}
      </button>
      <button id="agent-spawn-submit" class=${`${queued ? 'agent-spawn-launch-btn' : 'primary'}${busy ? ' slop-pull-active' : ''}`} type="button"
        disabled=${busy} aria-busy=${busy ? 'true' : undefined}
        title=${queued ? 'Launch it now and remove it from the queue' : requestOnly ? `Send the request to ${remote.node}'s operator (Ctrl/⌘+Enter)` : 'Spawn the agent now (Ctrl/⌘+Enter)'}
        onClick=${() => { void submit(); }}>
        ${busy && !queueing ? (requestOnly ? 'Sending…' : 'Spawning…') : (queued ? '▶ Spawn now' : requestOnly ? 'Send request' : 'Spawn')}
      </button>
    </div>
  </${Overlay}>`;
}

export function AgentSpawnApp(props) {
  const current = props.state.dialog.value;
  // The upload tray outlives the dialog, so an upload stays visible after it closes.
  if (!current) return html`<${TransferTray} />`;
  return html`<${Fragment}><${AgentSpawnDialog} key=${current.key} current=${current}
    state=${props.state} actions=${props.actions} confirmDiscard=${props.confirmDiscard} ...${props.notify ? { notify: props.notify } : {}} /><${TransferTray} /></${Fragment}>`;
}

export function mountAgentSpawnIsland({ host, state, actions, confirmDiscard, registerCleanup }) {
  const unregister = registerAgentSpawnController(Object.freeze({
    open: state.open,
    refreshSandboxPolicy: state.refreshSandboxPolicy,
  }));
  render(html`<${AgentSpawnApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  registerCleanup(() => {
    unregister();
    state.dispose();
    render(null, host);
  });
}
