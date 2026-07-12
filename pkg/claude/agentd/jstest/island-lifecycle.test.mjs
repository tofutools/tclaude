import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

test('declarative descriptor resolves named multi-hosts and preserves cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const { createIslandDescriptor, mountIslandDescriptor } =
    await harness.importDashboardModule('js/island-lifecycle.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div')); host.id = 'feature';
  const badge = harness.document.body.appendChild(harness.document.createElement('span')); badge.id = 'badge';
  let received;
  const descriptor = createIslandDescriptor({
    name: 'described', label: 'Described', hosts: { host: '#feature', badge: '#badge' },
    load: async (context) => ({
      state: {},
      mount(registerCleanup) { received = context; registerCleanup(() => {}); },
    }),
  });
  const cleanup = await mountIslandDescriptor(descriptor, { api: 'dependency' });
  assert.equal(received.hosts.host, host);
  assert.equal(received.hosts.badge, badge);
  assert.equal(received.dependencies.api, 'dependency');
  assert.equal(host.dataset.islandOwner, 'described');
  cleanup();
  assert.equal(host.dataset.islandOwner, undefined);
  assert.equal(badge.dataset.islandOwner, undefined);
});

test('descriptor skips absent hosts and isolates loader failures in its primary host', async (t) => {
  const harness = await createPreactHarness(t);
  const { createIslandDescriptor, mountIslandDescriptor } =
    await harness.importDashboardModule('js/island-lifecycle.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div')); host.id = 'primary';
  const absent = createIslandDescriptor({
    name: 'absent', label: 'Absent', hosts: { host: '#missing' },
    load: async () => { throw new Error('must not load'); },
  });
  assert.equal(await mountIslandDescriptor(absent), null);

  const broken = createIslandDescriptor({
    name: 'broken-descriptor', label: 'Broken descriptor', hosts: { host: '#primary' },
    load: async () => { throw new Error('optional chunk failed'); },
  });
  const mount = (options) => mountFeatureIsland({ ...options, logger: { error() {} } });
  const { mountFeatureIsland } = await harness.importDashboardModule('js/island-lifecycle.js');
  const ambientDocument = globalThis.document;
  delete globalThis.document;
  try {
    assert.equal(await mountIslandDescriptor(broken, {}, {
      documentRef: harness.document, mount,
    }), null);
  } finally {
    globalThis.document = ambientDocument;
  }
  assert.match(getByRole(host, 'alert').textContent, /optional chunk failed/);
});

test('island lifecycle claims hosts, registers state, and cleans up exactly once', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ mountFeatureIsland }, { featureState }] = await Promise.all([
    harness.importDashboardModule('js/island-lifecycle.js'),
    harness.importDashboardModule('js/feature-state-registry.js'),
  ]);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const badge = harness.document.body.appendChild(harness.document.createElement('span'));
  const state = { ready: true };
  let unmounts = 0;
  const cleanup = await mountFeatureIsland({
    name: 'test-feature', label: 'Test feature', hosts: [host, badge],
    load: async () => ({
      state,
      mount: (registerCleanup) => registerCleanup(() => { unmounts += 1; }),
    }),
  });

  assert.equal(host.dataset.islandOwner, 'test-feature');
  assert.equal(badge.dataset.islandOwner, 'test-feature');
  assert.equal(featureState('test-feature'), state);
  cleanup();
  cleanup();
  assert.equal(unmounts, 1);
  assert.equal(featureState('test-feature'), null);
  assert.equal(host.dataset.islandOwner, undefined);
  assert.equal(badge.dataset.islandOwner, undefined);
});

test('optional feature load failures stay inside the claimed host', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountFeatureIsland } = await harness.importDashboardModule('js/island-lifecycle.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const errors = [];
  const cleanup = await mountFeatureIsland({
    name: 'broken', label: 'Broken feature', hosts: [host],
    load: async () => { throw new Error('missing optional asset'); },
    logger: { error: (...args) => errors.push(args) },
  });

  assert.equal(cleanup, null);
  assert.match(getByRole(host, 'alert').textContent, /Broken feature failed to load: missing optional asset/);
  assert.equal(host.dataset.islandOwner, 'broken');
  assert.equal(errors.length, 1);
});

