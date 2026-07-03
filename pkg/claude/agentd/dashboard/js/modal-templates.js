// modal-templates.js — the templates tab and its modals.
//
// The templates tab listing, the template editor, the instantiate and
// from-group modals, the group-import modal, and the group-context
// modal. Extracted from dashboard.js in the Stage 2 module split.

import { $, $$, esc } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { recordGroupInteraction } from './last-group.js';
// wizWord swaps the template vocabulary for 🧙 wizard mode: a template is a
// SUMMONING CIRCLE — chalk a new one, trace a party into one, cast one to
// summon the whole party. Static HTML spots swap via the .tpl-word-regular /
// .tpl-word-wizard span pair in dashboard.html; the JS-rendered spots (cards,
// empty states, the editor title) swap here, like modal-profiles.js does.
import { wizWord } from './slop.js';
// lastSnapshot lives in dashboard.js; refresh() / confirmModal / toast
// in refresh.js. Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, confirmModal, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';


// ---- Group templates --------------------------------------------------
//
// A template is a reusable blueprint for a working group: a name, a
// shared default context, and an ordered list of agent specs (name,
// role, descr, task brief, owner flag, permission slugs).
// Instantiating one creates a fresh group and spawns its whole team.
//
// templateEditorEditing holds the original name while editing an
// existing template (the PATCH target); null while creating.
// templateEditorAgents / templateEditorPattern mirror the editor's
// agent and work-pattern rows so add/remove/reorder can re-render the
// containers without losing typed values.
let templateEditorEditing = null;
let templateEditorAgents = [];
let templateEditorPattern = [];

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
      ? wizWord('No templates match the filter.', 'No circles match the filter.')
      : wizWord(
        'No templates yet — press <b>+ new template</b> to define one, or <b>⤓ from a group</b> to snapshot an existing group.',
        'No summoning circles chalked yet — press <b>+ chalk a new circle</b> to inscribe one, or <b>⤓ trace a party</b> to copy an existing party’s shape.')}</div>`;
    return;
  }
  host.innerHTML = list.map(templateCardHTML).join('');
}

// ---- Templates… management overlay ------------------------------------
//
// The former Templates tab, now reached from the Groups tab's ⚙ cog.
// Opening paints the list once immediately; the 2s auto-refresh keeps it
// live afterwards (the overlay is a .manage-overlay, so it does not
// suspend the refresh — see dashboard.css). Its child modals (editor /
// instantiate / from-group) open on top and DO suspend.
function openTemplatesManageModal() {
  $('#templates-manage-modal').classList.add('show');
  renderTemplatesTab();
  setTimeout(() => $('#filter-templates').focus(), 0);
}

function closeTemplatesManageModal() { $('#templates-manage-modal').classList.remove('show'); }

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
      <span class="tc-count">${n} ${wizWord('agent', 'familiar')}${n === 1 ? '' : 's'}</span>
      ${(t.work_pattern || []).length ? `<span class="tc-count" title="${wizWord('work pattern — ordered briefing messages delivered after the team spawns', 'rite of command — ordered whispers delivered once the party stands')}">⇶ ${(t.work_pattern || []).length}-step ${wizWord('pattern', 'rite')}</span>` : ''}
      <span class="tc-actions">
        <button class="primary" data-tact="instantiate" data-template="${esc(t.name)}" title="${wizWord('Create a group from this template', 'Cast this circle — summon a fresh party from it')}">${wizWord('⎘ instantiate', '🕯 cast')}</button>
        <button class="tool" data-tact="edit" data-template="${esc(t.name)}">edit</button>
        <button class="tool" data-tact="delete" data-template="${esc(t.name)}">delete</button>
      </span>
    </div>
    ${agents ? `<div class="tc-agents">${agents}</div>` : ''}
  </div>`;
}

function templatesByName() {
  // Null prototype: template names are human-typed, and a plain {} would
  // false-positive existence checks on Object.prototype keys — a template
  // named "constructor" or "toString" must only exist if actually saved.
  const m = Object.create(null);
  for (const t of (lastSnapshot && lastSnapshot.templates) || []) m[t.name] = t;
  return m;
}

function blankTemplateAgent() {
  return { name: '', role: '', descr: '', initial_message: '', is_owner: false, permissions: [] };
}

