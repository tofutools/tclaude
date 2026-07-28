import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { profileSummary, profileAliasesLabel, profileChoices, findProfileByHandle } from './profiles.js';
import { roleSummary } from './roles.js';
import { AUTO_MEMORY_TRI_OPTIONS, dirtyDraft, harnessByName, harnessDefaults, profileDraft, profilePayload, readTri, roleDraft, rolePayload, TRI_OPTIONS } from './management-model.js';
import { registerManagementController } from './management-controller.js';
import {
  sandboxAccessAxes,
  sandboxAccessDraftErrors,
  sandboxPredictionWarnings,
  sandboxProfileSummary,
} from './sandbox-profiles-data.js';
import { pickDirectory } from './helpers.js';
import { lineDiff } from './line-diff.js';
import { useDialogFocus } from './dialog-focus.js';
import { wizWord } from './slop.js';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { GroupCloneDialog, GroupContextDialog, GroupImportDialog, TemplateDeployDialog, TemplateDuplicateDialog, TemplateEditor, TemplateFromGroupDialog, TemplateImportDialog, TemplateManager, TemplateStartersDialog } from './template-management-island.js';
import { approvalPolicyLabel, approvalReviewerHelp, approvalReviewerOptions } from './approval-controls.js';
import { HelpDisclosure, HelpField } from './help-field.js';
import { SandboxImplHint } from './sandbox-impl-hint.js';
import {
  autoCompactWindowHintFor, sandboxModeHelpForImplementation,
  sandboxImplHintFor, sandboxImplClearedNoticeFor, sandboxImplOptionsFor,
} from './agent-spawn-model.js';

// Mirrors the spawn dialog's copy: which layer owns the wall, the experimental
// framing, and the platform requirement stated rather than implied. A profile
// may pin the layer on a host that cannot run it — that is legitimate authoring
// — so the editor discloses instead of refusing.
const SANDBOX_IMPL_TITLE = 'Which layer owns OS-level containment for agents launched from this '
  + 'profile. harness-builtin is offered only when the selected harness owns a real OS sandbox. '
  + "tclaude's built-in OS sandbox is EXPERIMENTAL: it runs the "
  + "whole harness process inside a tclaude-owned bubblewrap namespace and turns the harness's own "
  + 'sandbox off inside it. Linux only, and it needs bwrap plus unprivileged user namespaces — a '
  + 'host without them refuses the launch instead of falling back. '
  + 'Unset leaves the choice to the spawn-time profile chain.';

// Shared with the spawn dialog's own copy of this control: the two consequences
// an operator has to know are the cap and the status-line decoupling.
const AUTO_COMPACT_WINDOW_TITLE = 'Context capacity in tokens for Claude Code\'s auto-compaction '
  + '(CLAUDE_CODE_AUTO_COMPACT_WINDOW). Accepts 450000, 450k or 0.5M; blank uses the model default. '
  + 'Pin it below a 1M model\'s real window so a long-lived agent compacts while it is still sharp. '
  + 'Capped at the model\'s actual context window.';
const NETWORK_ACCESS_HELP = 'List rows describe outbound destinations. Host matches one exact DNS '
  + 'name. Domain matches the named domain and can optionally include its subdomains. CIDR matches '
  + 'an IP network. Loopback covers connections to the local machine. Ports are optional integer '
  + 'ports; blank allows all ports for that destination. Applied global, group, and explicit list '
  + 'policies compose by intersection: destinations and ports must be allowed by every applicable '
  + 'list, and compatible destination selectors and port sets are intersected. The Effective '
  + 'policy preview reports enforcement capability limits for the selected implementation, '
  + 'harness, and platform.';

const html = htm.bind(h);

function message(error) { return error?.message || String(error); }
function clone(value) { return JSON.parse(JSON.stringify(value)); }
function change(setDraft, key, value) { setDraft((draft) => ({ ...draft, [key]: value })); }
function accessRowShapeError(network, unixSockets) {
  const isRow = (row) => !!row && typeof row === 'object' && !Array.isArray(row);
  const networkIndex = (network.allow || []).findIndex((row) => !isRow(row));
  if (networkIndex >= 0) return `Network row ${networkIndex + 1} must be a JSON object containing a host, domain, CIDR, or loopback selector.`;
  const socketIndex = (unixSockets.allow || []).findIndex((row) => !isRow(row));
  return socketIndex >= 0
    ? `Unix-socket row ${socketIndex + 1} must be a JSON object containing a path or path_glob selector.`
    : '';
}

/* One entry of the common-rule preset menu: a button that inserts the entry's
   audited paths as ordinary deny rows, with its rationale, warning and the
   exact paths it would insert visible before the click. Nothing about the
   entry is stored — after insertion the rows are plain, editable table rows. */
function CommonRuleEntry({ entry, onAdd, variant = 'filesystem' }) {
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
  const noPaths = variant === 'filesystem' && !paths.length;
  return html`<div class=${variant === 'filesystem' ? 'sbx-common-rule-entry' : 'sbx-access-template-entry'} data-rule=${entry.id}>
    <button type="button" class=${variant === 'filesystem' ? 'sbx-common-rule-add' : 'sbx-access-template-add'} aria-describedby=${describedBy} aria-disabled=${noPaths ? 'true' : null} onClick=${() => { if (!noPaths) onAdd(entry); }}>＋ ${entry.label || entry.id}</button>
    <span class="sbx-common-rule-descr" id=${descrID}>${entry.description || ''}</span>
    ${entry.warning ? html`<span class="sbx-common-rule-warn" id=${warnID}>⚠ ${entry.warning}</span>` : null}
    <code class="sbx-common-rule-paths" id=${pathsID}>${paths.length ? paths.join(' · ') : '(no audited paths on this platform)'}</code>
  </div>`;
}

const ACCESS_MODE_OPTIONS = [
  ['', 'No override'],
  ['open', 'Full access'],
  ['closed', 'No access'],
  ['list', 'Access list'],
];

function NetworkAccessEditor({ draft, setDraft, catalog, notice, setNotice }) {
  const [helpOpen, setHelpOpen] = useState('');
  // Access-rule arrays are sparse on the wire: Go deliberately omits an empty
  // `allow`, including for list-mode empty intersections. Normalize at the
  // render boundary so legacy and modern-empty payloads share one safe shape.
  const rules = sandboxAccessAxes({ network: draft.network }).network;
  const update = (patch) => setDraft((value) => ({ ...value, network: { ...value.network, ...patch } }));
  const updateRow = (index, patch) => update({ allow: rules.allow.map((row, i) => i === index ? { ...row, ...patch } : row) });
  // Prefer the selector that survives wire normalization, then preserve an
  // intentionally empty domain/CIDR key while its value is being authored.
  const selector = (row) => row.domain ? 'domain'
    : row.cidr ? 'cidr'
      : row.loopback === true ? 'loopback'
        : row.host ? 'host'
          : Object.hasOwn(row, 'domain') ? 'domain'
            : Object.hasOwn(row, 'cidr') ? 'cidr'
              : 'host';
  const changeSelector = (index, kind) => {
    const next = kind === 'loopback' ? { loopback: true, ports: rules.allow[index].ports || [] } : { [kind]: '', ports: rules.allow[index].ports || [] };
    update({ allow: rules.allow.map((row, i) => i === index ? next : row) });
  };
  const insert = (entry) => {
    const incoming = clone(entry.entries || []);
    const existing = new Set(rules.allow.map((row) => JSON.stringify(row)));
    const added = incoming.filter((row) => !existing.has(JSON.stringify(row)));
    update({ mode: entry.mode || 'list', allow: [...rules.allow, ...added] });
    setNotice({ label: entry.label, added: added.length, skipped: incoming.length - added.length, warning: entry.warning || '', note: entry.note || '' });
  };
  return html`<fieldset class="sbx-section sbx-access-axis" hidden=${false}><legend class="sbx-section-legend">Network <${HelpDisclosure}
      id="sandbox-profile-editor-network-help" label="Network access" help=${NETWORK_ACCESS_HELP}
      open=${helpOpen === 'sandbox-profile-editor-network-help'} setOpen=${setHelpOpen}/></legend>
    <${Select} id="sandbox-profile-editor-network-mode" value=${rules.mode || ''} onChange=${(mode) => update({ mode, allow: mode === 'list' ? rules.allow : [] })} options=${ACCESS_MODE_OPTIONS}/>
    ${rules.mode === 'list' && html`<div class="sbx-rows sbx-network-rows">${rules.allow.map((row, index) => { const kind = selector(row); return html`<div key=${index} class="sbx-row sbx-access-row sbx-network-row">
      <${Select} class="sbx-network-selector" value=${kind} onChange=${(value) => changeSelector(index, value)} options=${[['host', 'host'], ['domain', 'domain'], ['cidr', 'CIDR'], ['loopback', 'loopback']]}/>
      ${kind === 'loopback' ? html`<span class="sbx-network-value sbx-network-value-readonly" aria-hidden="true">—</span>` : html`<input class="sbx-network-value" value=${row[kind] || ''} placeholder=${kind === 'cidr' ? '192.0.2.0/24' : 'example.com'} onInput=${(event) => updateRow(index, { [kind]: event.currentTarget.value })}/>`}
      <span class="sbx-network-modifier">${kind === 'domain' && html`<label class="sbx-inline-check"><input type="checkbox" checked=${!!row.include_subdomains} onChange=${(event) => updateRow(index, { include_subdomains: event.currentTarget.checked })}/> subdomains</label>`}</span>
      <input class="sbx-network-ports" list="sandbox-common-ports" value=${Array.isArray(row.ports) ? row.ports.join(', ') : row.ports || ''} placeholder="ports (optional)" title="Comma-separated ports. Common suggestions are 22, 80, and 443; leaving this blank allows all ports for the destination." onInput=${(event) => updateRow(index, { ports: event.currentTarget.value })}/>
      <button type="button" aria-label="Delete network row" onClick=${() => update({ allow: rules.allow.filter((_, i) => i !== index) })}>×</button>
    </div>`; })}</div>
    <datalist id="sandbox-common-ports"><option value="443"/><option value="80, 443"/><option value="22"/></datalist>
    <button type="button" class="sbx-add-row" onClick=${() => update({ allow: [...rules.allow, { host: '', ports: [] }] })}>＋ add destination</button>`}
    <details class="sbx-common-rules"><summary>＋ insert network template</summary><div class="sbx-common-rule-list">${(catalog.network_templates || []).map((entry) => html`<${CommonRuleEntry} key=${entry.id} variant="access" entry=${{ ...entry, description: entry.note, paths: (entry.entries || []).map((row) => row.domain || row.host || row.cidr || 'loopback') }} onAdd=${() => insert(entry)}/>` )}</div></details>
    ${(catalog.global_network || []).length > 0 && html`<details class="sbx-inherited-access"><summary>Inherited global network config (${catalog.global_network.length})</summary>${catalog.global_network.map((row, index) => html`<div key=${index} class="sbx-rule-note"><strong>${row.origin?.harness} · ${row.origin?.setting}:</strong> ${JSON.stringify(row.entry || { mode: row.mode })}</div>`)}</details>`}
    ${notice && html`<div class="sbx-common-rule-notice" role="status">Inserted “${notice.label}”: ${notice.added} added, ${notice.skipped} already present.${notice.warning ? ` ⚠ ${notice.warning}` : ''}</div>`}
  </fieldset>`;
}

