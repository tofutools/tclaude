// modal-cron.js — the sudo-grant and cron-job modals.
//
// The sudo-grant modal, the cron create/edit modal, and the shared
// group/member target picker (used by this modal and the message
// modal). Extracted from dashboard.js in the Stage 2 module split.

import { $, $$, esc, shortId, shortAgentId } from './helpers.js';
import { renderCronTab, formatInterval } from './tabs.js';
// lastSnapshot / sudoBadge live in dashboard.js; refresh() / toast and
// the sudo state (sudoGrantBlocklist, sudoByConv) live in refresh.js.
// Imported back here — deliberate, benign cycles (see render.js).
// TDZ-safe: no top-level code reads them; the modal functions touch
// them only when invoked, after every module has finished evaluating.
import { lastSnapshot, sudoBadge } from './dashboard.js';
import { refresh, toast, sudoGrantBlocklist, sudoByConv, bindBackdropDiscard } from './refresh.js';

// openSudoGrantModal: builds the slug picker from the snapshot's
// registry, restores the conv field from a per-page memory so
// reopening keeps focus, and traps Escape to close. Submission
// hits POST /api/sudo and falls through to refresh() on success
// so the new grant lands on the list immediately.
function openSudoGrantModal(prefillConv) {
  const slugs = (lastSnapshot && lastSnapshot.slugs) || [];
  const wrap = $('#sudo-grant-slugs');
  wrap.innerHTML = slugs.map(s => {
    const blocked = sudoGrantBlocklist.includes(s.slug);
    return `<label class="${blocked ? 'blocked' : ''}" title="${esc(s.descr || '')}">
      <input type="checkbox" value="${esc(s.slug)}"${blocked ? ' disabled' : ''}>
      ${esc(s.slug)}
    </label>`;
  }).join('');
  wrap.querySelectorAll('input[type=checkbox]').forEach(cb => {
    cb.addEventListener('change', () => {
      cb.parentElement.classList.toggle('checked', cb.checked);
    });
  });
  if (prefillConv != null) $('#sudo-grant-conv').value = prefillConv;
  $('#sudo-grant-error').textContent = '';
  $('#sudo-grant-modal').classList.add('show');
  setTimeout(() => $('#sudo-grant-conv').focus(), 0);
}
function closeSudoGrantModal() {
  $('#sudo-grant-modal').classList.remove('show');
}

async function submitSudoGrant() {
  const conv = $('#sudo-grant-conv').value.trim();
  const slugs = $$('#sudo-grant-slugs input[type=checkbox]:checked').map(cb => cb.value);
  const duration = $('#sudo-grant-duration').value.trim();
  const reason = $('#sudo-grant-reason').value.trim();
  const errEl = $('#sudo-grant-error');
  errEl.textContent = '';
  if (!conv) { errEl.textContent = 'Conv is required.'; return; }
  if (!slugs.length) { errEl.textContent = 'Pick at least one slug.'; return; }
  const btn = $('#sudo-grant-submit');
  btn.disabled = true;
  try {
    const r = await fetch('/api/sudo', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ conv, slugs, duration, reason }),
    });
    if (!r.ok) {
      const text = await r.text();
      errEl.textContent = text || ('HTTP ' + r.status);
      return;
    }
    const resp = await r.json();
    const ok = (resp.grants || []).filter(g => g.id > 0).length;
    const failed = (resp.grants || []).length - ok;
    toast(`Granted ${ok} slug${ok === 1 ? '' : 's'} to ${resp.conv_id ? shortId(resp.conv_id) : conv}` +
      (failed > 0 ? ` (${failed} failed)` : ''));
    closeSudoGrantModal();
    await refresh();
  } catch (e) {
    errEl.textContent = 'Network error: ' + (e.message || e);
  } finally {
    btn.disabled = false;
  }
}

// Per-row sudo-revoke is handled by bindRowActions (data-act="sudo-revoke").
// The Grant modal hooks into bindSudoModal below.

