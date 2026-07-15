// modal-message.js — legacy group-create/template/mirror behavior.
//
// Message, human-reply, sudo, shared chooser, and permission dialogs are
// Preact-owned behind message-access-dialog-controller.js. Keep this module
// limited to the unrelated group-create surface until TCL-455 migrates it.

import { $, esc, pickDirectory } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { recordGroupInteraction } from './last-group.js';
import { lastSnapshot } from './dashboard.js';
import {
  refresh, toast, openCleanupModal, openDeleteRetiredPreview, bindBackdropDiscard,
} from './refresh.js';
import {
  openTemplatesManageModal, templateReadbackBadges, templateRosterRowsHTML,
} from './modal-templates.js';
import { wizWord } from './slop.js';

// ---- Group create modal -------------------------------------------------
//
// The dialog creates an EMPTY group by default ("(blank party)"), but can also
// start from a template / summoning circle (JOH-356): pick a circle and the
// group's own editable copy of descr / startup context is prefilled, a roster
// readback appears, the per-instantiation Task field is surfaced, and submit
// routes through the template instantiate path (spawns the whole roster) instead
// of the empty-group create. Agent : spawn-profile :: party : template.

// gcTemplateCache holds a freshly-fetched template list for the picker, or null
// to fall back to the live snapshot. Opening "⧉ manage circles…" stacks the
// templates manager over this dialog; on manager close we fetch /api/templates
// directly and hold the result here so a just-created circle is immediately
// visible and resolvable regardless of the main poll's timing.
let gcTemplateCache = null;

// groupCreateTemplates returns the templates the picker should offer — the
// freshly-fetched list when we have one (after managing circles), else the live
// snapshot.
function groupCreateTemplates() {
  if (gcTemplateCache) return gcTemplateCache;
  return (lastSnapshot && lastSnapshot.templates) || [];
}

// selectedGroupCreateTemplate resolves the party-profile dropdown's current
// value to its full template object, or null for "(blank party)".
function selectedGroupCreateTemplate() {
  const name = $('#group-create-template').value;
  if (!name) return null;
  return groupCreateTemplates().find(t => t.name === name) || null;
}

function groupCreateMirrorOptions() {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const opts = [`<option value="">${wizWord('template settings (top-level)', 'circle lore (top-level)')}</option>`];
  for (const g of groups) {
    if (g && g.name) opts.push(`<option value="${esc(g.name)}">${esc(g.name)}</option>`);
  }
  return opts.join('');
}

function groupCreateMirrorSource() {
  const sel = $('#group-create-source');
  return sel ? sel.value.trim() : '';
}

function groupCreateSnapshot(groupName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  return groups.find(g => g && g.name === groupName) || null;
}

function combineGroupAndTemplateContext(groupContext, templateContext) {
  const group = String(groupContext || '').trim();
  const tmpl = String(templateContext || '').trim();
  if (group && tmpl) {
    return `## Mirrored group context\n\n${group}\n\n## Template context\n\n${tmpl}`;
  }
  return group || tmpl;
}

function prefillGroupCreateFromSource(groupName) {
  const g = groupCreateSnapshot(groupName);
  if (!g) return;
  const tmpl = selectedGroupCreateTemplate();
  $('#group-create-descr').value = g.descr || '';
  $('#group-create-cwd').value = g.default_cwd || '';
  $('#group-create-context').value = combineGroupAndTemplateContext(
    g.default_context,
    tmpl && tmpl.default_context);
}

// populateGroupCreateTemplates fills the party-profile dropdown with a blank
// default + every template. An optional preset preselects a named circle; a
// preset that no longer exists falls back to blank. Preserves nothing else —
// callers re-apply the selection's effects.
function populateGroupCreateTemplates(preset) {
  const sel = $('#group-create-template');
  const templates = groupCreateTemplates();
  const opts = [`<option value="">${wizWord('(blank party)', '(no circle — a blank party)')}</option>`];
  for (const t of templates) opts.push(`<option value="${esc(t.name)}">${esc(t.name)}</option>`);
  sel.innerHTML = opts.join('');
  if (preset && templates.some(t => t.name === preset)) sel.value = preset;
}