function SocketAccessEditor({ draft, setDraft, catalog, notice, setNotice }) {
  const rules = sandboxAccessAxes({ unix_sockets: draft.unix_sockets }).unix_sockets;
  const update = (patch) => setDraft((value) => ({ ...value, unix_sockets: { ...value.unix_sockets, ...patch } }));
  const updateRow = (index, patch) => update({ allow: rules.allow.map((row, i) => i === index ? { ...row, ...patch } : row) });
  const insert = (entry) => {
    const mode = entry.mode || 'list';
    const incoming = clone(entry.entries || []);
    const existing = new Set(rules.allow.map((row) => JSON.stringify(row)));
    const added = incoming.filter((row) => !existing.has(JSON.stringify(row)));
    const removed = mode === 'list' ? 0 : rules.allow.length;
    update({ mode, allow: mode === 'list' ? [...rules.allow, ...added] : [] });
    setNotice({ label: entry.label, added: added.length, skipped: incoming.length - added.length, removed, warning: entry.warning || '' });
  };
  return html`<fieldset class="sbx-section sbx-access-axis"><legend>Unix sockets</legend>
    <${Select} id="sandbox-profile-editor-unix-sockets-mode" value=${rules.mode || ''} onChange=${(mode) => update({ mode, allow: mode === 'list' ? rules.allow : [] })} options=${ACCESS_MODE_OPTIONS}/>
    <p class="sbx-axis-help">The tclaude agentd socket is always reachable and is not an editable row.</p>
    ${rules.mode === 'list' && html`<div class="sbx-rows sbx-socket-rows">${rules.allow.map((row, index) => { const glob = Object.hasOwn(row, 'path_glob'); return html`<div key=${index} class="sbx-row sbx-access-row sbx-socket-row">
      <${Select} class="sbx-socket-selector" value=${glob ? 'path_glob' : 'path'} onChange=${(kind) => update({ allow: rules.allow.map((item, i) => i === index ? { [kind]: '' } : item) })} options=${[['path', 'path'], ['path_glob', 'glob']]}/>
      <input class="sbx-socket-value" value=${glob ? row.path_glob || '' : row.path || ''} placeholder=${glob ? '/tmp/ssh-*/agent.*' : '/run/example.sock'} onInput=${(event) => updateRow(index, glob ? { path_glob: event.currentTarget.value } : { path: event.currentTarget.value })}/>
      <button type="button" aria-label="Delete Unix-socket row" onClick=${() => update({ allow: rules.allow.filter((_, i) => i !== index) })}>×</button>
    </div>`; })}</div>
    <button type="button" class="sbx-add-row" onClick=${() => update({ allow: [...rules.allow, { path: '' }] })}>＋ add socket</button>`}
    <details class="sbx-common-rules"><summary>＋ insert socket template</summary><div class="sbx-common-rule-list">${(catalog.socket_templates || []).map((entry) => html`<${CommonRuleEntry} key=${entry.id} variant="access" entry=${{ ...entry, description: entry.note, paths: (entry.entries || []).map((row) => row.path || row.path_glob) }} onAdd=${() => insert(entry)}/>` )}</div></details>
    ${(catalog.global_unix_sockets || []).length > 0 && html`<details class="sbx-inherited-access"><summary>Inherited global socket config (${catalog.global_unix_sockets.length})</summary>${catalog.global_unix_sockets.map((row, index) => html`<div key=${index} class="sbx-rule-note"><strong>${row.origin?.harness} · ${row.origin?.setting}:</strong> ${JSON.stringify(row.entry || { mode: row.mode })}</div>`)}</details>`}
    ${notice && html`<div class="sbx-common-rule-notice" role="status">Inserted “${notice.label}”: ${notice.added} added, ${notice.skipped} already present.${notice.removed ? ` ${notice.removed} incompatible existing row${notice.removed === 1 ? '' : 's'} removed.` : ''}${notice.warning ? ` ⚠ ${notice.warning}` : ''}</div>`}
  </fieldset>`;
}

function commonRulePaths(entry) {
  return [...new Set((entry?.paths || []).map((path) => String(path || '').trim()).filter(Boolean))];
}

function globalFilesystemAccessLabel(access) {
  return ({ read: 'read', write: 'write', deny: 'deny', 'deny-read': 'deny read', 'deny-write': 'deny write' })[access] || access;
}

function globalFilesystemHarnessLabel(harnesses) {
  const set = new Set(harnesses || []);
  if (set.has('claude') && set.has('codex')) return 'Claude + Codex';
  if (set.has('claude')) return 'Claude';
  if (set.has('codex')) return 'Codex';
  return 'global';
}

function globalFilesystemRuleTooltip(rule) {
  const access = globalFilesystemAccessLabel(rule.access);
  const origins = (rule.origins || []).map((origin) => {
    const harness = origin.harness === 'claude' ? 'Claude Code' : origin.harness === 'codex' ? 'Codex' : origin.harness;
    return `${harness}: ${origin.source} → ${origin.setting}.${origin.note ? ` ${origin.note}` : ''}`;
  });
  return [`Inherited ${access} rule for ${rule.path}. This row is read-only because it belongs to global harness config, not this profile.`, ...origins].join('\n');
}

