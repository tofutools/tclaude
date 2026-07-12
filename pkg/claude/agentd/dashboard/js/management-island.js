import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { profileSummary } from './profiles.js';
import { roleSummary } from './roles.js';
import { dirtyDraft, harnessByName, profileDraft, profilePayload, readTri, roleDraft, rolePayload, TRI_OPTIONS } from './management-model.js';
import { registerManagementController } from './management-controller.js';
import { sandboxProfileSummary } from './sandbox-profiles-data.js';
import { makeModalResizable, pickDirectory } from './helpers.js';
import { wizWord } from './slop.js';

const html = htm.bind(h);

function message(error) { return error?.message || String(error); }
function clone(value) { return JSON.parse(JSON.stringify(value)); }
function change(setDraft, key, value) { setDraft((draft) => ({ ...draft, [key]: value })); }

function Overlay({ id, manage = false, labelledby, onClose, dirty = false, blocked = false, confirmDiscard, resizeKey = '', children }) {
  const overlayRef = useRef(null);
  const dialogRef = useRef(null);
  const close = async () => { if (blocked) return; if (!dirty || await confirmDiscard()) onClose(); };
  useEffect(() => {
    const key = (event) => {
      if (event.key !== 'Escape') return;
      const overlays = document.querySelectorAll('.manage-overlay.show, .modal-overlay.show');
      if (overlays[overlays.length - 1] !== overlayRef.current) return;
      event.preventDefault();
      void close();
    };
    document.addEventListener('keydown', key); return () => document.removeEventListener('keydown', key);
  }, [dirty, blocked]);
  useEffect(() => resizeKey ? makeModalResizable(dialogRef.current, resizeKey) : undefined, [resizeKey]);
  return html`<div ref=${overlayRef} class=${manage ? 'manage-overlay show' : 'modal-overlay show'} id=${id} onMouseDown=${(event) => { if (event.target === event.currentTarget) void close(); }}><div ref=${dialogRef} class=${manage ? 'manage-modal' : 'cron-create-modal template-editor-modal'} role="dialog" aria-modal="true" aria-labelledby=${labelledby}>${children}</div></div>`;
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
  const list = all.filter((item) => !q || [item.name, item.descr, item.role, item.model, item.harness, item.agent_name].some((value) => String(value || '').toLowerCase().includes(q)));
  const title = profiles ? html`<span class="profiles-word-regular">Spawn profiles</span><span class="profiles-word-wizard">Familiar patterns</span>` : roles ? html`<span class="roles-word-regular">Role library</span><span class="roles-word-wizard">Class library</span>` : html`<span class="sandbox-word-regular">Sandbox profiles</span><span class="sandbox-word-wizard">Wards</span>`;
  return html`<${Overlay} id=${`${domKind}-manage-modal`} manage labelledby=${`${domKind}-manage-title`} onClose=${state.closeManager} confirmDiscard=${confirmDiscard}>
    <h3 id=${`${domKind}-manage-title`}>${title}</h3>
    <p class="manage-intro">${profiles ? "Reusable bundles of the spawn dialog's launch and identity fields." : roles ? 'Named reusable role briefs, launch defaults, and permissions.' : 'Filesystem and environment policy applied when an agent launches.'}</p>
    <div class="filter-bar"><input id=${`filter-${kind}`} value=${filter} onInput=${(event) => { setFilter.value = event.currentTarget.value; }} placeholder="Filter" autocomplete="off" spellcheck="false" autofocus /><span class="filter-count" id=${`filter-${kind}-count`}>${q ? `${list.length} / ${all.length}` : all.length}</span><button class="clear-filter" onClick=${() => { setFilter.value = ''; }}>×</button><span class="spacer"></span>
      ${profiles && html`<button id="profile-export-open" class="tool" onClick=${() => state.openDialog({ kind: 'profile-export' })}>⇪ export</button><button id="profile-import-open" class="tool" onClick=${() => state.openDialog({ kind: 'profile-import' })}>⤒ import</button>`}
      ${kind === 'sandbox' && html`<button id="sandbox-profile-export-open" class="tool" onClick=${() => state.openDialog({ kind: 'sandbox-export' })}>⇪ export</button><button id="sandbox-profile-import-open" class="tool" onClick=${() => state.openDialog({ kind: 'sandbox-import' })}>⤒ import</button><button id="sandbox-profile-scribe-open" class="tool" onClick=${() => actions.configureSandboxWithAgent({ name: '', filesystem: [], environment: [] })}>🤖 configure with agent</button>`}
      <button id=${profiles ? 'profile-create-open' : roles ? 'role-create-open' : 'sandbox-profile-create-open'} class="primary" onClick=${() => profiles ? actions.openProfileEditor() : roles ? actions.openRoleEditor() : actions.openSandboxEditor()}>${profiles ? html`<span class="profiles-word-regular">+ new profile</span><span class="profiles-word-wizard">+ new pattern</span>` : roles ? html`<span class="roles-word-regular">+ new role</span><span class="roles-word-wizard">+ new class</span>` : html`<span class="sandbox-word-regular">+ new sandbox profile</span><span class="sandbox-word-wizard">+ new ward</span>`}</button>
    </div>
    <div id=${profiles ? 'profiles-list' : roles ? 'roles-list' : 'sandbox-profiles-list'}><${RequestList} request=${request} label=${kind} retry=${() => actions.load(kind)}>${list.length ? list.map((item) => html`<div key=${item.name} class=${`template-card ${profiles ? 'profile' : roles ? 'role' : 'sandbox-profile'}-card`} data-key=${item.name}><div class="tc-head"><span class="tc-name">${item.name}</span><span class="tc-descr">${profiles ? profileSummary(item) : roles ? roleSummary(item) : sandboxProfileSummary(item)}</span><span class="tc-actions"><button class="tool" onClick=${() => profiles ? actions.openProfileEditor(item) : roles ? actions.openRoleEditor(item) : actions.openSandboxEditor(item)}>edit</button><button class="tool" onClick=${() => profiles ? actions.removeProfile(item.name) : roles ? actions.removeRole(item.name) : actions.removeSandbox(item.name)}>delete</button></span></div>${roles && item.descr && html`<div class="tc-sub">${item.descr}</div>`}${kind === 'sandbox' && html`<div class="sbx-caps">${(item.filesystem || []).map((entry) => html`<div key=${`${entry.access}:${entry.path}`} class="sbx-cap"><span class=${`sbx-cap-tag sbx-cap-${entry.access}`}>${entry.access}</span><span class="sbx-cap-val" title=${entry.path}>${entry.path}</span></div>`)}${(item.includes || []).map((name) => html`<div key=${`inc:${name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-inc">include</span><span class="sbx-cap-val" title=${name}>${name}</span></div>`)}${(item.environment || []).map((entry) => html`<div key=${`env:${entry.name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-env">env</span><span class="sbx-cap-val" title=${entry.name}>${entry.name}</span></div>`)}${(item.agent_directories || []).map((name) => html`<div key=${`own:${name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-own">own</span><span class="sbx-cap-val" title=${`${name} — isolated per agent`}>${name}</span></div>`)}</div>`}</div>`) : html`<div class="template-empty">${all.length ? wizWord('No items match the filter.', 'No items match the filter.') : profiles ? wizWord('No spawn profiles yet', 'No familiar patterns yet') : roles ? wizWord('No roles yet', 'No classes yet') : wizWord('No sandbox profiles yet', 'No wards yet')}</div>`}</${RequestList}></div>
    <div class="modal-buttons"><span class="spacer"></span><button onClick=${state.closeManager}>Close</button></div>
  </${Overlay}>`;
}

function Select({ value, onChange, options, ...props }) { return html`<select ...${props} value=${value} onChange=${(event) => onChange(event.currentTarget.value)}>${options.map(([key, label]) => html`<option key=${key} value=${key}>${label}</option>`)}</select>`; }
function Row({ label, hidden = false, title = '', children }) { return html`<label class="cron-create-row" hidden=${hidden} title=${title}><span class="cron-create-label">${label}</span>${children}</label>`; }

function HarnessFields({ draft, setDraft, catalog, profile = false }) {
  const hEntry = harnessByName(catalog, draft.harness);
  const models = hEntry?.models || [];
  const updateHarness = (harness) => {
    const h = harnessByName(catalog, harness);
    setDraft((current) => ({ ...current, harness, model: '', effort: '', sandbox: h?.default_sandbox || '', approval: h?.default_approval || '', ask_user_question_timeout: h?.default_ask_timeout || '', trust_dir: '', remote_control: '' }));
  };
  return html`
    <${Row} label="Harness"><${Select} id=${profile ? 'profile-editor-harness' : 'role-editor-harness'} value=${draft.harness} onChange=${updateHarness} options=${catalog.map((entry) => [entry.name, entry.display_name || entry.name])} /></${Row}>
    <${Row} label="Model"><input id=${profile ? 'profile-editor-model' : 'role-editor-model'} aria-label="Model id" value=${draft.model} list=${`${profile ? 'profile' : 'role'}-models`} onInput=${(event) => change(setDraft, 'model', event.currentTarget.value)} placeholder="Default (unset)" autocomplete="off" spellcheck="false"/><datalist id=${`${profile ? 'profile' : 'role'}-models`}>${models.map((model) => html`<option value=${model} />`)}</datalist></${Row}>
    <${Row} label="Effort"><${Select} value=${draft.effort} onChange=${(value) => change(setDraft, 'effort', value)} options=${[['', "Default (harness's own)"], ...(hEntry?.effort_levels || ['low', 'medium', 'high', 'xhigh', 'max']).map((value) => [value, value])]} /></${Row}>
    <${Row} label="Sandbox" hidden=${!hEntry?.can_sandbox}><${Select} value=${draft.sandbox} onChange=${(value) => change(setDraft, 'sandbox', value)} options=${(hEntry?.sandbox_modes || []).map((value) => [value, value + (value === hEntry.default_sandbox ? ' (recommended)' : '')])} /></${Row}>
    <${Row} label="Permission mode" hidden=${!hEntry?.can_approval}><${Select} value=${draft.approval} onChange=${(value) => change(setDraft, 'approval', value)} options=${(hEntry?.approval_modes || []).map((value) => [value, value + (value === hEntry.default_approval ? ' (recommended)' : '')])} /></${Row}>
    ${profile && html`<${Row} label="Question timeout" hidden=${!hEntry?.can_ask_timeout}><${Select} id="profile-editor-ask-timeout" value=${draft.ask_user_question_timeout} onChange=${(value) => change(setDraft, 'ask_user_question_timeout', value)} options=${(hEntry?.ask_timeout_modes || []).map((value) => [value, value])} /></${Row}>`}
  `;
}

function ProfileEditor({ descriptor, state, actions, confirmDiscard, openProfilePermissions }) {
  const { seed, options = {}, catalog = [] } = descriptor;
  const baseline = useMemo(() => profileDraft(seed, options, catalog), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline));
  const dirty = dirtyDraft(draft, baseline); const local = !!options.local;
  const submit = async () => {
    state.error.value = '';
    if (!local && !draft.name.trim()) { state.error.value = 'profile name is required'; return; }
    await actions.saveProfile({ draft, original: options.editExisting === false ? null : seed, options, payload: profilePayload(draft, seed, catalog, { local }) });
  };
  const saving = state.busy.value === 'profile-save';
  const close = async () => { if (saving) return; if (!dirty || await confirmDiscard()) state.closeDialog(); };
  const hEntry = harnessByName(catalog, draft.harness);
  return html`<${Overlay} id="profile-editor-modal" labelledby="profile-editor-title" onClose=${state.closeDialog} dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard}><h3 id="profile-editor-title">${local ? wizWord('Custom launch — this agent only', 'Bespoke summons — this familiar only') : seed && options.editExisting !== false ? wizWord(`Edit profile: ${seed.name}`, `Edit pattern: ${seed.name}`) : wizWord('New spawn profile', 'New familiar pattern')}</h3>
    <${Row} label="Name" hidden=${local}><input id="profile-editor-name" value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} autofocus autocomplete="off" spellcheck="false" /></${Row}>
    <${HarnessFields} draft=${draft} setDraft=${setDraft} catalog=${catalog} profile />
    <${Row} label="Trust dir" hidden=${draft.harness !== 'codex'}><${Select} id="profile-editor-trust-dir" value=${draft.trust_dir} onChange=${(value) => change(setDraft, 'trust_dir', value)} options=${TRI_OPTIONS}/></${Row}>
    <${Row} label="Remote control" hidden=${hEntry && !hEntry.can_remote_control}><${Select} id="profile-editor-remote-control" value=${draft.remote_control} onChange=${(value) => change(setDraft, 'remote_control', value)} options=${TRI_OPTIONS}/></${Row}>
    ${[['Agent name', 'agent_name'], ['Role', 'role'], ['Descr', 'descr']].map(([label, key]) => html`<${Row} key=${key} label=${label} hidden=${local}><input value=${draft[key]} onInput=${(event) => change(setDraft, key, event.currentTarget.value)} autocomplete="off" spellcheck="false"/></${Row}>`)}
    <${Row} label="Initial msg" hidden=${local}><textarea value=${draft.initial_message} onInput=${(event) => change(setDraft, 'initial_message', event.currentTarget.value)} rows="3" /></${Row}>
    ${[['Sync worktree', 'sync_worktree'], ['Auto focus', 'auto_focus'], ['Group context', 'include_group_default_context'], ['Group owner', 'is_owner']].map(([label, key]) => html`<${Row} key=${key} label=${label} hidden=${local && key !== 'is_owner'}><${Select} id=${key === 'is_owner' ? 'profile-editor-owner' : `profile-editor-${key.replaceAll('_', '-')}`} value=${draft[key]} onChange=${(value) => change(setDraft, key, value)} options=${TRI_OPTIONS}/></${Row}>`)}
    <div class="cron-create-row"><span class="cron-create-label">Permissions</span><button id="profile-editor-perms" type="button" onClick=${() => openProfilePermissions({ overrides: draft.permission_overrides, ownsGroup: readTri(draft.is_owner) === true, label: draft.agent_name.trim(), onSave: (kept) => change(setDraft, 'permission_overrides', kept) })}>Permissions…</button><span>${Object.keys(draft.permission_overrides).length || ''}</span></div>
    <div class="cron-create-error" role="alert">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving} onClick=${close}>Cancel</button><span class="spacer"></span><button id="profile-editor-submit" class="primary" disabled=${saving} onClick=${submit}>${saving ? 'Saving…' : local ? 'Apply' : 'Save profile'}</button></div>
  </${Overlay}>`;
}

function RoleEditor({ descriptor, current, state, actions, confirmDiscard }) {
  const { seed, catalog = [], slugs = [] } = descriptor;
  const baseline = useMemo(() => roleDraft(seed, catalog), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline)); const dirty = dirtyDraft(draft, baseline);
  const saving = state.busy.value === 'role-save';
  const profiles = current.profiles.map((item) => item.name); if (draft.spawn_profile && !profiles.includes(draft.spawn_profile)) profiles.push(draft.spawn_profile);
  const toggle = (slug) => setDraft((value) => ({ ...value, permissions: value.permissions.includes(slug) ? value.permissions.filter((item) => item !== slug) : [...value.permissions, slug] }));
  const submit = async () => { state.error.value = ''; if (!draft.name.trim()) { state.error.value = 'role name is required'; return; } await actions.saveRole({ draft, original: seed, payload: rolePayload(draft, catalog) }); };
  return html`<${Overlay} id="role-editor-modal" labelledby="role-editor-title" onClose=${state.closeDialog} dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard}><h3 id="role-editor-title">${seed ? `Edit role: ${seed.name}` : 'New role'}</h3>
    <${Row} label="Name"><input value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} autofocus /></${Row}><${Row} label="Descr"><input value=${draft.descr} onInput=${(event) => change(setDraft, 'descr', event.currentTarget.value)} /></${Row}><${Row} label="Brief"><textarea id="role-editor-brief" rows="5" value=${draft.brief} onInput=${(event) => change(setDraft, 'brief', event.currentTarget.value)} /></${Row}>
    <${HarnessFields} draft=${draft} setDraft=${setDraft} catalog=${catalog}/><${Row} label="Spawn profile"><${Select} value=${draft.spawn_profile} onChange=${(value) => change(setDraft, 'spawn_profile', value)} options=${[['', '(none)'], ...profiles.map((name) => [name, name + (!current.profiles.some((item) => item.name === name) ? ' (missing)' : '')])]} /></${Row}>
    <div class="cron-create-row"><span class="cron-create-label">Permissions (${draft.permissions.length})</span><div class="ta-perms-list">${slugs.map((slug) => html`<label key=${slug.slug} title=${slug.description || ''}><input type="checkbox" checked=${draft.permissions.includes(slug.slug)} onChange=${() => toggle(slug.slug)} /> ${slug.slug}</label>`)}</div></div>
    <div class="cron-create-error" role="alert">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving} onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${saving} onClick=${submit}>${saving ? 'Saving…' : 'Save role'}</button></div>
  </${Overlay}>`;
}

