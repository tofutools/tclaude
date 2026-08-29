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
  const headings = [...mounted.container.querySelectorAll('.cfg-section > h3')].map(node => node.textContent);

  assert.ok(headings.includes('Terminals & windows'));
  assert.ok(headings.includes('Usage, costs & rate limits'));
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
    log_level: 'warn', terminal: 'ghostty', record_hooks: true,
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
  assert.equal(mounted.container.querySelector('#cfg-dashboard-default-web-terminal').checked, false);
  assert.equal(mounted.container.querySelector('#cfg-dashboard-default-web-directory-picker').checked, false);
  assert.equal(mounted.container.querySelector('#cfg-ratelimit-5h').disabled, false);
  assert.match(mounted.container.querySelector('#cfg-notice').textContent, /future_root/);
  const assembled = adapter.assembleConfig();
  assert.equal(assembled.ratelimit.future_limit, 7);
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

  const assembled = adapter.assembleConfig();
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