// applyGroupCreateTemplate reacts to a party-profile selection: prefill the
// group's OWN editable copy of descr / startup context from the picked circle
// (or clear them back for "(blank party)"), toggle the Task field + roster
// preview + Max-members visibility, and re-flavour the submit button. The
// prefill OVERWRITES those template-derived fields — they are the circle's
// suggested starting point, edited freely before submit; the stored template is
// never touched. Name, cwd and max-members are user fields and are not prefilled
// (a template carries no name or cwd; max-members is not honoured by the
// instantiate path, so its row is hidden while a circle is picked).
function applyGroupCreateTemplate() {
  const t = selectedGroupCreateTemplate();
  const taskRow = $('#group-create-task-row');
  const previewRow = $('#group-create-template-preview-row');
  const maxRow = $('#group-create-max-members-row');
  const sourceRow = $('#group-create-source-row');
  const parentRow = $('#group-create-parent-row');
  const submitBtn = $('#group-create-submit');
  if (!t) {
    $('#group-create-descr').value = '';
    $('#group-create-context').value = '';
    $('#group-create-task').value = '';
    $('#group-create-source').value = '';
    $('#group-create-parent').checked = false;
    sourceRow.style.display = 'none';
    parentRow.style.display = 'none';
    taskRow.style.display = 'none';
    previewRow.style.display = 'none';
    maxRow.style.display = '';
    submitBtn.textContent = 'Create';
    // Switching a pinned subgroup dialog back to the blank profile restores
    // the parent's defaults instead of leaving the inherited fields empty.
    if (groupCreateParent) prefillGroupCreateFromSource(groupCreateParent);
    return;
  }
  const source = groupCreateMirrorSource();
  if (groupCreateParent) {
    // The pinned parent stays authoritative for descr/cwd. The helper combines
    // its startup context with the selected template's context.
    prefillGroupCreateFromSource(groupCreateParent);
  } else if (source) {
    prefillGroupCreateFromSource(source);
  } else {
    $('#group-create-descr').value = t.descr || '';
    $('#group-create-context').value = t.default_context || '';
  }
  // A per-group quick-create already pins both the nesting parent and the
  // inherited settings source. Hide the mirror selector and its parent
  // checkbox there so neither control implies that a different group applies.
  sourceRow.style.display = groupCreateParent ? 'none' : '';
  parentRow.style.display = source && !groupCreateParent ? '' : 'none';
  taskRow.style.display = '';
  previewRow.style.display = '';
  maxRow.style.display = 'none';
  submitBtn.textContent = 'Create & spawn';
  renderGroupCreateTemplatePreview();
}

// renderGroupCreateTemplatePreview paints the picked circle's readback badges +
// the roster's final agent names under the typed group name — reusing the same
// helpers the instantiate / deploy previews use, so all three read identically.
function renderGroupCreateTemplatePreview() {
  const t = selectedGroupCreateTemplate();
  const host = $('#group-create-template-preview');
  if (!t) { host.innerHTML = ''; return; }
  host.innerHTML =
    `<div class="tp-badges">${templateReadbackBadges(t)}</div>`
    + templateRosterRowsHTML(t, $('#group-create-name').value);
}

// Set only by the per-group "create subgroup" shortcut. Keeping the parent as
// modal state lets blank and template-backed creation share this dialog.
let groupCreateParent = '';

function openGroupCreateModal(presetTemplate, parentGroup) {
  // Start from the live snapshot each open; a stale manage-fetch cache from a
  // prior session must not shadow it.
  gcTemplateCache = null;
  groupCreateParent = parentGroup || '';
  const regularTitle = $('#group-create-title .group-create-title-regular');
  const wizardTitle = $('#group-create-title .group-create-title-wizard');
  regularTitle.textContent = groupCreateParent
    ? `Create a subgroup under ${groupCreateParent}`
    : 'Create a new agent group';
  wizardTitle.textContent = groupCreateParent
    ? `⚔ Form a sub-party under ${groupCreateParent}`
    : '⚔ Form a party';
  $('#group-create-name').value = '';
  $('#group-create-descr').value = '';
  $('#group-create-cwd').value = '';
  $('#group-create-context').value = '';
  $('#group-create-task').value = '';
  $('#group-create-max-members').value = '';
  $('#group-create-source').innerHTML = groupCreateMirrorOptions();
  $('#group-create-source').value = '';
  $('#group-create-parent').checked = false;
  $('#group-create-error').textContent = '';
  populateGroupCreateTemplates(presetTemplate);
  applyGroupCreateTemplate();
  $('#group-create-modal').classList.add('show');
  // With a circle preselected the roster preview is the point of interest, but
  // the group name is still the first thing to type — focus it either way.
  setTimeout(() => $('#group-create-name').focus(), 0);
}

