// Web Push in the dashboard (js/push-client.js, js/push-settings.js, the
// phone's More entry): when this browser can get pushes and why not,
// subscribing from the tap with the permission asked first, keeping a
// subscription current without reviving a removed device, and the
// settings' categories, content-free mode and devices.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const KEY = 'BAAA'; // agentd's public key, base64url
const keyBytes = () => Uint8Array.from(atob('BAAA'), (c) => c.charCodeAt(0)).buffer;

// fakeWindow is a browser with a service worker and a push manager.
function fakeWindow({ secure = true, ua = 'Mozilla/5.0 (X11; Linux) Chrome/140', standalone = false, permission = 'default', log = [] } = {}) {
  let subscription = null;
  let registered = false;
  const pushManager = {
    getSubscription: async () => subscription,
    subscribe: async (opts) => {
      // As Chrome and Safari: no subscribing while the worker installs.
      if (!registration.active) throw new Error('Subscription failed - no active Service Worker');
      log.push(['subscribe', btoa(String.fromCharCode(...opts.applicationServerKey))]);
      subscription = {
        endpoint: 'https://fcm.googleapis.com/fcm/send/abc',
        options: { applicationServerKey: keyBytes() },
        toJSON: () => ({ endpoint: 'https://fcm.googleapis.com/fcm/send/abc', keys: { p256dh: 'pk', auth: 'au' } }),
        unsubscribe: async () => { log.push(['unsubscribe']); subscription = null; return true; },
      };
      return subscription;
    },
  };
  // register() resolves while the worker is still installing; ready
  // resolves once it is active, a turn later.
  const registration = { pushManager, active: null };
  let activate;
  const ready = new Promise((resolve) => { activate = resolve; });
  const win = {
    isSecureContext: secure,
    navigator: {
      userAgent: ua, standalone, platform: 'Linux',
      serviceWorker: {
        getRegistration: async () => (registered ? registration : undefined),
        register: async (url, opts) => {
          registered = true;
          log.push(['register', url, opts.scope]);
          return registration;
        },
        // The worker activates once something waits for it: code that
        // subscribes without waiting always hits the installing worker.
        get ready() { registration.active = {}; activate(registration); return ready; },
        addEventListener: () => {}, removeEventListener: () => {},
      },
    },
    PushManager: function PushManager() {},
    Notification: Object.assign(function Notification() {}, {
      permission,
      requestPermission: async () => { log.push(['permission']); win.Notification.permission = 'granted'; return 'granted'; },
    }),
    matchMedia: () => ({ matches: standalone }),
    btoa: (s) => btoa(s), atob: (s) => atob(s),
  };
  return { win, log, setSubscription: (s) => { subscription = s; registered = true; registration.active = {}; activate(registration); } };
}

function pushState(over = {}) {
  return {
    enabled: true, content_free: false, public_key: KEY,
    category_order: ['access_requests', 'operator_questions', 'awaiting_permission', 'awaiting_input', 'human_messages', 'present_pr', 'idle', 'error', 'exited'],
    categories: { access_requests: true, operator_questions: true, awaiting_permission: true, awaiting_input: true },
    devices: [], ...over,
  };
}

// fakeAgentd answers the push routes and records every call.
// fakeAgentd answers /api/push* as agentd does: a refresh updates only a
// device it knows (by endpoint, or the old_endpoint it replaces), else 404.
function fakeAgentd(log, { state = pushState(), refresh = 0 } = {}) {
  let current = state;
  const known = new Set();
  return async (url, init = {}) => {
    const method = init.method || 'GET';
    const body = init.body ? JSON.parse(init.body) : null;
    log.push([method, url, body]);
    const ok = (json) => ({ ok: true, status: 200, json: async () => json, text: async () => '' });
    if (url === '/api/push' && method === 'GET') return ok(current);
    if (url === '/api/push/key') return ok({ public_key: KEY });
    if (url === '/api/push/devices' && body?.refresh) {
      const isKnown = known.has(body.endpoint) || known.has(body.old_endpoint);
      const status = refresh || (isKnown ? 200 : 404);
      if (status !== 200) return { ok: false, status, json: async () => ({}), text: async () => 'no' };
      known.delete(body.old_endpoint);
      known.add(body.endpoint);
      return ok({ ...current, device: { id: 7 } });
    }
    if (url === '/api/push/devices') {
      known.add(body.endpoint);
      current = { ...current, devices: [{ id: 7, label: 'Linux · Chrome', created_at: '2026-11-27T10:00:00Z' }] };
      return ok({ ...current, device: { id: 7 }, new: true });
    }
    if (url === '/api/push/config') {
      current = { ...current, categories: { ...current.categories, ...(body.categories || {}) }, content_free: body.content_free ?? current.content_free };
      return ok(current);
    }
    if (url.startsWith('/api/push/devices/') && method === 'DELETE') {
      known.clear();
      current = { ...current, devices: [] };
      return ok(current);
    }
    if (url === '/api/push/test') return ok({ queued: 1 });
    return { ok: false, status: 404, json: async () => ({}), text: async () => 'nope' };
  };
}

