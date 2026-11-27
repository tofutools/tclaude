// push-client.js -- this browser's side of Web Push (agentd push.go): whether
// it can receive pushes, subscribing it as a device, keeping that
// subscription current, and the pushes the service worker (pwa/sw.js) hands
// a focused window instead of showing them.
//
// A subscription is the browser's: it is made here, with the node's VAPID
// key, and given to agentd, which sends to it from then on. Every call takes
// its window (win) and fetch so tests can stand in for both.

const SW_URL = '/sw.js';

// isIOS reports an iPhone or iPad browser (iPadOS reports a Mac, with touch).
function isIOS(win) {
  const nav = win?.navigator || {};
  return /iPhone|iPad|iPod/.test(nav.userAgent || '') || (nav.platform === 'MacIntel' && nav.maxTouchPoints > 1);
}

// isStandalone reports the app opened from the Home Screen.
function isStandalone(win) {
  try {
    if (win?.navigator?.standalone === true) return true;
    return !!win?.matchMedia?.('(display-mode: standalone)')?.matches;
  } catch {
    return false;
  }
}

// pushSupport says whether this browser can receive pushes and, when it
// cannot, why, in one line the settings show beside a disabled switch.
export function pushSupport(win = globalThis) {
  if (win?.isSecureContext !== true) {
    return { ok: false, reason: 'Needs HTTPS: open tclaude through remote access or a tunnel.' };
  }
  if (isIOS(win) && !isStandalone(win)) {
    return { ok: false, reason: 'On iPhone, add tclaude to the Home Screen first (Share › Add to Home Screen), then open it from there.' };
  }
  if (!win?.navigator?.serviceWorker || typeof win.PushManager !== 'function' || typeof win.Notification !== 'function') {
    return { ok: false, reason: 'This browser can’t receive push notifications.' };
  }
  if (win.Notification.permission === 'denied') {
    return { ok: false, reason: 'Notifications are blocked for this site: allow them in the browser’s settings.' };
  }
  return { ok: true, reason: '' };
}

// b64url encodes bytes as base64url without padding, as agentd keys are.
function b64url(buffer, win) {
  if (!buffer) return '';
  const bytes = new Uint8Array(buffer);
  let s = '';
  for (const b of bytes) s += String.fromCharCode(b);
  return win.btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// fromB64url decodes a base64url key into bytes, for applicationServerKey.
function fromB64url(text, win) {
  const padded = String(text).replace(/-/g, '+').replace(/_/g, '/');
  const bin = win.atob(padded + '='.repeat((4 - (padded.length % 4)) % 4));
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}

async function json(response) {
  if (!response.ok) {
    const text = await response.text().catch(() => '');
    const error = new Error(text.trim() || `HTTP ${response.status}`);
    error.status = response.status;
    throw error;
  }
  return response.json();
}

function postJSON(fetchImpl, path, body) {
  return fetchImpl(path, {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body ?? {}),
  });
}

// registration is the service worker's registration, registering it first,
// once its worker is active: a push manager refuses to subscribe while the
// worker is still installing (Chrome: "no active Service Worker").
async function registration(win) {
  const sw = win.navigator.serviceWorker;
  const existing = await sw.getRegistration(SW_URL);
  if (!existing) await sw.register(SW_URL, { scope: '/' });
  if (existing?.active) return existing;
  return sw.ready;
}

// currentSubscription is this browser's push subscription, or null.
export async function currentSubscription(win = globalThis) {
  if (!pushSupport(win).ok) return null;
  try {
    const reg = await win.navigator.serviceWorker.getRegistration(SW_URL);
    return reg ? await reg.pushManager.getSubscription() : null;
  } catch {
    return null;
  }
}

// subscriptionBody is what agentd takes for a subscription.
function subscriptionBody(sub, key, extra, win) {
  const raw = typeof sub.toJSON === 'function' ? sub.toJSON() : sub;
  const keyText = key || b64url(sub.options?.applicationServerKey, win);
  return { endpoint: raw.endpoint, keys: raw.keys, application_server_key: keyText, ...extra };
}

