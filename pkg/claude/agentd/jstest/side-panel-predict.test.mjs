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
    ['short-travel', line(880, 940, { n: 5, dt: 10 })],
    ['braking', [0, 16, 32, 48, 64].map((t, i) => ({ t, x: [300, 480, 590, 640, 660][i], y: 300 }))],
    ['short-of-edge', line(500, 620, { n: 5, dt: 20 })],
  ];
  for (const [reason, samples] of cases) {
    const got = predictEdgeLanding(samples, EDGE);
    assert.equal(got.peek, false, reason);
    assert.equal(got.reason, reason, `${reason}: got ${got.reason}`);
  }
  const off = predictEdgeLanding(line(300, 750, { y0: 300, y1: 340, n: 5 }), EDGE, { top: 100, bottom: 320 });
  assert.equal(off.reason, 'off-panel', 'a pointer below the panel now is not heading for it');
  const across = predictEdgeLanding(line(300, 750, { y0: 100, y1: 300, n: 5 }), EDGE, { top: 200, bottom: 400 });
  assert.equal(across.reason, 'ok', 'only x is judged: a diagonal fling ending within the panel’s height peeks');
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
  await harness.act(() => { predictDebug.value = { prediction: predictEdgeLanding(line(600, 650, { n: 5 }), EDGE), edgeX: EDGE }; });
  assert.equal(host.querySelector('.spp-overlay.peek'), null);
  assert.match(host.querySelector('.spp-label').textContent, /^slow /);
  assert.equal(predictDebugOn({ location: { search: '?side_panel_predict_debug=1' } }), true);
  assert.equal(predictDebugOn({ location: { search: '' }, localStorage: { getItem: () => '1' } }), true);
  assert.equal(predictDebugOn({ location: { search: '' }, localStorage: { getItem: () => null } }), false, 'off by default');
});

test('v2 helpers: the crossing must not brake at all, the peek grows towards the panel, and a stop near it reveals', async (t) => {
  const harness = await createPreactHarness(t);
  const { crossingHolds, peekWidthFor, stopReveals, PREDICT } = await harness.importDashboardModule('js/side-panel-predict.js');
  const ok = { peek: true, vx: 2 };
  assert.equal(crossingHolds({ ...ok, px: 2.1 }), true, 'no braking');
  assert.equal(crossingHolds({ ...ok, px: 2 * PREDICT.CROSS_BRAKE_RATIO }), true, 'at the ratio');
  assert.equal(crossingHolds({ ...ok, px: 1.6 }), false, 'braking a little (0.8), which the peek itself allows');
  assert.ok(1.6 >= 2 * PREDICT.BRAKE_RATIO, 'that braking still peeks');
  assert.equal(crossingHolds({ peek: false, vx: 2, px: 2 }), false, 'only while the prediction holds');

  const g = { startX: 500, fullLeft: 721, edgeX: 953, panelW: 264 };
  assert.equal(peekWidthFor({ ...g, x: 500 }), PREDICT.PEEK_MIN_PX, 'starts at PEEK_MIN_PX');
  assert.equal(peekWidthFor({ ...g, x: 721 }), 264, 'the panel’s width at its edge');
  assert.equal(peekWidthFor({ ...g, x: 900 }), 264, 'and no wider');
  assert.equal(peekWidthFor({ ...g, x: 600, prev: 200 }), 200, 'never shrinks while the prediction holds');
  assert.equal(peekWidthFor({ ...g, startX: 800, x: 876 }), 155, 'from inside the panel’s area it grows towards the edge zone');

  assert.equal(stopReveals(721, 721, 900), true, 'inside the full panel’s area');
  assert.equal(stopReveals(700, 721, 700 + PREDICT.STOP_MARGIN_PX), true, 'within the margin of the peek');
  assert.equal(stopReveals(700, 721, 701 + PREDICT.STOP_MARGIN_PX), false, 'beyond it');
});

test('the speed needed falls with the distance the motion started from, to a floor; no band refuses outright', async (t) => {
  const harness = await createPreactHarness(t);
  const { predictEdgeLanding, minSpeedFor, PREDICT } = await harness.importDashboardModule('js/side-panel-predict.js');
  const samples = line(560, 700, { n: 6, dt: 32 }); // 0.875 px/ms
  const near = predictEdgeLanding(samples, EDGE, null, PREDICT, { x: 560, fullLeft: 721 });
  const far = predictEdgeLanding(samples, EDGE, null, PREDICT, { x: 100, fullLeft: 721 });
  assert.equal(near.reason, 'ok', `from 161px out (${near.minVx})`);
  assert.equal(far.reason, 'slow', `from 621px out (${far.minVx})`);
  assert.equal(far.minVx, PREDICT.MIN_VX);
  assert.equal(minSpeedFor(0), PREDICT.MIN_VX_FLOOR, 'the floor at no distance');
  assert.equal(minSpeedFor(-50), PREDICT.MIN_VX_FLOOR, 'and from past the panel’s edge');
  const fromPanel = predictEdgeLanding(line(640, 760, { n: 5, dt: 16 }), EDGE, null, PREDICT, { x: 640, fullLeft: 721 });
  assert.equal(fromPanel.reason, 'ok', 'a fast move from 81px left of the panel’s edge is judged (no near band)');
  const twitch = predictEdgeLanding(line(900, 950, { n: 5, dt: 10 }), EDGE, null, PREDICT, { x: 900, fullLeft: 721 });
  assert.equal(twitch.reason, 'short-travel', 'a twitch by the edge travels too little');
});

test('a snap reveals on arrival when it travelled far at its start’s fling speed', async (t) => {
  const harness = await createPreactHarness(t);
  const { arrivalReveals, PREDICT } = await harness.importDashboardModule('js/side-panel-predict.js');
  const fullLeft = 721;
  assert.equal(arrivalReveals({ startX: 60, startT: 0, x: 990, t: 48, fullLeft }), true, 'three frames from the left');
  assert.equal(arrivalReveals({ startX: 60, startT: 0, x: 990, t: 1000, fullLeft }), false, 'the same distance in a second is slow');
  assert.equal(arrivalReveals({ startX: 850, startT: 0, x: 990, t: 16, fullLeft }), false, 'too short a travel, however fast');
  assert.equal(arrivalReveals({ startX: 990 - PREDICT.ARRIVAL_MIN_TRAVEL_PX, startT: 0, x: 990, t: 100, fullLeft }), true,
    'the minimum travel, at the floor speed its start needs');
});