// ---- Template editor modal --------------------------------------------

function openTemplateEditor(tmpl) {
  templateEditorEditing = tmpl ? tmpl.name : null;
  $('#template-editor-title').textContent = tmpl
    ? wizWord(`Edit template: ${tmpl.name}`, `Redraw the circle: ${tmpl.name}`)
    : wizWord('New group template', 'Chalk a new summoning circle');
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
  templateEditorPattern = tmpl
    ? (tmpl.work_pattern || []).map(e => ({ send_to: e.send_to || 'all', value: e.value || '' }))
    : [];
  renderEditorAgents();
  renderEditorPattern();
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
      <span class="template-agent-num">${wizWord('Agent', 'Familiar')} ${idx + 1}</span>
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

// ---- Work-pattern rows (JOH-336) --------------------------------------
//
// The template's default work pattern: an ORDERED list of routed
// briefing messages delivered after the whole roster has spawned — each
// step to one roster agent by name, or to every member ("all"). {{task}}
// in a step's text is replaced with the per-instantiation task. The rows
// reuse the agent-row panel chrome (.template-agent-row) so the wizard
// re-skin covers them for free.

function renderEditorPattern() {
  const names = templateEditorAgents.map(a => (a.name || '').trim()).filter(Boolean);
  $('#template-editor-pattern').innerHTML =
    templateEditorPattern.map((e, i) => patternRowHTML(e, i, names)).join('');
}

function patternRowHTML(e, idx, agentNames) {
  const known = ['all', ...agentNames];
  // A stale target (its agent was renamed/removed) stays selectable —
  // flagged — so the typed text isn't silently rerouted; the server
  // rejects it with a clear error if submitted as-is.
  const stale = e.send_to && !known.includes(e.send_to);
  const options =
    (stale ? `<option value="${esc(e.send_to)}" selected>⚠ ${esc(e.send_to)} (no such agent)</option>` : '')
    + known.map(n => {
      const label = n === 'all' ? wizWord('all members', 'the whole party') : n;
      return `<option value="${esc(n)}"${n === e.send_to ? ' selected' : ''}>${esc(label)}</option>`;
    }).join('');
  return `<div class="template-agent-row template-pattern-row" data-idx="${idx}">
    <div class="template-agent-row-head">
      <span class="template-agent-num">${wizWord('Step', 'Verse')} ${idx + 1}</span>
      <label class="template-pattern-sendto">${wizWord('send to', 'whisper to')}
        <select class="tw-sendto">${options}</select>
      </label>
      <button type="button" class="tool tw-up" title="Move this step up">↑</button>
      <button type="button" class="tool tw-down" title="Move this step down">↓</button>
      <button type="button" class="tool tw-remove" title="Remove this step">✕</button>
    </div>
    <textarea class="tw-value" rows="2" placeholder="briefing message delivered after the whole team is up — {{task}} is replaced with the dispatch task (newlines OK)">${esc(e.value)}</textarea>
  </div>`;
}

// scrapeEditorPattern reads the pattern rows back into
// templateEditorPattern — same never-lose-typed-values contract as
// scrapeEditorAgents.
function scrapeEditorPattern() {
  templateEditorPattern = $$('#template-editor-pattern .template-pattern-row').map(row => ({
    send_to: $('.tw-sendto', row).value,
    value: $('.tw-value', row).value,
  }));
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
  scrapeEditorPattern();
  const name = $('#template-editor-name').value.trim();
  const errEl = $('#template-editor-error');
  errEl.textContent = '';
  if (!name) { errEl.textContent = 'template name is required'; return; }
  const payload = {
    name,
    descr: $('#template-editor-descr').value.trim(),
    default_context: $('#template-editor-context').value,
    agents: templateEditorAgents,
    work_pattern: templateEditorPattern,
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
    toast(wizWord(
      'no templates yet — define one via the Groups cog ⚙ → ⧉ templates… first',
      'no summoning circles yet — chalk one via the Groups cog ⚙ → ⧉ circles… first'), true);
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
    host.innerHTML = `<span class="tp-empty">${wizWord('this template has no agents', 'this circle names no familiars')}</span>`;
    return;
  }
  const shown = prefix || wizWord('‹group›', '‹party›');
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
    // Instantiate can be launched from inside the Templates… overlay (a
    // card's ⎘ instantiate) — close it too so the Groups tab we jump to
    // below isn't hidden behind the panel. No-op when launched from the
    // cog's standalone "from template" shortcut.
    closeTemplatesManageModal();
    const failed = resp.failed || 0;
    toast(failed
      ? `group ${groupName}: spawned ${resp.spawned || 0}, ${failed} failed — check the group`
      : `group ${groupName}: spawned ${resp.spawned || 0} agent${resp.spawned === 1 ? '' : 's'}`,
      failed > 0);
    // Work-pattern outcome gets its own toast — a silently-skipped
    // kick-off briefing must not hide behind a happy spawn count.
    const perrs = resp.pattern_errors || [];
    if (perrs.length) {
      toast(`⚠ work pattern: ${perrs.length} step${perrs.length === 1 ? '' : 's'} not sent — ${perrs[0]}`, true);
    } else if (resp.pattern_delivered) {
      toast(`work pattern: ${resp.pattern_delivered} briefing${resp.pattern_delivered === 1 ? '' : 's'} sent`);
    }
    try { dashPrefs.setItem('tclaude.dash.group.' + groupName, '1'); } catch (_) {}
    recordGroupInteraction(groupName);
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

// openFromGroupModal opens the snapshot dialog. With a presetGroup (the
// per-group ⚙ "save as template…" action) that group is preselected and
// the template name is prefilled with the group's own name — selected so
// one keystroke replaces it; the API 409s if the name is already taken.
function openFromGroupModal(presetGroup) {
  const groups = ((lastSnapshot && lastSnapshot.groups) || []).map(g => g.name);
  if (!groups.length) { toast('no groups to snapshot', true); return; }
  const sel = $('#template-from-group-group');
  sel.innerHTML = groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
  const preset = presetGroup && groups.includes(presetGroup);
  if (preset) sel.value = presetGroup;
  $('#template-from-group-name').value = preset ? presetGroup : '';
  $('#template-from-group-error').textContent = '';
  refreshFromGroupUpdateState();
  $('#template-from-group-modal').classList.add('show');
  setTimeout(() => {
    const inp = $('#template-from-group-name');
    inp.focus();
    if (preset) inp.select();
  }, 0);
}

function closeFromGroupModal() { $('#template-from-group-modal').classList.remove('show'); }

// refreshFromGroupUpdateState flips the from-group dialog between its
// create and update modes as the human types: when the typed template
// name already exists, submitting re-snapshots that template IN PLACE
// (roster/owners/permissions/context re-traced from the group; curated
// per-agent briefs kept on name match), so the note and the submit
// label say so before anything is overwritten. .tfg-updating is a MODE
// flag on the modal (like the cron dialog's .cron-editing) — the wizard
// lever's Re-trace copy keys on it in pure CSS.
function refreshFromGroupUpdateState() {
  const name = $('#template-from-group-name').value.trim();
  const updating = !!templatesByName()[name];
  $('#template-from-group-modal').classList.toggle('tfg-updating', updating);
  const note = $('#template-from-group-update-note');
  note.style.display = updating ? '' : 'none';
  note.textContent = updating
    ? wizWord(
      `⚠ A template “${name}” already exists — saving re-snapshots it in place: roles, owners, permissions and context are re-traced from the group; curated per-agent task briefs are kept for matching agents.`,
      `⚠ A circle “${name}” is already chalked — tracing redraws it in place: roles, owners, powers and lore are re-traced from the party; curated familiar briefs are kept for matching names.`)
    : '';
  $('#template-from-group-submit').textContent =
    updating ? 'Update template' : 'Save as template';
}

async function submitFromGroup() {
  const group = $('#template-from-group-group').value;
  const name = $('#template-from-group-name').value.trim();
  const errEl = $('#template-from-group-error');
  errEl.textContent = '';
  if (!group) { errEl.textContent = 'pick a group'; return; }
  if (!name) { errEl.textContent = 'template name is required'; return; }
  // The mode the dialog showed the human — kept in lockstep with the
  // typed name by refreshFromGroupUpdateState, so update is only ever
  // sent after the "will update in place" note was visible. The server
  // fails closed either way (409 without update on a taken name).
  const updating = $('#template-from-group-modal').classList.contains('tfg-updating');
  const btn = $('#template-from-group-submit');
  btn.disabled = true;
  try {
    const r = await fetch('/api/templates/from-group', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ group, template_name: name, update: updating }),
    });
    const txt = await r.text();
    if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
    let tmpl = null;
    try { tmpl = JSON.parse(txt); } catch (_) {}
    closeFromGroupModal();
    if (tmpl && tmpl.updated) {
      const kept = (tmpl.briefs_kept || []).length;
      const added = (tmpl.added || []).length;
      const removed = (tmpl.removed || []).length;
      toast(`template updated from ${group}: ${name}`
        + ` (briefs kept: ${kept}, added: ${added}, removed: ${removed})`);
    } else {
      toast(`template created from ${group}: ${name}`);
    }
    refresh();
    // Open the editor on the fresh template so the human can fill in
    // per-agent task briefs (from-group leaves new agents' blank).
    if (tmpl) openTemplateEditor(tmpl);
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

function bindTemplatesUI() {
  // Entry points: the Groups cog's "⧉ templates…" management overlay, its
  // "+ new template" / "⤓ from a group" buttons, and the cog's standalone
  // "⎘ from template" instantiate shortcut.
  $('#templates-manage-open').addEventListener('click', openTemplatesManageModal);
  $('#templates-manage-close').addEventListener('click', closeTemplatesManageModal);
  bindManageOverlayDismiss('templates-manage-modal', closeTemplatesManageModal);
  $('#template-create-open').addEventListener('click', () => openTemplateEditor(null));
  $('#template-from-group-open').addEventListener('click', () => openFromGroupModal(null));
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
    scrapeEditorPattern();
    templateEditorAgents.push(blankTemplateAgent());
    renderEditorAgents();
    // The roster changed — refresh the pattern rows' send-to options.
    renderEditorPattern();
  });
  // Delegated handlers on the (re-rendered) agent container.
  $('#template-editor-agents').addEventListener('click', e => {
    const rm = e.target.closest('.ta-remove');
    if (!rm) return;
    const row = rm.closest('.template-agent-row');
    scrapeEditorAgents();
    scrapeEditorPattern();
    templateEditorAgents.splice(parseInt(row.dataset.idx, 10), 1);
    renderEditorAgents();
    renderEditorPattern();
  });
  // Keep each agent row's permission count in sync as boxes toggle.
  // Owner is a plain per-agent checkbox — a group can have several
  // owners, so there is no single-select enforcement.
  // A committed agent-name edit (change fires on blur) also refreshes
  // the work-pattern rows' send-to options to the new roster names.
  $('#template-editor-agents').addEventListener('change', e => {
    if (e.target.classList.contains('ta-perm')) {
      const row = e.target.closest('.template-agent-row');
      $('.ta-perms-count', row).textContent =
        $$('.ta-perm', row).filter(c => c.checked).length;
    } else if (e.target.classList.contains('ta-name')) {
      scrapeEditorAgents();
      scrapeEditorPattern();
      renderEditorPattern();
    }
  });
  // Work-pattern rows: add / remove / reorder (delegated — the container
  // re-renders on every mutation).
  $('#template-editor-add-pattern').addEventListener('click', () => {
    scrapeEditorAgents();
    scrapeEditorPattern();
    templateEditorPattern.push({ send_to: 'all', value: '' });
    renderEditorPattern();
  });
  $('#template-editor-pattern').addEventListener('click', e => {
    const btn = e.target.closest('.tw-remove, .tw-up, .tw-down');
    if (!btn) return;
    const idx = parseInt(btn.closest('.template-pattern-row').dataset.idx, 10);
    scrapeEditorAgents();
    scrapeEditorPattern();
    const arr = templateEditorPattern;
    if (btn.classList.contains('tw-remove')) arr.splice(idx, 1);
    else if (btn.classList.contains('tw-up') && idx > 0) [arr[idx - 1], arr[idx]] = [arr[idx], arr[idx - 1]];
    else if (btn.classList.contains('tw-down') && idx < arr.length - 1) [arr[idx], arr[idx + 1]] = [arr[idx + 1], arr[idx]];
    renderEditorPattern();
  });
  bindBackdropDiscard('template-editor-modal', closeTemplateEditor);

  // Instantiate modal.
  $('#template-instantiate-cancel').addEventListener('click', closeInstantiateModal);
  $('#template-instantiate-submit').addEventListener('click', submitInstantiate);
  $('#template-instantiate-template').addEventListener('change', renderInstantiatePreview);
  $('#template-instantiate-group').addEventListener('input', renderInstantiatePreview);
  bindBackdropDiscard('template-instantiate-modal', closeInstantiateModal);

  // From-group modal. Typing the name live-flips the dialog between its
  // create and update-in-place modes.
  $('#template-from-group-cancel').addEventListener('click', closeFromGroupModal);
  $('#template-from-group-submit').addEventListener('click', submitFromGroup);
  $('#template-from-group-name').addEventListener('input', refreshFromGroupUpdateState);
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
    recordGroupInteraction(body.group);
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

