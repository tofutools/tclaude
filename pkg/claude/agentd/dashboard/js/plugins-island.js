import { Fragment, h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { AsyncLoadState } from './async-load-state.js';
import { relTime } from './helpers.js';
import { pluginBusyKey } from './plugins-state.js';

const html = htm.bind(h);

function statusPill(plugin) {
  if (plugin.disabled) return plugin.status === 'warn'
    ? { cls: 'state-awaiting', text: 'still active', title: 'deactivated, but a stoppable step still passes its check — click the lamp to retry the teardown' }
    : { cls: 'state-offline', text: 'off', title: 'deactivated on purpose — click the lamp to bring it back' };
  if (plugin.status === 'ok') return { cls: 'state-working', text: 'active', title: 'every check passes — click the lamp to deactivate' };
  if (plugin.status === 'warn') return { cls: 'state-awaiting', text: 'not active', title: 'at least one check fails — click the lamp to activate' };
  return { cls: 'state-offline', text: 'unknown', title: 'no check has run yet (or no steps define one)' };
}

function pluginLamp(plugin) {
  if (plugin.disabled) return plugin.status === 'warn'
    ? { glyph: '●', cls: 'status-dot-warn', verb: 'deactivate', tip: 'deactivated, but a stoppable step still passes its check — click to run the stop commands again' }
    : { glyph: '○', cls: 'status-dot-offline', verb: 'activate', tip: 'off — click to activate (runs each step in order, skipping satisfied ones)' };
  if (plugin.status === 'ok') return { glyph: '●', cls: 'status-dot-online', verb: 'deactivate', tip: "active — click to deactivate (runs each step's stop command in reverse order and marks the plugin off)" };
  if (plugin.status === 'warn') return { glyph: '●', cls: 'status-dot-warn', verb: 'activate', tip: 'not active — click to activate (runs each step in order, skipping satisfied ones)' };
  return { glyph: '○', cls: 'status-dot-offline', verb: 'activate', tip: 'status unknown — click to activate (runs each step in order, skipping satisfied ones)' };
}

function stepLamp(plugin, step) {
  const when = step.check && step.checked && step.checked_at ? ` (checked ${relTime(step.checked_at)})` : '';
  const active = !!(step.check && step.checked && step.ok);
  if (active) return { glyph: '●', cls: 'status-dot-online', state: `check passes${when}`, verb: step.stop ? 'stop' : '' };
  if (step.check && step.checked) return plugin.disabled
    ? { glyph: '○', cls: 'status-dot-offline', state: `check fails (plugin is off)${when}`, verb: step.run ? 'run' : '' }
    : { glyph: '●', cls: 'plugin-dot-fail', state: `check fails${when}`, verb: step.run ? 'run' : '' };
  if (step.check) return { glyph: '○', cls: 'status-dot-offline', state: 'not checked yet', verb: step.run ? 'run' : '' };
  return { glyph: '—', cls: 'status-dot-offline', state: 'no check command — state unknown', verb: step.run ? 'run' : '' };
}

function Command({ value }) {
  if (!value) return html`<span class="muted">—</span>`;
  const shown = value.length > 70 ? value.slice(0, 70) + '…' : value;
  return html`<code class="plugin-cmd" title=${value}>${shown}</code>`;
}

function StepRow({ plugin, step, index, actions, busy }) {
  const lamp = stepLamp(plugin, step);
  const key = pluginBusyKey('plugin-step-toggle', plugin.name, index);
  const isBusy = busy.has(key);
  const command = lamp.verb === 'stop' ? step.stop : step.run;
  const tip = lamp.verb ? `${lamp.state} — click to ${lamp.verb}:\n${command}` : lamp.state;
  return html`<tr>
    <td>${lamp.verb
      ? html`<button type="button" class=${`status-dot ${lamp.cls}`} disabled=${isBusy}
          title=${tip} aria-label=${tip}
          onClick=${() => actions.toggleStep(plugin, index, lamp.verb, key)}>${isBusy ? '◐' : lamp.glyph}</button>`
      : html`<span class=${lamp.cls} title=${tip}>${lamp.glyph}</span>`}</td>
    <td><span class="rowname">${step.name}</span></td>
    <td><${Command} value=${step.check} /></td>
    <td><${Command} value=${step.run} /></td>
    <td><${Command} value=${step.stop} /></td>
    <td>${step.output && html`<span class="muted plugin-out" title=${step.output}>${step.output.split('\n')[0].slice(0, 60)}</span>`}</td>
  </tr>`;
}

function PluginCard({ plugin, state, actions, busy }) {
  const pill = statusPill(plugin);
  const lamp = pluginLamp(plugin);
  const toggleKey = pluginBusyKey('plugin-toggle', plugin.name);
  const checkKey = pluginBusyKey('plugin-check', plugin.name);
  return html`<div class="plugin-card" data-key=${`plugin-${plugin.name}`}>
    <div class="plugin-head">
      <button type="button" class=${`status-dot ${lamp.cls}`} disabled=${busy.has(toggleKey)}
        title=${lamp.tip} aria-label=${lamp.tip}
        onClick=${() => actions.togglePlugin(plugin, lamp.verb, toggleKey)}>${busy.has(toggleKey) ? '◐' : lamp.glyph}</button>
      <span class=${`state-pill ${pill.cls}`} title=${pill.title}>${pill.text}</span>
      <span class="rowname">${plugin.name}</span>
      ${plugin.descr && html`<span class="muted">${plugin.descr}</span>`}
      <span class="spacer"></span>
      <div class="row-actions">
        <button disabled=${busy.has(checkKey)} onClick=${() => actions.checkPlugin(plugin, checkKey)}
          title="Re-run this plugin's check commands now">${busy.has(checkKey) ? 'checking…' : 'check'}</button>
        <button onClick=${() => state.openModal(plugin)} title="Edit this plugin's definition">edit</button>
        <button class="danger" onClick=${() => actions.deletePlugin(plugin, pluginBusyKey('plugin-delete', plugin.name))}
          title="Remove this plugin definition (does not undo anything its steps did)">delete</button>
      </div>
    </div>
    <table class="plugin-steps">
      <thead><tr><th></th><th>step</th><th>check</th><th>run</th><th>stop</th><th>last output</th></tr></thead>
      <tbody>${(plugin.steps || []).map((step, index) => html`
        <${StepRow} key=${`${plugin.name}:${index}`} plugin=${plugin} step=${step} index=${index} actions=${actions} busy=${busy} />
      `)}</tbody>
    </table>
  </div>`;
}

function Catalog({ plugins, actions, busy }) {
  if (!plugins.length) return null;
  return html`<${Fragment}>
    <h3 class="plugins-section-head">Available from catalog</h3>
    ${plugins.map((plugin) => {
      const key = pluginBusyKey('plugin-install', plugin.name);
      return html`<div class="plugin-card plugin-catalog-card" data-key=${`catalog-${plugin.name}`} key=${plugin.name}>
        <div class="plugin-head">
          <span class="tag">catalog</span><span class="rowname">${plugin.name}</span>
          ${plugin.descr && html`<span class="muted">${plugin.descr}</span>`}
          <span class="spacer"></span>
          <button class="primary" disabled=${busy.has(key)} onClick=${() => actions.install(plugin, key)}
            title="Add this plugin to your installed set (you can edit it afterwards)">${busy.has(key) ? 'installing…' : '+ install'}</button>
        </div>
        <ul class="plugin-catalog-steps">${(plugin.steps || []).map((step, index) => html`
          <li key=${`${plugin.name}:${index}`}><span class="rowname">${step.name}</span> <${Command} value=${step.run || step.check} /></li>
        `)}</ul>
      </div>`;
    })}
  </${Fragment}>`;
}

export function PluginsApp({ state, actions }) {
  const current = state.view.value;
  const inputRef = useRef(null);
  useEffect(() => {
    if (!current.request.hasLoaded) return;
    document.body.classList.toggle('hide-plugins', !current.visible);
    if (!current.visible && current.activeTab === 'plugins') {
      document.querySelector('nav [data-tab="groups"]')?.click();
    }
  }, [current.request.hasLoaded, current.visible, current.activeTab]);

  const count = current.query
    ? `${current.installed.length} / ${current.all.length}`
    : `${current.all.length} plugin${current.all.length === 1 ? '' : 's'}`;
  const checkKey = pluginBusyKey('plugins-check-all');
  return html`<div class="plugins-island">
    <div class="filter-bar">
      <input ref=${inputRef} id="filter-plugins" type="text" aria-label="Filter plugins"
        placeholder="Filter (name / descr / step name / command)" autocomplete="off" spellcheck=${false}
        value=${current.query} onInput=${(event) => state.setQuery(event.currentTarget.value)} />
      <span class="filter-count" id="filter-plugins-count" aria-live="polite">${count}</span>
      <button class="clear-filter" id="filter-plugins-clear" title="Clear filter" aria-label="Clear plugin filter"
        onClick=${() => { state.setQuery(''); inputRef.current?.focus(); }}>×</button>
      <span class="spacer"></span>
      <button id="plugins-check-now" class="tool" disabled=${current.busy.has(checkKey)}
        title="Re-run every plugin's check commands right now (the daemon also re-checks once a minute)"
        onClick=${() => actions.checkAll(checkKey)}>${current.busy.has(checkKey) ? 'checking…' : '⟳ check all now'}</button>
      <button id="plugin-create-open" class="primary" onClick=${() => state.openModal()}
        title="Define a new plugin — a named list of check/run shell-command steps">+ new plugin</button>
    </div>
    <${AsyncLoadState} label="Plugins" request=${current.request} retry=${actions.refresh} errorClass="plugins-error" />
    <div id="plugins-list" aria-busy=${current.request.phase === 'loading' ? 'true' : 'false'}>
      ${!current.request.hasLoaded ? null
        : current.registryError
          ? html`<div class="empty">⚠ plugin registry unreadable: <code>${current.registryError}</code> — fix or delete ~/.tclaude/plugins.json</div>`
          : html`<${Fragment}>
            ${current.all.length === 0
              ? html`<div class="empty">No plugins installed yet. Install one from the catalog below, or define your own with <strong>+ new plugin</strong>.</div>`
              : current.query && current.installed.length === 0
                ? html`<div class="empty">No plugin matches the filter.</div>`
                : current.installed.map((plugin) => html`<${PluginCard} key=${plugin.name} plugin=${plugin} state=${state} actions=${actions} busy=${current.busy} />`)}
            <${Catalog} plugins=${current.catalog} actions=${actions} busy=${current.busy} />
          </${Fragment}>`}
    </div>
  </div>`;
}

export function PluginsBadge({ state }) {
  const count = state.view.value.warningCount;
  return html`<span id="plugins-badge" class="tab-badge warn" hidden=${count === 0}>${count > 99 ? '99+' : count}</span>`;
}

function StepEditor({ step, index, state }) {
  const field = (name, label, rows, placeholder) => html`<label class="cron-create-row">
    <span class="cron-create-label">${label}</span>
    ${rows
      ? html`<textarea data-step-field=${name} rows=${rows} placeholder=${placeholder} spellcheck=${false}
          value=${step[name]} onInput=${(event) => state.updateStep(index, { [name]: event.currentTarget.value })}></textarea>`
      : html`<input type="text" data-step-field=${name} placeholder=${placeholder} autocomplete="off" spellcheck=${false}
          value=${step[name]} onInput=${(event) => state.updateStep(index, { [name]: event.currentTarget.value })} />`}
  </label>`;
  return html`<fieldset class="plugin-step-edit">
    <div class="plugin-step-edit-head"><span class="muted plugin-step-edit-n">step ${index + 1}</span><span class="spacer"></span>
      <button type="button" class="danger" data-step-remove title="Remove this step" onClick=${() => state.removeStep(index)}>remove</button></div>
    ${field('name', 'Name', 0, 'e.g. canvas server (docker)')}
    ${field('check', 'Check', 2, 'shell command — exit 0 means this step is satisfied (optional)')}
    ${field('run', 'Run', 2, 'shell command that performs the step (optional)')}
    ${field('stop', 'Stop', 2, 'shell command that undoes the step — powers deactivate (optional)')}
  </fieldset>`;
}

export function PluginsModal({ state, actions }) {
  const draft = state.view.value.modal;
  const nameRef = useRef(null);
  const close = () => { state.closeModal(); void actions.refresh({ force: true }); };
  const { dialogRef } = useDialogFocus({ open: !!draft, initialFocusRef: nameRef, onEscape: close });
  if (!draft) return null;
  return html`<div ref=${dialogRef} class="modal-overlay show" id="plugin-modal"
    onClick=${(event) => { if (event.target === event.currentTarget) close(); }}>
    <div class="cron-create-modal" role="dialog" aria-modal="true" aria-labelledby="plugin-modal-title">
      <h3 id="plugin-modal-title">${draft.mode === 'edit' ? 'Edit plugin' : 'New plugin'}</h3>
      <label class="cron-create-row"><span class="cron-create-label">Name</span>
        <input ref=${nameRef} id="plugin-modal-name" type="text" placeholder="e.g. excalidraw-mcp" autocomplete="off" spellcheck=${false}
          value=${draft.name} onInput=${(event) => state.updateModal({ name: event.currentTarget.value })} /></label>
      <label class="cron-create-row"><span class="cron-create-label">Descr</span>
        <textarea id="plugin-modal-descr" rows="2" placeholder="optional — what this plugin provides" spellcheck=${false}
          value=${draft.descr} onInput=${(event) => state.updateModal({ descr: event.currentTarget.value })}></textarea></label>
      <div id="plugin-modal-steps">${draft.steps.map((step, index) => html`<${StepEditor} key=${step._key} step=${step} index=${index} state=${state} />`)}</div>
      <div class="modal-buttons"><button id="plugin-modal-add-step" type="button" class="tool" onClick=${() => state.addStep()}
        title="Append another check/run step">+ add step</button></div>
      <div class="cron-create-error" id="plugin-modal-error" role=${draft.error ? 'alert' : undefined}>${draft.error}</div>
      <div class="modal-buttons">
        <button id="plugin-modal-cancel" type="button" onClick=${close}>Cancel</button><span class="spacer"></span>
        <button id="plugin-modal-submit" class="primary" type="button" disabled=${draft.submitting}
          onClick=${() => actions.save(draft)}>${draft.submitting ? 'Saving…' : draft.mode === 'edit' ? 'Save changes' : 'Create plugin'}</button>
      </div>
    </div>
  </div>`;
}

export function mountPluginsIsland({ host, badgeHost, modalHost, state, actions, registerCleanup }) {
  state.initialize();
  render(html`<${PluginsApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
  render(html`<${PluginsBadge} state=${state} />`, badgeHost);
  registerCleanup(() => render(null, badgeHost));
  render(html`<${PluginsModal} state=${state} actions=${actions} />`, modalHost);
  registerCleanup(() => render(null, modalHost));
}
