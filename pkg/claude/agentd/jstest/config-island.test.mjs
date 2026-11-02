import test from 'node:test';
import assert from 'node:assert/strict';
import { assertSameNode } from './assertions.mjs';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

async function selectValue(harness, select, value) {
  const applySelection = () => {
    for (const option of select.querySelectorAll('option')) {
      option.removeAttribute('selected');
      option.selected = false;
    }
    const selected = select.querySelector(`option[value="${value}"]`);
    selected.setAttribute('selected', '');
    selected.selected = true;
  };
  applySelection();
  await harness.act(() => harness.fireEvent(select, 'input'));
  // LinkeDOM resets an uncontrolled select while Preact handles the routed
  // dirty-state render; restore the browser-owned live selection for assertions.
  applySelection();
}

test('Config island owns the complete form markup and tracks dirty input', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  assert.ok(mounted.container.querySelector('#cfg-log-level'), mounted.container.innerHTML.slice(0, 500));
  assert.ok(mounted.container.querySelector('#cfg-sudo-json'));
  assert.ok(mounted.container.querySelector('#cfg-save'));
  state.lifecycle.loaded({ raw: '{}' });
  const terminal = mounted.container.querySelector('select[aria-label="Terminal emulator"]');
  await selectValue(harness, terminal, 'ghostty');
  assert.equal(state.view.value.dirty, true);
  await mounted.unmount();
});

test('Config island renders static HTML characters instead of entity source text', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  const text = mounted.container.textContent;
  const panels = [...mounted.container.querySelectorAll('.cfg-panel-head h2')].map(node => node.textContent);

  assert.ok(panels.some(title => title.includes('Terminals & windows')), panels.join(' | '));
  assert.ok(panels.some(title => title.includes('Usage & costs')), panels.join(' | '));
  assert.ok(text.includes('keep the header & tabs pinned'));
  assert.ok(text.includes('tclaude:<id>'));
  assert.ok(text.includes('tclaude remote-access add-client <name>'));
  assert.ok(text.includes('https://<host>:<port>'));
  assert.ok(text.includes('*\u00a0→\u00a0state'));
  assert.ok(text.includes('30\u00a0min'));
  assert.doesNotMatch(text, /&(amp|lt|gt|nbsp);/i);
  await mounted.unmount();
});

test('Config installs its lazy loader before initial route activation', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
  ]);
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  nav.innerHTML = '<button data-tab="config">Config</button>';
  let loads = 0;
  const fetchImpl = async () => {
    loads++;
    return { ok: true, json: async () => ({ raw: '{}', path: '/tmp/config.json' }) };
  };
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{ fetchImpl, isCyclingTabs: () => true }} />`);
  harness.fireEvent(nav.querySelector('button'), 'click');
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(loads, 1);
  assert.equal(state.view.value.phase, 'ready');
  await mounted.unmount();
  nav.remove();
});

test('Config load populates representative fields, conditions, notices, and round-trips unowned keys', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const config = {
    log_level: 'warn', terminal: 'ghostty', record_hooks: true, startup_timing: true,
    cost: { daily_limit_usd: 12.5, monthly_limit_usd: 300 },
    ratelimit: { five_hour_percent_max_used: 88, seven_day_percent_max_used: 97.5, future_limit: 7 },
    agent: { spawn_max_per_hour: 3, sudo: { max_duration: '2h' } },
    slop: { volume: 0.4 },
    dashboard: { default_terminal: 'native', default_directory_picker: 'native' },
  };
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const fetchImpl = async (url) => {
    assert.equal(url, '/api/config');
    return { ok: true, json: async () => ({ raw: JSON.stringify(config), path: '/tmp/config.json', unknown_keys: ['future_root'], warning: 'test warning' }) };
  };
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{ fetchImpl }} />`);
  await adapter.loadConfigTab();
  assert.notEqual(state.view.value.phase, 'error', state.view.value.error);
  const logLevel = mounted.container.querySelector('#cfg-log-level');
  assert.equal(logLevel.querySelector('option[value="warn"]').selected, true, logLevel.outerHTML);
  const terminal = mounted.container.querySelector('#cfg-terminal');
  assert.equal(terminal.tagName, 'SELECT');
  assert.equal(terminal.querySelector('option[value="ghostty"]').selected, true, terminal.outerHTML);
  assert.equal(adapter.assembleConfig().terminal, 'ghostty');
  assert.equal(mounted.container.querySelector('#cfg-record-hooks').checked, true);
  const startupTiming = mounted.container.querySelector('#cfg-startup-timing');
  assert.equal(startupTiming.checked, true);
  assert.equal(adapter.assembleConfig().startup_timing, true);
  startupTiming.checked = false;
  assert.equal(adapter.assembleConfig().startup_timing, false);
  assert.equal(mounted.container.querySelector('#cfg-dashboard-default-web-terminal').checked, false);
  assert.equal(mounted.container.querySelector('#cfg-dashboard-default-web-directory-picker').checked, false);
  assert.equal(mounted.container.querySelector('#cfg-ratelimit-5h').disabled, false);
  assert.match(mounted.container.querySelector('#cfg-notice').textContent, /future_root/);
  const assembled = adapter.assembleConfig();
  assert.equal(assembled.ratelimit.future_limit, 7);
  // The legacy spend limits have no field any more (the daemon converts
  // them into usage limits); a value still on disk round-trips untouched.
  assert.equal(assembled.cost.daily_limit_usd, 12.5);
  assert.equal(assembled.cost.monthly_limit_usd, 300);
  assert.equal(mounted.container.querySelector('#cfg-cost-daily-limit'), null);
  assert.ok(mounted.container.querySelector('#cfg-open-usage-rules'));
  assert.equal(assembled.slop.volume, 0.4);
  assert.deepEqual(assembled.agent.sudo, { max_duration: '2h' });
  assert.equal(assembled.dashboard.default_terminal, 'native');
  assert.equal(assembled.dashboard.default_directory_picker, 'native');
  assert.equal(state.view.value.phase, 'ready');
  assert.equal(state.view.value.dirty, false);
  await mounted.unmount();
});