function SandboxEditor({ descriptor, current, state, actions, confirmDiscard }) {
  const seed = descriptor.seed || null; const options = descriptor.options || {};
  const baseline = useMemo(() => ({ name: seed?.name || '', filesystem: clone(seed?.filesystem || []), environment: clone(seed?.environment || []), includes: clone(seed?.includes || []), agent_directories: clone(seed?.agent_directories || []) }), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline)); const [advanced, setAdvanced] = useState(false); const [rawFS, setRawFS] = useState(() => JSON.stringify(baseline.filesystem, null, 2)); const [rawEnv, setRawEnv] = useState(() => JSON.stringify(baseline.environment, null, 2)); const [rawIncludes, setRawIncludes] = useState(() => JSON.stringify(baseline.includes, null, 2)); const [rawAgentDirs, setRawAgentDirs] = useState(() => JSON.stringify(baseline.agent_directories, null, 2));
  const [directoryStatus, setDirectoryStatus] = useState({ missing: [], creatable: [] }); const [directoryBusy, setDirectoryBusy] = useState(false);
  const directoryGeneration = useRef(0); const filesystemSignature = JSON.stringify(draft.filesystem); const latestFilesystem = useRef(filesystemSignature); latestFilesystem.current = filesystemSignature;
  const dirty = dirtyDraft(draft, baseline);
  const saving = state.busy.value === 'sandbox-save';
  const setFS = (index, patch) => setDraft((value) => ({ ...value, filesystem: value.filesystem.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  const setEnv = (index, patch) => setDraft((value) => ({ ...value, environment: value.environment.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  const parseRaw = () => { const filesystem = JSON.parse(rawFS || '[]'); const environment = JSON.parse(rawEnv || '[]'); const includes = JSON.parse(rawIncludes || '[]'); const agent_directories = JSON.parse(rawAgentDirs || '[]'); if (![filesystem, environment, includes, agent_directories].every(Array.isArray)) throw new Error('filesystem, environment, includes and agent dirs must be arrays'); return { filesystem, environment, includes, agent_directories }; };
  const applyRaw = () => { try { const parsed = parseRaw(); setDraft((value) => ({ ...value, ...parsed })); state.error.value = ''; return true; } catch (error) { state.error.value = error.message || String(error); return false; } };
  const toggleAdvanced = () => { if (advanced && !applyRaw()) return; if (!advanced) { setRawFS(JSON.stringify(draft.filesystem, null, 2)); setRawEnv(JSON.stringify(draft.environment, null, 2)); setRawIncludes(JSON.stringify(draft.includes, null, 2)); setRawAgentDirs(JSON.stringify(draft.agent_directories, null, 2)); } setAdvanced(!advanced); };
  const submit = async () => { let value = draft; if (advanced) { try { value = { ...draft, ...parseRaw() }; } catch (error) { state.error.value = error.message || String(error); return; } } await actions.saveSandbox({ draft: value, original: seed, options }); };
  useEffect(() => { if (advanced) return undefined; let active = true; const generation = ++directoryGeneration.current; const filesystem = clone(draft.filesystem); const timer = setTimeout(async () => { try { const result = await actions.inspectDirectories(filesystem); if (active && generation === directoryGeneration.current) setDirectoryStatus({ missing: result?.missing || [], creatable: result?.creatable || [] }); } catch (_) { if (active && generation === directoryGeneration.current) setDirectoryStatus({ missing: [], creatable: [] }); } }, 300); return () => { active = false; clearTimeout(timer); }; }, [advanced, filesystemSignature]);
  const createMissing = async () => { const filesystem = clone(draft.filesystem); const signature = JSON.stringify(filesystem); const generation = ++directoryGeneration.current; setDirectoryBusy(true); state.error.value = ''; try { const result = await actions.createDirectories(filesystem); const refreshed = await actions.inspectDirectories(filesystem); if (generation === directoryGeneration.current && signature === latestFilesystem.current) { const created = result?.created || []; state.error.value = `Created ${created.length} sandbox director${created.length === 1 ? 'y' : 'ies'}.`; setDirectoryStatus({ missing: refreshed?.missing || [], creatable: refreshed?.creatable || [] }); } } catch (error) { if (generation === directoryGeneration.current) state.error.value = error.message || String(error); } finally { setDirectoryBusy(false); } };
  const configureWithAgent = () => { let value = draft; if (advanced) { try { value = { ...draft, ...parseRaw() }; } catch (error) { state.error.value = error.message || String(error); return; } } state.closeDialog(); void actions.configureSandboxWithAgent(value, { targetName: options.targetName || seed?.name || '', onCreate: options.onCreate }); };
  const rawDirty = advanced && [rawFS !== JSON.stringify(draft.filesystem, null, 2), rawEnv !== JSON.stringify(draft.environment, null, 2), rawIncludes !== JSON.stringify(draft.includes, null, 2), rawAgentDirs !== JSON.stringify(draft.agent_directories, null, 2)].some(Boolean);
  return html`<${Overlay} id="sandbox-profile-editor-modal" labelledby="sandbox-profile-editor-title" onClose=${state.closeDialog} dirty=${dirty || rawDirty} blocked=${saving || directoryBusy} confirmDiscard=${confirmDiscard} resizeKey="tclaude.dash.modalSize.sandbox-profile-editor"><h3 id="sandbox-profile-editor-title">${seed ? wizWord(`Edit sandbox profile: ${seed.name}`, `Edit ward: ${seed.name}`) : wizWord('New sandbox profile', 'New ward')}</h3><p class="modal-meta">Directory grants widen the sandbox; environment values are injected at launch. Agent-owned directories create a fresh writable cache directory for each spawned agent and set the named environment variable to its path. Environment values are ordinary configuration, not secrets.</p><${Row} label="Name"><input value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} autofocus autocomplete="off" spellcheck="false"/></${Row}>
    <fieldset class="sbx-section" hidden=${advanced}><legend>Filesystem</legend><div class="sbx-rows">${draft.filesystem.map((row, index) => html`<div key=${index} class="sbx-row"><${Select} value=${row.access || 'read'} onChange=${(access) => setFS(index, { access })} options=${[['read', 'read'], ['write', 'write'], ['deny', 'deny']]}/><input class="sbx-path" value=${row.path || ''} onInput=${(event) => setFS(index, { path: event.currentTarget.value })}/><button type="button" onClick=${async () => { const result = await pickDirectory({ startDir: row.path || '', title: 'Select a sandbox directory' }); if (result.path) setFS(index, { path: result.path }); else if (result.error) state.error.value = result.error; }}>Browse…</button><button type="button" onClick=${() => setDraft((value) => ({ ...value, filesystem: value.filesystem.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row" onClick=${() => setDraft((value) => ({ ...value, filesystem: [...value.filesystem, { path: '', access: 'read' }] }))}>＋ add directory</button></fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend>Environment</legend><div class="sbx-rows">${draft.environment.map((row, index) => html`<div key=${index} class="sbx-row"><input value=${row.name || ''} placeholder="NAME" onInput=${(event) => setEnv(index, { name: event.currentTarget.value })}/><input value=${row.value || ''} placeholder="value" onInput=${(event) => setEnv(index, { value: event.currentTarget.value })}/><button type="button" onClick=${() => setDraft((value) => ({ ...value, environment: value.environment.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row" onClick=${() => setDraft((value) => ({ ...value, environment: [...value.environment, { name: '', value: '' }] }))}>＋ add variable</button></fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend title="Included profiles apply first, in order; this profile overrides them.">Includes</legend><div class="sbx-rows">${draft.includes.map((name, index) => html`<div key=${index} class="sbx-row"><${Select} value=${name} onChange=${(value) => setDraft((old) => ({ ...old, includes: old.includes.map((item, i) => i === index ? value : item) }))} options=${[['', '— choose profile —'], ...current.sandboxProfiles.filter((item) => item.name !== seed?.name || item.name === name).map((item) => [item.name, item.name])]} /><button type="button" onClick=${() => setDraft((old) => ({ ...old, includes: old.includes.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row sbx-include-add" onClick=${() => setDraft((old) => ({ ...old, includes: [...old.includes, ''] }))}>＋ include profile</button></fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend title="Environment-variable names backed by isolated writable directories created per agent.">Agent-owned directories</legend><div class="sbx-rows">${draft.agent_directories.map((name, index) => html`<div key=${index} class="sbx-row"><input class="sbx-agent-name" value=${name} placeholder="GOCACHE" onInput=${(event) => setDraft((old) => ({ ...old, agent_directories: old.agent_directories.map((item, i) => i === index ? event.currentTarget.value : item) }))}/><button type="button" onClick=${() => setDraft((old) => ({ ...old, agent_directories: old.agent_directories.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row sbx-agent-add" onClick=${() => setDraft((old) => ({ ...old, agent_directories: [...old.agent_directories, ''] }))}>＋ add agent-owned directory</button></fieldset>
    ${!advanced && directoryStatus.missing.length > 0 && html`<div class="sbx-missing"><span>${directoryStatus.missing.length} director${directoryStatus.missing.length === 1 ? 'y does' : 'ies do'} not exist. Saving is allowed; read/write rules activate on a later launch, while deny targets must exist before launch.</span>${directoryStatus.creatable.length > 0 && html`<button type="button" disabled=${directoryBusy || saving} onClick=${createMissing}>${directoryBusy ? 'Creating…' : `Create ${directoryStatus.creatable.length} missing director${directoryStatus.creatable.length === 1 ? 'y' : 'ies'}`}</button>`}</div>`}
    <button type="button" class="sbx-advanced-toggle" aria-expanded=${advanced} onClick=${toggleAdvanced}>${advanced ? '▾' : '▸'} Advanced — edit raw JSON</button>${advanced && html`<div class="sbx-advanced-body"><${Row} label="Filesystem JSON"><textarea id="sandbox-profile-editor-filesystem" rows="6" value=${rawFS} onInput=${(event) => setRawFS(event.currentTarget.value)}/></${Row}><${Row} label="Environment JSON"><textarea id="sandbox-profile-editor-environment" rows="6" value=${rawEnv} onInput=${(event) => setRawEnv(event.currentTarget.value)}/></${Row}><${Row} label="Includes JSON"><textarea id="sandbox-profile-editor-includes" rows="3" value=${rawIncludes} onInput=${(event) => setRawIncludes(event.currentTarget.value)}/></${Row}><${Row} label="Agent dirs JSON"><textarea id="sandbox-profile-editor-agent-directories" rows="3" value=${rawAgentDirs} onInput=${(event) => setRawAgentDirs(event.currentTarget.value)}/></${Row}></div>`}
    <div role="alert" class="cron-create-error">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving || directoryBusy} onClick=${state.closeDialog}>Cancel</button><button id="sandbox-profile-editor-scribe" disabled=${saving || directoryBusy} onClick=${configureWithAgent}>🤖 configure with agent</button><span class="spacer"></span><button class="primary" disabled=${saving || directoryBusy} onClick=${submit}>${saving ? 'Saving…' : 'Save sandbox profile'}</button></div></${Overlay}>`;
}

function ProfileExport({ current, state, actions, confirmDiscard }) {
  const [selected, setSelected] = useState(() => new Set(current.profiles.map((item) => item.name))); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const toggle = (name) => setSelected((old) => { const next = new Set(old); next.has(name) ? next.delete(name) : next.add(name); return next; });
  const submit = async () => { if (!selected.size) { setError('select at least one profile'); return; } setBusy(true); try { await actions.exportProfileBundle([...selected]); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(false); } };
  return html`<${Overlay} id="profile-export-modal" labelledby="profile-export-title" onClose=${state.closeDialog} confirmDiscard=${confirmDiscard}><h3 id="profile-export-title">Export spawn profiles</h3><div id="profile-export-list" class="profile-transfer-list">${current.profiles.map((item) => html`<label key=${item.name} class="profile-transfer-row"><input type="checkbox" checked=${selected.has(item.name)} onChange=${() => toggle(item.name)}/><span>${item.name} ${profileSummary(item)}</span></label>`)}</div><div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy} onClick=${submit}>${busy ? 'Exporting…' : 'Export'}</button></div></${Overlay}>`;
}

function ProfileImportRow({ row, decision, update }) {
  return html`<div class="profile-transfer-row"><input type="checkbox" disabled=${!row.valid} checked=${decision?.include} onChange=${(event) => update({ include: event.currentTarget.checked })}/><span>${row.name}${row.error && ` — ${row.error}`}</span>${row.exists && row.valid && html`<span class="profile-import-conflict"><${Select} value=${decision?.action} onChange=${(value) => update({ action: value })} options=${[['rename', 'Rename'], ['overwrite', 'Overwrite']]} />${decision?.action === 'rename' && html`<input value=${decision?.as} onInput=${(event) => update({ as: event.currentTarget.value })}/>`}</span>`}</div>`;
}

function ProfileImport({ state, actions, confirmDiscard }) {
  const [raw, setRaw] = useState(''); const [envelope, setEnvelope] = useState(null); const [preview, setPreview] = useState(null); const [decisions, setDecisions] = useState({}); const [error, setError] = useState(''); const [busy, setBusy] = useState('');
  const inspect = async () => { setError(''); setBusy('inspect'); try { const parsed = JSON.parse(raw); const found = await actions.inspectProfiles(parsed); setEnvelope(parsed); setPreview(found); const initial = {}; for (const row of found.profiles || []) initial[row.name] = { include: !!row.valid, action: row.exists ? 'rename' : 'create', as: row.default_name || `${row.name}-copy` }; setDecisions(initial); } catch (e) { setError(message(e)); } finally { setBusy(''); } };
  const update = (name, patch) => setDecisions((value) => ({ ...value, [name]: { ...value[name], ...patch } }));
  const submit = async () => { if (!preview) { setError('preview the import first'); return; } setBusy('import'); try { const rows = Object.entries(decisions).map(([name, value]) => ({ name, ...value })); await actions.importProfileBundle(envelope, rows); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(''); } };
  const dirty = !!raw;
  return html`<${Overlay} id="profile-import-modal" labelledby="profile-import-title" onClose=${state.closeDialog} dirty=${dirty} confirmDiscard=${confirmDiscard}><h3 id="profile-import-title">Import spawn profiles</h3><${Row} label="File"><input type="file" accept=".json,application/json" onChange=${async (event) => { const file = event.currentTarget.files?.[0]; if (file) { setRaw(await file.text()); setPreview(null); } }}/></${Row}><${Row} label="or paste"><textarea rows="6" value=${raw} onInput=${(event) => { setRaw(event.currentTarget.value); setPreview(null); }} /></${Row}><button disabled=${busy} onClick=${inspect}>Preview</button>
    ${preview && html`<div id="profile-import-preview" class="profile-transfer-list">${(preview.profiles || []).map((row) => html`<${ProfileImportRow} key=${row.name} row=${row} decision=${decisions[row.name]} update=${(patch) => update(row.name, patch)} />`)}</div>`}
    <div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy || !preview} onClick=${submit}>${busy === 'import' ? 'Importing…' : 'Import selected'}</button></div></${Overlay}>`;
}

function SandboxExport({ current, state, actions, confirmDiscard }) {
  const [selected, setSelected] = useState(() => new Set(current.sandboxProfiles.map((item) => item.name))); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const toggle = (name) => setSelected((old) => { const next = new Set(old); next.has(name) ? next.delete(name) : next.add(name); return next; });
  const submit = async () => { if (!selected.size) { setError('select at least one sandbox profile'); return; } setBusy(true); try { await actions.exportSandboxBundle([...selected]); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(false); } };
  return html`<${Overlay} id="sandbox-profile-export-modal" labelledby="sandbox-profile-export-title" onClose=${state.closeDialog} confirmDiscard=${confirmDiscard}><h3 id="sandbox-profile-export-title"><span class="sandbox-word-regular">Export sandbox profiles</span><span class="sandbox-word-wizard">📜 Inscribe wards</span></h3><div class="profile-transfer-list">${current.sandboxProfiles.map((item) => html`<label key=${item.name} class="profile-transfer-row"><input type="checkbox" checked=${selected.has(item.name)} onChange=${() => toggle(item.name)}/><span>${item.name} ${sandboxProfileSummary(item)}</span></label>`)}</div><div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy} onClick=${submit}>${busy ? 'Exporting…' : 'Export'}</button></div></${Overlay}>`;
}

function SandboxImport({ current, state, actions, confirmDiscard }) {
  const [raw, setRaw] = useState(''); const [envelope, setEnvelope] = useState(null); const [preview, setPreview] = useState(null); const [conflict, setConflict] = useState('skip'); const [error, setError] = useState(''); const [busy, setBusy] = useState('');
  const inspect = async () => { setError(''); setBusy('inspect'); try { const parsed = JSON.parse(raw); if (parsed?.format !== 'tclaude-sandbox-profiles' || parsed?.format_version !== 1) throw new Error('not a tclaude sandbox-profile export'); const found = await actions.inspectSandboxBundle(parsed); setEnvelope(parsed); setPreview(found); } catch (e) { setError(message(e)); } finally { setBusy(''); } };
  const submit = async () => { if (!preview) { setError('preview the import first'); return; } setBusy('import'); try { await actions.importSandboxBundle(envelope, conflict); state.closeDialog(); } catch (e) { setError(message(e)); } finally { setBusy(''); } };
  const existing = new Set(current.sandboxProfiles.map((item) => item.name)); const incoming = preview?.profiles || envelope?.profiles || [];
  return html`<${Overlay} id="sandbox-profile-import-modal" labelledby="sandbox-profile-import-title" onClose=${state.closeDialog} dirty=${!!raw} confirmDiscard=${confirmDiscard}><h3 id="sandbox-profile-import-title"><span class="sandbox-word-regular">Import sandbox profiles</span><span class="sandbox-word-wizard">📜 Read wards</span></h3><${Row} label="File"><input type="file" accept=".json,application/json" onChange=${async (event) => { const file = event.currentTarget.files?.[0]; if (file) { setRaw(await file.text()); setPreview(null); } }}/></${Row}><${Row} label="or paste"><textarea rows="6" value=${raw} onInput=${(event) => { setRaw(event.currentTarget.value); setPreview(null); }}/></${Row}><button disabled=${busy} onClick=${inspect}>Preview</button>${preview && html`<div class="profile-transfer-list">${incoming.map((item) => html`<div key=${item.name} class="profile-transfer-row"><span>${item.name} · ${sandboxProfileSummary(item)}${existing.has(item.name) ? ' · already exists locally' : ''}</span></div>`)}</div>${incoming.some((item) => existing.has(item.name)) && html`<${Row} label="Name conflicts"><${Select} id="sandbox-profile-import-conflict" value=${conflict} onChange=${setConflict} options=${[['skip', 'Skip existing'], ['overwrite', 'Overwrite existing'], ['error', 'Stop with an error']]}/></${Row}>`}`}
    <div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button onClick=${state.closeDialog}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy || !preview} onClick=${submit}>${busy === 'import' ? 'Importing…' : 'Import'}</button></div></${Overlay}>`;
}

function ManagementApp({ state, actions, confirmDiscard, openProfilePermissions }) {
  const current = state.view.value; const descriptor = current.dialog;
  const previousManager = useRef('');
  useEffect(() => {
    if (previousManager.current && !current.manager) document.dispatchEvent(new CustomEvent('tclaude:management-closed', { detail: { kind: previousManager.current } }));
    previousManager.current = current.manager;
  }, [current.manager]);
  return html`${current.manager && html`<${Manager} kind=${current.manager} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'profile-editor' && html`<${ProfileEditor} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions}/>`}
    ${descriptor?.kind === 'role-editor' && html`<${RoleEditor} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'profile-export' && html`<${ProfileExport} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'profile-import' && html`<${ProfileImport} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'sandbox-editor' && html`<${SandboxEditor} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'sandbox-export' && html`<${SandboxExport} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}
    ${descriptor?.kind === 'sandbox-import' && html`<${SandboxImport} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`}`;
}

export function mountManagementIsland({ host, state, actions, confirmDiscard, openProfilePermissions, registerCleanup }) {
  const controller = {
    openProfilesManageModal: () => actions.openManager('profiles'), openProfileEditor: actions.openProfileEditor, removeProfile: actions.removeProfile,
    openRolesManageModal: () => actions.openManager('roles'), openRoleEditor: actions.openRoleEditor, removeRole: actions.removeRole,
    openSandboxProfilesManageModal: () => actions.openManager('sandbox'), openSandboxProfileEditor: actions.openSandboxEditor, removeSandboxProfile: actions.removeSandbox,
  };
  const unregister = registerManagementController(controller);
  render(html`<${ManagementApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions}/>` , host);
  registerCleanup(() => { unregister(); render(null, host); });
}
