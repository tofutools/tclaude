import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The predictive peek's projection (side-panel-predict.js), on its own: every
// rule that refuses, named by its reason, so a tuning change on main shows
// which rule it moved.

const EDGE = 968;
const line = (x0, x1, { y0 = 300, y1 = y0, n = 6, dt = 16, t0 = 0 } = {}) =>
  Array.from({ length: n }, (_, i) => ({ t: t0 + i * dt, x: x0 + ((x1 - x0) * i) / (n - 1), y: y0 + ((y1 - y0) * i) / (n - 1) }));

test('a level fling at the edge lands past it and peeks', async (t) => {
  const harness = await createPreactHarness(t);
  const { predictEdgeLanding } = await harness.importDashboardModule('js/side-panel-predict.js');
  const got = predictEdgeLanding(line(300, 750, { n: 5 }), EDGE);
  assert.equal(got.reason, 'ok');
  assert.equal(got.peek, true);
  assert.ok(got.landingX >= EDGE, `landing ${got.landingX}`);
});

test('each refusal names its rule', async (t) => {
  const harness = await createPreactHarness(t);
  const { predictEdgeLanding, PREDICT } = await harness.importDashboardModule('js/side-panel-predict.js');
  const cases = [
    ['few', line(300, 750, { n: 3 })],
    ['short', line(300, 750, { n: 5, dt: 5 })],
    ['slow', line(600, 650, { n: 5 })],
    ['unsteady', [{ t: 0, x: 300, y: 300 }, { t: 16, x: 500, y: 300 }, { t: 32, x: 502, y: 300 }, { t: 48, x: 700, y: 300 }, { t: 64, x: 760, y: 300 }]],
    ['diagonal', line(300, 700, { y0: 100, y1: 400, n: 5 })],
    ['near', line(800, 940, { n: 5, dt: 10 })],
    ['braking', [0, 16, 32, 48, 64].map((t, i) => ({ t, x: [300, 480, 590, 640, 660][i], y: 300 }))],
    ['short-of-edge', line(500, 620, { n: 5, dt: 20 })],
  ];
  for (const [reason, samples] of cases) {
    const got = predictEdgeLanding(samples, EDGE);
    assert.equal(got.peek, false, reason);
    assert.equal(got.reason, reason, `${reason}: got ${got.reason}`);
  }
  const off = predictEdgeLanding(line(300, 750, { y0: 300, y1: 340, n: 5 }), EDGE, { top: 100, bottom: 320 });
  assert.equal(off.reason, 'off-panel', 'landing below the panel is not heading for it');
  const handle = predictEdgeLanding(line(300, 750, { n: 5 }), EDGE, null, PREDICT, { top: 280, bottom: 320 });
  assert.equal(handle.reason, 'handle', 'a landing at the folded handle is aiming at the handle');
  assert.ok(Object.isFrozen(PREDICT), 'the constants are one frozen table');
});

test('the window keeps only recent moves', async (t) => {
  const harness = await createPreactHarness(t);
  const { recentSamples, PREDICT } = await harness.importDashboardModule('js/side-panel-predict.js');
  const kept = recentSamples(line(0, 100, { n: 10, dt: 20 }), 180);
  assert.ok(kept.every((sample) => 180 - sample.t <= PREDICT.WINDOW_MS));
  assert.ok(kept.length < 10);
});

test('the debug overlay draws the last prediction: the landing dot, green when it peeks, and the reason', async (t) => {
  const harness = await createPreactHarness(t);
  const { PredictDebugOverlay, predictDebug, predictDebugOn } = await harness.importDashboardModule('js/side-panel-predict-debug.js');
  const { predictEdgeLanding } = await harness.importDashboardModule('js/side-panel-predict.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.mount(harness.html`<${PredictDebugOverlay} />`, host);
  assert.equal(host.querySelector('.spp-overlay'), null, 'nothing before the first prediction');
  await harness.act(() => { predictDebug.value = { prediction: predictEdgeLanding(line(300, 750, { n: 5 }), EDGE), edgeX: EDGE }; });
  assert.ok(host.querySelector('.spp-overlay.peek .spp-dot'));
  assert.match(host.querySelector('.spp-label').textContent, /^ok /);
  await harness.act(() => { predictDebug.value = { prediction: predictEdgeLanding(line(300, 700, { y0: 100, y1: 400, n: 5 }), EDGE), edgeX: EDGE }; });
  assert.equal(host.querySelector('.spp-overlay.peek'), null);
  assert.match(host.querySelector('.spp-label').textContent, /^diagonal /);
  assert.equal(predictDebugOn({ location: { search: '?side_panel_predict_debug=1' } }), true);
  assert.equal(predictDebugOn({ location: { search: '' }, localStorage: { getItem: () => '1' } }), true);
  assert.equal(predictDebugOn({ location: { search: '' }, localStorage: { getItem: () => null } }), false, 'off by default');
});