test('pushSupport says why this browser cannot get pushes, in one line', async (t) => {
  const harness = await createPreactHarness(t);
  const { pushSupport } = await harness.importDashboardModule('js/push-client.js');
  assert.match(pushSupport(fakeWindow({ secure: false }).win).reason, /Needs HTTPS/);
  assert.match(pushSupport(fakeWindow({ ua: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0) Safari/604.1' }).win).reason, /Home Screen/);
  assert.equal(pushSupport(fakeWindow({ ua: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0) Safari/604.1', standalone: true }).win).ok, true,
    'the Home Screen app can');
  assert.match(pushSupport(fakeWindow({ permission: 'denied' }).win).reason, /blocked/);
  const noPush = fakeWindow().win;
  delete noPush.PushManager;
  assert.match(pushSupport(noPush).reason, /can’t receive push/);
  assert.equal(pushSupport(fakeWindow().win).ok, true);
});

test('subscribing asks permission first, then subscribes with the node\'s key and hands agentd the subscription', async (t) => {
  const harness = await createPreactHarness(t);
  const { subscribeThisDevice } = await harness.importDashboardModule('js/push-client.js');
  const log = [];
  const { win } = fakeWindow({ log });
  const state = await subscribeThisDevice({ fetchImpl: fakeAgentd(log), win });
  assert.deepEqual(log[0], ['permission'], 'the prompt comes before any round trip, while the tap still counts');
  assert.deepEqual(log.find((e) => e[0] === 'register'), ['register', '/sw.js', '/']);
  assert.ok(log.some((e) => e[0] === 'subscribe'));
  const posted = log.find((e) => e[1] === '/api/push/devices');
  assert.equal(posted[2].endpoint, 'https://fcm.googleapis.com/fcm/send/abc');
  assert.deepEqual(posted[2].keys, { p256dh: 'pk', auth: 'au' });
  assert.equal(posted[2].application_server_key, KEY);
  assert.equal(state.device.id, 7);
});

test('an app open refreshes a subscribed device, and a device removed elsewhere stays removed', async (t) => {
  const harness = await createPreactHarness(t);
  const { refreshThisDevice, subscribeThisDevice } = await harness.importDashboardModule('js/push-client.js');
  const log = [];
  const { win } = fakeWindow({ log });
  assert.equal(await refreshThisDevice({ fetchImpl: fakeAgentd(log), win }), 0, 'nothing to refresh before subscribing');
  assert.equal(log.length, 0, 'no call without a subscription');
  await subscribeThisDevice({ fetchImpl: fakeAgentd(log), win });

  log.length = 0;
  assert.equal(await refreshThisDevice({ fetchImpl: fakeAgentd(log, { refresh: 200 }), win }), 7);
  assert.equal(log[0][2].refresh, true, 'a refresh never adds a device');

  log.length = 0;
  assert.equal(await refreshThisDevice({ fetchImpl: fakeAgentd(log, { refresh: 404 }), win }), 0);
  assert.ok(log.some((e) => e[0] === 'unsubscribe'), 'removed at agentd: the browser drops its subscription too');
});

test('a subscription for an old key is replaced on the next app open', async (t) => {
  const harness = await createPreactHarness(t);
  const { refreshThisDevice, subscribeThisDevice } = await harness.importDashboardModule('js/push-client.js');
  const log = [];
  const { win } = fakeWindow({ log });
  const agentd = fakeAgentd(log);
  await subscribeThisDevice({ fetchImpl: agentd, win });
  log.length = 0;
  let refreshed = false;
  const fetchImpl = async (url, init = {}) => {
    const body = init.body ? JSON.parse(init.body) : null;
    if (url === '/api/push/devices' && body?.refresh && !refreshed) {
      refreshed = true;
      return { ok: false, status: 409, json: async () => ({}), text: async () => 'another key' };
    }
    return agentd(url, init);
  };
  assert.equal(await refreshThisDevice({ fetchImpl, win }), 7);
  assert.deepEqual(log.filter((e) => e[0] === 'unsubscribe' || e[0] === 'subscribe').map((e) => e[0]), ['unsubscribe', 'subscribe']);
  assert.ok(!log.some((e) => e[0] === 'permission'), 'no prompt: permission is already granted');
  const repost = log.filter((e) => e[1] === '/api/push/devices').at(-1)[2];
  assert.equal(repost.refresh, true, 'the re-subscribe is a refresh: it moves the known device, never adds one');
  assert.equal(repost.old_endpoint, 'https://fcm.googleapis.com/fcm/send/abc');
});

test('the boot and the settings refreshing at once make one call', async (t) => {
  const harness = await createPreactHarness(t);
  const { refreshThisDevice, subscribeThisDevice } = await harness.importDashboardModule('js/push-client.js');
  const log = [];
  const { win } = fakeWindow({ log });
  const agentd = fakeAgentd(log);
  await subscribeThisDevice({ fetchImpl: agentd, win });
  log.length = 0;
  const [a, b] = await Promise.all([refreshThisDevice({ fetchImpl: agentd, win }), refreshThisDevice({ fetchImpl: agentd, win })]);
  assert.deepEqual([a, b], [7, 7]);
  assert.equal(log.filter((e) => e[1] === '/api/push/devices').length, 1);
});

test('the settings: this device, the categories, content-free and the devices', async (t) => {
  const harness = await createPreactHarness(t);
  const { PushSettings } = await harness.importDashboardModule('js/push-settings.js');
  const log = [];
  const { win } = fakeWindow({ log });
  const toasts = [];
  const fetchImpl = fakeAgentd(log);
  const { container } = await harness.mount(harness.html`<${PushSettings} fetchImpl=${fetchImpl} win=${win} notify=${(m, e) => toasts.push([m, !!e])} />`);
  const settled = async (ok) => { for (let i = 0; i < 50 && !ok(); i++) await harness.act(async () => {}); };
  await settled(() => container.querySelector('.push-settings-cats'));

  const cats = [...container.querySelectorAll('[data-push-category]')];
  assert.deepEqual(cats.map((c) => c.getAttribute('data-push-category')).slice(0, 4), ['access_requests', 'operator_questions', 'awaiting_permission', 'awaiting_input']);
  assert.deepEqual(cats.map((c) => c.hasAttribute('checked')), [true, true, true, true, false, false, false, false, false]);
  assert.match(container.textContent, /an agent asks me something/);
  assert.match(container.textContent, /No device gets pushes yet/);

  const idle = container.querySelector('[data-push-category="idle"]');
  idle.checked = true;
  await harness.act(async () => { idle.dispatchEvent(new Event('change')); });
  await settled(() => log.some((e) => e[1] === '/api/push/config'));
  assert.deepEqual(log.find((e) => e[1] === '/api/push/config')[2], { categories: { idle: true } });

  const sw = container.querySelector('.push-settings-switch input');
  assert.equal(sw.hasAttribute('checked'), false);
  await harness.act(async () => { sw.dispatchEvent(new Event('change')); });
  await settled(() => container.querySelector('.push-settings-dev'));
  assert.match(container.querySelector('.push-settings-dev').textContent, /Linux · Chrome.*this device/);
  assert.match(container.querySelector('.push-settings-dev').textContent, /no push yet/);
  assert.equal(toasts.at(-1)[0], 'This device now gets push notifications.');

  const remove = [...container.querySelectorAll('.push-settings-dev button')].find((b) => b.textContent === 'Remove');
  await harness.act(async () => { remove.click(); });
  await settled(() => !container.querySelector('.push-settings-dev'));
  assert.ok(log.some((e) => e[0] === 'DELETE' && e[1] === '/api/push/devices/7'));
  assert.ok(log.some((e) => e[0] === 'unsubscribe'), 'removing this device drops its subscription');
});

test('where push cannot work the switch is off with the reason, and the master switch is offered', async (t) => {
  const harness = await createPreactHarness(t);
  const { PushSettings } = await harness.importDashboardModule('js/push-settings.js');
  const log = [];
  const { win } = fakeWindow({ secure: false, log });
  const { container } = await harness.mount(harness.html`<${PushSettings} fetchImpl=${fakeAgentd(log, { state: pushState({ enabled: false }) })} win=${win} />`);
  for (let i = 0; i < 50 && !container.querySelector('.push-settings-cats'); i++) await harness.act(async () => {});
  assert.ok(container.querySelector('.push-settings-switch input').hasAttribute('disabled'));
  assert.match(container.querySelector('.push-settings-why').textContent, /Needs HTTPS/);
  assert.match(container.querySelector('.push-settings-warn').textContent, /Notifications are off for this node/);
});

test('More\'s Phone push entry is this node\'s only, and a foreground push reads as a toast', async (t) => {
  const harness = await createPreactHarness(t);
  const { PushEntry } = await harness.importDashboardModule('js/phone-more.js');
  const opened = [];
  const local = await harness.mount(harness.html`<ul><${PushEntry} remote=${false} open=${(sheet) => opened.push(sheet.label)} /></ul>`);
  await harness.act(async () => { local.container.querySelector('button').click(); });
  assert.deepEqual(opened, ['Phone push']);
  const peer = await harness.mount(harness.html`<ul><${PushEntry} remote=${true} /></ul>`);
  assert.equal(peer.container.querySelector('button'), null);

  const { pushToastText } = await harness.importDashboardModule('js/push-client.js');
  assert.equal(pushToastText({ title: 'tclaude: access request', body: 'worker asks for git.push' }), 'tclaude: access request: worker asks for git.push');
  assert.equal(pushToastText({ tag: 'x' }), 'tclaude needs you');
});