// subscribeThisDevice subscribes this browser and returns agentd's answer
// (the push state, with .device). It must run from the operator's tap: the
// permission prompt is asked first, before any network round trip, which
// iOS Safari and Firefox require.
export async function subscribeThisDevice({ fetchImpl = globalThis.fetch, win = globalThis } = {}) {
  const support = pushSupport(win);
  if (!support.ok) throw new Error(support.reason);
  const permission = await win.Notification.requestPermission();
  if (permission !== 'granted') throw new Error('Notifications were not allowed for this site.');
  const { public_key: key } = await json(await postJSON(fetchImpl, '/api/push/key'));
  const reg = await registration(win);
  let sub = await reg.pushManager.getSubscription();
  if (sub && b64url(sub.options?.applicationServerKey, win) !== key) {
    // Made for another key (before a reset-keys): it can never be pushed to.
    await sub.unsubscribe();
    sub = null;
  }
  if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: fromB64url(key, win) });
  return json(await postJSON(fetchImpl, '/api/push/devices', subscriptionBody(sub, key, {}, win)));
}

// refreshThisDevice re-posts this browser's subscription so agentd keeps its
// keys current, and returns this device's id, or 0 when this browser is not
// a subscribed device. Only a known device is refreshed: one the operator
// removed (from here or elsewhere) stays removed, and its subscription is
// dropped here too. A subscription for an old key is replaced.
//
// One refresh runs at a time: the boot and the settings both ask, and two
// key-change re-subscribes at once would race each other.
let refreshing = null;
export function refreshThisDevice(opts = {}) {
  if (!refreshing) refreshing = refreshOnce(opts).finally(() => { refreshing = null; });
  return refreshing;
}

async function refreshOnce({ fetchImpl = globalThis.fetch, win = globalThis } = {}) {
  const sub = await currentSubscription(win);
  if (!sub) return 0;
  const response = await postJSON(fetchImpl, '/api/push/devices', subscriptionBody(sub, '', { refresh: true }, win));
  if (response.status === 404) {
    await sub.unsubscribe().catch(() => {});
    return 0;
  }
  if (response.status === 409) {
    // A device agentd knows, subscribed for an old key: subscribe again
    // with the current one and move the device onto it, without asking
    // (permission is already granted).
    const oldEndpoint = sub.endpoint;
    const { public_key: key } = await json(await postJSON(fetchImpl, '/api/push/key'));
    await sub.unsubscribe().catch(() => {});
    const reg = await registration(win);
    const fresh = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: fromB64url(key, win) });
    const state = await json(await postJSON(fetchImpl, '/api/push/devices',
      subscriptionBody(fresh, key, { refresh: true, old_endpoint: oldEndpoint }, win)));
    return state?.device?.id || 0;
  }
  const state = await json(response);
  return state?.device?.id || 0;
}

// unsubscribeThisDevice removes this device at agentd and drops the
// browser's subscription.
export async function unsubscribeThisDevice(deviceId, { fetchImpl = globalThis.fetch, win = globalThis } = {}) {
  if (deviceId) {
    const response = await fetchImpl(`/api/push/devices/${encodeURIComponent(deviceId)}`, { method: 'DELETE', credentials: 'same-origin' });
    if (!response.ok && response.status !== 404) await json(response);
  }
  const sub = await currentSubscription(win);
  if (sub) await sub.unsubscribe().catch(() => {});
}

// startPushClient runs on every page load of this node's own dashboard:
// it keeps a subscribed device's subscription current, and hands pushes the
// service worker gives a focused window to onPush (a toast). It does
// nothing until the operator subscribed this browser. Returns a cleanup.
export function startPushClient({ fetchImpl = globalThis.fetch, win = globalThis, onPush = () => {} } = {}) {
  if (!pushSupport(win).ok || win.Notification?.permission !== 'granted') return () => {};
  const sw = win.navigator.serviceWorker;
  const onMessage = (event) => {
    if (event?.data?.type === 'tclaude-push') onPush(event.data.payload || {});
  };
  sw.addEventListener?.('message', onMessage);
  void refreshThisDevice({ fetchImpl, win }).catch(() => {});
  return () => sw.removeEventListener?.('message', onMessage);
}

// pushToastText is a foreground push as a toast line.
export function pushToastText(payload) {
  if (!payload?.title) return 'tclaude needs you';
  return payload.body ? `${payload.title}: ${payload.body}` : payload.title;
}
