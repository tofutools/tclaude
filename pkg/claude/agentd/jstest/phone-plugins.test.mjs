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
  const [{ createPluginsState }, { PhonePlugins }, { phoneBar }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'),
    harness.importDashboardModule('js/phone-plugins.js'),
    harness.importDashboardModule('js/phone-page.js'),
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
  return { harness, state, snapshot, calls, c: mounted.container, mounted, phoneBar };
}

test('a card per plugin: on/off, Check, steps with run and stop', async (t) => {
  const { harness, calls, c, phoneBar } = await setup(t, [
    { name: 'postgres-dev', descr: 'Local DB', status: 'warn', disabled: false, steps: [
      { name: 'container', check: 'docker inspect pg', run: 'docker run pg', stop: 'docker stop pg', checked: true, ok: true },
      { name: 'migrations', check: 'migrate status', run: 'migrate up', checked: true, ok: false, output: 'exit 1 · relation "orders" missing\nmore' },
      { name: 'notes' },
    ] },
    { name: 'vpn-tunnel', status: 'unknown', disabled: true, steps: [] },
    { name: 'ollama', status: 'ok', disabled: false, steps: [] },
    { name: 'tunnel-left', status: 'warn', disabled: true, steps: [] },
  ]);
  const cards = [...c.querySelectorAll('.phone-plugin')];
  assert.deepEqual(cards.map((card) => card.querySelector('.phone-plugin-name').textContent), ['postgres-dev', 'vpn-tunnel', 'ollama', 'tunnel-left']);
  const [pg, vpn, ollama, left] = cards;
  // The switch is on exactly when the desktop lamp would deactivate, so a
  // flip always does what it shows: an enabled plugin that is not active
  // (warn) shows off and activates; one still active after a teardown shows
  // on and deactivates.
  const label = (card) => card.querySelector('input[role=switch]').getAttribute('aria-label');
  assert.deepEqual([pg, vpn, ollama, left].map(label),
    ['postgres-dev: activate', 'vpn-tunnel: activate', 'ollama: deactivate', 'tunnel-left: deactivate']);
  assert.equal(pg.querySelector('.phone-plugin-sub').firstChild.textContent, 'Local DB', 'the description, as the mock');
  assert.equal(pg.querySelector('.phone-plugin-sr').textContent, ' · not active', 'its state, read out');
  // A plugin that needs a look shows its steps; one off, or with every check
  // passing, folds to one line (the mock's ollama and vpn-tunnel), dimmed when off.
  const shown = (card) => card.querySelector('.phone-plugin-fold').getAttribute('aria-expanded');
  assert.deepEqual([pg, vpn, ollama, left].map(shown), ['true', 'false', 'false', 'true']);
  assert.equal(vpn.querySelector('.phone-plugin-sub').textContent, 'off', 'off already says it');
  assert.ok(vpn.classList.contains('off'));
  assert.equal(ollama.classList.contains('off'), false);
  assert.equal(ollama.querySelector('.phone-plugin-steps, .phone-plugin-check'), null, 'folded: no steps and no Check');
  const steps = [...pg.querySelectorAll('.phone-plugin-step')];
  assert.deepEqual(steps.map((step) => step.querySelector('.phone-plugin-act')?.getAttribute('aria-label') || ''),
    ['Stop container', 'Run migrations', ''], 'a step offers what its lamp offers');
  assert.equal(steps[1].querySelector('.phone-plugin-out').textContent, 'exit 1 · relation "orders" missing');
  await harness.act(() => steps[0].querySelector('.phone-plugin-act').click());
  await harness.act(() => steps[1].querySelector('.phone-plugin-act').click());
  await harness.act(() => harness.fireEvent(pg.querySelector('input'), 'change'));
  await harness.act(() => [...pg.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Check').click());
  await harness.act(() => harness.fireEvent(vpn.querySelector('input'), 'change'));
  await harness.act(() => harness.fireEvent(ollama.querySelector('input'), 'change'));
  await harness.act(() => harness.fireEvent(left.querySelector('input'), 'change'));
  // Check all is in the app bar, as a word.
  const checkAll = phoneBar.value.actions[0];
  assert.deepEqual([phoneBar.value.tab, checkAll.label, checkAll.text], ['plugins', 'Check all', 'Check all']);
  await harness.act(() => checkAll.onClick());
  assert.deepEqual(calls, [
    ['step', 'postgres-dev', 0, 'stop'], ['step', 'postgres-dev', 1, 'run'],
    ['toggle', 'postgres-dev', 'activate'], ['check', 'postgres-dev'],
    ['toggle', 'vpn-tunnel', 'activate'], ['toggle', 'ollama', 'deactivate'], ['toggle', 'tunnel-left', 'deactivate'], ['checkAll'],
  ]);
  assert.match(c.querySelector('.phone-desktop-only').textContent, /catalog and editing plugins are desktop only/);
  assert.equal(c.textContent.includes('github'), false, 'no catalog');
  assert.equal([...c.querySelectorAll('button')].some((b) => /^(edit|delete|\+ install)$/i.test(b.textContent.trim())), false, 'no editor, delete or install');
});

test('nothing installed, and an unreadable registry', async (t) => {
  const { harness, c, snapshot, phoneBar } = await setup(t, []);
  assert.match(c.textContent, /No plugins installed yet/);
  assert.equal(phoneBar.value, null, 'no Check all with nothing to check');
  await harness.act(() => { snapshot.value = { ...snapshot.value, plugins_error: 'bad json' }; });
  assert.match(c.querySelector('[role=alert]').textContent, /unreadable: bad json/);
});

test('a folded card opens on a tap and says how many steps and when they were checked', async (t) => {
  const { harness, c } = await setup(t, [
    { name: 'ollama', descr: '', status: 'ok', disabled: false, steps: [
      { name: 'serve', check: 'curl :11434', checked: true, ok: true, checked_at: new Date(Date.now() - 3 * 60_000).toISOString() },
      { name: 'model', check: 'ollama list', checked: true, ok: true, checked_at: new Date(Date.now() - 5 * 60_000).toISOString() },
    ] },
  ]);
  const card = c.querySelector('.phone-plugin');
  assert.equal(card.querySelector('.phone-plugin-sub').textContent, '2 steps · checked 3m ago · active',
    'the line, and its state for a screen reader');
  assert.equal(card.querySelector('.phone-plugin-fold').getAttribute('aria-label'), null, 'the button reads as its text');
  await harness.act(() => card.querySelector('.phone-plugin-fold').click());
  assert.equal(card.querySelectorAll('.phone-plugin-step').length, 2);
  assert.ok(card.querySelector('.phone-plugin-check'));
  await harness.act(() => card.querySelector('.phone-plugin-fold').click());
  assert.equal(card.querySelector('.phone-plugin-step'), null);
});

test('lastChecked compares times, not text', async (t) => {
  const harness = await createPreactHarness(t);
  const { lastChecked } = await harness.importDashboardModule('js/phone-plugins.js');
  // 03:30+02:00 is 01:30Z; 02:45+01:00 is 01:45Z, the later one, though it sorts first as text.
  assert.equal(lastChecked({ steps: [{ checked_at: '2026-10-25T03:30:00+02:00' }, { checked_at: '2026-10-25T02:45:00+01:00' }, {}] }),
    '2026-10-25T02:45:00+01:00');
  assert.equal(lastChecked({ steps: [{ name: 'x' }] }), '');
});
