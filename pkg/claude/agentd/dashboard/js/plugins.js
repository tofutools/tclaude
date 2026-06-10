// plugins.js — the Plugins tab: renderer, click actions, and the
// create/edit modal.
//
// A plugin is a human-defined bundle of steps; each step is a `check`
// shell command (exit 0 = satisfied), a `run` shell command that
// performs the step, and/or a `stop` command that undoes it (see
// plugins.go). The tab renders one card per installed plugin with
// per-step status and run/stop buttons, plus a card-level
// activate/deactivate toggle (deactivate runs the stop commands in
// reverse and marks the plugin off so its failing checks stop
// warning), plus a catalog section of built-in definitions ready for
// one-click install. The nav button carries a warning badge when any
// enabled plugin has a failing check — "installed but not active" at a
// glance from any tab — or when a DISABLED plugin still has a
// stoppable step running.

import { $, esc, relTime } from './helpers.js';
// lastSnapshot lives in dashboard.js; refresh()/toast/confirmModal in
// refresh.js. Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, toast, confirmModal, bindBackdropDiscard } from './refresh.js';

// -- rendering ----------------------------------------------------------

// pluginStatusPill colorises the aggregate plugin status from the
// snapshot (see dashboardPlugin in plugins.go for the semantics).
// Reuses the cron pill palette so status reads the same dashboard-wide.
// A disabled plugin reads "off" — its failing checks are intentional —
// and only warns when deactivation didn't take.
function pluginStatusPill(p) {
  if (p.disabled) {
    return p.status === 'warn'
      ? '<span class="state-pill state-awaiting" title="deactivated, but a stoppable step still passes its check — press deactivate again or stop the step below">still active</span>'
      : '<span class="state-pill state-offline" title="deactivated on purpose — press activate to bring it back">off</span>';
  }
  if (p.status === 'ok') return '<span class="state-pill state-working" title="every check passes">active</span>';
  if (p.status === 'warn') return '<span class="state-pill state-awaiting" title="at least one check fails — activate the plugin or run the failing step(s) below">not active</span>';
  return '<span class="state-pill state-offline" title="no check has run yet (or no steps define one)">unknown</span>';
}

// stepStatusDot is the per-step ●/○ in the steps table.
function stepStatusDot(s) {
  if (!s.check) return '<span class="muted" title="no check command — run-only step">—</span>';
  if (!s.checked) return '<span class="offline" title="not checked yet">○</span>';
  const when = s.checked_at ? ' (checked ' + relTime(s.checked_at) + ')' : '';
  return s.ok
    ? `<span class="online" title="check passes${esc(when)}">●</span>`
    : `<span class="plugin-dot-fail" title="check fails${esc(when)}">●</span>`;
}

// cmdCell renders a shell command, truncated with the full text on
// hover, so long docker invocations don't blow the table apart.
function cmdCell(cmd) {
  if (!cmd) return '<span class="muted">—</span>';
  const trunc = cmd.length > 70 ? cmd.slice(0, 70) + '…' : cmd;
  return `<code class="plugin-cmd" title="${esc(cmd)}">${esc(trunc)}</code>`;
}

// busyActions tracks in-flight plugin actions (keyed act:name:idx) so
// the 2s snapshot re-render can't resurrect an enabled button while
// its command — possibly a minutes-long docker pull — is still
// running. renderPluginCard re-disables any button whose key is here.
const busyActions = new Set();
const busyKey = (act, name, idx) => `${act}:${name}:${idx ?? ''}`;

// busyAttrs renders the disabled state + busy label for a button that
// has an in-flight action, or the normal label otherwise.
function busyAttrs(act, name, idx, label, busyLabel) {
  return busyActions.has(busyKey(act, name, idx))
    ? `disabled>${busyLabel}`
    : `>${label}`;
}

