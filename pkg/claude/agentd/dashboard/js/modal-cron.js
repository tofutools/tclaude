// modal-cron.js — the legacy cron create/edit modal. Its shared target
// picker and agent chooser are Preact-owned; this module talks to them only
// through the dependency-free message/access dialog controller.

import { $, $$, esc } from './helpers.js';
import { wizWord } from './slop.js';
import { formatJobInterval } from './jobs-format.js';
import { featureState } from './feature-state-registry.js';
import { lastSnapshot } from './dashboard.js';
import { bindBackdropDiscard } from './refresh.js';
import {
  configureCronTargetPicker, pickAgent, readCronTargetPicker,
  setCronTargetModeListener,
} from './message-access-dialog-controller.js';

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
let cronOriginalExpr = '';           // edit mode: the job's cron_expr on open ('' = interval job)
let cronScopeGroup = '';

// Keep the accessible title in the active theme as well as the visible title.
// Wizard CSS paints the short ritual heading, while this underlying text is
// what aria-labelledby exposes and what regular mode reveals after a live
// theme flip.
function renderCronTitle() {
  const title = $('#cron-create-title');
  if (cronEditId != null) {
    title.textContent = wizWord('Edit cron job', 'Re-bind the recurring ritual');
  } else if (cronScopeGroup) {
    title.textContent = wizWord(
      `Schedule a cron job for group "${cronScopeGroup}"`,
      `Bind a recurring ritual for party "${cronScopeGroup}"`,
    );
  } else {
    title.textContent = wizWord('Schedule a cron job', 'Bind a recurring ritual');
  }
}

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
  cronOriginalExpr = '';
  cronScopeGroup = (prefill && prefill.scopeGroup) || '';
  renderCronTitle();
  // Mode flag (create vs edit) for the 🧙 wizard title/submit ::before copy.
  // It reflects the modal's MODE, not the theme. CSS swaps the painted heading;
  // renderCronTitle keeps its underlying accessible name in the same voice.
  $('#cron-create-modal').classList.remove('cron-editing');
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
  cronScopeGroup = '';
  // Baseline the change-detection on the stable agent_id (conv-id
  // fallback for a pre-identity job), matching jobToPrefill's target
  // prefill — so reopening an edit and saving without touching the
  // target doesn't spuriously re-send it (JOH-312).
  cronOriginalTarget = job.target_agent || job.target_conv || '';
  cronOriginalGroupID = job.group_id || 0;
  cronOriginalExpr = job.cron_expr || '';
  renderCronTitle();
  // Edit mode: the wizard title/submit ::before copy reads "Re-bind…" (see the
  // .cron-editing rules in the wizard CSS block). Mode flag, not a theme read.
  $('#cron-create-modal').classList.add('cron-editing');
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
    interval: formatJobInterval(job.interval_seconds) || '',
    cronExpr: job.cron_expr || '',
    subject: job.subject || '',
    body: job.body || '',
    enabled: !!job.enabled,
    role: isGroup ? (job.target_role || '') : '',
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
  // Schedule mode: a job with a cron expression opens in expression mode
  // (with an immediate explainer fetch so the edit dialog shows what the
  // stored expression means); everything else in interval mode.
  const cronExpr = p.cronExpr || '';
  $('#cron-create-cron').value = cronExpr;
  setScheduleMode(cronExpr ? 'cron' : 'interval');
  $('#cron-create-cron-explain').innerHTML = '';
  if (cronExpr) scheduleCronExplain(0);
  // Target — shared solo/group picker. Accepts { targetMode, target,
  // groupName } straight off the prefill object. scopeGroup, set only
  // by a group header's "⏰ multicast" button, locks the picker to
  // that group: the dropdown cannot retarget, and Solo mode offers
  // only that group's members. Absent (global "+ new cron job", a
  // member ⏰, an edit) → the picker is unrestricted, as before.
  configureCronTargetPicker(p);
  // Role filter (JOH-244): the controlled target root notifies bindCronModal
  // when its mode changes so the legacy scheduling form can reveal this row.
  $('#cron-create-role').value = p.role || '';
  $('#cron-create-error').textContent = '';
}

function setSelectedChip(value) {
  const chips = $$('#cron-create-chips button');
  chips.forEach(c => c.classList.toggle('selected', c.dataset.chip === value));
}

// --- schedule mode (interval chips ⟷ cron expression) ----------------

