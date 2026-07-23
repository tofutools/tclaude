import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function response(body, { ok = true, status = 200, text = '' } = {}) {
  return {
    ok,
    status,
    json: async () => body,
    text: async () => text,
  };
}

const enabledSettings = {
  enabled: true,
  types: {
    idle: true,
    awaiting_permission: false,
    awaiting_input: true,
    error: true,
    exited: false,
  },
  human_messages: false,
  access_requests: true,
  delivery: 'both',
};

test('notification state normalizes daemon settings and keeps the bell on snapshot state', async (t) => {
  const harness = await createPreactHarness(t);
  const { createNotifyState, normalizeNotifySettings } =
    await harness.importDashboardModule('js/notify-state.js');
  const normalized = normalizeNotifySettings({ enabled: 1, types: { idle: 1 } });
  assert.equal(normalized.enabled, true);
  assert.equal(normalized.types.idle, true);
  assert.equal(normalized.types.exited, false);
  assert.equal(normalized.humanMessages, true, 'human messages default on unless explicitly false');
  assert.equal(normalized.delivery, 'os', 'absent delivery is the desktop default');
  assert.equal(normalizeNotifySettings({ delivery: 'browser' }).delivery, 'browser');
  assert.equal(normalizeNotifySettings({ delivery: 'carrier-pigeon' }).delivery, 'os',
    'an unrecognised delivery falls back to os');

  const snapshot = harness.signals.signal(null);
  const state = createNotifyState({ snapshot });
  assert.equal(state.view.value.bellReady, false);
  await harness.act(() => { snapshot.value = { notifications_enabled: true }; });
  assert.equal(state.view.value.bellReady, true);
  assert.equal(state.view.value.bellEnabled, true);

  const requestId = state.beginRequest();
  assert.equal(state.commitRequest(requestId, enabledSettings), true);
  assert.equal(state.view.value.settings.humanMessages, false);
  assert.equal(state.view.value.settings.accessRequests, true);
  assert.equal(state.commitRequest(requestId - 1, {}), false, 'stale responses cannot repaint state');
});

test('notification actions GET on every open, repaint from POST, and reload after failure', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createNotifyState }, { createNotifyActions }] = await Promise.all([
    harness.importDashboardModule('js/notify-state.js'),
    harness.importDashboardModule('js/notify-menu.js'),
  ]);
  const state = createNotifyState({ snapshot: harness.signals.signal({ notifications_enabled: true }) });
  const calls = [];
  const notices = [];
  const queue = [
    response(enabledSettings),
    response({ ...enabledSettings, enabled: false }),
    response({ ...enabledSettings, enabled: false, types: { ...enabledSettings.types, exited: true } }),
    response(null, { ok: false, status: 500, text: 'disk write failed' }),
    response(enabledSettings),
  ];
  const config = harness.document.body.appendChild(harness.document.createElement('button'));
  config.dataset.tab = 'config';
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  nav.append(config);
  let configClicks = 0;
  config.addEventListener('click', () => { configClicks += 1; });
  const actions = createNotifyActions({
    state,
    notify: (message, error) => notices.push([message, error]),
    documentRef: harness.document,
    fetchImpl: async (path, options = {}) => {
      calls.push([path, options]);
      return queue.shift();
    },
  });

  await actions.open();
  assert.equal(calls[0][1].credentials, 'same-origin');
  assert.equal(state.view.value.settings.enabled, true);
  actions.close();
  await actions.open();
  assert.equal(calls.filter(([, options]) => !options.method).length, 2, 'every open performs a fresh GET');
  assert.equal(state.view.value.settings.enabled, false);

  assert.equal(await actions.setType('exited', true), true);
  assert.equal(calls[2][1].method, 'POST');
  assert.deepEqual(JSON.parse(calls[2][1].body), { types: { exited: true } });
  assert.equal(state.view.value.settings.types.exited, true, 'POST response is authoritative');

  assert.equal(await actions.setEnabled(true), false);
  assert.deepEqual(notices.at(-1), ['Notification update failed: disk write failed', true]);
  assert.equal(state.view.value.settings.enabled, true, 'failure reload repaints from disk');
  assert.equal(calls[4][1].method, undefined, 'POST failure is followed by GET');

  actions.openConfig();
  assert.equal(state.open.value, false);
  assert.equal(configClicks, 1);
  nav.remove();
});

test('setDelivery persists the channel and asks the browser for permission when it includes the browser', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createNotifyState }, { createNotifyActions }] = await Promise.all([
    harness.importDashboardModule('js/notify-state.js'),
    harness.importDashboardModule('js/notify-menu.js'),
  ]);
  const state = createNotifyState({ snapshot: harness.signals.signal({ notifications_enabled: true }) });
  const calls = [];
  // A stub Notification so requestBrowserNotifyPermission has something to
  // call — the LinkeDOM window has none by default.
  let permissionRequests = 0;
  globalThis.Notification = class {
    static permission = 'default';
    static requestPermission = async () => { permissionRequests += 1; return 'granted'; };
  };
  t.after(() => { delete globalThis.Notification; });

  const actions = createNotifyActions({
    state,
    notify: () => {},
    documentRef: harness.document,
    fetchImpl: async (path, options = {}) => {
      calls.push([path, options]);
      return response({ ...enabledSettings, delivery: JSON.parse(options.body).delivery });
    },
  });

  assert.equal(await actions.setDelivery('both'), true);
  assert.deepEqual(JSON.parse(calls.at(-1)[1].body), { delivery: 'both' });
  assert.equal(state.view.value.settings.delivery, 'both', 'POST response is authoritative');
  assert.equal(permissionRequests, 1, 'a browser channel asks for permission from this click');

  await actions.setDelivery('os');
  assert.equal(permissionRequests, 1, 'the desktop-only channel does not prompt');
});

