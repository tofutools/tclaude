// TableHead (js/table-kit.js) replaced eight hand-built header rows. Each case
// renders the old hand-built form beside TableHead's and requires the same
// markup, so moving a table onto it cannot change its header.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function kit(t) {
  const harness = await createPreactHarness(t);
  const { TableHead } = await harness.importDashboardModule('js/table-kit.js');
  const { SortTh } = await harness.importDashboardModule('js/ui-components.js');
  const markup = async (head) => {
    const { container } = await harness.mount(harness.html`<table>${head}</table>`);
    return container.innerHTML;
  };
  return { harness, TableHead, SortTh, markup };
}

const COLS = [{ label: '' }, { label: 'ID', col: 'id' }, { label: 'Name', col: 'name' }, { label: '' }];

test('a delegated header matches the Groups view hand-built one', async (t) => {
  const { harness: { html }, TableHead, SortTh, markup } = await kit(t);
  const old = html`<thead><tr>${COLS.map((c) => (c.col
    ? html`<${SortTh} active=${c.col === 'name'} dir="desc" data-sort-table="retired" data-sort-col=${c.col} sortLabel=${c.label}>${c.label}<//>`
    : html`<th>${c.label || ''}</th>`))}</tr></thead>`;
  const now = html`<${TableHead} cols=${COLS} activeCol="name" dir="desc" sortTable="retired" />`;
  assert.equal(await markup(now), await markup(old));
});

test('an onSort header matches, sorts by the column key, and has no data-sort attributes', async (t) => {
  const { harness, TableHead, SortTh, markup } = await kit(t);
  const { html } = harness;
  const old = html`<thead><tr>${COLS.map((c) => (c.col
    ? html`<${SortTh} active=${false} dir=${undefined} sortLabel=${c.label} onSort=${() => {}}>${c.label}<//>`
    : html`<th>${c.label || ''}</th>`))}</tr></thead>`;
  const sorted = [];
  const now = html`<${TableHead} cols=${COLS} activeCol=${null} onSort=${(col) => sorted.push(col)} />`;
  assert.equal(await markup(now), await markup(old));
  const { container } = await harness.mount(html`<table>${now}</table>`);
  container.querySelectorAll('th.sortable')[1].click();
  assert.deepEqual(sorted, ['name']);
  assert.equal(container.querySelector('[data-sort-table], [data-sort-col]'), null);
});

test('custom keys, a lead cell, a thead class and a trailing cell render as before', async (t) => {
  const { harness: { html }, TableHead, SortTh, markup } = await kit(t);
  const cols = [{ key: 'ctl', label: '' }, { label: 'Cost', sort: 'cost' }, { label: 'Detail', sort: null }];
  const lead = html`<span class="lead">⋯</span>`;
  const old = html`<thead class="gf-lead-head"><tr>
    <th>${lead}</th>
    <${SortTh} active=${true} dir="asc" sortLabel="Cost" onSort=${() => {}}>Cost<//>
    <th>Detail</th>
    <th class="cost-ignore-col" aria-label="Ignore"></th></tr></thead>`;
  const now = html`<${TableHead} cols=${cols} activeCol="cost" dir="asc" lead=${lead} theadClass="gf-lead-head"
    isSortable=${(c) => !!c.sort} colOf=${(c) => c.sort} onSort=${() => {}}
    trailing=${html`<th class="cost-ignore-col" aria-label="Ignore"></th>`} />`;
  assert.equal(await markup(now), await markup(old));
});

test('nextSort cycles ascending, descending, then unsorted', async (t) => {
  const harness = await createPreactHarness(t);
  const { nextSort } = await harness.importDashboardModule('js/sort.js');
  assert.deepEqual(nextSort(null, 'a'), { col: 'a', dir: 'asc' });
  assert.deepEqual(nextSort({ col: 'a', dir: 'asc' }, 'a'), { col: 'a', dir: 'desc' });
  assert.equal(nextSort({ col: 'a', dir: 'desc' }, 'a'), null);
  assert.deepEqual(nextSort({ col: 'a', dir: 'desc' }, 'b'), { col: 'b', dir: 'asc' });
});

test('ColGroup renders one classed col per column, as the task board wrote by hand', async (t) => {
  const harness = await createPreactHarness(t);
  const { html } = harness;
  const { ColGroup } = await harness.importDashboardModule('js/table-kit.js');
  const keys = ['ctl', 'name', 'state', 'last', 'age', 'who', 'ref', 'deps'];
  const old = html`<colgroup>${keys.map((key) => html`<col key=${key} class=${`ui-col-fit tb-col-${key}`} />`)}<col class="ui-col-rest tb-col-files" /></colgroup>`;
  const cols = [...keys, 'files'].map((key) => ({ key }));
  const now = html`<${ColGroup} cols=${cols} colClass=${(c) => `${c.key === 'files' ? 'ui-col-rest' : 'ui-col-fit'} tb-col-${c.key}`} />`;
  const markup = async (vnode) => (await harness.mount(html`<table>${vnode}</table>`)).container.innerHTML;
  assert.equal(await markup(now), await markup(old));
});
