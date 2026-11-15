// js/context-meter.js: the desktop's context meter, shared with the phone,
// and the exact figure the phone's agent menu shows.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('contextFigure: the percentage, and the tokens over the window when reported', async (t) => {
  const harness = await createPreactHarness(t);
  const { contextFigure } = await harness.importDashboardModule('js/context-meter.js');
  assert.equal(contextFigure({ context_pct: 91.2, tokens_input: 180_000, tokens_output: 2_000, context_window_size: 200_000 }), 'ctx 91% · 182k / 200k');
  assert.equal(contextFigure({ context_pct: 50, tokens_input: 500_000, context_window_max: 1_000_000, context_window_size: 200_000 }), 'ctx 50% · 500k / 1M', 'a configured cap wins');
  assert.equal(contextFigure({ context_pct: 42 }), 'ctx 42%', 'no token counts: the percentage alone');
  assert.equal(contextFigure({ context_pct: 0 }), 'ctx 0%');
  assert.equal(contextFigure({}), '', 'not reported');
  assert.equal(contextFigure(undefined), '');
});

test('ContextMeter: five segments lit by band, its figure in the title, a wizard twin, dimmed when unknown', async (t) => {
  const harness = await createPreactHarness(t);
  const { ContextMeter } = await harness.importDashboardModule('js/context-meter.js');
  const { container } = await harness.mount(harness.html`<div><${ContextMeter} state=${{ context_pct: 91, tokens_input: 182_000, context_window_size: 200_000, context_window_source: 'reported' }} /></div>`);
  const [regular, mana] = container.querySelectorAll('.ctx-meter');
  assert.ok(regular.classList.contains('ctx-regular') && mana.classList.contains('ctx-mana'));
  assert.deepEqual([...regular.querySelectorAll('.ctx-seg')].map((s) => s.className), ['ctx-seg lit-green', 'ctx-seg lit-green', 'ctx-seg lit-yellow', 'ctx-seg lit-yellow', 'ctx-seg lit-red']);
  assert.equal(regular.getAttribute('title'), 'context: 182k / 200k tokens (reported cap) — 91%');
  assert.equal(regular.getAttribute('role'), 'img');
  assert.equal(regular.getAttribute('aria-label'), regular.getAttribute('title'), 'a screen reader reads its figure');
  assert.match(mana.getAttribute('title'), /^🔮 Mana: 182k \/ 200k channeled/);
  const unknown = await harness.mount(harness.html`<div><${ContextMeter} state=${{}} /></div>`);
  assert.ok(unknown.container.querySelector('.ctx-meter.ctx-unknown'));
  assert.equal(unknown.container.querySelector('.ctx-regular').getAttribute('title'), 'context window: usage not reported yet');
});