function setScheduleMode(mode) {
  $$('input[name=cron-create-schedule-mode]').forEach(r => {
    r.checked = r.value === mode;
  });
  $('#cron-create-schedule-interval').style.display = mode === 'cron' ? 'none' : '';
  $('#cron-create-schedule-cron').style.display = mode === 'cron' ? '' : 'none';
}

function getScheduleMode() {
  return ($$('input[name=cron-create-schedule-mode]:checked')[0] || {}).value || 'interval';
}

// The cron-expression "auto explainer": a debounced POST /api/cron/explain
// as the user types, rendering the English description + concrete next
// fire times (in the browser's locale) under the input — or the parse
// error while the expression is invalid. The seq counter discards
// out-of-order responses so a slow explain for an old keystroke can't
// overwrite the answer for the current one.
let cronExplainTimer = null;
let cronExplainSeq = 0;

function scheduleCronExplain(delay) {
  clearTimeout(cronExplainTimer);
  cronExplainTimer = setTimeout(runCronExplain, delay === undefined ? 350 : delay);
}

async function runCronExplain() {
  const box = $('#cron-create-cron-explain');
  const expr = $('#cron-create-cron').value.trim();
  const seq = ++cronExplainSeq;
  if (!expr) { box.innerHTML = ''; return; }
  let resp;
  try {
    const r = await fetch('/api/cron/explain', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ expr }),
    });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    resp = await r.json();
  } catch (e) {
    if (seq === cronExplainSeq) {
      box.innerHTML = `<span class="cron-explain-error">explain failed: ${esc(e.message || String(e))}</span>`;
    }
    return;
  }
  if (seq !== cronExplainSeq) return; // stale — a newer keystroke owns the box
  if (!resp.valid) {
    box.innerHTML = `<span class="cron-explain-error">${esc(resp.error || 'invalid expression')}</span>`;
    return;
  }
  const fires = (resp.next || [])
    .map(t => esc(new Date(t).toLocaleString()))
    .join(' · ');
  box.innerHTML =
    (resp.description ? `<div class="cron-explain-desc">${esc(resp.description)}</div>` : '') +
    (fires ? `<div>next: ${fires}</div>` : '') +
    `<div>evaluated in the daemon's timezone (${esc(resp.tz || 'local')}) unless the expression carries CRON_TZ=</div>`;
}


function showCronCreateModal() {
  $('#cron-create-modal').classList.add('show');
  setTimeout(() => $('#cron-create-name').focus(), 0);
}
function closeCronCreateModal() {
  $('#cron-create-modal').classList.remove('show');
  cronEditId = null;
  cronScopeGroup = '';
  // Drop controlled target/scope state with the modal. The next open seeds an
  // authoritative target from its own prefill; a prior group scope must not
  // leak into a later global launch while the form is hidden.
  configureCronTargetPicker({});
  // Orphan any in-flight explain so a late response can't write into
  // the (hidden) box the next open would otherwise inherit.
  cronExplainSeq++;
  clearTimeout(cronExplainTimer);
}

