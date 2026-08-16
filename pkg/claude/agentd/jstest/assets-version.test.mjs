// assets-version.js decides when an open dashboard must reload because agentd
// now serves a different embedded frontend than the modules running on the
// page. The baseline of record is the <meta name="tclaude-assets-version"> the
// daemon stamps into the served HTML; the first snapshot's fingerprint seeds
// it only when the tag is absent. A triggered reload latches so the 2s poll
// cannot request navigation more than once.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function watcher(t, metaContent) {
  const harness = await createPreactHarness(t);
  if (metaContent) {
    const meta = harness.document.createElement('meta');
    meta.setAttribute('name', 'tclaude-assets-version');
    meta.setAttribute('content', metaContent);
    harness.document.head.appendChild(meta);
  }
  const { checkAssetsVersion } = await harness.importDashboardModule('js/assets-version.js');
  const reloads = [];
  return {
    reloads,
    check: (version) => checkAssetsVersion(version, () => reloads.push(version)),
  };
}

test('meta baseline: matching snapshots never reload, a diverging one reloads once', async (t) => {
  const w = await watcher(t, 'aaaa000011112222');
  assert.equal(w.check('aaaa000011112222'), false);
  assert.equal(w.check('aaaa000011112222'), false);
  assert.deepEqual(w.reloads, []);
  // The daemon upgraded: a new fingerprint appears on the poll.
  assert.equal(w.check('bbbb000011112222'), true);
  // Later ticks racing the navigation bail without asking again.
  assert.equal(w.check('bbbb000011112222'), true);
  assert.equal(w.check('aaaa000011112222'), true);
  assert.deepEqual(w.reloads, ['bbbb000011112222']);
});

test('meta baseline catches an upgrade that landed before the first poll', async (t) => {
  // The page's HTML was served by the OLD build; the very first snapshot
  // already carries the new fingerprint — reload immediately, do not adopt it.
  const w = await watcher(t, 'aaaa000011112222');
  assert.equal(w.check('bbbb000011112222'), true);
  assert.deepEqual(w.reloads, ['bbbb000011112222']);
});

test('without a meta tag the first snapshot seeds the baseline', async (t) => {
  const w = await watcher(t, null);
  assert.equal(w.check('cccc000011112222'), false);
  assert.equal(w.check('cccc000011112222'), false);
  assert.deepEqual(w.reloads, []);
  assert.equal(w.check('dddd000011112222'), true);
  assert.deepEqual(w.reloads, ['dddd000011112222']);
});

test('an absent fingerprint (older daemon) never reloads and never seeds', async (t) => {
  const w = await watcher(t, null);
  assert.equal(w.check(''), false);
  assert.equal(w.check(undefined), false);
  // The first REAL fingerprint still becomes the baseline, not a mismatch.
  assert.equal(w.check('eeee000011112222'), false);
  assert.deepEqual(w.reloads, []);
});
