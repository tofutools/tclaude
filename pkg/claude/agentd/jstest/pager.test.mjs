import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('the shared pager pages by number or by offset range, and says where a walk is heading', async (t) => {
  const harness = await createPreactHarness(t);
  const { Pager } = await harness.importDashboardModule('js/pager.js');
  const pages = []; const actions = [];
  const mounted = await harness.mount(harness.html`<div>
    <${Pager} id="p" prefix="mail-pager" page=${2} pages=${5} stepping=${4} onPage=${(n) => pages.push(n)} sizes=${[25, 50]} pageSize=${50} />
    <${Pager} layout="range" total=${120} offset=${50} limit=${50} onAction=${(a) => actions.push(a)} sizes=${[25, 50]} sizeOption=${(n) => `${n}/page`} />
    <${Pager} layout="range" total=${10} offset=${0} limit=${50} sizes=${[50]} />
    <${Pager} layout="range" total=${120} offset=${0} limit=${50} delegated data=${{ 'data-list': 'retired', 'data-node': 'inst_forge' }} sizes=${[50]} />
  </div>`);
  const c = mounted.container;
  assert.match(c.querySelector('#p .mail-pager-pos').textContent, /Page 2 \/ 5 → 4…/);
  for (const title of ['First page', 'Previous page', 'Next page', 'Last page']) await harness.act(() => harness.fireEvent(c.querySelector(`#p button[title="${title}"]`), 'click'));
  assert.deepEqual(pages, [1, 1, 3, 5]);
  assert.deepEqual([...c.querySelectorAll('#p option')].map((o) => o.textContent), ['25', '50']);

  const ranges = c.querySelectorAll('.list-pager');
  assert.equal(ranges.length, 2, 'a range that fits on its first page shows no pager');
  assert.equal(ranges[0].querySelector('.list-pager-count').textContent, '51–100 of 120');
  await harness.act(() => harness.fireEvent(ranges[0].querySelector('button[title="Next page"]'), 'click'));
  assert.deepEqual(actions, ['next']);
  const next = ranges[1].querySelector('button[title="Next page"]');
  assert.equal(next.dataset.pager, 'next'); assert.equal(next.dataset.list, 'retired'); assert.equal(next.dataset.node, 'inst_forge');
  assert.equal(ranges[1].querySelector('select').dataset.pager, 'size');
  assert.ok(ranges[1].querySelector('button[title="First page"]').disabled);
  await mounted.unmount();
});
