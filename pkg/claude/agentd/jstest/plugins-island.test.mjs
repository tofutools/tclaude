import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const storage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };

function page(name = 'canvas') {
  return {
    plugins: [{
      name, descr: 'drawing tools', status: 'warn', disabled: false,
      steps: [{ name: 'server', check: 'docker inspect canvas', run: 'docker run canvas', stop: 'docker stop canvas', checked: true, ok: false }],
    }],
    plugins_catalog: [{ name: 'github', descr: 'source control', steps: [{ name: 'server', run: 'gh serve' }] }],
    plugins_warn: 1, plugins_tab_visible: true,
  };
}

function actions(calls) {
  return {
    refresh: async () => calls.push('refresh'),
    checkAll: () => calls.push('check-all'), checkPlugin: () => calls.push('check'),
    toggleStep: () => calls.push('step'), togglePlugin: () => calls.push('toggle'),
    install: () => calls.push('install'), deletePlugin: () => calls.push('delete'),
    save: () => calls.push('save'),
  };
}

test('Plugins island renders loading/filter/error states and preserves keyed focus across polls', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createPluginsState }, { PluginsApp, PluginsBadge }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'),
    harness.importDashboardModule('js/plugins-island.js'),
  ]);
  const snapshot = harness.signals.signal(null);
  const poll = harness.signals.signal({ phase: 'loading', requestId: 1, error: null });
  const state = createPluginsState({ snapshot, poll, prefs: storage });
  state.initialize();
  const calls = [];
  const mounted = await harness.mount(harness.html`<${PluginsApp} state=${state} actions=${actions(calls)} />`);
  assert.match(mounted.container.textContent, /Loading plugins/);

  await harness.act(() => {
    snapshot.value = page();
    poll.value = { phase: 'ready', requestId: 1, error: null };
  });
  const badge = await harness.mount(harness.html`<${PluginsBadge} state=${state} />`);
  assert.equal(badge.container.querySelector('#plugins-badge').textContent, '1');
  const card = mounted.container.querySelector('[data-key="plugin-canvas"]');
  const edit = getByRole(card, 'button', { name: 'edit' });
  edit.focus();
  const nameText = card.querySelector('.rowname').firstChild;
  await harness.act(() => { snapshot.value = page(); });
  assert.equal(mounted.container.querySelector('[data-key="plugin-canvas"]'), card);
  assert.equal(card.querySelector('.rowname').firstChild, nameText);
  assert.equal(harness.document.activeElement, edit);

  const filter = getByRole(mounted.container, 'textbox', { name: 'Filter plugins' });
  await harness.input(filter, 'github');
  assert.match(mounted.container.textContent, /No plugin matches the filter/);
  assert.ok(mounted.container.querySelector('[data-key="catalog-github"]'));
  assert.equal(mounted.container.querySelector('#filter-plugins-count').textContent, '0 / 1');

  await harness.act(() => { snapshot.value = { ...page(), plugins_error: 'invalid json' }; });
  assert.match(mounted.container.textContent, /plugin registry unreadable/);
  await badge.unmount();
  await mounted.unmount();
});

