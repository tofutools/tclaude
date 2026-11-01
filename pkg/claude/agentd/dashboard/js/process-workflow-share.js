// process-workflow-share.js -- share processes as .pwf workflow files
// (docs/pwf.md) from the dashboard: export a template with every template it
// references, and import one after a review of what it would run. The review
// mirrors `tclaude agent process-templates import`: an import applies exactly
// the reviewed preview (its reviewDigest) and never starts a run.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { downloadBlob } from './paf-model.js';
import { shellConfirm } from './shell-state.js';
import { managementController } from './management-controller.js';

const html = htm.bind(h);
const PWF_TYPE = 'application/vnd.tclaude.pwf+zip';
const MAX_FILE = 32 << 20;
// A .paf can carry workflows beside whole conversations.
const MAX_PAF = 1 << 30;

async function errorText(response) {
  const body = await response.json().catch(() => ({}));
  return body.message || body.error || `${response.status} ${response.statusText}`;
}

// importError keeps the response body: a stale-review conflict carries the
// fresh preview to review again.
async function importError(response) {
  const body = await response.json().catch(() => ({}));
  const err = new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  err.status = response.status;
  err.body = body;
  return err;
}

// Programs run as argv without a shell, so the review shows each argument as
// its own token: quoted when it holds spaces or quotes, with control and
// bidi characters escaped so the text shown is the text that runs.
const INVISIBLE = /[\u0000-\u001f\u007f-\u009f\u00ad\u061c\u200b-\u200f\u2028-\u202e\u2060-\u2069\ufeff]/gu;
export function displayArg(arg) {
  const text = String(arg ?? '').replace(/\\/g, '\\\\').replace(INVISIBLE, (c) => `\\u{${c.codePointAt(0).toString(16)}}`);
  return text === '' || /[\s'"]/.test(text) ? `'${text.replace(/'/g, "'\\''")}'` : text;
}

function listed(lines) {
  const shown = lines.slice(0, 12).map((line) => `• ${line}`);
  if (lines.length > shown.length) shown.push(`…and ${lines.length - shown.length} more`);
  return shown.join('\n');
}

// pathLine says why one machine-specific path matters.
export function pathLine(finding) {
  const why = finding.kind === 'home' ? 'reveals a username' : 'may not exist elsewhere';
  return `${finding.path} (${why}), in ${finding.where}`;
}

// exportConfirmation is the confirm() request before sharing a workflow that
// fails validation, carries paths from this machine or names specific agents.
// Problems and paths export as they are, the way a draft saves, and the
// importer sees the same lists in their review. A step naming an agent that
// has a role here is shared with that role instead; the rest stay named.
export function exportConfirmation(id, problems, paths, assignees = []) {
  const parts = [];
  const converted = assignees.filter((a) => a.role);
  const kept = assignees.filter((a) => !a.role);
  if (problems.length > 0) parts.push(`${id} cannot run until these are fixed. You can still share it; whoever imports it sees the same list.\n\n${listed(problems)}`);
  if (paths.length > 0) parts.push(`${id} carries paths from this machine. A param or a relative path travels better; whoever imports it sees the same list.\n\n${listed(paths.map(pathLine))}`);
  if (converted.length > 0) parts.push(`These steps name an agent the importing node will not have, so they are shared without the name. They ask for the agent's role, or for the profile they already have.\n\n${listed(converted.map((a) => `${a.name} (role ${a.role}), in ${a.steps.join(', ')}`))}`);
  if (kept.length > 0) parts.push(`These agents have no single role here, so their steps stay named. On the importing node they fall to their profile, or wait until reassigned.\n\n${listed(kept.map((a) => `${a.name}, in ${a.steps.join(', ')}`))}`);
  let title = 'Share a workflow that names specific agents?';
  if (problems.length > 0) title = 'Share a workflow that fails validation?';
  else if (paths.length > 0) title = 'Share a workflow with paths from this machine?';
  return {
    title,
    body: parts.join('\n\n'),
    preformatted: true,
    okLabel: converted.length > 0 ? 'Use roles and share' : 'Share anyway',
  };
}

// exportWorkflow downloads <id>.pwf, asking first when exportConfirmation has
// anything to say. It resolves to '' (also when the operator
// declines) or an error message.
export async function exportWorkflow(id, { fetchImpl = fetch, download = downloadBlob, confirm = shellConfirm } = {}) {
  const url = `/v1/process/templates/${encodeURIComponent(id)}/export`;
  const check = await fetchImpl(`${url}?check=1`, { credentials: 'same-origin' });
  if (!check.ok) return errorText(check);
  const { problems = [], paths = [], assignees = [] } = await check.json().catch(() => ({}));
  if ((problems.length > 0 || paths.length > 0 || assignees.length > 0)
    && !(await confirm(exportConfirmation(id, problems, paths, assignees)))) return '';
  const response = await fetchImpl(assignees.some((a) => a.role) ? `${url}?assignees=roles` : url, { credentials: 'same-origin' });
  if (!response.ok) return errorText(response);
  download(await response.blob(), `${String(id).replace(/[^A-Za-z0-9._-]+/g, '-')}.pwf`);
  return '';
}

// importQuery spells the importer's choices the way the daemon reviews them:
// role mappings, rename on an id collision, and a group to pin the process to.
// They go on the preview and the apply alike, so the reviewed digest covers
// them.
export function importQuery(choices = {}) {
  const params = new URLSearchParams();
  for (const [from, to] of Object.entries(choices.roleMap || {}).sort(([a], [b]) => a.localeCompare(b))) {
    if (to && to !== from) params.append('map', `${from}=${to}`);
  }
  if (choices.rename) params.set('rename', '1');
  if (choices.group) params.set('group', choices.group);
  if (choices.workflow) params.set('workflow', choices.workflow);
  return params.toString();
}

// previewImport posts the file without apply: the daemon verifies it and
// says what importing would change here, writing nothing.
export async function previewImport(bytes, { choices = {}, fetchImpl = fetch } = {}) {
  const query = importQuery(choices);
  const response = await fetchImpl(`/v1/process/import${query ? `?${query}` : ''}`, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': PWF_TYPE }, body: bytes,
  });
  if (!response.ok) throw new Error(await errorText(response));
  return response.json();
}

// applyImport imports exactly the reviewed preview.
export async function applyImport(bytes, preview, { replace = false, choices = {}, fetchImpl = fetch } = {}) {
  const extra = importQuery(choices);
  const query = `apply=1&review=${encodeURIComponent(preview.reviewDigest || '')}${replace ? '&replace=1' : ''}${extra ? `&${extra}` : ''}`;
  const response = await fetchImpl(`/v1/process/import?${query}`, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': PWF_TYPE }, body: bytes,
  });
  if (!response.ok) throw await importError(response);
  return response.json();
}

const STATUS_TEXT = {
  new: 'new here',
  identical: 'already here, unchanged',
  update: 'replaces your current version',
  version: 'older pinned version, stored alongside',
};

// importReview shapes a preview for the dialog: what the reviewer must look
// at, in the order they should look at it.
export function importReview(preview) {
  const signatures = preview.signatures || [];
  const valid = signatures.filter((s) => s.status === 'valid');
  let signer = 'Unsigned: nothing proves where this file came from.';
  if (valid.length) {
    const trust = valid.map((s) => s.trust || 'unknown key');
    signer = trust.includes('this node') ? 'Signed by this node.'
      : trust.find((t) => t.startsWith('known peer')) ? `Signed by a ${trust.find((t) => t.startsWith('known peer'))}.`
        : 'Signed by an unknown key: the file is unchanged since signing, but the signer is not known here.';
  } else if (signatures.length) {
    signer = 'The signature does not verify: treat this file as unsigned.';
  }
  const templates = (preview.templates || []).map((t) => {
    let label = STATUS_TEXT[t.status] || t.status;
    if (t.importId && t.importId !== t.id) label += `, as ${t.importId}`;
    else if (t.importRef) label += ', roles mapped';
    return { ...t, label };
  });
  return {
    title: preview.title || preview.rootId || 'Workflow',
    summary: preview.summary || '',
    credit: [preview.author && `By ${preview.author} (as the file says)`, preview.licence && `Licence: ${preview.licence}`].filter(Boolean).join(' · '),
    signer,
    trusted: valid.some((s) => s.trust === 'this node' || (s.trust || '').startsWith('known peer')),
    params: preview.params || [],
    roles: preview.roles || [],
    roleMap: preview.roleMap || [],
    localRoles: preview.localRoles || [],
    renamed: templates.some((t) => t.importId && t.importId !== t.id),
    humans: preview.humans || [],
    programs: (preview.programs || []).map((p) => ({ ...p, command: [p.run, ...(p.args || [])].map(displayArg).join(' ') })),
    assignees: preview.assignees || [],
    problems: preview.problems || [],
    paths: preview.paths || [],
    refused: preview.refused || '',
    templates,
    updates: preview.updates || 0,
    envelope: preview.envelope || null,
    nothingToDo: templates.length > 0 && templates.every((t) => t.status === 'identical'),
  };
}

// processGroupNames lists the groups a process can be pinned to.
async function processGroupNames(fetchImpl = fetch) {
  const response = await fetchImpl('/v1/process/context', { credentials: 'same-origin' });
  if (!response.ok) return [];
  const body = await response.json().catch(() => ({}));
  return (body.groups || []).map((g) => g.group).filter(Boolean);
}

// EnvelopeSection says what else the .paf holds. Importing the workflow takes
// none of it: each performer lands through the agent import and its own review.
function EnvelopeSection({ envelope, workflow, disabled, onPick, onLand }) {
  return html`<div class="pwf-section" data-pwf-envelope>
    ${envelope.workflows.length > 1 && html`<label class="pwf-group">This file holds ${envelope.workflows.length} workflows. Reviewing <select data-pwf-workflow disabled=${disabled} value=${workflow} onChange=${(e) => onPick(e.currentTarget.value)}>
      ${envelope.workflows.map((w) => html`<option key=${w.key} value=${w.key}>${w.root.split('@')[0]}</option>`)}
    </select></label>`}
    ${envelope.performers.length > 0 && html`<h4>Agents it ships</h4><ul class="pwf-roles">${envelope.performers.map((p) => html`<li key=${p.role} data-pwf-performer=${p.role}>
      <b>${p.target}</b> <span class="muted">· ${p.name} on ${p.harness}${p.model ? ` (${p.model})` : ''}, ${p.history ? 'with its conversation' : 'no conversation'}</span>
      <button type="button" class="tool pwf-land" data-pwf-land=${p.agentKey} disabled=${disabled} onClick=${() => onLand(p)}>Land it…</button></li>`)}</ul>
      <p class="muted">Importing the workflow lands none of them. Land one to give a role a pre-briefed agent, with the agent import's own review; or use your own agents and ignore these.</p>`}
  </div>`;
}

function ImportDialog({ file, source, bytes, preview: initialPreview, group: initialGroup = '', onClose, onImported }) {
  const [preview, setPreview] = useState(initialPreview);
  const review = importReview(preview);
  const [replace, setReplace] = useState(false);
  const [choices, setChoices] = useState({ roleMap: {}, rename: false, group: initialGroup, workflow: initialPreview.envelope?.workflow || '' });
  const [groups, setGroups] = useState([]);
  const [reviewing, setReviewing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const latest = useRef(0);
  // The choices the shown review was made with, and the newest ones picked.
  const reviewed = useRef(choices);
  const picked = useRef(choices);
  const [imported, setImported] = useState(false);
  useEffect(() => { processGroupNames().then(setGroups, () => setGroups([])); }, []);
  // Every choice changes what an import stores, so it is reviewed again
  // before Import applies it.
  const choose = async (next) => {
    const merged = { ...picked.current, ...next };
    picked.current = merged;
    setChoices(merged);
    setError('');
    setReviewing(true);
    const seq = ++latest.current;
    try {
      const fresh = await previewImport(bytes, { choices: merged });
      if (seq === latest.current) {
        reviewed.current = merged;
        setPreview(fresh);
      }
    } catch (err) {
      // The review still shows the last choices that previewed; show those.
      if (seq === latest.current) {
        picked.current = reviewed.current;
        setChoices(reviewed.current);
        setError(err?.message || String(err));
      }
    } finally {
      if (seq === latest.current) setReviewing(false);
    }
  };
  const blocked = Boolean(review.refused) || (review.updates > 0 && !replace);
  const submit = async () => {
    if (busy || reviewing || imported || blocked || (review.nothingToDo && !choices.group)) return;
    setBusy(true);
    setError('');
    try {
      const result = await applyImport(bytes, preview, { replace, choices });
      onImported?.(result);
      onClose();
    } catch (err) {
      // A failed apply may have stored some templates, and a stale review
      // returns the current preview: refresh the list and review again.
      onImported?.(null);
      if (err?.body?.preview) {
        setPreview(err.body.preview);
        setReplace(false);
        if (err.body.preview.applied) {
          // Stored, but not pinned: the list refreshes and the import is done.
          onImported?.(err.body.preview);
          setImported(true);
          setError(err.message);
          setBusy(false);
          return;
        }
        setError(`${err.message} The review below is updated; check it again.`);
      } else {
        setError(err?.message || String(err));
      }
      setBusy(false);
    }
  };
  return html`<${Overlay} id="process-workflow-import" dialogClass="modal pwf-import-dialog" overlayClass="pwf-import-modal"
      labelledby="pwf-import-title" onClose=${onClose} blocked=${busy}>
    <h3 id="pwf-import-title">Import workflow: ${review.title}</h3>
    <p class="muted pwf-file">${file}</p>
    ${review.summary && html`<p class="pwf-summary">${review.summary}</p>`}
    ${review.credit && html`<p class="muted" data-pwf-credit>${review.credit}</p>`}
    <p class=${`pwf-signer ${review.trusted ? 'is-trusted' : 'is-untrusted'}`} data-pwf-signer>${review.signer}</p>
    ${review.refused && html`<div class="island-error" role="alert" data-pwf-refused>Import refused: ${review.refused}. Change this in Config › Experimental.</div>`}
    <div class="pwf-section"><h4>Commands it would run</h4>
      ${review.programs.length === 0 ? html`<p class="muted">None. Every step is an agent or you.</p>` : html`<ul class="pwf-commands">${review.programs.map((p) => html`<li key=${p.step} data-pwf-command=${p.step}><code>${p.command}</code> <span class="muted">· ${p.step} · program profile <b>${p.profile || 'default'}</b></span></li>`)}</ul>
      <p class="muted">Nothing runs on import. Starting a run asks you to allow each program profile first.</p>`}
    </div>
    ${review.roleMap.length > 0 ? html`<div class="pwf-section"><h4>Agent roles it asks for</h4><ul class="pwf-roles">${review.roleMap.map((r) => html`<li key=${r.name} data-pwf-role=${r.name}>
        <b>${r.name}</b> <span class="muted">· ${r.steps.join(', ')}</span>
        <label class="pwf-role-map">use <select data-pwf-role-map=${r.name} disabled=${busy} value=${choices.roleMap[r.name] || r.name}
          onChange=${(e) => choose({ roleMap: { ...choices.roleMap, [r.name]: e.currentTarget.value } })}>
          <option value=${r.name}>${r.name} (as named)</option>
          ${review.localRoles.filter((n) => n !== r.name).map((n) => html`<option key=${n} value=${n}>${n}</option>`)}
        </select></label>
        ${!r.known && html`<span class="pwf-role-unknown" data-pwf-role-unknown>nothing here has ${r.target} yet</span>`}</li>`)}</ul>
      <p class="muted">A step waits until a member of the run's group has the role, or its fallback spawns one. Mapping rewrites the imported steps.</p></div>`
    : review.roles.length > 0 && html`<div class="pwf-section"><h4>Agent roles it asks for</h4><ul>${review.roles.map((r) => html`<li key=${r.name}><b>${r.name}</b> <span class="muted">· ${r.steps.join(', ')}</span></li>`)}</ul>
      <p class="muted">A step waits until a member of the run's group has the role, or its fallback spawns one.</p></div>`}
    ${review.humans.length > 0 && html`<div class="pwf-section"><h4>Asks you</h4><p class="muted">${review.humans.join(', ')}</p></div>`}
    ${review.params.length > 0 && html`<div class="pwf-section"><h4>Start inputs</h4><p>${review.params.map((p) => html`<span key=${p.name} class="pwf-param">${p.name}${p.required ? ' *' : ''}</span>`)}</p></div>`}
    ${review.problems.length > 0 && html`<div class="pwf-section pwf-warn" data-pwf-problems><h4>Fails validation</h4><p class="muted">It imports as it is, but cannot run until these are fixed here.</p><ul>${review.problems.map((p, i) => html`<li key=${i}>${p}</li>`)}</ul></div>`}
    ${review.assignees.length > 0 && html`<div class="pwf-section pwf-warn" data-pwf-assignees><h4>Not portable</h4><ul>${review.assignees.map((a) => html`<li key=${a.name}>${a.steps.join(', ')} name the agent <b>${a.name}</b>, which may not exist here. Those steps fall to their profile, or wait until reassigned.</li>`)}</ul></div>`}
    ${review.paths.length > 0 && html`<div class="pwf-section pwf-warn" data-pwf-paths><h4>Paths from another machine</h4><p class="muted">Steps using these may need changing here.</p><ul>${review.paths.map((p, i) => html`<li key=${i}><code>${p.path}</code> <span class="muted">· ${p.where}</span></li>`)}</ul></div>`}
    ${review.envelope && html`<${EnvelopeSection} envelope=${review.envelope} workflow=${choices.workflow} disabled=${busy}
      onPick=${(workflow) => choose({ workflow, roleMap: {} })} onLand=${(performer) => { managementController().openGroupImport(source, { agent: performer.agentKey, role: performer.target }); onClose(); }} />`}
    <div class="pwf-section"><h4>What changes here</h4><ul class="pwf-templates">${review.templates.map((t) => html`<li key=${t.ref} data-pwf-template=${t.id} data-pwf-status=${t.status}><b>${t.name || t.id}</b> <span class=${`pwf-status is-${t.status}`}>${t.label}</span></li>`)}</ul>
      ${(review.updates > 0 || choices.rename) && html`<label class="pwf-replace"><input type="checkbox" data-pwf-rename checked=${choices.rename} disabled=${busy} onChange=${(e) => { setReplace(false); choose({ rename: e.currentTarget.checked }); }} /> Import under new ids (${'<id>-2'}, …) instead of replacing mine</label>`}
      ${review.updates > 0 && !choices.rename && html`<label class="pwf-replace"><input type="checkbox" data-pwf-replace checked=${replace} onChange=${(e) => setReplace(e.currentTarget.checked)} /> Replace my current version of ${review.updates === 1 ? 'that template' : `those ${review.updates} templates`}</label>`}
    </div>
    ${(groups.length > 0 || choices.group) && html`<div class="pwf-section"><h4>Save to a group</h4>
      <label class="pwf-group">Pin it to <select data-pwf-group disabled=${busy} value=${choices.group} onChange=${(e) => choose({ group: e.currentTarget.value })}>
        <option value="">no group (library only)</option>
        ${[...new Set([...(choices.group ? [choices.group] : []), ...groups])].map((g) => html`<option key=${g} value=${g}>${g}</option>`)}
      </select></label></div>`}
    ${error && html`<div class="island-error" role="alert">${error}</div>`}
    <div class="modal-buttons"><button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      <button class="primary" type="button" data-pwf-import disabled=${busy || reviewing || imported || blocked || (review.nothingToDo && !choices.group)} onClick=${submit}>${imported ? 'Imported' : review.refused ? 'Refused' : reviewing ? 'Reviewing…' : review.nothingToDo && !choices.group ? 'Already imported' : busy ? 'Importing…' : 'Import'}</button></div>
  </${Overlay}>`;
}

// WorkflowImportButton picks a .pwf file, previews it and opens the review.
// A file dropped elsewhere (the dock's Processes section) comes in as
// `dropped` and takes the same review.
export function WorkflowImportButton({
  group = '', onImported, dropped = null, from = null, label = '⤒ import workflow', className = 'process-action', id = 'process-workflow-import-btn',
}) {
  const input = useRef(null);
  const [state, setState] = useState(null);
  const [error, setError] = useState('');
  const load = async (picked) => {
    if (!picked) return;
    setError('');
    if (picked.size > (/\.paf$/i.test(picked.name) ? MAX_PAF : MAX_FILE)) {
      setError('That file is too large to be a workflow.');
      return;
    }
    try {
      const bytes = new Uint8Array(await picked.arrayBuffer());
      setState({ file: picked.name, source: picked, bytes, preview: await previewImport(bytes, { choices: { group } }) });
    } catch (err) {
      setError(`Cannot import ${picked.name}: ${err?.message || err}`);
    }
  };
  const choose = (event) => {
    const picked = event.currentTarget.files?.[0];
    event.currentTarget.value = '';
    void load(picked);
  };
  useEffect(() => { void load(dropped); }, [dropped]);
  return html`<span class="pwf-import-entry">
    <input ref=${input} type="file" accept=".pwf,.paf,${PWF_TYPE}" hidden data-pwf-file onChange=${choose} />
    <button id=${id || undefined} class=${className} type="button" title="Import a shared .pwf workflow file, or a .paf carrying one: you review what it runs before anything is stored" onClick=${() => (from ? void load(from) : input.current?.click())}>${label}</button>
    ${error && html`<span class="island-error pwf-inline-error" role="alert">${error}</span>`}
    ${state && html`<${ImportDialog} ...${state} group=${group} onClose=${() => setState(null)} onImported=${onImported} />`}
  </span>`;
}

// WorkflowExportButton downloads one template as a .pwf.
export function WorkflowExportButton({ id, className = 'process-action', disabled = false, title = '' }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const run = async () => {
    setBusy(true);
    setError('');
    try {
      setError(await exportWorkflow(id));
    } catch (err) {
      setError(err?.message || String(err));
    } finally {
      setBusy(false);
    }
  };
  return html`<button class=${className} data-process-action="export" data-id=${id} type="button" disabled=${busy || disabled}
    title=${error || title || 'Download as a .pwf workflow file to share; it carries the templates this one uses'} aria-invalid=${error ? 'true' : undefined}
    onClick=${run}>${busy ? 'exporting…' : error ? 'export failed' : 'share'}</button>`;
}
