// plugins.js — the Plugins tab: renderer, click actions, and the
// create/edit modal.
//
// A plugin is a human-defined bundle of steps; each step is a `check`
// shell command (exit 0 = satisfied), a `run` shell command that
// performs the step, and/or a `stop` command that undoes it (see
// plugins.go). The tab renders one card per installed plugin, plus a
// catalog section of built-in definitions ready for one-click install.
//
// Power control mirrors the Groups tab's agent dot: the status lamps
// ARE the buttons. The card-level lamp toggles the whole plugin
// (activate = run steps forward, skipping satisfied ones; deactivate =
// run stop commands in reverse and persist the off intent); each
// step's lamp runs or stops that one step. The nav button carries a
// warning badge when any enabled plugin has a failing check —
// "installed but not active" at a glance from any tab — or when a
// DISABLED plugin still has a stoppable step running.

import { $, esc, relTime } from './helpers.js';
import { morphInto } from './morph.js';
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
      ? '<span class="state-pill state-awaiting" title="deactivated, but a stoppable step still passes its check — click the lamp to retry the teardown">still active</span>'
      : '<span class="state-pill state-offline" title="deactivated on purpose — click the lamp to bring it back">off</span>';
  }
  if (p.status === 'ok') return '<span class="state-pill state-working" title="every check passes — click the lamp to deactivate">active</span>';
  if (p.status === 'warn') return '<span class="state-pill state-awaiting" title="at least one check fails — click the lamp to activate">not active</span>';
  return '<span class="state-pill state-offline" title="no check has run yet (or no steps define one)">unknown</span>';
}

// lampAttrs renders a status-lamp button's glyph, or the in-flight
// marker + disabled state while its action runs (same busyActions
// machinery as the text buttons, so the 2s re-render can't resurrect
// a clickable lamp mid-command).
function lampAttrs(act, name, idx, glyph) {
  return busyActions.has(busyKey(act, name, idx))
    ? 'disabled>◐'
    : `>${glyph}`;
}

// pluginLamp is the card-level power lamp — like the agent dot on the
// Groups tab, the light IS the toggle. It shows the plugin's actual
// aggregate state and one click moves it toward the obvious desired
// state: green (active) → deactivate; everything else → activate,
// except a deactivated plugin that is STILL running, where the click
// retries the teardown.
function pluginLamp(p) {
  let glyph, cls, verb, tip;
  if (p.disabled) {
    if (p.status === 'warn') {
      glyph = '●'; cls = 'status-dot-warn'; verb = 'deactivate';
      tip = 'deactivated, but a stoppable step still passes its check — click to run the stop commands again';
    } else {
      glyph = '○'; cls = 'status-dot-offline'; verb = 'activate';
      tip = 'off — click to activate (runs each step in order, skipping satisfied ones)';
    }
  } else if (p.status === 'ok') {
    glyph = '●'; cls = 'status-dot-online'; verb = 'deactivate';
    tip = "active — click to deactivate (runs each step's stop command in reverse order and marks the plugin off)";
  } else if (p.status === 'warn') {
    glyph = '●'; cls = 'status-dot-warn'; verb = 'activate';
    tip = 'not active — click to activate (runs each step in order, skipping satisfied ones)';
  } else {
    glyph = '○'; cls = 'status-dot-offline'; verb = 'activate';
    tip = 'status unknown — click to activate (runs each step in order, skipping satisfied ones)';
  }
  return `<button type="button" class="status-dot ${cls}" data-act="plugin-toggle" data-name="${esc(p.name)}"` +
    ` data-verb="${verb}" title="${esc(tip)}" aria-label="${esc(tip)}" ${lampAttrs('plugin-toggle', p.name, '', glyph)}</button>`;
}

