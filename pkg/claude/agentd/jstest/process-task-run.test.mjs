// js/process-task-run.js, groups-taskboard-process.js and the phone board's
// card: a process run for a board task (tk_8aa88514) as the task card shows
// it, the "will follow" line, and the opt-in gates.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';

// A review loop for task tk_1: draft done, review waiting on the reviewer in
// its second round, ship planned.
function run(overrides = {}) {
  return {
    runId: 'run_1', name: 'Review loop', status: 'running', startedAt: '2026-11-15T10:00:00Z',
    scope: { kind: 'group', group: 'webshop', task: 'tk_1', taskTitle: 'Checkout redesign' },
    nodes: [
      { id: 'draft', label: 'Draft', state: 'completed' },
      { id: 'review', label: 'Review', state: 'running', round: 2, rounds: 3, dispatch: { agentId: AGENT, name: '' } },
      { id: 'ship', label: 'Ship', state: 'planned' },
    ],
    edges: [{ from: 'draft', to: 'review' }, { from: 'review', to: 'ship' }],
    ...overrides,
  };
}

test('a task finds its run, a live one before a finished one, and says where it is', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/process-task-run.js');
  const old = run({ runId: 'run_0', status: 'failed', startedAt: '2026-11-15T11:00:00Z' });
  const context = { runs: { run_0: old, run_1: run(), run_2: run({ runId: 'run_2', scope: { kind: 'group', task: 'tk_2' } }) } };
  assert.equal(m.taskRunOf(context, 'tk_1').runId, 'run_1', 'the live run wins over a later finished one');
  assert.equal(m.taskRunOf(context, 'tk_9'), null);
  assert.equal(m.taskRunOf(null, 'tk_1'), null);

  const p = m.taskRunProgress(run(), (id) => (id === AGENT ? 'reviewer' : ''));
  assert.equal(p.name, 'Review loop');
  assert.equal(p.index, 2);
  assert.equal(p.total, 3);
  assert.equal(p.label, 'Review');
  assert.equal(p.round, 2);
  assert.equal(p.waitingOn, 'reviewer');
  assert.deepEqual(p.segments, ['done', 'running', 'planned']);
  const human = run({ nodes: [{ id: 'ok', label: 'Approve', state: 'awaiting', performer: { kind: 'human' } }], edges: [] });
  assert.equal(m.taskRunProgress(human).waitingOn, 'you');
});

test('the task card line opens its run, and a task to follow says so with change and no process', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/process-task-run.js');
  const { html } = harness;
  const opened = [];
  const line = await harness.mount(html`<${m.TaskRunLine} run=${run()} nameOf=${() => 'reviewer'} onOpen=${(r) => opened.push(r.runId)} />`);
  const button = line.container.querySelector('[data-task-run="run_1"]');
  assert.ok(button);
  assert.match(button.textContent, /Review loop · step 2\/3 Review \(round 2\) · waiting on reviewer/);
  assert.equal(line.container.querySelectorAll('.tk-seg').length, 3);
  assert.ok(line.container.querySelector('.tk-seg-running.tk-seg-current'), 'the current step is marked');
  await harness.act(() => button.click());
  assert.deepEqual(opened, ['run_1']);

  const done = await harness.mount(html`<${m.TaskRunLine} run=${run({ status: 'completed' })} />`);
  assert.match(done.container.textContent, /completed/);

  assert.equal(m.taskProcessFollowed({ process: '' }, 'review'), 'review', 'the board default');
  assert.equal(m.taskProcessFollowed({ process: 'other' }, 'review'), 'other', 'its own choice');
  assert.equal(m.taskProcessFollowed({ process: 'none' }, 'review'), '', 'opted out');
  assert.equal(m.taskProcessFollowed({}, ''), '', 'opt-in: nothing set, nothing followed');

  const picks = [];
  const follow = await harness.mount(html`<${m.WillFollowLine} task=${{ id: 'tk_1' }} boardDefault="review" nameOf=${() => 'Review loop'}
    onChange=${() => picks.push('change')} onNone=${() => picks.push('none')} />`);
  assert.match(follow.container.textContent, /will follow Review loop when started/);
  await harness.act(() => follow.container.querySelector('[data-task-follow-none]').click());
  await harness.act(() => follow.container.querySelector('[data-task-follow-change]').click());
  assert.deepEqual(picks, ['none', 'change']);
  const nothing = await harness.mount(html`<${m.WillFollowLine} task=${{ id: 'tk_1' }} />`);
  assert.equal(nothing.container.textContent.trim(), '');
});

