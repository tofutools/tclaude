// js/phone-plugins.js: the Plugins tab on a phone, a card per plugin with its
// steps as rows, run on the desktop's state and actions.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const storage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };

const snapshotOf = (plugins) => ({
  plugins,
  plugins_catalog: [{ name: 'github', steps: [] }],
  plugins_warn: 1, plugins_tab_visible: true,
});

async function setup(t, plugins) {
  const harness = await createPreactHarness(t);
  const [{ createPluginsState }, { PhonePlugins }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'),
    harness.importDashboardModule('js/phone-plugins.js'),
  ]);
  const snapshot = harness.signals.signal(snapshotOf(plugins));
  const poll = harness.signals.signal({ phase: 'ok', requestId: 1, error: null });
  const state = createPluginsState({ snapshot, poll, prefs: storage });
  const calls = [];
  const actions = {
    checkAll: () => calls.push(['checkAll']),
    checkPlugin: (plugin) => calls.push(['check', plugin.name]),
    togglePlugin: (plugin, verb) => calls.push(['toggle', plugin.name, verb]),
    toggleStep: (plugin, index, verb) => calls.push(['step', plugin.name, index, verb]),
  };
  const mounted = await harness.mount(harness.html`<${PhonePlugins} state=${state} actions=${actions} />`);
  return { harness, state, snapshot, calls, c: mounted.container, mounted };
}

test('a card per plugin: on/off, Check, steps with run and stop', async (t) => {
  const { harness, calls, c } = await setup(t, [
    { name: 'postgres-dev', descr: 'Local DB', status: 'warn', disabled: false, steps: [
      { name: 'container', check: 'docker inspect pg', run: 'docker run pg', stop: 'docker stop pg', checked: true, ok: true },
      { name: 'migrations', check: 'migrate status', run: 'migrate up', checked: true, ok: false, output: 'exit 1 · relation "orders" missing\nmore' },
      { name: 'notes' },
    ] },
    { name: 'vpn-tunnel', status: 'unknown', disabled: true, steps: [] },
  ]);
  const cards = [...c.querySelectorAll('.phone-plugin')];
  assert.deepEqual(cards.map((card) => card.querySelector('.phone-plugin-name').textContent), ['postgres-dev', 'vpn-tunnel']);
  const [pg, vpn] = cards;
  assert.equal(pg.querySelector('input[role=switch], input[type=checkbox]').getAttribute('aria-label'), 'postgres-dev: activate');
  assert.match(pg.querySelector('.phone-plugin-sub').textContent, /not active · Local DB/);
  const steps = [...pg.querySelectorAll('.phone-plugin-step')];
  assert.deepEqual(steps.map((step) => step.querySelector('.phone-plugin-act')?.getAttribute('aria-label') || ''),
    ['Stop container', 'Run migrations', ''], 'a step offers what its lamp offers');
  assert.equal(steps[1].querySelector('.phone-plugin-out').textContent, 'exit 1 · relation "orders" missing');
  await harness.act(() => steps[0].querySelector('.phone-plugin-act').click());
  await harness.act(() => steps[1].querySelector('.phone-plugin-act').click());
  await harness.act(() => harness.fireEvent(pg.querySelector('input'), 'change'));
  await harness.act(() => [...pg.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Check').click());
  await harness.act(() => harness.fireEvent(vpn.querySelector('input'), 'change'));
  await harness.act(() => [...c.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Check all').click());
  assert.deepEqual(calls, [
    ['step', 'postgres-dev', 0, 'stop'], ['step', 'postgres-dev', 1, 'run'],
    ['toggle', 'postgres-dev', 'activate'], ['check', 'postgres-dev'],
    ['toggle', 'vpn-tunnel', 'activate'], ['checkAll'],
  ]);
  assert.match(c.querySelector('.phone-desktop-only').textContent, /catalog and editing plugins are desktop only/);
  assert.equal(c.textContent.includes('github'), false, 'no catalog');
  assert.equal([...c.querySelectorAll('button')].some((b) => /^(edit|delete|\+ install)$/i.test(b.textContent.trim())), false, 'no editor, delete or install');
});

test('nothing installed, and an unreadable registry', async (t) => {
  const { harness, c, snapshot } = await setup(t, []);
  assert.match(c.textContent, /No plugins installed yet/);
  assert.equal([...c.querySelectorAll('button')].some((b) => b.textContent.includes('Check all')), false);
  await harness.act(() => { snapshot.value = { ...snapshot.value, plugins_error: 'bad json' }; });
  assert.match(c.querySelector('[role=alert]').textContent, /unreadable: bad json/);
});
