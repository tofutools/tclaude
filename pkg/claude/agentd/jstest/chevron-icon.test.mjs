import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('pagers, steps and back buttons draw the shared Chevron, never a text glyph', async (t) => {
  const harness = await createPreactHarness(t);
  const { Pager } = await harness.importDashboardModule('js/pager.js');
  const { Chevron } = await harness.importDashboardModule('js/chevron-icon.js');
  const mounted = await harness.mount(harness.html`<${Pager} page=${2} pages=${5} onPage=${() => {}} />`);
  const buttons = [...mounted.container.querySelectorAll('button')];
  assert.deepEqual(buttons.map((b) => b.getAttribute('aria-label')), ['First page', 'Previous page', 'Next page', 'Last page']);
  for (const b of buttons) {
    assert.ok(b.querySelector('svg.chevron-ico'), `${b.getAttribute('aria-label')} draws a chevron`);
    assert.equal(b.textContent.trim(), '', `${b.getAttribute('aria-label')} carries no glyph`);
  }
  const one = await harness.mount(harness.html`<${Chevron} dir="left" />`);
  const two = await harness.mount(harness.html`<${Chevron} dir="left" double=${true} />`);
  assert.notEqual(one.container.querySelector('path').getAttribute('d'), two.container.querySelector('path').getAttribute('d'));
});