test('Config defaults terminal and directory actions to web while persisting native opt-outs', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw: '{}' }) }),
  }} />`);
  await adapter.loadConfigTab();

  const terminal = mounted.container.querySelector('#cfg-dashboard-default-web-terminal');
  const directoryPicker = mounted.container.querySelector('#cfg-dashboard-default-web-directory-picker');
  assert.equal(terminal.checked, true);
  assert.equal(directoryPicker.checked, true);
  assert.equal(adapter.assembleConfig().dashboard?.default_terminal, undefined);
  assert.equal(adapter.assembleConfig().dashboard?.default_directory_picker, undefined);

  terminal.checked = false;
  directoryPicker.checked = false;
  assert.equal(adapter.assembleConfig().dashboard.default_terminal, 'native');
  assert.equal(adapter.assembleConfig().dashboard.default_directory_picker, 'native');
  await mounted.unmount();
});

test('Config terminal dropdown defaults to auto-detect and omits the setting', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  state.lifecycle.loaded({ raw: '{}' });

  const terminal = mounted.container.querySelector('#cfg-terminal');
  const automatic = terminal.querySelector('option[value=""]');
  assert.equal(automatic.textContent, 'Auto-detect (default)');
  assert.equal(adapter.assembleConfig().terminal, undefined);

  await mounted.unmount();
});

test('Config terminal dropdown canonicalizes aliases and preserves unknown current values', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  let raw = JSON.stringify({ terminal: 'iterm' });
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);

  await adapter.loadConfigTab();
  const terminal = mounted.container.querySelector('#cfg-terminal');
  assert.equal(terminal.querySelector('option[value="iterm2"]').selected, true);
  assert.equal(adapter.assembleConfig().terminal, 'iterm2');

  raw = JSON.stringify({ terminal: 'future-terminal' });
  await adapter.loadConfigTab();
  const current = terminal.querySelector('option[data-current-terminal]');
  assert.equal(current.value, 'future-terminal');
  assert.equal(current.textContent, 'future-terminal (current value)');
  assert.equal(current.selected, true);
  assert.equal(adapter.assembleConfig().terminal, 'future-terminal');

  raw = '{}';
  await adapter.loadConfigTab();
  assert.equal(terminal.querySelector('option[data-current-terminal]'), null);
  assert.equal(adapter.assembleConfig().terminal, undefined);

  await mounted.unmount();
});

test('Config treats agent directory parent mounting as default-on with an explicit opt-out', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  let raw = '{}';
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);

  await adapter.loadConfigTab();
  const checkbox = mounted.container.querySelector('#cfg-feature-agent-dirs-mount-parent');
  assert.equal(checkbox.checked, true, 'absent key loads as checked');
  assert.equal(adapter.assembleConfig().features?.agent_dirs_mount_parent, undefined,
    'checked default stays omitted');

  raw = JSON.stringify({ features: { agent_dirs_mount_parent: false } });
  await adapter.loadConfigTab();
  assert.equal(checkbox.checked, false, 'explicit false loads as unchecked');
  assert.equal(adapter.assembleConfig().features.agent_dirs_mount_parent, false,
    'unchecked persists the explicit opt-out');

  raw = JSON.stringify({ features: { agent_dirs_mount_parent: true } });
  await adapter.loadConfigTab();
  assert.equal(checkbox.checked, true, 'explicit true loads as checked');
  assert.equal(adapter.assembleConfig().features?.agent_dirs_mount_parent, undefined,
    'checked canonicalizes explicit true to the omitted default');
  await mounted.unmount();
});

test('Config keeps the terminal command-palette shortcut default-off with an explicit opt-in', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  let raw = '{}';
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);

  await adapter.loadConfigTab();
  const checkbox = mounted.container.querySelector(
    '#cfg-feature-terminal-command-palette-shortcut',
  );
  assert.equal(checkbox.checked, false, 'absent key loads as unchecked');
  assert.equal(adapter.assembleConfig().features?.terminal_command_palette_shortcut, undefined,
    'unchecked default stays omitted');

  raw = JSON.stringify({ features: { terminal_command_palette_shortcut: true } });
  await adapter.loadConfigTab();
  assert.equal(checkbox.checked, true, 'explicit true loads as checked');
  assert.equal(adapter.assembleConfig().features.terminal_command_palette_shortcut, true,
    'checked persists the explicit opt-in');

  await harness.act(() => {
    checkbox.checked = false;
    harness.fireEvent(checkbox, 'input');
  });
  assert.equal(adapter.assembleConfig().features?.terminal_command_palette_shortcut, undefined,
    'unchecking removes the opt-in');
  await mounted.unmount();
});

test('Config keeps recorded sandbox details default-off with an explicit opt-in', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  let raw = '{}';
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);

  await adapter.loadConfigTab();
  const checkbox = mounted.container.querySelector('#cfg-feature-recorded-sandbox-details');
  assert.equal(checkbox.checked, false, 'absent key loads as unchecked');
  assert.equal(adapter.assembleConfig().features?.recorded_sandbox_details, undefined,
    'unchecked default stays omitted');

  raw = JSON.stringify({ features: { recorded_sandbox_details: true } });
  await adapter.loadConfigTab();
  assert.equal(checkbox.checked, true, 'explicit true loads as checked');
  assert.equal(adapter.assembleConfig().features.recorded_sandbox_details, true,
    'checked persists the explicit opt-in');

  await harness.act(() => {
    checkbox.checked = false;
    harness.fireEvent(checkbox, 'input');
  });
  assert.equal(adapter.assembleConfig().features?.recorded_sandbox_details, undefined,
    'unchecking removes the opt-in');
  await mounted.unmount();
});

test('Config keeps triggers default-off with an explicit opt-in', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  let raw = '{}';
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);

  await adapter.loadConfigTab();
  const checkbox = mounted.container.querySelector('#cfg-feature-triggers');
  assert.equal(checkbox.checked, false, 'absent key loads as unchecked');
  assert.equal(adapter.assembleConfig().features?.triggers, undefined,
    'unchecked default stays omitted');

  raw = JSON.stringify({ features: { triggers: true } });
  await adapter.loadConfigTab();
  assert.equal(checkbox.checked, true, 'explicit true loads as checked');
  assert.equal(adapter.assembleConfig().features.triggers, true,
    'checked persists the explicit opt-in');

  await harness.act(() => {
    checkbox.checked = false;
    harness.fireEvent(checkbox, 'input');
  });
  assert.equal(adapter.assembleConfig().features?.triggers, undefined,
    'unchecking removes the opt-in');
  await mounted.unmount();
});

test('Config keeps group attachments default-off and saves float/fixed modes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  let raw = '{}';
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);

  await adapter.loadConfigTab();
  const select = mounted.container.querySelector('#cfg-feature-group-attachments');
  assert.equal(select.querySelector('option[value="off"]').selected, true,
    'absent key loads as off');
  assert.equal(adapter.assembleConfig().features?.group_attachments, undefined,
    'off default stays omitted');

  raw = JSON.stringify({
    features: {
      group_attachments: 'float',
      future_feature_owned_elsewhere: true,
    },
  });
  await adapter.loadConfigTab();
  assert.equal(select.querySelector('option[value="float"]').selected, true,
    'float loads as selected');
  assert.equal(adapter.assembleConfig().features.group_attachments, 'float',
    'float persists');
  assert.equal(adapter.assembleConfig().features.future_feature_owned_elsewhere, true,
    'the form preserves unrelated feature keys');

  raw = JSON.stringify({
    features: {
      group_attachments: 'fixed',
      future_feature_owned_elsewhere: true,
    },
  });
  await adapter.loadConfigTab();
  assert.equal(select.querySelector('option[value="fixed"]').selected, true,
    'fixed loads as selected');
  assert.equal(adapter.assembleConfig().features.group_attachments, 'fixed',
    'fixed persists');

  await harness.act(() => {
    for (const option of select.querySelectorAll('option')) {
      option.selected = option.value === 'off';
    }
    harness.fireEvent(select, 'input');
  });
  assert.equal(adapter.assembleConfig().features?.group_attachments, undefined,
    'selecting off removes only the mode');
  assert.equal(adapter.assembleConfig().features.future_feature_owned_elsewhere, true,
    'selecting off still preserves unrelated feature keys');
  await mounted.unmount();
});

test('Config round-trips all web terminal attach resize strategies and timings', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  let raw = '{}';
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);

  await adapter.loadConfigTab();
  assert.equal(adapter.assembleConfig().dashboard?.terminal_attach, undefined,
    'the historical repair defaults remain sparse');

  raw = JSON.stringify({ dashboard: { terminal_attach: {
    mode: 'pre_attach', initial_resize_delay_ms: 30,
    repair_delay_ms: 400, pre_attach_delay_ms: 175,
  } } });
  await adapter.loadConfigTab();
  const mode = mounted.container.querySelector('#cfg-terminal-attach-mode');
  assert.equal(mode.querySelector('option[value="pre_attach"]').selected, true);
  assert.equal(mounted.container.querySelector('#cfg-terminal-attach-initial-delay').value, '30');
  assert.equal(mounted.container.querySelector('#cfg-terminal-attach-repair-delay').value, '400');
  assert.equal(mounted.container.querySelector('#cfg-terminal-attach-pre-delay').value, '175');
  assert.deepEqual(adapter.assembleConfig().dashboard.terminal_attach, {
    mode: 'pre_attach', initial_resize_delay_ms: 30,
    repair_delay_ms: 400, pre_attach_delay_ms: 175,
  });
  await mounted.unmount();
});

test('Config saves live wizard and slop activity-bot selections over their loaded defaults', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw: '{}' }) }),
  }} />`);
  await adapter.loadConfigTab();

  // A browser preserves the loaded `selected` attribute as defaultSelected
  // while changing the live properties when the human picks another option.
  const selectLiveOption = (id, from, to) => {
    const select = mounted.container.querySelector(id);
    const loaded = select.querySelector(`option[value="${from}"]`);
    const chosen = select.querySelector(`option[value="${to}"]`);
    assert.equal(loaded.hasAttribute('selected'), true, `${id} loaded default is reflected in markup`);
    Object.defineProperty(loaded, 'selected', { configurable: true, value: false });
    Object.defineProperty(chosen, 'selected', { configurable: true, value: true });
    assert.equal(loaded.hasAttribute('selected'), true, `${id} loaded attribute remains stale after interaction`);
    assert.equal(chosen.hasAttribute('selected'), false, `${id} new live selection needs no markup attribute`);
  };
  selectLiveOption('#cfg-dashboard-activity-bots-wizard', 'emoji', 'sprites');
  selectLiveOption('#cfg-dashboard-activity-bots-slop', 'sprites', 'emoji');

  const windowControls = mounted.container.querySelector('#cfg-dashboard-window-controls');
  assert.deepEqual([...windowControls.querySelectorAll('option')].map((option) => option.textContent), ['Automatic', 'macOS', 'Windows', 'Linux', 'Classic']);
  assert.equal(adapter.assembleConfig().dashboard.window_controls, undefined, 'Automatic is the default and stays unset');
  selectLiveOption('#cfg-dashboard-window-controls', 'auto', 'classic');

  const assembled = adapter.assembleConfig();
  assert.equal(assembled.dashboard.window_controls, 'classic');
  assert.equal(assembled.dashboard.activity_bots.wizard, 'sprites');
  assert.equal(assembled.dashboard.activity_bots.slop, 'emoji');
  await mounted.unmount();
});

