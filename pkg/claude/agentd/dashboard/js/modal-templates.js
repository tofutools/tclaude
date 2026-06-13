// modal-templates.js — the templates tab and its modals.
//
// The templates tab listing, the template editor, the instantiate and
// from-group modals, the group-import modal, and the group-context
// modal. Extracted from dashboard.js in the Stage 2 module split.

import { $, $$, esc } from './helpers.js';
import { dashPrefs } from './prefs.js';
// lastSnapshot lives in dashboard.js; refresh() / confirmModal / toast
// in refresh.js. Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, confirmModal, toast, bindBackdropDiscard } from './refresh.js';


// ---- Group templates --------------------------------------------------
//
// A template is a reusable blueprint for a working group: a name, a
// shared default context, and an ordered list of agent specs (name,
// role, descr, task brief, owner flag, permission slugs).
// Instantiating one creates a fresh group and spawns its whole team.
//
// templateEditorEditing holds the original name while editing an
// existing template (the PATCH target); null while creating.
// templateEditorAgents mirrors the editor's agent rows so add/remove
// can re-render the container without losing typed values.
let templateEditorEditing = null;
let templateEditorAgents = [];

function filterTemplates(list, q) {
  if (!q) return list;
  const n = q.toLowerCase();
  return list.filter(t =>
    (t.name || '').toLowerCase().includes(n) ||
    (t.descr || '').toLowerCase().includes(n) ||
    (t.agents || []).some(a =>
      (a.name || '').toLowerCase().includes(n) ||
      (a.role || '').toLowerCase().includes(n)));
}

function renderTemplatesTab() {
  if (!lastSnapshot) return;
  const q = $('#filter-templates').value;
  const all = lastSnapshot.templates || [];
  const list = filterTemplates(all, q);
  const countEl = $('#filter-templates-count');
  if (countEl) countEl.textContent = q ? `${list.length} / ${all.length}` : `${all.length}`;
  const host = $('#templates-list');
  if (!list.length) {
    host.innerHTML = `<div class="template-empty">${all.length
      ? 'No templates match the filter.'
      : 'No templates yet — press <b>+ new template</b> to define one, or <b>⤓ from a group</b> to snapshot an existing group.'}</div>`;
    return;
  }
  host.innerHTML = list.map(templateCardHTML).join('');
}

function templateCardHTML(t) {
  const agents = (t.agents || []).map(a => {
    const owner = a.is_owner ? '<span class="tc-owner" title="group owner">★</span> ' : '';
    const role = a.role ? ` <span class="tc-role">${esc(a.role)}</span>` : '';
    const np = (a.permissions || []).length;
    const perms = np
      ? ` <span class="tc-role" title="${esc((a.permissions || []).join(', '))}">+${np}🔑</span>`
      : '';
    return `<span class="tc-agent">${owner}${esc(a.name)}${role}${perms}</span>`;
  }).join('');
  const n = (t.agents || []).length;
  return `<div class="template-card" data-template="${esc(t.name)}">
    <div class="tc-head">
      <span class="tc-name">${esc(t.name)}</span>
      ${t.descr ? `<span class="tc-descr">${esc(t.descr)}</span>` : ''}
      <span class="tc-count">${n} agent${n === 1 ? '' : 's'}</span>
      <span class="tc-actions">
        <button class="primary" data-tact="instantiate" data-template="${esc(t.name)}" title="Create a group from this template">⎘ instantiate</button>
        <button class="tool" data-tact="edit" data-template="${esc(t.name)}">edit</button>
        <button class="tool" data-tact="delete" data-template="${esc(t.name)}">delete</button>
      </span>
    </div>
    ${agents ? `<div class="tc-agents">${agents}</div>` : ''}
  </div>`;
}

function templatesByName() {
  const m = {};
  for (const t of (lastSnapshot && lastSnapshot.templates) || []) m[t.name] = t;
  return m;
}

