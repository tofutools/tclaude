import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('resource model: this browser\'s places win, then what the server says runs', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/resource-model.js');
  const places = m.browserPlaces({
    strip: ['page:pg_tab', 'groupterm:crew', 'agent:x', 'board:b_both'],
    overlays: [
      { kind: 'board', board: 'b_float', minimized: false },
      { kind: 'board', board: 'b_both', minimized: false },
      { id: 'process:tpl_min', kind: 'process', template: 'tpl_min', minimized: true },
      { kind: 'terminal', paneKey: 'groupterm:side', minimized: false },
      { kind: 'terminal', paneKey: 'term:ag_1:start:', minimized: true },
      { kind: 'terminal', paneKey: 'window:ag_1', minimized: false },
    ],
  });
  const state = (kind, id, server) => m.resourceState({ kind, id, state: server }, places);
  assert.equal(state('page', 'pg_tab', 'running'), 'open-tab');
  assert.equal(state('shell', 'crew', 'running'), 'open-tab');
  assert.equal(state('board', 'b_float', 'closed'), 'floating');
  assert.equal(state('board', 'b_both', 'closed'), 'floating', 'an overlay is in front of a strip tab');
  assert.equal(state('process', 'tpl_min', 'running'), 'minimized');
  assert.equal(state('shell', 'side', 'running'), 'floating');
  assert.equal(state('process', 'tpl_run', 'running'), 'running');
  assert.equal(state('page', 'pg_bg', 'running'), 'background');
  assert.equal(state('shell', 'other', 'running'), 'background');
  assert.equal(state('page', 'pg_shut', 'closed'), 'closed');
  assert.equal(state('board', 'b_rest', 'closed'), 'closed');
  assert.equal(state('shell', 'term:ag_1:start:', 'running'), 'minimized', "an agent's shell is a shell resource too");
  assert.equal(places.size, 7, 'only resource panes count');
});

test('resource model: a group\'s resources from the snapshot', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/resource-model.js');
  const snapshot = { group_resources: [
    { kind: 'page', id: 'pg_1', group_id: 1, title: 'App', state: 'running' },
    { kind: 'board', id: 'b_1', group_id: 2, title: 'Plan', state: 'closed' },
    { kind: 'page', id: 'pg_2', group_id: 1, title: 'Old', state: 'closed' },
  ] };
  const rows = m.groupResourcesOf(snapshot, 1, m.browserPlaces({ strip: ['page:pg_1'] }));
  assert.deepEqual(rows.map((r) => [r.id, r.view]), [['pg_1', 'open-tab'], ['pg_2', 'closed']]);
  assert.deepEqual(m.groupResourcesOf({}, 1), []);
});
