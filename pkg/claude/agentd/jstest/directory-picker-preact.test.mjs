import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('directory picker state serializes requests and settles explicit results', async (t) => {
  const harness = await createPreactHarness(t);
  const { createDirectoryPickerState } = await harness.importDashboardModule('js/directory-picker-state.js');
  const state = createDirectoryPickerState();
  const first = state.open({ startDir: '/repo', title: 'Choose' });
  assert.deepEqual(state.request.value, { startDir: '/repo', title: 'Choose' });
  assert.deepEqual(await state.open({ startDir: '/other' }), { error: 'a directory picker is already open' });
  state.finish({ path: '/repo' });
  assert.deepEqual(await first, { path: '/repo' });
  assert.equal(state.request.value, null);
});

test('directory picker actions preserve host path payloads and API errors', async (t) => {
  const harness = await createPreactHarness(t);
  const { createDirectoryPickerActions } = await harness.importDashboardModule('js/directory-picker-actions.js');
  const requests = [];
  const actions = createDirectoryPickerActions({
    fetchImpl: async (url, options) => {
      requests.push([url, options]);
      return new Response(JSON.stringify({ path: '/srv', directories: [] }), {
        status: 200, headers: { 'Content-Type': 'application/json' },
      });
    },
  });
  assert.equal((await actions.browse(' /srv ')).path, '/srv');
  assert.equal(requests[0][0], '/api/browse-directories');
  assert.deepEqual(JSON.parse(requests[0][1].body), { path: '/srv' });

  const failing = createDirectoryPickerActions({
    fetchImpl: async () => new Response(JSON.stringify({ error: 'permission denied' }), {
      status: 403, headers: { 'Content-Type': 'application/json' },
    }),
  });
  await assert.rejects(() => failing.browse('/root'), /permission denied/);
});

test('Preact picker navigates host folders, chooses, cancels, and restores focus', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDirectoryPickerState }, { DirectoryPickerApp }] = await Promise.all([
    harness.importDashboardModule('js/directory-picker-state.js'),
    harness.importDashboardModule('js/directory-picker-island.js'),
  ]);
  const state = createDirectoryPickerState();
  const calls = [];
  const actions = {
    async browse(path) {
      calls.push(path);
      if (!path || path === '/root') {
        return {
          path: '/root', parent: '/', home: '/home/me',
          directories: [{ name: 'alpha', path: '/root/alpha' }],
        };
      }
      return { path, parent: '/root', home: '/home/me', directories: [] };
    },
  };
  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.focus();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${DirectoryPickerApp} state=${state} actions=${actions} />`, host);

  let chosen;
  await harness.act(() => { chosen = state.open({ startDir: '/root', title: 'Select a workspace' }); });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(host.querySelector('#directory-picker-title').textContent, 'Select a workspace');
  assert.equal(host.querySelector('#directory-picker-path').value, '/root');
  assert.equal(harness.document.activeElement.id, 'directory-picker-path');
  host.querySelector('.directory-picker-list button').click();
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(calls, ['/root', '/root/alpha']);
  assert.equal(host.querySelector('#directory-picker-path').value, '/root/alpha');
  assert.match(host.querySelector('.directory-picker-empty').textContent, /No subdirectories/);
  host.querySelector('.modal-buttons button.primary').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(await chosen, { path: '/root/alpha' });
  assert.equal(host.querySelector('#directory-picker-modal'), null);
  assert.equal(harness.document.activeElement, invoker);

  let canceled;
  await harness.act(() => { canceled = state.open({}); });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  host.querySelector('.modal-buttons button:not(.primary)').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(await canceled, { canceled: true });
  await mounted.unmount();
});

test('helper policy recognizes loopback and the configured web bridge', async (t) => {
  const harness = await createPreactHarness(t);
  const helpers = await harness.importDashboardModule('js/helpers.js');
  assert.equal(helpers.isLoopbackDashboard('localhost'), true);
  assert.equal(helpers.isLoopbackDashboard('127.0.0.42'), true);
  assert.equal(helpers.isLoopbackDashboard('::1'), true);
  assert.equal(helpers.isLoopbackDashboard('dashboard.example.test'), false);
  helpers.configureDirectoryPickerBridge({
    prefersWeb: () => true,
    open: async (options) => ({ path: options.startDir }),
  });
  assert.deepEqual(await helpers.pickDirectory({ startDir: '/remote' }), { path: '/remote' });
  helpers.configureDirectoryPickerBridge(null);
});