function blankTemplateAgent() {
  return { name: '', role: '', descr: '', initial_message: '', is_owner: false, permissions: [] };
}

// ---- Template editor modal --------------------------------------------

function openTemplateEditor(tmpl) {
  templateEditorEditing = tmpl ? tmpl.name : null;
  $('#template-editor-title').textContent =
    tmpl ? `Edit template: ${tmpl.name}` : 'New group template';
  $('#template-editor-name').value = tmpl ? tmpl.name : '';
  $('#template-editor-descr').value = tmpl ? (tmpl.descr || '') : '';
  $('#template-editor-context').value = tmpl ? (tmpl.default_context || '') : '';
  $('#template-editor-error').textContent = '';
  templateEditorAgents = tmpl
    ? (tmpl.agents || []).map(a => ({
        name: a.name || '', role: a.role || '', descr: a.descr || '',
        initial_message: a.initial_message || '', is_owner: !!a.is_owner,
        permissions: (a.permissions || []).slice(),
      }))
    : [blankTemplateAgent()];
  renderEditorAgents();
  $('#template-editor-modal').classList.add('show');
  setTimeout(() => $('#template-editor-name').focus(), 0);
}

function closeTemplateEditor() { $('#template-editor-modal').classList.remove('show'); }

function renderEditorAgents() {
  const slugs = (lastSnapshot && lastSnapshot.slugs) || [];
  $('#template-editor-agents').innerHTML =
    templateEditorAgents.map((a, i) => editorAgentRowHTML(a, i, slugs)).join('');
}

function editorAgentRowHTML(a, idx, slugs) {
  const perms = new Set(a.permissions || []);
  const checks = slugs.map(s =>
    `<label title="${esc(s.description || '')}"><input type="checkbox" class="ta-perm" data-slug="${esc(s.slug)}"${perms.has(s.slug) ? ' checked' : ''} /> ${esc(s.slug)}</label>`
  ).join('');
  return `<div class="template-agent-row" data-idx="${idx}">
    <div class="template-agent-row-head">
      <span class="template-agent-num">Agent ${idx + 1}</span>
      <label class="template-agent-owner" title="Mark this agent as an owner of the instantiated group — a group can have several owners">
        <input type="checkbox" class="ta-owner"${a.is_owner ? ' checked' : ''} /> owner
      </label>
      <button type="button" class="tool ta-remove" title="Remove this agent">✕</button>
    </div>
    <div class="template-agent-grid">
      <input type="text" class="ta-name" placeholder="name (e.g. PO, dev1)" value="${esc(a.name)}" />
      <input type="text" class="ta-role" placeholder="role (e.g. product-owner)" value="${esc(a.role)}" />
    </div>
    <input type="text" class="ta-descr" placeholder="one-line description (dashboard column)" value="${esc(a.descr)}" />
    <textarea class="ta-initmsg" rows="3" placeholder="task brief for this agent — delivered to its inbox at spawn (newlines OK)">${esc(a.initial_message)}</textarea>
    <details class="ta-perms-details">
      <summary>Permissions (<span class="ta-perms-count">${perms.size}</span>)</summary>
      <div class="ta-perms-list">${checks}</div>
    </details>
  </div>`;
}

// scrapeEditorAgents reads the agent rows back into templateEditorAgents
// — called before any add/remove (which re-renders the container) and
// before submit, so typed-but-uncommitted values are never lost.
function scrapeEditorAgents() {
  templateEditorAgents = $$('#template-editor-agents .template-agent-row').map(row => ({
    name: $('.ta-name', row).value.trim(),
    role: $('.ta-role', row).value.trim(),
    descr: $('.ta-descr', row).value.trim(),
    initial_message: $('.ta-initmsg', row).value,
    is_owner: $('.ta-owner', row).checked,
    permissions: $$('.ta-perm', row).filter(c => c.checked).map(c => c.dataset.slug),
  }));
}