// ---- Group clone ------------------------------------------------------
//
// Clones an entire group via POST /api/groups/{name}/clone. The new
// group carries every source setting + owners; the checkbox controls
// whether the member agents are cloned too (no_clone_members).

// Strips a trailing -c-<N> / -clone-<N> suffix, mirroring the daemon's
// cloneSuffixRegex so a clone-of-a-clone bumps N rather than nesting.
const GROUP_CLONE_SUFFIX_RE = /^(.*?)-(?:c|clone)-\d+$/;

// defaultGroupCloneName computes the smallest free `<base>-c-<N>` name
// from the current snapshot — the same scheme the daemon's
// nextGroupCloneName applies server-side. Client-side it is only a
// prefill hint: when the user accepts it unchanged we send no name and
// let the daemon pick authoritatively (race-free), so a stale snapshot
// can never produce a colliding request.
function defaultGroupCloneName(srcName) {
  const m = GROUP_CLONE_SUFFIX_RE.exec(srcName);
  const base = m ? m[1] : srcName;
  const prefix = base + '-c-';
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const used = new Set();
  for (const g of groups) {
    if (!g || !g.name || !g.name.startsWith(prefix)) continue;
    const suffix = g.name.slice(prefix.length);
    if (/^\d+$/.test(suffix)) used.add(parseInt(suffix, 10));
  }
  let n = 1;
  while (used.has(n)) n++;
  return prefix + n;
}