function renderPluginCard(p) {
  const steps = (p.steps || []).map((s, i) => {
    const runBtn = s.run
      ? `<button data-act="plugin-run-step" data-name="${esc(p.name)}" data-idx="${i}" title="Run this step's command now:\n${esc(s.run)}" ${busyAttrs('plugin-run-step', p.name, i, 'run', 'running…')}</button>`
      : '';
    const stopBtn = s.stop
      ? `<button data-act="plugin-stop-step" data-name="${esc(p.name)}" data-idx="${i}" title="Run this step's stop command now:\n${esc(s.stop)}" ${busyAttrs('plugin-stop-step', p.name, i, 'stop', 'stopping…')}</button>`
      : '';
    const out = s.output
      ? `<span class="muted plugin-out" title="${esc(s.output)}">${esc(s.output.split('\n')[0].slice(0, 60))}</span>`
      : '';
    return `
      <tr>
        <td>${stepStatusDot(s)}</td>
        <td><span class="rowname">${esc(s.name)}</span></td>
        <td>${cmdCell(s.check)}</td>
        <td>${cmdCell(s.run)}</td>
        <td>${cmdCell(s.stop)}</td>
        <td>${out}</td>
        <td><div class="row-actions">${runBtn}${stopBtn}</div></td>
      </tr>`;
  }).join('');
  // The toggle button mirrors intent: an enabled plugin offers
  // deactivate (stop commands, reverse order); a disabled one offers
  // activate (run commands, forward, skipping satisfied steps).
  const toggleBtn = p.disabled
    ? `<button class="primary" data-act="plugin-activate" data-name="${esc(p.name)}" title="Re-enable this plugin: run its steps in order (steps whose check already passes are skipped)" ${busyAttrs('plugin-activate', p.name, '', 'activate', 'activating…')}</button>`
    : `<button data-act="plugin-deactivate" data-name="${esc(p.name)}" title="Temporarily disable this plugin: run each step's stop command in reverse order and mark it off (checks keep running but stop warning)" ${busyAttrs('plugin-deactivate', p.name, '', 'deactivate', 'deactivating…')}</button>`;
  return `
    <div class="plugin-card">
      <div class="plugin-head">
        ${pluginStatusPill(p)}
        <span class="rowname">${esc(p.name)}</span>
        ${p.descr ? `<span class="muted">${esc(p.descr)}</span>` : ''}
        <span class="spacer"></span>
        <div class="row-actions">
          ${toggleBtn}
          <button data-act="plugin-check" data-name="${esc(p.name)}" title="Re-run this plugin's check commands now" ${busyAttrs('plugin-check', p.name, '', 'check', 'checking…')}</button>
          <button data-act="plugin-edit" data-name="${esc(p.name)}" title="Edit this plugin's definition">edit</button>
          <button class="danger" data-act="plugin-delete" data-name="${esc(p.name)}" title="Remove this plugin definition (does not undo anything its steps did)">delete</button>
        </div>
      </div>
      <table class="plugin-steps">
        <thead><tr><th></th><th>step</th><th>check</th><th>run</th><th>stop</th><th>last output</th><th></th></tr></thead>
        <tbody>${steps}</tbody>
      </table>
    </div>`;
}

// renderCatalog lists built-in definitions whose name is not yet
// installed. Installing copies the definition into plugins.json — from
// then on it's the human's own, freely editable.
function renderCatalog(catalog, installedNames) {
  const avail = (catalog || []).filter(c => !installedNames.has(c.name));
  if (!avail.length) return '';
  const cards = avail.map(c => `
    <div class="plugin-card plugin-catalog-card">
      <div class="plugin-head">
        <span class="tag">catalog</span>
        <span class="rowname">${esc(c.name)}</span>
        ${c.descr ? `<span class="muted">${esc(c.descr)}</span>` : ''}
        <span class="spacer"></span>
        <button class="primary" data-act="plugin-install" data-name="${esc(c.name)}" title="Add this plugin to your installed set (you can edit it afterwards)" ${busyAttrs('plugin-install', c.name, '', '+ install', 'installing…')}</button>
      </div>
      <ul class="plugin-catalog-steps">
        ${(c.steps || []).map(s => `<li><span class="rowname">${esc(s.name)}</span> ${cmdCell(s.run || s.check)}</li>`).join('')}
      </ul>
    </div>`).join('');
  return `<h3 class="plugins-section-head">Available from catalog</h3>${cards}`;
}