// pickSudoAgentModal opens a filtered agent picker and resolves to
// the chosen conv-id (or "" on cancel). Reuses the .add-member-modal
// CSS shape so the look matches the existing "Add member" overlay.
// Simpler than addMemberModal: no per-group "exclude existing" — the
// daemon's policy already handles policy enforcement on submit.
function pickSudoAgentModal() {
  return new Promise(resolve => {
    const overlay = $('#sudo-pick-agent-modal');
    const search = $('#sudo-pick-agent-search');
    const list = $('#sudo-pick-agent-list');
    const includeAll = $('#sudo-pick-agent-all');
    search.value = '';
    includeAll.checked = false;
    let highlight = 0;
    let candidates = [];

    function buildCandidates() {
      const out = [];
      const seen = new Set();
      for (const a of (lastSnapshot?.agents || [])) {
        if (!a.conv_id || seen.has(a.conv_id)) continue;
        if (!includeAll.checked && !a.online) continue;
        seen.add(a.conv_id);
        out.push(a);
      }
      out.sort((a, b) => {
        if (!!b.online !== !!a.online) return (b.online ? 1 : 0) - (a.online ? 1 : 0);
        return (a.title || '').localeCompare(b.title || '');
      });
      return out;
    }

    function applyFilter(rows, q) {
      if (!q) return rows;
      const needle = q.toLowerCase();
      return rows.filter(a =>
        (a.title || '').toLowerCase().includes(needle) ||
        (a.conv_id || '').toLowerCase().includes(needle) ||
        (a.groups || []).some(g => g.toLowerCase().includes(needle)));
    }

    function render() {
      candidates = applyFilter(buildCandidates(), search.value);
      if (highlight >= candidates.length) highlight = Math.max(0, candidates.length - 1);
      if (highlight < 0) highlight = 0;
      if (!candidates.length) {
        list.innerHTML = '<div class="add-member-empty">No matching conversations. ' +
          (includeAll.checked
            ? '(Try a different filter.)'
            : '(Try ticking "Include offline / archived" for a wider pool.)') +
          '</div>';
        return;
      }
      list.innerHTML = candidates.map((a, i) => {
        const dot = a.online
          ? '<span class="online" title="online">●</span>'
          : '<span class="offline" title="offline">○</span>';
        const groups = (a.groups || []).length
          ? `<span class="groups-tag">in: ${esc((a.groups || []).join(', '))}</span>`
          : '';
        // Surface the 🔓 badge inline so the human can see who
        // already holds active grants while picking — useful for
        // "extend alice's window" without a tab switch.
        const badge = sudoBadge(sudoByConv[a.conv_id], a.conv_id);
        return `<div class="add-member-row${i === highlight ? ' highlighted' : ''}" data-i="${i}">` +
               `${dot}<span class="rowname">${esc(a.title || '(unnamed)')}</span>` +
               `<span class="id">${esc(shortId(a.conv_id))}</span>${badge}${groups}` +
               `</div>`;
      }).join('');
      const hl = list.querySelector('.add-member-row.highlighted');
      if (hl) hl.scrollIntoView({block: 'nearest'});
    }

    function close(convID) {
      overlay.classList.remove('show');
      document.removeEventListener('keydown', onKey, true);
      resolve(convID || '');
    }

    function onKey(e) {
      if (!overlay.classList.contains('show')) return;
      // Capture-phase + stopImmediatePropagation so Escape closes only
      // this picker — never the form modal underneath that opened it.
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopImmediatePropagation();
        close('');
      }
      else if (e.key === 'ArrowDown') { e.preventDefault(); highlight++; render(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); highlight--; render(); }
      else if (e.key === 'Enter') {
        e.preventDefault();
        const c = candidates[highlight];
        if (c) close(c.conv_id);
      }
    }

    list.onclick = (e) => {
      const row = e.target.closest('.add-member-row');
      if (!row) return;
      const i = parseInt(row.dataset.i, 10);
      const c = candidates[i];
      if (c) close(c.conv_id);
    };
    search.oninput = () => { highlight = 0; render(); };
    includeAll.onchange = render;
    overlay.onclick = (e) => { if (e.target === overlay) close(''); };
    document.addEventListener('keydown', onKey, true);

    overlay.classList.add('show');
    render();
    setTimeout(() => search.focus(), 0);
  });
}

