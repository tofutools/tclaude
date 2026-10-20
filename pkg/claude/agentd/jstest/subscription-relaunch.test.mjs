import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const fleet = { self: { id: 'inst_me', name: 'desk' }, peers: [{ id: 'inst_mum', name: 'mum-laptop' }, { id: 'inst_bob', name: 'bob' }] };
const snap = (sub) => ({ groups: [{ name: 'crew', members: [{ agent_id: 'agt_1', conv_id: 'c1', claude_subscription: sub }] }] });

async function setup(t) {
  const harness = await createPreactHarness(t);
  globalThis.__subConfirms = []; globalThis.__subToasts = [];
  await harness.replaceDashboardModule('js/refresh.js', `
    export async function confirmModal(o) { globalThis.__subConfirms.push(o); return o.action ? o.action() : true; }
    export function toast(m, e) { globalThis.__subToasts.push([m, !!e]); }
    export function refresh() {}
  `);
  const calls = [];
  const prev = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => { calls.push([String(url), init.method, init.body || '']); return { ok: true, status: 200, json: async () => ({}), text: async () => '{}' }; };
  harness.window.fetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = prev; });
  const [m, { fleetState }] = await Promise.all([
    harness.importDashboardModule('js/subscription-relaunch.js'),
    harness.importDashboardModule('js/fleet-state.js'),
  ]);
  fleetState.fleet.value = fleet;
  return { m, calls, confirms: globalThis.__subConfirms, toasts: globalThis.__subToasts };
}

test('a lost subscription token: its owner sends it again, anyone else switches the login', async (t) => {
  const { m, calls, confirms } = await setup(t);
  // Held, or no subscription at all: nothing to ask.
  assert.equal(await m.ensureClaudeSubscription({ agent: 'agt_1', label: 'helper', snapshot: snap({ peer: 'inst_me', held: true }) }), true);
  assert.equal(await m.ensureClaudeSubscription({ agent: 'agt_1', label: 'helper', snapshot: snap(undefined) }), true);
  assert.equal(confirms.length, 0);

  const mine = m.subscriptionChoice({ sub: { peer: 'inst_me' }, label: 'helper', nodeName: 'mum-laptop', self: 'inst_me', fleet });
  assert.equal(mine.method, 'POST');
  assert.match(mine.body, /runs on your Claude subscription, and mum-laptop no longer holds the token/);
  const theirs = m.subscriptionChoice({ sub: { peer: 'inst_bob' }, label: 'helper', nodeName: 'this node', self: 'inst_me', fleet });
  assert.equal(theirs.method, 'DELETE');
  assert.match(theirs.body, /runs on bob's Claude subscription.*only bob can send it again.*billed there/);

  assert.equal(await m.ensureClaudeSubscription({ agent: 'agt_1', label: 'helper', snapshot: snap({ peer: 'inst_me', held: false }), verb: 'restart it' }), true);
  assert.match(confirms.at(-1).title, /Send your Claude subscription to this node again/);
  assert.deepEqual(calls.at(-1), ['/api/agents/agt_1/claude-subscription', 'POST', '{"claude_subscription":true}'], 'the page sends only the choice; its node adds the token');

  // A refusal the snapshot does not show yet still asks.
  assert.equal(await m.ensureClaudeSubscription({ agent: 'agt_1', label: 'helper', snapshot: snap({ peer: 'inst_bob', held: true }), lost: true }), true);
  assert.equal(calls.at(-1)[1], 'DELETE');
});