function pluginMatches(p, needle) {
  if ((p.name || '').toLowerCase().includes(needle)) return true;
  if ((p.descr || '').toLowerCase().includes(needle)) return true;
  return (p.steps || []).some(s =>
    ((s.name || '').toLowerCase().includes(needle)) ||
    ((s.check || '').toLowerCase().includes(needle)) ||
    ((s.run || '').toLowerCase().includes(needle)) ||
    ((s.stop || '').toLowerCase().includes(needle)));
}

export function renderPluginsTab() {
  if (!lastSnapshot) return;
  // A broken plugins.json arrives as plugins_error with an empty list —
  // show the real failure instead of pretending nothing is installed.
  if (lastSnapshot.plugins_error) {
    $('#plugins-list').innerHTML =
      `<div class="empty">⚠ plugin registry unreadable: <code>${esc(lastSnapshot.plugins_error)}</code> — fix or delete ~/.tclaude/plugins.json</div>`;
    $('#filter-plugins-count').textContent = '';
    return;
  }
  const q = ($('#filter-plugins').value || '').toLowerCase();
  const all = lastSnapshot.plugins || [];
  const catalog = lastSnapshot.plugins_catalog || [];
  const installed = q ? all.filter(p => pluginMatches(p, q)) : all;
  const installedNames = new Set(all.map(p => p.name));

  let html = installed.map(renderPluginCard).join('');
  if (!all.length) {
    html = '<div class="empty">No plugins installed yet. Install one from the catalog below, or define your own with <strong>+ new plugin</strong>.</div>';
  } else if (q && !installed.length) {
    html = '<div class="empty">No plugin matches the filter.</div>';
  }
  const shownCatalog = q ? catalog.filter(c => pluginMatches(c, q)) : catalog;
  html += renderCatalog(shownCatalog, installedNames);
  $('#plugins-list').innerHTML = html;
  $('#filter-plugins-count').textContent = q
    ? `${installed.length} / ${all.length}`
    : `${all.length} plugin${all.length === 1 ? '' : 's'}`;
}

export function renderPluginsBadge(warn) {
  const badge = $('#plugins-badge');
  if (!badge) return;
  badge.textContent = warn > 99 ? '99+' : String(warn);
  badge.hidden = !warn;
}

// -- modal (create + edit) ----------------------------------------------

let pluginModalState = { mode: 'create', originalName: null };

// addStepRow appends one editable step block to the modal. `step` pre-
// fills it for edit mode.
function addStepRow(step) {
  step = step || {};
  const wrap = document.createElement('fieldset');
  wrap.className = 'plugin-step-edit';
  wrap.innerHTML = `
    <div class="plugin-step-edit-head">
      <span class="muted plugin-step-edit-n"></span>
      <span class="spacer"></span>
      <button type="button" class="danger" data-step-remove title="Remove this step">remove</button>
    </div>
    <label class="cron-create-row">
      <span class="cron-create-label">Name</span>
      <input type="text" data-step-name placeholder="e.g. canvas server (docker)" autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Check</span>
      <textarea data-step-check rows="2" placeholder="shell command — exit 0 means this step is satisfied (optional)" spellcheck="false"></textarea>
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Run</span>
      <textarea data-step-run rows="2" placeholder="shell command that performs the step (optional)" spellcheck="false"></textarea>
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Stop</span>
      <textarea data-step-stop rows="2" placeholder="shell command that undoes the step — powers deactivate (optional)" spellcheck="false"></textarea>
    </label>`;
  wrap.querySelector('[data-step-name]').value = step.name || '';
  wrap.querySelector('[data-step-check]').value = step.check || '';
  wrap.querySelector('[data-step-run]').value = step.run || '';
  wrap.querySelector('[data-step-stop]').value = step.stop || '';
  wrap.querySelector('[data-step-remove]').addEventListener('click', () => {
    wrap.remove();
    renumberStepRows();
  });
  $('#plugin-modal-steps').appendChild(wrap);
  renumberStepRows();
  return wrap;
}

function renumberStepRows() {
  [...document.querySelectorAll('#plugin-modal-steps .plugin-step-edit-n')]
    .forEach((el, i) => { el.textContent = 'step ' + (i + 1); });
}

