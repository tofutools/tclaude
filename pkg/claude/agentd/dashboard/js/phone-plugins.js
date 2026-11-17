// phone-plugins.js -- the Plugins tab on a phone (data-layout="phone").
//
// One card per installed plugin: its lamp, name and description, an on/off
// switch (activate, or deactivate: the desktop lamp's own action) and Check.
// A plugin whose checks all pass, or one switched off (dimmed), folds to one
// line ("2 steps · checked 3m ago"); a tap on its name unfolds it. Its steps
// are rows, a lamp, the name and the last output, with ▶ run or ■ stop where
// the desktop step lamp offers that, instead of a six-column table. Installing
// from the catalog and the plugin editor stay on the desktop. It runs on the
// desktop island's state and actions (plugins-state.js, plugins-actions.js),
// for this node: a fused view's peers' plugins are on the desktop too.

import { h, render } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { pluginLamp, PluginsBadge, statusPill, stepLamp, usePluginsTabShown } from './plugins-island.js';
import { pluginBusyKey } from './plugins-state.js';
import { DesktopOnlyNote } from './phone-desktop-only.js';
import { Card } from './phone-ui.js';
import { usePhoneBar } from './phone-page.js';
import { relTime } from './helpers.js';
import { Button, Switch } from './ui-components.js';
import { Empty, ErrorNote } from './phone-ui.js';

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

// lastChecked is when the plugin's steps were last checked: the latest of
// their check times, or '' when none has run.
export function lastChecked(plugin) {
  // By time, not text: a daemon writes local offsets, which do not sort
  // as strings across a DST change.
  let latest = '';
  let latestAt = -Infinity;
  for (const step of plugin.steps || []) {
    const at = Date.parse(step.checked_at || '');
    if (at > latestAt) { latest = step.checked_at; latestAt = at; }
  }
  return latest;
}

// cardSummary is the line under a plugin's name: its description, then for a
// card shown folded, how many steps and when they were checked ("off" for a
// plugin switched off).
export function cardSummary(plugin, folded) {
  if (plugin.disabled && statusPill(plugin).text === 'off') return [plugin.descr, 'off'].filter(Boolean).join(' · ');
  if (!folded) return plugin.descr || statusPill(plugin).text;
  const steps = (plugin.steps || []).length;
  const at = lastChecked(plugin);
  return [plugin.descr, steps && `${steps} step${steps === 1 ? '' : 's'}`, steps && (at ? `checked ${relTime(at)}` : 'not checked yet')]
    .filter(Boolean).join(' · ');
}

// opensFolded says whether a plugin's card starts folded to one line: one
// switched off, or one whose every check passes. A plugin that needs a look
// (a failing or unchecked step, a teardown that did not finish) shows its
// steps.
export function opensFolded(plugin) {
  if (plugin.disabled) return statusPill(plugin).text === 'off';
  return plugin.status === 'ok';
}

function PluginCard({ plugin, actions, busy }) {
  const lamp = pluginLamp(plugin);
  const toggleKey = pluginBusyKey('plugin-toggle', plugin.name);
  const checkKey = pluginBusyKey('plugin-check', plugin.name);
  // The switch is on exactly when the desktop lamp would deactivate (active,
  // or still active after a teardown), so flipping it always does what it
  // shows: on activates, off deactivates.
  const on = lamp.verb === 'deactivate';
  const steps = plugin.steps || [];
  // Folded by its state until the operator opens or folds it; a change of
  // state (a step starts failing) unfolds it again.
  const auto = opensFolded(plugin);
  const [chosen, setChosen] = useState(null);
  const [seen, setSeen] = useState(auto);
  if (seen !== auto) { setSeen(auto); setChosen(null); }
  const folded = chosen ?? auto;
  const off = plugin.disabled && statusPill(plugin).text === 'off';
  // The button reads as the name and the summary line. Where that line (the
  // mock's: just the description) leaves out the state, the lamp shows it
  // and a screen reader hears it.
  const summary = cardSummary(plugin, folded);
  const status = statusPill(plugin).text;
  return html`<${Card} class=${`phone-plugin${off ? ' off' : ''}`} data-key=${`plugin-${plugin.name}`}>
    <div class="phone-plugin-head">
      <span class=${`phone-plugin-dot ${lamp.cls}`} aria-hidden="true">${lamp.glyph}</span>
      <button data-ui-exempt type="button" class="phone-plugin-text phone-plugin-fold" aria-expanded=${folded ? 'false' : 'true'}
        onClick=${() => setChosen(!folded)}>
        <span class="phone-plugin-name">${plugin.name}</span>
        <span class="phone-plugin-sub" title=${statusPill(plugin).title}>${summary}${!summary.includes(status) && html`<span class="phone-plugin-sr"> · ${status}</span>`}</span>
      </button>
      <${Switch} checked=${on} disabled=${busy.has(toggleKey)} aria-label=${`${plugin.name}: ${lamp.verb}`}
        onChange=${() => actions.togglePlugin(plugin, lamp.verb, toggleKey)} />
    </div>
    ${!folded && steps.length > 0 && html`<ul class="phone-plugin-steps">${steps.map((step, index) => html`
      <${StepRow} key=${`${plugin.name}:${index}`} plugin=${plugin} step=${step} index=${index} actions=${actions} busy=${busy} />`)}</ul>`}
    ${!folded && html`<${Button} class="phone-wide phone-plugin-check" disabled=${busy.has(checkKey)} onClick=${() => actions.checkPlugin(plugin, checkKey)}>
      ${busy.has(checkKey) ? 'Checking…' : 'Check'}<//>`}
  <//>`;
}

export function PhonePlugins({ state, actions }) {
  const current = state.view.value;
  usePluginsTabShown(current, current.visible);
  const checkKey = pluginBusyKey('plugins-check-all');
  const checking = current.busy.has(checkKey);
  // Check all is the app bar's (the mock's), while there is a plugin to check.
  usePhoneBar({ tab: 'plugins', actions: [{ key: 'check-all', label: 'Check all', text: checking ? 'Checking…' : 'Check all',
    disabled: checking, onClick: () => actions.checkAll(checkKey) }] }, current.request.hasLoaded && current.all.length > 0);
  if (!current.request.hasLoaded) return html`<${Empty}>Loading…<//>`;
  if (current.registryError) {
    return html`<${ErrorNote}>Could not read the plugin registry: <code>${current.registryError}</code><//>`;
  }
  return html`<div class="phone-plugins">
    ${current.all.length === 0
      ? html`<${Empty}>No plugins installed yet.<//>`
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