test('Config parses the OpenCode legacy pricing cutoff as a positive safe integer', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw: '{}' }) }),
  }} />`);
  await adapter.loadConfigTab();
  const cutoff = mounted.container.querySelector('#cfg-opencode-legacy-long-context-pricing-cutoff');

  cutoff.value = '2e5';
  assert.equal(adapter.assembleConfig().opencode.legacy_long_context_pricing_cutoff, 200000,
    'valid exponent notation is parsed as the complete number');

  for (const invalid of ['272000.5', '0', '-1', '9007199254740992']) {
    cutoff.value = invalid;
    assert.throws(() => adapter.assembleConfig(), /positive whole number/,
      `${invalid} must not be silently truncated or persisted`);
  }
  await mounted.unmount();
});

test('Config save validates, confirms, writes against its baseline, and clears dirty state', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const baseline = JSON.stringify({ terminal: 'xterm' });
  const saved = JSON.stringify({ terminal: 'ghostty' });
  const requests = [];
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const fetchImpl = async (url, options = {}) => {
    requests.push({ url, options });
    if (!options.method) return { ok: true, json: async () => ({ raw: baseline, path: '/tmp/config.json' }) };
    if (url.endsWith('?dry_run=1')) return { ok: true, json: async () => ({ raw: saved }) };
    return { ok: true, json: async () => ({ raw: saved, path: '/tmp/config.json' }) };
  };
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl,
  }} />`);
  await adapter.loadConfigTab();
  const terminal = mounted.container.querySelector('#cfg-terminal');
  terminal.focus();
  await selectValue(harness, terminal, 'ghostty');
  assert.equal(state.view.value.dirty, true);
  const saving = adapter.saveConfig();
  await new Promise(resolve => setTimeout(resolve, 0));
  const modal = mounted.container.querySelector('#config-diff-modal');
  assert.match(modal.querySelector('#config-diff-sub').textContent, /\/tmp\/config.json/);
  const confirm = modal.querySelector('#config-diff-confirm');
  const cancel = modal.querySelector('#config-diff-cancel');
  assertSameNode(harness.document.activeElement, confirm);
  cancel.focus();
  harness.fireEvent(cancel, 'keydown', { key: 'Tab', shiftKey: true });
  assertSameNode(harness.document.activeElement, confirm);
  harness.fireEvent(confirm, 'click');
  await saving;
  assert.deepEqual(requests.map(({ url }) => url), ['/api/config', '/api/config?dry_run=1', '/api/config']);
  const posted = JSON.parse(requests[1].options.body);
  assert.equal(posted.base, baseline);
  assert.equal(posted.config.terminal, 'ghostty');
  assert.equal(state.view.value.phase, 'ready');
  assert.equal(state.view.value.dirty, false);
  assertSameNode(harness.document.activeElement, terminal);
  await mounted.unmount();
});