// -- Cron create / edit form ----------------------------------------
//
// One modal serves both Create and Edit. Edit mode prefills every
// field from the selected row and routes the submit to PATCH; create
// mode starts blank and POSTs. The "Save & create another" button
// posts then resets the form for the next entry — handy when building
// out a multi-cron team in one sitting.
//
// Target has two radio modes: Solo agent (text input + agent picker
// overlay) or Group (dropdown of group names from the snapshot).
// Owner is always a text input + optional picker — humans rarely
// need to override the default, but per-agent self-nudge cron jobs
// benefit from prefill (owner = the agent itself).

// Active edit id; null in create mode. Reset every time the modal
// opens or closes.
let cronEditId = null;
let cronOriginalTarget = null;       // for PATCH: only send `target` if user changed it
let cronOriginalGroupID = null;      // ditto for `group_id`

// openCronCreateModal opens the modal in create mode. prefill is an
// optional object: { targetMode, target, owner, name, subject, body,
// interval, enabled }. All optional. Used by the context-aware
// entry-point buttons (Agents row ⏰, Groups member ⏰, group header ⏰)
// to drop the relevant defaults into the form before the user opens
// it.
function openCronCreateModal(prefill) {
  cronEditId = null;
  cronOriginalTarget = null;
  cronOriginalGroupID = null;
  const scopeGroup = prefill && prefill.scopeGroup;
  $('#cron-create-title').textContent = scopeGroup
    ? `Schedule a cron job for group "${scopeGroup}"`
    : 'Schedule a cron job';
  $('#cron-create-meta').style.display = 'none';
  $('#cron-create-submit').textContent = 'Create';
  $('#cron-create-save-another').style.display = '';
  populateCronForm(prefill || {});
  showCronCreateModal();
}

// openCronEditModal opens the modal in edit mode, prefilled from the
// selected cron job row. Submit PATCHes /api/cron/{id} instead of
// POSTing /api/cron.
function openCronEditModal(job) {
  cronEditId = job.id;
  // Baseline the change-detection on the stable agent_id (conv-id
  // fallback for a pre-identity job), matching jobToPrefill's target
  // prefill — so reopening an edit and saving without touching the
  // target doesn't spuriously re-send it (JOH-312).
  cronOriginalTarget = job.target_agent || job.target_conv || '';
  cronOriginalGroupID = job.group_id || 0;
  $('#cron-create-title').textContent = 'Edit cron job';
  const meta = $('#cron-create-meta');
  meta.style.display = 'block';
  meta.textContent = `#${job.id} · ${job.name || '(unnamed)'}`;
  $('#cron-create-submit').textContent = 'Save';
  // Edit mode hides "Save & create another" — that pattern only
  // makes sense for create.
  $('#cron-create-save-another').style.display = 'none';
  populateCronForm(jobToPrefill(job));
  showCronCreateModal();
}

function jobToPrefill(job) {
  // target_kind, not group_id>0: a conv-target job routed through a
  // shared group also has a non-zero group_id but is not a multicast.
  const isGroup = job.target_kind === 'group';
  return {
    name: job.name || '',
    // Lead owner/target with the stable agent_id (conv-id fallback for a
    // pre-identity job) — the same rotation-immune handle the 🔍 picker
    // now fills and that cronOriginalTarget baselines against, so an
    // untouched target round-trips without a spurious re-send (JOH-312).
    owner: job.owner_agent || job.owner_conv || '',
    targetMode: isGroup ? 'group' : 'solo',
    target: isGroup ? '' : (job.target_agent || job.target_conv || ''),
    groupName: isGroup ? (job.group_name || '') : '',
    interval: formatInterval(job.interval_seconds) || '',
    subject: job.subject || '',
    body: job.body || '',
    enabled: !!job.enabled,
  };
}

