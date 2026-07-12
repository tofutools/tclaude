import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Plugins actions preserve routes, success messages, failures, and busy cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createPluginsState }, { createPluginsActions }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'),
    harness.importDashboardModule('js/plugins-actions.js'),
  ]);
  const state = createPluginsState({
    snapshot: harness.signals.signal(null),
    poll: harness.signals.signal({ phase: 'idle', requestId: 0, error: null }),
    prefs: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  });
  const calls = [];
  const notices = [];
  let fail = false;
  const actions = createPluginsActions({
    state,
    refresh: async () => calls.push(['refresh']),
    confirm: async () => true,
    notify: (message, error) => notices.push([message, error]),
    requestMutation: async (path, options) => {
      calls.push([path, options]);
      if (fail) throw Object.assign(new Error('HTTP 500'), { body: 'boom' });
      if (path.endsWith('/check')) return path === '/api/plugins/check' ? { warn: 1 } : { status: 'ok' };
      if (path.includes('/steps/')) return { ok: true, output: 'started\nmore' };
      if (path.endsWith('/activate')) return { ok: true, ran: true };
      return {};
    },
  });
  const plugin = { name: 'canvas', steps: [] };
  assert.equal(await actions.checkAll('all'), true);
  assert.equal(calls[0][0], '/api/plugins/check');
  assert.match(notices[0][0], /1 plugin not active/);

  assert.equal(await actions.toggleStep(plugin, 2, 'run', 'step'), true);
  assert.equal(calls[1][0], '/api/plugins/canvas/steps/2/run');
  assert.match(notices[1][0], /step run OK: started/);
  assert.equal(await actions.togglePlugin(plugin, 'activate', 'toggle'), true);
  assert.equal(calls[2][0], '/api/plugins/canvas/activate');

  fail = true;
  assert.equal(await actions.checkPlugin(plugin, 'failed'), false);
  assert.equal(state.busy.value.size, 0, 'busy state is released after failure');
  assert.deepEqual(notices.at(-1), ['Request failed: HTTP 500: boom', true]);
});

test('Plugins save surfaces validation failures in the modal and closes on success', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createPluginsState }, { createPluginsActions }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'),
    harness.importDashboardModule('js/plugins-actions.js'),
  ]);
  const state = createPluginsState({
    snapshot: harness.signals.signal(null),
    poll: harness.signals.signal({ phase: 'idle', requestId: 0, error: null }),
    prefs: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  });
  let failure = true;
  const calls = [];
  let refreshOptions = null;
  const actions = createPluginsActions({
    state, refresh: async (options) => { refreshOptions = options; }, confirm: async () => true, notify: () => {},
    requestMutation: async (...args) => {
      calls.push(args);
      if (failure) throw Object.assign(new Error('HTTP 400'), { body: 'name required' });
    },
  });
  state.openModal();
  state.updateModal({ name: '  demo  ', steps: [{ name: ' one ', check: '', run: ' true ', stop: '' }] });
  assert.equal(await actions.save(state.modal.value), false);
  assert.match(state.modal.value.error, /name required/);
  failure = false;
  assert.equal(await actions.save(state.modal.value), true);
  assert.equal(calls[1][0], '/api/plugins');
  assert.equal(calls[1][1].body.name, 'demo');
  assert.equal(calls[1][1].refreshAfter, false);
  assert.equal(state.modal.value, null);
  assert.deepEqual(refreshOptions, { force: true });
});