test('duplicate island ownership fails loudly without replacing the owner', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountFeatureIsland } = await harness.importDashboardModule('js/island-lifecycle.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.dataset.islandOwner = 'existing';
  await assert.rejects(
    mountFeatureIsland({
      name: 'other', label: 'Other', hosts: [host],
      load: async () => ({ mount: () => () => {} }),
    }),
    /already owned by existing/,
  );
  assert.equal(host.dataset.islandOwner, 'existing');
});

test('a feature that omits cleanup fails locally and unregisters its state', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ mountFeatureIsland }, { featureState }] = await Promise.all([
    harness.importDashboardModule('js/island-lifecycle.js'),
    harness.importDashboardModule('js/feature-state-registry.js'),
  ]);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const badge = harness.document.body.appendChild(harness.document.createElement('span'));
  badge.textContent = 'partial render';
  const cleanup = await mountFeatureIsland({
    name: 'no-cleanup', label: 'No cleanup', hosts: [host, badge],
    load: async () => ({ state: {}, mount: () => undefined }),
    logger: { error: () => {} },
  });
  assert.equal(cleanup, null);
  assert.equal(featureState('no-cleanup'), null);
  assert.equal(badge.childElementCount, 0);
  assert.equal(badge.textContent, '');
  assert.match(getByRole(host, 'alert').textContent, /must register cleanup/);
});

test('a partial mount rolls back each registered side effect', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ mountFeatureIsland }, { featureState }] = await Promise.all([
    harness.importDashboardModule('js/island-lifecycle.js'),
    harness.importDashboardModule('js/feature-state-registry.js'),
  ]);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  let liveSubscriptions = 0;
  const cleanup = await mountFeatureIsland({
    name: 'partial', label: 'Partial', hosts: [host],
    load: async () => ({
      state: {},
      mount: (registerCleanup) => {
        liveSubscriptions += 1;
        registerCleanup(() => { liveSubscriptions -= 1; });
        throw new Error('second host failed');
      },
    }),
    logger: { error: () => {} },
  });
  assert.equal(cleanup, null);
  assert.equal(liveSubscriptions, 0, 'first-host subscription was rolled back');
  assert.equal(featureState('partial'), null);
  assert.match(getByRole(host, 'alert').textContent, /second host failed/);
});

test('failed-mount rollback retries only transiently failing cleanup steps', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountFeatureIsland } = await harness.importDashboardModule('js/island-lifecycle.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  let liveSubscriptions = 1;
  let attempts = 0;
  const errors = [];
  const cleanup = await mountFeatureIsland({
    name: 'rollback-retry', label: 'Rollback retry', hosts: [host],
    load: async () => ({
      mount: (registerCleanup) => {
        registerCleanup(() => {
          attempts += 1;
          if (attempts === 1) throw new Error('transient cleanup failure');
          liveSubscriptions -= 1;
        });
        throw new Error('mount failed');
      },
    }),
    logger: { error: (...args) => errors.push(args) },
  });
  assert.equal(cleanup, null);
  assert.equal(attempts, 2);
  assert.equal(liveSubscriptions, 0);
  assert.equal(errors.length, 1, 'resolved rollback failure is not logged as permanent');
});

test('throwing cleanup attempts every disposer and retains ownership until retry succeeds', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ mountFeatureIsland }, { featureState }] = await Promise.all([
    harness.importDashboardModule('js/island-lifecycle.js'),
    harness.importDashboardModule('js/feature-state-registry.js'),
  ]);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const badge = harness.document.body.appendChild(harness.document.createElement('span'));
  const state = {};
  let shouldThrow = true;
  let badgeCleanups = 0;
  const cleanup = await mountFeatureIsland({
    name: 'throwing-cleanup', label: 'Throwing cleanup', hosts: [host, badge],
    load: async () => ({
      state,
      mount: (registerCleanup) => {
        registerCleanup(() => { if (shouldThrow) throw new Error('main unmount failed'); });
        registerCleanup(() => { badgeCleanups += 1; });
      },
    }),
  });

  assert.throws(cleanup, /island throwing-cleanup cleanup failed/);
  assert.equal(badgeCleanups, 1, 'later-host cleanup still ran');
  assert.equal(host.dataset.islandOwner, 'throwing-cleanup');
  assert.equal(badge.dataset.islandOwner, 'throwing-cleanup');
  assert.equal(featureState('throwing-cleanup'), null);

  shouldThrow = false;
  cleanup();
  assert.equal(badgeCleanups, 2, 'idempotent cleanup steps can be retried safely');
  assert.equal(host.dataset.islandOwner, undefined);
  assert.equal(badge.dataset.islandOwner, undefined);
});