// groupSnapshot looks up a group's full row from the latest snapshot.
function groupSnapshot(groupName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  return groups.find(g => g && g.name === groupName) || null;
}

// renderGroupClonePreview builds the "what you'll get" panel from the
// source group's snapshot: every setting the clone carries, plus owner
// and member-agent counts. withAgents toggles how the member-agents row
// reads (cloned vs skipped) so the preview tracks the checkbox live.
function renderGroupClonePreview(g, withAgents) {
  if (!g) return '<div class="gcp-title">Preview unavailable (group not in current snapshot)</div>';
  const rows = g.members || [];
  // The snapshot's members[] also carries pure-owner rows (owners who
  // aren't members: role "owner", no descr) so the list stays
  // comprehensive. The clone loop only forks genuine membership rows, so
  // exclude pure owners from the member-agent count to match exactly what
  // gets cloned. Owners are tallied separately below (all owner rows).
  const isPureOwner = m => m && m.role === 'owner' && !m.descr;
  const memberRows = rows.filter(m => m && !isPureOwner(m));
  const memberCount = memberRows.length;
  const onlineCount = memberRows.filter(m => m.online).length;
  const ownerCount = rows.filter(m => m && m.owner).length;
  const ctxLen = g.default_context ? g.default_context.length : 0;
  const row = (key, val, muted) =>
    `<div class="gcp-row"><span class="gcp-key">${esc(key)}</span>`
    + `<span class="gcp-val${muted ? ' muted' : ''}">${esc(val)}</span></div>`;
  const orNone = (v, label) => v ? row(label, v) : row(label, 'none', true);
  const memberVal = memberCount === 0
    ? 'none'
    : `${memberCount} (${onlineCount} online)` + (withAgents
      ? ' — cloned with history'
      : ' — skipped (settings + owners only)');
  return '<div class="gcp-title">Clone will carry</div>'
    + orNone(g.default_cwd, '📁 directory')
    + orNone(g.descr, '📝 description')
    + row('📋 startup context', ctxLen > 0 ? `${ctxLen} chars` : 'none', ctxLen === 0)
    + orNone(g.default_profile, '🧠 profile')
    + row('👥 max members', g.max_members ? String(g.max_members) : 'unlimited', !g.max_members)
    + row('🔔 notifications', g.notify_enabled ? 'on' : 'off')
    + row('👤 owners', ownerCount > 0 ? `${ownerCount} (copied)` : 'none', ownerCount === 0)
    + row('🤖 member agents', memberVal, memberCount === 0 || !withAgents);
}

