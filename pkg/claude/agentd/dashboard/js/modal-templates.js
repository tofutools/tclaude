// modal-templates.js — the templates tab and its modals.
//
// The templates tab listing, the template editor, the instantiate and
// from-group modals, the group-import modal, and the group-context
// modal. Extracted from dashboard.js in the Stage 2 module split.

import { $, $$, esc, makeModalResizable } from './helpers.js';
import { morphInto } from './morph.js';
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
import { refresh, confirmModal, confirmDiscard, toast, bindBackdropDiscard, bindManageOverlayDismiss } from './refresh.js';
// Scribe summon (JOH-361): the "Edit with agent" buttons hand a template off to
// a pre-briefed, pre-granted chat agent. On a headless daemon the summon opens
// an in-browser terminal instead of a native window — same fallback the spawn
// dialog / open-window action use, so we reuse their modal.
import { openTermModal } from './modal-term.js';
// Roles (JOH-240): the per-agent role dropdown reads the role library through
// the roles.js data layer. loadRoles fills the cache on editor open; cachedRoles
// feeds the synchronous row render.
import { loadRoles, cachedRoles } from './roles.js';
// Spawn profiles (JOH-350): a template agent's launch config is now "pick a
// stored spawn profile" instead of a bespoke inline field set. The profile
// picker reads the same profiles.js data layer the spawn dialog and profiles
// manager use; the "＋ new" / "⧉ manage…" / "Extract to profile…" affordances
// open the REAL profiles editor/overlay (modal-profiles.js) so there is one
// editing surface for launch config + birth-time permissions, everywhere.
import { loadProfiles, cachedProfiles, profileSummary } from './profiles.js';
import { openProfileEditor, openProfilesManageModal } from './modal-profiles.js';
// roleInspectHTML (JOH-351): the shared "what does this role carry?" panel, so
// picking a role in the dropdown isn't blind. Reused verbatim by any role picker.
import { roleInspectHTML } from './role-inspect.js';


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
let templateEditorProcess = [];
let templateEditorRhythms = [];

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
  // Morph rather than swap: the overlay repaints every 2s while the user reads
  // it (a .manage-overlay is deliberately not refresh-suspended), so a plain
  // innerHTML swap would wipe a selection each tick. Cards are keyed by name.
  if (!list.length) {
    morphInto(host, `<div class="template-empty">${all.length
      ? wizWord('No templates match the filter.', 'No circles match the filter.')
      : wizWord(
        'No templates yet — press <b>+ new template</b> to define one, <b>⤓ from a group</b> to snapshot an existing group, or <b>⭐ starters</b> above to copy in a ready-made team.',
        'No summoning circles chalked yet — press <b>+ chalk a new circle</b> to inscribe one, <b>⤓ trace a party</b> to copy an existing party’s shape, or <b>⭐ conjure a preset party</b> above to copy in a ready-made party.')}</div>`);
    return;
  }
  morphInto(host, list.map(templateCardHTML).join(''));
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

// ---- Edit with agent: summon a scribe (JOH-361) -----------------------
//
// A "scribe" is an ordinary chat agent summoned to edit templates for the
// human — it comes up already briefed on the task and already holding the
// templates.manage permission (see the bundled agent-circles skill). Two entry
// points share one reusable summon: the Templates overlay header (library
// scope — edit any circle) and the template editor (template scope — anchored
// on the open circle). Both reuse ONE stable scribe (reuse-if-alive on the
// daemon), so a repeat click re-briefs + re-focuses the live scribe rather than
// littering a new one per click. The daemon endpoint is generic {name, slugs,
// brief}; the brief is the only thing that differs between the two entry points.

// SCRIBE_NAME is the stable name the daemon reuses-if-alive; both entry points
// summon the same scribe, differing only in the brief. SCRIBE_SLUGS is the
// minimal grant bundle a template scribe needs (the agent-circles skill's
// recommendation) — templates.manage only; instantiate/roles/profiles stay with
// the human.
const SCRIBE_NAME = 'circle-scribe';
const SCRIBE_SLUGS = ['templates.manage'];

// scribeLibraryBrief anchors the scribe on the whole template library.
function scribeLibraryBrief() {
  return [
    'You are a scribe: your job is editing this daemon’s tclaude group templates (a.k.a. summoning circles) by chat, on the human’s behalf.',
    'Read the `agent-circles` skill for the full workflow and the template JSON wire shape.',
    'Discover templates with `tclaude agent templates ls`, inspect one with `tclaude agent templates show <name> --json`, then edit with the safe show-json → edit --file loop — `edit` is a FULL REPLACE, so always post the whole desired state.',
    'Verify with `tclaude agent templates show <name>` after each edit. You already hold the templates.manage grant.',
    'The human will tell you which circle to change and how — wait for their instructions.',
  ].join('\n\n');
}

// scribeTemplateBrief anchors the scribe on one existing template by name.
function scribeTemplateBrief(name) {
  return [
    `You are a scribe: your job is editing the tclaude group template (summoning circle) named "${name}" by chat, on the human’s behalf.`,
    'Read the `agent-circles` skill for the workflow and the template JSON wire shape.',
    `Start by loading its current state: \`tclaude agent templates show "${name}" --json\`. Then edit with the safe show-json → edit --file loop — \`edit\` is a FULL REPLACE, so always post the whole desired state.`,
    `Verify with \`tclaude agent templates show "${name}"\` after each edit. You already hold the templates.manage grant.`,
    'The human will tell you what to change — wait for their instructions.',
  ].join('\n\n');
}

// scribeNewTemplateBrief anchors the scribe on creating a fresh template.
function scribeNewTemplateBrief() {
  return [
    'You are a scribe: your job is creating a new tclaude group template (summoning circle) by chat, on the human’s behalf.',
    'Read the `agent-circles` skill for the workflow and the template JSON wire shape — a minimal template is just a name plus an agents roster (each with a name and an initial_message).',
    'Gather the roster and any choreography from the human, write the JSON, and create it with `tclaude agent templates create --file <path>`.',
    'Verify with `tclaude agent templates show <name>` after creating. You already hold the templates.manage grant.',
    'Wait for the human to describe the team they want.',
  ].join('\n\n');
}

// summonScribe POSTs the brief to the daemon's reusable scribe endpoint and
// surfaces the result. On a headless daemon (no native window) it opens the
// in-browser terminal the daemon hands back, mirroring the spawn dialog's focus
// fallback (openTermModal + hideConv so closing it detaches cleanly).
async function summonScribe(brief) {
  try {
    const r = await fetch('/api/scribe', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: SCRIBE_NAME, slugs: SCRIBE_SLUGS, brief }),
    });
    if (!r.ok) { toast((await r.text()) || `HTTP ${r.status}`, true); return; }
    let resp = {};
    try { resp = await r.json(); } catch (_) {}
    const verb = resp.reused ? 'resumed' : 'summoned';
    if (resp.focus_mode === 'browser' && resp.focus_ws) {
      openTermModal({ wsPath: resp.focus_ws, label: SCRIBE_NAME, hideConv: resp.conv_id || null });
      toast(`${verb} scribe ${SCRIBE_NAME} — opened in-browser terminal`);
    } else {
      toast(`${verb} scribe ${SCRIBE_NAME} — opening its terminal`);
    }
  } catch (err) {
    toast((err && err.message) || String(err), true);
  }
}

// templateEditorDiscard holds bindBackdropDiscard's dirty handle for the
// editor, so the editor's "Edit with agent" button can offer the same discard
// confirm before it closes the editor to hand off (JOH-361).
let templateEditorDiscard = null;

// summonScribeFromEditor hands the open editor off to a scribe. Handling dirty
// state first is a correctness requirement: `edit` is a full replace, so an
// editor left open with unsaved changes would overwrite the scribe's work on
// its next Save. So we close the editor before summoning; if it is dirty we
// offer the same discard confirm Escape does (Save first + re-click to keep the
// edits). templateEditorEditing carries the saved name, or null when creating.
async function summonScribeFromEditor() {
  if (templateEditorDiscard && templateEditorDiscard.isDirty() && !(await confirmDiscard())) return;
  const editing = templateEditorEditing;
  closeTemplateEditor();
  await summonScribe(editing ? scribeTemplateBrief(editing) : scribeNewTemplateBrief());
}

