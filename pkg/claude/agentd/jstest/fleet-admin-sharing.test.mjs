// tcl-pf0ekz: Fleet → Usage sharing lists both directions with spend bars,
// and acts on the shares this node gives (extend, raise, revoke).

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const now = Date.parse('2026-10-19T12:00:00Z');
const given = [
  { peer: 'inst_desk', peer_name: 'desk', gateway: 'team-key', dialect: 'anthropic', max_usd: 15, spent_usd: 12.6, since: '2026-10-18T12:00:00Z', expires_at: '2026-10-25T12:00:00Z', grant_expires_at: '2026-10-21T12:00:00Z', scope: 'http_proxy=team-key' },
  { peer: 'inst_pool', peer_name: 'pooled', gateway: 'team-key', dialect: 'anthropic', max_usd: 5, spent_usd: 1, since: '2026-10-18T12:00:00Z', grant_expires_at: '2026-10-21T12:00:00Z', pool: 'contractors' },
  { peer: 'inst_lab', peer_name: 'lab', gateway: 'team-key', dialect: 'anthropic', spent_usd: 0, unrestricted: true },
];
const received = [
  { peer: 'inst_forge', peer_name: 'forge', gateway: 'team-key', dialect: 'anthropic', max_usd: 20, spent_usd: 20, used_up: true, since: '2026-10-18T12:00:00Z' },
];

test('helpers: bars, ends and an extension counted from now once passed', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-admin-sharing.js');
  assert.equal(m.shareBar(given[0]), 84);
  assert.equal(m.shareBar(given[2]), null);
  assert.equal(m.endsText(given[0], now), 'ends in 6 days');
  assert.equal(m.endsText(given[2], now), 'no end');
  assert.equal(m.extendedExpiry(given[0], now), '2026-10-28T12:00:00.000Z');
  assert.equal(m.extendedExpiry({ grant_expires_at: '2026-10-01T00:00:00Z' }, now), '2026-10-26T12:00:00.000Z');
});

test('the page lists both directions and extends, raises and revokes a given share', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-admin-sharing.js');
  const calls = [];
  const actions = {
    modelSharesGiven: async () => ({ shares: given }),
    modelSharesReceived: async () => ({ shares: received, errors: { inst_old: 'timeout' } }),
    extend: async (b) => calls.push(['extend', b]),
    grant: async (b) => calls.push(['grant', b]),
    revoke: async (b) => calls.push(['revoke', b]),
  };
  const confirm = async (o) => { await o.action(); return true; };
  const toasts = [];
  const mounted = await harness.mount(harness.html`<${m.SharingPage} actions=${actions} confirm=${confirm} toast=${(msg) => toasts.push(msg)} now=${now} />`);
  await harness.act(() => new Promise((r) => setTimeout(r, 20)));
  const root = mounted.container;
  const recv = root.querySelector('#fleet-shares-received tr[data-share="inst_forge/team-key"]');
  assert.match(recv.textContent, /forge.*\$20\.00 \/ \$20\.00 · used up/);
  assert.match(root.textContent, /Could not ask: inst_old \(timeout\)/);
  const desk = root.querySelector('#fleet-shares-given tr[data-share="inst_desk/team-key"]');
  assert.equal(desk.querySelector('.fa-share-bar').getAttribute('aria-valuenow'), '84');
  assert.match(root.querySelector('#fleet-shares-given tr[data-share="inst_lab/team-key"]').textContent, /unrestricted trust/);
  assert.equal(root.querySelector('#fleet-shares-given tr[data-share="inst_lab/team-key"] button'), null, 'trust is not a grant to act on');

  const pooled = root.querySelector('#fleet-shares-given tr[data-share="inst_pool/team-key"]');
  assert.match(pooled.textContent, /from node group contractors/);
  assert.equal(pooled.querySelector('button'), null, 'a node group grant is changed as the group\'s');
  await harness.act(() => desk.querySelector('[data-fa="extend-share"]').click());
  const add = desk.querySelector('.fa-share-add');
  add.value = '5';
  await harness.act(() => harness.fireEvent(add, 'input'));
  await harness.act(() => desk.querySelector('[data-fa="raise-share"]').click());
  await harness.act(() => desk.querySelector('[data-fa="revoke-share"]').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 20)));
  const grant = { peer: 'inst_desk', slug: 'models.proxy', scope: 'http_proxy=team-key' };
  assert.deepEqual(calls, [
    ['extend', { ...grant, expires_at: '2026-10-28T12:00:00.000Z' }],
    ['grant', { ...grant, spawn_policy: { max_usd: 20 }, expires_at: '2026-10-21T12:00:00Z' }],
    ['revoke', grant],
  ]);
  await mounted.unmount();
});
