import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const prefs = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
const payload = (title = 'Alpha') => ({
  generated_at: '2026-07-12T00:00:00Z',
  permissions: { defaults: ['agent.send'], overrides: { a: { 'agent.spawn': 'grant' } } },
  slugs: [{ slug: 'agent.send', description: 'Send messages', owner_implied: true }],
  agents: [{ conv_id: 'a', agent_id: 'agt_alpha', title }],
  sudo: [{ id: 7, conv_id: 'a', agent_id: 'agt_alpha', conv_title: title, slug: 'agent.send', granted_at: '2026-07-11T23:00:00Z', expires_at: '2026-07-12T00:00:05Z' }],
});

test('Access island owns navigation, filtering, keyed rows, and local countdowns', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createAccessState }, { AccessApp }] = await Promise.all([
    harness.importDashboardModule('js/access-state.js'), harness.importDashboardModule('js/access-island.js'),
  ]);
  let now = Date.parse('2026-07-12T00:00:00Z');
  const snapshot = harness.signals.signal(payload());
  const state = createAccessState({ snapshot, prefs, now: () => now }); state.initialize(); state.setSubtab('sudo');
  const actions = { openGrant: () => {}, revoke: async () => true };
  const mounted = await harness.mount(harness.html`<${AccessApp} state=${state} actions=${actions} />`);
  const row = mounted.container.querySelector('tr[data-key="sudo-7"]');
  const countdown = row.querySelector('[data-sudo-countdown]');
  const filter = getByRole(mounted.container, 'textbox', { name: 'Filter active sudo grants' }); filter.focus();
  now += 1000; await harness.act(() => state.tick(now));
  assert.equal(mounted.container.querySelector('tr[data-key="sudo-7"]'), row);
  assert.equal(row.querySelector('[data-sudo-countdown]'), countdown);
  assert.equal(countdown.textContent, '4s');
  assert.equal(harness.document.activeElement, filter);
  await harness.input(filter, 'missing');
  assert.match(mounted.container.textContent, /0 \/ 1/);
  const slugs = mounted.container.querySelector('[data-subtab="slugs"]');
  await harness.act(() => harness.fireEvent(slugs, 'click', { button: 0 }));
  assert.equal(state.view.value.subtab, 'slugs');
  assert.match(mounted.container.textContent, /Send messages/);
  await mounted.unmount();
});

test('Access island makes partial snapshot failures explicit and production cleanup works', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createAccessState }, { AccessApp }] = await Promise.all([
    harness.importDashboardModule('js/access-state.js'), harness.importDashboardModule('js/access-island.js'),
  ]);
  const state = createAccessState({ snapshot: harness.signals.signal({ permissions: null }), prefs }); state.initialize();
  const mounted = await harness.mount(harness.html`<${AccessApp} state=${state} actions=${{ openGrant() {}, revoke() {} }} />`);
  assert.match(mounted.container.querySelector('#permissions-body [role="alert"]').textContent, /Permissions data is unavailable/);
  await mounted.unmount();
  const host = harness.document.body.appendChild(harness.document.createElement('div')); host.id = 'access-root';
  const { mountAccessFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const cleanup = await mountAccessFeature({ requestMutation: async () => {}, confirm: async () => true, notify: () => {}, openGrant: () => {} });
  assert.ok(host.querySelector('.access-subnav'));
  cleanup();
  assert.equal(host.childElementCount, 0);
});