function populateCronForm(p) {
  $('#cron-create-name').value = p.name || '';
  $('#cron-create-owner').value = p.owner || '';
  $('#cron-create-subject').value = p.subject || '';
  $('#cron-create-body').value = p.body || '';
  $('#cron-create-enabled').checked = p.enabled === undefined ? true : !!p.enabled;
  // Interval: prefill the text input; if it matches a chip preset,
  // visually highlight the chip too.
  const interval = p.interval || '';
  $('#cron-create-interval').value = interval;
  setSelectedChip(interval);
  // Target — shared solo/group picker. Accepts { targetMode, target,
  // groupName } straight off the prefill object. scopeGroup, set only
  // by a group header's "⏰ multicast" button, locks the picker to
  // that group: the dropdown cannot retarget, and Solo mode offers
  // only that group's members. Absent (global "+ new cron job", a
  // member ⏰, an edit) → the picker is unrestricted, as before.
  setTargetPickerScope('cron-create', p.scopeGroup);
  populateTargetPicker('cron-create', p);
  $('#cron-create-error').textContent = '';
}

function setSelectedChip(value) {
  const chips = $$('#cron-create-chips button');
  chips.forEach(c => c.classList.toggle('selected', c.dataset.chip === value));
}

// --- shared solo/group target picker --------------------------------
// A solo-agent / group-multicast target selector shared by the cron
// form and the one-shot message form, so the two never drift. Each
// host passes a unique idPrefix; the markup + element ids are derived
// from it (e.g. prefix "cron-create" → #cron-create-target,
// #cron-create-group, radio group name "cron-create-target-mode"), so
// a host's own JS can still address fields directly. The host places
// an empty <div id="${prefix}-target-mount"> in its modal markup;
// bindTargetPicker mounts the picker into it once at page init.
//
// targetPickerScopes[prefix] — when set to a group name, the picker
// is "scoped" to that group: Group mode locks its dropdown to that
// one group, and Solo mode offers a <select> of only that group's
// members instead of the all-agents free-text input + 🔍. The
// selection then cannot structurally leave the group. setTargetPicker‑
// Scope arms / clears it; only the cron form's group-multicast entry
// point ("⏰ multicast" on a group header) sets a scope today.
const targetPickerScopes = {};
function setTargetPickerScope(prefix, groupName) {
  if (groupName) targetPickerScopes[prefix] = groupName;
  else delete targetPickerScopes[prefix];
  // Keep the group dropdown's locked (disabled) state in sync with
  // the scope right here — so it can never be left disabled by an
  // earlier scoped open, independent of when populateTargetPickerGroups
  // next runs.
  const sel = $('#' + prefix + '-group');
  if (sel) sel.disabled = !!groupName;
}

function targetPickerMarkup(prefix) {
  return `
    <div class="cron-target-modes">
      <label><input type="radio" name="${prefix}-target-mode" value="solo" checked /> Solo agent</label>
      <label><input type="radio" name="${prefix}-target-mode" value="group" /> Group (multicast)</label>
    </div>
    <div class="cron-target-input-row" id="${prefix}-target-solo">
      <input id="${prefix}-target" type="text" placeholder="agt_ id / title / conv-id / 8+-char prefix" autocomplete="off" spellcheck="false" />
      <button type="button" id="${prefix}-target-pick" title="Pick from agent list">🔍</button>
    </div>
    <!-- Scoped solo row — shown instead of the free-text input when
         the picker is scoped to a group (setTargetPickerScope): a
         <select> of just that group's members, so a scoped solo
         target cannot structurally leave the group. -->
    <div class="cron-target-input-row" id="${prefix}-target-scoped" style="display:none">
      <select id="${prefix}-scoped-member"></select>
    </div>
    <div class="cron-target-input-row" id="${prefix}-target-group" style="display:none">
      <select id="${prefix}-group"></select>
    </div>`;
}