test('Config save preserves dirty edits and reports a stale baseline conflict', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  let calls = 0;
  const fetchImpl = async (_url, options = {}) => {
    calls++;
    if (!options.method) return { ok: true, json: async () => ({ raw: '{}', path: '/tmp/config.json' }) };
    return { ok: false, status: 409, json: async () => ({ error: 'config.json changed on disk' }) };
  };
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{ fetchImpl }} />`);
  await adapter.loadConfigTab();
  const terminal = mounted.container.querySelector('#cfg-terminal');
  await selectValue(harness, terminal, 'ghostty');
  await adapter.saveConfig();
  assert.equal(calls, 2);
  assert.equal(state.view.value.phase, 'error');
  assert.equal(state.view.value.dirty, true);
  assert.match(state.view.value.error, /changed on disk/);
  assert.equal(terminal.querySelector('option[value="ghostty"]').selected, true);
  assert.match(mounted.container.querySelector('#cfg-errors').textContent, /Reload/);
  await mounted.unmount();
});

test('Config rejects invalid advanced sudo JSON before issuing a save request', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  let calls = 0;
  const fetchImpl = async (_url, options = {}) => {
    calls++;
    assert.equal(options.method, undefined);
    return { ok: true, json: async () => ({ raw: '{}', path: '/tmp/config.json' }) };
  };
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{ fetchImpl }} />`);
  await adapter.loadConfigTab();
  await harness.input(mounted.container.querySelector('#cfg-sudo-json'), '{broken');
  await adapter.saveConfig();
  assert.equal(calls, 1);
  assert.equal(state.view.value.phase, 'error');
  assert.equal(state.view.value.dirty, true);
  assert.match(state.view.value.error, /sudo/i);
  assert.match(mounted.container.querySelector('#cfg-errors').textContent, /sudo/i);
  await mounted.unmount();
});

test('Config list add and remove actions mark the form dirty', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const raw = JSON.stringify({ pre_compact_guard: { thresholds: [{ window_size: 200000, min_tokens: 150000 }] } });
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);
  await adapter.loadConfigTab();
  harness.fireEvent(mounted.container.querySelector('#cfg-precompact-thresholds .cfg-row-del'), 'click');
  assert.equal(state.view.value.dirty, true);
  state.lifecycle.loaded({ raw });
  harness.fireEvent(mounted.container.querySelector('#cfg-precompact-thresholds .cfg-list-add'), 'click');
  assert.equal(state.view.value.dirty, true);
  await mounted.unmount();
});

