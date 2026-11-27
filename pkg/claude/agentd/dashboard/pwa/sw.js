// sw.js -- the dashboard's service worker: it receives Web Push messages
// (agentd push.go) and shows them as notifications, so a phone gets them with
// the app closed. Served at /sw.js, scope "/". It caches nothing: the
// dashboard is live data, and the page works without it.
//
// A push's payload (notify.PushPayload) is {v, tag, title?, body?, url, bn?}:
// no title means content-free mode, which shows "tclaude needs you". tag
// names the notification, so a newer one with the same tag replaces it
// rather than stacking (a question asker's collapsed pushes, a re-sent push).
// bn is the browser queue id when the same notification also went to the
// open-tab queue (browser-notify.js), whose banners use the same tag.

const CONTENT_FREE_TITLE = 'tclaude needs you';

// iOS shows every push: WebKit revokes a subscription whose pushes show
// nothing. Elsewhere a push is handed to a focused window instead.
const ALWAYS_SHOW = /iPhone|iPad|iPod/.test(self.navigator.userAgent);

self.addEventListener('install', () => { self.skipWaiting(); });
self.addEventListener('activate', (event) => { event.waitUntil(self.clients.claim()); });

function readPayload(event) {
  try {
    return event.data ? event.data.json() : {};
  } catch {
    return {};
  }
}

// notificationTag is the tag a notification is shown under.
function notificationTag(payload) {
  if (payload.bn) return `tclaude-bn-${payload.bn}`;
  return `tclaude-push-${payload.tag || 'x'}`;
}

self.addEventListener('push', (event) => {
  const payload = readPayload(event);
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    const focused = windows.filter((client) => client.focused && client.visibilityState === 'visible');
    if (focused.length > 0 && !ALWAYS_SHOW) {
      // The operator is looking at the app: it shows the alert in place
      // (a toast) instead of a second, system one.
      for (const client of focused) client.postMessage({ type: 'tclaude-push', payload });
      return;
    }
    await self.registration.showNotification(payload.title || CONTENT_FREE_TITLE, {
      body: payload.title ? (payload.body || '') : 'Open tclaude to see what is waiting.',
      tag: notificationTag(payload),
      icon: '/pwa/icon-192.png',
      badge: '/pwa/icon-192.png',
      data: { url: typeof payload.url === 'string' && payload.url.startsWith('/') ? payload.url : '/' },
    });
  })());
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = new URL(event.notification.data?.url || '/', self.location.origin).href;
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const client of windows) {
      if (new URL(client.url).origin !== self.location.origin) continue;
      await client.focus();
      if ('navigate' in client && client.url !== url) {
        try { await client.navigate(url); } catch { /* an uncontrolled window cannot be navigated; it is focused */ }
      }
      return;
    }
    await self.clients.openWindow(url);
  })());
});

// The browser renewed (or dropped) the subscription: subscribe again with
// the node's key and hand agentd the new one, so pushes keep arriving
// without the operator opening the app.
self.addEventListener('pushsubscriptionchange', (event) => {
  event.waitUntil((async () => {
    try {
      const keyResponse = await fetch('/api/push/key', { method: 'POST', credentials: 'same-origin' });
      if (!keyResponse.ok) return;
      const { public_key: key } = await keyResponse.json();
      const sub = await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
      const body = sub.toJSON();
      body.application_server_key = key;
      await fetch('/api/push/devices', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      });
    } catch { /* the app re-posts its subscription on its next open */ }
  })());
});
