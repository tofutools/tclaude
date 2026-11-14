// phone-plugins.js -- the Plugins tab on a phone (data-layout="phone").
//
// One card per installed plugin: its lamp and name, an on/off switch
// (activate, or deactivate: the desktop lamp's own action) and Check. Its steps
// are rows, a lamp, the name and the last output, with ▶ run or ■ stop where
// the desktop step lamp offers that, instead of a six-column table. Installing
// from the catalog and the plugin editor stay on the desktop. It runs on the
// desktop island's state and actions (plugins-state.js, plugins-actions.js),
// for this node: a fused view's peers' plugins are on the desktop too.

import { h, render } from 'preact';
import htm from 'htm';
import { pluginLamp, PluginsBadge, statusPill, stepLamp, usePluginsTabShown } from './plugins-island.js';
import { pluginBusyKey } from './plugins-state.js';
import { DesktopOnlyNote } from './phone-desktop-only.js';
import { Button, Switch } from './ui-components.js';

const html = htm.bind(h);

function StepRow({ plugin, step, index, actions, busy }) {
  const lamp = stepLamp(plugin, step);
  const key = pluginBusyKey('plugin-step-toggle', plugin.name, index);
  const isBusy = busy.has(key);
  const output = step.output ? step.output.split('\n')[0] : '';
  return html`<li class="phone-plugin-step">
    <span class=${`phone-plugin-dot ${lamp.cls}`} title=${lamp.state} aria-hidden="true">${lamp.glyph}</span>
    <span class="phone-plugin-text">
      <span class="phone-plugin-step-name">${step.name}</span>
      ${output && html`<span class="phone-plugin-out" title=${step.output}>${output}</span>`}
    </span>
    ${lamp.verb && html`<${Button} size="sm" class="phone-plugin-act" disabled=${isBusy}
      aria-label=${`${lamp.verb === 'stop' ? 'Stop' : 'Run'} ${step.name}`} title=${`${lamp.state}: ${lamp.verb === 'stop' ? step.stop : step.run}`}
      onClick=${() => actions.toggleStep(plugin, index, lamp.verb, key)}>${isBusy ? '…' : lamp.verb === 'stop' ? '■' : '▶'}<//>`}
  </li>`;
}

function PluginCard({ plugin, actions, busy }) {
  const pill = statusPill(plugin);
  const lamp = pluginLamp(plugin);
  const toggleKey = pluginBusyKey('plugin-toggle', plugin.name);
  const checkKey = pluginBusyKey('plugin-check', plugin.name);
  // The switch shows what the plugin is set to (off is a choice); flipping
  // it runs the lamp's action, which also retries a teardown that left a
  // step running.
  const on = !plugin.disabled;
  const steps = plugin.steps || [];
  return html`<div class="phone-plugin ui-card" data-key=${`plugin-${plugin.name}`}>
    <div class="phone-plugin-head">
      <span class=${`phone-plugin-dot ${lamp.cls}`} aria-hidden="true">${lamp.glyph}</span>
      <span class="phone-plugin-text">
        <span class="phone-plugin-name">${plugin.name}</span>
        <span class="phone-plugin-sub"><span class=${`state-pill ${pill.cls}`} title=${pill.title}>${pill.text}</span>${plugin.descr ? ` · ${plugin.descr}` : ''}</span>
      </span>
      <${Switch} checked=${on} disabled=${busy.has(toggleKey)} aria-label=${`${plugin.name}: ${lamp.verb}`}
        onChange=${() => actions.togglePlugin(plugin, lamp.verb, toggleKey)} />
    </div>
    ${steps.length > 0 && html`<ul class="phone-plugin-steps">${steps.map((step, index) => html`
      <${StepRow} key=${`${plugin.name}:${index}`} plugin=${plugin} step=${step} index=${index} actions=${actions} busy=${busy} />`)}</ul>`}
    <${Button} class="phone-wide phone-plugin-check" disabled=${busy.has(checkKey)} onClick=${() => actions.checkPlugin(plugin, checkKey)}>
      ${busy.has(checkKey) ? 'Checking…' : 'Check'}<//>
  </div>`;
}

export function PhonePlugins({ state, actions }) {
  const current = state.view.value;
  usePluginsTabShown(current, current.visible);
  const checkKey = pluginBusyKey('plugins-check-all');
  if (!current.request.hasLoaded) return html`<p class="ui-empty phone-empty">Loading…</p>`;
  if (current.registryError) {
    return html`<p class="ui-error-box" role="alert">The plugin registry is unreadable: <code>${current.registryError}</code></p>`;
  }
  return html`<div class="phone-plugins">
    ${current.all.length > 0 && html`<${Button} class="phone-wide" disabled=${current.busy.has(checkKey)} onClick=${() => actions.checkAll(checkKey)}>
      ${current.busy.has(checkKey) ? 'Checking…' : 'Check all'}<//>`}
    ${current.all.length === 0
      ? html`<p class="ui-empty phone-empty">No plugins installed yet.</p>`
      : current.all.map((plugin) => html`<${PluginCard} key=${plugin.name} plugin=${plugin} actions=${actions} busy=${current.busy} />`)}
    <${DesktopOnlyNote} what="Installing from the catalog and editing plugins are" />
  </div>`;
}

export function mountPhonePluginsIsland({ host, badgeHost, state, actions, registerCleanup }) {
  state.initialize();
  render(html`<${PhonePlugins} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
  render(html`<${PluginsBadge} state=${state} />`, badgeHost);
  registerCleanup(() => render(null, badgeHost));
}