test('Config list reconciliation preserves unrelated typing and focuses the added row', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw: '{}' }) }),
  }} />`);
  await adapter.loadConfigTab();
  const cloneCooldown = mounted.container.querySelector('#cfg-agent-clonecooldown');
  cloneCooldown.focus();
  await harness.input(cloneCooldown, 'half-typed');
  harness.fireEvent(mounted.container.querySelector('#cfg-agent-permissions .cfg-list-add'), 'click');
  await new Promise(resolve => queueMicrotask(resolve));
  assertSameNode(mounted.container.querySelector('#cfg-agent-clonecooldown'), cloneCooldown);
  assert.equal(cloneCooldown.value, 'half-typed');
  assertSameNode(harness.document.activeElement,
    mounted.container.querySelector('#cfg-agent-permissions .cfg-list-row:last-of-type input'));
  await mounted.unmount();
});

test('Config edits the default Steward permissions with the slug list and drops an empty one', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw: JSON.stringify({ steward: { name: 'Jeeves' } }) }) }),
  }} />`);
  await adapter.loadConfigTab();
  await harness.act(() => {});
  const steward = adapter.assembleConfig().steward;
  assert.equal(steward.name, 'Jeeves');
  assert.equal('source_download' in steward, false, 'asking is the default, so it is not written out');
  await selectValue(harness, mounted.container.querySelector('#cfg-steward-source-download'), 'each_version');
  assert.equal(adapter.assembleConfig().steward.source_download, 'each_version');
  await mounted.unmount();
});

test('Config follows a source choice made outside the form and keeps other edits', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter, { SOURCE_CHOICE_EVENT }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
    harness.importDashboardModule('js/knowledge-source-island.js'),
  ]);
  let onDisk = { steward: { name: 'Jeeves' } };
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw: JSON.stringify(onDisk) }) }),
  }} />`);
  await adapter.loadConfigTab();
  await harness.act(() => {});
  const name = mounted.container.querySelector('#cfg-steward-name');
  name.value = 'Unsaved';
  onDisk = { steward: { name: 'Jeeves', source_download: 'never' } };
  await harness.act(async () => {
    document.dispatchEvent(new CustomEvent(SOURCE_CHOICE_EVENT));
    await new Promise((r) => setTimeout(r, 0));
  });
  const steward = adapter.assembleConfig().steward;
  assert.equal(steward.source_download, 'never');
  assert.equal(steward.name, 'Unsaved', 'the rest of the form keeps its edits');
  await mounted.unmount();
});

test('Config teardown cancels its Preact-owned diff modal', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  const result = state.confirmDiff('{}', '{\n  "terminal": "ghostty"\n}', false, '/tmp/config.json');
  await harness.act(() => {});
  assert.equal(mounted.container.querySelector('#config-diff-modal').classList.contains('show'), true);
  await mounted.unmount();
  assert.equal(await result, false);
});

test('Config remount discards loaded ownership and reloads the fresh form', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
  ]);
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  nav.innerHTML = '<button data-tab="config">Config</button>';
  let loads = 0;
  const fetchImpl = async () => {
    loads++;
    const terminal = loads === 1 ? 'xterm' : 'ghostty';
    return { ok: true, json: async () => ({ raw: JSON.stringify({ terminal }) }) };
  };
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const dependencies = { fetchImpl, isCyclingTabs: () => true };
  const first = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${dependencies} />`);
  harness.fireEvent(nav.querySelector('button'), 'click');
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(first.container.querySelector('#cfg-terminal option[value="xterm"]').selected, true);
  await first.unmount();

  const second = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${dependencies} />`);
  harness.fireEvent(nav.querySelector('button'), 'click');
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(loads, 2);
  assert.equal(second.container.querySelector('#cfg-terminal option[value="ghostty"]').selected, true);
  await second.unmount();
  nav.remove();
});

test('Config round-trips harness factors and distinguishes explicit 1 from inheritance', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const cfg = { cost: { estimate_factor: 2, harness_factors: { claude: 1.2, codex: 1 }, show_on_subscription: true } };
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw: JSON.stringify(cfg) }) }),
  }} />`);
  await adapter.loadConfigTab();
  assert.deepEqual(adapter.assembleConfig().cost, cfg.cost);
  mounted.container.querySelector('#cfg-cost-factor-claude').value = '';
  mounted.container.querySelector('#cfg-cost-factor-opencode').value = '1.5';
  const cost = adapter.assembleConfig().cost;
  assert.deepEqual(cost.harness_factors, { codex: 1, opencode: 1.5 });
  assert.equal(cost.estimate_factor, 2);
  assert.equal(cost.show_on_subscription, true);
  await mounted.unmount();
});

