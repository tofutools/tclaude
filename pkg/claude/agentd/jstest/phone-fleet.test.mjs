// js/phone-fleet.js: Fleet on a phone, read-only: this node and its hub, the
// trusted peers, those asking to be trusted; other Fleet admin sections are
// desktop only.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const STATUS = {
  instance_id: 'inst_3a9f0000', name: 'laptop-jk', enabled: true, hub_url: 'wss://hub.example.net/ws', hub: { state: 'connected' },
  peers: [
    { instance_id: 'inst_b', name: 'build-box', trusted: true, online: true, level: 'unrestricted' },
    { instance_id: 'inst_c', name: 'pi-lab', trusted: true, online: false, level: 'restricted', last_seen: '2026-07-12T06:00:00Z' },
    { instance_id: 'inst_d', name: 'studio-mac', trusted: false, online: true, trust_request: { note: 'full please', received_at: '2026-07-12T08:58:00Z' } },
    { instance_id: 'inst_e', name: 'stranger', trusted: false, online: true },
  ],
};

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [{ createFleetState }, { PhoneFleet }] = await Promise.all([
    harness.importDashboardModule('js/fleet-state.js'),
    harness.importDashboardModule('js/phone-fleet.js'),
  ]);
  const prefs = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
  const state = createFleetState({ activeTab: harness.signals.signal('fleet-admin'), prefs, search: '', pathname: '/fleet-admin' });
  t.after(() => state.dispose());
  const page = harness.signals.signal('');
  const mounted = await harness.mount(harness.html`<${PhoneFleet} state=${state} page=${page} />`);
  return { harness, state, page, c: mounted.container };
}

test('this node and its hub, trusted peers, and who asks to be trusted', async (t) => {
  const { harness, state, c } = await setup(t);
  assert.match(c.textContent, /Loading/);
  await harness.act(() => { state.setStatus(STATUS); state.markStatusLoaded(); });
  const self = c.querySelector('.phone-fleet-self');
  assert.match(self.textContent, /laptop-jk\s*this node/);
  assert.match(self.querySelector('.phone-fleet-hub').textContent, /hub connected · hub\.example\.net/);
  const section = (label) => c.querySelector(`[aria-label="${label}"]`);
  const names = (label) => [...section(label).querySelectorAll('.phone-fleet-row .phone-fleet-name')].map((n) => n.textContent);
  assert.deepEqual(names('Trusted peers'), ['build-box', 'pi-lab']);
  const [build, pi] = section('Trusted peers').querySelectorAll('.phone-fleet-row');
  assert.equal(build.querySelector('.phone-fleet-when').textContent, 'online');
  assert.equal(build.querySelector('.phone-fleet-sub').textContent, 'unrestricted');
  assert.notEqual(pi.querySelector('.phone-fleet-when').textContent, 'online');
  assert.deepEqual(names('Waiting to be trusted'), ['studio-mac'], 'a stranger that did not ask is not listed');
  assert.match(section('Waiting to be trusted').textContent, /“full please”/);
  assert.equal(c.querySelectorAll('button').length, 1, 'read-only: only the Desktop site link');
  assert.match(c.querySelector('.phone-desktop-only').textContent, /rest of Fleet admin are desktop only/);
});

test('another Fleet admin section is desktop only; no fleet says so', async (t) => {
  const { harness, state, page, c } = await setup(t);
  await harness.act(() => { page.value = 'grants'; });
  assert.match(c.querySelector('.phone-desktop-page').textContent, /Fleet admin is desktop only/);
  await harness.act(() => { page.value = 'peers'; state.clearFleet(); state.markStatusLoaded(); });
  assert.match(c.textContent, /not set up for a fleet/);
});
