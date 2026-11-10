// NodePill and StampAgo (js/cell-parts.js) and CopyCode (js/fa-table.js)
// replaced hand-written cells. Each case renders the old form beside the
// shared one and requires the same markup.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('NodePill matches the scope pills it replaced, local or not', async (t) => {
  const harness = await createPreactHarness(t);
  const { html } = harness;
  const { NodePill } = await harness.importDashboardModule('js/cell-parts.js');
  const markup = async (vnode) => (await harness.mount(vnode)).container.innerHTML;
  for (const node of [{ name: 'forge', color: '#f80', local: false }, { name: 'box', color: '#08f', local: true }]) {
    assert.equal(await markup(html`<${NodePill} node=${node} />`),
      await markup(html`<span class="scope-pill" style=${`--nc:${node.color}`}>${node.local ? '⌂ ' : ''}${node.name}</span>`));
    assert.equal(await markup(html`<${NodePill} node=${node} class="audit-node" />`),
      await markup(html`<span class="scope-pill audit-node" style=${`--nc:${node.color}`}>${node.local ? '⌂ ' : ''}${node.name}</span>`));
  }
});

test('StampAgo shows the stamp, then how long ago when the time is known', async (t) => {
  const harness = await createPreactHarness(t);
  const { html } = harness;
  const { StampAgo } = await harness.importDashboardModule('js/cell-parts.js');
  const { relTime } = await harness.importDashboardModule('js/helpers.js');
  const markup = async (vnode) => (await harness.mount(html`<td>${vnode}</td>`)).container.innerHTML;
  const iso = new Date(Date.now() - 120_000).toISOString();
  assert.equal(await markup(html`<${StampAgo} iso=${iso} text="stamp" />`),
    await markup(html`<span class="last-hook" title=${iso}>stamp</span>${relTime(iso) && html` <span class="muted">(${relTime(iso)})</span>`}`));
  assert.equal(await markup(html`<${StampAgo} iso=${undefined} title="" text="—" />`),
    await markup(html`<span class="last-hook" title="">—</span>`));
});

test('CopyCode matches the fingerprint and setup copy cells and toasts the copy', async (t) => {
  const harness = await createPreactHarness(t);
  const { html } = harness;
  const { CopyCode } = await harness.importDashboardModule('js/fa-table.js');
  const { Button } = await harness.importDashboardModule('js/ui-components.js');
  const markup = async (vnode) => (await harness.mount(vnode)).container.innerHTML;
  const toasts = [];
  const props = { value: 'ab12-cd34', what: 'Fingerprint', copy: async () => {}, toast: (text) => toasts.push(text) };
  assert.equal(await markup(html`<${CopyCode} class="fa-fp" ...${props} />`),
    await markup(html`<span class="fa-fp"><code>ab12-cd34</code> <${Button} size="sm" variant="link" class="fa-link" title="Copy the fingerprint">copy<//></span>`));
  const { container } = await harness.mount(html`<${CopyCode} class="fs-copy" ...${props} />`);
  await harness.act(async () => { container.querySelector('button').click(); await Promise.resolve(); });
  assert.deepEqual(toasts, ['Fingerprint copied']);
});
