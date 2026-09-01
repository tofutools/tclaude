import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('group drag edge scrolling ramps toward both viewport edges', async (t) => {
  const harness = await createPreactHarness(t);
  const { edgeScrollVelocity } = await harness.importDashboardModule('js/groups-drag-autoscroll.js');

  assert.equal(edgeScrollVelocity(400, 800), 0);
  assert.equal(edgeScrollVelocity(112, 800), 0);
  assert.equal(edgeScrollVelocity(688, 800), 0);

  const nearTop = edgeScrollVelocity(80, 800);
  const atTop = edgeScrollVelocity(0, 800);
  assert.ok(nearTop < 0);
  assert.ok(atTop < nearTop);
  assert.equal(atTop, -800);

  const nearBottom = edgeScrollVelocity(720, 800);
  const atBottom = edgeScrollVelocity(800, 800);
  assert.ok(nearBottom > 0);
  assert.ok(atBottom > nearBottom);
  assert.equal(atBottom, 800);
});

test('group drag edge scrolling rejects unusable geometry', async (t) => {
  const harness = await createPreactHarness(t);
  const { edgeScrollVelocity } = await harness.importDashboardModule('js/groups-drag-autoscroll.js');

  assert.equal(edgeScrollVelocity(Number.NaN, 800), 0);
  assert.equal(edgeScrollVelocity(0, 0), 0);
});

test('a detached drag source still stops the edge-scroll frame', async (t) => {
  const harness = await createPreactHarness(t);
  const { bindGroupsDragAutoScroll } = await harness.importDashboardModule('js/groups-drag-autoscroll.js');
  const source = harness.document.body.appendChild(harness.document.createElement('div'));
  source.className = 'dnd-draggable';

  const cancelled = [];
  harness.window.requestAnimationFrame = () => 41;
  harness.window.cancelAnimationFrame = (id) => cancelled.push(id);
  Object.defineProperty(harness.window, 'innerHeight', { configurable: true, value: 800 });

  const cleanup = bindGroupsDragAutoScroll();
  harness.fireEvent(source, 'dragstart');
  harness.fireEvent(source, 'dragover', { clientY: 799 });
  source.remove();
  harness.fireEvent(source, 'dragend');

  assert.deepEqual(cancelled, [41],
    'source-local dragend cancels scrolling after reconciliation detaches the source');
  cleanup();
});