test('a board offers Run with a process only where processes are used', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/groups-taskboard-process.js');
  const snapshot = { features: { processes: true } };
  const group = { name: 'webshop', members: [] };
  const used = { groups: [{ group: 'webshop', processes: [{ templateId: 'review', name: 'Review loop' }] }], runs: {} };
  const unused = { groups: [{ group: 'webshop', processes: [] }], runs: {} };
  const task = { id: 'tk_1', title: 'Checkout', status: 'open', stage: 'next' };

  const board = m.taskProcessBoard({ group, entry: {}, snapshot, actions: null, context: used });
  assert.ok(board);
  assert.equal(m.runWithProcessItem(board, task)?.attrs['data-task-menu'], 'run-process');
  assert.equal(m.runWithProcessItem(board, { ...task, status: 'done' }), null, 'a closed task runs nothing');
  assert.equal(m.runWithProcessItem(board, { ...task, kind: 'epic' }), null, 'an epic runs nothing');
  const busy = m.taskProcessBoard({ group, entry: {}, snapshot, actions: null, context: { ...used, runs: { run_1: run() } } });
  assert.equal(m.runWithProcessItem(busy, task), null, 'one live run per task');

  const plain = m.taskProcessBoard({ group, entry: {}, snapshot, actions: null, context: unused });
  assert.equal(m.runWithProcessItem(plain, task), null, 'a group that uses no process sees nothing new');
  const withDefault = m.taskProcessBoard({ group, entry: { process: 'review' }, snapshot, actions: null, context: unused });
  assert.ok(m.runWithProcessItem(withDefault, task), 'a board default counts as use');
  assert.equal(m.taskProcessBoard({ group: { ...group, fleet_node: { id: 'n2', local: false } }, entry: {}, snapshot, actions: null, context: used }), null,
    'a linked node\'s board has its own processes');
});

test('a task card shows its run or the process it will follow, and change and no process write the task', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/groups-taskboard-process.js');
  const { html } = harness;
  const snapshot = { features: { processes: true } };
  const writes = [];
  const actions = {
    api: { edit: async (id, change) => { writes.push([id, change]); return {}; } },
    run: async (_key, _label, fn) => { await fn(actions.api); return true; },
  };
  const context = { groups: [{ group: 'webshop', processes: [{ templateId: 'review', name: 'Review loop' }, { templateId: 'quick', name: 'Quick fix' }] }],
    runs: { run_1: run() } };
  const board = m.taskProcessBoard({ group: { name: 'webshop', members: [{ agent_id: AGENT, title: 'reviewer' }] }, entry: { process: 'review' }, snapshot, actions, context });
  const view = (task) => html`<${m.TaskProcess.Provider} value=${board}><${m.TaskProcessLine} task=${task} /><//>`;

  const running = await harness.mount(view({ id: 'tk_1', status: 'open', started_at: '2026-11-15T10:00:00Z' }));
  assert.match(running.container.textContent, /step 2\/3 Review \(round 2\) · waiting on reviewer/);

  const fresh = await harness.mount(view({ id: 'tk_2', title: 'New', status: 'open', stage: 'next' }));
  assert.match(fresh.container.textContent, /will follow Review loop when started/);
  await harness.act(() => fresh.container.querySelector('[data-task-follow-none]').click());
  assert.deepEqual(writes.pop(), ['tk_2', { process: 'none' }]);
  await harness.act(() => fresh.container.querySelector('.tk-follow-change').click());
  await harness.act(() => fresh.container.querySelector('[data-task-follow-pick="quick"]').click());
  assert.deepEqual(writes.pop(), ['tk_2', { process: 'quick' }]);

  const started = await harness.mount(view({ id: 'tk_3', status: 'open', started_at: '2026-11-15T10:00:00Z' }));
  assert.equal(started.container.textContent.trim(), '', 'a task under way without a run says nothing');
  const out = await harness.mount(view({ id: 'tk_4', status: 'open', process: 'none' }));
  assert.equal(out.container.textContent.trim(), '', 'opted out');
  const none = await harness.mount(html`<${m.TaskProcessLine} task=${{ id: 'tk_2', status: 'open' }} />`);
  assert.equal(none.container.textContent.trim(), '', 'no process side on the board, nothing shows');
});

test('the board options model carries the default way of working', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/groups-taskboard-options-model.js');
  assert.deepEqual(m.flatOptions({ process: 'review' }), { process: 'review' });
  assert.deepEqual(m.inheritedOf({}, 'process'), { value: '', source: 'default' }, 'unset by default');
  assert.deepEqual(m.patchOf({}, { process: 'review' }), { set: { process: 'review' }, reset: [] });
  assert.deepEqual(m.patchOf({ process: 'review' }, {}), { set: {}, reset: ['process'] });
  const chip = m.optionsSummary({ set: [] }, [], 'review').find((part) => part.section === 'process');
  assert.equal(chip?.text, '◈ follows review');
  assert.equal(m.optionsSummary({ set: [] }, []).some((part) => part.section === 'process'), false);
});