test('Config rail shows one panel at a time and marks panels with unsaved edits', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  const root = mounted.container;
  const current = () => [...root.querySelectorAll('[data-cfg-panel][data-cfg-current="true"]')].map(node => node.getAttribute('data-cfg-panel'));
  assert.deepEqual(current(), ['appearance']);

  await harness.act(() => harness.fireEvent(root.querySelector('[data-cfg-panel-link="notifications"]'), 'click'));
  assert.deepEqual(current(), ['notifications']);
  assert.equal(root.querySelector('[data-cfg-panel-link="notifications"]').getAttribute('aria-current'), 'page');

  // Every panel stays mounted, so the adapter can still read hidden fields.
  assert.ok(root.querySelector('[data-cfg-panel="logs"] #cfg-log-level'));

  state.lifecycle.loaded({ raw: '{}' });
  const cooldown = root.querySelector('#cfg-notif-cooldown');
  cooldown.value = '9';
  await harness.act(() => harness.fireEvent(cooldown, 'input'));
  assert.deepEqual(state.dirtyPanels.value, ['notifications']);
  assert.equal(root.querySelector('[data-cfg-panel-link="notifications"] .cfg-rail-dirty').hasAttribute('hidden'), false);
  assert.equal(root.querySelector('[data-cfg-panel-link="logs"] .cfg-rail-dirty').hasAttribute('hidden'), true);
  assert.match(root.querySelector('.cfg-save-summary').textContent, /Unsaved changes · Notifications/);

  // Typing a search query is not an edit.
  const filter = root.querySelector('#cfg-filter');
  filter.value = 'log';
  await harness.act(() => harness.fireEvent(filter, 'input'));
  assert.deepEqual(state.dirtyPanels.value, ['notifications']);

  state.lifecycle.saved({ raw: '{}' });
  await harness.act(() => {});
  assert.deepEqual(state.dirtyPanels.value, []);
  await mounted.unmount();
});

test('Config search filters individual settings across panels', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
  ]);
  const tab = harness.document.body.appendChild(harness.document.createElement('section'));
  tab.id = 'tab-config';
  const host = tab.appendChild(harness.document.createElement('div'));
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`, host);
  const root = mounted.container;
  const filter = root.querySelector('#cfg-filter');
  const search = async (value) => {
    filter.value = value;
    await harness.act(() => harness.fireEvent(filter, 'input'));
  };
  const rowFor = id => root.querySelector(`#${id}`).closest('.cfg-row');

  await search('ghostty');
  assert.ok(root.querySelector('#cfg-layout').hasAttribute('data-cfg-searching'));
  assert.equal(rowFor('cfg-terminal').hidden, false);
  assert.equal(rowFor('cfg-focus-raiseonly').hidden, true);
  assert.equal(root.querySelector('[data-cfg-panel="notifications"]').hasAttribute('data-cfg-nomatch'), true);
  assert.equal(root.querySelector('[data-cfg-hits="terminals"]').textContent, '1');
  assert.match(root.querySelector('#cfg-filter-count').textContent, /^1 matching setting$/);

  // With Collab off the Steward panel is hidden, so it adds no matches.
  harness.document.body.classList.add('hide-collab');
  await search('steward.name');
  assert.equal(root.querySelector('[data-cfg-hits="steward"]').textContent, '');
  assert.equal(root.querySelector('#cfg-filter-count').textContent, '0 matching settings');
  assert.equal(root.querySelector('#cfg-filter-empty').hidden, false);
  harness.document.body.classList.remove('hide-collab');
  await search('steward.name');
  assert.match(root.querySelector('#cfg-filter-count').textContent, /^1 matching setting$/);

  // Keys and folded "More…" guidance are searchable too.
  await search('notifications.delivery');
  assert.equal(rowFor('cfg-notif-delivery').hidden, false);
  await search('d-bus');
  assert.equal(rowFor('cfg-notif-delivery').hidden, false);

  // A matching card heading keeps every row in that card.
  await search('rate limits');
  assert.equal(rowFor('cfg-ratelimit-7d').hidden, false);

  // A term that only appears in a panel's lede still matches per setting.
  await search('token');
  assert.equal(rowFor('cfg-agent-persisttoken').hidden, false);
  assert.equal(rowFor('cfg-agent-dashboardport').hidden, true);

  // A matching panel title keeps the whole panel.
  await search('experimental');
  assert.equal(rowFor('cfg-feature-triggers').hidden, false);

  await search('zzzz-nothing');
  assert.equal(root.querySelector('#cfg-filter-empty').hidden, false);

  // Picking a rail section leaves search mode.
  await harness.act(() => harness.fireEvent(root.querySelector('[data-cfg-panel-link="logs"]'), 'click'));
  assert.equal(filter.value, '');
  assert.equal(root.querySelector('#cfg-layout').hasAttribute('data-cfg-searching'), false);
  assert.equal(rowFor('cfg-focus-raiseonly').hidden, false);
  await mounted.unmount();
});

test('Config moves the static remote-access certificate panel into Daemon & network and back', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
  ]);
  const tab = harness.document.body.appendChild(harness.document.createElement('section'));
  tab.id = 'tab-config';
  const host = tab.appendChild(harness.document.createElement('div'));
  const admin = tab.appendChild(harness.document.createElement('div'));
  admin.id = 'cfg-remote-admin';
  admin.className = 'cfg-section';
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`, host);
  assert.equal(admin.parentElement.id, 'cfg-remote-admin-slot');
  assert.equal(admin.closest('[data-cfg-panel]').getAttribute('data-cfg-panel'), 'daemon');
  await mounted.unmount();
  assert.equal(admin.parentElement, tab);
});

test('Config auto-close terminal tabs matrix shows defaults and persists only overrides', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  let raw = '{}';
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw }) }),
  }} />`);
  const cell = (event, target) => mounted.container.querySelector(`#cfg-close-tabs-${event}-${target}`);

  await adapter.loadConfigTab();
  assert.equal(cell('session_ended', 'group_terminal').checked, true);
  assert.equal(cell('session_ended', 'agent_terminal').checked, true);
  assert.equal(cell('session_ended', 'agent_window').checked, false);
  assert.equal(cell('detached', 'group_terminal').checked, false);
  assert.equal(adapter.assembleConfig().dashboard?.close_terminal_tabs, undefined, 'defaults stay sparse');

  raw = JSON.stringify({ dashboard: { close_terminal_tabs: {
    session_ended: { group_terminal: false, agent_window: true },
    detached: { agent_terminal: true },
  } } });
  await adapter.loadConfigTab();
  assert.equal(cell('session_ended', 'group_terminal').checked, false);
  assert.equal(cell('session_ended', 'agent_window').checked, true);
  assert.equal(cell('detached', 'agent_terminal').checked, true);
  cell('detached', 'agent_terminal').checked = false;
  assert.deepEqual(adapter.assembleConfig().dashboard.close_terminal_tabs, {
    session_ended: { group_terminal: false, agent_window: true },
  });
  await mounted.unmount();
});