function closeGroupCreateModal() {
  $('#group-create-modal').classList.remove('show');
}

async function submitGroupCreate() {
  // A picked party profile routes through the template instantiate path; the
  // blank default keeps the original empty-group create verbatim.
  const tmpl = selectedGroupCreateTemplate();
  if (tmpl) { await submitGroupCreateFromTemplate(tmpl); return; }

  const name = $('#group-create-name').value.trim();
  const descr = $('#group-create-descr').value.trim();
  const cwd = $('#group-create-cwd').value.trim();
  const context = $('#group-create-context').value.trim();
  const errEl = $('#group-create-error');
  errEl.textContent = '';
  if (!name) {
    errEl.textContent = 'name is required';
    return;
  }
  // Max members: blank means unlimited (0); a negative value is a
  // mistake — surface it rather than letting the daemon clamp it.
  const maxRaw = $('#group-create-max-members').value.trim();
  let maxMembers = 0;
  if (maxRaw !== '') {
    maxMembers = parseInt(maxRaw, 10);
    if (!Number.isInteger(maxMembers) || maxMembers < 0) {
      errEl.textContent = 'max members must be a non-negative integer (0 = unlimited)';
      return;
    }
  }
  const submitBtn = $('#group-create-submit');
  submitBtn.disabled = true;
  try {
    const r = await fetch('/api/groups', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        name, parent: groupCreateParent, descr, default_cwd: cwd,
        default_context: context, max_members: maxMembers,
      }),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    closeGroupCreateModal();
    toast(groupCreateParent
      ? `subgroup created: ${name} under ${groupCreateParent}`
      : `group created: ${name}`);
    // Persist the expanded state so the new group shows expanded on next render.
    try { dashPrefs.setItem('tclaude.dash.group.' + name, '1'); } catch (_) {}
    recordGroupInteraction(name);
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submitBtn.disabled = false;
  }
}