// bindTargetPicker mounts the picker markup into #${prefix}-target-mount
// (idempotent) and wires the mode radios + the 🔍 agent-picker button.
function bindTargetPicker(prefix) {
  const mount = $('#' + prefix + '-target-mount');
  if (mount && !mount.dataset.mounted) {
    mount.innerHTML = targetPickerMarkup(prefix);
    mount.dataset.mounted = '1';
  }
  $$('input[name=' + prefix + '-target-mode]').forEach(rdo => {
    rdo.addEventListener('change', () => setTargetPickerMode(prefix, rdo.value, false));
  });
  $('#' + prefix + '-target-pick').addEventListener('click', async () => {
    // pickCronTargetModal resolves to the picked agent's stable agent_id
    // (conv-id fallback) — the rotation-immune target token (JOH-312).
    const picked = await pickCronTargetModal();
    if (picked) $('#' + prefix + '-target').value = picked;
  });
}

function setTargetPickerMode(prefix, mode, populateOnly) {
  const solo = mode === 'solo';
  const scoped = !!targetPickerScopes[prefix];
  // Solo mode shows the free-text input + 🔍 normally, or — when the
  // picker is scoped to a group — a <select> of that group's members.
  $('#' + prefix + '-target-solo').style.display = (solo && !scoped) ? '' : 'none';
  $('#' + prefix + '-target-scoped').style.display = (solo && scoped) ? '' : 'none';
  $('#' + prefix + '-target-group').style.display = solo ? 'none' : '';
  if (!populateOnly) {
    if (solo && scoped) populateTargetPickerMembers(prefix);
    else if (!solo) populateTargetPickerGroups(prefix);
  }
}

function populateTargetPickerGroups(prefix) {
  const sel = $('#' + prefix + '-group');
  const scope = targetPickerScopes[prefix];
  // Scoped → the dropdown is locked to the one scoped group, so a
  // scoped multicast cannot be retargeted to a different group.
  const groups = scope
    ? [scope]
    : (lastSnapshot?.groups || []).map(g => g.name).sort();
  const prev = sel.value;
  sel.innerHTML = groups.length
    ? groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('')
    : '<option value="">(no groups — create one first)</option>';
  if (prev && groups.includes(prev)) sel.value = prev;
  sel.disabled = !!scope;
}

// populateTargetPickerMembers fills the scoped-mode solo <select>
// with the members of the scoped group — the structural guarantee
// that a scoped solo target can only ever be a member of that group.
function populateTargetPickerMembers(prefix) {
  const sel = $('#' + prefix + '-scoped-member');
  const scope = targetPickerScopes[prefix];
  const g = (lastSnapshot?.groups || []).find(x => x.name === scope);
  const members = (g && g.members) || [];
  const prev = sel.value;
  // Key the option on the stable agent_id (conv-id fallback for a
  // pre-identity member) so a scoped solo target submits the rotation-
  // immune handle, like the free-text picker (JOH-312).
  sel.innerHTML = members.length
    ? members.map(m => `<option value="${esc(m.agent_id || m.conv_id)}">${esc(m.title || m.conv_id)}${m.online ? '' : ' (offline)'}</option>`).join('')
    : '<option value="">(no members in this group)</option>';
  if (prev && members.some(m => (m.agent_id || m.conv_id) === prev)) sel.value = prev;
}

// populateTargetPicker fills the picker from a prefill object
// { targetMode, target, groupName } — all optional, defaulting to a
// blank solo target.
function populateTargetPicker(prefix, p) {
  const mode = (p && p.targetMode) || 'solo';
  $$('input[name=' + prefix + '-target-mode]').forEach(r => {
    r.checked = r.value === mode;
  });
  setTargetPickerMode(prefix, mode, /*populateOnly=*/true);
  if (mode === 'solo') {
    if (targetPickerScopes[prefix]) {
      // Scoped solo → pick from the group's member <select>. Honour a
      // prefilled target only when it is actually a member.
      populateTargetPickerMembers(prefix);
      const sel = $('#' + prefix + '-scoped-member');
      const want = (p && p.target) || '';
      if (want && Array.from(sel.options).some(o => o.value === want)) {
        sel.value = want;
      } else if (sel.options.length) {
        sel.selectedIndex = 0;
      }
    } else {
      $('#' + prefix + '-target').value = (p && p.target) || '';
    }
  } else {
    populateTargetPickerGroups(prefix);
    const sel = $('#' + prefix + '-group');
    const want = (p && p.groupName) || '';
    // Preserve a target group that is no longer in the snapshot
    // (archived / deleted since the job was created) as an explicit
    // "(missing)" option — silently falling back to the first group
    // would, on a cron edit-save, reroute the job to the wrong group.
    const found = Array.from(sel.options).some(o => o.value === want);
    if (want && !found) {
      const opt = document.createElement('option');
      opt.value = want;
      opt.textContent = `${want} (missing)`;
      sel.prepend(opt);
    }
    if (want) sel.value = want;
    else if (sel.options.length) sel.selectedIndex = 0;
  }
}