// templateWaveCount is the number of distinct staged-spawn waves a template's
// agents span (JOH-244). 1 (or 0) = a single synchronous pass, no wave badge.
function templateWaveCount(t) {
  const waves = new Set((t.agents || []).map(a => a.wave || 0));
  return waves.size;
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
  // Deployed forces (JOH-245): groups deployed/instantiated from THIS template
  // (source_template match). Rendered as a compact "task forces" strip under
  // the card so the Templates tab doubles as a light Task Forces view — each
  // shows its group name and (if deployed) the mission it was sent against.
  const forces = deployedForcesFor(t.name);
  const forcesHTML = forces.length
    ? `<div class="tc-forces" title="${wizWord('task forces deployed from this template', 'hero parties summoned from this circle')}">`
      + `<span class="tc-forces-label">${wizWord('🚀 forces', '⚔ parties')}:</span> `
      + forces.map(g =>
        `<span class="tc-force" data-force-group="${esc(g.name)}" title="${g.mission ? esc(g.mission) : ''}">`
        + `${esc(g.name)}${g.mission ? ` <span class="tc-force-mission">— ${esc(oneLineMission(g.mission))}</span>` : ''}</span>`).join('')
      + `</div>`
    : '';
  return `<div class="template-card" data-key="${esc(t.name)}" data-template="${esc(t.name)}">
    <div class="tc-head">
      <span class="tc-name">${esc(t.name)}</span>
      ${t.descr ? `<span class="tc-descr">${esc(t.descr)}</span>` : ''}
      <span class="tc-count">${n} ${wizWord('agent', 'familiar')}${n === 1 ? '' : 's'}</span>
      ${(t.work_pattern || []).length ? `<span class="tc-count" title="${wizWord('work pattern — ordered briefing messages delivered after the team spawns', 'rite of command — ordered whispers delivered once the party stands')}">⇶ ${(t.work_pattern || []).length}-step ${wizWord('pattern', 'rite')}</span>` : ''}
      ${(t.process || []).length ? `<span class="tc-count" title="${wizWord('process — an ordered, advisory phase plan tracked at runtime', 'quest plan — an ordered, advisory chapter plan tracked as the party works')}">◆ ${(t.process || []).length}-${wizWord('phase', 'chapter')} ${wizWord('process', 'quest')}</span>` : ''}
      ${(t.rhythms || []).length ? `<span class="tc-count" title="${wizWord('rhythms — recurring nudges materialized as group cron jobs at deploy', 'drumbeats — recurring nudges cast as group cron jobs when the party is summoned')}">🥁 ${(t.rhythms || []).length} ${wizWord('rhythm', 'drumbeat')}${(t.rhythms || []).length === 1 ? '' : 's'}</span>` : ''}
      ${templateWaveCount(t) > 1 ? `<span class="tc-count" title="${wizWord('staged spawn — agents span multiple waves; higher waves spawn once the prior wave settles', 'marching order — the party musters in waves, each after the last has drawn breath')}">🌊 ${templateWaveCount(t)} ${wizWord('waves', 'ranks')}</span>` : ''}
      <span class="tc-actions">
        <button class="primary" data-tact="deploy" data-template="${esc(t.name)}" title="${wizWord('Deploy a task force from this template against a mission', 'Summon a hero party from this circle against a quest')}">${wizWord('🚀 deploy', '🧙 summon')}</button>
        <button class="tool" data-tact="instantiate" data-template="${esc(t.name)}" title="${wizWord('Create a group from this template (no mission)', 'Cast this circle — summon a fresh party from it')}">${wizWord('⎘ instantiate', '🕯 cast')}</button>
        <button class="tool" data-tact="edit" data-template="${esc(t.name)}">edit</button>
        <button class="tool" data-tact="duplicate" data-template="${esc(t.name)}" title="${wizWord('Make a full copy of this template under a new name', 'Mirror this circle — chalk an identical copy under a new name')}">${wizWord('⧉ duplicate', '🪞 mirror')}</button>
        <button class="tool" data-tact="export" data-template="${esc(t.name)}" title="${wizWord('Download this template as a portable .task-force.json file to share or re-import', 'Inscribe this circle onto a scroll — a portable .task-force.json to carry or copy')}">${wizWord('⇪ export', '📜 inscribe')}</button>
        <button class="tool" data-tact="delete" data-template="${esc(t.name)}">delete</button>
      </span>
    </div>
    ${agents ? `<div class="tc-agents">${agents}</div>` : ''}
    ${forcesHTML}
  </div>`;
}

// deployedForcesFor returns the groups deployed/instantiated from the named
// template — its source_template matches — newest-ish (snapshot order), so a
// template card can show the forces it has fielded. Never null.
function deployedForcesFor(templateName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  return groups.filter(g => g.source_template === templateName);
}

// oneLineMission collapses a mission to a single short line for the card's
// force strip — a long or multi-line mission would blow out the row.
function oneLineMission(m) {
  const first = String(m || '').split('\n')[0].trim();
  return first.length > 60 ? first.slice(0, 57) + '…' : first;
}

function templatesByName() {
  // Null prototype: template names are human-typed, and a plain {} would
  // false-positive existence checks on Object.prototype keys — a template
  // named "constructor" or "toString" must only exist if actually saved.
  const m = Object.create(null);
  for (const t of (lastSnapshot && lastSnapshot.templates) || []) m[t.name] = t;
  return m;
}

// ---- Shared template preview helpers (JOH-356) ------------------------
//
// The instantiate, deploy and "Form a party" previews all render the same two
// things: a compact readback of the template's shape (familiars / waves /
// process / rhythms / pattern chips) and the roster's final agent names under
// the typed group prefix. Extracted here so all three read identically and a
// tweak lands in one place.

// templateReadbackBadges renders the roster-shape chips for a full template
// object — the starterBadges twin, driven off the template's own arrays so a
// picked circle is never blind. "" for a null template.
export function templateReadbackBadges(t) {
  if (!t) return '';
  const n = (t.agents || []).length;
  const chips = [`<span class="tc-count">${n} ${wizWord('agent', 'familiar')}${n === 1 ? '' : 's'}</span>`];
  const waves = templateWaveCount(t);
  if (waves > 1) chips.push(`<span class="tc-count" title="${wizWord('staged-spawn waves', 'marching ranks')}">🌊 ${waves} ${wizWord('waves', 'ranks')}</span>`);
  if ((t.process || []).length) chips.push(`<span class="tc-count" title="${wizWord('process phases', 'quest chapters')}">◆ ${(t.process || []).length}-${wizWord('phase', 'chapter')}</span>`);
  if ((t.rhythms || []).length) chips.push(`<span class="tc-count" title="${wizWord('seeded rhythms', 'drumbeats')}">🥁 ${(t.rhythms || []).length}</span>`);
  if ((t.work_pattern || []).length) chips.push(`<span class="tc-count" title="${wizWord('work-pattern steps', 'rite verses')}">⇶ ${(t.work_pattern || []).length}</span>`);
  return chips.join(' ');
}

// templateRosterRowsHTML renders the per-agent "final name + launch shape" rows
// for a template's roster under the typed group prefix — agent "PO" shows as
// "<group>-PO". A blank prefix falls back to a ‹group› placeholder. Shared by
// the instantiate / deploy / party-create previews.
export function templateRosterRowsHTML(t, prefix) {
  const agents = (t && t.agents) || [];
  if (!agents.length) {
    return `<span class="tp-empty">${wizWord('this template has no agents', 'this circle names no familiars')}</span>`;
  }
  const shown = (prefix || '').trim() || wizWord('‹group›', '‹party›');
  return agents.map(a => {
    const owner = a.is_owner ? '<span class="tp-owner" title="group owner">★ owner</span>' : '';
    const np = (a.permissions || []).length;
    // Per-role launch hint (JOH-239): the profile ref or the most telling
    // inline override so the human sees each role's launch shape before spawning.
    const launch = a.spawn_profile
      ? `⚙ ${esc(a.spawn_profile)}`
      : [a.harness, a.model, a.effort].filter(Boolean).map(esc).join('/');
    const meta = [a.role ? esc(a.role) : '', launch, np ? `+${np}🔑` : '', owner]
      .filter(Boolean).join(' · ');
    return `<div class="tp-row"><span class="tp-name">${esc(shown)}-${esc(a.name)}</span>`
      + (meta ? ` <span class="tp-meta">${meta}</span>` : '') + `</div>`;
  }).join('');
}