// Repaints the preview's member-agents line (and the rest) to match the
// current "clone agents" checkbox state.
function refreshGroupClonePreview() {
  const g = groupSnapshot(groupCloneModalGroup);
  const withAgents = $('#group-clone-with-agents').checked;
  $('#group-clone-preview').innerHTML = renderGroupClonePreview(g, withAgents);
}

// The group being cloned + the prefilled default name, so submit can
// tell "accepted the default" (send no name) from an explicit override.
let groupCloneModalGroup = '';
let groupCloneDefaultName = '';

function openGroupCloneModal(groupName) {
  groupCloneModalGroup = groupName;
  groupCloneDefaultName = defaultGroupCloneName(groupName);
  $('#group-clone-name').value = groupCloneDefaultName;
  $('#group-clone-with-agents').checked = true;
  $('#group-clone-error').textContent = '';
  const meta = $('#group-clone-meta');
  meta.textContent = `source: ${groupName}`;
  meta.style.display = '';
  refreshGroupClonePreview();
  $('#group-clone-modal').classList.add('show');
  setTimeout(() => {
    const inp = $('#group-clone-name');
    inp.focus();
    inp.select();
  }, 0);
}

function closeGroupCloneModal() {
  $('#group-clone-modal').classList.remove('show');
  groupCloneModalGroup = '';
  groupCloneDefaultName = '';
}

