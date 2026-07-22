import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { profileSummary, profileAliasesLabel, profileChoices, findProfileByHandle } from './profiles.js';
import { roleSummary } from './roles.js';
import { AUTO_MEMORY_TRI_OPTIONS, dirtyDraft, harnessByName, harnessDefaults, profileDraft, profilePayload, readTri, roleDraft, rolePayload, TRI_OPTIONS } from './management-model.js';
import { registerManagementController } from './management-controller.js';
import { sandboxProfileSummary } from './sandbox-profiles-data.js';
import { assignedBreakGlass, BREAK_GLASS_ACK_CODE, BREAK_GLASS_WARNING, breakGlassRules, describeBreakGlassEntries, resolvedBreakGlass } from './sandbox-break-glass.js';
import { pickDirectory } from './helpers.js';
import { lineDiff } from './line-diff.js';
import { useDialogFocus } from './dialog-focus.js';
import { wizWord } from './slop.js';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { GroupCloneDialog, GroupContextDialog, GroupImportDialog, TemplateDeployDialog, TemplateDuplicateDialog, TemplateEditor, TemplateFromGroupDialog, TemplateImportDialog, TemplateManager, TemplateStartersDialog } from './template-management-island.js';
import { approvalPolicyLabel, approvalReviewerHelp, approvalReviewerOptions } from './approval-controls.js';
import { HelpField } from './help-field.js';

const html = htm.bind(h);

function message(error) { return error?.message || String(error); }
function clone(value) { return JSON.parse(JSON.stringify(value)); }
function change(setDraft, key, value) { setDraft((draft) => ({ ...draft, [key]: value })); }

/* One entry of the common-rule preset menu: a button that inserts the entry's
   audited paths as ordinary deny rows, with its rationale, warning and the
   exact paths it would insert visible before the click. Nothing about the
   entry is stored — after insertion the rows are plain, editable table rows. */
function CommonRuleEntry({ entry, onAdd }) {
  const paths = commonRulePaths(entry);
  // The rationale, the warning and the exact paths are what make the button
  // safe to press, so they are announced with it rather than left as nearby
  // text a screen-reader or keyboard operator can tab straight past.
  const base = `sbx-common-rule-${String(entry.id || '').replace(/[^a-zA-Z0-9_-]+/g, '-')}`;
  const descrID = `${base}-descr`; const warnID = `${base}-warn`; const pathsID = `${base}-paths`;
  const describedBy = [descrID, entry.warning ? warnID : '', pathsID].filter(Boolean).join(' ');
  // An entry with no paths on this platform has nothing to insert, but
  // `disabled` would take it out of the tab order and take its description —
  // which is precisely the explanation of WHY it does nothing — with it.
  // aria-disabled keeps both reachable; the handler refuses instead of the DOM.
  const noPaths = !paths.length;
  return html`<div class="sbx-common-rule-entry" data-rule=${entry.id}>
    <button type="button" class="sbx-common-rule-add" aria-describedby=${describedBy} aria-disabled=${noPaths ? 'true' : null} onClick=${() => { if (!noPaths) onAdd(entry); }}>＋ ${entry.label || entry.id}</button>
    <span class="sbx-common-rule-descr" id=${descrID}>${entry.description || ''}</span>
    ${entry.warning ? html`<span class="sbx-common-rule-warn" id=${warnID}>⚠ ${entry.warning}</span>` : null}
    <code class="sbx-common-rule-paths" id=${pathsID}>${paths.length ? paths.join(' · ') : '(no audited paths on this platform)'}</code>
  </div>`;
}

function commonRulePaths(entry) {
  return [...new Set((entry?.paths || []).map((path) => String(path || '').trim()).filter(Boolean))];
}

/* Comparison-only path identity, mirroring the daemon's own `filepath.Clean`:
   trailing separators, duplicated separators, `.` segments and `..` segments
   all name the same location there, so treating them as distinct lets a preset
   append a deny for a path the operator already authored as `write` — the
   daemon canonicalizes and folds deny over write, silently overriding the
   authored row while the notice claims it was left as authored. `..` is folded
   lexically rather than skipped because that is exactly what the daemon does:
   sandboxpolicy's canonicalization Cleans before it calls EvalSymlinks, so no
   `..` segment ever survives to be resolved against a symlink. Symlinks
   themselves stay unresolved — they need the filesystem — so two names for one
   inode remain distinct here, as they must.

   `~` is an ordinary leading segment: `~/go/` and `~/go` match, `~/go` and
   `/home/op/go` do NOT, even though the daemon expands `~` to its own home
   before cleaning. That alias is a real remaining hole with the same
   silent-override consequence; closing it needs the daemon's home directory,
   which the catalog payload does not carry today (TCL-635). Until then a `~`
   path that also climbs (`~/../x`) folds to a relative identity the daemon
   would have made absolute — harmless, because a relative row cannot save at
   all: canonicalization refuses it loudly rather than folding it silently.

   The inserted row always keeps the catalog's own spelling; only the
   comparison normalizes. */
function pathIdentity(path) {
  const raw = String(path || '').trim();
  if (!raw) return '';
  const rooted = raw.startsWith('/');
  const out = [];
  for (const segment of raw.split('/')) {
    if (!segment || segment === '.') continue;
    if (segment !== '..') { out.push(segment); continue; }
    // `..` past the root is the root, as filepath.Clean has it; on a relative
    // path a leading `..` has nothing to pop and stays.
    if (out.length && out[out.length - 1] !== '..') out.pop();
    else if (!rooted) out.push('..');
  }
  if (rooted) return `/${out.join('/')}`;
  return out.length ? out.join('/') : '.';
}

function RequestList({ request, label, retry, children }) {
  if ((request.phase === 'idle' || request.phase === 'loading') && !request.data?.length) return html`<div class="template-empty">Loading ${label}…</div>`;
  if (request.phase === 'error' && !request.data?.length) return html`<div class="template-empty" role="alert">Could not load ${label}: ${request.error} <button onClick=${retry}>retry</button></div>`;
  return html`${request.phase === 'error' && html`<div class="island-error" role="alert">Refresh failed: ${request.error} <button onClick=${retry}>retry</button></div>`}${children}`;
}