function openPluginModal(plugin) {
  const editing = !!plugin;
  pluginModalState = { mode: editing ? 'edit' : 'create', originalName: editing ? plugin.name : null };
  $('#plugin-modal-title').textContent = editing ? 'Edit plugin' : 'New plugin';
  $('#plugin-modal-submit').textContent = editing ? 'Save changes' : 'Create plugin';
  $('#plugin-modal-name').value = editing ? plugin.name : '';
  $('#plugin-modal-descr').value = editing ? (plugin.descr || '') : '';
  $('#plugin-modal-error').textContent = '';
  $('#plugin-modal-steps').innerHTML = '';
  (editing && plugin.steps && plugin.steps.length ? plugin.steps : [{}]).forEach(addStepRow);
  $('#plugin-modal').classList.add('show');
  setTimeout(() => $('#plugin-modal-name').focus(), 0);
}

function closePluginModal() {
  $('#plugin-modal').classList.remove('show');
  // Modals suspend the auto-refresh poll; nudge one now so the tab
  // doesn't sit stale until the next tick.
  refresh();
}

function collectPluginModal() {
  const steps = [...document.querySelectorAll('#plugin-modal-steps .plugin-step-edit')].map(row => ({
    name: row.querySelector('[data-step-name]').value.trim(),
    check: row.querySelector('[data-step-check]').value.trim(),
    run: row.querySelector('[data-step-run]').value.trim(),
    stop: row.querySelector('[data-step-stop]').value.trim(),
  }));
  return {
    name: $('#plugin-modal-name').value.trim(),
    descr: $('#plugin-modal-descr').value.trim(),
    steps,
  };
}

