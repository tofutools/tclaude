// A task's refs in the forms (tk_c447158c, tk_39630a0f): the New task form
// takes several, and ↑ makes one the first, which is the task's ref for
// --ref and the List.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('the New task form sends its refs: the first as ref, the rest as add_refs', async (t) => {
  const harness = await createPreactHarness(t);
  const { newTaskPlan } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  const plan = newTaskPlan({ title: 'Port it', refs: ['LIN-1', 'https://github.com/o/r/pull/7'], newRef: 'tcl-a1b2c3', start: 'idea' });
  assert.equal(plan.kind, 'add');
  assert.equal(plan.task.ref, 'LIN-1');
  assert.deepEqual(plan.task.addRefs, ['https://github.com/o/r/pull/7', 'tcl-a1b2c3'], 'the add box counts as added');
  assert.equal(plan.task.resolveRef, true);
  const none = newTaskPlan({ title: 'Port it', refs: [], newRef: '', start: 'idea' });
  assert.equal(none.task.ref, '');
  assert.equal(none.task.addRefs, undefined);
});

test('an edit names a new first ref only when the order the daemon keeps would differ', async (t) => {
  const harness = await createPreactHarness(t);
  const { refsPlan } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  const task = { refs: [{ kind: 'linear', label: 'LIN-1' }, { kind: 'awb', label: 'tcl-a1b2c3' }] };
  const keys = (await harness.importDashboardModule('js/groups-taskboard-card.js')).refsOf(task)
    .map((await harness.importDashboardModule('js/groups-taskboard-card.js')).refKey);
  assert.equal(refsPlan(task, { refs: keys, newRef: '' }), null, 'no change');
  assert.deepEqual(refsPlan(task, { refs: [keys[1], keys[0]], newRef: '' }), { firstRef: keys[1] });
  assert.deepEqual(refsPlan(task, { refs: ['LIN-9', keys[1]], newRef: '' }),
    { rmRefs: [keys[0]], addRefs: ['LIN-9'], resolveRef: true, firstRef: 'LIN-9' }, 'a new ref made first');
  assert.deepEqual(refsPlan(task, { refs: [keys[1]], newRef: '' }), { rmRefs: [keys[0]] }, 'removing the first already makes the next first');
  assert.deepEqual(refsPlan(task, { refs: keys, newRef: 'LIN-9' }), { addRefs: ['LIN-9'], resolveRef: true }, 'appended: no reorder');
});

test('↑ moves a ref to the front; the first has none', async (t) => {
  const harness = await createPreactHarness(t);
  const { RefsField } = await harness.importDashboardModule('js/groups-taskboard-refs-field.js');
  let refs = ['LIN-1', 'LIN-2', 'LIN-3'];
  const view = await harness.mount(harness.html`<${RefsField} draft=${{ refs, newRef: '' }} onChange=${(next) => { refs = next; }} onNewRef=${() => {}} />`);
  const ups = [...view.container.querySelectorAll('.tb-ref-first')];
  assert.equal(ups.length, 2, 'every ref but the first');
  assert.equal(ups[1].getAttribute('aria-label'), 'Make LIN-3 the first ref');
  await harness.act(() => harness.fireEvent(ups[1], 'click'));
  assert.deepEqual(refs, ['LIN-3', 'LIN-1', 'LIN-2'], 'the others keep their order');
  assert.match(view.container.textContent, /added on Save/);
});

test('a second ↑ keeps the rest in the order the daemon keeps, so the form shows what Save writes', async (t) => {
  const harness = await createPreactHarness(t);
  const { madeFirst } = await harness.importDashboardModule('js/groups-taskboard-refs-field.js');
  const { refsPlan } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  const task = { refs: [{ kind: 'linear', label: 'x' }, { kind: 'linear', label: 'y' }, { kind: 'linear', label: 'z' }] };
  let refs = madeFirst(task, ['x', 'y', 'z'], 'z');
  assert.deepEqual(refs, ['z', 'x', 'y']);
  refs = madeFirst(task, refs, 'x');
  assert.deepEqual(refs, ['x', 'y', 'z'], 'x first again is the stored order, not x, z, y');
  assert.equal(refsPlan(task, { refs, newRef: '' }), null);

  const two = { refs: [{ kind: 'linear', label: 'x' }, { kind: 'linear', label: 'y' }] };
  refs = madeFirst(two, ['x', 'y', 'A'], 'A');
  assert.deepEqual(refs, ['A', 'x', 'y']);
  refs = madeFirst(two, refs, 'x');
  assert.deepEqual(refs, ['x', 'y', 'A'], 'a new ref goes back after the stored ones');
  assert.deepEqual(refsPlan(two, { refs, newRef: '' }), { addRefs: ['A'], resolveRef: true });
});
