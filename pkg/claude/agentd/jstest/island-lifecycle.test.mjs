import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

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
    load: async () => ({ state, mount: () => () => { unmounts += 1; } }),
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
  assert.match(getByRole(host, 'alert').textContent, /must return a cleanup function/);
});
