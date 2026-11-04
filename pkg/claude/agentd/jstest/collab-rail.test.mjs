import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = () => new Promise((resolve) => setTimeout(resolve, 0));

const SNAPSHOT = {
  groups: [{ id: 1, name: 'tclaude' }, { id: 2, name: 'home-lab' }],
  group_resources: [
    { kind: 'board', id: 'launch', group_id: 1, title: 'Launch-resolution map', state: 'closed' },
    { kind: 'page', id: 'pg_dash', group_id: 1, title: 'Dashboard preview', state: 'running' },
    { kind: 'process', id: 'pr-review', group_id: 1, title: 'pr-review', state: 'running' },
    { kind: 'shell', id: 'tclaude', group_id: 1, title: 'Group shell', state: 'running', session: 's' },
    { kind: 'board', id: 'release', group_id: 1, title: 'Release planning', state: 'closed' },
    { kind: 'board', id: 'net', group_id: 2, title: 'Network diagram', state: 'closed' },
    { kind: 'page', id: 'pg_status', group_id: 2, title: 'Status page', state: 'closed' },
  ],
};

test('rail model: resources by group in group order, then terminals, each in a bucket', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/collab-rail-model.js');
  const model = await harness.importDashboardModule('js/resource-model.js');
  const places = model.browserPlaces({
    strip: ['board:launch', 'agent:x'],
    overlays: [{ kind: 'page', page: 'pg_dash', minimized: true }, { kind: 'board', board: 'net', minimized: true }],
  });
  const all = m.railItems({
    resources: SNAPSHOT.group_resources, groups: SNAPSHOT.groups, places,
    panes: [{ key: 'board:launch', seed: { board: 'launch' }, label: 'Launch' }, { key: 'agent:x', seed: { agent: 'x' }, label: 'launch-uniform' }],
    overlays: [{ id: 'term:y', kind: 'terminal', paneKey: 'agent:y', label: 'relaunch-table', minimized: false }],
    background: [{ kind: 'agent', key: 'agent:live', tmux_session: 'tc-live', label: 'live-page' }, { kind: 'group_shell', key: 'groupterm:tclaude', tmux_session: 'g' },
      { kind: 'shell', key: 'agent:y', tmux_session: 'held', label: 'held by a window' },
      { kind: 'agent', tmux_session: 'old-gen', label: 'superseded' }],
  });
  assert.deepEqual(all.sections.map((s) => s.name), ['tclaude', 'home-lab']);
  const buckets = Object.fromEntries(all.sections.flatMap((s) => s.items).map((i) => [i.key, i.bucket]));
  assert.deepEqual(buckets, {
    'board:launch': 'open', 'page:pg_dash': 'minimized', 'process:pr-review': 'background', 'shell:tclaude': 'background',
    'board:release': 'closed', 'board:net': 'minimized', 'page:pg_status': 'closed',
  });
  assert.deepEqual(all.terminals.map((i) => [i.title, i.bucket]), [['launch-uniform', 'open'], ['relaunch-table', 'open'], ['live-page', 'background']],
    'a strip agent pane, a floating terminal, a background session; a group shell is the group\'s, not a terminal');

  assert.ok(!all.terminals.some((i) => i.title === 'held by a window'), 'a session an overlay window holds is listed once');
  assert.ok(!all.terminals.some((i) => i.title === 'superseded'), 'a session that cannot be attached is not offered');

  // Open here but in no group's list: it still shows, under Not in a group.
  const loose = m.railItems({
    resources: SNAPSHOT.group_resources, groups: SNAPSHOT.groups,
    panes: [{ key: 'board:scratch', seed: { board: 'scratch' }, label: 'Scratch board' }],
    overlays: [{ kind: 'page', page: 'pg_private', label: 'Private page', minimized: true }],
  });
  const other = loose.sections.find((s) => s.id === 'other');
  assert.deepEqual(other.items.map((i) => [i.key, i.bucket]), [['board:scratch', 'open'], ['page:pg_private', 'minimized']]);

  const open = m.filterRail(all, { bucket: 'open' });
  assert.deepEqual(open.counts, { all: 10, open: 3, minimized: 2, background: 3, closed: 2 });
  assert.deepEqual(open.sections.map((s) => s.name), ['tclaude'], 'a group with nothing open drops out');
  const found = m.filterRail(all, { query: 'lab' });
  assert.deepEqual(found.sections.map((s) => s.name), ['home-lab'], 'search matches the group name too');
  assert.equal(found.counts.all, 2);
});

test('rail: lists, filters, opens an item, folds to icons', async (t) => {
  const harness = await createPreactHarness(t);
  const [rail, places] = await Promise.all([
    harness.importDashboardModule('js/collab-rail.js'),
    harness.importDashboardModule('js/resource-places.js'),
  ]);
  await harness.act(() => { places.stripPaneKeys.value = ['board:launch']; rail.setRailCollapsed(false); });
  const activated = [];
  const actions = { activatePane: (key) => activated.push(key), closePane: () => {}, popOutPane: () => {} };
  const { container } = await harness.mount(harness.html`<${rail.CollabRail}
    panes=${[{ key: 'board:launch', seed: { board: 'launch' }, label: 'Launch' }]} actions=${actions} snapshot=${{ value: SNAPSHOT }} />`);
  const root = container.querySelector('[data-collab-rail]');
  assert.deepEqual([...root.querySelectorAll('[data-rail-group]')].map((el) => el.dataset.railGroup), ['tclaude', 'home-lab']);
  assert.equal(root.querySelector('[data-rail-filter="open"]').textContent, 'Open 1');
  assert.equal(root.querySelector('[data-rail-item="page:pg_dash"] .cr-mark').textContent, 'live', 'a page out of sight is live');
  assert.equal(root.querySelector('[data-rail-item="shell:tclaude"] .cr-mark').textContent, 'bg');

  // A strip tab comes forward on click, and offers pop out and close.
  const launch = root.querySelector('[data-rail-item="board:launch"]');
  assert.equal(launch.querySelectorAll('.cr-tool').length, 2);
  await harness.act(async () => { harness.fireEvent(launch.querySelector('.cr-name'), 'click'); await wait(); });
  assert.deepEqual(activated, ['board:launch']);

  await harness.act(() => harness.fireEvent(root.querySelector('[data-rail-filter="closed"]'), 'click'));
  assert.deepEqual([...root.querySelectorAll('[data-rail-item]')].map((el) => el.dataset.railItem), ['board:release', 'board:net', 'page:pg_status']);
  const search = root.querySelector('.cr-search');
  await harness.act(() => { search.value = 'status'; harness.fireEvent(search, 'input'); });
  assert.deepEqual([...root.querySelectorAll('[data-rail-item]')].map((el) => el.dataset.railItem), ['page:pg_status']);

  await harness.act(() => harness.fireEvent(root.querySelector('[data-rail-fold]'), 'click'));
  const folded = container.querySelector('[data-collab-rail]');
  assert.ok(folded.classList.contains('collapsed'));
  assert.equal(folded.querySelector('.cr-search'), null);
  assert.equal(folded.querySelectorAll('[data-rail-item]').length, 7, 'folded, every item shows as its icon');
  assert.equal(rail.railCollapsed.value, true);
});