function globalFilesystemForHarness(rows, filter) {
  if (filter === 'both') return rows || [];
  if (filter === 'none') return [];
  return (rows || []).flatMap((row) => {
    const origins = (row.origins || []).filter((origin) => origin.harness === filter);
    if (origins.length === 0 && !(row.harnesses || []).includes(filter)) return [];
    const originAccess = origins.map((origin) => origin.access).filter(Boolean);
    const access = originAccess.includes('write') ? 'write' : originAccess.includes('read') ? 'read' : originAccess[0] || row.access;
    return [{ ...row, access, harnesses: [filter], origins }];
  });
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

   A leading `~` or `~/` expands against the daemon home shipped with the
   catalog before cleaning, in the same order as the daemon. `~otheruser/...`
   stays literal because the daemon does not guess another account's home.
   When talking to an older daemon that does not ship its home, `~` also stays
   literal so the comparison remains conservative.

   The inserted row always keeps the catalog's own spelling; only the
   comparison normalizes. */
function pathIdentity(path, home = '') {
  let raw = String(path || '').trim();
  if (!raw) return '';
  const daemonHome = String(home || '').trim();
  if (daemonHome && (raw === '~' || raw.startsWith('~/'))) {
    raw = raw === '~' ? daemonHome : `${daemonHome}/${raw.slice(2)}`;
  }
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
    <div id=${profiles ? 'profiles-list' : roles ? 'roles-list' : 'sandbox-profiles-list'}><${RequestList} request=${request} label=${kind} retry=${() => actions.load(kind)}>${list.length ? list.map((item) => html`<div key=${item.name} class=${`template-card ${profiles ? 'profile' : roles ? 'role' : 'sandbox-profile'}-card${profiles && item.disabled ? ' profile-card-disabled' : ''}`} data-key=${item.name}><div class="tc-head"><span class="tc-name">${item.name}</span>${profiles && item.disabled ? html`<span class="tc-disabled" aria-label="Disabled profile">🚫 Disabled</span>` : null}${profiles && item.aliases?.length ? html`<span class="tc-aliases">${profileAliasesLabel(item)}</span>` : null}<span class="tc-descr">${profiles ? profileSummary(item) : roles ? roleSummary(item) : sandboxProfileSummary(item)}</span><span class="tc-actions"><button class="tool" onClick=${() => profiles ? actions.openProfileEditor(item) : roles ? actions.openRoleEditor(item) : actions.openSandboxEditor(item)}>edit</button>${kind === 'sandbox' && html`<button class="tool sandbox-profile-clone" onClick=${() => actions.openSandboxClone(item)}>clone</button>`}<button class="tool" onClick=${() => profiles ? actions.removeProfile(item.name) : roles ? actions.removeRole(item.name) : actions.removeSandbox(item.name)}>delete</button></span></div>${profiles && item.disabled && html`<div class="tc-sub tc-disabled-reason">${item.disabled_reason}</div>`}${roles && item.descr && html`<div class="tc-sub">${item.descr}</div>`}${kind === 'sandbox' && html`<div class="sbx-caps">${(item.filesystem || []).map((entry) => html`<div key=${`${entry.access}:${entry.path}`} class="sbx-cap"><span class=${`sbx-cap-tag sbx-cap-${entry.access}`}>${entry.access}</span><span class="sbx-cap-val" title=${entry.path}>${entry.path}</span></div>`)}${(item.includes || []).map((name) => html`<div key=${`inc:${name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-inc">include</span><span class="sbx-cap-val" title=${name}>${name}</span></div>`)}${(item.environment || []).map((entry) => { const binding = `${entry.name} → ${entry.value}`; return html`<div key=${`env:${entry.name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-env">env</span><span class="sbx-cap-val" title=${binding}>${binding}</span></div>`; })}${(item.agent_directories || []).map((name) => html`<div key=${`own:${name}`} class="sbx-cap"><span class="sbx-cap-tag sbx-cap-own">own</span><span class="sbx-cap-val" title=${`${name} — isolated per agent`}>${name}</span></div>`)}</div>`}</div>`) : html`<div class="template-empty">${all.length ? wizWord('No items match the filter.', 'No items match the filter.') : profiles ? wizWord('No spawn profiles yet', 'No familiar patterns yet') : roles ? wizWord('No roles yet', 'No classes yet') : wizWord('No sandbox profiles yet', 'No wards yet')}</div>`}</${RequestList}></div>
    <div class="modal-buttons"><span class="spacer"></span><button onClick=${state.closeManager}>Close</button></div>
  </${Overlay}>`;
}

function Select({ value, onChange, options, ...props }) { return html`<select ...${props} value=${value} onChange=${(event) => onChange(event.currentTarget.value)}>${options.map(([key, label]) => html`<option key=${key} value=${key}>${label}</option>`)}</select>`; }
function Row({ label, hidden = false, title = '', children }) { return html`<label class="cron-create-row" hidden=${hidden} title=${title}><span class="cron-create-label">${label}</span>${children}</label>`; }

function HarnessFields({ draft, setDraft, catalog, actions, profile = false, sandboxImpl = {} }) {
  const hEntry = harnessByName(catalog, draft.harness);
  const models = hEntry?.models || [];
  const hasModelList = models.length > 0;
  const [customModel, setCustomModel] = useState(() => hasModelList && !!draft.model && !models.includes(draft.model));

  // Preview warning and informational messages for the effective boundary. The
  // daemon decides — an explicit `off` is unsafe on any machine, while
  // `inherit` depends on host settings the browser cannot know, and OpenCode's
  // split server boundary needs a non-warning disclosure. The profile probe has
  // no dir, so the verdict reflects the portable, machine-global tiers.
  const [autonomyWarnings, setAutonomyWarnings] = useState([]);
  const [sandboxInfo, setSandboxInfo] = useState([]);
  const autonomyRequest = useRef(0);
  useEffect(() => {
    if (typeof actions?.loadUnsandboxedAutonomy !== 'function') return undefined;
    const request = ++autonomyRequest.current;
    // Selects fire on change, not per keystroke, so a short debounce only
    // collapses a rapid harness→mode retap; it is imperceptible otherwise.
    const timer = setTimeout(() => {
      Promise.resolve(actions.loadUnsandboxedAutonomy({
        harness: draft.harness, sandbox: draft.sandbox,
        sandboxImplementation: profile ? draft.sandbox_implementation : '',
        approval: draft.approval,
      })).then((result) => {
        if (request !== autonomyRequest.current) return;
        setSandboxInfo(result?.info || []);
        setAutonomyWarnings(result?.warnings || []);
      });
    }, 200);
    return () => clearTimeout(timer);
  }, [draft.harness, draft.sandbox, draft.sandbox_implementation, draft.approval, profile]);
  const updateHarness = (harness) => {
    const h = harnessByName(catalog, harness);
    const defaults = harnessDefaults(h);
    setCustomModel(false);
    setDraft((current) => ({
      ...current, harness, model: '', effort: '', ...defaults,
      trust_dir: '', remote_control: '', auto_memory: '',
      ssh_workaround: !!h?.can_ssh_workaround,
      // Keep every explicit implementation visible across harness switches.
      // An incapable selection gets an inline refusal warning and the server
      // remains the apply authority.
      sandbox_implementation: current.sandbox_implementation,
      sandbox_implementation_cleared: null,
    }));
  };
  const [helpOpen, setHelpOpen] = useState('');
  const modelID = profile ? 'profile-editor-model' : 'role-editor-model';
  const approvalID = profile ? 'profile-editor-approval' : 'role-editor-approval';
  const sandboxID = profile ? 'profile-editor-sandbox' : 'role-editor-sandbox';
  const toolsID = profile ? 'profile-editor-tools' : 'role-editor-tools';
  const approvalLabel = draft.harness === 'codex' ? 'Approval policy' : 'Permission mode';
  const approvalHelp = hEntry?.approval_mode_help?.[draft.approval] || '';
  const sandboxHelp = sandboxModeHelpForImplementation(
    hEntry?.sandbox_mode_help?.[draft.sandbox],
    draft.sandbox_implementation || '',
    draft.harness,
  );
  const toolsHelp = hEntry?.tools_mode_help?.[draft.tools] || '';
  const askTimeoutHelp = hEntry?.ask_timeout_mode_help?.[draft.ask_user_question_timeout] || '';
  const autoCompactWindowHint = autoCompactWindowHintFor(
    { autoCompactWindow: draft.auto_compact_window },
    {
      autoCompactWindowMin: Number(hEntry?.auto_compact_window_min) || 0,
      autoCompactWindowMax: Number(hEntry?.auto_compact_window_max) || 0,
    },
  );
  const harnessLabel = hEntry?.display_name || hEntry?.name || '';
  const sandboxImplOptions = sandboxImplOptionsFor(
    sandboxImpl?.options, harnessLabel, hEntry?.can_builtin_os_sandbox !== false,
  );
  const sandboxImplCleared = sandboxImplClearedNoticeFor(
    { sandboxImplCleared: draft.sandbox_implementation_cleared },
  );
  const sandboxImplHint = sandboxImplHintFor(
    { sandboxImpl: draft.sandbox_implementation },
    {
      showSandboxImpl: !!hEntry,
      sandboxImplDefault: sandboxImpl?.default || 'harness-builtin',
      sandboxImplCanBuiltin: hEntry?.can_builtin_os_sandbox !== false,
      sandboxImplHarness: harnessLabel,
      sandboxImplCanStacked: !!hEntry?.can_stacked,
      sandboxImplStackedAvailability: sandboxImpl?.stacked?.[hEntry?.name] || {},
      // A profile may legitimately pin stacked for a DIFFERENT machine — that
      // is the whole reason this editor discloses instead of refusing — so the
      // AppArmor answer is passed as what it is: a fact about the host running
      // this dashboard. The hint copy says "on this host" for the same reason.
      sandboxImplStackedAppArmorLikely: !!sandboxImpl?.stacked_apparmor_nested_bwrap_likely,
      sandboxImplHostAvailable: hEntry?.tclaude_layer_server_boundary
        ? sandboxImpl?.server_host_available !== false
        : sandboxImpl?.host_available !== false,
      sandboxImplHostReason: hEntry?.tclaude_layer_server_boundary
        ? sandboxImpl?.server_host_unavailable_reason || ''
        : sandboxImpl?.host_unavailable_reason || '',
    },
  );
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
    ${profile && hEntry && html`<${Row} label="Sandbox impl"
      title=${SANDBOX_IMPL_TITLE}>
      <div class="cron-create-target">
        <${Select} id="profile-editor-sandbox-impl" value=${draft.sandbox_implementation}
          onChange=${(value) => setDraft((current) => ({
    ...current, sandbox_implementation: value, sandbox_implementation_cleared: null,
  }))}
          options=${[['', 'Unset (inherit at spawn)'],
    ...sandboxImplOptions.map((option) => [option.value, option.label])]} />
        <${SandboxImplHint} hint=${sandboxImplHint} />
      </div>
    </${Row}>`}
    ${profile && sandboxImplCleared && html`<div class="cron-create-row"
      id="profile-editor-sandbox-impl-cleared-row" role="alert">
      <span class="cron-create-label"></span>
      <div class="cron-create-target">
        <div class="spawn-field-hint warn">${sandboxImplCleared.text}</div>
      </div>
    </div>`}
    <${HelpField} id=${approvalID} label=${approvalLabel} title="Controls when the harness requests approval; it does not change the sandbox."
      value=${draft.approval}
      options=${(hEntry?.approval_modes || []).map((value) => ({ value, label: approvalPolicyLabel(draft.harness, value, hEntry.default_approval) }))}
      onChange=${(event) => change(setDraft, 'approval', event.currentTarget.value)}
      help=${approvalHelp} open=${helpOpen === approvalID} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_approval} />
    <${HelpField} id=${toolsID} label="Tool governance" title="Uniform action for OpenCode's bash, glob, grep, lsp, task, and skill tools."
      value=${draft.tools}
      options=${(hEntry?.tools_modes || []).map((value) => ({ value, label: value + (value === hEntry.default_tools ? ' (recommended)' : '') }))}
      onChange=${(event) => change(setDraft, 'tools', event.currentTarget.value)}
      help=${toolsHelp} open=${helpOpen === toolsID} setOpen=${setHelpOpen}
      disabled=${!hEntry?.can_tools} />
    <div class=${`cron-create-row${sandboxInfo.length === 0 ? ' sandbox-info-pending' : ''}`}
      id=${`${profile ? 'profile' : 'role'}-editor-sandbox-info`}>
      <span class="cron-create-label"></span>
      <div class="cron-create-target" role="status">
        ${sandboxInfo.map((message) => html`<div class="spawn-field-hint info" key=${message}>ℹ ${message}</div>`)}
      </div>
    </div>
    ${autonomyWarnings.length > 0 && html`<div class="cron-create-row" id=${`${profile ? 'profile' : 'role'}-editor-autonomy-warning`}>
      <span class="cron-create-label"></span>
      <div class="cron-create-target" role="alert">
        ${autonomyWarnings.map((warning) => html`<div class="spawn-field-hint warn" key=${warning}>${warning}</div>`)}
      </div>
    </div>`}
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
    ${profile && hEntry?.can_auto_compact_window && html`<${Row} label="Compact at"
      title=${AUTO_COMPACT_WINDOW_TITLE}>
      <div class="cron-create-target">
        <input id="profile-editor-auto-compact-window" type="text" aria-label="Auto-compact window (tokens)"
          value=${draft.auto_compact_window}
          onInput=${(event) => change(setDraft, 'auto_compact_window', event.currentTarget.value)}
          placeholder="blank = model default; e.g. 450k" autocomplete="off" spellcheck="false" inputmode="numeric" />
        ${autoCompactWindowHint && html`<div
          class=${`spawn-field-hint${autoCompactWindowHint.warn ? ' warn' : ''}`}>${autoCompactWindowHint.text}</div>`}
      </div>
    </${Row}>`}
  `;
}

function ProfileEditor({ descriptor, state, actions, confirmDiscard, openProfilePermissions, openProfileContextFeatures }) {
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
  const sshWorkaroundAvailable = !!hEntry?.can_ssh_workaround
    && draft.sandbox === 'tclaude-agent';
  return html`<${Overlay} id="profile-editor-modal" labelledby="profile-editor-title" onClose=${state.closeDialog} onSubmitHotkey=${saving ? null : submit} dirty=${dirty} blocked=${saving} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="profile-editor-title">${local ? wizWord('Custom launch — this agent only', 'Bespoke summons — this familiar only') : seed && options.editExisting !== false ? wizWord(`Edit profile: ${seed.name}`, `Edit pattern: ${seed.name}`) : wizWord('New spawn profile', 'New familiar pattern')}</h3>
    <${Row} label="Name" hidden=${local}><input id="profile-editor-name" value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} placeholder="profile name — kebab-or-snake-case label" autofocus autocomplete="off" spellcheck="false" /></${Row}>
    <${Row} label="Aliases" hidden=${local} title="Alternate handles for this same profile. Separate multiple aliases with commas."><input id="profile-editor-aliases" value=${draft.aliases_text} onInput=${(event) => change(setDraft, 'aliases_text', event.currentTarget.value)} placeholder="e.g. codex-reviewer, cold-reviewer" autocomplete="off" spellcheck="false" /></${Row}>
    <${Row} label="Disabled" hidden=${local} title="Keep this profile visible and editable, but block every spawn that would use it."><input id="profile-editor-disabled" type="checkbox" checked=${draft.disabled} onChange=${(event) => change(setDraft, 'disabled', event.currentTarget.checked)} /></${Row}>
    <${Row} label="Disable reason" hidden=${local} title="Required while disabled. Retained when enabled so it can be reviewed or reused later."><textarea id="profile-editor-disabled-reason" value=${draft.disabled_reason} onInput=${(event) => change(setDraft, 'disabled_reason', event.currentTarget.value)} rows="2" placeholder="required when disabled — retained after re-enabling" spellcheck="true" /></${Row}>
    <${HarnessFields} draft=${draft} setDraft=${setDraft} catalog=${catalog} actions=${actions}
      sandboxImpl=${descriptor.sandboxImpl} profile />
    <${Row} label="Trust dir" hidden=${hEntry && !hEntry.can_dir_trust} title=${`Pre-trust the launch directory so the agent doesn't freeze on the harness's trust-folder dialog${hEntry?.dir_trust_store ? ` (edits ${hEntry.dir_trust_store})` : ''}.`}><${Select} id="profile-editor-trust-dir" value=${draft.trust_dir} onChange=${(value) => change(setDraft, 'trust_dir', value)} options=${TRI_OPTIONS}/></${Row}>
    <${Row} label="Remote control" hidden=${hEntry && !hEntry.can_remote_control}><${Select} id="profile-editor-remote-control" value=${draft.remote_control} onChange=${(value) => change(setDraft, 'remote_control', value)} options=${TRI_OPTIONS}/></${Row}>
    <${Row} label="Auto memory" hidden=${hEntry && !hEntry.can_auto_memory} title="Claude Code's built-in auto memory. tclaude disables it by default: agents sharing a repo all read one per-project memory store and cross-pollute each other's notes. Does not affect CLAUDE.md."><${Select} id="profile-editor-auto-memory" value=${draft.auto_memory} onChange=${(value) => change(setDraft, 'auto_memory', value)} options=${AUTO_MEMORY_TRI_OPTIONS}/></${Row}>
    <${Row} label="SSH workaround" hidden=${!hEntry?.can_ssh_workaround} title=${sshWorkaroundAvailable ? "Use an agent-owned copy of the host SSH client config to avoid Codex sandbox ownership errors. This overrides Git core.sshCommand; uncheck it if the workaround conflicts with your setup." : "Available only for the Codex tclaude-agent managed sandbox."}><input id="profile-editor-ssh-workaround" type="checkbox" checked=${sshWorkaroundAvailable && draft.ssh_workaround} disabled=${!sshWorkaroundAvailable} onChange=${(event) => change(setDraft, 'ssh_workaround', event.currentTarget.checked)} /></${Row}>
    ${[['Agent name', 'agent_name', 'optional — names the spawned agent'], ['Role', 'role', 'optional — e.g. researcher, planner'], ['Descr', 'descr', 'optional — short one-line description']].map(([label, key, placeholder]) => html`<${Row} key=${key} label=${label} hidden=${local}><input value=${draft[key]} onInput=${(event) => change(setDraft, key, event.currentTarget.value)} placeholder=${placeholder} autocomplete="off" spellcheck="false"/></${Row}>`)}
    <${Row} label="Initial msg" hidden=${local}><textarea value=${draft.initial_message} onInput=${(event) => change(setDraft, 'initial_message', event.currentTarget.value)} rows="3" placeholder="optional — task brief pre-filled into the spawn dialog" spellcheck="false" /></${Row}>
    ${[['Sync worktree', 'sync_worktree'], ['Auto focus', 'auto_focus'], ['Group context', 'include_group_default_context'], ['Group owner', 'is_owner']].map(([label, key]) => html`<${Row} key=${key} label=${label} hidden=${local && key !== 'is_owner'}><${Select} id=${key === 'is_owner' ? 'profile-editor-owner' : `profile-editor-${key.replaceAll('_', '-')}`} value=${draft[key]} onChange=${(value) => change(setDraft, key, value)} options=${TRI_OPTIONS}/></${Row}>`)}
    <div class="cron-create-row"><span class="cron-create-label">Permissions</span><button id="profile-editor-perms" class="tool" type="button" onClick=${() => openProfilePermissions({ overrides: draft.permission_overrides, ownsGroup: readTri(draft.is_owner) === true, label: draft.agent_name.trim(), onSave: (kept) => change(setDraft, 'permission_overrides', kept) })}>Permissions…</button><span>${Object.keys(draft.permission_overrides).length || ''}</span></div>
    ${(!hEntry || hEntry.can_context_features) && html`<div class="cron-create-row" title="How much of Claude Code's startup context agents from this profile load. Trimming bundled skills, unused tool schemas and system-prompt blocks leaves more of the window for the actual task."><span class="cron-create-label">Startup context</span><button id="profile-editor-context-features" class="tool" type="button" onClick=${() => openProfileContextFeatures({ catalog: hEntry?.context_features || [], selection: draft.context_features, label: draft.agent_name.trim(), onSave: (kept) => change(setDraft, 'context_features', kept) })}>Context…</button><span>${contextFeatureSummary(draft.context_features)}</span></div>`}
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
    <${HarnessFields} draft=${draft} setDraft=${setDraft} catalog=${catalog} actions=${actions}/><${Row} label="Spawn profile"><${Select} value=${draft.spawn_profile} onChange=${(value) => change(setDraft, 'spawn_profile', value)} options=${[['', '(none)'], ...choices.map((choice) => [choice.value, choice.label])]} /></${Row}>
    <div class="cron-create-row"><span class="cron-create-label">Permissions (${draft.permissions.length})</span><div class="ta-perms-list">${slugs.map((slug) => html`<label key=${slug.slug} title=${slug.description || ''}><input type="checkbox" checked=${draft.permissions.includes(slug.slug)} onChange=${() => toggle(slug.slug)} /> ${slug.slug}</label>`)}</div></div>
    <div class="cron-create-error" role="alert">${state.error.value}</div><div class="modal-buttons"><button disabled=${saving} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button id="role-editor-submit" class="primary" disabled=${saving} onClick=${submit}>${saving ? 'Saving…' : 'Save role'}</button></div>
  </${Overlay}>`;
}

function sandboxFilesystemEditorRows(filesystem, spellings) {
  const retained = new Map((spellings?.rules || []).map((rule) => [rule.resolved_path, rule.spellings || []]));
  return (filesystem || []).map((row) => {
    const aliases = retained.get(row.path) || [];
    if (!aliases.length) return clone(row);
    return {
      ...clone(row),
      path: aliases[0],
      _resolved_path: row.path,
      _spellings: clone(aliases),
    };
  });
}

function sandboxFilesystemWire(draft, baseline) {
  const pathsUnchanged = JSON.stringify((draft.filesystem || []).map((row) => row.path))
    === JSON.stringify((baseline.filesystem || []).map((row) => row.path));
  const retained = draft.filesystem_spellings;
  if (pathsUnchanged && retained?.version === 1) {
    return {
      filesystem: (draft.filesystem || []).map((row) => ({
        path: row._resolved_path || row.path,
        access: row.access,
      })),
      filesystem_spellings: clone(retained),
    };
  }
  // A path add/remove/edit is an explicit re-authoring operation. The daemon
  // canonicalizes the visible spellings and writes a fresh sidecar; it never
  // infers new launch authority from an old retained spelling.
  return {
    filesystem: (draft.filesystem || []).map((row) => ({ path: row.path, access: row.access })),
    filesystem_spellings: null,
  };
}

function SandboxEditor({ descriptor, sandboxProfiles, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const seed = descriptor.seed || null; const options = descriptor.options || {};
  const baseline = useMemo(() => {
    const axes = sandboxAccessAxes(seed || {});
    const filesystem_spellings = clone(seed?.filesystem_spellings ?? null);
    return { id: seed?.id || 0, name: seed?.name || '', filesystem: sandboxFilesystemEditorRows(seed?.filesystem || [], filesystem_spellings), filesystem_spellings, environment: clone(seed?.environment || []), includes: clone(seed?.includes || []), agent_directories: clone(seed?.agent_directories || []), network_access: '', network: axes.network, unix_sockets: axes.unix_sockets };
  }, [descriptor]);
  const initialFilesystemWire = sandboxFilesystemWire(baseline, baseline);
  const [draft, setDraft] = useState(() => clone(baseline)); const [advanced, setAdvanced] = useState(false); const [rawFS, setRawFS] = useState(() => JSON.stringify(initialFilesystemWire.filesystem, null, 2)); const [rawSpellings, setRawSpellings] = useState(() => JSON.stringify(initialFilesystemWire.filesystem_spellings, null, 2)); const [rawEnv, setRawEnv] = useState(() => JSON.stringify(baseline.environment, null, 2)); const [rawIncludes, setRawIncludes] = useState(() => JSON.stringify(baseline.includes, null, 2)); const [rawAgentDirs, setRawAgentDirs] = useState(() => JSON.stringify(baseline.agent_directories, null, 2)); const [rawNetwork, setRawNetwork] = useState(() => JSON.stringify(baseline.network, null, 2)); const [rawSockets, setRawSockets] = useState(() => JSON.stringify(baseline.unix_sockets, null, 2));
  // The audited common-rule presets. They are pure row inserters: nothing from
  // the catalog is persisted, so a profile never depends on it being loaded.
  const [commonRules, setCommonRules] = useState({ version: 0, categories: [], informational: [], global_filesystem: [], global_network: [], global_unix_sockets: [], network_templates: [], socket_templates: [], global_config_warnings: [] });
  // Global harness rows are context, not draft state. Keep the potentially
  // long ambient list folded until the operator asks to inspect it.
  const [showGlobalFilesystem, setShowGlobalFilesystem] = useState(false);
  const [globalHarnessFilter, setGlobalHarnessFilter] = useState('both');
  // The menu is long and most profiles never touch it, so it ships folded.
  const [commonRulesOpen, setCommonRulesOpen] = useState(false);
  // What the last insertion did, including the entry's warning — the operator
  // must see the consequence of the rows that just appeared in the table.
  const [commonRuleNotice, setCommonRuleNotice] = useState(null);
  const [networkTemplateNotice, setNetworkTemplateNotice] = useState(null);
  const [socketTemplateNotice, setSocketTemplateNotice] = useState(null);
  const [evaluateFor, setEvaluateFor] = useState('');
  const [prediction, setPrediction] = useState(null);
  const [predictionError, setPredictionError] = useState('');
  const [predictionBusy, setPredictionBusy] = useState(false);
  const [effectiveContext, setEffectiveContext] = useState(0);
  // The feed is optional and its failures are the menu's own business. They
  // must never reach `state.error`, which carries save and validation
  // refusals: a late rejection would replace the reason a save was refused
  // with an explanation of a convenience the operator did not ask for.
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
      setCommonRules(value || { version: 0, categories: [], informational: [], global_filesystem: [], global_network: [], global_unix_sockets: [], network_templates: [], socket_templates: [], global_config_warnings: [] });
      setCommonRuleFeedError(''); setCommonRuleFeedBusy(false);
    }).catch((error) => {
      if (generation !== commonRuleGeneration.current) return;
      setCommonRuleFeedError(message(error)); setCommonRuleFeedBusy(false);
    });
  };
  // Unmount bumps the generation so an in-flight load resolves into nothing.
  useEffect(() => { loadCommonRules(); return () => { commonRuleGeneration.current++; }; }, []);
  const [directoryStatus, setDirectoryStatus] = useState({ missing: [], creatable: [] }); const [directoryBusy, setDirectoryBusy] = useState(false);
  const directoryGeneration = useRef(0); const submitRef = useRef(null); const wasSaving = useRef(false); const filesystemSignature = JSON.stringify(draft.filesystem); const latestFilesystem = useRef(filesystemSignature); latestFilesystem.current = filesystemSignature;
  const dirty = dirtyDraft(draft, baseline);
  const saving = state.busy.value === 'sandbox-save';
  const setFS = (index, patch) => setDraft((value) => ({ ...value, filesystem: value.filesystem.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  const setEnv = (index, patch) => setDraft((value) => ({ ...value, environment: value.environment.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  const parseRaw = () => { const filesystem = JSON.parse(rawFS || '[]'); const filesystem_spellings = JSON.parse(rawSpellings || 'null'); const environment = JSON.parse(rawEnv || '[]'); const includes = JSON.parse(rawIncludes || '[]'); const agent_directories = JSON.parse(rawAgentDirs || '[]'); const network = JSON.parse(rawNetwork || '{}'); const unix_sockets = JSON.parse(rawSockets || '{}'); if (![filesystem, environment, includes, agent_directories].every(Array.isArray)) throw new Error('filesystem, environment, includes and agent dirs must be arrays'); if (filesystem_spellings !== null && (!filesystem_spellings || Array.isArray(filesystem_spellings))) throw new Error('filesystem spellings must be a JSON object or null'); if (!network || typeof network !== 'object' || Array.isArray(network) || !unix_sockets || typeof unix_sockets !== 'object' || Array.isArray(unix_sockets)) throw new Error('network and unix sockets must be JSON objects'); if ([network, unix_sockets].some((axis) => axis.allow != null && !Array.isArray(axis.allow))) throw new Error('network and unix socket allow fields must be arrays'); const rowError = accessRowShapeError(network, unix_sockets); if (rowError) throw new Error(rowError); const axes = sandboxAccessAxes({ network, unix_sockets }); return { filesystem, filesystem_spellings, environment, includes, agent_directories, network: axes.network, unix_sockets: axes.unix_sockets }; };
  const applyRaw = () => { try { const parsed = parseRaw(); setDraft((value) => ({ ...value, ...parsed, filesystem: sandboxFilesystemEditorRows(parsed.filesystem, parsed.filesystem_spellings) })); state.error.value = ''; return true; } catch (error) { state.error.value = error.message || String(error); return false; } };
  const toggleAdvanced = () => { if (advanced && !applyRaw()) return; if (!advanced) { const wire = sandboxFilesystemWire(draft, baseline); setRawFS(JSON.stringify(wire.filesystem, null, 2)); setRawSpellings(JSON.stringify(wire.filesystem_spellings, null, 2)); setRawEnv(JSON.stringify(draft.environment, null, 2)); setRawIncludes(JSON.stringify(draft.includes, null, 2)); setRawAgentDirs(JSON.stringify(draft.agent_directories, null, 2)); setRawNetwork(JSON.stringify(draft.network, null, 2)); setRawSockets(JSON.stringify(draft.unix_sockets, null, 2)); } setAdvanced(!advanced); };
  const submit = async () => {
    let value = { ...draft, ...sandboxFilesystemWire(draft, baseline) };
    if (advanced) { try { value = { ...draft, ...parseRaw() }; } catch (error) { state.error.value = error.message || String(error); return; } }
    await actions.saveSandbox({ draft: value, original: seed, options });
  };
  useEffect(() => {
    if (wasSaving.current && !saving) queueMicrotask(() => {
      const button = submitRef.current;
      if (button?.isConnected && !button.disabled && !button.closest('[inert]')) button.focus();
    });
    wasSaving.current = saving;
  }, [saving]);
  useEffect(() => { if (advanced) return undefined; let active = true; const generation = ++directoryGeneration.current; const filesystem = clone(draft.filesystem); const timer = setTimeout(async () => { try { const result = await actions.inspectDirectories(filesystem); if (active && generation === directoryGeneration.current) setDirectoryStatus({ missing: result?.missing || [], creatable: result?.creatable || [] }); } catch (_) { if (active && generation === directoryGeneration.current) setDirectoryStatus({ missing: [], creatable: [] }); } }, 300); return () => { active = false; clearTimeout(timer); }; }, [advanced, filesystemSignature]);
  let predictionDraft = { ...draft, ...sandboxFilesystemWire(draft, baseline) };
  let predictionDraftError = '';
  if (advanced) {
    try { predictionDraft = { ...draft, ...parseRaw() }; }
    catch (error) { predictionDraftError = message(error); }
  }
  const accessErrors = sandboxAccessDraftErrors(draft);
  // Raw JSON is authoritative while Advanced is open, so a repaired raw axis
  // can resume preview even if the hidden structured draft remains invalid.
  const predictionAccessErrors = predictionDraftError ? [] : advanced
    ? sandboxAccessDraftErrors(predictionDraft)
    : accessErrors;
  const predictionPauseReason = predictionDraftError || predictionAccessErrors[0] || '';
  const predictionPaused = !!predictionDraft.name.trim() && !!predictionPauseReason;
  const predictionSignature = JSON.stringify([predictionDraftError ? null : predictionDraft, evaluateFor, options.group || '']);
  useEffect(() => {
    if (typeof actions.predictSandbox !== 'function') return undefined;
    if (predictionDraftError || !predictionDraft.name.trim() || predictionAccessErrors.length > 0) {
      setPrediction(null); setPredictionError(''); setPredictionBusy(false);
      return undefined;
    }
    let active = true;
    setPredictionBusy(true);
    const targets = evaluateFor ? [(() => { const [implementation, harness, platform] = evaluateFor.split('/'); return { implementation, harness, platform }; })()] : [];
    const timer = setTimeout(() => {
      actions.predictSandbox(predictionDraft, targets, { group: options.group || '' }).then((value) => {
        if (!active) return;
        setPrediction(value); setPredictionError(''); setPredictionBusy(false); setEffectiveContext((index) => Math.min(index, Math.max(0, (value.contexts || []).length - 1)));
      }).catch((error) => {
        if (!active) return;
        setPrediction(null); setPredictionError(message(error)); setPredictionBusy(false);
      });
    }, 300);
    return () => { active = false; clearTimeout(timer); };
  }, [predictionSignature]);
  const createMissing = async () => { const filesystem = clone(draft.filesystem); const signature = JSON.stringify(filesystem); const generation = ++directoryGeneration.current; setDirectoryBusy(true); state.error.value = ''; try { const result = await actions.createDirectories(filesystem); const refreshed = await actions.inspectDirectories(filesystem); if (generation === directoryGeneration.current && signature === latestFilesystem.current) { const created = result?.created || []; state.error.value = `Created ${created.length} sandbox director${created.length === 1 ? 'y' : 'ies'}.`; setDirectoryStatus({ missing: refreshed?.missing || [], creatable: refreshed?.creatable || [] }); } } catch (error) { if (generation === directoryGeneration.current) state.error.value = error.message || String(error); } finally { setDirectoryBusy(false); } };
  const configureWithAgent = () => { let value = { ...draft, ...sandboxFilesystemWire(draft, baseline) }; if (advanced) { try { value = { ...draft, ...parseRaw() }; } catch (error) { state.error.value = error.message || String(error); return; } } const editExisting = options.editExisting ?? !!seed; const targetName = editExisting ? options.targetName || seed?.name || '' : ''; state.closeDialog(); void actions.configureSandboxWithAgent(value, { targetName, editExisting, cloneSourceName: options.cloneSourceName, onCreate: options.onCreate }); };
  const structuredFilesystemWire = sandboxFilesystemWire(draft, baseline);
  const rawDirty = advanced && [rawFS !== JSON.stringify(structuredFilesystemWire.filesystem, null, 2), rawSpellings !== JSON.stringify(structuredFilesystemWire.filesystem_spellings, null, 2), rawEnv !== JSON.stringify(draft.environment, null, 2), rawIncludes !== JSON.stringify(draft.includes, null, 2), rawAgentDirs !== JSON.stringify(draft.agent_directories, null, 2), rawNetwork !== JSON.stringify(draft.network, null, 2), rawSockets !== JSON.stringify(draft.unix_sockets, null, 2)].some(Boolean);
  // A preset inserts ordinary deny rows and then forgets it ever existed: no
  // stored ID, no hidden state. Paths already present in the table are left
  // exactly as authored rather than silently re-denied, and the notice says so.
  // The running set also absorbs an entry whose own paths alias each other,
  // which no audited entry does today — if one ever did, the notice's skip
  // count would need to distinguish that from "already in the table".
  const addCommonRule = (entry) => {
    const paths = commonRulePaths(entry);
    const existing = new Set(draft.filesystem.map((row) => pathIdentity(row.path, commonRules.home)).filter(Boolean));
    const added = [];
    for (const path of paths) {
      const identity = pathIdentity(path, commonRules.home);
      if (!identity || existing.has(identity)) continue;
      existing.add(identity);
      added.push(path);
    }
    if (added.length) setDraft((value) => ({ ...value, filesystem: [...value.filesystem, ...added.map((path) => ({ path, access: 'deny' }))] }));
    setCommonRuleNotice({ label: entry.label || entry.id, added, skipped: paths.length - added.length, warning: entry.warning || '' });
  };
  const globalFilesystem = commonRules.global_filesystem || [];
  const visibleGlobalFilesystem = globalFilesystemForHarness(globalFilesystem, globalHarnessFilter);
  const globalConfigWarnings = commonRules.global_config_warnings || [];
  // Same guard as the Save button, so the hotkey can never reach a save the
  // mouse path refuses.
  const warnings = sandboxPredictionWarnings(prediction);
  const selectedEffective = prediction?.contexts?.[effectiveContext] || null;
  const selectedEffectiveAxes = selectedEffective ? sandboxAccessAxes(selectedEffective) : null;
  const submitBlocked = saving || directoryBusy || (!advanced && accessErrors.length > 0);
  return html`<${Overlay} id="sandbox-profile-editor-modal" labelledby="sandbox-profile-editor-title" onClose=${state.closeDialog} onSubmitHotkey=${submitBlocked ? null : submit} dirty=${dirty || rawDirty} blocked=${saving || directoryBusy} confirmDiscard=${confirmDiscard} registerClose=${registerClose} resizeKey="tclaude.dash.modalSize.sandbox-profile-editor"><h3 id="sandbox-profile-editor-title">${options.cloneSourceName ? wizWord(`Clone sandbox profile: ${options.cloneSourceName}`, `Mirror ward: ${options.cloneSourceName}`) : seed ? wizWord(`Edit sandbox profile: ${seed.name}`, `Edit ward: ${seed.name}`) : wizWord('New sandbox profile', 'New ward')}</h3><p class="modal-meta">Directory grants widen the sandbox; environment values are injected at launch. Agent-owned directories create a fresh writable cache directory for each spawned agent and set the named environment variable to its path. Network and Unix-socket fields compose by intersection. The tclaude agent socket remains reachable independently of editable socket policy.</p><${Row} label="Name"><input value=${draft.name} onInput=${(event) => change(setDraft, 'name', event.currentTarget.value)} placeholder="e.g. shared-build-caches" autofocus autocomplete="off" spellcheck="false"/></${Row}>
    ${!advanced && html`<${NetworkAccessEditor} draft=${draft} setDraft=${setDraft} catalog=${commonRules} notice=${networkTemplateNotice} setNotice=${setNetworkTemplateNotice}/><${SocketAccessEditor} draft=${draft} setDraft=${setDraft} catalog=${commonRules} notice=${socketTemplateNotice} setNotice=${setSocketTemplateNotice}/>`}
    <fieldset class="sbx-section" hidden=${advanced}><legend>Filesystem</legend>
      ${(globalFilesystem.length > 0 || globalConfigWarnings.length > 0) && html`<div class="sbx-global-filesystem">
        <div class="sbx-global-controls"><label class="sbx-global-toggle" title="These read-only rows come from Claude Code and Codex global sandbox config. They are launch context, not part of the named profile."><input id="sandbox-profile-editor-show-global-filesystem" type="checkbox" checked=${showGlobalFilesystem} onChange=${(event) => setShowGlobalFilesystem(event.currentTarget.checked)}/> Show inherited global config rules${globalFilesystem.length ? ` (${globalFilesystem.length})` : ''}</label>
          ${showGlobalFilesystem && globalFilesystem.length > 0 && html`<label class="sbx-global-filter" for="sandbox-profile-editor-global-harness-filter">Builtins <select id="sandbox-profile-editor-global-harness-filter" value=${globalHarnessFilter} onChange=${(event) => setGlobalHarnessFilter(event.currentTarget.value)}><option value="both">Claude + Codex</option><option value="claude">Claude only</option><option value="codex">Codex only</option><option value="none">None</option></select></label>`}
        </div>
        ${showGlobalFilesystem && visibleGlobalFilesystem.length > 0 && html`<div id="sandbox-profile-editor-global-filesystem" class="sbx-rows sbx-global-rows">
          ${visibleGlobalFilesystem.map((row, index) => { const tooltip = globalFilesystemRuleTooltip(row); return html`<div key=${`${row.path}:${row.access}:${index}`} class="sbx-row sbx-global-row" role="group" title=${tooltip} aria-label=${tooltip}><span class=${`sbx-access sbx-global-access sbx-global-access-${row.access}`}>${globalFilesystemAccessLabel(row.access)}</span><input class="sbx-path" value=${row.path || ''} readonly aria-readonly="true" tabindex="-1"/><span class="sbx-global-harness">${globalFilesystemHarnessLabel(row.harnesses)}</span></div>`; })}
        </div>`}
        ${globalConfigWarnings.map((warning, index) => html`<div key=${index} class="sbx-global-warning" role="status">⚠ ${warning}</div>`)}
      </div>`}
      <div class="sbx-rows">${draft.filesystem.map((row, index) => html`<div key=${index} class="sbx-row"><${Select} class="sbx-access" value=${row.access || 'read'} onChange=${(access) => setFS(index, { access })} options=${[['read', 'read'], ['write', 'write'], ['deny', 'deny']]}/><span class="sbx-path-binding"><input class="sbx-path" value=${row.path || ''} onInput=${(event) => setFS(index, { path: event.currentTarget.value })}/>${row._resolved_path && html`<span class="sbx-binding-target">binds → ${row._resolved_path}${row._spellings?.length > 1 ? ` · also retained: ${row._spellings.slice(1).join(', ')}` : ''}</span>`}</span><button type="button" onClick=${async () => { const result = await pickDirectory({ startDir: row.path || '', title: 'Select a sandbox directory' }); if (result.path) setFS(index, { path: result.path }); else if (result.error) state.error.value = result.error; }}>Browse…</button><button type="button" onClick=${() => setDraft((value) => ({ ...value, filesystem: value.filesystem.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row" onClick=${() => setDraft((value) => ({ ...value, filesystem: [...value.filesystem, { path: '', access: 'read' }] }))}>＋ add directory</button>
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
        ${(commonRules.informational || []).length > 0 && html`<details class="sbx-common-rule-informational"><summary>Required, non-removable access</summary>${(commonRules.informational || []).map((entry) => html`<div key=${entry.id} class="sbx-rule-note"><strong>${entry.label}:</strong> ${entry.description}</div>`)}</details>`}
      </details>
      ${commonRuleNotice && html`<div id="sandbox-profile-editor-common-rule-notice" class="sbx-common-rule-notice" role="status">
        <span>${commonRuleNotice.added.length ? `Added ${commonRuleNotice.added.length} deny row${commonRuleNotice.added.length === 1 ? '' : 's'} from “${commonRuleNotice.label}”: ${commonRuleNotice.added.join(' · ')}.` : `“${commonRuleNotice.label}” added no rows.`}${commonRuleNotice.skipped ? ` ${commonRuleNotice.skipped} path${commonRuleNotice.skipped === 1 ? ' was' : 's were'} already in the table and left as authored.` : ''}</span>
        ${commonRuleNotice.warning ? html`<span class="sbx-common-rule-warn">⚠ ${commonRuleNotice.warning}</span>` : null}
        <button type="button" class="sbx-common-rule-dismiss" aria-label="Dismiss common-rule notice" onClick=${() => setCommonRuleNotice(null)}>×</button>
      </div>`}
    </fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend>Environment</legend><div class="sbx-rows">${draft.environment.map((row, index) => html`<div key=${index} class="sbx-row"><input value=${row.name || ''} placeholder="NAME" onInput=${(event) => setEnv(index, { name: event.currentTarget.value })}/><input value=${row.value || ''} placeholder="value" onInput=${(event) => setEnv(index, { value: event.currentTarget.value })}/><button type="button" onClick=${() => setDraft((value) => ({ ...value, environment: value.environment.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row" onClick=${() => setDraft((value) => ({ ...value, environment: [...value.environment, { name: '', value: '' }] }))}>＋ add variable</button></fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend title="Included profiles apply first, in order; this profile overrides them.">Includes</legend><div class="sbx-rows">${draft.includes.map((name, index) => html`<div key=${index} class="sbx-row"><${Select} class="sbx-inc-name" value=${name} onChange=${(value) => setDraft((old) => ({ ...old, includes: old.includes.map((item, i) => i === index ? value : item) }))} options=${[['', '— choose profile —'], ...sandboxProfiles.filter((item) => item.name !== seed?.name || item.name === name).map((item) => [item.name, item.name])]} /><button type="button" onClick=${() => setDraft((old) => ({ ...old, includes: old.includes.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row sbx-include-add" onClick=${() => setDraft((old) => ({ ...old, includes: [...old.includes, ''] }))}>＋ include profile</button></fieldset>
    <fieldset class="sbx-section" hidden=${advanced}><legend title="Environment-variable names backed by isolated writable directories created per agent.">Agent-owned directories</legend><div class="sbx-rows">${draft.agent_directories.map((name, index) => html`<div key=${index} class="sbx-row"><input class="sbx-agent-name" value=${name} placeholder="GOCACHE" onInput=${(event) => setDraft((old) => ({ ...old, agent_directories: old.agent_directories.map((item, i) => i === index ? event.currentTarget.value : item) }))}/><button type="button" onClick=${() => setDraft((old) => ({ ...old, agent_directories: old.agent_directories.filter((_, i) => i !== index) }))}>×</button></div>`)}</div><button type="button" class="sbx-add-row sbx-agent-add" onClick=${() => setDraft((old) => ({ ...old, agent_directories: [...old.agent_directories, ''] }))}>＋ add agent-owned directory</button></fieldset>
    <fieldset class="sbx-section sbx-effective-preview"><legend>Effective policy preview</legend>
      <label>Evaluate for <select id="sandbox-profile-editor-evaluate-for" value=${evaluateFor} onChange=${(event) => setEvaluateFor(event.currentTarget.value)}>
        <option value="">Resolved default target</option>
        <option value="harness-builtin/claude/linux">Claude builtin · Linux</option>
        <option value="harness-builtin/claude/darwin">Claude builtin · macOS</option>
        <option value="harness-builtin/codex/linux">Codex builtin · Linux</option>
        <option value="harness-builtin/codex/darwin">Codex builtin · macOS</option>
        <option value="tclaude-layer/claude/linux">tclaude layer · Claude · Linux</option>
        <option value="tclaude-layer/claude/darwin">tclaude layer · Claude · macOS</option>
        <option value="stacked/claude/linux">stacked · Claude · Linux</option>
        <option value="stacked/claude/darwin">stacked · Claude · macOS</option>
      </select></label>
      ${predictionPaused && html`<div class="sbx-preview-status">Effective policy preview paused: ${predictionPauseReason}</div>`}
      ${predictionBusy && html`<div class="sbx-preview-status">Evaluating draft…</div>`}
      ${predictionError && html`<div class="sbx-preview-error" role="alert">Could not evaluate draft: ${predictionError}</div>`}
      ${prediction?.targets?.map((target, index) => html`<div key=${index} class="sbx-capability-preview">
        <strong>${target.target.implementation} · ${target.target.harness} · ${target.target.platform}</strong>${target.resolved_by ? ` — ${target.resolved_by}` : ''}
        <div>Network: ${target.axes.network.tier} · ${target.axes.network.outcome}</div><div class=${target.axes.network.outcome === 'enforced' ? '' : 'warn'}>${target.axes.network.detail}</div>
        <div>Unix sockets: ${target.axes.unix_sockets.tier} · ${target.axes.unix_sockets.outcome}</div><div class=${target.axes.unix_sockets.outcome === 'enforced' ? '' : 'warn'}>${target.axes.unix_sockets.detail}</div>
      </div>`)}
      ${(prediction?.contexts?.length || 0) > 1 && html`<label>Assignment context <select id="sandbox-profile-editor-effective-context" value=${effectiveContext} onChange=${(event) => setEffectiveContext(Number(event.currentTarget.value))}>${prediction.contexts.map((context, index) => html`<option value=${index}>${context.context.group_name ? `group ${context.context.group_name}` : context.context.global === draft.name ? 'global assignment' : 'explicit selection'}</option>`)}</select></label>`}
      ${selectedEffective && html`<div class="sbx-effective-values">
        <div><strong>Layers:</strong> ${['global', 'group', 'explicit'].flatMap((scope) => selectedEffective.context[scope] ? [`${scope} “${selectedEffective.context[scope]}”`] : []).join(' → ') || 'draft only'}</div>
        <div><strong>Network:</strong> ${selectedEffectiveAxes.network.mode || 'unset'}${selectedEffectiveAxes.network.mode === 'list' ? ` · ${selectedEffectiveAxes.network.allow.length} destination(s)` : ''}</div>
        <div><strong>Unix sockets:</strong> ${selectedEffectiveAxes.unix_sockets.mode || 'unset'}${selectedEffectiveAxes.unix_sockets.mode === 'list' ? ` · ${selectedEffectiveAxes.unix_sockets.allow.length} entry(s)` : ''}</div>
        <div><strong>agentd socket:</strong> ${selectedEffective.agentd_socket}</div>
      </div>`}
      ${prediction?.remaining_contexts ? html`<div class="sbx-preview-status">Showing 10 contexts; ${prediction.remaining_contexts} more assignment contexts are omitted.</div>` : null}
      ${warnings.composition.map((notice, index) => html`<div key=${index} class="sbx-composition-warning" role="alert">⚠ ${notice.detail}</div>`)}
      ${warnings.capability.map((detail, index) => html`<div key=${index} class="sbx-capability-warning" role="status">⚠ ${detail}</div>`)}
    </fieldset>
    ${!advanced && accessErrors.map((error, index) => html`<div key=${index} class="sbx-access-validation" role="alert">⚠ ${error}</div>`)}
    ${!advanced && directoryStatus.missing.length > 0 && html`<div class="sbx-missing"><span>${directoryStatus.missing.length} director${directoryStatus.missing.length === 1 ? 'y does' : 'ies do'} not exist. Saving is allowed; read/write rules activate on a later launch, while deny targets must exist before launch.</span>${directoryStatus.creatable.length > 0 && html`<button type="button" disabled=${directoryBusy || saving} onClick=${createMissing}>${directoryBusy ? 'Creating…' : `Create ${directoryStatus.creatable.length} missing director${directoryStatus.creatable.length === 1 ? 'y' : 'ies'}`}</button>`}</div>`}
    <button type="button" class="sbx-advanced-toggle" aria-expanded=${advanced} onClick=${toggleAdvanced}>${advanced ? '▾' : '▸'} Advanced — edit raw JSON</button>${advanced && html`<div class="sbx-advanced-body"><${Row} label="Filesystem JSON"><textarea id="sandbox-profile-editor-filesystem" rows="6" value=${rawFS} onInput=${(event) => setRawFS(event.currentTarget.value)}/></${Row}><${Row} label="Filesystem spellings JSON"><textarea id="sandbox-profile-editor-filesystem-spellings" rows="6" value=${rawSpellings} onInput=${(event) => setRawSpellings(event.currentTarget.value)}/></${Row}><${Row} label="Environment JSON"><textarea id="sandbox-profile-editor-environment" rows="6" value=${rawEnv} onInput=${(event) => setRawEnv(event.currentTarget.value)}/></${Row}><${Row} label="Network JSON"><textarea id="sandbox-profile-editor-network" rows="6" value=${rawNetwork} onInput=${(event) => setRawNetwork(event.currentTarget.value)}/></${Row}><${Row} label="Unix sockets JSON"><textarea id="sandbox-profile-editor-unix-sockets" rows="6" value=${rawSockets} onInput=${(event) => setRawSockets(event.currentTarget.value)}/></${Row}><${Row} label="Includes JSON"><textarea id="sandbox-profile-editor-includes" rows="3" value=${rawIncludes} onInput=${(event) => setRawIncludes(event.currentTarget.value)}/></${Row}><${Row} label="Agent dirs JSON"><textarea id="sandbox-profile-editor-agent-directories" rows="3" value=${rawAgentDirs} onInput=${(event) => setRawAgentDirs(event.currentTarget.value)}/></${Row}></div>`}
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
  return html`<${Overlay} id="profile-import-modal" labelledby="profile-import-title" onClose=${state.closeDialog} dirty=${dirty} blocked=${!!busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="profile-import-title">Import spawn profiles</h3><${Row} label="File"><input type="file" accept=".json,application/json" onChange=${async (event) => { const file = event.currentTarget.files?.[0]; if (file) { setRaw(await file.text()); setPreview(null); } }}/></${Row}><${Row} label="or paste"><textarea rows="6" value=${raw} onInput=${(event) => { setRaw(event.currentTarget.value); setPreview(null); }} /></${Row}><button type="button" class="tool profile-transfer-preview-button" disabled=${busy} onClick=${inspect}>Preview</button>
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
  const inspect = async () => {
    setError(''); setBusy('inspect');
    try {
      const parsed = JSON.parse(raw);
      if (parsed?.format !== 'tclaude-sandbox-profiles' || ![1, 2, 3, 4, 5, 6, 7, 8].includes(parsed?.format_version)) throw new Error('not a tclaude sandbox-profile export');
      const found = await actions.inspectSandboxBundle(parsed);
      setEnvelope(parsed); setPreview(found);
    } catch (e) {
      setError(message(e));
    } finally { setBusy(''); }
  };
  const existing = new Set(current.sandboxProfiles.map((item) => item.name)); const incoming = preview?.profiles || envelope?.profiles || [];
  // The inspect reports include-graph errors PER conflict policy
  // ("skip" keeps a clashing local profile's own includes, so only one policy
  // may be invalid). Importing under "error" only succeeds when no names
  // clash — every incoming profile lands — so it shares the overwrite graph.
  const includeError = preview?.include_errors?.[conflict === 'skip' ? 'skip' : 'overwrite'] || '';
  const submit = async () => {
    if (!preview) { setError('preview the import first'); return; }
    if (includeError) { setError(includeError); return; }
    setBusy('import');
    try { await actions.importSandboxBundle(envelope, conflict); state.closeDialog(); }
    catch (e) { setError(message(e)); }
    finally { setBusy(''); }
  };
  return html`<${Overlay} id="sandbox-profile-import-modal" labelledby="sandbox-profile-import-title" onClose=${state.closeDialog} dirty=${!!raw} blocked=${!!busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}><h3 id="sandbox-profile-import-title"><span class="sandbox-word-regular">Import sandbox profiles</span><span class="sandbox-word-wizard">📜 Read wards</span></h3><${Row} label="File"><input type="file" accept=".json,application/json" onChange=${async (event) => { const file = event.currentTarget.files?.[0]; if (file) { setRaw(await file.text()); setPreview(null); } }}/></${Row}><${Row} label="or paste"><textarea rows="6" value=${raw} onInput=${(event) => { setRaw(event.currentTarget.value); setPreview(null); }}/></${Row}><button type="button" class="tool profile-transfer-preview-button" disabled=${busy} onClick=${inspect}>Preview</button>${preview && html`<div class="profile-transfer-list">${incoming.map((item) => html`<div key=${item.name} class="profile-transfer-row"><span>${item.name} · ${sandboxProfileSummary(item)}${existing.has(item.name) ? ' · already exists locally' : ''}</span></div>`)}</div>${incoming.some((item) => existing.has(item.name)) && html`<${Row} label="Name conflicts"><${Select} id="sandbox-profile-import-conflict" value=${conflict} onChange=${(value) => setConflict(value)} options=${[['skip', 'Skip existing'], ['overwrite', 'Overwrite existing'], ['error', 'Stop with an error']]}/></${Row}>`}${includeError && html`<div id="sandbox-profile-import-include-error" role="alert" class="cron-create-error">Include graph invalid under this conflict policy: ${includeError}</div>`}`}
    <div role="alert" class="cron-create-error">${error}</div><div class="modal-buttons"><button disabled=${!!busy} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span><button class="primary" disabled=${busy || !preview || !!includeError} onClick=${submit}>${busy === 'import' ? 'Importing…' : 'Import'}</button></div></${Overlay}>`;
}

function SandboxDiffModal({ model, close }) {
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
  return html`<div ref=${dialogRef} id="sandbox-profile-diff-modal" class="modal-overlay show" role="dialog" aria-modal="true" aria-labelledby="sandbox-profile-diff-title" onClick=${cancelOutside}>
    <div class="config-diff-modal">
      <h3 id="sandbox-profile-diff-title">Confirm sandbox profile changes</h3>
      <p id="sandbox-profile-diff-sub" class="cfg-diff-sub">${model.before ? `${adds} line(s) added, ${dels} removed — server-normalized preview` : `${adds} line(s) added — new server-normalized profile`}</p>
      ${(model.notices || []).map((notice, index) => html`<div key=${index} class="sbx-composition-warning" role="alert">⚠ ${notice.detail}</div>`)}
      <div id="sandbox-profile-diff-body" class="config-diff">${diff.map((line, index) => html`<span key=${index} class=${`dl ${line.t}`}>${sign[line.t]} ${line.s}</span>`)}</div>
      <div class="modal-buttons"><button id="sandbox-profile-diff-cancel" type="button" onClick=${() => close(false)}>Cancel</button><span class="spacer"></span><button ref=${confirmRef} id="sandbox-profile-diff-confirm" class="primary" type="button" onClick=${() => close(true)}>Save sandbox profile</button></div>
    </div>
  </div>`;
}

// Keep signal subscriptions at the overlay that consumes them. The dashboard
// snapshot poll republishes template/group arrays every two seconds; a single
// aggregate subscription here would make that unrelated change reconcile all
// open management controls (and close native select popups in Chromium).
function TemplateManagerSlot({ state, actions, confirmDiscard }) {
  if (!state.templateManager.value) return null;
  const current = {
    templates: state.templates.value || [],
    templateGroups: state.templateGroups.value || [],
    profiles: state.profiles.value || [],
    templateFilter: state.templateFilter.value,
  };
  return html`<${TemplateManager} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
}

function TemplateEditorSlot({ state, actions, confirmDiscard, confirm }) {
  const descriptor = state.templateDialog.value;
  if (descriptor?.kind !== 'template-editor') return null;
  const current = {
    busy: state.busy.value,
    error: state.error.value,
    roles: state.roles.value || [],
    profiles: state.profiles.value || [],
  };
  return html`<${TemplateEditor} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} confirm=${confirm}/>`;
}

function ManagerSlot({ state, actions, confirmDiscard }) {
  const kind = state.manager.value;
  if (!kind) return null;
  const current = kind === 'profiles'
    ? { profiles: state.profiles.value || [], profileFilter: state.profileFilter.value, requests: { profiles: state.profilesRequest.request.value } }
    : kind === 'roles'
      ? { roles: state.roles.value || [], roleFilter: state.roleFilter.value, requests: { roles: state.rolesRequest.request.value } }
      : { sandboxProfiles: state.sandboxProfiles.value || [], sandboxFilter: state.sandboxFilter.value, requests: { sandbox: state.sandboxRequest.request.value } };
  return html`<${Manager} kind=${kind} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
}

// contextFeatureSummary renders the profile editor's inline badge for a trim map.
function contextFeatureSummary(features) {
  const states = Object.values(features || {});
  const trimmed = states.filter((state) => state === 'off').length;
  const kept = states.filter((state) => state === 'on').length;
  return [trimmed ? `${trimmed} trimmed` : '', kept ? `${kept} kept` : ''].filter(Boolean).join(' · ');
}

function DialogSlot({ state, actions, confirmDiscard, openProfilePermissions, openProfileContextFeatures }) {
  const descriptor = state.dialog.value;
  switch (descriptor?.kind) {
    case 'profile-editor':
      return html`<${ProfileEditor} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions} openProfileContextFeatures=${openProfileContextFeatures}/>`;
    case 'role-editor':
      return html`<${RoleEditor} descriptor=${descriptor} current=${{ profiles: state.profiles.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'profile-export':
      return html`<${ProfileExport} current=${{ profiles: state.profiles.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'profile-import':
      return html`<${ProfileImport} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'sandbox-editor':
      return html`<${SandboxEditor} descriptor=${descriptor} sandboxProfiles=${state.sandboxProfiles.value || []} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'sandbox-export':
      return html`<${SandboxExport} current=${{ sandboxProfiles: state.sandboxProfiles.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'sandbox-import':
      return html`<${SandboxImport} current=${{ sandboxProfiles: state.sandboxProfiles.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-duplicate':
      return html`<${TemplateDuplicateDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-import':
      return html`<${TemplateImportDialog} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-from-group':
      return html`<${TemplateFromGroupDialog} descriptor=${descriptor} current=${{ templates: state.templates.value || [] }} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-starters':
      return html`<${TemplateStartersDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'group-import':
      return html`<${GroupImportDialog} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'group-context':
      return html`<${GroupContextDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'group-clone':
      return html`<${GroupCloneDialog} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    case 'template-deploy': {
      const current = { templates: state.templates.value || [], templateGroups: state.templateGroups.value || [], profiles: state.profiles.value || [] };
      return html`<${TemplateDeployDialog} descriptor=${descriptor} current=${current} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
    }
    default:
      return null;
  }
}

function SandboxDiffSlot({ state }) {
  const model = state.sandboxDiff.value;
  return html`<${SandboxDiffModal} model=${model} close=${state.cancelSandboxDiff} />`;
}

function ManagementApp({ state, actions, confirm, confirmDiscard, openProfilePermissions, openProfileContextFeatures }) {
  return html`<${TemplateManagerSlot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>
    <${TemplateEditorSlot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} confirm=${confirm}/>
    <${ManagerSlot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>
    <${DialogSlot} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions} openProfileContextFeatures=${openProfileContextFeatures}/>
    <${SandboxDiffSlot} state=${state}/>`;
}

export function mountManagementIsland({ host, state, actions, confirm, confirmDiscard, openProfilePermissions, openProfileContextFeatures, registerCleanup }) {
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
  render(html`<${ManagementApp} state=${state} actions=${actions} confirm=${confirm} confirmDiscard=${confirmDiscard} openProfilePermissions=${openProfilePermissions} openProfileContextFeatures=${openProfileContextFeatures}/>` , host);
  registerCleanup(() => { state.cancelSandboxDiff(false); unregister(); render(null, host); });
}
