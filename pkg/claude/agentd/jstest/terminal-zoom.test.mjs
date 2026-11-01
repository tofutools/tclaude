// Tests for a terminal's zoom controls and the size kept for it (tcl-6j0smf).

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function memoryPrefs() {
  const map = new Map();
  return { map, getItem: (key) => map.get(key) ?? null, setItem: (key, value) => map.set(key, String(value)), removeItem: (key) => map.delete(key) };
}

async function setup(t) {
  const harness = await createPreactHarness(t);
  const zoom = await harness.importDashboardModule('js/terminal-zoom.js');
  return { harness, zoom };
}

test('the size is kept per terminal and per surface, and reset forgets it', async (t) => {
  const { harness, zoom } = await setup(t);
  const prefs = memoryPrefs();
  const reports = {};
  function Probe({ surface, terminal }) {
    const [font, onFontChange] = zoom.useTerminalFont(surface, terminal, prefs);
    reports[`${surface}/${terminal}`] = onFontChange;
    return harness.html`<i>${font.size}</i>`;
  }
  await harness.mount(harness.html`<div><${Probe} surface="tab" terminal="agt_a" /><${Probe} surface="companion" terminal="agt_a" /><${Probe} surface="tab" terminal="agt_b" /></div>`);

  await harness.act(() => reports['tab/agt_a']({ size: 16, zoomed: true }));
  await harness.act(() => reports['companion/agt_a']({ size: 10.5, zoomed: true }));
  assert.equal(zoom.readTerminalFont('tab', 'agt_a', prefs), 16);
  assert.equal(zoom.readTerminalFont('companion', 'agt_a', prefs), 10.5);
  assert.equal(zoom.readTerminalFont('tab', 'agt_b', prefs), null);

  await harness.act(() => reports['tab/agt_a']({ size: 13, zoomed: false }));
  assert.equal(zoom.readTerminalFont('tab', 'agt_a', prefs), null);
  assert.equal(zoom.readTerminalFont('companion', 'agt_a', prefs), 10.5, 'the other surface keeps its own');
});

test('a fit the terminal chose is not remembered as a zoom', async (t) => {
  const { harness, zoom } = await setup(t);
  const prefs = memoryPrefs();
  let report;
  function Probe() {
    const [, onFontChange] = zoom.useTerminalFont('tab', 'agt_a', prefs);
    report = onFontChange;
    return null;
  }
  await harness.mount(harness.html`<${Probe} />`);
  await harness.act(() => report({ size: 11.5, zoomed: false }));
  assert.equal(prefs.map.size, 0);
});

test('Ctrl+wheel anywhere on the tools panel reaches the terminal, and stops with the controls', async (t) => {
  const { harness, zoom } = await setup(t);
  const wheels = [];
  const widget = { zoomWheel: (event) => wheels.push(event.target) };
  const view = await harness.mount(harness.html`<div class="companion-floating">
    <button type="button" class="companion-mic">mic</button>
    <${zoom.TerminalZoom} font=${{ size: 15, zoomed: false }} widget=${() => widget} />
  </div>`);
  const panel = view.container.querySelector('.companion-floating');
  const mic = view.container.querySelector('.companion-mic');
  const size = view.container.querySelector('.term-zoom-size');

  for (const target of [mic, panel, size]) harness.fireEvent(target, 'wheel', { ctrlKey: true, deltaY: -120 });
  assert.deepEqual(wheels, [mic, panel, size], 'once each, wherever the pointer is');

  await view.unmount();
  harness.fireEvent(panel, 'wheel', { ctrlKey: true, deltaY: -120 });
  assert.equal(wheels.length, 3);
});

test('the controls step, show the size, reset, and leave the keyboard alone', async (t) => {
  const { harness, zoom } = await setup(t);
  const calls = [];
  const widget = { zoomBy: (step) => calls.push(step), resetZoom: () => calls.push('reset'), zoomWheel: () => calls.push('wheel') };
  const view = await harness.mount(harness.html`<${zoom.TerminalZoom} font=${{ size: 15, zoomed: true }} widget=${() => widget} />`);
  const button = (name) => view.container.querySelector(`[aria-label="${name}"]`);
  assert.equal(view.container.querySelector('.term-zoom-size').textContent, '15');

  await harness.act(() => button('Zoom terminal in').click());
  await harness.act(() => button('Zoom terminal out').click());
  await harness.act(() => button('Reset terminal zoom').click());
  assert.deepEqual(calls, [1, -1, 'reset']);

  const name = 'onmousedown' in button('Zoom terminal in') ? 'mousedown' : 'MouseDown';
  assert.ok(harness.fireEvent(button('Zoom terminal in'), name).defaultPrevented, 'the terminal keeps the focus');

  const unzoomed = await harness.mount(harness.html`<${zoom.TerminalZoom} font=${{ size: 8, zoomed: false }} widget=${() => widget} />`);
  assert.equal(unzoomed.container.querySelector('[aria-label="Reset terminal zoom"]').disabled, true);
  assert.equal(unzoomed.container.querySelector('[aria-label="Zoom terminal out"]').disabled, true);
});