// stepLamp is the per-step ●/○ in the steps table — clickable when
// the step has something to do from its current state: a passing step
// with a stop command stops on click, a failing/unchecked one with a
// run command runs on click. Steps with nothing applicable render a
// plain (non-clickable) dot, same glyphs as before.
function stepLamp(p, s, i) {
  const when = (s.check && s.checked && s.checked_at) ? ' (checked ' + relTime(s.checked_at) + ')' : '';
  const active = !!(s.check && s.checked && s.ok);
  let glyph, cls, state;
  if (active) {
    glyph = '●'; cls = 'status-dot-online'; state = 'check passes' + when;
  } else if (s.check && s.checked) {
    // A failing check on a deactivated plugin is the intended state —
    // grey, not alarm-red.
    if (p.disabled) { glyph = '○'; cls = 'status-dot-offline'; state = 'check fails (plugin is off)' + when; }
    else { glyph = '●'; cls = 'plugin-dot-fail'; state = 'check fails' + when; }
  } else if (s.check) {
    glyph = '○'; cls = 'status-dot-offline'; state = 'not checked yet';
  } else {
    glyph = '—'; cls = 'status-dot-offline'; state = 'no check command — state unknown';
  }
  const verb = active ? (s.stop ? 'stop' : '') : (s.run ? 'run' : '');
  if (!verb) {
    return `<span class="${cls}" title="${esc(state)}">${glyph}</span>`;
  }
  const cmd = verb === 'stop' ? s.stop : s.run;
  const tip = `${state} — click to ${verb}:\n${cmd}`;
  return `<button type="button" class="status-dot ${cls}" data-act="plugin-step-toggle" data-name="${esc(p.name)}"` +
    ` data-idx="${i}" data-verb="${verb}" title="${esc(tip)}" aria-label="${esc(tip)}" ${lampAttrs('plugin-step-toggle', p.name, i, glyph)}</button>`;
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
    const out = s.output
      ? `<span class="muted plugin-out" title="${esc(s.output)}">${esc(s.output.split('\n')[0].slice(0, 60))}</span>`
      : '';
    return `
      <tr>
        <td>${stepLamp(p, s, i)}</td>
        <td><span class="rowname">${esc(s.name)}</span></td>
        <td>${cmdCell(s.check)}</td>
        <td>${cmdCell(s.run)}</td>
        <td>${cmdCell(s.stop)}</td>
        <td>${out}</td>
      </tr>`;
  }).join('');
  return `
    <div class="plugin-card" data-key="plugin-${esc(p.name)}">
      <div class="plugin-head">
        ${pluginLamp(p)}
        ${pluginStatusPill(p)}
        <span class="rowname">${esc(p.name)}</span>
        ${p.descr ? `<span class="muted">${esc(p.descr)}</span>` : ''}
        <span class="spacer"></span>
        <div class="row-actions">
          <button data-act="plugin-check" data-name="${esc(p.name)}" title="Re-run this plugin's check commands now" ${busyAttrs('plugin-check', p.name, '', 'check', 'checking…')}</button>
          <button data-act="plugin-edit" data-name="${esc(p.name)}" title="Edit this plugin's definition">edit</button>
          <button class="danger" data-act="plugin-delete" data-name="${esc(p.name)}" title="Remove this plugin definition (does not undo anything its steps did)">delete</button>
        </div>
      </div>
      <table class="plugin-steps">
        <thead><tr><th></th><th>step</th><th>check</th><th>run</th><th>stop</th><th>last output</th></tr></thead>
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
    <div class="plugin-card plugin-catalog-card" data-key="catalog-${esc(c.name)}">
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
    morphInto($('#plugins-list'),
      `<div class="empty">⚠ plugin registry unreadable: <code>${esc(lastSnapshot.plugins_error)}</code> — fix or delete ~/.tclaude/plugins.json</div>`);
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
  // Morph rather than swap so a selection in a plugin-cmd / last-output cell
  // survives the 2s tick (cards are keyed by name — see renderPluginCard).
  morphInto($('#plugins-list'), html);
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
        case 'plugin-step-toggle': {
          const idx = btn.getAttribute('data-idx');
          const verb = btn.getAttribute('data-verb'); // run | stop
          await withBusy(busyKey(act, name, idx), btn, '◐', async () => {
            const r = await fetch(`/api/plugins/${encodeURIComponent(name)}/steps/${encodeURIComponent(idx)}/${encodeURIComponent(verb)}`, { method: 'POST', credentials: 'same-origin' });
            if (!r.ok) { toast(verb + ' failed: ' + ((await r.text()) || r.status), true); return; }
            const res = await r.json();
            const firstLine = (res.output || '').split('\n')[0].slice(0, 120);
            toast(res.ok ? `step ${verb} OK${firstLine ? ': ' + firstLine : ''}` : `step ${verb} FAILED${firstLine ? ': ' + firstLine : ''}`, !res.ok);
            refresh();
          });
          break;
        }
        case 'plugin-toggle': {
          const verb = btn.getAttribute('data-verb'); // activate | deactivate
          await withBusy(busyKey(act, name, ''), btn, '◐', async () => {
            const r = await fetch(`/api/plugins/${encodeURIComponent(name)}/${encodeURIComponent(verb)}`, { method: 'POST', credentials: 'same-origin' });
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
            toast(`plugin ${name} installed — click its lamp to bring it up`);
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