// submitGroupCreateFromTemplate deploys the picked circle: create the group and
// spawn its whole roster via the template instantiate endpoint, sending the
// group's edited copy of descr / startup context (context_override) + the Task.
// Respects the endpoint's 409-on-existing-name. Mirrors the instantiate modal's
// spawn-count / work-pattern toasts so the outcome reads the same wherever a
// circle is cast.
async function submitGroupCreateFromTemplate(tmpl) {
  const name = $('#group-create-name').value.trim();
  const descr = $('#group-create-descr').value.trim();
  const cwd = $('#group-create-cwd').value.trim();
  const context = $('#group-create-context').value;
  const task = $('#group-create-task').value;
  const mirrorSource = groupCreateMirrorSource();
  const errEl = $('#group-create-error');
  errEl.textContent = '';
  if (!name) {
    errEl.textContent = 'name is required';
    return;
  }
  const submitBtn = $('#group-create-submit');
  submitBtn.disabled = true;
  try {
    const payload = { group_name: name, task, cwd, descr_override: descr, context_override: context };
    if (groupCreateParent) payload.parent = groupCreateParent;
    else if (mirrorSource && $('#group-create-parent').checked) payload.parent = mirrorSource;
    const r = await fetch(`/api/templates/${encodeURIComponent(tmpl.name)}/instantiate`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const txt = await r.text();
    if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
    let resp = {};
    try { resp = JSON.parse(txt); } catch (_) {}
    closeGroupCreateModal();
    const failed = resp.failed || 0;
    toast(failed
      ? `group ${name}: spawned ${resp.spawned || 0}, ${failed} failed — check the group`
      : `group ${name}: spawned ${resp.spawned || 0} agent${resp.spawned === 1 ? '' : 's'}`,
      failed > 0);
    // A silently-skipped kick-off briefing gets its own toast — it must not
    // hide behind a happy spawn count.
    const perrs = resp.pattern_errors || [];
    if (perrs.length) {
      toast(`⚠ work pattern: ${perrs.length} step${perrs.length === 1 ? '' : 's'} not sent — ${perrs[0]}`, true);
    } else if (resp.pattern_delivered) {
      toast(`work pattern: ${resp.pattern_delivered} briefing${resp.pattern_delivered === 1 ? '' : 's'} sent`);
    }
    try { dashPrefs.setItem('tclaude.dash.group.' + name, '1'); } catch (_) {}
    recordGroupInteraction(name);
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submitBtn.disabled = false;
  }
}

// Ask the daemon to open a native directory picker and drop the chosen
// path into the Default cwd field. The browser can't pop an OS folder
// chooser itself, so agentd — running on the human's desktop — does it
// and reports the path back (POST /api/pick-directory). The fetch stays
// pending while the dialog is open; a cancel leaves the field untouched.
async function browseGroupCreateCwd() {
  const input = $('#group-create-cwd');
  const btn = $('#group-create-cwd-browse');
  const errEl = $('#group-create-error');
  errEl.textContent = '';
  const prevLabel = btn.textContent;
  btn.disabled = true;
  btn.textContent = 'Opening…';
  try {
    const res = await pickDirectory({
      startDir: input.value.trim(),
      title: 'Select the group default working directory',
    });
    if (res.error) { errEl.textContent = res.error; return; }
    if (res.canceled) return; // dialog dismissed — leave the field as-is
    input.value = res.path;
    input.focus();
  } finally {
    btn.disabled = false;
    btn.textContent = prevLabel;
  }
}

// repopulateGroupCreateTemplatesIfOpen refreshes the party-profile dropdown
// after the templates manager (opened from "⧉ manage circles…") closes, so a
// circle created / renamed / deleted there shows up — but only while the
// create-group dialog is still open behind it. We fetch /api/templates DIRECTLY
// and hold it in gcTemplateCache so manager-close does not wait for the next
// main poll (a failed fetch falls back to the snapshot). If the
// selection survived, the human's edited copy of descr / context / task is left
// intact (no re-prefill over their edits); if it vanished (deleted in the
// manager), the dependent fields are reconciled back to the blank state.
async function repopulateGroupCreateTemplatesIfOpen() {
  if (!$('#group-create-modal').classList.contains('show')) return;
  const cur = $('#group-create-template').value;
  try {
    const r = await fetch('/api/templates', { credentials: 'same-origin' });
    if (r.ok) {
      const list = await r.json();
      if (Array.isArray(list)) gcTemplateCache = list;
    }
  } catch (_) { /* keep the snapshot fallback */ }
  populateGroupCreateTemplates(cur);
  if ($('#group-create-template').value !== cur) applyGroupCreateTemplate();
  else renderGroupCreateTemplatePreview();
}

function bindGroupCreateModal() {
  $('#group-create-open').addEventListener('click', () => openGroupCreateModal());
  $('#group-create-cwd-browse').addEventListener('click', browseGroupCreateCwd);
  // Party profile picker (JOH-356): selecting a circle prefills + reveals the
  // template-only fields; typing the group name re-flows the roster preview's
  // "<group>-<agent>" names.
  $('#group-create-template').addEventListener('change', applyGroupCreateTemplate);
  $('#group-create-source').addEventListener('change', () => {
    $('#group-create-parent').checked = false;
    applyGroupCreateTemplate();
  });
  $('#group-create-name').addEventListener('input', renderGroupCreateTemplatePreview);
  // "⧉ manage circles…" opens the same templates manager the Groups cog does
  // (the JOH-350 "⧉ manage…" idiom); its create/edit/delete is picked up when
  // it closes (both close paths — Close button and backdrop).
  $('#group-create-manage-templates').addEventListener('click', () => openTemplatesManageModal());
  document.addEventListener('tclaude:management-closed', (event) => {
    if (event.detail?.kind === 'templates') repopulateGroupCreateTemplatesIfOpen();
  });
  // 🧹 cleanup: the Groups tab's "clean up" button opens the rich
  // multi-category cleanup modal — bulk unjoin / retire / delete /
  // reinstate spanning active agents, retired agents and plain
  // conversations (openCleanupModal mode 'agents').
  $('#cleanup-all-open').addEventListener('click', () => openCleanupModal({ mode: 'agents' }));
  // Retired-scoped batch delete (JOH-31) — the filterable preview of every
  // retired agent, all ticked, with per-row opt-out before the purge. The
  // discoverable twin of the command palette's "Delete retired agents…".
  $('#delete-retired-open').addEventListener('click', () => openDeleteRetiredPreview());
  $('#group-create-cancel').addEventListener('click', closeGroupCreateModal);
  $('#group-create-submit').addEventListener('click', submitGroupCreate);
  bindBackdropDiscard('group-create-modal', closeGroupCreateModal);
  $('#group-create-modal').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.target.id === 'group-create-name' || e.target.id === 'group-create-descr' || e.target.id === 'group-create-cwd')) {
      e.preventDefault();
      submitGroupCreate();
    }
  });
}

export {
  bindGroupCreateModal, openGroupCreateModal,
};