function Manager({ kind, current, state, actions, confirmDiscard }) {
  const profiles = kind === 'profiles'; const roles = kind === 'roles';
  const all = profiles ? current.profiles : roles ? current.roles : current.sandboxProfiles;
  const filter = profiles ? current.profileFilter : roles ? current.roleFilter : current.sandboxFilter;
  const setFilter = profiles ? state.profileFilter : roles ? state.roleFilter : state.sandboxFilter;
  const request = current.requests[kind === 'sandbox' ? 'sandbox' : kind];
  const domKind = kind === 'sandbox' ? 'sandbox-profiles' : kind;
  const q = filter.trim().toLowerCase();
  const list = all.filter((item) => !q || [item.name, ...(item.aliases || []), item.disabled_reason, item.descr, item.role, item.model, item.harness, item.agent_name].some((value) => String(value || '').toLowerCase().includes(q)));
  const title = profiles ? html`<span class="profiles-word-regular">Spawn profiles</span><span class="profiles-word-wizard">Familiar patterns</span>` : roles ? html`<span class="roles-word-regular">Role library</span><span class="roles-word-wizard">Class library</span>` : html`<span class="sandbox-word-regular">Sandbox profiles</span><span class="sandbox-word-wizard">Wards</span>`;
  return html`<${Overlay} id=${`${domKind}-manage-modal`} manage labelledby=${`${domKind}-manage-title`} onClose=${state.closeManager} confirmDiscard=${confirmDiscard}>
    <h3 id=${`${domKind}-manage-title`}>${title}</h3>
    <p class="manage-intro">${profiles ? "Reusable bundles of the spawn dialog's launch and identity fields." : roles ? 'Named reusable role briefs, launch defaults, and permissions.' : 'Filesystem and environment policy applied when an agent launches.'}</p>
    <div class="filter-bar"><input id=${`filter-${kind}`} value=${filter} onInput=${(event) => { setFilter.value = event.currentTarget.value; }} placeholder="Filter" autocomplete="off" spellcheck="false" autofocus /><span class="filter-count" id=${`filter-${kind}-count`}>${q ? `${list.length} / ${all.length}` : all.length}</span><button class="clear-filter" onClick=${() => { setFilter.value = ''; }}>×</button><span class="spacer"></span>
      ${profiles && html`<button id="profile-export-open" class="tool" onClick=${() => state.openDialog({ kind: 'profile-export' })}>⇪ export</button><button id="profile-import-open" class="tool" onClick=${() => state.openDialog({ kind: 'profile-import' })}>⤒ import</button>`}
      ${kind === 'sandbox' && html`<button id="sandbox-profile-export-open" class="tool" onClick=${() => state.openDialog({ kind: 'sandbox-export' })}>⇪ export</button><button id="sandbox-profile-import-open" class="tool" onClick=${() => state.openDialog({ kind: 'sandbox-import' })}>⤒ import</button><button id="sandbox-profile-scribe-open" class="tool" onClick=${() => actions.configureSandboxWithAgent({ name: '', filesystem: [], environment: [], network_access: '' })}>🤖 configure with agent</button>`}
      <button id=${profiles ? 'profile-create-open' : roles ? 'role-create-open' : 'sandbox-profile-create-open'} class="primary" onClick=${() => profiles ? actions.openProfileEditor() : roles ? actions.openRoleEditor() : actions.openSandboxEditor()}>${profiles ? html`<span class="profiles-word-regular">+ new profile</span><span class="profiles-word-wizard">+ new pattern</span>` : roles ? html`<span class="roles-word-regular">+ new role</span><span class="roles-word-wizard">+ new class</span>` : html`<span class="sandbox-word-regular">+ new sandbox profile</span><span class="sandbox-word-wizard">+ new ward</span>`}</button>
    </div>
    <div id=${profiles ? 'profiles-list' : roles ? 'roles-list' : 'sandbox-profiles-list'}><${RequestList} request=${request} label=${kind} retry=${() => actions.load(kind)}>${list.length ? list.map((item) => html`<div key=${item.name} class=${`template-card ${profiles ? 'profile' : roles ? 'role' : 'sandbox-profile'}-card${profiles && item.disabled ? ' profile-card-disabled' : ''}`} data-key=${item.name}><div class="tc-head"><span class="tc-name">${item.name}</span>${profiles && item.disabled ? html`<span class="tc-disabled" aria-label="Disabled profile">🚫 Disabled</span>` : null}${profiles && item.aliases?.length ? html`<span class="tc-aliases">${profileAliasesLabel(item)}</span>` : null}<span class="tc-descr">${profiles ? profileSummary(item) : roles ? roleSummary(item) : sandboxProfileSummary(item)}</span><span class="tc-actions"><button class="tool" onClick=${() => profiles ? actions.openProfileEditor(item) : roles ? actions.openRoleEditor(item) : actions.openSandboxEditor(item)}>edit</button>${kind === 'sandbox' && html`<button class="tool sandbox-profile-clone" onClick=${() => actions.openSandboxClone(item)}>clone</button>`}<button class="tool" onClick=${() => profiles ? actions.removeProfile(item.name) : roles ? actions.removeRole(item.name) : actions.removeSandbox(item.name)}>delete</button></span></div>${profiles && item.disabled && html`<div class="tc-sub tc-disabled-reason">${item.disabled_reason}</div>`}${roles && item.descr && html`<div class="tc-sub">${item.descr}</div>`}${kind === 'sandbox' && html`<div class="sbx-caps">${assignedBreakGlass(item.name, all, 'profile').map((entry) => { const via = entry.origins.filter((origin) => origin !== `profile:${item.name}`).map((origin) => origin.slice('profile:'.length)); return html`<div key=${`bg:${entry.access}:${entry.path}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-bg" title=${BREAK_GLASS_WARNING}>🚨 break-glass ${entry.access}</span><span class="sbx-cap-val" title=${`${entry.path} — protected tclaude/harness state (${entry.origins.join(', ')})`}>${entry.path}${via.length ? ` — via ${via.join(', ')}` : ''}</span></div>`; })}${(item.filesystem || []).map((entry) => html`<div key=${`${entry.access}:${entry.path}`} class="sbx-cap"><span class=${`sbx-cap-tag sbx-cap-${entry.access}`}>${entry.access}</span><span class="sbx-cap-val" title=${entry.path}>${entry.path}</span></div>`)}${(item.includes || []).map((name) => html`<div key=${`inc:${name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-inc">include</span><span class="sbx-cap-val" title=${name}>${name}</span></div>`)}${(item.environment || []).map((entry) => { const binding = `${entry.name} → ${entry.value}`; return html`<div key=${`env:${entry.name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-env">env</span><span class="sbx-cap-val" title=${binding}>${binding}</span></div>`; })}${(item.agent_directories || []).map((name) => html`<div key=${`own:${name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-own">own</span><span class="sbx-cap-val" title=${`${name} — isolated per agent`}>${name}</span></div>`)}</div>`}</div>`) : html`<div class="template-empty">${all.length ? wizWord('No items match the filter.', 'No items match the filter.') : profiles ? wizWord('No spawn profiles yet', 'No familiar patterns yet') : roles ? wizWord('No roles yet', 'No classes yet') : wizWord('No sandbox profiles yet', 'No wards yet')}</div>`}</${RequestList}></div>
    <div class="modal-buttons"><span class="spacer"></span><button onClick=${state.closeManager}>Close</button></div>
  </${Overlay}>`;
}

function Select({ value, onChange, options, ...props }) { return html`<select ...${props} value=${value} onChange=${(event) => onChange(event.currentTarget.value)}>${options.map(([key, label]) => html`<option key=${key} value=${key}>${label}</option>`)}</select>`; }
function Row({ label, hidden = false, title = '', children }) { return html`<label class="cron-create-row" hidden=${hidden} title=${title}><span class="cron-create-label">${label}</span>${children}</label>`; }

function HarnessFields({ draft, setDraft, catalog, profile = false }) {
  const hEntry = harnessByName(catalog, draft.harness);
  const models = hEntry?.models || [];
  const hasModelList = models.length > 0;
  const [customModel, setCustomModel] = useState(() => hasModelList && !!draft.model && !models.includes(draft.model));
  const updateHarness = (harness) => {
    const h = harnessByName(catalog, harness);
    const defaults = harnessDefaults(h);
    setCustomModel(false);
    setDraft((current) => ({ ...current, harness, model: '', effort: '', ...defaults, trust_dir: '', remote_control: '', auto_memory: '' }));
  };
  const [helpOpen, setHelpOpen] = useState('');
  const modelID = profile ? 'profile-editor-model' : 'role-editor-model';
  const approvalID = profile ? 'profile-editor-approval' : 'role-editor-approval';
  const sandboxID = profile ? 'profile-editor-sandbox' : 'role-editor-sandbox';
  const approvalLabel = draft.harness === 'codex' ? 'Approval policy' : 'Permission mode';
  const approvalHelp = hEntry?.approval_mode_help?.[draft.approval] || '';
  const sandboxHelp = hEntry?.sandbox_mode_help?.[draft.sandbox] || '';
  const askTimeoutHelp = hEntry?.ask_timeout_mode_help?.[draft.ask_user_question_timeout] || '';
  const reviewerHelp = approvalReviewerHelp(draft.approval_reviewer, draft.approval);
  const modelControl = hasModelList ? html`<div class="cron-create-target"><${Select} id=${modelID} value=${customModel ? '__custom__' : draft.model} onChange=${(value) => { if (value === '__custom__') { setCustomModel(true); change(setDraft, 'model', ''); } else { setCustomModel(false); change(setDraft, 'model', value); } }} options=${[['', 'Default (unset)'], ...models.map((model) => [model, model]), ['__custom__', 'Custom model id…']]} />${customModel && html`<input id=${`${modelID}-custom`} type="text" aria-label="Custom model id" value=${draft.model} onInput=${(event) => change(setDraft, 'model', event.currentTarget.value)} placeholder="model id or alias" autocomplete="off" spellcheck="false" autofocus />`}</div>` : html`<input id=${modelID} type="text" aria-label="Model id" value=${draft.model} onInput=${(event) => change(setDraft, 'model', event.currentTarget.value)} placeholder="blank = unset; model id or alias" autocomplete="off" spellcheck="false"/>`;
  return html`
    <${Row} label="Harness"><${Select} id=${profile ? 'profile-editor-harness' : 'role-editor-harness'} value=${draft.harness} onChange=${updateHarness} options=${catalog.map((entry) => [entry.name, entry.display_name || entry.name])} /></${Row}>
    <${Row} label="Model" title="Model suggested by the selected harness. Blank leaves it unset; Custom model id accepts an out-of-catalog model supported by that harness.">${modelControl}</${Row}>
    <${Row} label="Effort"><${Select} value=${draft.effort} onChange=${(value) => change(setDraft, 'effort', value)} options=${[['', "Default (harness's own)"], ...(hEntry?.effort_levels || ['low', 'medium', 'high', 'xhigh', 'max']).map((value) => [value, value])]} /></${Row}>
    <${HelpField} id=${sandboxID} label="Sandbox" title="Launch containment for the agent. The modes are per-harness."
      value=${draft.sandbox}
      options=${(hEntry?.sandbox_modes || []).map((value) => ({ value, label: value + (value === hEntry.default_sandbox ? ' (recommended)' : '') }))}
      onChange=${(event) => change(setDraft, 'sandbox', event.currentTarget.value)}
      help=${sandboxHelp} open=${helpOpen === sandboxID} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_sandbox} />
    <${HelpField} id=${approvalID} label=${approvalLabel} title="Controls when the harness requests approval; it does not change the sandbox."
      value=${draft.approval}
      options=${(hEntry?.approval_modes || []).map((value) => ({ value, label: approvalPolicyLabel(draft.harness, value, hEntry.default_approval) }))}
      onChange=${(event) => change(setDraft, 'approval', event.currentTarget.value)}
      help=${approvalHelp} open=${helpOpen === approvalID} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_approval} />
    ${profile && html`<${HelpField} id="profile-editor-approval-reviewer" label="Approval reviewer" title="Controls who decides eligible approval requests; it does not change the approval policy or sandbox."
      value=${draft.approval_reviewer} options=${approvalReviewerOptions(true)}
      onChange=${(event) => change(setDraft, 'approval_reviewer', event.currentTarget.value)}
      help=${reviewerHelp} open=${helpOpen === 'profile-editor-approval-reviewer'} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_auto_review} />`}
    ${profile && html`<${HelpField} id="profile-editor-ask-timeout" label="Question timeout" title="AskUserQuestion idle-timeout for the agent."
      value=${draft.ask_user_question_timeout}
      options=${(hEntry?.ask_timeout_modes || []).map((value) => ({ value, label: value + (value === hEntry.default_ask_timeout ? ' (recommended)' : '') }))}
      onChange=${(event) => change(setDraft, 'ask_user_question_timeout', event.currentTarget.value)}
      help=${askTimeoutHelp} open=${helpOpen === 'profile-editor-ask-timeout'} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_ask_timeout} />`}
  `;
}

function ProfileEditor({ descriptor, state, actions, confirmDiscard, openProfilePermissions }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const { seed, options = {}, catalog = [] } = descriptor;
  const baseline = useMemo(() => profileDraft(seed, options, catalog), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline));
  const dirty = dirtyDraft(draft, baseline); const local = !!options.local;
  const submit = async () => {
    state.error.value = '';
    if (!local && !draft.name.trim()) { state.error.value = 'profile name is required'; return; }
    if (!local && draft.disabled && !draft.disabled_reason.trim()) { state.error.value = 'a reason is required when disabling a profile'; return; }
    await actions.saveProfile({ draft, original: options.editExisting === false ? null : seed, options, payload: profilePayload(draft, seed, catalog, { local }) });
  };
  const saving = state.busy.value === 'profile-save';
  const hEntry = harnessByName(catalog, draft.harness);
  return html`<${Overlay} id="profile-editor-modal" labelledby="profile-editor-title" onClose=${state.closeDialog} onSubmitHotkey=${saving ? null : submit} dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="profile-editor-title">${local ? wizWord('Custom launch — this agent only', 'Bespoke summons — this familiar only') : seed && options.editExisting !== false ? wizWord(`Edit profile: ${seed.name}`, `Edit pattern: ${seed.name}`) : wizWord('New spawn profile', 'New familiar pattern')}</h3>
    <${Row} label="Name" hidden=${local}><input id="profile-editor-name" value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} placeholder="profile name — kebab-or-snake-case label" autofocus autocomplete="off" spellcheck="false" /></${Row}>
    <${Row} label="Aliases" hidden=${local} title="Alternate handles for this same profile. Separate multiple aliases with commas."><input id="profile-editor-aliases" value=${draft.aliases_text} onInput=${(event) => change(setDraft, 'aliases_text', event.currentTarget.value)} placeholder="e.g. codex-reviewer, cold-reviewer" autocomplete="off" spellcheck="false" /></${Row}>
    <${Row} label="Disabled" hidden=${local} title="Keep this profile visible and editable, but block every spawn that would use it."><input id="profile-editor-disabled" type="checkbox" checked=${draft.disabled} onChange=${(event) => change(setDraft, 'disabled', event.currentTarget.checked)} /></${Row}>
    <${Row} label="Disable reason" hidden=${local} title="Required while disabled. Retained when enabled so it can be reviewed or reused later."><textarea id="profile-editor-disabled-reason" value=${draft.disabled_reason} onInput=${(event) => change(setDraft, 'disabled_reason', event.currentTarget.value)} rows="2" placeholder="required when disabled — retained after re-enabling" spellcheck="true" /></${Row}>
    <${HarnessFields} draft=${draft} setDraft=${setDraft} catalog=${catalog} profile />
    <${Row} label="Trust dir" hidden=${draft.harness !== 'codex'}><${Select} id="profile-editor-trust-dir" value=${draft.trust_dir} onChange=${(value) => change(setDraft, 'trust_dir', value)} options=${TRI_OPTIONS}/></${Row}>
    <${Row} label="Remote control" hidden=${hEntry && !hEntry.can_remote_control}><${Select} id="profile-editor-remote-control" value=${draft.remote_control} onChange=${(value) => change(setDraft, 'remote_control', value)} options=${TRI_OPTIONS}/></${Row}>
    <${Row} label="Auto memory" hidden=${hEntry && !hEntry.can_auto_memory} title="Claude Code's built-in auto memory. tclaude disables it by default: agents sharing a repo all read one per-project memory store and cross-pollute each other's notes. Does not affect CLAUDE.md."><${Select} id="profile-editor-auto-memory" value=${draft.auto_memory} onChange=${(value) => change(setDraft, 'auto_memory', value)} options=${AUTO_MEMORY_TRI_OPTIONS}/></${Row}>
    ${[['Agent name', 'agent_name', 'optional — names the spawned agent'], ['Role', 'role', 'optional — e.g. researcher, planner'], ['Descr', 'descr', 'optional — short one-line description']].map(([label, key, placeholder]) => html`<${Row} key=${key} label=${label} hidden=${local}><input value=${draft[key]} onInput=${(event) => change(setDraft, key, event.currentTarget.value)} placeholder=${placeholder} autocomplete="off" spellcheck="false"/></${Row}>`)}
    <${Row} label="Initial msg" hidden=${local}><textarea value=${draft.initial_message} onInput=${(event) => change(setDraft, 'initial_message', event.currentTarget.value)} rows="3" placeholder="optional — task brief pre-filled into the spawn dialog" spellcheck="false" /></${Row}>
    ${[['Sync worktree', 'sync_worktree'], ['Auto focus', 'auto_focus'], ['Group context', 'include_group_default_context'], ['Group owner', 'is_owner']].map(([label, key]) => html`<${Row} key=${key} label=${label} hidden=${local && key !== 'is_owner'}><${Select} id=${key === 'is_owner' ? 'profile-editor-owner' : `profile-editor-${key.replaceAll('_', '-')}`} value=${draft[key]} onChange=${(value) => change(setDraft, key, value)} options=${TRI_OPTIONS}/></${Row}>`)}
    <div class="cron-create-row"><span class="cron-create-label">Permissions</span><button id="profile-editor-perms" class="tool" type="button" onClick=${() => openProfilePermissions({ overrides: draft.permission_overrides, ownsGroup: readTri(draft.is_owner) === true, label: draft.agent_name.trim(), onSave: (kept) => change(setDraft, 'permission_overrides', kept) })}>Permissions…</button><span>${Object.keys(draft.permission_overrides).length || ''}</span></div>
    <div class="cron-create-error" role="alert">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button id="profile-editor-submit" class="primary" disabled=${saving} onClick=${submit}>${saving ? 'Saving…' : local ? 'Apply' : 'Save profile'}</button></div>
  </${Overlay}>`;
}

function RoleEditor({ descriptor, current, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const { seed, catalog = [], slugs = [] } = descriptor;
  const baseline = useMemo(() => roleDraft(seed, catalog), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline)); const dirty = dirtyDraft(draft, baseline);
  const saving = state.busy.value === 'role-save';
  const choices = profileChoices(current.profiles); const selectedProfile = findProfileByHandle(current.profiles, draft.spawn_profile); if (draft.spawn_profile && !selectedProfile) choices.push({ value: draft.spawn_profile, label: `${draft.spawn_profile} (missing)` });
  const toggle = (slug) => setDraft((value) => ({ ...value, permissions: value.permissions.includes(slug) ? value.permissions.filter((item) => item !== slug) : [...value.permissions, slug] }));
  const submit = async () => { state.error.value = ''; if (!draft.name.trim()) { state.error.value = 'role name is required'; return; } await actions.saveRole({ draft, original: seed, payload: rolePayload(draft, catalog) }); };
  return html`<${Overlay} id="role-editor-modal" labelledby="role-editor-title" onClose=${state.closeDialog} dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="role-editor-title">${seed ? `Edit role: ${seed.name}` : 'New role'}</h3>
    <${Row} label="Name"><input id="role-editor-name" value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} placeholder="role name — kebab-or-snake-case label (e.g. reviewer)" autofocus autocomplete="off" spellcheck="false" /></${Row}><${Row} label="Descr"><input id="role-editor-descr" value=${draft.descr} onInput=${(event) => change(setDraft, 'descr', event.currentTarget.value)} placeholder="optional — short one-line description" autocomplete="off" spellcheck="false" /></${Row}><${Row} label="Brief"><textarea id="role-editor-brief" rows="5" value=${draft.brief} onInput=${(event) => change(setDraft, 'brief', event.currentTarget.value)} placeholder="canonical role-brief — prepended to a referencing agent's startup context (newlines OK)" spellcheck="false" /></${Row}>
    <${HarnessFields} draft=${draft} setDraft=${setDraft} catalog=${catalog}/><${Row} label="Spawn profile"><${Select} value=${draft.spawn_profile} onChange=${(value) => change(setDraft, 'spawn_profile', value)} options=${[['', '(none)'], ...choices.map((choice) => [choice.value, choice.label])]} /></${Row}>
    <div class="cron-create-row"><span class="cron-create-label">Permissions (${draft.permissions.length})</span><div class="ta-perms-list">${slugs.map((slug) => html`<label key=${slug.slug} title=${slug.description || ''}><input type="checkbox" checked=${draft.permissions.includes(slug.slug)} onChange=${() => toggle(slug.slug)} /> ${slug.slug}</label>`)}</div></div>
    <div class="cron-create-error" role="alert">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button id="role-editor-submit" class="primary" disabled=${saving} onClick=${submit}>${saving ? 'Saving…' : 'Save role'}</button></div>
  </${Overlay}>`;
}

function SandboxEditor({ descriptor, current, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const seed = descriptor.seed || null; const options = descriptor.options || {};
  const baseline = useMemo(() => ({ name: seed?.name || '', filesystem: clone(seed?.filesystem || []), environment: clone(seed?.environment || []), includes: clone(seed?.includes || []), agent_directories: clone(seed?.agent_directories || []), network_access: seed?.network_access || '', break_glass_filesystem: clone(breakGlassRules(seed)) }), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline)); const [advanced, setAdvanced] = useState(false); const [rawFS, setRawFS] = useState(() => JSON.stringify(baseline.filesystem, null, 2)); const [rawEnv, setRawEnv] = useState(() => JSON.stringify(baseline.environment, null, 2)); const [rawIncludes, setRawIncludes] = useState(() => JSON.stringify(baseline.includes, null, 2)); const [rawAgentDirs, setRawAgentDirs] = useState(() => JSON.stringify(baseline.agent_directories, null, 2)); const [rawBreakGlass, setRawBreakGlass] = useState(() => JSON.stringify(baseline.break_glass_filesystem, null, 2));
  // The audited common-rule presets. They are pure row inserters: nothing from
  // the catalog is persisted, so a profile never depends on it being loaded.
  const [commonRules, setCommonRules] = useState({ version: 0, categories: [], informational: [] });
  // The menu is long and most profiles never touch it, so it ships folded.
  const [commonRulesOpen, setCommonRulesOpen] = useState(false);
  // What the last insertion did, including the entry's warning — the operator
  // must see the consequence of the rows that just appeared in the table.
  const [commonRuleNotice, setCommonRuleNotice] = useState(null);
  // The feed is optional and its failures are the menu's own business. They
  // must never reach `state.error`, which carries save, validation and
  // break-glass refusals: a late rejection would replace the reason a save was
  // refused with an explanation of a convenience the operator did not ask for.
  const [commonRuleFeedError, setCommonRuleFeedError] = useState('');
  const [commonRuleFeedBusy, setCommonRuleFeedBusy] = useState(false);
  const commonRuleGeneration = useRef(0);
  // Retry stays live even while a load is in flight: a request that never
  // settles would otherwise leave the only way back permanently disabled. A
  // second attempt simply supersedes the first by generation.
  const loadCommonRules = () => {
    if (typeof actions.loadCommonRuleCatalog !== 'function') return;
    const generation = ++commonRuleGeneration.current;
    setCommonRuleFeedBusy(true);
    // Resolve.then rather than a bare call: a feed that throws synchronously
    // must land in the catch like any other failure, or the busy flag sticks.
    Promise.resolve().then(() => actions.loadCommonRuleCatalog()).then((value) => {
      if (generation !== commonRuleGeneration.current) return;
      setCommonRules(value || { version: 0, categories: [], informational: [] });
      setCommonRuleFeedError(''); setCommonRuleFeedBusy(false);
    }).catch((error) => {
      if (generation !== commonRuleGeneration.current) return;
      setCommonRuleFeedError(message(error)); setCommonRuleFeedBusy(false);
    });
  };
  // Unmount bumps the generation so an in-flight load resolves into nothing.
  useEffect(() => { loadCommonRules(); return () => { commonRuleGeneration.current++; }; }, []);
  // The acknowledgement is deliberately NOT part of the draft: it never
  // persists, and every editor session must collect it afresh.
  const [breakGlassAck, setBreakGlassAck] = useState(false);
  // After a daemon-side acknowledgement refusal whose registry reload
  // FAILED, the editor cannot see the rules it would be acknowledging, so
  // saving stays blocked until an authoritative reload succeeds.
  const [recoveryBlocked, setRecoveryBlocked] = useState(false);
  const [recoveryBusy, setRecoveryBusy] = useState(false);
  const retryRecovery = async () => {
    setRecoveryBusy(true);
    state.error.value = '';
    try {
      if ((await actions.load('sandbox')) === true) {
        setRecoveryBlocked(false);
        state.error.value = 'Registry reloaded — review the current break-glass rules above and re-acknowledge before saving.';
      } else {
        state.error.value = 'Registry reload failed again — saving stays blocked until an authoritative reload succeeds.';
      }
    } catch (error) {
      state.error.value = error.message || String(error);
    } finally {
      setRecoveryBusy(false);
    }
  };
  const [directoryStatus, setDirectoryStatus] = useState({ missing: [], creatable: [] }); const [directoryBusy, setDirectoryBusy] = useState(false);
  const directoryGeneration = useRef(0); const submitRef = useRef(null); const wasSaving = useRef(false); const filesystemSignature = JSON.stringify(draft.filesystem); const latestFilesystem = useRef(filesystemSignature); latestFilesystem.current = filesystemSignature;
  const dirty = dirtyDraft(draft, baseline);
  const saving = state.busy.value === 'sandbox-save';
  const setFS = (index, patch) => setDraft((value) => ({ ...value, filesystem: value.filesystem.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  const setEnv = (index, patch) => setDraft((value) => ({ ...value, environment: value.environment.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  const parseRaw = () => { const filesystem = JSON.parse(rawFS || '[]'); const environment = JSON.parse(rawEnv || '[]'); const includes = JSON.parse(rawIncludes || '[]'); const agent_directories = JSON.parse(rawAgentDirs || '[]'); const break_glass_filesystem = JSON.parse(rawBreakGlass || '[]'); if (![filesystem, environment, includes, agent_directories, break_glass_filesystem].every(Array.isArray)) throw new Error('filesystem, environment, includes, agent dirs and break-glass rules must be arrays'); return { filesystem, environment, includes, agent_directories, break_glass_filesystem }; };
  const applyRaw = () => { try { const parsed = parseRaw(); setDraft((value) => ({ ...value, ...parsed })); state.error.value = ''; return true; } catch (error) { state.error.value = error.message || String(error); return false; } };
  const toggleAdvanced = () => { if (advanced && !applyRaw()) return; if (!advanced) { setRawFS(JSON.stringify(draft.filesystem, null, 2)); setRawEnv(JSON.stringify(draft.environment, null, 2)); setRawIncludes(JSON.stringify(draft.includes, null, 2)); setRawAgentDirs(JSON.stringify(draft.agent_directories, null, 2)); setRawBreakGlass(JSON.stringify(draft.break_glass_filesystem, null, 2)); } setAdvanced(!advanced); };
  const submit = async () => {
    let value = draft;
    if (advanced) { try { value = { ...draft, ...parseRaw() }; } catch (error) { state.error.value = error.message || String(error); return; } }
    if (resolvedBreakGlass(value, current.sandboxProfiles, seed?.name || '').length && !breakGlassAck) { state.error.value = 'break-glass rules (including ones carried by includes) require the explicit risk acknowledgement below before saving'; return; }
    const outcome = await actions.saveSandbox({ draft: value, original: seed, options, breakGlassAcknowledged: breakGlassAck });
    // The daemon refused the commit because break-glass authority appeared
    // or changed after the preview. The stale acknowledgement must not carry
    // over — and if the registry reload failed, the editor cannot even show
    // the rules a fresh acknowledgement would cover, so saving stays blocked
    // until an authoritative reload succeeds.
    if (outcome && typeof outcome === 'object' && outcome.breakGlassAckRequired) {
      setBreakGlassAck(false);
      setRecoveryBlocked(outcome.recovered !== true);
    }
  };
  useEffect(() => {
    if (wasSaving.current && !saving) queueMicrotask(() => {
      const button = submitRef.current;
      if (button?.isConnected && !button.disabled && !button.closest('[inert]')) button.focus();
    });
    wasSaving.current = saving;
  }, [saving]);
  useEffect(() => { if (advanced) return undefined; let active = true; const generation = ++directoryGeneration.current; const filesystem = clone(draft.filesystem); const timer = setTimeout(async () => { try { const result = await actions.inspectDirectories(filesystem); if (active && generation === directoryGeneration.current) setDirectoryStatus({ missing: result?.missing || [], creatable: result?.creatable || [] }); } catch (_) { if (active && generation === directoryGeneration.current) setDirectoryStatus({ missing: [], creatable: [] }); } }, 300); return () => { active = false; clearTimeout(timer); }; }, [advanced, filesystemSignature]);
  const createMissing = async () => { const filesystem = clone(draft.filesystem); const signature = JSON.stringify(filesystem); const generation = ++directoryGeneration.current; setDirectoryBusy(true); state.error.value = ''; try { const result = await actions.createDirectories(filesystem); const refreshed = await actions.inspectDirectories(filesystem); if (generation === directoryGeneration.current && signature === latestFilesystem.current) { const created = result?.created || []; state.error.value = `Created ${created.length} sandbox director${created.length === 1 ? 'y' : 'ies'}.`; setDirectoryStatus({ missing: refreshed?.missing || [], creatable: refreshed?.creatable || [] }); } } catch (error) { if (generation === directoryGeneration.current) state.error.value = error.message || String(error); } finally { setDirectoryBusy(false); } };
  const configureWithAgent = () => { let value = draft; if (advanced) { try { value = { ...draft, ...parseRaw() }; } catch (error) { state.error.value = error.message || String(error); return; } } const editExisting = options.editExisting ?? !!seed; const targetName = editExisting ? options.targetName || seed?.name || '' : ''; state.closeDialog(); void actions.configureSandboxWithAgent(value, { targetName, editExisting, cloneSourceName: options.cloneSourceName, onCreate: options.onCreate }); };
  const rawDirty = advanced && [rawFS !== JSON.stringify(draft.filesystem, null, 2), rawEnv !== JSON.stringify(draft.environment, null, 2), rawIncludes !== JSON.stringify(draft.includes, null, 2), rawAgentDirs !== JSON.stringify(draft.agent_directories, null, 2), rawBreakGlass !== JSON.stringify(draft.break_glass_filesystem, null, 2)].some(Boolean);
  const setBG = (index, patch) => setDraft((value) => ({ ...value, break_glass_filesystem: value.break_glass_filesystem.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  // A preset inserts ordinary deny rows and then forgets it ever existed: no
  // stored ID, no hidden state. Paths already present in the table are left
  // exactly as authored rather than silently re-denied, and the notice says so.
  // The running set also absorbs an entry whose own paths alias each other,
  // which no audited entry does today — if one ever did, the notice's skip
  // count would need to distinguish that from "already in the table".
  const addCommonRule = (entry) => {
    const paths = commonRulePaths(entry);
    const existing = new Set(draft.filesystem.map((row) => pathIdentity(row.path)).filter(Boolean));
    const added = [];
    for (const path of paths) {
      const identity = pathIdentity(path);
      if (!identity || existing.has(identity)) continue;
      existing.add(identity);
      added.push(path);
    }
    if (added.length) setDraft((value) => ({ ...value, filesystem: [...value.filesystem, ...added.map((path) => ({ path, access: 'deny' }))] }));
    setCommonRuleNotice({ label: entry.label || entry.id, added, skipped: paths.length - added.length, warning: entry.warning || '' });
  };
  // The warning and acknowledgement must track the profile that would be
  // saved: the raw JSON when advanced mode is authoritative (falling back to
  // the structured rows while it is unparseable), and break-glass carried by
  // INCLUDES resolved against the current registry — a wrapper whose include
  // contributes a rule is exactly as dangerous as one carrying it directly.
  const candidate = (() => {
    if (!advanced) return draft;
    try { return { ...draft, ...parseRaw() }; } catch (_) { return draft; }
  })();
  const draftBreakGlass = candidate.break_glass_filesystem || [];
  const resolvedBG = resolvedBreakGlass(candidate, current.sandboxProfiles, seed?.name || '');
  // Same guard as the Save button, so the hotkey can never reach a save the
  // mouse path refuses. The overlay already suppresses its hotkey while
  // `blocked` (saving || directoryBusy); recoveryBlocked is the term that adds
  // to it — a failed break-glass registry reload must refuse the keyboard too.
  const submitBlocked = saving || directoryBusy || recoveryBlocked;
  return html`<${Overlay} id="sandbox-profile-editor-modal" labelledby="sandbox-profile-editor-title" onClose=${state.closeDialog} onSubmitHotkey=${submitBlocked ? null : submit} dirty=${dirty || rawDirty} blocked=${saving || directoryBusy} confirmDiscard=${confirmDiscard} registerClose=${registerClose} resizeKey="tclaude.dash.modalSize.sandbox-profile-editor"><h3 id="sandbox-profile-editor-title">${options.cloneSourceName ? wizWord(`Clone sandbox profile: ${options.cloneSourceName}`, `Mirror ward: ${options.cloneSourceName}`) : seed ? wizWord(`Edit sandbox profile: ${seed.name}`, `Edit ward: ${seed.name}`) : wizWord('New sandbox profile', 'New ward')}</h3><p class="modal-meta">Directory grants widen the sandbox; environment values are injected at launch. Agent-owned directories create a fresh writable cache directory for each spawned agent and set the named environment variable to its path. Network policies control external IP connectivity while retaining the tclaude agent socket. Managed Codex profiles block the host tmux server independently. Environment values are ordinary configuration, not secrets.</p><${Row} label="Name"><input value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} placeholder="e.g. shared-build-caches" autofocus autocomplete="off" spellcheck="false"/></${Row}><${Row} label="Network"><${Select} id="sandbox-profile-editor-network" value=${draft.network_access} onChange=${(value) => change(setDraft, 'network_access', value)} options=${[['', 'No override (inherit profile layers)'], ['internet', 'Internet access'], ['none', 'Offline (macOS; unavailable on Linux/WSL)']]}/></${Row}>
    <fieldset class="sbx-section" hidden=${advanced}><legend>Filesystem</legend><div class="sbx-rows">${draft.filesystem.map((row, index) => html`<div key=${index} class="sbx-row"><${Select} class="sbx-access" value=${row.access || 'read'} onChange=${(access) => setFS(index, { access })} options=${[['read', 'read'], ['write', 'write'], ['deny', 'deny']]}/><input class="sbx-path" value=${row.path || ''} onInput=${(event) => setFS(index, { path: event.currentTarget.value })}/><button type="button" onClick=${async () => { const result = await pickDirectory({ startDir: row.path || '', title: 'Select a sandbox directory' }); if (result.path) setFS(index, { path: result.path }); else if (result.error) state.error.value = result.error; }}>Browse…</button><button type="button" onClick=${() => setDraft((value) => ({ ...value, filesystem: value.filesystem.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row" onClick=${() => setDraft((value) => ({ ...value, filesystem: [...value.filesystem, { path: '', access: 'read' }] }))}>＋ add directory</button>
      ${/* `|| null` rather than a bare boolean: where `open` is not a settable
           DOM property, Preact falls back to setAttribute, and setting it to
           `false` still leaves the attribute present (i.e. open). null removes
           it on both paths. */ ''}
      <details class="sbx-common-rules" id="sandbox-profile-editor-common-rules" open=${commonRulesOpen || null}
        onToggle=${(event) => setCommonRulesOpen(event.currentTarget.open)}>
        ${/* The menu ships folded, and nothing inside a closed <details> is in
             the accessibility tree — so a feed failure has to be legible on the
             summary itself or an operator never learns the presets are gone. */ ''}
        <summary class="sbx-common-rule-summary">＋ add common rule${commonRuleFeedError ? ' — unavailable' : ''}</summary>
        <div class="sbx-common-rule-intro">Audited presets for locations most profiles want denied. Each one inserts ordinary deny rows into the table above — visible, editable, and yours to adjust or remove afterwards. Nothing else is stored.</div>
        ${commonRuleFeedError && html`<div id="sandbox-profile-editor-common-rule-feed-error" class="sbx-common-rule-feed-error" role="alert">Could not load the common-rule catalog: ${commonRuleFeedError} <button type="button" onClick=${loadCommonRules}>${commonRuleFeedBusy ? 'retrying…' : 'retry'}</button></div>`}
        <div class="sbx-common-rule-list">${(commonRules.categories || []).map((entry) => html`<${CommonRuleEntry} key=${entry.id} entry=${entry} onAdd=${addCommonRule}/>`)}</div>
        ${(commonRules.informational || []).length > 0 && html`<details class="sbx-common-rule-informational"><summary>Required, non-removable access</summary>${(commonRules.informational || []).map((entry) => html`<div key=${entry.id} class="sbx-bg-intro"><strong>${entry.label}:</strong> ${entry.description}</div>`)}</details>`}
      </details>
      ${commonRuleNotice && html`<div id="sandbox-profile-editor-common-rule-notice" class="sbx-common-rule-notice" role="status">
        <span>${commonRuleNotice.added.length ? `Added ${commonRuleNotice.added.length} deny row${commonRuleNotice.added.length === 1 ? '' : 's'} from “${commonRuleNotice.label}”: ${commonRuleNotice.added.join(' · ')}.` : `“${commonRuleNotice.label}” added no rows.`}${commonRuleNotice.skipped ? ` ${commonRuleNotice.skipped} path${commonRuleNotice.skipped === 1 ? ' was' : 's were'} already in the table and left as authored.` : ''}</span>
        ${commonRuleNotice.warning ? html`<span class="sbx-common-rule-warn">⚠ ${commonRuleNotice.warning}</span>` : null}
        <button type="button" class="sbx-common-rule-dismiss" aria-label="Dismiss common-rule notice" onClick=${() => setCommonRuleNotice(null)}>×</button>
      </div>`}
    </fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend>Environment</legend><div class="sbx-rows">${draft.environment.map((row, index) => html`<div key=${index} class="sbx-row"><input value=${row.name || ''} placeholder="NAME" onInput=${(event) => setEnv(index, { name: event.currentTarget.value })}/><input value=${row.value || ''} placeholder="value" onInput=${(event) => setEnv(index, { value: event.currentTarget.value })}/><button type="button" onClick=${() => setDraft((value) => ({ ...value, environment: value.environment.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row" onClick=${() => setDraft((value) => ({ ...value, environment: [...value.environment, { name: '', value: '' }] }))}>＋ add variable</button></fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend title="Included profiles apply first, in order; this profile overrides them.">Includes</legend><div class="sbx-rows">${draft.includes.map((name, index) => html`<div key=${index} class="sbx-row"><${Select} class="sbx-inc-name" value=${name} onChange=${(value) => setDraft((old) => ({ ...old, includes: old.includes.map((item, i) => i === index ? value : item) }))} options=${[['', '— choose profile —'], ...current.sandboxProfiles.filter((item) => item.name !== seed?.name || item.name === name).map((item) => [item.name, item.name])]} /><button type="button" onClick=${() => setDraft((old) => ({ ...old, includes: old.includes.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row sbx-include-add" onClick=${() => setDraft((old) => ({ ...old, includes: [...old.includes, ''] }))}>＋ include profile</button></fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend title="Environment-variable names backed by isolated writable directories created per agent.">Agent-owned directories</legend><div class="sbx-rows">${draft.agent_directories.map((name, index) => html`<div key=${index} class="sbx-row"><input class="sbx-agent-name" value=${name} placeholder="GOCACHE" onInput=${(event) => setDraft((old) => ({ ...old, agent_directories: old.agent_directories.map((item, i) => i === index ? event.currentTarget.value : item) }))}/><button type="button" onClick=${() => setDraft((old) => ({ ...old, agent_directories: old.agent_directories.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row sbx-agent-add" onClick=${() => setDraft((old) => ({ ...old, agent_directories: [...old.agent_directories, ''] }))}>＋ add agent-owned directory</button></fieldset>
    <fieldset class="sbx-section sbx-break-glass" hidden=${advanced}><legend title="Exact-path read/write access to normally protected tclaude/harness state. An exception mechanism for debugging tclaude itself — not a recommended posture.">🚨 Break-glass protected access</legend>
      ${resolvedBG.length ? html`<div class="sbx-bg-warning" role="alert"><strong>🚨 Dangerous:</strong> This profile grants break-glass protected access: ${describeBreakGlassEntries(resolvedBG)}. ${BREAK_GLASS_WARNING}</div>` : html`<div class="sbx-bg-intro">Grants access to normally protected tclaude/harness state (daemon database, sessions, credentials). Exceptional debugging only — leave empty unless you are deliberately debugging tclaude itself.</div>`}
      <div class="sbx-rows">${draftBreakGlass.map((row, index) => html`<div key=${index} class="sbx-row"><${Select} class="sbx-access" value=${row.access || 'read'} onChange=${(access) => setBG(index, { access })} options=${[['read', 'read'], ['write', 'write']]}/><input class="sbx-path" value=${row.path || ''} placeholder="~/.tclaude/data" onInput=${(event) => setBG(index, { path: event.currentTarget.value })}/><button type="button" onClick=${() => setDraft((value) => ({ ...value, break_glass_filesystem: value.break_glass_filesystem.filter((_, i) => i !== index) }))}>×</button></div>`)}</div>
      <button type="button" class="sbx-add-row sbx-bg-add" onClick=${() => setDraft((value) => ({ ...value, break_glass_filesystem: [...value.break_glass_filesystem, { path: '', access: 'read' }] }))}>＋ add break-glass rule (dangerous)</button>
    </fieldset>
    ${resolvedBG.length > 0 && html`<label class="sbx-bg-ack"><input type="checkbox" id="sandbox-profile-editor-break-glass-ack" checked=${breakGlassAck} onChange=${(event) => setBreakGlassAck(event.currentTarget.checked)}/> I understand this profile grants break-glass access to protected tclaude/harness state — including possible credential and session disclosure, state corruption, authorization bypass, host-control risk, and daemon/harness breakage — and I accept that risk.</label>`}
    ${!advanced && directoryStatus.missing.length > 0 && html`<div class="sbx-missing"><span>${directoryStatus.missing.length} director${directoryStatus.missing.length === 1 ? 'y does' : 'ies do'} not exist. Saving is allowed; read/write rules activate on a later launch, while deny targets must exist before launch.</span>${directoryStatus.creatable.length > 0 && html`<button type="button" disabled=${directoryBusy || saving} onClick=${createMissing}>${directoryBusy ? 'Creating…' : `Create ${directoryStatus.creatable.length} missing director${directoryStatus.creatable.length === 1 ? 'y' : 'ies'}`}</button>`}</div>`}
    <button type="button" class="sbx-advanced-toggle" aria-expanded=${advanced} onClick=${toggleAdvanced}>${advanced ? '▾' : '▸'} Advanced — edit raw JSON</button>${advanced && html`<div class="sbx-advanced-body"><${Row} label="Filesystem JSON"><textarea id="sandbox-profile-editor-filesystem" rows="6" value=${rawFS} onInput=${(event) => setRawFS(event.currentTarget.value)}/></${Row}><${Row} label="Environment JSON"><textarea id="sandbox-profile-editor-environment" rows="6" value=${rawEnv} onInput=${(event) => setRawEnv(event.currentTarget.value)}/></${Row}><${Row} label="Includes JSON"><textarea id="sandbox-profile-editor-includes" rows="3" value=${rawIncludes} onInput=${(event) => setRawIncludes(event.currentTarget.value)}/></${Row}><${Row} label="Agent dirs JSON"><textarea id="sandbox-profile-editor-agent-directories" rows="3" value=${rawAgentDirs} onInput=${(event) => setRawAgentDirs(event.currentTarget.value)}/></${Row}><${Row} label="Break-glass JSON" title="Exact-path {path, access: read|write} rules for normally protected tclaude/harness state. Dangerous; requires the explicit acknowledgement to save."><textarea id="sandbox-profile-editor-break-glass" rows="3" value=${rawBreakGlass} onInput=${(event) => setRawBreakGlass(event.currentTarget.value)}/></${Row}></div>`}
    ${recoveryBlocked && html`<div id="sandbox-profile-editor-recovery" class="sbx-bg-warning" role="alert">The daemon refused this save because the profile now carries break-glass authority this editor cannot see: the registry reload failed, so the current rules are unknown. Saving stays blocked until an authoritative reload succeeds. <button type="button" id="sandbox-profile-editor-recovery-retry" disabled=${recoveryBusy || saving} onClick=${() => { void retryRecovery(); }}>${recoveryBusy ? 'Reloading…' : '↻ Retry registry reload'}</button></div>`}
    <div role="alert" class="cron-create-error">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving || directoryBusy} onClick=${() => { void requestClose(); }}>Cancel</button><button id="sandbox-profile-editor-scribe" disabled=${saving || directoryBusy} onClick=${configureWithAgent}>🤖 configure with agent</button><span class="spacer"></span><button ref=${submitRef} id="sandbox-profile-editor-submit" class="primary" disabled=${submitBlocked} onClick=${submit}>${saving ? 'Saving…' : 'Save sandbox profile'}</button></div></${Overlay}>`;
}

function ProfileExport({ current, state, actions, confirmDiscard }) {
  const [selected, setSelected] = useState(() => new Set(current.profiles.map((item) => item.name))); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const toggle = (name) => setSelected((old) => { const next = new Set(old); next.has(name) ? next.delete(name) : next.add(name); return next; });
  const submit = async () => { if (!selected.size) { setError('select at least one profile'); return; } setBusy(true); try { await actions.exportProfileBundle([...selected]); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(false); } };
  return html`<${Overlay} id="profile-export-modal" labelledby="profile-export-title" onClose=${state.closeDialog} confirmDiscard=${confirmDiscard}><h3 id="profile-export-title">Export spawn profiles</h3><div id="profile-export-list" class="profile-transfer-list">${current.profiles.map((item) => html`<label key=${item.name} class="profile-transfer-row"><input type="checkbox" checked=${selected.has(item.name)} onChange=${() => toggle(item.name)}/><span>${item.name} ${profileAliasesLabel(item)} ${profileSummary(item)}</span></label>`)}</div><div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy} onClick=${submit}>${busy ? 'Exporting…' : 'Export'}</button></div></${Overlay}>`;
}

function ProfileImportRow({ row, decision, update }) {
  const renameLabel = row.aliases?.length ? 'Rename copy (aliases omitted)' : 'Rename';
  return html`<div class="profile-transfer-row"><input type="checkbox" disabled=${!row.valid} checked=${decision?.include} onChange=${(event) => update({ include: event.currentTarget.checked })}/><span>${row.name}${row.error && ` — ${row.error}`}</span>${row.exists && row.valid && html`<span class="profile-import-conflict"><${Select} value=${decision?.action} onChange=${(value) => update({ action: value })} options=${[['rename', renameLabel], ['overwrite', 'Overwrite']]} />${decision?.action === 'rename' && html`<input value=${decision?.as} onInput=${(event) => update({ as: event.currentTarget.value })}/>`}</span>`}</div>`;
}

function ProfileImport({ state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [raw, setRaw] = useState(''); const [envelope, setEnvelope] = useState(null); const [preview, setPreview] = useState(null); const [decisions, setDecisions] = useState({}); const [error, setError] = useState(''); const [busy, setBusy] = useState('');
  const inspect = async () => { setError(''); setBusy('inspect'); try { const parsed = JSON.parse(raw); const found = await actions.inspectProfiles(parsed); setEnvelope(parsed); setPreview(found); const initial = {}; for (const row of found.profiles || []) initial[row.name] = { include: !!row.valid, action: row.exists ? 'rename' : 'create', as: row.default_name || `${row.name}-copy` }; setDecisions(initial); } catch (e) { setError(message(e)); } finally { setBusy(''); } };
  const update = (name, patch) => setDecisions((value) => ({ ...value, [name]: { ...value[name], ...patch } }));
  const submit = async () => { if (!preview) { setError('preview the import first'); return; } setBusy('import'); try { const rows = Object.entries(decisions).map(([name, value]) => ({ name, ...value })); await actions.importProfileBundle(envelope, rows); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(''); } };
  const dirty = !!raw;
  return html`<${Overlay} id="profile-import-modal" labelledby="profile-import-title" onClose=${state.closeDialog} dirty=${dirty} blocked=${!!busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="profile-import-title">Import spawn profiles</h3><${Row} label="File"><input type="file" accept=".json,application/json" onChange=${async (event) => { const file = event.currentTarget.files?.[0]; if (file) { setRaw(await file.text()); setPreview(null); } }}/></${Row}><${Row} label="or paste"><textarea rows="6" value=${raw} onInput=${(event) => { setRaw(event.currentTarget.value); setPreview(null); }} /></${Row}><button disabled=${busy} onClick=${inspect}>Preview</button>
    ${preview && html`<div id="profile-import-preview" class="profile-transfer-list">${(preview.profiles || []).map((row) => html`<${ProfileImportRow} key=${row.name} row=${row} decision=${decisions[row.name]} update=${(patch) => update(row.name, patch)} />`)}</div>`}
    <div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button disabled=${!!busy} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy || !preview} onClick=${submit}>${busy === 'import' ? 'Importing…' : 'Import selected'}</button></div></${Overlay}>`;
}

function SandboxExport({ current, state, actions, confirmDiscard }) {
  const [selected, setSelected] = useState(() => new Set(current.sandboxProfiles.map((item) => item.name))); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const toggle = (name) => setSelected((old) => { const next = new Set(old); next.has(name) ? next.delete(name) : next.add(name); return next; });
  const submit = async () => { if (!selected.size) { setError('select at least one sandbox profile'); return; } setBusy(true); try { await actions.exportSandboxBundle([...selected]); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(false); } };
  return html`<${Overlay} id="sandbox-profile-export-modal" labelledby="sandbox-profile-export-title" onClose=${state.closeDialog} confirmDiscard=${confirmDiscard}><h3 id="sandbox-profile-export-title"><span class="sandbox-word-regular">Export sandbox profiles</span><span class="sandbox-word-wizard">📜 Inscribe wards</span></h3><div class="profile-transfer-list">${current.sandboxProfiles.map((item) => html`<label key=${item.name} class="profile-transfer-row"><input type="checkbox" checked=${selected.has(item.name)} onChange=${() => toggle(item.name)}/><span>${item.name} ${sandboxProfileSummary(item)}</span></label>`)}</div><div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy} onClick=${submit}>${busy ? 'Exporting…' : 'Export'}</button></div></${Overlay}>`;
}

function SandboxImport({ current, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [raw, setRaw] = useState(''); const [envelope, setEnvelope] = useState(null); const [preview, setPreview] = useState(null); const [conflict, setConflict] = useState('skip'); const [error, setError] = useState(''); const [busy, setBusy] = useState('');
  const [bgAck, setBgAck] = useState(false);
  // Sticky after a typed 422 whose registry reload FAILED: the cached local
  // registry may hide server-side break-glass, so carriers composed from it
  // cannot be trusted. While set, EVERY preview attempt — including the
  // ordinary Preview button — must reload the registry before inspecting,
  // and only both succeeding restores the preview and lifts the block.
  // Bundle or policy edits do not clear it (the registry is still stale) —
  // and neither does closing and reopening this dialog: the marker lives in
  // the shared management state, whose stale cached registry it describes,
  // so only a successful authoritative reload clears it.
  const registryRecoveryRequired = state.sandboxRegistryRecoveryRequired;
  const inspect = async () => {
    setError(''); setBusy('inspect');
    try {
      const parsed = JSON.parse(raw);
      if (parsed?.format !== 'tclaude-sandbox-profiles' || ![1, 2, 3, 4].includes(parsed?.format_version)) throw new Error('not a tclaude sandbox-profile export');
      if (registryRecoveryRequired.value) {
        let registryOk = false;
        try { registryOk = (await actions.load('sandbox')) === true; } catch (_) { registryOk = false; }
        if (!registryOk) {
          setPreview(null);
          setError('reloading the sandbox-profile registry failed — the current break-glass rules are unknown, so preview and import stay blocked until an authoritative reload succeeds');
          return;
        }
      }
      const found = await actions.inspectSandboxBundle(parsed);
      setEnvelope(parsed); setPreview(found); setBgAck(false);
      registryRecoveryRequired.value = false;
    } catch (e) {
      if (registryRecoveryRequired.value) setPreview(null);
      setError(message(e));
    } finally { setBusy(''); }
  };
  const existing = new Set(current.sandboxProfiles.map((item) => item.name)); const incoming = preview?.profiles || envelope?.profiles || [];
  // Break-glass carried anywhere in an imported profile's composition — its
  // own rules or ones an included bundle/local profile contributes — needs a
  // fresh acknowledgement on this machine before import. The registry the
  // composition resolves against must honor the conflict policy: under
  // "skip", a conflicting incoming profile is discarded (its rules must not
  // demand acknowledgement) and includes keep resolving to the RETAINED
  // local version (whose break-glass must not go unwarned); under
  // "overwrite" (and "error", which only succeeds without conflicts) the
  // incoming versions win.
  const incomingNames = new Set(incoming.map((item) => item.name));
  const localNames = new Set(current.sandboxProfiles.map((item) => item.name));
  const imported = conflict === 'skip' ? incoming.filter((item) => !localNames.has(item.name)) : incoming;
  const composedProfiles = conflict === 'skip'
    ? [...current.sandboxProfiles, ...imported]
    : [...current.sandboxProfiles.filter((item) => !incomingNames.has(item.name)), ...incoming];
  const breakGlassCarriers = imported
    .map((item) => ({ name: item.name, entries: assignedBreakGlass(item.name, composedProfiles, 'import') }))
    .filter((item) => item.entries.length);
  const needsAck = breakGlassCarriers.length > 0;
  // The ack-free inspect reports include-graph errors PER conflict policy
  // ("skip" keeps a clashing local profile's own includes, so only one policy
  // may be invalid). Importing under "error" only succeeds when no names
  // clash — every incoming profile lands — so it shares the overwrite graph.
  const includeError = preview?.include_errors?.[conflict === 'skip' ? 'skip' : 'overwrite'] || '';
  const submit = async () => {
    if (!preview) { setError('preview the import first'); return; }
    if (includeError) { setError(includeError); return; }
    if (needsAck && !bgAck) { setError('break-glass profiles require the explicit risk acknowledgement before import'); return; }
    setBusy('import');
    try { await actions.importSandboxBundle(envelope, conflict, needsAck && bgAck); state.closeDialog(); }
    catch (e) {
      if (e?.code === BREAK_GLASS_ACK_CODE) {
        // The daemon's authoritative import plan demanded an acknowledgement
        // this preview did not anticipate (its state moved after inspect).
        // The carriers the operator must acknowledge are composed from BOTH
        // sides, so recovery refreshes BOTH: the local sandbox-profile
        // registry (a RETAINED local profile may have gained break-glass
        // that a skip-policy wrapper now reaches) and the authoritative
        // bundle inspection. Either refresh failing is a recovery failure:
        // the preview clears so Import stays blocked. Never resend
        // automatically — a fresh explicit acknowledgement is required.
        setBgAck(false);
        let failure = '';
        let refreshed = null;
        let registryOk = false;
        try { registryOk = (await actions.load('sandbox')) === true; } catch (_) { registryOk = false; }
        if (!registryOk) {
          failure = 'reloading the sandbox-profile registry failed';
          // Sticky: the cached registry cannot be trusted, so every later
          // preview attempt must reload it first (see inspect above).
          registryRecoveryRequired.value = true;
        } else {
          try { refreshed = await actions.inspectSandboxBundle(envelope); }
          catch (inspectError) { failure = `re-running the authoritative preview failed (${message(inspectError)})`; }
        }
        if (failure) {
          setPreview(null);
          setError(`${message(e)} ${failure} — import stays blocked; preview again once the daemon is reachable.`);
        } else {
          setPreview(refreshed);
          registryRecoveryRequired.value = false;
          setError(`${message(e)} The registry and authoritative preview were refreshed — review the current break-glass carriers and re-acknowledge before importing again.`);
        }
      } else setError(message(e));
    }
    finally { setBusy(''); }
  };
  return html`<${Overlay} id="sandbox-profile-import-modal" labelledby="sandbox-profile-import-title" onClose=${state.closeDialog} dirty=${!!raw} blocked=${!!busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="sandbox-profile-import-title"><span class="sandbox-word-regular">Import sandbox profiles</span><span class="sandbox-word-wizard">📜 Read wards</span></h3><${Row} label="File"><input type="file" accept=".json,application/json" onChange=${async (event) => { const file = event.currentTarget.files?.[0]; if (file) { setRaw(await file.text()); setPreview(null); setBgAck(false); } }}/></${Row}><${Row} label="or paste"><textarea rows="6" value=${raw} onInput=${(event) => { setRaw(event.currentTarget.value); setPreview(null); setBgAck(false); }}/></${Row}><button disabled=${busy} onClick=${inspect}>Preview</button>${preview && html`<div class="profile-transfer-list">${incoming.map((item) => html`<div key=${item.name} class="profile-transfer-row"><span>${item.name} · ${sandboxProfileSummary(item)}${existing.has(item.name) ? ' · already exists locally' : ''}</span></div>`)}</div>${needsAck && html`<div class="sbx-bg-warning" role="alert"><strong>🚨 Break-glass protected access in this bundle:</strong> ${breakGlassCarriers.map((carrier) => `${carrier.name}: ${describeBreakGlassEntries(carrier.entries)}`).join(' — ')}. ${BREAK_GLASS_WARNING} Importing on this machine requires a fresh acknowledgement after paths are canonicalized.</div><label class="sbx-bg-ack"><input type="checkbox" id="sandbox-profile-import-break-glass-ack" checked=${bgAck} onChange=${(event) => setBgAck(event.currentTarget.checked)}/> I understand these profiles grant break-glass access to protected tclaude/harness state and I accept that risk on this machine.</label>`}${incoming.some((item) => existing.has(item.name)) && html`<${Row} label="Name conflicts"><${Select} id="sandbox-profile-import-conflict" value=${conflict} onChange=${(value) => { setConflict(value); setBgAck(false); }} options=${[['skip', 'Skip existing'], ['overwrite', 'Overwrite existing'], ['error', 'Stop with an error']]}/></${Row}>`}${includeError && html`<div id="sandbox-profile-import-include-error" role="alert" class="cron-create-error">Include graph invalid under this conflict policy: ${includeError}</div>`}`}
    <div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button disabled=${!!busy} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy || !preview || !!includeError || (needsAck && !bgAck)} onClick=${submit}>${busy === 'import' ? 'Importing…' : 'Import'}</button></div></${Overlay}>`;
}

function SandboxDiffModal({ model, close, profiles = [] }) {
  const confirmRef = useRef(null);
  const { dialogRef } = useDialogFocus({ open: !!model, initialFocusRef: confirmRef, onEscape: () => close(false) });
  useEffect(() => {
    if (!model) return undefined;
    const editor = document.querySelector('#sandbox-profile-editor-modal');
    const editorDialog = editor?.querySelector('[role="dialog"]');
    if (!editor) return undefined;
    editor.inert = true;
    editor.setAttribute('aria-hidden', 'true');
    editorDialog?.setAttribute('aria-modal', 'false');
    return () => {
      editor.inert = false;
      editor.removeAttribute('aria-hidden');
      editorDialog?.setAttribute('aria-modal', 'true');
    };
  }, [model]);
  if (!model) return null;
  const beforeRaw = model.before ? JSON.stringify(model.before, null, 2) : '';
  const afterRaw = JSON.stringify(model.after, null, 2);
  const diff = model.before ? lineDiff(beforeRaw, afterRaw) : afterRaw.split('\n').map((s) => ({ t: 'add', s }));
  const adds = diff.filter((line) => line.t === 'add').length;
  const dels = diff.filter((line) => line.t === 'del').length;
  const sign = { add: '+', del: '\u2212', ctx: ' ' };
  const cancelOutside = (event) => { if (event.target === event.currentTarget) close(false); };
  // Resolve against the registry so break-glass carried purely by includes
  // stays visible in the confirmation, matching the daemon's acknowledgement
  // rule. Own rules are also covered by breakGlassRules on the raw payload.
  const afterBreakGlass = resolvedBreakGlass(model.after, profiles, model.before?.name || '');
  const beforeBreakGlass = model.before ? resolvedBreakGlass(model.before, profiles, model.before.name) : [];
  return html`<div ref=${dialogRef} id="sandbox-profile-diff-modal" class="modal-overlay show" role="dialog" aria-modal="true" aria-labelledby="sandbox-profile-diff-title" onClick=${cancelOutside}>
    <div class="config-diff-modal">
      <h3 id="sandbox-profile-diff-title">Confirm sandbox profile changes</h3>
      ${afterBreakGlass.length > 0 && html`<div id="sandbox-profile-diff-break-glass" class="sbx-bg-warning" role="alert"><strong>🚨 Break-glass protected access:</strong> ${describeBreakGlassEntries(afterBreakGlass)} — ${BREAK_GLASS_WARNING}</div>`}
      ${!afterBreakGlass.length && beforeBreakGlass.length > 0 && html`<div id="sandbox-profile-diff-break-glass-removed" class="cfg-diff-sub">Break-glass protected access is removed by this change.</div>`}
      <p id="sandbox-profile-diff-sub" class="cfg-diff-sub">${model.before ? `${adds} line(s) added, ${dels} removed — server-normalized preview` : `${adds} line(s) added — new server-normalized profile`}</p>
      <div id="sandbox-profile-diff-body" class="config-diff">${diff.map((line, index) => html`<span key=${index} class=${`dl ${line.t}`}>${sign[line.t]} ${line.s}</span>`)}</div>
      <div class="modal-buttons"><button id="sandbox-profile-diff-cancel" type="button" onClick=${() => close(false)}>Cancel</button><span class="spacer"></span><button ref=${confirmRef} id="sandbox-profile-diff-confirm" class="primary" type="button" onClick=${() => close(true)}>Save sandbox profile</button></div>
    </div>
  </div>`;
}

function ManagementApp({ state, actions, confirm, confirmDiscard, openProfilePermissions }) {
  const current = state.view.value; const descriptor = current.dialog;
  return html`${current.templateManager && html`<${TemplateManager} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${current.templateDialog?.kind === 'template-editor' && html`<${TemplateEditor} descriptor=${current.templateDialog} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} confirm=${confirm}/>`}
    ${current.manager && html`<${Manager} kind=${current.manager} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'profile-editor' && html`<${ProfileEditor} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions}/>`}
    ${descriptor?.kind === 'role-editor' && html`<${RoleEditor} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'profile-export' && html`<${ProfileExport} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'profile-import' && html`<${ProfileImport} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'sandbox-editor' && html`<${SandboxEditor} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'sandbox-export' && html`<${SandboxExport} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'sandbox-import' && html`<${SandboxImport} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'template-duplicate' && html`<${TemplateDuplicateDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'template-import' && html`<${TemplateImportDialog} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'template-from-group' && html`<${TemplateFromGroupDialog} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'template-starters' && html`<${TemplateStartersDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'group-import' && html`<${GroupImportDialog} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'group-context' && html`<${GroupContextDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'group-clone' && html`<${GroupCloneDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'template-deploy' && html`<${TemplateDeployDialog} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    <${SandboxDiffModal} model=${current.sandboxDiff} close=${state.cancelSandboxDiff} profiles=${current.sandboxProfiles} />`;
}

export function mountManagementIsland({ host, state, actions, confirm, confirmDiscard, openProfilePermissions, registerCleanup }) {
  const controller = {
    openProfilesManageModal: () => actions.openManager('profiles'), openProfileEditor: actions.openProfileEditor, removeProfile: actions.removeProfile,
    openRolesManageModal: () => actions.openManager('roles'), openRoleEditor: actions.openRoleEditor, removeRole: actions.removeRole,
    openSandboxProfilesManageModal: () => actions.openManager('sandbox'), openSandboxProfileEditor: actions.openSandboxEditor, removeSandboxProfile: actions.removeSandbox,
    openTemplatesManageModal: actions.openTemplateManager, openTemplateEditor: actions.openTemplateEditor,
    updateTemplates: actions.updateTemplates, removeTemplate: actions.removeTemplate,
    exportTemplate: actions.exportTemplate,
    openTemplateDuplicate: actions.openTemplateDuplicate, openTemplateFromGroup: actions.openTemplateFromGroup,
    openTemplateImport: actions.openTemplateImport, openTemplateStarters: actions.openTemplateStarters,
    openTemplateDeploy: actions.openTemplateDeploy,
    openGroupImport: actions.openGroupImport, openGroupContext: actions.openGroupContext, openGroupClone: actions.openGroupClone,
  };
  const unregister = registerManagementController(controller);
  render(html`<${ManagementApp} state=${state} actions=${actions} confirm=${confirm} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions}/>` , host);
  registerCleanup(() => { state.cancelSandboxDiff(false); unregister(); render(null, host); });
}