test('Config ask sandbox defaults to global and round-trips modes and exact library profiles', async (t) => {
  const harness = await createPreactHarness(t);
  t.mock.method(globalThis, 'fetch', async (url) => ({
    ok: true, json: async () => url === '/api/sandbox-profiles' ? [{ name: 'dev' }, { name: 'off' }] : [],
  }));
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  adapter.populateConfigForm({});
  await new Promise(resolve => setTimeout(resolve, 0));
  const sandbox = mounted.container.querySelector('select[aria-label="Ask sandbox policy"]');
  assert.ok(sandbox);
  assert.equal(sandbox.querySelector('option[value="global"]').selected, true);
  assert.equal(adapter.assembleConfig().ask?.sandbox, undefined);
  assert.equal(sandbox.querySelector('option[value="profile:off"]').textContent, 'off');
  for (const value of ['auto', 'off', 'profile:dev', 'profile:off']) {
    await selectValue(harness, sandbox, value);
    assert.equal(adapter.assembleConfig().ask.sandbox, value);
  }
  adapter.populateConfigForm({ ask: { sandbox: 'deleted', model: 'haiku' } });
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.match(sandbox.querySelector('option[value="profile:deleted"]').textContent, /missing/);
  assert.equal(adapter.assembleConfig().ask.sandbox, 'profile:deleted');
  assert.equal(adapter.assembleConfig().ask.model, 'haiku');
  await mounted.unmount();
});

test('Config voice phrases: each has its own switch, and hands-free stays separate', async (t) => {
  const harness = await createPreactHarness(t);
  t.mock.method(globalThis, 'fetch', async () => ({ ok: true, json: async () => [] }));
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  const el = (id) => mounted.container.querySelector(`#${id}`);
  const click = (id) => harness.act(() => {
    el(id).checked = !el(id).checked;
    el(id).dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  });
  const voice = () => adapter.assembleConfig().dashboard?.voice;

  adapter.populateConfigForm({});
  assert.equal(el('cfg-voice-on-send').checked, true);
  assert.equal(el('cfg-voice-on-wake').checked, false);
  assert.equal(el('cfg-voice-phrase-send').disabled, false);
  assert.equal(el('cfg-voice-phrase-wake').disabled, true);
  assert.equal(el('cfg-voice-phrase-wake').value, 'hey agent');
  assert.equal(voice(), undefined, 'the defaults store nothing');
  assert.equal(el('cfg-voice-mark').checked, true, 'dictated messages are marked by default');
  await click('cfg-voice-mark');
  assert.deepEqual(voice(), { mark_dictated: false });
  await click('cfg-voice-mark');
  assert.equal(voice(), undefined);

  // Unticking keeps the text, disables the field, and re-ticking restores it.
  await click('cfg-voice-on-send');
  assert.equal(el('cfg-voice-phrase-send').disabled, true);
  assert.equal(el('cfg-voice-phrase-send').value, 'send now');
  assert.deepEqual(voice().phrases, [{ action: 'send', phrase: 'send now', enabled: false }]);
  await click('cfg-voice-on-send');
  assert.equal(el('cfg-voice-phrase-send').disabled, false);
  assert.equal(voice(), undefined);

  // Continuous mode is off, and the stop phrase on, until changed.
  assert.equal(el('cfg-voice-continuous').checked, false);
  assert.equal(el('cfg-voice-on-stop').checked, true);
  assert.equal(el('cfg-voice-phrase-stop').value, 'stop listening');
  await click('cfg-voice-continuous');
  assert.deepEqual(voice(), { continuous_enabled: true }, 'the default silence stores nothing');
  el('cfg-voice-continuous-idle').value = '45';
  assert.deepEqual(voice(), { continuous_enabled: true, continuous_idle_seconds: 45 });
  el('cfg-voice-continuous-idle').value = '';
  await click('cfg-voice-continuous');
  el('cfg-voice-phrase-stop').value = 'Send  Now';
  assert.throws(() => adapter.assembleConfig(), /send phrase and the stop phrase must differ/);
  el('cfg-voice-phrase-stop').value = 'stop listening';
  assert.equal(voice(), undefined);

  // Pause punctuation is off until asked for, and each mark has its own switch.
  assert.equal(el('cfg-voice-comma').checked, false);
  assert.equal(el('cfg-voice-period').checked, false);
  await click('cfg-voice-comma');
  await click('cfg-voice-period');
  assert.deepEqual(voice(), { comma_pause_seconds: 0.7, period_pause_seconds: 1.5 });
  el('cfg-voice-comma-seconds').value = '1.5';
  assert.throws(() => adapter.assembleConfig(), /comma pause must be shorter/);
  el('cfg-voice-comma-seconds').value = '0.5';
  el('cfg-voice-period-seconds').value = '4';
  await click('cfg-voice-silence');
  el('cfg-voice-silence-seconds').value = '3';
  assert.throws(() => adapter.assembleConfig(), /shorter than send after silence/);
  await click('cfg-voice-silence');
  assert.deepEqual(voice(), { comma_pause_seconds: 0.5, period_pause_seconds: 4 });
  await click('cfg-voice-comma');
  await click('cfg-voice-period');
  el('cfg-voice-comma-seconds').value = '';
  el('cfg-voice-period-seconds').value = '';
  el('cfg-voice-silence-seconds').value = '';
  assert.equal(voice(), undefined);
  adapter.populateConfigForm({ dashboard: { voice: { comma_pause_seconds: 0.9 } } });
  assert.equal(el('cfg-voice-comma').checked, true);
  assert.equal(el('cfg-voice-comma-seconds').value, '0.9');
  adapter.populateConfigForm({});

  // Neither wake setting moves the other; a one-sided combination gets a hint.
  await click('cfg-voice-wake');
  assert.equal(el('cfg-voice-on-wake').checked, false);
  assert.equal(el('cfg-voice-wake-no-phrase').hidden, false);
  assert.deepEqual(voice(), { wake_enabled: true });
  await click('cfg-voice-wake');
  await click('cfg-voice-on-wake');
  assert.equal(el('cfg-voice-wake').checked, false);
  assert.equal(el('cfg-voice-wake-no-phrase').hidden, true);
  assert.equal(el('cfg-voice-wake-no-listener').hidden, false);
  assert.deepEqual(voice(), { phrases: [{ action: 'wake', phrase: 'hey agent', enabled: true }] });

  // The room warning needs a wake that can really start.
  await click('cfg-voice-silence');
  assert.equal(el('cfg-voice-hands-free-warning').hidden, true);
  await click('cfg-voice-wake');
  assert.equal(el('cfg-voice-hands-free-warning').hidden, false);
  await click('cfg-voice-on-wake');
  assert.equal(el('cfg-voice-hands-free-warning').hidden, true);

  // A wake phrase only clashes with a send phrase that is on, and an empty
  // phrase can't be on.
  adapter.populateConfigForm({ dashboard: { voice: { phrases: [{ action: 'wake', phrase: 'send now', enabled: true }] } } });
  assert.throws(() => adapter.assembleConfig(), /must differ/);
  await click('cfg-voice-on-send');
  assert.ok(voice().phrases);
  el('cfg-voice-phrase-wake').value = ' ';
  assert.throws(() => adapter.assembleConfig(), /Focused phrase: type a phrase/);

  // The older ways to turn a phrase off read as off, with text to come back to.
  adapter.populateConfigForm({ dashboard: { dictation_send_phrase: '' } });
  assert.equal(el('cfg-voice-on-send').checked, false);
  assert.equal(el('cfg-voice-phrase-send').value, 'send now');
  adapter.populateConfigForm({ dashboard: { dictation_send_phrase: 'over' } });
  assert.equal(el('cfg-voice-on-send').checked, true);
  assert.equal(el('cfg-voice-phrase-send').value, 'over');
  adapter.populateConfigForm({ dashboard: { voice: { phrases: [{ action: 'send', phrase: '' }] } } });
  assert.equal(el('cfg-voice-on-send').checked, false);
  adapter.populateConfigForm({ dashboard: { voice: { phrases: [{ action: 'send', enabled: true }] } } });
  assert.equal(el('cfg-voice-on-send').checked, false, 'a hand-edited entry with no phrase loads');
  await mounted.unmount();
});

