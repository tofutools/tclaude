// Tests for the Config tab on a peer's per-node page: the access block of
// /api/config (config_remote.go) makes a row the viewer may not change
// read-only with the reason, shows a withheld secret only as set or not set
// with Replace and Clear where its key is writable, tags the rail, and sends
// the secrets to clear with a save. On this node's own page nothing changes.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const KEYS = [
  { path: 'log_level', class: 'general' },
  { path: 'notifications.enabled', class: 'general' },
  { path: 'notifications.notification_command', class: 'local', secret: true },
  { path: 'agent.default_permissions', class: 'agents' },
  { path: 'agent.spawn_max_per_hour', class: 'agents' },
  { path: 'agent.dashboard_port', class: 'network' },
];
const SLUGS = { general: 'node.config.write', agents: 'node.config.agents', network: 'node.config.network', credentials: 'node.config.credentials', installation: '', local: '' };

function restricted(classes) {
  return { level: 'restricted', classes: { general: 'read', agents: 'read', network: 'read', credentials: 'read', installation: 'read', local: 'read', ...classes }, slugs: SLUGS, keys: KEYS };
}

async function mountConfig(t, response, { node = null } = {}) {
  const harness = await createPreactHarness(t);
  if (node) {
    globalThis.__tclaudeRemoteNode = { id: node.id };
    globalThis.document.documentElement.dataset.remoteNodeName = node.name;
    t.after(() => { delete globalThis.__tclaudeRemoteNode; });
  }
  const [{ createConfigState }, { ConfigApp }, adapter, { setConfigPanel }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
    harness.importDashboardModule('js/config-panel.js'),
  ]);
  setConfigPanel('notifications');
  const posts = [];
  const fetchImpl = async (path, init = {}) => {
    if (path.startsWith('/api/config') && init.method === 'POST') {
      posts.push({ path, body: JSON.parse(init.body) });
      return { ok: true, status: 200, json: async () => ({ ...response, raw: response.raw, secrets_changed: ['notifications.notification_command'] }) };
    }
    if (path.startsWith('/api/config')) return { ok: true, status: 200, json: async () => response };
    return { ok: false, status: 404, json: async () => ({}) };
  };
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const confirms = [];
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl, confirmDiff: async (...args) => { confirms.push(args); return true; },
  }} />`);
  await adapter.loadConfigTab();
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve)); });
  const row = (k) => mounted.container.querySelector(`[data-cfg-key="${k}"]`);
  return { harness, adapter, root: mounted.container, row, posts, confirms };
}

const locked = (fieldset) => fieldset.hasAttribute('disabled');

const RAW = JSON.stringify({ log_level: 'info', notifications: { enabled: true }, agent: { spawn_max_per_hour: 10 } }, null, 2);

test('a peer page makes the rows the viewer may not change read-only, with the reason', async (t) => {
  const { row, root } = await mountConfig(t, {
    raw: RAW, version: 'v1', access: restricted({ general: 'write' }),
    redacted: [{ path: 'notifications.notification_command', set: true }],
  }, { node: { id: 'inst_bobbobbobbob1234', name: 'bob-laptop' } });
  assert.equal(root.querySelector('#cfg-path').textContent, 'bob-laptop’s config (remote)', 'the header names the node it edits');
  assert.equal(row('log_level').dataset.cfgAccess, 'write');
  assert.equal(locked(row('log_level').querySelector('fieldset.cfg-row-ctl')), false);
  const perms = row('agent.default_permissions');
  assert.equal(perms.dataset.cfgAccess, 'read');
  assert.equal(locked(perms.querySelector('fieldset.cfg-row-ctl')), true);
  assert.match(perms.querySelector('.cfg-access-note').textContent, /Needs node\.config\.agents on this node/);
  const cmd = row('notifications.notification_command');
  assert.match(cmd.querySelector('.cfg-access-note').textContent, /Change on the node itself/);
  assert.equal(cmd.querySelector('.cfg-secret-state').textContent, 'set');
  assert.equal(cmd.querySelector('[data-cfg-secret-act]'), null, 'a secret the viewer may not change has no Replace or Clear');
  assert.equal(locked(cmd.querySelector('fieldset.cfg-row-ctl')), true);
  // The rail tags a panel the viewer can only partly change.
  const agentsLink = root.querySelector('[data-cfg-panel-link="agents"] .cfg-rail-access');
  assert.ok(agentsLink, 'the agents panel is tagged');
  assert.match(agentsLink.textContent, /read-only|partial/);
  assert.equal(root.querySelector('[data-cfg-panel-link="notifications"] .cfg-rail-access')?.textContent, 'partial');
});

test('a writable withheld secret can be replaced or cleared, and a save sends the clear', async (t) => {
  const { row, harness, adapter, posts, confirms } = await mountConfig(t, {
    raw: RAW, version: 'v1', access: restricted({ general: 'write', local: 'write' }),
    redacted: [{ path: 'notifications.notification_command', set: true }],
  }, { node: { id: 'inst_bobbobbobbob1234', name: 'bob-laptop' } });
  const cmd = row('notifications.notification_command');
  assert.equal(locked(cmd.querySelector('fieldset.cfg-row-ctl')), true, 'withheld until Replace');
  await harness.act(() => cmd.querySelector('[data-cfg-secret-act="replace"]').click());
  assert.equal(locked(row('notifications.notification_command').querySelector('fieldset.cfg-row-ctl')), false);
  assert.equal(row('notifications.notification_command').querySelector('.cfg-secret-state').textContent, 'replacing');
  await harness.act(() => row('notifications.notification_command').querySelector('[data-cfg-secret-act="keep"]').click());
  await harness.act(() => row('notifications.notification_command').querySelector('[data-cfg-secret-act="clear"]').click());
  assert.equal(row('notifications.notification_command').querySelector('.cfg-secret-state').textContent, 'will be cleared');
  await adapter.saveConfig();
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve)); });
  assert.ok(posts.length >= 1, 'a dry run is sent');
  assert.deepEqual(posts[0].body.clear, ['notifications.notification_command']);
  assert.equal(posts[0].body.base, RAW);
  assert.deepEqual(confirms[0]?.[4], ['notifications.notification_command'], 'the confirmation names the secret the save clears');
  assert.equal(confirms[0]?.[3], 'bob-laptop’s config (remote)', 'the confirmation names the node it writes to');
});

test('this node’s own page keeps every row as it was', async (t) => {
  const { row, root } = await mountConfig(t, {
    raw: RAW, version: 'v1',
    access: { level: 'local', classes: { general: 'write', agents: 'write', network: 'write', credentials: 'write', installation: 'write', local: 'write' }, slugs: SLUGS, keys: KEYS },
  });
  for (const k of ['log_level', 'agent.default_permissions', 'notifications.notification_command']) {
    assert.equal(row(k).dataset.cfgAccess, undefined, k);
    assert.equal(locked(row(k).querySelector('fieldset.cfg-row-ctl')), false, k);
    assert.equal(row(k).querySelector('.cfg-access-note'), null, k);
  }
  assert.equal(root.querySelector('.cfg-rail-access'), null);
  assert.equal(root.querySelector('.cfg-secret'), null);
});