test('Plugins modal supports create/edit fields, actions, and listener cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createPluginsState }, { PluginsModal }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'),
    harness.importDashboardModule('js/plugins-island.js'),
  ]);
  const state = createPluginsState({
    snapshot: harness.signals.signal(page()),
    poll: harness.signals.signal({ phase: 'ready', requestId: 1, error: null }), prefs: storage,
  });
  const calls = [];
  const mounted = await harness.mount(harness.html`<${PluginsModal} state=${state} actions=${actions(calls)} />`);
  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.textContent = 'open plugins';
  invoker.focus();
  await harness.act(() => state.openModal());
  await new Promise(resolve => queueMicrotask(resolve));
  const dialog = getByRole(mounted.container, 'dialog');
  assert.match(dialog.textContent, /New plugin/);
  assert.equal(dialog.querySelectorAll('.plugin-step-edit').length, 1);
  const name = dialog.querySelector('#plugin-modal-name');
  assert.equal(harness.document.activeElement, name);
  await harness.input(name, 'demo');
  assert.equal(state.modal.value.name, 'demo');
  assert.ok(dialog.querySelector('#plugin-modal-add-step'));
  await harness.act(() => state.addStep());
  assert.equal(state.modal.value.steps.length, 2);
  assert.equal(dialog.querySelectorAll('.plugin-step-edit').length, 2);
  assert.ok(dialog.querySelector('[data-step-remove]'));
  await harness.act(() => state.removeStep(0));
  assert.equal(dialog.querySelectorAll('.plugin-step-edit').length, 1);
  await harness.act(() => harness.fireEvent(dialog.querySelector('#plugin-modal-submit'), 'click'));
  assert.ok(calls.includes('save'));

  const submit = dialog.querySelector('#plugin-modal-submit');
  submit.focus();
  await harness.act(() => harness.fireEvent(submit, 'keydown', { key: 'Tab' }));
  assert.equal(harness.document.activeElement, name, 'Tab wraps to the first dialog control');
  await harness.act(() => harness.fireEvent(name, 'keydown', { key: 'Tab', shiftKey: true }));
  assert.equal(harness.document.activeElement, submit, 'Shift+Tab wraps to the last dialog control');
  invoker.focus();
  await harness.act(() => harness.fireEvent(invoker, 'keydown', { key: 'Tab' }));
  assert.equal(harness.document.activeElement, name, 'Tab from outside is contained at the first control');
  invoker.focus();
  await harness.act(() => harness.fireEvent(invoker, 'keydown', { key: 'Tab', shiftKey: true }));
  assert.equal(harness.document.activeElement, submit, 'Shift+Tab from outside is contained at the last control');
  await harness.act(() => harness.fireEvent(submit, 'keydown', { key: 'Escape' }));
  assert.equal(state.modal.value, null);
  assert.equal(harness.document.activeElement, invoker);
  assert.ok(calls.includes('refresh'));
  await mounted.unmount();
  invoker.remove();
});

test('hidden Plugins tab redirects an active deep link when active-tab state changes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createPluginsState }, { PluginsApp }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'),
    harness.importDashboardModule('js/plugins-island.js'),
  ]);
  const activeTab = harness.signals.signal('groups');
  const hidden = { ...page(), plugins: [], plugins_tab_visible: false };
  const state = createPluginsState({
    snapshot: harness.signals.signal(hidden), activeTab,
    poll: harness.signals.signal({ phase: 'ready', requestId: 1, error: null }), prefs: storage,
  });
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  const groups = nav.appendChild(harness.document.createElement('button'));
  groups.dataset.tab = 'groups';
  let clicks = 0;
  groups.addEventListener('click', () => { clicks += 1; });
  const mounted = await harness.mount(harness.html`<${PluginsApp} state=${state} actions=${actions([])} />`);
  assert.ok(harness.document.body.classList.contains('hide-plugins'));
  assert.equal(clicks, 0);
  await harness.act(() => { activeTab.value = 'plugins'; });
  assert.equal(clicks, 1);
  await mounted.unmount();
});

test('Plugins visibility remains unknown until the initial snapshot loads', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createPluginsState }, { PluginsApp }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'),
    harness.importDashboardModule('js/plugins-island.js'),
  ]);
  const snapshot = harness.signals.signal(null);
  const activeTab = harness.signals.signal('plugins');
  const state = createPluginsState({
    snapshot, activeTab,
    poll: harness.signals.signal({ phase: 'loading', requestId: 1, error: null }), prefs: storage,
  });
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  const groups = nav.appendChild(harness.document.createElement('button'));
  groups.dataset.tab = 'groups';
  let clicks = 0;
  groups.addEventListener('click', () => { clicks += 1; });
  const mounted = await harness.mount(harness.html`<${PluginsApp} state=${state} actions=${actions([])} />`);
  assert.equal(clicks, 0);
  assert.equal(harness.document.body.classList.contains('hide-plugins'), false);
  await harness.act(() => { snapshot.value = page(); });
  assert.equal(clicks, 0);
  assert.equal(harness.document.body.classList.contains('hide-plugins'), false);
  await mounted.unmount();
});

test('production loader owns and cleans up all Plugins hosts', async (t) => {
  const harness = await createPreactHarness(t);
  for (const id of ['plugins-root', 'plugins-badge-root', 'plugins-modal-root']) {
    const host = harness.document.body.appendChild(harness.document.createElement('div'));
    host.id = id;
  }
  const { mountPluginsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const cleanup = await mountPluginsFeature({
    requestMutation: async () => ({}), refresh: async () => {}, confirm: async () => true, notify: () => {},
  });
  assert.equal(typeof cleanup, 'function');
  assert.ok(harness.document.querySelector('#filter-plugins'));
  assert.ok(harness.document.querySelector('#plugins-badge'));
  cleanup();
  for (const id of ['plugins-root', 'plugins-badge-root', 'plugins-modal-root']) {
    assert.equal(harness.document.querySelector(`#${id}`).childElementCount, 0);
  }
});
