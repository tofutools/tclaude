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

test('directory picker derives a final-component filter and ranks prefixes first', async (t) => {
  const harness = await createPreactHarness(t);
  const { directoryFilterTerm, filterDirectories } = await harness.importDashboardModule(
    'js/directory-picker-island.js',
  );
  assert.equal(directoryFilterTerm('/root/tcl', '/root'), 'tcl');
  assert.equal(directoryFilterTerm('/root/', '/root'), '');
  assert.equal(directoryFilterTerm('/root', '/root'), '');
  assert.equal(directoryFilterTerm('/tmp/tcl', '/root'), null);
  assert.equal(directoryFilterTerm('/root/team/tcl', '/root'), null);
  assert.equal(directoryFilterTerm('/tmp', '/'), 'tmp');

  const directories = [
    { name: 'project-tclaude', path: '/root/project-tclaude' },
    { name: 'TClaude', path: '/root/TClaude' },
    { name: 'alpha', path: '/root/alpha' },
    { name: 'tclaude-dir-picker', path: '/root/tclaude-dir-picker' },
  ];
  assert.deepEqual(
    filterDirectories(directories, 'tcl').map((directory) => directory.name),
    ['TClaude', 'tclaude-dir-picker', 'project-tclaude'],
  );
  assert.equal(filterDirectories(directories, ''), directories);
});

