// The service worker (dashboard/pwa/sw.js), run in a vm context with a fake
// `self`: when a push is shown and when it is handed to a focused window
// instead, and what it posts when the browser rotates the subscription.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const SW = await readFile(new URL('../dashboard/pwa/sw.js', import.meta.url), 'utf8');
const ORIGIN = 'https://box.example:7777';
const CHROME = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36';
const FIREFOX = 'Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0';
const IPAD_APP = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15';
const IOS_CHROME = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/140.0 Mobile/15E148 Safari/604.1';

// worker loads sw.js with the given user agent and open windows, and returns
// a way to fire its events and what it did.
function worker({ ua = CHROME, windows = [], fetchImpl } = {}) {
  const listeners = {};
  const shown = [];
  const posted = [];
  const fetches = [];
  const clients = windows.map((w) => ({
    url: ORIGIN + w.path, focused: w.focused ?? true, visibilityState: w.visible === false ? 'hidden' : 'visible',
    postMessage: (m) => posted.push([w.path, m]),
  }));
  const self = {
    navigator: { userAgent: ua },
    location: new URL(ORIGIN + '/sw.js'),
    addEventListener: (type, fn) => { listeners[type] = fn; },
    skipWaiting: () => {},
    clients: { matchAll: async () => clients, claim: async () => {}, openWindow: async () => {} },
    registration: {
      showNotification: async (title, opts) => { shown.push({ title, ...opts }); },
      pushManager: { subscribe: async () => { throw new Error('should use event.newSubscription'); } },
    },
  };
  const fetch = fetchImpl || (async (url, init) => {
    fetches.push([url, init?.body ? JSON.parse(init.body) : null]);
    if (url === '/api/push/key') return { ok: true, json: async () => ({ public_key: 'BKEY' }) };
    return { ok: true, json: async () => ({}) };
  });
  vm.runInNewContext(SW, { self, fetch, URL, console });
  const fire = async (type, event) => {
    let done;
    listeners[type]({ ...event, waitUntil: (p) => { done = p; } });
    await done;
  };
  const push = (payload) => fire('push', { data: { json: () => payload } });
  return { fire, push, shown, posted, fetches };
}

const PAYLOAD = { v: 1, tag: 'abc', title: 'tclaude: access', body: 'worker asks', url: '/access' };

test('a focused local dashboard gets the push as a toast, not a second notification', async () => {
  for (const ua of [CHROME, FIREFOX]) {
    const w = worker({ ua, windows: [{ path: '/groups' }] });
    await w.push(PAYLOAD);
    assert.equal(w.shown.length, 0, ua);
    assert.deepEqual(JSON.parse(JSON.stringify(w.posted)), [['/groups', { type: 'tclaude-push', payload: PAYLOAD }]]);
  }
});

test('a window that would drop the hand-over gets a system notification', async () => {
  for (const path of ['/groups?node=peer-1', '/terminals', '/terminals/abc', '/dashboard/login']) {
    const w = worker({ windows: [{ path }] });
    await w.push(PAYLOAD);
    assert.equal(w.posted.length, 0, `${path}: nothing handed over`);
    assert.equal(w.shown.length, 1, `${path}: the push is shown`);
  }
  const hidden = worker({ windows: [{ path: '/', visible: false }] });
  await hidden.push(PAYLOAD);
  assert.equal(hidden.shown.length, 1, 'a hidden window does not count');
});

test('WebKit always shows the push, even with the app focused', async () => {
  for (const ua of [IPAD_APP, IOS_CHROME]) {
    const w = worker({ ua, windows: [{ path: '/' }] });
    await w.push(PAYLOAD);
    assert.equal(w.shown.length, 1, ua);
    assert.equal(w.posted.length, 0, ua);
  }
});

test('the notification: its tag, content-free text and a same-origin deep link', async () => {
  const w = worker();
  await w.push({ ...PAYLOAD, bn: 42 });
  await w.push({ v: 1, tag: 'q1', url: 'https://evil.example/' });
  assert.equal(w.shown[0].tag, 'tclaude-bn-42', 'the open-tab banner tag, so one banner shows');
  assert.equal(w.shown[0].data.url, '/access');
  assert.equal(w.shown[1].title, 'tclaude needs you');
  assert.equal(w.shown[1].tag, 'tclaude-push-q1');
  assert.equal(w.shown[1].data.url, '/', 'a link off the origin is dropped');
});

test('a rotated subscription is posted as a refresh replacing the old endpoint', async () => {
  const w = worker();
  const newSubscription = { toJSON: () => ({ endpoint: 'https://fcm.googleapis.com/new', keys: { p256dh: 'p', auth: 'a' } }) };
  await w.fire('pushsubscriptionchange', { oldSubscription: { endpoint: 'https://fcm.googleapis.com/old' }, newSubscription });
  const [, body] = w.fetches.find(([url]) => url === '/api/push/devices');
  assert.equal(body.endpoint, 'https://fcm.googleapis.com/new');
  assert.equal(body.old_endpoint, 'https://fcm.googleapis.com/old');
  assert.equal(body.refresh, true, 'never re-adds a device the operator removed');
  assert.equal(body.application_server_key, 'BKEY');
});