// readTargetPicker returns { mode, target } where target is a raw
// solo selector or a "group:NAME" multicast token, or "" when the
// picker has no usable value (the caller surfaces the inline error).
function readTargetPicker(prefix) {
  const mode = ($$('input[name=' + prefix + '-target-mode]:checked')[0] || {}).value || 'solo';
  let target = '';
  if (mode === 'solo') {
    target = targetPickerScopes[prefix]
      ? $('#' + prefix + '-scoped-member').value.trim()
      : $('#' + prefix + '-target').value.trim();
  } else {
    const g = $('#' + prefix + '-group').value;
    if (g) target = 'group:' + g;
  }
  return { mode, target };
}

function showCronCreateModal() {
  $('#cron-create-modal').classList.add('show');
  setTimeout(() => $('#cron-create-name').focus(), 0);
}
function closeCronCreateModal() {
  $('#cron-create-modal').classList.remove('show');
  cronEditId = null;
  // Drop the scope so the registry's lifetime matches the modal's;
  // the next open re-arms it from its prefill regardless.
  setTargetPickerScope('cron-create', null);
}

// submitCronForm POSTs (create) or PATCHes (edit). On success, the
// dashboard refreshes; on `Save & create another`, the form resets
// for the next entry instead.
async function submitCronForm(keepOpen) {
  const errEl = $('#cron-create-error');
  errEl.textContent = '';
  const name = $('#cron-create-name').value.trim();
  const owner = $('#cron-create-owner').value.trim();
  const { mode, target } = readTargetPicker('cron-create');
  const interval = $('#cron-create-interval').value.trim();
  const subject = $('#cron-create-subject').value.trim();
  const bodyText = $('#cron-create-body').value;
  const enabled = $('#cron-create-enabled').checked;

  // Client-side gates with inline errors — same shape as the sudo
  // modal's #sudo-grant-error pattern. Daemon does authoritative
  // validation too, so we don't need to enumerate every rule here.
  if (!target) {
    // Scoped solo mode has no free-text input / 🔍 — its empty case
    // is an empty group, so the instruction must not mention them.
    const scopedSolo = mode === 'solo' && !!targetPickerScopes['cron-create'];
    errEl.textContent = mode === 'group'
      ? 'Pick a group from the dropdown (or create one first via the Groups tab).'
      : scopedSolo
        ? 'This group has no members to nudge — switch to Group (multicast), or add a member to the group first.'
        : 'Target is required — type an agt_ id / title / conv-id or use 🔍 to pick.';
    return;
  }
  if (!bodyText) {
    errEl.textContent = 'Body is required (the message text the cron job sends).';
    return;
  }
  if (!cronEditId && !interval) {
    errEl.textContent = 'Schedule is required — click a chip or type a custom duration.';
    return;
  }

  const submitBtn = $('#cron-create-submit');
  const otherBtn = $('#cron-create-save-another');
  submitBtn.disabled = true; otherBtn.disabled = true;
  try {
    let r;
    if (cronEditId) {
      const patch = { name, body: bodyText, subject, enabled };
      if (owner) patch.owner = owner;
      if (interval) patch.interval = interval;
      // Only send target/group_id if the user actually changed
      // them — avoids re-resolving and possibly tripping validation
      // on an unchanged field.
      if (mode === 'solo' && target !== cronOriginalTarget) {
        patch.target = target;
        patch.group_id = 0;
      } else if (mode === 'group') {
        // Switching to group mode (or staying in it with a different
        // pick): send target=group:<name>; daemon resolves to the
        // group's conv set on its own.
        patch.target = target;
      }
      r = await fetch(`/api/cron/${cronEditId}`, {
        method: 'PATCH', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(patch),
      });
    } else {
      const payload = { name, target, interval, subject, body: bodyText, enabled };
      if (owner) payload.owner = owner;
      r = await fetch('/api/cron', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
    }
    if (!r.ok) {
      errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
      return;
    }
    const resp = await r.json();
    const verb = cronEditId ? 'saved' : 'created';
    toast(`cron ${verb}: ${resp.name || ('#' + (resp.id || ''))}`);
    // Optimistic insert/update so the table updates before the next
    // 2s snapshot poll. We just got the canonical row back; splice it
    // into lastSnapshot.cron and re-render.
    if (lastSnapshot) {
      lastSnapshot.cron = lastSnapshot.cron || [];
      const idx = lastSnapshot.cron.findIndex(j => j.id === resp.id);
      if (idx >= 0) lastSnapshot.cron[idx] = resp;
      else lastSnapshot.cron.push(resp);
      renderCronTab();
    }
    if (keepOpen) {
      // Reset body + name for the next entry; keep target/schedule
      // since "create another" is usually batch-style.
      $('#cron-create-name').value = '';
      $('#cron-create-subject').value = '';
      $('#cron-create-body').value = '';
      $('#cron-create-name').focus();
      return;
    }
    closeCronCreateModal();
    // Fire a snapshot refresh so anything we missed (e.g. server
    // re-routed the job through a shared group) gets picked up.
    refresh();
  } catch (e) {
    errEl.textContent = 'Network error: ' + (e.message || e);
  } finally {
    submitBtn.disabled = false; otherBtn.disabled = false;
  }
}

// pickCronTargetModal opens a filtered candidate list. Reuses the
// .add-member-modal CSS. Mode "agent" → solo conv pool (matches the
// sudo picker); mode "group" → would surface groups but in v1 we
// already have a <select> for groups, so this is agent-only. Returns
// the picked agent's stable agent_id — the rotation-immune target token
// (conv-id fallback for a pre-identity agent; "" on cancel) (JOH-312).
function pickCronTargetModal() {
  return new Promise(resolve => {
    const overlay = $('#cron-pick-target-modal');
    const search = $('#cron-pick-target-search');
    const list = $('#cron-pick-target-list');
    const includeAll = $('#cron-pick-target-all');
    search.value = '';
    includeAll.checked = false;
    let highlight = 0;
    let candidates = [];

    function buildCandidates() {
      const out = [];
      const seen = new Set();
      for (const a of (lastSnapshot?.agents || [])) {
        if (!a.conv_id || seen.has(a.conv_id)) continue;
        if (!includeAll.checked && !a.online) continue;
        seen.add(a.conv_id);
        out.push(a);
      }
      out.sort((a, b) => {
        if (!!b.online !== !!a.online) return (b.online ? 1 : 0) - (a.online ? 1 : 0);
        return (a.title || '').localeCompare(b.title || '');
      });
      return out;
    }

    function applyFilter(rows, q) {
      if (!q) return rows;
      const needle = q.toLowerCase();
      // Match agent_id too — it's the value this picker now leads with and
      // returns, so a human pasting an agt_ id must be able to find a row.
      return rows.filter(a =>
        (a.title || '').toLowerCase().includes(needle) ||
        (a.agent_id || '').toLowerCase().includes(needle) ||
        (a.conv_id || '').toLowerCase().includes(needle) ||
        (a.groups || []).some(g => g.toLowerCase().includes(needle)));
    }

    function render() {
      candidates = applyFilter(buildCandidates(), search.value);
      if (highlight >= candidates.length) highlight = Math.max(0, candidates.length - 1);
      if (highlight < 0) highlight = 0;
      if (!candidates.length) {
        list.innerHTML = '<div class="add-member-empty">No matching conversations. ' +
          (includeAll.checked
            ? '(Try a different filter.)'
            : '(Try ticking "Include offline / archived" for a wider pool.)') +
          '</div>';
        return;
      }
      list.innerHTML = candidates.map((a, i) => {
        const dot = a.online
          ? '<span class="online" title="online">●</span>'
          : '<span class="offline" title="offline">○</span>';
        const groups = (a.groups || []).length
          ? `<span class="groups-tag">in: ${esc((a.groups || []).join(', '))}</span>`
          : '';
        // Lead the id column with the stable agent_id (conv-id prefix as
        // the fallback), conv-id on hover — matching what this picker now
        // returns and the agent-led cron-list / message-member rows.
        return `<div class="add-member-row${i === highlight ? ' highlighted' : ''}" data-i="${i}">` +
               `${dot}<span class="rowname">${esc(a.title || '(unnamed)')}</span>` +
               `<span class="id" title="${esc(a.conv_id)}">${esc(shortAgentId(a.agent_id, a.conv_id))}</span>${groups}` +
               `</div>`;
      }).join('');
      const hl = list.querySelector('.add-member-row.highlighted');
      if (hl) hl.scrollIntoView({block: 'nearest'});
    }

    // The resolved value is the picked agent's stable agent_id (conv-id
    // fallback) — the rotation-immune target token, not a generation.
    function close(agentID) {
      overlay.classList.remove('show');
      document.removeEventListener('keydown', onKey, true);
      resolve(agentID || '');
    }
    function onKey(e) {
      if (!overlay.classList.contains('show')) return;
      // Capture-phase + stopImmediatePropagation so Escape closes only
      // this picker — never the form modal underneath that opened it.
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopImmediatePropagation();
        close('');
      }
      else if (e.key === 'ArrowDown') { e.preventDefault(); highlight++; render(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); highlight--; render(); }
      else if (e.key === 'Enter') {
        e.preventDefault();
        const c = candidates[highlight];
        if (c) close(c.agent_id || c.conv_id);
      }
    }
    list.onclick = (e) => {
      const row = e.target.closest('.add-member-row');
      if (!row) return;
      const i = parseInt(row.dataset.i, 10);
      const c = candidates[i];
      if (c) close(c.agent_id || c.conv_id);
    };
    search.oninput = () => { highlight = 0; render(); };
    includeAll.onchange = render;
    overlay.onclick = (e) => { if (e.target === overlay) close(''); };
    document.addEventListener('keydown', onKey, true);
    overlay.classList.add('show');
    render();
    setTimeout(() => search.focus(), 0);
  });
}