test('Preact picker filters its folder pane and completes the active match', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDirectoryPickerState }, { DirectoryPickerApp }] = await Promise.all([
    harness.importDashboardModule('js/directory-picker-state.js'),
    harness.importDashboardModule('js/directory-picker-island.js'),
  ]);
  const state = createDirectoryPickerState();
  const calls = [];
  const rootView = {
    path: '/root', parent: '/', home: '/home/me',
    directories: [
      { name: 'alpha', path: '/root/alpha' },
      { name: 'tclaude', path: '/root/tclaude' },
      { name: 'tclaude-dir-picker', path: '/root/tclaude-dir-picker' },
      { name: 'project-tclaude', path: '/root/project-tclaude' },
      { name: 'zebra', path: '/root/zebra' },
    ],
  };
  const actions = {
    async browse(path) {
      calls.push(path);
      if (path === '/root') return rootView;
      return { path, parent: '/root', home: '/home/me', directories: [] };
    },
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${DirectoryPickerApp} state=${state} actions=${actions} />`, host);
  let result;
  await harness.act(() => { result = state.open({ startDir: '/root', title: 'Choose' }); });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));

  const input = host.querySelector('#directory-picker-path');
  await harness.input(input, '/root/tcl');
  assert.deepEqual(
    [...host.querySelectorAll('.directory-picker-list button')].map((button) => button.textContent),
    ['📁tclaude', '📁tclaude-dir-picker', '📁project-tclaude'],
  );
  assert.equal(host.querySelector('.directory-picker-count').textContent, '3 of 5 folders');
  assert.equal(host.querySelector('.directory-picker-list button.active').title, '/root/tclaude');
  assert.equal(input.getAttribute('role'), 'combobox');
  assert.equal(input.getAttribute('aria-autocomplete'), 'list');
  assert.equal(host.querySelector('.directory-picker-list').getAttribute('role'), 'listbox');
  assert.equal(input.getAttribute('aria-activedescendant'), 'directory-picker-option-1');
  assert.equal(host.querySelector('.directory-picker-list button.active').getAttribute('role'), 'option');
  assert.equal(host.querySelector('.directory-picker-list button.active').getAttribute('aria-selected'), 'true');
  assert.equal(host.querySelector('.directory-picker-list button.active').tabIndex, -1);

  const down = harness.fireEvent(input, 'keydown', { key: 'ArrowDown' });
  await harness.act(() => Promise.resolve());
  assert.equal(down.defaultPrevented, true);
  assert.equal(host.querySelector('.directory-picker-list button.active').title, '/root/tclaude-dir-picker');
  assert.equal(input.getAttribute('aria-activedescendant'), 'directory-picker-option-2');
  assert.equal(host.querySelector('.directory-picker-list button.active').getAttribute('aria-selected'), 'true');
  assert.equal(host.querySelector('#directory-picker-option-1').getAttribute('aria-selected'), 'false');

  const reverseTab = harness.fireEvent(input, 'keydown', { key: 'Tab', shiftKey: true });
  await harness.act(() => Promise.resolve());
  assert.equal(reverseTab.defaultPrevented, true, 'dialog focus trap handles reverse Tab');
  assert.equal(input.value, '/root/tcl');

  const tab = harness.fireEvent(input, 'keydown', { key: 'Tab' });
  await harness.act(() => Promise.resolve());
  assert.equal(tab.defaultPrevented, true);
  assert.equal(input.value, '/root/tclaude-dir-picker');
  assert.equal(host.querySelector('.directory-picker-count').textContent, '1 of 5 folders');
  const completedTab = harness.fireEvent(input, 'keydown', { key: 'Tab' });
  assert.equal(completedTab.defaultPrevented, false);

  await harness.input(input, '/root/tcl');
  harness.fireEvent(input, 'keydown', { key: 'ArrowDown' });
  await harness.act(() => Promise.resolve());
  harness.fireEvent(host.querySelector('.directory-picker-path'), 'submit');
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(calls, ['/root', '/root/tclaude-dir-picker']);
  assert.equal(input.value, '/root/tclaude-dir-picker');
  state.finish({ canceled: true });
  assert.deepEqual(await result, { canceled: true });
  await mounted.unmount();
});

test('Preact picker leaves the list intact for a directly typed path', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDirectoryPickerState }, { DirectoryPickerApp }] = await Promise.all([
    harness.importDashboardModule('js/directory-picker-state.js'),
    harness.importDashboardModule('js/directory-picker-island.js'),
  ]);
  const state = createDirectoryPickerState();
  const calls = [];
  const actions = { browse: async (path) => {
    calls.push(path);
    return { path, parent: '/', home: '/home/me', directories: path === '/root'
      ? [{ name: 'alpha', path: '/root/alpha' }, { name: 'beta', path: '/root/beta' }]
      : [] };
  } };
  const mounted = await harness.mount(
    harness.html`<${DirectoryPickerApp} state=${state} actions=${actions} />`,
  );
  let result;
  await harness.act(() => { result = state.open({ startDir: '/root', title: 'Choose' }); });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  const input = mounted.container.querySelector('#directory-picker-path');
  await harness.input(input, '/other/workspace');
  assert.equal(mounted.container.querySelectorAll('.directory-picker-list button').length, 2);
  assert.equal(input.getAttribute('role'), null);
  assert.equal(mounted.container.querySelector('.directory-picker-list').getAttribute('role'), 'list');
  assert.match(mounted.container.querySelector('.directory-picker-hint').textContent, /Press Enter/);
  harness.fireEvent(mounted.container.querySelector('.directory-picker-path'), 'submit');
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(calls, ['/root', '/other/workspace']);
  state.finish({ canceled: true });
  await result;
  await mounted.unmount();
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
  assert.equal(helpers.isLoopbackDashboard('[::1]'), true);
  assert.equal(helpers.isLoopbackDashboard('localhost.'), true);
  assert.equal(helpers.isLoopbackDashboard('127.example.test'), false);
  assert.equal(helpers.isLoopbackDashboard('127.0.0.999'), false);
  assert.equal(helpers.isLoopbackDashboard('dashboard.example.test'), false);
  helpers.configureDirectoryPickerBridge({
    prefersWeb: () => true,
    open: async (options) => ({ path: options.startDir }),
  });
  assert.deepEqual(await helpers.pickDirectory({ startDir: '/remote' }), { path: '/remote' });
  helpers.configureDirectoryPickerBridge(null);
});