async function submitGroupClone() {
  const group = groupCloneModalGroup;
  if (!group) { closeGroupCloneModal(); return; }
  const name = $('#group-clone-name').value.trim();
  const withAgents = $('#group-clone-with-agents').checked;
  const errEl = $('#group-clone-error');
  errEl.textContent = '';
  const submitBtn = $('#group-clone-submit');
  submitBtn.disabled = true;
  try {
    const body = { no_clone_members: !withAgents };
    // Only send an explicit name when the user overrode the default —
    // accepting the prefill sends nothing so the daemon picks the next
    // free -c-<N> itself, immune to a stale-snapshot collision.
    if (name && name !== groupCloneDefaultName) body.new_name = name;
    const r = await fetch(`/api/groups/${encodeURIComponent(group)}/clone`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    let created = '';
    let failed = 0;
    try {
      const out = await r.json();
      created = out.group || '';
      failed = (out.members || []).filter(m => m && m.error).length;
    } catch (_) { /* non-JSON success — fall back to a generic toast */ }
    closeGroupCloneModal();
    const where = created ? `"${created}"` : 'new group';
    toast(withAgents
      ? (failed > 0
        ? `Cloned ${group} → ${where} (${failed} member(s) skipped — see CLI for detail)`
        : `Cloned ${group} → ${where}`)
      : `Cloned ${group} → ${where} (settings + owners only)`,
      failed > 0);
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submitBtn.disabled = false;
  }
}

function bindGroupCloneModal() {
  $('#group-clone-cancel').addEventListener('click', closeGroupCloneModal);
  $('#group-clone-submit').addEventListener('click', submitGroupClone);
  // Repaint the preview's member-agents line as the checkbox toggles.
  $('#group-clone-with-agents').addEventListener('change', refreshGroupClonePreview);
  bindBackdropDiscard('group-clone-modal', closeGroupCloneModal);
}

export {
  renderTemplatesTab, bindTemplatesUI, bindGroupImportModal,
  openGroupContextModal, bindGroupContextModal, groupDefaultContext,
  openGroupCloneModal, bindGroupCloneModal, openFromGroupModal,
};
