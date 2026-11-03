// The Config tab's remote-access certificate panel (JOH-278) is a Preact
// island: it loads /api/remote-access/info when the Config tab opens and
// posts its forms, showing the server's error in its own banner.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = () => new Promise((resolve) => setTimeout(resolve, 0));

function stubFetch(t, info) {
  const calls = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), method: init.method || 'GET', body: init.body ? JSON.parse(init.body) : null });
    if (String(url) === '/api/remote-access/add-hosts' && calls.at(-1).body.hosts === 'bad') {
      return { ok: false, status: 400, text: async () => 'invalid host name\n' };
    }
    return { ok: true, status: 200, json: async () => (String(url) === '/api/remote-access/info' ? info : {}) };
  };
  t.after(() => { globalThis.fetch = previous; });
  return calls;
}

test('the remote-access panel loads when Config opens and lists SANs and devices', async (t) => {
  const harness = await createPreactHarness(t);
  const calls = stubFetch(t, {
    material_exists: true, running: true, running_bind: '0.0.0.0:8443',
    sans: ['myhost.ts.net', '10.0.0.2'],
    clients: [{ name: 'phone', has_p12: true, not_after: '2030-01-02T00:00:00Z' }, { name: 'old laptop', has_p12: false }],
  });
  const { RemoteAdminPanel } = await harness.importDashboardModule('js/remote-admin.js');
  const activeTab = harness.signals.signal('groups');
  const { container, unmount } = await harness.mount(harness.html`<${RemoteAdminPanel} activeTab=${activeTab} />`);
  assert.equal(calls.length, 0, 'nothing loads until Config opens');
  await harness.act(async () => { activeTab.value = 'config'; await wait(); });
  assert.deepEqual(calls.map((c) => c.url), ['/api/remote-access/info']);
  assert.match(container.querySelector('#ra-admin-state').textContent, /Listener live on https:\/\/0\.0\.0\.0:8443/);
  assert.deepEqual([...container.querySelectorAll('.ra-chip')].map((c) => c.textContent), ['myhost.ts.net', '10.0.0.2']);
  const devices = [...container.querySelectorAll('.ra-device')];
  assert.equal(devices.length, 2);
  assert.equal(devices[0].querySelector('a[download]').getAttribute('href'), '/api/remote-access/client?name=phone');
  assert.match(devices[1].textContent, /\.p12 removed/);
  assert.equal(container.querySelector('#ra-ca-btn').getAttribute('href'), '/api/remote-access/ca.crt');
  assert.equal(container.querySelector('#ra-setup-details').hasAttribute('open'), false, 'setup stays folded once material exists');
  await unmount();
});

test('the remote-access panel posts host names and shows the server error', async (t) => {
  const harness = await createPreactHarness(t);
  const calls = stubFetch(t, { material_exists: false, sans: [], clients: [] });
  const { RemoteAdminPanel } = await harness.importDashboardModule('js/remote-admin.js');
  const { container, unmount } = await harness.mount(harness.html`<${RemoteAdminPanel} activeTab=${harness.signals.signal('config')} />`);
  await harness.act(() => wait());
  assert.equal(container.querySelector('#ra-setup-details').hasAttribute('open'), true, 'no material opens first-time setup');
  assert.match(container.querySelector('#ra-admin-devices').textContent, /No devices issued yet/);

  await harness.act(() => harness.fireEvent(container.querySelector('#ra-addhosts-btn'), 'click'));
  assert.equal(container.querySelector('#ra-admin-msg').textContent, 'Enter one or more host names.');
  assert.equal(calls.length, 1, 'an empty form posts nothing');

  await harness.input(container.querySelector('#ra-addhosts-input'), 'bad');
  await harness.act(async () => { harness.fireEvent(container.querySelector('#ra-addhosts-btn'), 'click'); await wait(); });
  assert.equal(container.querySelector('#ra-admin-msg').textContent, 'Add host names failed: invalid host name');
  assert.ok(container.querySelector('#ra-admin-msg').classList.contains('err'));

  await harness.input(container.querySelector('#ra-addhosts-input'), ' myhost.ts.net ');
  await harness.act(async () => { harness.fireEvent(container.querySelector('#ra-addhosts-btn'), 'click'); await wait(); await wait(); });
  assert.deepEqual(calls.at(-2), { url: '/api/remote-access/add-hosts', method: 'POST', body: { hosts: 'myhost.ts.net' } });
  assert.equal(calls.at(-1).url, '/api/remote-access/info', 'a successful post reloads the panel');
  assert.equal(container.querySelector('#ra-admin-msg'), null);
  assert.equal(container.querySelector('#ra-addhosts-input').value, '');
  await unmount();
});