// submitCronForm POSTs (create) or PATCHes (edit). On success, the
// dashboard refreshes; on `Save & create another`, the form resets
// for the next entry instead.
async function submitCronForm(keepOpen) {
  const errEl = $('#cron-create-error');
  errEl.textContent = '';
  const name = $('#cron-create-name').value.trim();
  const owner = $('#cron-create-owner').value.trim();
  const { mode, target } = readCronTargetPicker();
  // Role filter (JOH-244) applies only to a group target.
  const role = mode === 'group' ? $('#cron-create-role').value.trim() : '';
  const interval = $('#cron-create-interval').value.trim();
  const schedMode = getScheduleMode();
  const cronExpr = $('#cron-create-cron').value.trim();
  const subject = $('#cron-create-subject').value.trim();
  const bodyText = $('#cron-create-body').value;
  const enabled = $('#cron-create-enabled').checked;

  // Client-side gates with inline errors — same shape as the sudo
  // modal's #sudo-grant-error pattern. Daemon does authoritative
  // validation too, so we don't need to enumerate every rule here.
  if (!target) {
    // Scoped solo mode has no free-text input / 🔍 — its empty case
    // is an empty group, so the instruction must not mention them.
    const scopedSolo = mode === 'solo' && !!cronScopeGroup;
    errEl.textContent = mode === 'group'
      ? wizWord(
          'Pick a group from the dropdown (or create one first via the Groups tab).',
          'Pick a party from the dropdown (or form one first via the Groups tab).',
        )
      : scopedSolo
        ? wizWord(
            'This group has no members to nudge — switch to Group (multicast), or add a member to the group first.',
            'This party has no familiars to nudge — switch to Party (multicast), or invite a familiar first.',
          )
        : 'Target is required — type an agt_ id / title / conv-id or use 🔍 to pick.';
    return;
  }
  if (!bodyText) {
    errEl.textContent = 'Body is required (the message text the cron job sends).';
    return;
  }
  if (schedMode === 'cron' && !cronExpr) {
    errEl.textContent = 'Cron expression is required — type one (e.g. */5 * * * *) or switch back to Interval.';
    return;
  }
  if (!cronEditId && schedMode === 'interval' && !interval) {
    errEl.textContent = 'Schedule is required — click a chip or type a custom duration.';
    return;
  }
  if (cronEditId && schedMode === 'interval' && !interval && cronOriginalExpr) {
    errEl.textContent = 'Type an interval (e.g. 10m) — switching away from the cron expression needs one.';
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
      // Schedule: send the active mode's value; the daemon keeps the
      // exactly-one-mode invariant (an interval clears cron_expr and vice
      // versa). Interval mode sends cron_expr:'' explicitly so editing an
      // expression job over to interval mode actually switches it.
      if (schedMode === 'cron') {
        patch.cron_expr = cronExpr;
      } else if (interval) {
        patch.interval = interval;
        patch.cron_expr = '';
      }
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
      // Role filter (JOH-244) — always send in group mode so clearing it
      // back to whole-group persists. Only meaningful for a group target.
      if (mode === 'group') patch.role = role;
      r = await fetch(`/api/cron/${cronEditId}`, {
        method: 'PATCH', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(patch),
      });
    } else {
      const payload = { name, target, subject, body: bodyText, enabled };
      if (schedMode === 'cron') payload.cron_expr = cronExpr;
      else payload.interval = interval;
      if (owner) payload.owner = owner;
      if (mode === 'group' && role) payload.role = role;
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
    // into lastSnapshot.cron (legacy edit lookup) and publish it through the
    // Jobs Signals model. The next /api/jobs fetch replaces the full window.
    if (lastSnapshot) {
      lastSnapshot.cron = lastSnapshot.cron || [];
      const idx = lastSnapshot.cron.findIndex(j => j.id === resp.id);
      if (idx >= 0) lastSnapshot.cron[idx] = resp;
      else lastSnapshot.cron.push(resp);
    }
    featureState('jobs')?.upsertCron(resp);
    // A newly-created row may not belong in the active query/page. Refetch the
    // authoritative window (also while "save another" keeps this modal open)
    // instead of locally inserting a row that violates those constraints.
    void refresh();
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
  } catch (e) {
    errEl.textContent = 'Network error: ' + (e.message || e);
  } finally {
    submitBtn.disabled = false; otherBtn.disabled = false;
  }
}

function bindCronModal() {
  $('#cron-create-cancel').addEventListener('click', closeCronCreateModal);
  $('#cron-create-submit').addEventListener('click', () => submitCronForm(false));
  $('#cron-create-save-another').addEventListener('click', () => submitCronForm(true));
  bindBackdropDiscard('cron-create-modal', closeCronCreateModal);
  // Preact owns the shared target picker. This legacy form retains only the
  // role row associated with group fan-out and follows mode through the
  // controller rather than observing picker DOM.
  setCronTargetModeListener((mode) => {
    const roleRow = $('#cron-create-role-row');
    if (roleRow) roleRow.style.display = mode === 'group' ? '' : 'none';
  });
  document.addEventListener('tclaude:wizard', () => {
    if ($('#cron-create-modal').classList.contains('show')) renderCronTitle();
  });
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
  // Schedule-mode radios swap the chips+interval body for the cron
  // expression body; typing an expression re-runs the debounced explainer.
  $$('input[name=cron-create-schedule-mode]').forEach(rdo => {
    rdo.addEventListener('change', () => setScheduleMode(rdo.value));
  });
  $('#cron-create-cron').addEventListener('input', () => scheduleCronExplain());
  // Owner picker reuses the Preact-owned shared agent chooser.
  $('#cron-create-owner-pick').addEventListener('click', async () => {
    // Owner is also addressed by the stable agent_id the picker returns.
    const picked = await pickAgent({ title: 'Pick owner', identity: 'agent' });
    if (picked) $('#cron-create-owner').value = picked;
  });
}

export {
  openCronCreateModal, openCronEditModal, bindCronModal,
};