async function submitPluginModal() {
  const errEl = $('#plugin-modal-error');
  errEl.textContent = '';
  const submit = $('#plugin-modal-submit');
  const body = collectPluginModal();
  submit.disabled = true;
  try {
    const editing = pluginModalState.mode === 'edit';
    const url = editing
      ? `/api/plugins/${encodeURIComponent(pluginModalState.originalName)}`
      : '/api/plugins';
    const r = await fetch(url, {
      method: editing ? 'PUT' : 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    closePluginModal();
    toast(editing ? `plugin ${body.name} saved` : `plugin ${body.name} created`);
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submit.disabled = false;
  }
}

// -- actions ------------------------------------------------------------

// withBusy disables a button and swaps its label while an async action
// is in flight, so a slow docker pull can't be double-fired. `key`
// (from busyKey) keeps the disabled state across snapshot re-renders —
// the live btn node may be replaced mid-action, so the direct DOM
// tweak only covers the first 2 seconds.
async function withBusy(key, btn, busyLabel, fn) {
  if (busyActions.has(key)) return;
  busyActions.add(key);
  const orig = btn.textContent;
  btn.disabled = true;
  btn.textContent = busyLabel;
  try {
    await fn();
  } finally {
    busyActions.delete(key);
    btn.disabled = false;
    btn.textContent = orig;
  }
}

function findSnapshotPlugin(name) {
  return ((lastSnapshot && lastSnapshot.plugins) || []).find(p => p.name === name);
}

// bindPluginsUI wires the tab's static buttons, the modal, and one
// delegated listener for the per-card / per-step actions. row-actions'
// own delegated listener ignores these acts (unknown to its switch),
// so the two routers coexist.
export function bindPluginsUI() {
  $('#plugin-create-open').addEventListener('click', () => openPluginModal(null));
  $('#plugin-modal-cancel').addEventListener('click', closePluginModal);
  $('#plugin-modal-submit').addEventListener('click', submitPluginModal);
  $('#plugin-modal-add-step').addEventListener('click', () => addStepRow());
  bindBackdropDiscard('plugin-modal', closePluginModal);

  $('#plugins-check-now').addEventListener('click', (e) => withBusy(busyKey('plugins-check-all', '', ''), e.target, 'checking…', async () => {
    const r = await fetch('/api/plugins/check', { method: 'POST', credentials: 'same-origin' });
    if (!r.ok) { toast('check failed: ' + ((await r.text()) || r.status), true); return; }
    const data = await r.json();
    toast(data.warn ? `checks done — ${data.warn} plugin${data.warn === 1 ? '' : 's'} not active` : 'checks done — all plugins active');
    refresh();
  }));

  document.addEventListener('click', async (e) => {
    const btn = e.target.closest('[data-act^="plugin-"]');
    if (!btn) return;
    const act = btn.getAttribute('data-act');
    const name = btn.getAttribute('data-name');
    try {
      switch (act) {
        case 'plugin-check': {
          await withBusy(busyKey(act, name, ''), btn, 'checking…', async () => {
            const r = await fetch(`/api/plugins/${encodeURIComponent(name)}/check`, { method: 'POST', credentials: 'same-origin' });
            if (!r.ok) { toast('check failed: ' + ((await r.text()) || r.status), true); return; }
            const p = await r.json();
            toast(`${name}: ${p.status === 'ok' ? 'all checks pass' : p.status === 'warn' ? 'some checks fail' : 'status unknown'}`, p.status === 'warn');
            refresh();
          });
          break;
        }
        case 'plugin-edit': {
          const p = findSnapshotPlugin(name);
          if (p) openPluginModal(p);
          break;
        }
        case 'plugin-delete': {
          const confirmed = await confirmModal({
            title: 'Delete plugin?',
            body: `Remove the plugin definition "${name}" from plugins.json. This does NOT stop containers or unregister MCPs its steps set up — it only forgets the definition.`,
            okLabel: 'Delete',
          });
          if (!confirmed) return;
          const r = await fetch(`/api/plugins/${encodeURIComponent(name)}`, { method: 'DELETE', credentials: 'same-origin' });
          if (!r.ok) { toast('delete failed: ' + ((await r.text()) || r.status), true); return; }
          toast(`plugin ${name} deleted`);
          refresh();
          break;
        }
        case 'plugin-run-step':
        case 'plugin-stop-step': {
          const idx = btn.getAttribute('data-idx');
          const verb = act === 'plugin-stop-step' ? 'stop' : 'run';
          const busyLabel = verb === 'stop' ? 'stopping…' : 'running…';
          await withBusy(busyKey(act, name, idx), btn, busyLabel, async () => {
            const r = await fetch(`/api/plugins/${encodeURIComponent(name)}/steps/${encodeURIComponent(idx)}/${verb}`, { method: 'POST', credentials: 'same-origin' });
            if (!r.ok) { toast(verb + ' failed: ' + ((await r.text()) || r.status), true); return; }
            const res = await r.json();
            const firstLine = (res.output || '').split('\n')[0].slice(0, 120);
            toast(res.ok ? `step ${verb} OK${firstLine ? ': ' + firstLine : ''}` : `step ${verb} FAILED${firstLine ? ': ' + firstLine : ''}`, !res.ok);
            refresh();
          });
          break;
        }
        case 'plugin-activate':
        case 'plugin-deactivate': {
          const verb = act === 'plugin-activate' ? 'activate' : 'deactivate';
          const busyLabel = verb === 'activate' ? 'activating…' : 'deactivating…';
          await withBusy(busyKey(act, name, ''), btn, busyLabel, async () => {
            const r = await fetch(`/api/plugins/${encodeURIComponent(name)}/${verb}`, { method: 'POST', credentials: 'same-origin' });
            if (!r.ok) { toast(verb + ' failed: ' + ((await r.text()) || r.status), true); return; }
            const res = await r.json();
            const firstLine = (res.output || '').split('\n')[0].slice(0, 120);
            if (!res.ok) {
              toast(`${name} ${verb} had failures${firstLine ? ': ' + firstLine : ''} — see step outputs`, true);
            } else if (!res.ran) {
              toast(`${name} ${verb}d (nothing to ${verb === 'activate' ? 'run — all steps already satisfied' : 'stop — nothing was active'})`);
            } else {
              toast(`${name} ${verb}d`);
            }
            refresh();
          });
          break;
        }
        case 'plugin-install': {
          const def = ((lastSnapshot && lastSnapshot.plugins_catalog) || []).find(c => c.name === name);
          if (!def) return;
          await withBusy(busyKey(act, name, ''), btn, 'installing…', async () => {
            const r = await fetch('/api/plugins', {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(def),
            });
            if (!r.ok) { toast('install failed: ' + ((await r.text()) || r.status), true); return; }
            toast(`plugin ${name} installed — press activate to bring it up`);
            refresh();
          });
          break;
        }
      }
    } catch (err) {
      toast((err && err.message) || String(err), true);
    }
  });
}