test('notification island preserves markup, dismisses outside/Escape, and cleans document listeners', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createNotifyState }, { NotifyApp, mountNotifyIsland }] = await Promise.all([
    harness.importDashboardModule('js/notify-state.js'),
    harness.importDashboardModule('js/notify-island.js'),
  ]);
  const state = createNotifyState({ snapshot: harness.signals.signal({ notifications_enabled: true }) });
  state.commitRequest(state.beginRequest(), enabledSettings);
  const calls = [];
  const actions = {
    toggle: () => { state.setOpen(!state.open.value); calls.push('toggle'); },
    close: () => { state.setOpen(false); calls.push('close'); },
    setEnabled: (value) => calls.push(['enabled', value]),
    setType: (type, value) => calls.push(['type', type, value]),
    setHumanMessages: (value) => calls.push(['human', value]),
    setAccessRequests: (value) => calls.push(['access', value]),
    setDelivery: (value) => calls.push(['delivery', value]),
    openConfig: () => calls.push('config'),
  };
  const mounted = await harness.mount(
    harness.html`<${NotifyApp} state=${state} actions=${actions} documentRef=${harness.document} />`,
  );
  const bell = mounted.container.querySelector('#notify-global');
  assert.equal(bell.id, 'notify-global');
  assert.equal(bell.getAttribute('aria-controls'), 'notify-pop');
  assert.equal(bell.getAttribute('aria-expanded'), 'false');
  assert.equal(bell.dataset.enabled, '1');
  assert.equal(bell.textContent.trim(), '🔔');
  assert.equal(bell.title, 'Notifications ON — click to choose which notifications you want');
  const pop = mounted.container.querySelector('#notify-pop');
  assert.equal(pop.getAttribute('role'), 'group');
  assert.equal(pop.getAttribute('aria-label'), 'Notification settings');
  assert.equal(pop.querySelectorAll('[data-notify-type]').length, 5);
  assert.equal(pop.querySelector('#notify-pop-human').hasAttribute('checked'), false);
  assert.equal(pop.querySelector('#notify-pop-access').hasAttribute('checked'), true);
  assert.match(pop.querySelector('.notify-pop-master').title, /master switch/);
  assert.match(pop.querySelector('#notify-pop-human').parentElement.title, /notify-human/);
  assert.match(pop.querySelector('#notify-pop-access').parentElement.title, /--ask-human/);
  assert.match(pop.querySelector('#notify-pop-config').title, /full notifications settings/);
  // Delivery selector reflects the committed setting and drives setDelivery.
  const delivery = pop.querySelector('#notify-pop-delivery');
  const selectedOption = [...delivery.querySelectorAll('option')].find((o) => o.selected);
  assert.equal(selectedOption?.value, 'both', 'the selector shows the committed channel');
  assert.equal(delivery.querySelectorAll('option').length, 3);
  const browserOption = [...delivery.querySelectorAll('option')].find((o) => o.value === 'browser');
  selectedOption.selected = false;
  browserOption.selected = true;
  await harness.act(() => harness.fireEvent(delivery, 'change'));
  assert.deepEqual(calls.at(-1), ['delivery', 'browser']);

  await harness.act(() => harness.fireEvent(bell, 'click'));
  assert.equal(pop.classList.contains('open'), true);
  assert.equal(bell.getAttribute('aria-expanded'), 'true');
  const master = pop.querySelector('#notify-pop-enabled');
  master.checked = false;
  await harness.act(() => harness.fireEvent(master, 'change'));
  assert.deepEqual(calls.at(-1), ['enabled', false]);
  const exited = pop.querySelector('[data-notify-type="exited"]');
  exited.checked = true;
  await harness.act(() => harness.fireEvent(exited, 'change'));
  assert.deepEqual(calls.at(-1), ['type', 'exited', true]);

  await harness.act(() => harness.fireEvent(harness.document.body, 'pointerdown'));
  assert.equal(state.open.value, false, 'outside pointer closes');
  await harness.act(() => state.setOpen(true));
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(state.open.value, false, 'Escape closes');

  await mounted.unmount();
  state.setOpen(true);
  harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  assert.equal(state.open.value, true, 'unmount removes the document key listener');
  harness.fireEvent(harness.document.body, 'pointerdown');
  assert.equal(state.open.value, true, 'unmount removes the document pointer listener');

  const host = harness.document.body.appendChild(harness.document.createElement('span'));
  const cleanups = [];
  mountNotifyIsland({
    host,
    state,
    actions,
    documentRef: harness.document,
    registerCleanup: (cleanup) => cleanups.push(cleanup),
  });
  assert.equal(cleanups.length, 1);
  assert.ok(host.querySelector('#notify-global'));
  await harness.act(() => cleanups[0]());
  assert.equal(host.childElementCount, 0, 'registered cleanup unmounts the island host');
  host.remove();
});