function blankTemplateAgent() {
  return {
    name: '', role: '', descr: '', initial_message: '', is_owner: false, permissions: [],
    role_ref: '', spawn_profile: '', harness: '', model: '', effort: '', sandbox: '', approval: '',
    wave: 0,
  };
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
        role_ref: a.role_ref || '',
        spawn_profile: a.spawn_profile || '', harness: a.harness || '',
        model: a.model || '', effort: a.effort || '', sandbox: a.sandbox || '', approval: a.approval || '',
        wave: a.wave || 0,
      }))
    : [blankTemplateAgent()];
  templateEditorPattern = tmpl
    ? (tmpl.work_pattern || []).map(e => ({ send_to: e.send_to || 'all', value: e.value || '' }))
    : [];
  templateEditorProcess = tmpl
    ? (tmpl.process || []).map(ph => ({ name: ph.name || '', roles: (ph.roles || []).slice(), criteria: ph.criteria || '' }))
    : [];
  templateEditorRhythms = tmpl
    ? (tmpl.rhythms || []).map(r => ({
        name: r.name || '', target_role: r.target_role || '',
        interval: r.interval || '', cron_expr: r.cron_expr || '',
        subject: r.subject || '', body: r.body || '',
      }))
    : [];
  $('#template-editor-wave-max-wait').value = tmpl && tmpl.wave_max_wait ? tmpl.wave_max_wait : '';
  renderEditorAgents();
  renderEditorPattern();
  renderEditorProcess();
  renderEditorRhythms();
  // Fill the role-library AND spawn-profile caches so the per-agent role and
  // launch-profile dropdowns have options, then re-render the rows (the first
  // render used whatever was already cached). A load failure just leaves the
  // dropdowns with the "(none)" option. Both fetches are independent, so a
  // Promise.allSettled re-renders once when both settle.
  Promise.allSettled([loadRoles(), loadProfiles()]).then(() => {
    if (!$('#template-editor-modal').classList.contains('show')) return;
    scrapeEditorAgents(); // preserve anything typed while the fetch was in flight
    renderEditorAgents();
  });
  $('#template-editor-modal').classList.add('show');
  setTimeout(() => $('#template-editor-name').focus(), 0);
}

function closeTemplateEditor() { $('#template-editor-modal').classList.remove('show'); }

function renderEditorAgents() {
  $('#template-editor-agents').innerHTML =
    templateEditorAgents.map((a, i) => editorAgentRowHTML(a, i)).join('');
}

// profileRefOptionsHTML builds the <option> list for an agent's launch-profile
// dropdown from the cached spawn profiles (blank = "(none)"). A referenced
// profile that is no longer in the library stays selectable — flagged
// "⚠ missing" — so a dangling reference isn't silently cleared on the human
// (the same graceful-degrade the role dropdown does).
function profileRefOptionsHTML(current) {
  const names = cachedProfiles().map(p => p.name);
  const opts = [`<option value=""${current ? '' : ' selected'}>(none)</option>`];
  for (const n of names) {
    opts.push(`<option value="${esc(n)}"${n === current ? ' selected' : ''}>${esc(n)}</option>`);
  }
  if (current && !names.includes(current)) {
    opts.push(`<option value="${esc(current)}" selected>⚠ ${esc(current)} (missing)</option>`);
  }
  return opts.join('');
}

// profileSummaryHTML renders the compact one-line summary of the picked
// profile's set fields (harness/model/effort/… + owner/perms), reusing the
// profiles.js summariser the manage cards use. Empty when no profile is picked;
// a "not found here" note when the ref dangles.
function profileSummaryHTML(current) {
  if (!current) return '';
  const p = cachedProfiles().find(x => x.name === current);
  if (!p) return `<span class="ta-profile-summary-missing">⚠ no profile named “${esc(current)}” here — pick another or manage profiles</span>`;
  const s = profileSummary(p);
  return s ? esc(s) : '<span class="ta-profile-summary-empty">(profile sets no launch fields)</span>';
}

// agentLegacyInline reports the pre-cutover inline launch/permission fields a
// template agent still carries (JOH-350). They are no longer editable inline —
// they ride a profile now — but are honoured at deploy and preserved through a
// re-save so nothing is silently dropped; "Extract to profile…" migrates them.
function agentLegacyInline(a) {
  const parts = [];
  for (const k of ['harness', 'model', 'effort', 'sandbox', 'approval']) {
    if (a[k]) parts.push(`${k} ${a[k]}`);
  }
  const np = (a.permissions || []).length;
  if (np) parts.push(`${np} inline perm${np === 1 ? '' : 's'}`);
  return parts;
}

// legacyInlineNoticeHTML renders the read-only "legacy inline settings" notice +
// the Extract-to-profile button for an agent that still carries pre-cutover
// inline fields; "" when it carries none.
function legacyInlineNoticeHTML(a) {
  const parts = agentLegacyInline(a);
  if (!parts.length) return '';
  return `<div class="ta-legacy-note" title="These inline launch/permission settings predate the profile picker (JOH-350). They still apply when you deploy this template and are preserved when you save, but can no longer be edited inline. Extract them into a reusable spawn profile to manage them going forward — the owner flag stays here (it is structural).">
    <span class="ta-legacy-text">⚠ legacy inline: ${esc(parts.join(' · '))}</span>
    <button type="button" class="tool ta-extract-profile">Extract to profile…</button>
  </div>`;
}

// roleRefOptionsHTML builds the <option> list for a roster agent's role-library
// dropdown from the cached roles (blank = "(none)"). A referenced role that is
// no longer in the library stays selectable — flagged "⚠ missing" — so a
// dangling reference isn't silently cleared on the human.
function roleRefOptionsHTML(current) {
  const names = cachedRoles().map(r => r.name);
  const opts = [`<option value=""${current ? '' : ' selected'}>(none)</option>`];
  for (const n of names) {
    opts.push(`<option value="${esc(n)}"${n === current ? ' selected' : ''}>${esc(n)}</option>`);
  }
  if (current && !names.includes(current)) {
    opts.push(`<option value="${esc(current)}" selected>⚠ ${esc(current)} (missing)</option>`);
  }
  return opts.join('');
}

// roleInspectFor renders the inspect panel for a role name selected in a
// dropdown: the role's definition when it's in the library, a dangling-reference
// note for a "⚠ missing" ref, or '' (the panel collapses) for "(none)". Used to
// (re)paint the per-agent role-inspect container on open and on every change.
function roleInspectFor(name) {
  if (!name) return '';
  const rl = cachedRoles().find(r => r.name === name);
  if (!rl) return roleInspectHTML(null, { missing: true });
  return roleInspectHTML(rl);
}

