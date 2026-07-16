import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { shortCwd } from './helpers.js';
import {
  MODEL_CUSTOM_VALUE,
  WT_NEW,
  applySpawnProfile,
  attachKey,
  buildSpawnRequest,
  clearSpawnProfileFields,
  createSpawnDraft,
  deriveSpawnNameFromMessage,
  findSpawnProfile,
  formatAttachmentSize,
  groupHasContext,
  modelSelectValue,
  prepareSpawnDraft,
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
  spawnPermissionIndicator,
  spawnProfileChoices,
  spawnProfileSeed,
  syncSpawnWorktree,
  validateSpawnDraft,
} from './agent-spawn-model.js';
import { registerAgentSpawnController } from './agent-spawn-controller.js';
import { approvalPolicyLabel, approvalReviewerHelp, approvalReviewerOptions } from './approval-controls.js';

const html = htm.bind(h);
const PASTE_REPEAT_MS = 1000;
const PROFILE_OWNED_FIELDS = [
  'profile', 'name', 'role', 'descr', 'task', 'initialMessage',
  'harness', 'model', 'customModel', 'effort', 'sandbox', 'approval', 'approvalReviewer', 'askTimeout',
  'trustDir', 'trustDirSpecified', 'remoteControl', 'owner', 'permissionOverrides',
  'syncWorktree', 'autoFocus', 'includeGroupContext',
];

function errorMessage(error) {
  return error?.message || String(error);
}

