import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The one stage picker (groups-taskboard-stage.js) and what picking a
// custom column writes from the New task form, the edit form and ⋯
// Move to… / Reopen….

const COLUMNS = [
  { slug: 'in-review', label: 'In review' },
  { slug: 'in-ci', label: 'In CI' },
  { slug: 'parked', label: 'Parked', hidden: true },
  { slug: 'old', label: 'Old', disabled: true },
];

function recorder() {
  const calls = [];
  return new Proxy({ calls }, {
    get(target, name) {
      if (name in target) return target[name];
      return (...args) => { calls.push([name, ...args]); return Promise.resolve({ id: args[0] }); };
    },
  });
}

async function runAll(steps) {
  const api = recorder();
  for (const step of steps) await step.run(api);
  return api.calls;
}

test('the picker offers the board places, then the columns the board shows', async (t) => {
  const harness = await createPreactHarness(t);
  const { stageItems } = await harness.importDashboardModule('js/groups-taskboard-stage.js');
  const items = stageItems([{ key: 'idea', label: 'Idea' }, { key: 'next', label: 'Next' }], COLUMNS,
    { off: (key) => (key === 'next' ? 'it is here' : '') });
  assert.deepEqual(items.map((item) => item.key), ['idea', 'next', 'col:in-review', 'col:in-ci'], 'hidden and disabled columns are left out');
  assert.deepEqual(items.map((item) => item.group), ['Board', 'Board', 'Columns', 'Columns']);
  assert.equal(items[1].disabled, true);
  assert.equal(items[1].reason, 'it is here');
});

test('a new task starts in a custom column in its one add', async (t) => {
  const harness = await createPreactHarness(t);
  const { newTaskPlan } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  assert.deepEqual(newTaskPlan({ title: 'review it', start: 'col:in-review', to: 'agt_dev' }).task,
    { title: 'review it', body: '', ref: '', after: [], resolveRef: false, stage: 'in-review', status: '', to: 'agt_dev' });
  assert.equal(newTaskPlan({ title: 'tcl-a1b2c3', start: 'col:in-review' }).kind, 'add',
    'a tracker id starting in a column is an add with the id as its ref, not an import');
});

test('the edit form moves a task into a column, out of it, and reopens a closed one straight into it', async (t) => {
  const harness = await createPreactHarness(t);
  const { editPlan, draftOf, whereOf } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  const live = { id: 'tk_1', title: 'a', stage: 'next', status: 'open', assignee: { agent_id: 'agt_dev' } };

  assert.deepEqual(await runAll(editPlan(live, { ...draftOf(live), where: 'col:in-review' })), [['stage', 'tk_1', 'in-review']],
    'a started task moves in without being held first');

  const inColumn = { ...live, column: 'in-review' };
  assert.equal(whereOf(inColumn), 'col:in-review');
  assert.deepEqual(await runAll(editPlan(inColumn, draftOf(inColumn))), [], 'unchanged, nothing to write');
  assert.deepEqual(await runAll(editPlan(inColumn, { ...draftOf(inColumn), where: 'col:in-ci' })), [['stage', 'tk_1', 'in-ci']]);
  assert.deepEqual(await runAll(editPlan(inColumn, { ...draftOf(inColumn), where: 'progress' })), [['stage', 'tk_1', 'next']],
    'back to In progress takes it out of the column');

  const done = { ...live, status: 'done' };
  assert.deepEqual(await runAll(editPlan(done, { ...draftOf(done), where: 'col:in-ci' })), [['reopen', 'tk_1', { stage: 'in-ci' }]],
    'a reopen into a column is one write');
  assert.deepEqual(await runAll(editPlan(done, { ...draftOf(done), where: 'col:in-ci', to: '' })), [['reopen', 'tk_1', { stage: 'in-ci', to: '' }]]);
});

test('Move to… and Reopen… write one move, and the CLI line says it', async (t) => {
  const harness = await createPreactHarness(t);
  const { moveWrite, moveCommand } = await harness.importDashboardModule('js/groups-taskboard-stage.js');
  const task = { id: 'tk_1', stage: 'next', status: 'done', assignee: { agent_id: 'agt_dev' } };
  assert.deepEqual(await runAll([moveWrite(task, 'col:in-ci')]), [['stage', 'tk_1', 'in-ci']]);
  assert.deepEqual(await runAll([moveWrite(task, 'was', { reopen: true })]), [['reopen', 'tk_1', {}]]);
  assert.deepEqual(await runAll([moveWrite(task, 'col:in-ci', { reopen: true })]), [['reopen', 'tk_1', { stage: 'in-ci' }]]);
  assert.deepEqual(await runAll([moveWrite(task, 'idea', { reopen: true })]), [['reopen', 'tk_1', { stage: 'idea', to: '' }]],
    'an assigned task reopens as an idea without its assignee');
  assert.equal(moveCommand(task, 'col:in-ci', true), 'taskboard reopen tk_1 --stage in-ci');
  assert.equal(moveCommand(task, 'idea', true), 'taskboard reopen tk_1 --idea --unassign');
  assert.equal(moveCommand(task, 'col:in-ci', false), 'taskboard move tk_1 --stage in-ci');
});
