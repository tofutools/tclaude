// FaTable and ChangeTable (js/fa-table.js) replaced hand-written Fleet admin
// tables. Each case renders the old hand-written form beside the shared one
// and requires the same markup.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function kit(t) {
  const harness = await createPreactHarness(t);
  const kitModule = await harness.importDashboardModule('js/fa-table.js');
  const markup = async (vnode) => (await harness.mount(vnode)).container.innerHTML;
  return { harness, ...kitModule, markup };
}

test('FaTable matches the hand-written fa-table shell, with and without an id', async (t) => {
  const { harness: { html }, FaTable, markup } = await kit(t);
  const rows = html`<tr><td>a</td><td></td></tr>`;
  assert.equal(await markup(html`<${FaTable} id="fleet-x" head=${['Peer', '']}>${rows}<//>`),
    await markup(html`<table class="fa-table ui-compact" id="fleet-x"><thead><tr><th>Peer</th><th></th></tr></thead><tbody>${rows}</tbody></table>`));
  assert.equal(await markup(html`<${FaTable} head=${['When']}><//>`),
    await markup(html`<table class="fa-table ui-compact"><thead><tr><th>When</th></tr></thead><tbody></tbody></table>`));
});

test('ChangeTable renders each change and toggles an item in and out of the skip list', async (t) => {
  const { harness, ChangeTable, markup } = await kit(t);
  const { html } = harness;
  const changes = [{ item: 'a', action: 'create' }, { item: 'b', action: 'replace', security: true }, { item: 'c', action: 'unchanged', security: true }];
  const old = html`<table class="fa-table ui-compact" id="fleet-offer-changes"><thead><tr><th></th><th>Item</th><th>Change</th><th></th></tr></thead><tbody>${changes.map((c) => html`<tr key=${c.item} data-item=${c.item}>
    <td><input type="checkbox" aria-label=${`include ${c.item}`} checked=${!['b'].includes(c.item)} disabled=${c.action === 'unchanged'} onChange=${() => {}} /></td>
    <td><code>${c.item}</code></td>
    <td class=${c.action === 'replace' ? 'fa-warn' : c.action === 'unchanged' ? 'muted' : ''}>${c.action === 'replace' ? 'overwrites yours' : c.action === 'create' ? 'new' : 'unchanged'}</td>
    <td>${c.security && c.action !== 'unchanged' ? html`<span class="ui-tag ui-tag-accent fa-badge fa-sensitive" title="decides what this node's agents may do">security</span>` : ''}</td>
  </tr>`)}</tbody></table>`;
  const skips = [];
  const now = html`<${ChangeTable} id="fleet-offer-changes" changes=${changes} skip=${['b']} onSkip=${(skip) => skips.push(skip)} />`;
  assert.equal(await markup(now), await markup(old));
  const { container } = await harness.mount(now);
  const boxes = container.querySelectorAll('input[type=checkbox]');
  await harness.act(() => { boxes[0].checked = false; boxes[0].dispatchEvent(new harness.window.Event('change', { bubbles: true })); });
  await harness.act(() => { boxes[1].checked = true; boxes[1].dispatchEvent(new harness.window.Event('change', { bubbles: true })); });
  assert.deepEqual(skips, [['b', 'a'], []]);
  const locked = await harness.mount(html`<${ChangeTable} changes=${changes} skip=${[]} onSkip=${() => {}} locked=${true} />`);
  assert.ok([...locked.container.querySelectorAll('input')].every((box) => box.disabled));
});