function Words({ plain, wizard, prefix = 'theme-copy' }) {
  return html`<span class=${`${prefix}-regular`}>${plain}</span><span class=${`${prefix}-wizard`}>${wizard}</span>`;
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

function HelpField({
  id, descriptionID = `${id}-hint`, label, title, value, options,
  onChange, help, open, setOpen, disabled = false, busy = false,
}) {
  return html`<div class="cron-create-row" id=${`${id}-row`} title=${title} hidden=${disabled}>
    <label class="cron-create-label" for=${id}>${label}</label>
    <div class="cron-create-target spawn-field-with-help">
      <select id=${id} value=${value} title=${help} aria-describedby=${descriptionID} disabled=${busy}
        onChange=${onChange}>
        ${options.map((option) => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}
      </select>
      <button type="button" class="spawn-field-help-trigger" aria-label=${`Show ${label} help`}
        aria-controls=${descriptionID} aria-expanded=${open ? 'true' : 'false'} title=${`Show ${label} help`}
        disabled=${busy}
        onClick=${() => setOpen(open ? '' : id)}
        onFocus=${() => setOpen(id)}>?</button>
      <span id=${descriptionID} class="spawn-field-description" role="tooltip" tabindex="0"
        aria-live="polite" onFocus=${() => setOpen(id)}>${help}</span>
    </div>
  </div>`;
}

function SettingOptions({ setting }) {
  return setting.modes.map((mode) => ({
    value: mode,
    label: `${mode}${mode === setting.recommended ? ' (recommended)' : ''}`,
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

function AgentSpawnDialog({ current, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const context = useMemo(() => ({
    groups: current.groups,
    harnesses: current.harnesses,
    userDefaultModel: current.userDefaultModel,
    normalizeNames: current.normalizeNames,
  }), [current]);
  const rememberedEffort = (model) => actions.rememberedEffort(model);
  const initial = useMemo(() => {
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
  const [sandboxPolicy, setSandboxPolicy] = useState({ profiles: [], preview: '', error: '' });
  const [attachments, setAttachments] = useState([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [browseBusy, setBrowseBusy] = useState('');
  const [helpOpen, setHelpOpen] = useState('');
  const [dragOver, setDragOver] = useState(false);
  const nameRef = useRef(null);
  const fileRef = useRef(null);
  const touched = useRef(new Set());
  const submitLock = useRef(false);
  const busyRef = useRef(false);
  const profileRequest = useRef(0);
  const worktreeRequest = useRef(0);
  const sandboxRequest = useRef(0);
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
  const view = spawnCapabilityView(draft, context);
  const dirty = spawnDraftIsDirty(draft, baseline, attachments.length);
  const nameHint = spawnNameHint(draft.name, context.normalizeNames);
  const permissionsLabel = spawnPermissionIndicator(draft.permissionOverrides);

  useEffect(() => () => {
    profileRequest.current += 1;
    worktreeRequest.current += 1;
    sandboxRequest.current += 1;
    directoryRequest.current += 1;
    for (const attachment of attachmentsRef.current) {
      if (attachment.url) URL.revokeObjectURL(attachment.url);
    }
    attachmentsRef.current = [];
  }, []);

  useEffect(() => {
    const request = ++profileRequest.current;
    const generation = current.generation;
    actions.loadProfiles().then((nextProfiles) => {
      if (request !== profileRequest.current || !state.isCurrent(generation)) return;
      setProfiles(nextProfiles);
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
      if (current.options.role) {
        nextBaseline = { ...nextBaseline, role: current.options.role };
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
      setSandboxPolicy((value) => ({ ...value, preview: '', error: '' }));
      return undefined;
    }
    actions.loadSandboxPolicy(draft.group, draft.sandboxProfile).then((value) => {
      if (request !== sandboxRequest.current || !state.isCurrent(generation)) return;
      setSandboxPolicy({ ...value, error: '' });
      if (value.selected !== draft.sandboxProfile) {
        setDraft((before) => ({ ...before, sandboxProfile: value.selected }));
      }
    }).catch((cause) => {
      if (request !== sandboxRequest.current || !state.isCurrent(generation)) return;
      setSandboxPolicy((value) => ({
        ...value,
        preview: `Could not preview sandbox policy: ${errorMessage(cause)}`,
        error: errorMessage(cause),
      }));
    });
    return undefined;
  }, [draft.group, draft.sandboxProfile, view.sandboxProfilesDisabled, current.sandboxRevision]);

  const update = (key, value) => {
    touched.current.add(key);
    setDraft((before) => ({ ...before, [key]: value }));
  };
  const updateName = (value) => {
    touched.current.add('name');
    setDraft((before) => syncSpawnWorktree({ ...before, name: value }, worktrees.isRepo));
  };
  const changeGroup = (value) => {
    touched.current.add('group');
    touched.current.add('cwd');
    touched.current.add('wtRepo');
    touched.current.add('remoteControl');
    setDraft((before) => selectSpawnGroup(before, value, context));
  };
  const changeHarness = (value) => {
    touched.current.add('harness');
    for (const key of ['model', 'effort', 'sandbox', 'approval', 'approvalReviewer', 'askTimeout', 'trustDir', 'remoteControl']) {
      touched.current.add(key);
    }
    setDraft((before) => selectSpawnHarness(before, value, context, rememberedEffort));
  };
  const changeModel = (value) => {
    if (value === MODEL_CUSTOM_VALUE) {
      touched.current.add('model');
      touched.current.add('customModel');
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
    setDraft((before) => ({
      ...before, model: value, customModel: false, effort: rememberedEffort(value),
    }));
  };
  const changeProfile = (handle) => {
    if (!handle) {
      for (const field of PROFILE_OWNED_FIELDS) touched.current.add(field);
      setDraft((before) => clearSpawnProfileFields(before, context, {
        autoFocus: actions.autoFocusDefault(), rememberedEffort,
      }));
      return;
    }
    touched.current.add('profile');
    const profile = findSpawnProfile(profiles, handle);
    if (profile) {
      setDraft((before) => ({
        ...applySpawnProfile(before, profile, context, rememberedEffort, worktrees.isRepo),
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
      label: draft.name.trim(),
      onSave: (kept) => {
        if (!state.isCurrent(generation)) return;
        touched.current.add('permissionOverrides');
        setDraft((before) => ({ ...before, permissionOverrides: { ...kept } }));
      },
    });
  };
  const saveProfile = () => {
    const generation = current.generation;
    actions.openProfileEditor(spawnProfileSeed(draft, context), (name) => {
      actions.loadProfiles(true).then((nextProfiles) => {
        if (!state.isCurrent(generation)) return;
        setProfiles(nextProfiles);
        setDraft((before) => ({ ...before, profile: name }));
      }).catch(() => {});
    });
  };

  const submit = async () => {
    if (submitLock.current) return;
    submitLock.current = true;
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
    next = prepareSpawnDraft(next, context, derived, worktrees.isRepo);
    setDraft(next);
    setError('');
    busyRef.current = true;
    setBusy(true);
    actions.rememberLaunchPreferences(next);
    try {
      const worktreeKey = JSON.stringify([
        next.wtRepo, next.worktree, next.worktreeBranch, next.worktreeBase,
      ]);
      let worktreeSelection = resolvedWorktree.current.key === worktreeKey
        ? resolvedWorktree.current.value : null;
      if (!worktreeSelection) {
        worktreeSelection = await actions.resolveWorktree(next, worktrees);
        resolvedWorktree.current = { key: worktreeKey, value: worktreeSelection };
      }
      if (!state.isCurrent(current.generation)) return;
      const uploadKey = attachments.map((attachment) => `${attachment.id}:${attachKey(attachment.file)}`).join('|');
      let attachmentPaths = uploaded.current.key === uploadKey ? uploaded.current.paths : null;
      if (!attachmentPaths) {
        attachmentPaths = await actions.uploadAttachments(attachments);
        uploaded.current = { key: uploadKey, paths: attachmentPaths };
      }
      if (!state.isCurrent(current.generation)) return;
      const request = buildSpawnRequest(next, context, worktreeSelection, attachmentPaths);
      const payload = await actions.spawn(request);
      if (!state.isCurrent(current.generation)) return;
      state.close();
      actions.complete(payload, next);
    } catch (cause) {
      if (state.isCurrent(current.generation)) {
        setError(errorMessage(cause));
        busyRef.current = false;
        setBusy(false);
        submitLock.current = false;
      }
    }
  };

  const selectedModel = modelSelectValue(draft, context);
  const sandboxHelp = view.sandbox.help[draft.sandbox] || '';
  const approvalHelp = view.approval.help[draft.approval] || '';
  const reviewerHelp = approvalReviewerHelp(draft.approvalReviewer, draft.approval);
  const askTimeoutHelp = view.askTimeout.help[draft.askTimeout] || '';
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
    onSubmitHotkey=${submit}
    dirty=${dirty}
    blocked=${busy}
    confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}
    resizeKey="tclaude.dash.modalSize.agent-spawn"
    guardBackdropDrag=${true}
    initialFocusRef=${nameRef}
    dialogClass=${`cron-create-modal${dragOver ? ' spawn-drag-over' : ''}`}
    onDragEnter=${dragEnter}
    onDragOver=${dragOverEvent}
    onDragLeave=${dragLeave}
    onDrop=${drop}
    onPaste=${paste}
  >
    <h3 id="agent-spawn-title"><${Words} prefix="spawn-title"
      plain="Spawn a new agent" wizard="Summon a new familiar" /></h3>
    <div class="modal-meta" id="agent-spawn-meta" hidden=${!draft.fixedGroup}>
      ${draft.fixedGroup ? `joining group: ${draft.group}` : ''}
    </div>
    <label class="cron-create-row" id="agent-spawn-load-profile-row"
      title="Pre-fill this dialog from a saved spawn profile — a reusable bundle of the harness / model / effort / sandbox + name / role / descr / initial-message fields (NOT the directory or worktree).">
      <span class="cron-create-label"><${Words} prefix="profiles-word" plain="Profile" wizard="Pattern" /></span>
      <div class="cron-create-target"><div class="cron-target-input-row">
        <select id="agent-spawn-load-profile" value=${draft.profile} disabled=${busy}
          onChange=${(event) => changeProfile(event.currentTarget.value)}>
          <option value="">— none (blank form) —</option>
          ${spawnProfileChoices(profiles).map((choice) => html`<option key=${choice.value} value=${choice.value}>${choice.label}</option>`)}
        </select>
        <button id="agent-spawn-clear" type="button" disabled=${busy}
          title="Reset the profile-filled fields (harness / model / effort / name / role / …) to blank — leaves the group, directory and worktree untouched"
          onClick=${() => changeProfile('')}>Clear</button>
        <button id="agent-spawn-save-profile" type="button" disabled=${busy}
          title="Save the current dialog fields as a reusable spawn profile — opens the profile editor pre-filled so you can name it (the directory and worktree aren't stored)"
          onClick=${saveProfile}><${Words} prefix="profiles-word" plain="Save as profile…" wizard="Save as pattern…" /></button>
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
        placeholder="optional — the new agent's task brief; delivered to its inbox, newlines preserved (≤16384 chars)" spellcheck="false"></textarea>
    </label>
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
    <div class="cron-create-row spawn-role-row">
      <span class="cron-create-label">Role</span>
      <input id="agent-spawn-role" type="text" value=${draft.role} disabled=${busy}
        onInput=${(event) => update('role', event.currentTarget.value)}
        placeholder="optional — short tag (e.g. researcher, planner)" autocomplete="off" spellcheck="false" />
      <label class="spawn-owner-toggle" title="Make the new agent a group owner of the destination group at birth.">
        <input id="agent-spawn-owner" type="checkbox" checked=${draft.owner} disabled=${busy}
          onChange=${(event) => update('owner', event.currentTarget.checked)} /> owner
      </label>
      <button type="button" id="agent-spawn-perms" disabled=${busy}
        title="Set the new agent's permanent per-slug permissions (grant / deny / inherit) to apply when it spawns."
        onClick=${openPermissions}>Permissions…</button>
      <span id="agent-spawn-perms-indicator" class="spawn-perms-indicator" hidden=${!permissionsLabel}>${permissionsLabel}</span>
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
        placeholder="optional — Linear/GitHub/ticket URL (http(s)); shown in the Task column" autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row" id="agent-spawn-harness-row"
      title="Coding harness for the new agent. The harness switches the Model + Effort menus and launch controls.">
      <span class="cron-create-label">Harness</span>
      <select id="agent-spawn-harness" value=${draft.harness} disabled=${busy}
        onChange=${(event) => changeHarness(event.currentTarget.value)}>
        ${context.harnesses.map((harness) => html`<option key=${harness.name} value=${harness.name}>${harness.display_name || harness.name}</option>`)}
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
    </div>
    <label class="cron-create-row" id="agent-spawn-model-custom-row" hidden=${selectedModel !== MODEL_CUSTOM_VALUE}
      title="Type any model id or alias accepted by the selected harness. Validated when the agent spawns.">
      <span class="cron-create-label"></span>
      <input id="agent-spawn-model-custom" type="text" aria-label="Custom model id" value=${draft.model} disabled=${busy}
        onInput=${(event) => {
          const value = event.currentTarget.value;
          touched.current.add('model'); touched.current.add('effort');
            setDraft((before) => ({
              ...before, model: value, customModel: true, effort: rememberedEffort(value.trim()),
            }));
        }}
        placeholder="model id or alias" autocomplete="off" spellcheck="false" />
    </label>
    <${HelpField} id="agent-spawn-sandbox" label="Sandbox"
      title="Launch containment for the new agent. The modes are per-harness."
      value=${draft.sandbox} options=${SettingOptions({ setting: view.sandbox })}
      onChange=${(event) => {
        const value = event.currentTarget.value;
        touched.current.add('sandbox');
        setDraft((before) => ({
          ...before, sandbox: value,
          sandboxProfile: before.harness === 'codex' && value === 'danger-full-access' ? '' : before.sandboxProfile,
        }));
      }} help=${sandboxHelp} open=${helpOpen === 'agent-spawn-sandbox'} setOpen=${setHelpOpen}
      disabled=${!view.sandbox.visible} busy=${busy} />
    <${HelpField} id="agent-spawn-sandbox-profile" descriptionID="agent-spawn-sandbox-profile-preview" label="Sandbox profile"
      title="Optional explicit sandbox profile. Composes after the global and group sandbox profiles."
      value=${draft.sandboxProfile}
      options=${[
        { value: '', label: '— global + group defaults only —' },
        ...(sandboxPolicy.profiles || []).map((profile) => ({ value: profile.name, label: profile.name })),
      ]}
      onChange=${(event) => update('sandboxProfile', event.currentTarget.value)}
      help=${sandboxPolicy.preview} open=${helpOpen === 'agent-spawn-sandbox-profile'} setOpen=${setHelpOpen}
      disabled=${view.sandboxProfilesDisabled} busy=${busy} />
    <label class="cron-create-row" id="agent-spawn-approval-row" hidden=${!view.approval.visible}
      title="Controls when the new agent requests approval; it does not change the sandbox.">
      <span class="cron-create-label">${draft.harness === 'codex' ? 'Approval policy' : 'Permission mode'}</span>
      <div class="cron-create-target">
        <select id="agent-spawn-approval" value=${draft.approval} disabled=${busy}
          aria-describedby="agent-spawn-approval-hint" onChange=${(event) => update('approval', event.currentTarget.value)}>
          ${view.approval.modes.map((mode) => html`<option key=${mode} value=${mode}>${approvalPolicyLabel(draft.harness, mode, view.approval.recommended)}</option>`)}
        </select>
        <div id="agent-spawn-approval-hint" class=${`spawn-field-hint${approvalHelp.includes('⚠') ? ' warn' : ''}`}
          aria-live="polite">${approvalHelp}</div>
      </div>
    </label>
    <${HelpField} id="agent-spawn-approval-reviewer" label="Approval reviewer"
      title="Controls who decides eligible approval requests; it does not change the approval policy or sandbox."
      value=${draft.approvalReviewer} options=${approvalReviewerOptions(false)}
      onChange=${(event) => update('approvalReviewer', event.currentTarget.value)}
      help=${reviewerHelp} open=${helpOpen === 'agent-spawn-approval-reviewer'} setOpen=${setHelpOpen}
      disabled=${!view.showApprovalReviewer} busy=${busy} />
    <label class="cron-create-row" id="agent-spawn-ask-timeout-row" hidden=${!view.askTimeout.visible}
      title="AskUserQuestion idle-timeout for the new agent.">
      <span class="cron-create-label">Question timeout</span>
      <div class="cron-create-target">
        <select id="agent-spawn-ask-timeout" value=${draft.askTimeout} disabled=${busy}
          aria-describedby="agent-spawn-ask-timeout-hint" onChange=${(event) => update('askTimeout', event.currentTarget.value)}>
          ${SettingOptions({ setting: view.askTimeout }).map((option) => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}
        </select>
        <div id="agent-spawn-ask-timeout-hint" class=${`spawn-field-hint${askTimeoutHelp.includes('⚠') ? ' warn' : ''}`}
          aria-live="polite">${askTimeoutHelp}</div>
      </div>
    </label>
    <label class="cron-create-enabled cron-check-aligned" id="agent-spawn-trust-dir-row" hidden=${!view.showTrustDir}
      title="Pre-trust the launch directory for Codex so the new agent doesn't freeze on Codex's trust-folder modal.">
      <input id="agent-spawn-trust-dir" type="checkbox" checked=${draft.trustDir} disabled=${busy}
        onChange=${(event) => {
          touched.current.add('trustDir'); touched.current.add('trustDirSpecified');
          setDraft((before) => ({ ...before, trustDir: event.currentTarget.checked, trustDirSpecified: true }));
        }} /> Pre-trust this directory for Codex — skip the trust-folder modal (edits ~/.codex/config.toml)
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">CWD</span>
      <input id="agent-spawn-cwd" type="text" value=${draft.cwd} disabled=${busy}
        onInput=${(event) => { touched.current.add('cwd'); setDraft((before) => setSpawnCwd(before, event.currentTarget.value)); }}
        placeholder="optional — prefilled from the group's default dir; ~ expands to home" autocomplete="off" spellcheck="false" />
      <button id="agent-spawn-cwd-browse" type="button" class="dir-browse-btn" disabled=${busy || !!browseBusy}
        title="Open a native directory picker on the daemon's desktop" onClick=${() => { void browse('cwd'); }}>
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
      <button id="agent-spawn-wt-repo-browse" type="button" class="dir-browse-btn" disabled=${busy || !!browseBusy}
        title="Open a native directory picker on the daemon's desktop" onClick=${() => { void browse('repo'); }}>
        ${browseBusy === 'repo' ? 'Opening…' : 'Browse…'}
      </button>
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Worktree</span>
      <select id="agent-spawn-worktree" value=${draft.worktree} disabled=${busy || !worktreeUsable}
        onChange=${(event) => {
          for (const key of ['worktree', 'worktreeBranch', 'syncWorktree']) touched.current.add(key);
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
          setDraft((before) => syncSpawnWorktree({ ...before, syncWorktree: event.currentTarget.checked }, worktreeUsable));
        }} />
      Sync worktree branch with name
    </label>
    <label class="cron-create-row" id="agent-spawn-wt-new-row" hidden=${draft.worktree !== WT_NEW}>
      <span class="cron-create-label">New branch</span>
      <input id="agent-spawn-wt-branch" type="text" value=${draft.worktreeBranch} disabled=${busy}
        onInput=${(event) => {
          touched.current.add('worktreeBranch'); touched.current.add('syncWorktree');
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
    <${ErrorBanner} error=${error} onDismiss=${() => setError('')} />
    <div class="modal-buttons">
      <button id="agent-spawn-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button id="agent-spawn-submit" class=${`primary${busy ? ' slop-pull-active' : ''}`} type="button"
        disabled=${busy} aria-busy=${busy ? 'true' : undefined} onClick=${() => { void submit(); }}>
        ${busy ? 'Spawning…' : 'Spawn'}
      </button>
    </div>
  </${Overlay}>`;
}

export function AgentSpawnApp(props) {
  const current = props.state.dialog.value;
  if (!current) return null;
  return html`<${AgentSpawnDialog} key=${current.key} current=${current}
    state=${props.state} actions=${props.actions} confirmDiscard=${props.confirmDiscard} />`;
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