function bindCronModal() {
  $('#cron-create-open').addEventListener('click', () => openCronCreateModal({}));
  $('#cron-create-cancel').addEventListener('click', closeCronCreateModal);
  $('#cron-create-submit').addEventListener('click', () => submitCronForm(false));
  $('#cron-create-save-another').addEventListener('click', () => submitCronForm(true));
  bindBackdropDiscard('cron-create-modal', closeCronCreateModal);
  // Solo/group target picker — markup + mode radios + 🔍 button.
  bindTargetPicker('cron-create');
  // Schedule chips push value into the text input + highlight.
  $('#cron-create-chips').addEventListener('click', (e) => {
    const b = e.target.closest('button[data-chip]');
    if (!b) return;
    const v = b.dataset.chip;
    $('#cron-create-interval').value = v;
    setSelectedChip(v);
  });
  // Typing in the custom interval input clears the chip highlight.
  $('#cron-create-interval').addEventListener('input', () => {
    setSelectedChip($('#cron-create-interval').value.trim());
  });
  // Owner picker reuses the cron-pick-target overlay (the target
  // picker's own 🔍 is wired by bindTargetPicker above).
  $('#cron-create-owner-pick').addEventListener('click', async () => {
    // Owner is also addressed by the stable agent_id the picker returns.
    const picked = await pickCronTargetModal();
    if (picked) $('#cron-create-owner').value = picked;
  });
}

export {
  openSudoGrantModal, closeSudoGrantModal, submitSudoGrant, pickSudoAgentModal,
  openCronCreateModal, openCronEditModal, pickCronTargetModal, bindCronModal,
  bindTargetPicker, populateTargetPicker, readTargetPicker,
};
