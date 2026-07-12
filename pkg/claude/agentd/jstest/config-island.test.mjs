import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

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
  const terminal = getByRole(mounted.container, 'textbox', { name: 'Terminal emulator' });
  await harness.input(terminal, 'ghostty');
  assert.equal(state.view.value.dirty, true);
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
  assert.equal(mounted.container.querySelector('#cfg-terminal').value, 'ghostty');
  assert.equal(mounted.container.querySelector('#cfg-record-hooks').checked, true);
  assert.equal(mounted.container.querySelector('#cfg-ratelimit-5h').disabled, false);
  assert.match(mounted.container.querySelector('#cfg-notice').textContent, /future_root/);
  const assembled = adapter.assembleConfig();
  assert.equal(assembled.ratelimit.future_limit, 7);
  assert.equal(assembled.slop.volume, 0.4);
  assert.deepEqual(assembled.agent.sudo, { max_duration: '2h' });
  assert.equal(state.view.value.phase, 'ready');
  assert.equal(state.view.value.dirty, false);
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
  await harness.input(mounted.container.querySelector('#cfg-terminal'), 'ghostty');
  assert.equal(state.view.value.dirty, true);
  const saving = adapter.saveConfig();
  await new Promise(resolve => setTimeout(resolve, 0));
  const modal = mounted.container.querySelector('#config-diff-modal');
  assert.match(modal.querySelector('#config-diff-sub').textContent, /\/tmp\/config.json/);
  harness.fireEvent(modal.querySelector('#config-diff-confirm'), 'click');
  await saving;
  assert.deepEqual(requests.map(({ url }) => url), ['/api/config', '/api/config?dry_run=1', '/api/config']);
  const posted = JSON.parse(requests[1].options.body);
  assert.equal(posted.base, baseline);
  assert.equal(posted.config.terminal, 'ghostty');
  assert.equal(state.view.value.phase, 'ready');
  assert.equal(state.view.value.dirty, false);
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
  await harness.input(mounted.container.querySelector('#cfg-terminal'), 'unsaved');
  await adapter.saveConfig();
  assert.equal(calls, 2);
  assert.equal(state.view.value.phase, 'error');
  assert.equal(state.view.value.dirty, true);
  assert.match(state.view.value.error, /changed on disk/);
  assert.equal(mounted.container.querySelector('#cfg-terminal').value, 'unsaved');
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
  const terminal = mounted.container.querySelector('#cfg-terminal');
  terminal.focus();
  await harness.input(terminal, 'half-typed');
  harness.fireEvent(mounted.container.querySelector('#cfg-agent-permissions .cfg-list-add'), 'click');
  await new Promise(resolve => queueMicrotask(resolve));
  assert.equal(mounted.container.querySelector('#cfg-terminal'), terminal);
  assert.equal(terminal.value, 'half-typed');
  assert.equal(harness.document.activeElement,
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
    return { ok: true, json: async () => ({ raw: JSON.stringify({ terminal: `terminal-${loads}` }) }) };
  };
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const dependencies = { fetchImpl, isCyclingTabs: () => true };
  const first = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${dependencies} />`);
  harness.fireEvent(nav.querySelector('button'), 'click');
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(first.container.querySelector('#cfg-terminal').value, 'terminal-1');
  await first.unmount();

  const second = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${dependencies} />`);
  harness.fireEvent(nav.querySelector('button'), 'click');
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(loads, 2);
  assert.equal(second.container.querySelector('#cfg-terminal').value, 'terminal-2');
  await second.unmount();
  nav.remove();
});