test('picking a Config section is a navigation; a history location shows it', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, { CONFIG_PANELS }, core, panel] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-markup.js'), harness.importDashboardModule('js/nav-history-core.js'),
    harness.importDashboardModule('js/config-panel.js'),
  ]);
  assert.deepEqual(new Set(CONFIG_PANELS.map((p) => p.id)), core.KNOWN_SUBTABS.config, 'every section has a route');
  const navigated = [];
  const onNav = (e) => navigated.push(e.detail.location);
  harness.document.addEventListener('tclaude:navigated', onNav);
  t.after(() => harness.document.removeEventListener('tclaude:navigated', onNav));
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  const link = (id) => mounted.container.querySelector(`[data-cfg-panel-link="${id}"]`);
  await harness.act(() => link('daemon').click());
  assert.deepEqual(navigated, [{ tab: 'config', subtab: 'daemon' }]);
  assert.equal(link('daemon').getAttribute('aria-current'), 'page');
  await harness.act(() => link('daemon').click());
  assert.equal(navigated.length, 1, 're-picking the shown section records nothing');
  // The router restores a section by setting it directly.
  await harness.act(() => { panel.setConfigPanel('logs'); });
  assert.equal(link('logs').getAttribute('aria-current'), 'page');
  assert.equal(mounted.container.querySelector('[data-cfg-panel="logs"]').getAttribute('data-cfg-current'), 'true');
  assert.equal(navigated.length, 1);
  await mounted.unmount();
});

test('Config lists an external steward\'s tools from the daemon, and nothing without them', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'), harness.importDashboardModule('js/config-island.js'),
  ]);
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`);
  await harness.act(() => state.lifecycle.loaded({ raw: '{}' }));
  assert.equal(mounted.container.querySelector('.cfg-tool-list'), null);

  await harness.act(() => state.lifecycle.loaded({
    raw: '{}',
    external_steward: { enabled: false, tools: [
      { name: 'whoami', description: 'Show its name.', access: 'Always available' },
      { name: 'notify_human', description: '<b>Tell</b> the human.', access: 'Needs human.notify' },
    ] },
  }));
  const rows = mounted.container.querySelectorAll('.cfg-tool-list tbody tr');
  assert.equal(rows.length, 2);
  assert.match(rows[0].textContent, /whoami.*Always available/s);
  assert.match(rows[1].textContent, /human\.notify/);
  assert.equal(rows[1].querySelector('b'), null, 'descriptions are text');
  assert.match(mounted.container.textContent, /Off\. No credential can be made or used\./);
  await mounted.unmount();
});