async function submitTemplateEditor() {
  scrapeEditorAgents();
  const name = $('#template-editor-name').value.trim();
  const errEl = $('#template-editor-error');
  errEl.textContent = '';
  if (!name) { errEl.textContent = 'template name is required'; return; }
  const payload = {
    name,
    descr: $('#template-editor-descr').value.trim(),
    default_context: $('#template-editor-context').value,
    agents: templateEditorAgents,
  };
  const editing = templateEditorEditing;
  const url = editing ? `/api/templates/${encodeURIComponent(editing)}` : '/api/templates';
  const btn = $('#template-editor-submit');
  btn.disabled = true;
  try {
    const r = await fetch(url, {
      method: editing ? 'PATCH' : 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    if (!r.ok) { errEl.textContent = (await r.text()) || `HTTP ${r.status}`; return; }
    closeTemplateEditor();
    toast(editing ? `template updated: ${name}` : `template created: ${name}`);
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

async function deleteTemplate(name) {
  const ok = await confirmModal({
    title: 'Delete template?',
    body: `Delete the template "${name}"? This removes the blueprint only — any groups already instantiated from it are left untouched.`,
    meta: name,
    okLabel: 'Delete template',
  });
  if (!ok) return;
  try {
    const r = await fetch(`/api/templates/${encodeURIComponent(name)}`, {
      method: 'DELETE', credentials: 'same-origin',
    });
    if (!r.ok && r.status !== 204) { toast((await r.text()) || `HTTP ${r.status}`, true); return; }
    toast(`template deleted: ${name}`);
    refresh();
  } catch (err) {
    toast((err && err.message) || String(err), true);
  }
}

// ---- Instantiate-from-template modal ----------------------------------

function openInstantiateModal(presetName) {
  const templates = (lastSnapshot && lastSnapshot.templates) || [];
  if (!templates.length) {
    toast('no templates yet — define one in the Templates tab first', true);
    return;
  }
  const sel = $('#template-instantiate-template');
  sel.innerHTML = templates.map(t =>
    `<option value="${esc(t.name)}">${esc(t.name)}</option>`).join('');
  if (presetName && templates.some(t => t.name === presetName)) sel.value = presetName;
  $('#template-instantiate-group').value = '';
  $('#template-instantiate-task').value = '';
  $('#template-instantiate-cwd').value = '';
  $('#template-instantiate-error').textContent = '';
  renderInstantiatePreview();
  $('#template-instantiate-modal').classList.add('show');
  setTimeout(() => $('#template-instantiate-group').focus(), 0);
}

function closeInstantiateModal() { $('#template-instantiate-modal').classList.remove('show'); }

// renderInstantiatePreview paints the live "final agent names" list as
// the human types the group name — agent "PO" shows as "<group>-PO".
function renderInstantiatePreview() {
  const t = templatesByName()[$('#template-instantiate-template').value];
  const prefix = $('#template-instantiate-group').value.trim();
  const host = $('#template-instantiate-preview');
  const agents = (t && t.agents) || [];
  if (!agents.length) {
    host.innerHTML = '<span class="tp-empty">this template has no agents</span>';
    return;
  }
  const shown = prefix || '‹group›';
  host.innerHTML = agents.map(a => {
    const owner = a.is_owner ? '<span class="tp-owner" title="group owner">★ owner</span>' : '';
    const np = (a.permissions || []).length;
    const meta = [a.role ? esc(a.role) : '', np ? `+${np}🔑` : '', owner]
      .filter(Boolean).join(' · ');
    return `<div class="tp-row"><span class="tp-name">${esc(shown)}-${esc(a.name)}</span>`
      + (meta ? ` <span class="tp-meta">${meta}</span>` : '') + `</div>`;
  }).join('');
}

async function submitInstantiate() {
  const tmplName = $('#template-instantiate-template').value;
  const groupName = $('#template-instantiate-group').value.trim();
  const errEl = $('#template-instantiate-error');
  errEl.textContent = '';
  if (!tmplName) { errEl.textContent = 'pick a template'; return; }
  if (!groupName) { errEl.textContent = 'group name is required'; return; }
  const payload = {
    group_name: groupName,
    task: $('#template-instantiate-task').value,
    cwd: $('#template-instantiate-cwd').value.trim(),
  };
  const btn = $('#template-instantiate-submit');
  btn.disabled = true;
  btn.textContent = 'Spawning…';
  try {
    const r = await fetch(`/api/templates/${encodeURIComponent(tmplName)}/instantiate`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const txt = await r.text();
    if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
    let resp = {};
    try { resp = JSON.parse(txt); } catch (_) {}
    closeInstantiateModal();
    const failed = resp.failed || 0;
    toast(failed
      ? `group ${groupName}: spawned ${resp.spawned || 0}, ${failed} failed — check the group`
      : `group ${groupName}: spawned ${resp.spawned || 0} agent${resp.spawned === 1 ? '' : 's'}`,
      failed > 0);
    try { dashPrefs.setItem('tclaude.dash.group.' + groupName, '1'); } catch (_) {}
    // Jump to the Groups tab so the freshly-spawned group is visible.
    const gbtn = $$('nav button').find(b => b.dataset.tab === 'groups');
    if (gbtn) gbtn.click();
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
    btn.textContent = 'Create & spawn';
  }
}

// ---- Save-group-as-template modal -------------------------------------

function openFromGroupModal() {
  const groups = ((lastSnapshot && lastSnapshot.groups) || []).map(g => g.name);
  if (!groups.length) { toast('no groups to snapshot', true); return; }
  const sel = $('#template-from-group-group');
  sel.innerHTML = groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
  $('#template-from-group-name').value = '';
  $('#template-from-group-error').textContent = '';
  $('#template-from-group-modal').classList.add('show');
  setTimeout(() => $('#template-from-group-name').focus(), 0);
}

function closeFromGroupModal() { $('#template-from-group-modal').classList.remove('show'); }

async function submitFromGroup() {
  const group = $('#template-from-group-group').value;
  const name = $('#template-from-group-name').value.trim();
  const errEl = $('#template-from-group-error');
  errEl.textContent = '';
  if (!group) { errEl.textContent = 'pick a group'; return; }
  if (!name) { errEl.textContent = 'template name is required'; return; }
  const btn = $('#template-from-group-submit');
  btn.disabled = true;
  try {
    const r = await fetch('/api/templates/from-group', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ group, template_name: name }),
    });
    const txt = await r.text();
    if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
    let tmpl = null;
    try { tmpl = JSON.parse(txt); } catch (_) {}
    closeFromGroupModal();
    toast(`template created from ${group}: ${name}`);
    refresh();
    // Open the editor on the fresh template so the human can fill in
    // per-agent task briefs (from-group leaves those blank).
    if (tmpl) openTemplateEditor(tmpl);
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

function bindTemplatesUI() {
  // Entry points: Templates tab + the Groups tab's "⎘ from template".
  $('#template-create-open').addEventListener('click', () => openTemplateEditor(null));
  $('#template-from-group-open').addEventListener('click', openFromGroupModal);
  $('#group-from-template-open').addEventListener('click', () => openInstantiateModal(null));

  // Template-card actions (delegated — the list re-renders every poll).
  // data-tact (not data-act) keeps these off the global row-action bus.
  $('#templates-list').addEventListener('click', e => {
    const btn = e.target.closest('[data-tact]');
    if (!btn) return;
    const name = btn.dataset.template;
    if (btn.dataset.tact === 'instantiate') openInstantiateModal(name);
    else if (btn.dataset.tact === 'edit') {
      const t = templatesByName()[name];
      if (t) openTemplateEditor(t);
    } else if (btn.dataset.tact === 'delete') deleteTemplate(name);
  });

  // Editor modal.
  $('#template-editor-cancel').addEventListener('click', closeTemplateEditor);
  $('#template-editor-submit').addEventListener('click', submitTemplateEditor);
  $('#template-editor-add-agent').addEventListener('click', () => {
    scrapeEditorAgents();
    templateEditorAgents.push(blankTemplateAgent());
    renderEditorAgents();
  });
  // Delegated handlers on the (re-rendered) agent container.
  $('#template-editor-agents').addEventListener('click', e => {
    const rm = e.target.closest('.ta-remove');
    if (!rm) return;
    const row = rm.closest('.template-agent-row');
    scrapeEditorAgents();
    templateEditorAgents.splice(parseInt(row.dataset.idx, 10), 1);
    renderEditorAgents();
  });
  // Keep each agent row's permission count in sync as boxes toggle.
  // Owner is a plain per-agent checkbox — a group can have several
  // owners, so there is no single-select enforcement.
  $('#template-editor-agents').addEventListener('change', e => {
    if (e.target.classList.contains('ta-perm')) {
      const row = e.target.closest('.template-agent-row');
      $('.ta-perms-count', row).textContent =
        $$('.ta-perm', row).filter(c => c.checked).length;
    }
  });
  bindBackdropDiscard('template-editor-modal', closeTemplateEditor);

  // Instantiate modal.
  $('#template-instantiate-cancel').addEventListener('click', closeInstantiateModal);
  $('#template-instantiate-submit').addEventListener('click', submitInstantiate);
  $('#template-instantiate-template').addEventListener('change', renderInstantiatePreview);
  $('#template-instantiate-group').addEventListener('input', renderInstantiatePreview);
  bindBackdropDiscard('template-instantiate-modal', closeInstantiateModal);

  // From-group modal.
  $('#template-from-group-cancel').addEventListener('click', closeFromGroupModal);
  $('#template-from-group-submit').addEventListener('click', submitFromGroup);
  bindBackdropDiscard('template-from-group-modal', closeFromGroupModal);

}

// ---- Import-group modal ------------------------------------------------
//
// The ⤒ import button uploads a .zip produced by ⤓ export and
// recreates the group on this machine. Browsers cannot stream a raw
// body the way the CLI does, so the file rides in a multipart form to
// POST /api/groups/import.
//
// Before committing, the modal POSTs the same archive to
// /api/groups/import/inspect — a server-side dry run that returns the
// manifest summary plus a collision report (does the group name exist
// here? which conv-ids will be remapped to "-i-N" copies?) without
// writing anything. The Import button stays disabled until that
// preview is clean, so an import is never a blind action; a malformed
// or unsupported archive surfaces its error in the preview and blocks
// the confirm outright.

let giInspectSeq = 0;        // monotonic — stale inspect responses are dropped
let giLastInspection = null; // last successful inspection JSON, or null
let giAsDebounce = null;     // debounce timer for the "Import as" field

function openGroupImportModal() {
  $('#group-import-file').value = '';
  $('#group-import-into').value = '';
  $('#group-import-as').value = '';
  $('#group-import-error').textContent = '';
  giLastInspection = null;
  giInspectSeq++; // invalidate any inspect still in flight from a prior open
  const prev = $('#group-import-preview');
  prev.style.display = 'none';
  prev.innerHTML = '';
  $('#group-import-submit').disabled = true;
  $('#group-import-submit').textContent = 'Import';
  $('#group-import-modal').classList.add('show');
  setTimeout(() => $('#group-import-file').focus(), 0);
}

function closeGroupImportModal() {
  $('#group-import-modal').classList.remove('show');
  if (giAsDebounce) { clearTimeout(giAsDebounce); giAsDebounce = null; }
}

// groupImportInspect uploads the picked .zip to the dry-run endpoint
// and renders the preview. Each call bumps giInspectSeq; a response is
// applied only while it is still the latest request, so a fast re-pick
// or an "Import as" edit can't let a stale preview win.
async function groupImportInspect() {
  const fileEl = $('#group-import-file');
  const file = fileEl.files && fileEl.files[0];
  if (!file) {
    giLastInspection = null;
    $('#group-import-preview').style.display = 'none';
    $('#group-import-error').textContent = '';
    refreshGroupImportSubmitState();
    return;
  }
  const seq = ++giInspectSeq;
  const fd = new FormData();
  fd.append('archive', file);
  const asName = $('#group-import-as').value.trim();
  if (asName) fd.append('as', asName);

  const prev = $('#group-import-preview');
  prev.style.display = 'flex';
  prev.innerHTML = '<div class="gi-head">Inspecting archive…</div>';
  $('#group-import-error').textContent = '';
  $('#group-import-submit').disabled = true;

  let r, body;
  try {
    r = await fetch('/api/groups/import/inspect', {
      method: 'POST', credentials: 'same-origin', body: fd,
    });
    body = await r.json().catch(() => null);
  } catch (err) {
    if (seq !== giInspectSeq) return;
    giLastInspection = null;
    renderGroupImportPreviewError((err && err.message) || String(err));
    refreshGroupImportSubmitState();
    return;
  }
  if (seq !== giInspectSeq) return; // a newer inspect superseded this one

  if (!r.ok) {
    // Malformed / corrupt / unsupported-version archive — block confirm.
    giLastInspection = null;
    renderGroupImportPreviewError((body && body.error) || ('HTTP ' + r.status));
    refreshGroupImportSubmitState();
    return;
  }
  giLastInspection = body;
  renderGroupImportPreview();
}

function renderGroupImportPreviewError(msg) {
  const prev = $('#group-import-preview');
  prev.style.display = 'flex';
  prev.innerHTML =
    '<div class="gi-head">Archive</div>' +
    '<div class="gi-verdict gi-bad">✗ ' + esc(msg) + '</div>' +
    '<div class="gi-bad">This file is not an importable group archive — pick a .zip produced by the ⤓ export button.</div>';
}

// renderGroupImportPreview paints the manifest summary + collision
// report + verdict from giLastInspection. Also re-run when "Into dir"
// changes, since the verdict line depends on it.
function renderGroupImportPreview() {
  const insp = giLastInspection;
  const prev = $('#group-import-preview');
  if (!insp) {
    prev.style.display = 'none';
    refreshGroupImportSubmitState();
    return;
  }
  prev.style.display = 'flex';

  const row = (k, v, cls) =>
    '<div class="gi-row"><span class="gi-k">' + esc(k) + '</span>' +
    '<span class="gi-v ' + (cls || '') + '">' + esc(v) + '</span></div>';

  let h = '<div class="gi-head">Archive contents</div>';
  h += row('Source group', insp.source_group || '(unnamed)');
  h += row('Agents', String(insp.agent_count));
  h += row('Messages', String(insp.message_count));
  let convs = insp.conv_count + ' conversation' + (insp.conv_count === 1 ? '' : 's');
  if (insp.missing_convs > 0) convs += ' (' + insp.missing_convs + ' with no .jsonl content)';
  h += row('Conversations', convs);
  if (insp.source_os || insp.source_home) {
    h += row('Source machine',
      (insp.source_os || '?') + (insp.source_home ? ', home ' + insp.source_home : ''));
  }
  if (insp.exported_at) h += row('Exported', insp.exported_at);
  h += row('Format version', 'v' + insp.format_version + ' — supported', 'gi-ok');

  h += '<div class="gi-sep gi-head">Collisions on this machine</div>';
  const collisions = insp.conv_collisions || [];
  if (collisions.length === 0) {
    h += '<div class="gi-ok">✓ No conv-id collisions — every conversation id is preserved.</div>';
  } else {
    h += '<div class="gi-warn">⚠ ' + collisions.length + ' conversation' +
      (collisions.length === 1 ? '' : 's') +
      ' already exist locally — each is imported as a fresh copy, its agent retitled “-i-N”:</div>';
    h += '<ul class="gi-collide-list">';
    collisions.forEach((c) => {
      h += '<li>' + esc(c.title || c.conv_id) +
        ' <span class="gi-k">(' + esc((c.conv_id || '').slice(0, 8)) + ')</span></li>';
    });
    h += '</ul>';
  }

  // Verdict — exactly what enables or blocks the Import button.
  h += '<div class="gi-sep"></div>';
  const into = $('#group-import-into').value.trim();
  if (!insp.target_name_valid) {
    h += '<div class="gi-verdict gi-bad">✗ Invalid group name “' + esc(insp.target_name) +
      '”: ' + esc(insp.target_name_error || '') + '</div>';
  } else if (insp.group_name_taken) {
    h += '<div class="gi-verdict gi-bad">✗ A group named “' + esc(insp.target_name) +
      '” already exists here. Fill “Import as” with a free name.</div>';
  } else if (!into) {
    h += '<div class="gi-verdict gi-warn">⚠ Fill “Into dir” with a target directory to enable the import.</div>';
  } else {
    h += '<div class="gi-verdict gi-ok">✓ Ready — ' + insp.agent_count + ' agent' +
      (insp.agent_count === 1 ? '' : 's') + ' will be imported into group “' +
      esc(insp.target_name) + '”.</div>';
  }
  prev.innerHTML = h;
  refreshGroupImportSubmitState();
}

// refreshGroupImportSubmitState enables Import only when the latest
// dry run is clean: archive parsed, target name valid and free, and a
// target directory has been entered.
function refreshGroupImportSubmitState() {
  const insp = giLastInspection;
  const into = $('#group-import-into').value.trim();
  const ok = !!insp && insp.target_name_valid && !insp.group_name_taken && into !== '';
  $('#group-import-submit').disabled = !ok;
}

async function submitGroupImport() {
  const fileEl = $('#group-import-file');
  const file = fileEl.files && fileEl.files[0];
  const into = $('#group-import-into').value.trim();
  const asName = $('#group-import-as').value.trim();
  const errEl = $('#group-import-error');
  errEl.textContent = '';
  if (!file) { errEl.textContent = 'pick a .zip archive first'; return; }
  if (!into) { errEl.textContent = 'a target directory (Into dir) is required'; return; }

  const fd = new FormData();
  fd.append('archive', file);
  fd.append('into', into);
  if (asName) fd.append('as', asName);

  const submitBtn = $('#group-import-submit');
  submitBtn.disabled = true;
  submitBtn.textContent = 'Importing…';
  try {
    const r = await fetch('/api/groups/import', {
      method: 'POST', credentials: 'same-origin', body: fd,
    });
    const body = await r.json().catch(() => null);
    if (!r.ok) {
      // The import is transactional — a failure wrote nothing at all.
      errEl.textContent = 'Import failed: ' + ((body && body.error) || ('HTTP ' + r.status)) +
        ' — nothing was written. The import is all-or-nothing, so the group, its agents and' +
        ' conversations are exactly as before. Adjust the fields and try again.';
      return;
    }
    closeGroupImportModal();
    let summary = 'Imported group "' + body.group + '" — ' +
      body.agent_count + ' agent(s), ' + body.message_count + ' message(s)';
    const remaps = body.conv_remaps ? Object.keys(body.conv_remaps).length : 0;
    if (remaps > 0) summary += ' (' + remaps + ' conv-id(s) remapped to fresh copies)';
    const warnings = body.file_warnings || [];
    if (warnings.length > 0) {
      toast(summary + ' — ' + warnings.length + ' file warning(s); see the daemon log', true);
    } else {
      toast(summary);
    }
    // Show the imported group expanded on the next render.
    try { dashPrefs.setItem('tclaude.dash.group.' + body.group, '1'); } catch (_) {}
    refresh();
  } catch (err) {
    errEl.textContent = 'Import failed: ' + ((err && err.message) || String(err)) +
      ' — nothing was written.';
  } finally {
    submitBtn.textContent = 'Import';
    refreshGroupImportSubmitState();
  }
}

function bindGroupImportModal() {
  $('#group-import-open').addEventListener('click', openGroupImportModal);
  $('#group-import-cancel').addEventListener('click', closeGroupImportModal);
  $('#group-import-submit').addEventListener('click', submitGroupImport);
  // Picking (or changing) the file re-runs the dry-run preview.
  $('#group-import-file').addEventListener('change', groupImportInspect);
  // "Into dir" does not affect the archive analysis — collisions are
  // group-name + conv-id — so it only re-evaluates the verdict locally.
  $('#group-import-into').addEventListener('input', renderGroupImportPreview);
  // "Import as" DOES change the collision check (a different target
  // name), so editing it re-runs inspect — debounced so a burst of
  // keystrokes collapses into one request.
  $('#group-import-as').addEventListener('input', () => {
    if (giAsDebounce) clearTimeout(giAsDebounce);
    giAsDebounce = setTimeout(groupImportInspect, 350);
  });
  bindBackdropDiscard('group-import-modal', closeGroupImportModal);
  $('#group-import-modal').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' &&
        (e.target.id === 'group-import-into' || e.target.id === 'group-import-as') &&
        !$('#group-import-submit').disabled) {
      e.preventDefault();
      submitGroupImport();
    }
  });
}

// ---- Group startup-context modal ---------------------------------------
//
// Edits a group's default_context — the shared block of guidance
// injected into every agent spawned into the group. The cwd chip
// edits inline; context is multi-line so it gets a modal textarea.
// Save PATCHes /api/groups/{name} with {default_context}.

// groupDefaultContext looks up a group's startup context from the
// latest snapshot. "" when the group is unknown or has none.
function groupDefaultContext(groupName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const g = groups.find(x => x.name === groupName);
  return (g && g.default_context) || '';
}

// The group whose context the modal is currently editing.
let groupContextModalGroup = '';

function openGroupContextModal(groupName) {
  groupContextModalGroup = groupName;
  $('#group-context-text').value = groupDefaultContext(groupName);
  $('#group-context-error').textContent = '';
  const meta = $('#group-context-meta');
  meta.textContent = `group: ${groupName}`;
  meta.style.display = '';
  $('#group-context-modal').classList.add('show');
  setTimeout(() => $('#group-context-text').focus(), 0);
}

function closeGroupContextModal() {
  $('#group-context-modal').classList.remove('show');
  groupContextModalGroup = '';
}

async function submitGroupContext() {
  const group = groupContextModalGroup;
  if (!group) { closeGroupContextModal(); return; }
  const context = $('#group-context-text').value.trim();
  const errEl = $('#group-context-error');
  errEl.textContent = '';
  const submitBtn = $('#group-context-submit');
  submitBtn.disabled = true;
  try {
    const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
      method: 'PATCH', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ default_context: context }),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    closeGroupContextModal();
    toast(context ? `${group}: startup context updated` : `${group}: startup context cleared`);
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submitBtn.disabled = false;
  }
}

function bindGroupContextModal() {
  $('#group-context-cancel').addEventListener('click', closeGroupContextModal);
  $('#group-context-submit').addEventListener('click', submitGroupContext);
  bindBackdropDiscard('group-context-modal', closeGroupContextModal);
}

export {
  renderTemplatesTab, bindTemplatesUI, bindGroupImportModal,
  openGroupContextModal, bindGroupContextModal, groupDefaultContext,
};