function editorAgentRowHTML(a, idx) {
  // Launch config + birth-time permissions ride the picked spawn profile
  // (JOH-350 / JOH-354): no bespoke harness/model/effort/sandbox/approval field
  // set and no inline permission-checkbox list live here anymore — they are all
  // configured once, in the profile, editable via the real profiles dialog. The
  // ★ owner box stays (structural: which member leads the group). Deploy unions
  // this flag with the profile's own is_owner default.
  return `<div class="template-agent-row" data-idx="${idx}">
    <div class="template-agent-row-head">
      <span class="template-agent-num">${wizWord('Agent', 'Familiar')} ${idx + 1}</span>
      <label class="template-agent-owner" title="Mark this agent as an owner of the instantiated group — a group can have several owners. This is unioned with the picked profile's own owner default (either one makes the agent an owner).">
        <input type="checkbox" class="ta-owner"${a.is_owner ? ' checked' : ''} /> owner
      </label>
      <button type="button" class="tool ta-remove" title="Remove this agent">✕</button>
    </div>
    <div class="template-agent-grid">
      <input type="text" class="ta-name" placeholder="name (e.g. PO, dev1)" value="${esc(a.name)}" />
      <input type="text" class="ta-role" placeholder="role label (e.g. product-owner)" value="${esc(a.role)}" />
      <input type="number" class="ta-wave" min="0" title="Staged-spawn wave (JOH-244): wave 0 spawns first; higher waves spawn once the prior wave is up and idle. All wave 0 = one synchronous pass." placeholder="wave (0)" value="${a.wave || 0}" />
    </div>
    <label class="template-agent-roleref" title="Reference a role from the library (JOH-240): the agent inherits that role's canonical brief, default launch shape and default permissions — beneath the picked launch profile, which overrides. Blank = no role. Roles resolve at deploy time, so editing the role changes future deploys.">
      <span>Role library</span>
      <select class="ta-role-ref">${roleRefOptionsHTML(a.role_ref)}</select>
    </label>
    <div class="ta-role-inspect">${roleInspectFor(a.role_ref)}</div>
    <input type="text" class="ta-descr" placeholder="one-line description (dashboard column)" value="${esc(a.descr)}" />
    <textarea class="ta-initmsg" rows="3" placeholder="task brief for this agent — delivered to its inbox at spawn (newlines OK)">${esc(a.initial_message)}</textarea>
    <div class="template-agent-launch">
      <label class="template-agent-roleref ta-launch-pick" title="Launch profile (JOH-350): the agent's harness, model, effort, sandbox/approval AND its birth-time permissions all come from the picked spawn profile. Manage profiles to create/edit one — a profile is the unit of launch config.">
        <span>Launch profile</span>
        <select class="ta-profile-select">${profileRefOptionsHTML(a.spawn_profile)}</select>
      </label>
      <div class="ta-launch-actions">
        <button type="button" class="tool ta-profile-new" title="Create a new spawn profile and use it for this agent">＋ new</button>
        <button type="button" class="tool ta-profile-manage" title="Open the spawn-profiles manager to create or edit profiles">⧉ manage…</button>
      </div>
      <div class="ta-profile-summary">${profileSummaryHTML(a.spawn_profile)}</div>
      ${legacyInlineNoticeHTML(a)}
    </div>
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

// ---- Process phase rows (JOH-242) -------------------------------------
//
// The template's advisory process: an ORDERED list of phases (the party's
// quest plan / chapters), each a {name, roles, criteria}. It is rendered
// into every agent's briefing and tracked at runtime, but NOT enforced. The
// rows reuse the agent-row panel chrome (.template-agent-row) so the wizard
// re-skin covers them for free. Roles are a comma-separated free-text field
// (matched case-insensitively against a member's role; "all" = everyone).

function renderEditorProcess() {
  $('#template-editor-process').innerHTML =
    templateEditorProcess.map((ph, i) => processRowHTML(ph, i)).join('');
}

function processRowHTML(ph, idx) {
  const roles = (ph.roles || []).join(', ');
  return `<div class="template-agent-row template-process-row" data-idx="${idx}">
    <div class="template-agent-row-head">
      <span class="template-agent-num">${wizWord('Phase', 'Chapter')} ${idx + 1}</span>
      <button type="button" class="tool tpp-up" title="Move this phase up">↑</button>
      <button type="button" class="tool tpp-down" title="Move this phase down">↓</button>
      <button type="button" class="tool tpp-remove" title="Remove this phase">✕</button>
    </div>
    <div class="template-agent-grid">
      <input type="text" class="tpp-name" placeholder="phase name (e.g. design, build, review)" value="${esc(ph.name || '')}" />
      <input type="text" class="tpp-roles" placeholder="active roles, comma-separated (e.g. dev, reviewer; 'all' = everyone)" value="${esc(roles)}" />
    </div>
    <textarea class="tpp-criteria" rows="2" placeholder="criteria — entry / exit / handoff in plain words (advisory, not enforced)">${esc(ph.criteria || '')}</textarea>
  </div>`;
}

// scrapeEditorProcess reads the process rows back into templateEditorProcess
// — same never-lose-typed-values contract as scrapeEditorPattern. Roles are
// split on commas and trimmed; blank entries drop.
function scrapeEditorProcess() {
  templateEditorProcess = $$('#template-editor-process .template-process-row').map(row => ({
    name: $('.tpp-name', row).value.trim(),
    roles: $('.tpp-roles', row).value.split(',').map(s => s.trim()).filter(Boolean),
    criteria: $('.tpp-criteria', row).value,
  }));
}

// scrapeEditorAgents reads the agent rows back into templateEditorAgents
// — called before any add/remove (which re-renders the container) and
// before submit, so typed-but-uncommitted values are never lost.
function scrapeEditorAgents() {
  templateEditorAgents = $$('#template-editor-agents .template-agent-row').map(row => {
    // Legacy inline launch/permission fields (harness/model/effort/sandbox/
    // approval + the inline permission list) are no longer editable inline
    // (JOH-350) — they ride the picked profile now. Carry any a pre-cutover
    // template still holds forward from the in-memory model (keyed by the row's
    // render-time index) so a re-save never silently drops them; "Extract to
    // profile…" is the migration path that clears them into a profile.
    const prev = templateEditorAgents[parseInt(row.dataset.idx, 10)] || {};
    return {
      name: $('.ta-name', row).value.trim(),
      role: $('.ta-role', row).value.trim(),
      descr: $('.ta-descr', row).value.trim(),
      initial_message: $('.ta-initmsg', row).value,
      is_owner: $('.ta-owner', row).checked,
      role_ref: $('.ta-role-ref', row).value.trim(),
      spawn_profile: $('.ta-profile-select', row).value.trim(),
      harness: prev.harness || '',
      model: prev.model || '',
      effort: prev.effort || '',
      sandbox: prev.sandbox || '',
      approval: prev.approval || '',
      permissions: (prev.permissions || []).slice(),
      wave: parseInt($('.ta-wave', row).value, 10) || 0,
    };
  });
}

// ---- Launch-profile picker actions (JOH-350) --------------------------
//
// The three affordances on each agent's launch-profile row, all routing to the
// REAL profiles editor/overlay (modal-profiles.js) so profiles are created and
// edited in exactly one place. "＋ new" and "Extract to profile…" auto-select
// the resulting profile for that agent; "⧉ manage…" opens the manager overlay
// (its edits are picked up when it closes — see bindTemplatesUI).

// reloadProfilesAndRender re-fetches the profile list (after a create/edit
// through the real editor) and re-renders the agent rows so the dropdowns +
// summaries reflect it. It does NOT scrape — callers that mutate the in-memory
// model (selecting the just-created profile) must have scraped already, so a
// second scrape here would clobber that change with the stale DOM value.
function reloadProfilesAndRender() {
  if (!$('#template-editor-modal').classList.contains('show')) return;
  const render = () => {
    if ($('#template-editor-modal').classList.contains('show')) renderEditorAgents();
  };
  loadProfiles(true).then(render).catch(render);
}

// onManageProfilesClosed picks up profiles the "⧉ manage…" overlay created or
// edited: scrape first (preserve anything typed in the editor behind it), then
// reload + re-render. Distinct from reloadProfilesAndRender because here there
// is no pending in-memory selection to protect — the DOM is the source of truth.
function onManageProfilesClosed() {
  if (!$('#template-editor-modal').classList.contains('show')) return;
  scrapeEditorAgents();
  reloadProfilesAndRender();
}

// newProfileForAgent opens a blank profile editor; on save the fresh profile is
// selected for the agent at `idx`.
function newProfileForAgent(idx) {
  scrapeEditorAgents();
  openProfileEditor(null, {
    editExisting: false,
    onSaved: (name) => {
      scrapeEditorAgents();
      if (templateEditorAgents[idx]) templateEditorAgents[idx].spawn_profile = name;
      reloadProfilesAndRender();
    },
  });
}

// extractAgentToProfile materializes the agent's legacy inline launch fields +
// inline permission grants into a new spawn profile (the self-heal migration
// path for pre-cutover templates), then points the agent at it and clears the
// inline fields. The owner flag stays on the template agent (it is structural),
// so it is NOT folded into the profile.
function extractAgentToProfile(idx) {
  scrapeEditorAgents();
  const a = templateEditorAgents[idx];
  if (!a) return;
  const permission_overrides = {};
  for (const s of (a.permissions || [])) { if (s) permission_overrides[s] = 'grant'; }
  const seed = {
    harness: a.harness || '', model: a.model || '', effort: a.effort || '',
    sandbox: a.sandbox || '', approval: a.approval || '',
    permission_overrides,
  };
  openProfileEditor(seed, {
    editExisting: false,
    onSaved: (name) => {
      scrapeEditorAgents();
      const cur = templateEditorAgents[idx];
      if (cur) {
        cur.spawn_profile = name;
        cur.harness = cur.model = cur.effort = cur.sandbox = cur.approval = '';
        cur.permissions = [];
      }
      reloadProfilesAndRender();
    },
  });
}

// ---- Rhythm rows (JOH-244) --------------------------------------------
//
// The template's rhythms: recurring nudges (the party's drumbeats)
// materialized as group cron jobs at deploy. Each carries a name, a
// schedule (interval OR cron expression), an optional role filter, an
// optional subject, and a body. The rows reuse the agent-row panel chrome
// (.template-agent-row) so the wizard re-skin covers them for free.

function renderEditorRhythms() {
  $('#template-editor-rhythms').innerHTML =
    templateEditorRhythms.map((r, i) => rhythmRowHTML(r, i)).join('');
}

function rhythmRowHTML(r, idx) {
  return `<div class="template-agent-row template-rhythm-row" data-idx="${idx}">
    <div class="template-agent-row-head">
      <span class="template-agent-num">${wizWord('Rhythm', 'Drumbeat')} ${idx + 1}</span>
      <button type="button" class="tool trh-up" title="Move this rhythm up">↑</button>
      <button type="button" class="tool trh-down" title="Move this rhythm down">↓</button>
      <button type="button" class="tool trh-remove" title="Remove this rhythm">✕</button>
    </div>
    <div class="template-agent-grid">
      <input type="text" class="trh-name" placeholder="name (e.g. status-ping)" value="${esc(r.name || '')}" />
      <input type="text" class="trh-role" placeholder="role filter (blank / 'all' = whole group)" value="${esc(r.target_role || '')}" />
    </div>
    <div class="template-agent-grid">
      <input type="text" class="trh-interval" placeholder="interval (e.g. 10m) — OR cron below" value="${esc(r.interval || '')}" />
      <input type="text" class="trh-cron" placeholder="cron expr (e.g. '0 * * * *') — OR interval" value="${esc(r.cron_expr || '')}" />
    </div>
    <input type="text" class="trh-subject" placeholder="subject (optional)" value="${esc(r.subject || '')}" />
    <textarea class="trh-body" rows="2" placeholder="message body the nudge sends each tick (newlines OK)">${esc(r.body || '')}</textarea>
  </div>`;
}

// scrapeEditorRhythms reads the rhythm rows back into templateEditorRhythms
// — same never-lose-typed-values contract as scrapeEditorProcess.
function scrapeEditorRhythms() {
  templateEditorRhythms = $$('#template-editor-rhythms .template-rhythm-row').map(row => ({
    name: $('.trh-name', row).value.trim(),
    target_role: $('.trh-role', row).value.trim(),
    interval: $('.trh-interval', row).value.trim(),
    cron_expr: $('.trh-cron', row).value.trim(),
    subject: $('.trh-subject', row).value.trim(),
    body: $('.trh-body', row).value,
  }));
}

async function submitTemplateEditor() {
  scrapeEditorAgents();
  scrapeEditorPattern();
  scrapeEditorProcess();
  scrapeEditorRhythms();
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
    process: templateEditorProcess,
    rhythms: templateEditorRhythms,
    wave_max_wait: parseInt($('#template-editor-wave-max-wait').value, 10) || 0,
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
    // force keeps every circle-list mutation's refresh uniform: harmless here
    // (the editor is already closed), but immune to a modal left open above it.
    refresh({ force: true });
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
    // force so the deleted card leaves the list at once (see submitTemplateEditor).
    refresh({ force: true });
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
  $('#template-instantiate-preview').innerHTML =
    templateRosterRowsHTML(t, $('#template-instantiate-group').value);
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

// ---- Deploy-a-task-force modal (JOH-245) ------------------------------
//
// Deploy is instantiate's mission-framed twin: pick a template, state the
// mission (free text or a Linear link), optionally name the group / pick a
// cwd / land the force in a worktree. The group name is prefilled with a
// slug of the mission (matching the server's derivation) until the human
// edits it. Worktree resolution reuses POST /api/worktrees (the spawn
// modal's endpoint), turning a branch + cwd into a worktree path that
// becomes the deploy cwd.

// deployGroupEdited tracks whether the human has typed in the group field —
// once they have, the mission→group-name auto-prefill stops overwriting it.
let deployGroupEdited = false;

function openDeployModal(presetName) {
  const templates = (lastSnapshot && lastSnapshot.templates) || [];
  if (!templates.length) {
    toast(wizWord(
      'no templates yet — define one via the Groups cog ⚙ → ⧉ templates… first',
      'no summoning circles yet — chalk one via the Groups cog ⚙ → ⧉ circles… first'), true);
    return;
  }
  const sel = $('#template-deploy-template');
  sel.innerHTML = templates.map(t =>
    `<option value="${esc(t.name)}">${esc(t.name)}</option>`).join('');
  if (presetName && templates.some(t => t.name === presetName)) sel.value = presetName;
  $('#template-deploy-mission').value = '';
  $('#template-deploy-group').value = '';
  $('#template-deploy-cwd').value = '';
  $('#template-deploy-worktree').value = '';
  $('#template-deploy-error').textContent = '';
  deployGroupEdited = false;
  renderDeployPreview();
  $('#template-deploy-modal').classList.add('show');
  setTimeout(() => $('#template-deploy-mission').focus(), 0);
}

function closeDeployModal() { $('#template-deploy-modal').classList.remove('show'); }

// deploySlug mirrors the server's slugify: lowercase, runs of non-[a-z0-9]
// collapse to a single dash, trimmed, capped to 40. Used only to PREFILL
// the group-name field — the server re-derives (and uniquifies) authoritatively.
function deploySlug(s) {
  const out = String(s || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
  return out.length > 40 ? out.slice(0, 40).replace(/-+$/g, '') : out;
}

// deployIsBareURL mirrors the server's isBareURL: a single whitespace-free
// token that reads as a link has no words to slug, so the group name should
// fall back to the template name.
function deployIsBareURL(s) {
  const m = String(s || '').trim();
  if (!m || /\s/.test(m)) return false;
  const low = m.toLowerCase();
  return low.startsWith('http://') || low.startsWith('https://')
    || low.startsWith('linear.app/') || low.startsWith('www.');
}

// syncDeployGroupPrefill fills the group-name field with the mission slug (or
// the template name for a bare-URL mission) until the human takes it over.
function syncDeployGroupPrefill() {
  if (deployGroupEdited) return;
  const mission = $('#template-deploy-mission').value;
  const tmplName = $('#template-deploy-template').value;
  let base = deployIsBareURL(mission) ? '' : deploySlug(mission);
  if (!base) base = deploySlug(tmplName);
  $('#template-deploy-group').value = base;
  renderDeployPreview();
}

// renderDeployPreview paints the final agent names for the deploy, exactly
// like the instantiate preview — agent "PO" shows as "<group>-PO".
function renderDeployPreview() {
  const t = templatesByName()[$('#template-deploy-template').value];
  $('#template-deploy-preview').innerHTML =
    templateRosterRowsHTML(t, $('#template-deploy-group').value);
}

// resolveDeployWorktree turns a branch + repo (the cwd) into a worktree path
// via POST /api/worktrees, the same endpoint the spawn modal uses. Returns
// the created/checked-out path, or throws with a human message. A worktree
// needs a repo, so a blank cwd is a clear error rather than a confusing
// server-side one.
async function resolveDeployWorktree(branch, cwd) {
  if (!cwd) throw new Error('worktree needs a cwd inside a git repo');
  const r = await fetch('/api/worktrees', {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ repo: cwd, branch }),
  });
  const txt = await r.text();
  if (!r.ok) throw new Error(txt || `HTTP ${r.status}`);
  let resp = {};
  try { resp = JSON.parse(txt); } catch (_) {}
  if (!resp.path) throw new Error('worktree created but no path returned');
  return resp.path;
}

async function submitDeploy() {
  const tmplName = $('#template-deploy-template').value;
  const mission = $('#template-deploy-mission').value.trim();
  const groupName = $('#template-deploy-group').value.trim();
  const branch = $('#template-deploy-worktree').value.trim();
  let cwd = $('#template-deploy-cwd').value.trim();
  const errEl = $('#template-deploy-error');
  errEl.textContent = '';
  if (!tmplName) { errEl.textContent = 'pick a template'; return; }
  if (!mission) { errEl.textContent = 'a mission is required'; return; }
  const btn = $('#template-deploy-submit');
  btn.disabled = true;
  btn.textContent = 'Deploying…';
  try {
    // Land the whole force in a worktree when a branch is given — resolve it
    // to a path first, then use that path as the deploy cwd.
    if (branch) {
      cwd = await resolveDeployWorktree(branch, cwd);
    }
    const payload = { mission };
    if (groupName) payload.group_name = groupName;
    if (cwd) payload.cwd = cwd;
    const r = await fetch(`/api/templates/${encodeURIComponent(tmplName)}/deploy`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const txt = await r.text();
    if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
    let resp = {};
    try { resp = JSON.parse(txt); } catch (_) {}
    const finalGroup = resp.group || groupName;
    closeDeployModal();
    closeTemplatesManageModal();
    const failed = resp.failed || 0;
    // Plain mode: "task force … deployed against <mission>"; wizard mode: the
    // hero party is summoned against its quest.
    toast(failed
      ? wizWord(
        `hero party ${finalGroup}: ${resp.spawned || 0} summoned, ${failed} failed — check the party`,
        `hero party ${finalGroup}: ${resp.spawned || 0} summoned, ${failed} failed — check the party`)
      : wizWord(
        `task force ${finalGroup} deployed against “${oneLineMission(mission)}” — ${resp.spawned || 0} spawned`,
        `🧙 hero party ${finalGroup} summoned against its quest — ${resp.spawned || 0} familiars answer the call`),
      failed > 0);
    const perrs = resp.pattern_errors || [];
    if (perrs.length) {
      toast(`⚠ work pattern: ${perrs.length} step${perrs.length === 1 ? '' : 's'} not sent — ${perrs[0]}`, true);
    } else if (resp.pattern_delivered) {
      toast(`work pattern: ${resp.pattern_delivered} briefing${resp.pattern_delivered === 1 ? '' : 's'} sent`);
    }
    try { dashPrefs.setItem('tclaude.dash.group.' + finalGroup, '1'); } catch (_) {}
    recordGroupInteraction(finalGroup);
    const gbtn = $$('nav button').find(b => b.dataset.tab === 'groups');
    if (gbtn) gbtn.click();
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
    btn.textContent = 'Deploy task force';
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
    // A from-group snapshot can't recover per-agent briefs from a live group,
    // so blank_briefs counts how many agents this template would deploy with
    // nothing to do — surfaced honestly so the human edits before deploying
    // (JOH-344).
    const blank = (tmpl && tmpl.blank_briefs) || 0;
    const blankNote = blank > 0
      ? ` — ⚠ ${blank} agent brief(s) blank; edit the template before deploying`
      : '';
    if (tmpl && tmpl.updated) {
      const kept = (tmpl.briefs_kept || []).length;
      const added = (tmpl.added || []).length;
      const removed = (tmpl.removed || []).length;
      toast(`template updated from ${group}: ${name}`
        + ` (briefs kept: ${kept}, added: ${added}, removed: ${removed})${blankNote}`);
    } else {
      toast(`template created from ${group}: ${name}${blankNote}`);
    }
    // force: the editor reopens on the fresh circle right below, so this
    // refresh runs with a .modal-overlay open — without force its post-fetch
    // suspend re-check would drop the tick and the circle list behind the
    // editor would miss the just-created circle until a later poll.
    refresh({ force: true });
    // Open the editor on the fresh template so the human can fill in
    // per-agent task briefs (from-group leaves new agents' blank).
    if (tmpl) openTemplateEditor(tmpl);
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

// ---- Export / import (JOH-341) ----------------------------------------
//
// Export downloads a template as a portable `<name>.task-force.json`
// envelope (same shape `tclaude agent templates export` writes and
// `import` reads). Import takes a picked file OR pasted JSON, POSTs it to
// the daemon's import endpoint, and surfaces the returned degradation
// warnings (stripped profile refs / unknown permission slugs).

// exportTemplate starts a browser download of the portable envelope. The
// endpoint is same-origin, so the anchor's download attribute forces a
// save with our `<name>.task-force.json` filename regardless of the
// JSON content-type.
function exportTemplate(name) {
  const a = document.createElement('a');
  a.href = `/api/templates/${encodeURIComponent(name)}/export`;
  a.download = `${name}.task-force.json`;
  document.body.appendChild(a);
  a.click();
  a.remove();
}

// ---- Duplicate a template (JOH-365) -----------------------------------
//
// The ⧉ duplicate card action makes a full-fidelity copy of a template
// under a new name — the operator does this often enough to want it one
// click away. There is no dedicated backend: the whole template already
// round-trips through the create endpoint — every stored field (agents
// with roles/profiles/permissions/owner/initial messages, waves, process
// phases, rhythms, work pattern, descr, default context) is carried by
// templateJSON — so duplicate = re-POST the source template's own JSON to
// POST /api/templates with the name swapped. Roles and spawn profiles are
// referenced by name and already exist locally, so the copy shares them
// exactly; no export→import round-trip (which would re-embed them and emit
// "already exists" warnings) is needed. The server's 409-on-existing-name
// is the collision guard, surfaced inline so the dialog stays open.

let templateDuplicateSource = null;

function openDuplicateModal(name) {
  const t = templatesByName()[name];
  if (!t) { toast('template not found', true); return; }
  templateDuplicateSource = name;
  $('#template-duplicate-source').textContent = name;
  $('#template-duplicate-name').value = `${name}-copy`;
  $('#template-duplicate-error').textContent = '';
  $('#template-duplicate-modal').classList.add('show');
  setTimeout(() => {
    const inp = $('#template-duplicate-name');
    inp.focus();
    inp.select();
  }, 0);
}

function closeDuplicateModal() {
  $('#template-duplicate-modal').classList.remove('show');
  templateDuplicateSource = null;
}

async function submitDuplicate() {
  const src = templateDuplicateSource && templatesByName()[templateDuplicateSource];
  const errEl = $('#template-duplicate-error');
  errEl.textContent = '';
  if (!src) { errEl.textContent = 'source template not found — reopen the dialog'; return; }
  const name = $('#template-duplicate-name').value.trim();
  if (!name) { errEl.textContent = 'name is required'; return; }
  if (name === templateDuplicateSource) { errEl.textContent = 'pick a different name for the copy'; return; }
  // Full-fidelity clone: re-POST the source template's own JSON with the
  // name swapped. created_at/updated_at are response-only (the server
  // ignores them on input); dropping them keeps the payload honest rather
  // than rebuilding the body field-by-field, which could silently miss a
  // field as the template struct grows.
  const payload = { ...src, name };
  delete payload.created_at;
  delete payload.updated_at;
  const btn = $('#template-duplicate-submit');
  btn.disabled = true;
  try {
    const r = await fetch('/api/templates', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    if (!r.ok) { errEl.textContent = (await r.text()) || `HTTP ${r.status}`; return; }
    closeDuplicateModal();
    toast(`template duplicated: ${name}`);
    // force keeps every circle-list mutation's refresh uniform: harmless here
    // (the dialog is already closed), but immune to a modal left open above it.
    refresh({ force: true });
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

function openTemplateImportModal() {
  $('#template-import-file').value = '';
  $('#template-import-paste').value = '';
  $('#template-import-as').value = '';
  $('#template-import-update').checked = false;
  $('#template-import-error').textContent = '';
  $('#template-import-modal').classList.add('show');
  setTimeout(() => $('#template-import-paste').focus(), 0);
}

function closeTemplateImportModal() { $('#template-import-modal').classList.remove('show'); }

// readImportSource returns the JSON text to import: the picked file's
// contents if one is chosen, else the pasted text. A picked file wins so
// a stale paste can't shadow a freshly-picked file.
async function readImportSource() {
  const fileInput = $('#template-import-file');
  const file = fileInput.files && fileInput.files[0];
  if (file) return (await file.text()).trim();
  return $('#template-import-paste').value.trim();
}

async function submitTemplateImport() {
  const errEl = $('#template-import-error');
  errEl.textContent = '';
  const btn = $('#template-import-submit');
  btn.disabled = true;
  try {
    const raw = await readImportSource();
    if (!raw) { errEl.textContent = 'pick a file or paste the task-force JSON'; return; }
    // Validate JSON locally for a friendlier error than a raw 400; the
    // daemon is still the authority on the envelope format + version.
    try { JSON.parse(raw); } catch (e) {
      errEl.textContent = 'not valid JSON: ' + ((e && e.message) || String(e));
      return;
    }
    const q = new URLSearchParams();
    const as = $('#template-import-as').value.trim();
    if (as) q.set('as', as);
    if ($('#template-import-update').checked) q.set('update', 'true');
    const qs = q.toString();
    const r = await fetch('/api/templates/import' + (qs ? '?' + qs : ''), {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: raw,
    });
    const txt = await r.text();
    if (!r.ok) {
      let msg = txt;
      try { const j = JSON.parse(txt); if (j && j.error) msg = j.error; } catch (_) {}
      errEl.textContent = msg || `HTTP ${r.status}`;
      return;
    }
    let res = null;
    try { res = JSON.parse(txt); } catch (_) {}
    closeTemplateImportModal();
    const name = (res && res.imported) || as || 'template';
    const warnings = (res && res.warnings) || [];
    let msg = res && res.updated ? `template overwritten: ${name}` : `template imported: ${name}`;
    if (warnings.length) msg += ` — ${warnings.length} warning${warnings.length === 1 ? '' : 's'}: ${warnings.join('; ')}`;
    toast(msg);
    // force so the imported circle shows in the list at once (see submitTemplateEditor).
    refresh({ force: true });
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

// ---- Starter task forces (JOH-246) ----------------------------------
//
// A small starters section reachable from the templates overlay's ⭐ button.
// It lists the bundled starters (fetched from /api/starters) and installs one
// on click through the shared import path. Install never clobbers an existing
// template of the same name — the daemon reports a skip, which we surface as-is.

function openStartersModal() {
  $('#starters-error').textContent = '';
  $('#starters-list').innerHTML =
    `<div class="template-empty">${wizWord('Loading starters…', 'Conjuring presets…')}</div>`;
  $('#starters-modal').classList.add('show');
  loadStarters();
}

function closeStartersModal() { $('#starters-modal').classList.remove('show'); }

async function loadStarters() {
  const errEl = $('#starters-error');
  try {
    const r = await fetch('/api/starters', { credentials: 'same-origin' });
    const txt = await r.text();
    if (!r.ok) {
      let msg = txt;
      try { const j = JSON.parse(txt); if (j && j.error) msg = j.error; } catch (_) {}
      errEl.textContent = msg || `HTTP ${r.status}`;
      $('#starters-list').innerHTML = '';
      return;
    }
    let list = [];
    try { list = JSON.parse(txt) || []; } catch (_) {}
    renderStartersList(list);
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
    $('#starters-list').innerHTML = '';
  }
}

// starterBadges renders the compact "what it demonstrates" chips for a starter
// row — agents, waves, process phases, rhythms, work-pattern steps.
function starterBadges(s) {
  const n = s.agents || 0;
  const chips = [`<span class="tc-count">${n} ${wizWord('agent', 'familiar')}${n === 1 ? '' : 's'}</span>`];
  if ((s.waves || 0) > 1) chips.push(`<span class="tc-count" title="staged-spawn waves">🌊 ${s.waves} ${wizWord('waves', 'ranks')}</span>`);
  if ((s.process || 0) > 0) chips.push(`<span class="tc-count" title="process phases">◆ ${s.process}-${wizWord('phase', 'chapter')}</span>`);
  if ((s.rhythms || 0) > 0) chips.push(`<span class="tc-count" title="seeded rhythms">🥁 ${s.rhythms}</span>`);
  if ((s.work_pattern || 0) > 0) chips.push(`<span class="tc-count" title="work-pattern steps">⇶ ${s.work_pattern}</span>`);
  return chips.join(' ');
}

function renderStartersList(list) {
  const host = $('#starters-list');
  if (!list.length) {
    host.innerHTML = `<div class="template-empty">${wizWord('No starters bundled.', 'No presets bound.')}</div>`;
    return;
  }
  host.innerHTML = list.map(s => `<div class="starter-row" data-starter="${esc(s.name)}">
    <div class="starter-head">
      <span class="tc-name">${esc(s.name)}</span>
      ${s.descr ? `<span class="tc-descr">${esc(s.descr)}</span>` : ''}
    </div>
    <div class="starter-meta">${starterBadges(s)}</div>
    <div class="starter-actions">
      <button class="tool" data-sact="install" data-starter="${esc(s.name)}" title="${wizWord('Copy this starter into your templates list — this does NOT spawn a team; deploy or edit it from the list afterwards', 'Copy this preset into your circles — this summons NO party; cast or redraw it from your circles afterwards')}">${wizWord('⤓ copy to my templates', '⭐ copy into my circles')}</button>
    </div>
  </div>`).join('');
}

async function installStarter(name) {
  const errEl = $('#starters-error');
  errEl.textContent = '';
  const btn = $(`.starter-row[data-starter="${cssEscape(name)}"] [data-sact="install"]`);
  if (btn) btn.disabled = true;
  try {
    const r = await fetch(`/api/starters/${encodeURIComponent(name)}/install`, {
      method: 'POST', credentials: 'same-origin',
    });
    const txt = await r.text();
    if (!r.ok) {
      let msg = txt;
      try { const j = JSON.parse(txt); if (j && j.error) msg = j.error; } catch (_) {}
      errEl.textContent = msg || `HTTP ${r.status}`;
      return;
    }
    let res = null;
    try { res = JSON.parse(txt); } catch (_) {}
    if (res && res.skipped) {
      // The daemon's skip message already spells out "a template named X
      // already exists — skipped (your copy is left untouched) …"; surface it
      // verbatim, with a terse fallback if it's somehow absent.
      toast(res.message || wizWord(
        `${name} is already in your templates — nothing copied`,
        `${name} is already in your circles — nothing copied`));
    } else {
      const finalName = (res && res.name) || name;
      const warnings = (res && res.warnings) || [];
      // Say WHERE it landed and that nothing spawned — the copy-not-cast point.
      let msg = wizWord(
        `added to your templates: ${finalName} — deploy or edit it from the list (nothing spawned yet)`,
        `copied into your circles: ${finalName} — cast or redraw it from your circles (no party summoned yet)`);
      if (warnings.length) msg += ` — ${warnings.length} warning${warnings.length === 1 ? '' : 's'}: ${warnings.join('; ')}`;
      toast(msg);
    }
    // force: the starters picker stays open so several can be copied in a row,
    // and it is a .modal-overlay that would otherwise suspend the poll — so a
    // plain refresh() drops this tick and the circle list behind it stays stale
    // until the human closes and reopens the view. force repaints it now.
    refresh({ force: true });
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    if (btn) btn.disabled = false;
  }
}

// cssEscape quotes a starter name for a CSS attribute selector — starter names
// are kebab-case so this is belt-and-suspenders, but keeps the query robust.
function cssEscape(s) {
  return String(s).replace(/["\\]/g, '\\$&');
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
  // "Edit with agent" (JOH-361): the header button summons a library-scope
  // scribe; the editor's button (bound below) summons a template-scope one.
  $('#scribe-templates-open').addEventListener('click', () => summonScribe(scribeLibraryBrief()));
  // The cog's standalone "⎘ from template" shortcut now opens the "Form a party"
  // dialog with the circle preselected (JOH-356 — one obvious create-a-group
  // surface), so it is bound in bindGroupCreateModal (modal-message.js), not
  // here. The template-card "⎘ instantiate / 🕯 cast" action below still opens
  // the in-overlay instantiate dialog.

  // Template-card actions (delegated — the list re-renders every poll).
  // data-tact (not data-act) keeps these off the global row-action bus.
  $('#templates-list').addEventListener('click', e => {
    const btn = e.target.closest('[data-tact]');
    if (!btn) return;
    const name = btn.dataset.template;
    if (btn.dataset.tact === 'deploy') openDeployModal(name);
    else if (btn.dataset.tact === 'instantiate') openInstantiateModal(name);
    else if (btn.dataset.tact === 'edit') {
      const t = templatesByName()[name];
      if (t) openTemplateEditor(t);
    } else if (btn.dataset.tact === 'duplicate') openDuplicateModal(name);
    else if (btn.dataset.tact === 'export') exportTemplate(name);
    else if (btn.dataset.tact === 'delete') deleteTemplate(name);
  });

  // Duplicate modal (⧉ duplicate on a template card) — asks for the copy's name.
  $('#template-duplicate-cancel').addEventListener('click', closeDuplicateModal);
  $('#template-duplicate-submit').addEventListener('click', submitDuplicate);
  bindBackdropDiscard('template-duplicate-modal', closeDuplicateModal);
  // Keyboard submit: Ctrl/Cmd+Enter from anywhere in the dialog, and plain
  // Enter while the name input is focused — it is a single-line field with no
  // newlines possible, so Enter is unambiguously "submit" (like a browser
  // prompt). A modal-scoped listener fires only while focus is inside, so no
  // document-level capture / stacking handling is needed. Guard on the submit
  // button's disabled state so a held/repeated Enter can't fire a second POST
  // while the first is in flight (a double-create would 409 and confusingly
  // read as "name taken").
  $('#template-duplicate-modal').addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    if ($('#template-duplicate-submit').disabled) return;
    const modifier = e.ctrlKey || e.metaKey;
    const onNameInput = e.target === $('#template-duplicate-name');
    if (modifier || onNameInput) { e.preventDefault(); submitDuplicate(); }
  });

  // Import modal (⤒ import in the toolbar).
  $('#template-import-open').addEventListener('click', openTemplateImportModal);
  $('#template-import-cancel').addEventListener('click', closeTemplateImportModal);
  $('#template-import-submit').addEventListener('click', submitTemplateImport);
  bindBackdropDiscard('template-import-modal', closeTemplateImportModal);

  // Starters modal (⭐ starters in the toolbar) — install a bundled starter.
  $('#starters-open').addEventListener('click', openStartersModal);
  $('#starters-close').addEventListener('click', closeStartersModal);
  bindBackdropDiscard('starters-modal', closeStartersModal);
  $('#starters-list').addEventListener('click', e => {
    const btn = e.target.closest('[data-sact="install"]');
    if (!btn) return;
    installStarter(btn.dataset.starter);
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
  // Delegated handlers on the (re-rendered) agent container: remove an agent, or
  // one of the launch-profile picker actions (JOH-350).
  $('#template-editor-agents').addEventListener('click', e => {
    const row = e.target.closest('.template-agent-row');
    if (!row) return;
    const idx = parseInt(row.dataset.idx, 10);
    if (e.target.closest('.ta-remove')) {
      scrapeEditorAgents();
      scrapeEditorPattern();
      templateEditorAgents.splice(idx, 1);
      renderEditorAgents();
      renderEditorPattern();
    } else if (e.target.closest('.ta-profile-new')) {
      newProfileForAgent(idx);
    } else if (e.target.closest('.ta-profile-manage')) {
      // Preserve typed values before the overlay steals focus; its edits are
      // picked up when it closes (bindProfilesOverlayRefresh below).
      scrapeEditorAgents();
      openProfilesManageModal();
    } else if (e.target.closest('.ta-extract-profile')) {
      extractAgentToProfile(idx);
    }
  });
  // Owner is a plain per-agent checkbox — a group can have several owners, so
  // there is no single-select enforcement. A committed agent-name edit (change
  // fires on blur) refreshes the work-pattern rows' send-to options to the new
  // roster names; a launch-profile pick re-renders the row so its summary +
  // legacy-notice track the selection.
  $('#template-editor-agents').addEventListener('change', e => {
    if (e.target.classList.contains('ta-name')) {
      scrapeEditorAgents();
      scrapeEditorPattern();
      renderEditorPattern();
    } else if (e.target.classList.contains('ta-profile-select')) {
      scrapeEditorAgents();
      renderEditorAgents();
    } else if (e.target.classList.contains('ta-role-ref')) {
      // Repaint the role-inspect panel for the newly-picked role so the pick is
      // never blind (JOH-351). Scoped to this agent's row.
      const row = e.target.closest('.template-agent-row');
      const panel = $('.ta-role-inspect', row);
      if (panel) panel.innerHTML = roleInspectFor(e.target.value.trim());
    }
  });
  // When the spawn-profiles manager (opened from an agent's "⧉ manage…") closes,
  // pick up any profile it created/edited by re-fetching + re-rendering the
  // template editor's launch-profile dropdowns — but only while that editor is
  // still open behind it (onManageProfilesClosed guards on that). Covers both
  // close paths: the Close button and a backdrop click.
  $('#profiles-manage-close').addEventListener('click', onManageProfilesClosed);
  $('#profiles-manage-modal').addEventListener('click', (e) => {
    if (e.target === $('#profiles-manage-modal')) onManageProfilesClosed();
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
  // Process phase rows: add / remove / reorder (JOH-242, delegated — the
  // container re-renders on every mutation).
  $('#template-editor-add-phase').addEventListener('click', () => {
    scrapeEditorAgents();
    scrapeEditorPattern();
    scrapeEditorProcess();
    templateEditorProcess.push({ name: '', roles: [], criteria: '' });
    renderEditorProcess();
  });
  $('#template-editor-process').addEventListener('click', e => {
    const btn = e.target.closest('.tpp-remove, .tpp-up, .tpp-down');
    if (!btn) return;
    const idx = parseInt(btn.closest('.template-process-row').dataset.idx, 10);
    scrapeEditorProcess();
    const arr = templateEditorProcess;
    if (btn.classList.contains('tpp-remove')) arr.splice(idx, 1);
    else if (btn.classList.contains('tpp-up') && idx > 0) [arr[idx - 1], arr[idx]] = [arr[idx], arr[idx - 1]];
    else if (btn.classList.contains('tpp-down') && idx < arr.length - 1) [arr[idx], arr[idx + 1]] = [arr[idx + 1], arr[idx]];
    renderEditorProcess();
  });
  // Rhythm rows: add / remove / reorder (JOH-244, delegated — the container
  // re-renders on every mutation, so scrape all sibling editors first).
  $('#template-editor-add-rhythm').addEventListener('click', () => {
    scrapeEditorAgents();
    scrapeEditorPattern();
    scrapeEditorProcess();
    scrapeEditorRhythms();
    templateEditorRhythms.push({ name: '', target_role: '', interval: '', cron_expr: '', subject: '', body: '' });
    renderEditorRhythms();
  });
  $('#template-editor-rhythms').addEventListener('click', e => {
    const btn = e.target.closest('.trh-remove, .trh-up, .trh-down');
    if (!btn) return;
    const idx = parseInt(btn.closest('.template-rhythm-row').dataset.idx, 10);
    scrapeEditorRhythms();
    const arr = templateEditorRhythms;
    if (btn.classList.contains('trh-remove')) arr.splice(idx, 1);
    else if (btn.classList.contains('trh-up') && idx > 0) [arr[idx - 1], arr[idx]] = [arr[idx], arr[idx - 1]];
    else if (btn.classList.contains('trh-down') && idx < arr.length - 1) [arr[idx], arr[idx + 1]] = [arr[idx + 1], arr[idx]];
    renderEditorRhythms();
  });
  // Capture the dirty handle so the editor's "Edit with agent" button can offer
  // the discard confirm before it hands off to a scribe (JOH-361).
  templateEditorDiscard = bindBackdropDiscard('template-editor-modal', closeTemplateEditor);
  $('#scribe-editor-open').addEventListener('click', summonScribeFromEditor);
  // The summoning-circle editor is user-resizable, exactly like the spawn /
  // clone dialogs (JOH-357): drag the bottom-right grip on both axes, size
  // persists per modal via dashPrefs. See makeModalResizable + the paired
  // #template-editor-modal .cron-create-modal { resize } rule in dashboard.css.
  makeModalResizable($('#template-editor-modal .cron-create-modal'), 'tclaude.dash.modalSize.template-editor');
  // The summoning-circles management PANEL itself is resizable too — a long
  // roster of circles (or a small screen) makes the fixed panel cramped. It is
  // a scrollable LIST, not a form, so it wires with { fitContent: false }: keep
  // the persist/restore, drop the form-only content-tracking min-size + auto-
  // grow (which would make a long list un-shrinkable and fight the 2s live
  // refresh). Paired #templates-manage-modal .manage-modal { resize } rule in
  // dashboard.css supplies the resize + the fixed min-height floor.
  makeModalResizable($('#templates-manage-modal .manage-modal'), 'tclaude.dash.modalSize.templates-manage', { fitContent: false });

  // Instantiate modal.
  $('#template-instantiate-cancel').addEventListener('click', closeInstantiateModal);
  $('#template-instantiate-submit').addEventListener('click', submitInstantiate);
  $('#template-instantiate-template').addEventListener('change', renderInstantiatePreview);
  $('#template-instantiate-group').addEventListener('input', renderInstantiatePreview);
  bindBackdropDiscard('template-instantiate-modal', closeInstantiateModal);

  // Deploy modal (JOH-245). The mission drives the group-name prefill until
  // the human edits the group field; the template select refreshes both the
  // prefill and the preview.
  $('#template-deploy-cancel').addEventListener('click', closeDeployModal);
  $('#template-deploy-submit').addEventListener('click', submitDeploy);
  $('#template-deploy-mission').addEventListener('input', syncDeployGroupPrefill);
  $('#template-deploy-template').addEventListener('change', syncDeployGroupPrefill);
  $('#template-deploy-group').addEventListener('input', () => { deployGroupEdited = true; renderDeployPreview(); });
  bindBackdropDiscard('template-deploy-modal', closeDeployModal);

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
  openTemplatesManageModal,
};
