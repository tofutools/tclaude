// Tests for the Groups › Processes quick start (mocks Q1/Q2, tcl-7p6bgt): an
// empty group offers Get started, a starter opens as an editable step list
// filled from the roster, and Start saves the steps as a template and starts
// it in the group. Driven through the real Processes perspective.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const GROUPS = [
  { name: 'webshop', members: [
    { conv_id: 'c1', agent_id: 'agt_checkout', title: 'checkout', role: 'dev', online: true },
    { conv_id: 'c2', agent_id: 'agt_search', title: 'search', role: 'dev', online: true },
    { conv_id: 'c3', agent_id: 'agt_review', title: 'review-bot', role: 'reviewer', online: true },
  ] },
];

const EMPTY_CONTEXT = { runs: {}, groups: [{ groupId: 1, group: 'webshop', active: [], needsYou: [], done: [], planned: [] }], agents: [], orphaned: [] };

function fakeApi({ createError = null, startErrors = [] } = {}) {
  const calls = { validate: [], create: [], start: [] };
  return {
    calls,
    listTemplates: async () => [{ id: 'release-train', name: 'release-train' }, { id: 'db-migrate', name: 'db-migrate' }],
    templateRequirements: async () => ({ params: [], programProfiles: [] }),
    validateTemplate: async (view) => { calls.validate.push(view); return []; },
    createTemplate: async (view, options) => {
      calls.create.push({ view, options });
      if (createError) throw new Error(createError);
      return { id: `tmpl_${calls.create.length}`, ref: 'x@sha256:1' };
    },
    startGroupRun: async (request) => {
      calls.start.push(request);
      const error = startErrors.shift();
      if (error) throw new Error(error);
      return { id: 'run_1' };
    },
  };
}

async function mount(t, { api = fakeApi(), context = EMPTY_CONTEXT } = {}) {
  const harness = await createPreactHarness(t);
  const [mod, list, interactions, handoff] = await Promise.all([
    harness.importDashboardModule('js/groups-processes.js'),
    harness.importDashboardModule('js/groups-list.js'),
    harness.importDashboardModule('js/groups-interactions.js'),
    harness.importDashboardModule('js/process-draft-handoff.js'),
  ]);
  const source = { context: harness.signals.signal(context), status: harness.signals.signal('ready'), error: harness.signals.signal(''), refreshes: 0 };
  source.refresh = () => { source.refreshes++; return Promise.resolve(); };
  const notes = [];
  const snapshot = { processes_enabled: true, groups: GROUPS };
  const mounted = await harness.mount(harness.html`<${interactions.GroupsInteractionProvider}>
    <${mod.GroupsProcessesFocus} groups=${GROUPS} allGroups=${GROUPS} source=${source} filter="running" api=${api}
      notify=${(message, error) => notes.push({ message, error: !!error })}>
      <${list.GroupsNativeList} groups=${GROUPS} snapshot=${snapshot} actions=${{}} />
    <//>
  <//>`);
  const root = mounted.container;
  const settle = async () => {
    for (let i = 0; i < 4; i++) await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  };
  const click = async (el) => { assert.ok(el, 'element exists'); await harness.act(() => harness.fireEvent(el, 'click')); await settle(); };
  const setValue = async (el, value, type = 'input') => {
    Object.defineProperty(el, 'value', { configurable: true, writable: true, value });
    await harness.act(() => harness.fireEvent(el, type));
    await settle();
  };
  await settle();
  return { harness, root, api, source, notes, handoff, click, setValue, settle };
}

const steps = (root) => [...root.querySelectorAll('[data-step]')];

test('an empty group offers Get started: starters, the library, a blank list and the graph editor', async (t) => {
  const { root } = await mount(t);
  const panel = root.querySelector('[data-quickstart="webshop"]');
  assert.ok(panel, 'the empty group shows Get started');
  assert.equal(root.querySelector('.gp-empty'), null, 'instead of the empty note');
  assert.deepEqual([...panel.querySelectorAll('[data-starter]')].map((el) => el.dataset.starter),
    ['build-review-ship', 'plan-fan-out-merge', 'check-fix', 'ask-me-first']);
  assert.match(panel.querySelector('[data-starter="build-review-ship"] .gq-tag').textContent, /uses: dev, reviewer/);
  assert.deepEqual([...panel.querySelectorAll('[data-reuse-template]')].map((el) => el.dataset.reuseTemplate), ['release-train', 'db-migrate']);
  assert.ok(panel.querySelector('.gq-ask-in').disabled, 'describing in words is a later ticket');
});

test('a starter opens as a filled step list; Start saves it and starts it in the group', async (t) => {
  const { root, api, notes, source, click, setValue } = await mount(t);
  await click(root.querySelector('[data-starter="build-review-ship"]'));
  const editor = root.querySelector('[data-step-editor="webshop"]');
  assert.ok(editor, 'the step-list editor opens in the group body');
  assert.equal(root.querySelector('[data-quickstart]'), null);
  assert.equal(editor.querySelector('.gq-name').value, 'build-review-ship');
  assert.deepEqual(steps(root).map((el) => el.dataset.step), ['implement', 'review', 'approve', 'rework', 'final-say', 'merge']);
  assert.deepEqual(steps(root).map((el) => el.querySelector('.gq-who-select').dataset.who),
    ['role:dev', 'role:reviewer', 'you', 'role:dev', 'you', 'command']);
  assert.match(steps(root)[0].querySelector('.gq-res').textContent, /idle \(any dev\)/);
  assert.equal(steps(root)[0].querySelector('.gq-step-text').value, 'Implement {{issue}} on a branch and open a PR.', 'prompts read the {{name}} shorthand');
  const flow = editor.querySelector('[data-step-flow]');
  assert.deepEqual([...flow.querySelectorAll('[data-flow-row]')].map((el) => el.dataset.flowRow),
    ['start', 'implement', 'review', 'approve', 'rework', 'final-say', 'merge', 'end', 'stopped'], 'the preview reads top to bottom');
  assert.equal(flow.querySelector('[data-flow-row="approve"] .gq-flow-answers').textContent, 'ship → 6hold → stop (canceled)');
  assert.equal(flow.querySelectorAll('.gq-flow-rails path:not(.head)').length, 3, 'ship, hold and final say\'s hold jump on rails');
  assert.deepEqual([...editor.querySelectorAll('[data-start-param]')].map((el) => el.dataset.startParam), ['issue']);
  const start = editor.querySelector('[data-step-start]');
  assert.ok(start.disabled, 'a required start input blocks Start');
  assert.match(start.getAttribute('title'), /Fill issue first/);
  await setValue(editor.querySelector('[data-start-param="issue"]'), 'JOH-412');
  assert.match(start.getAttribute('title'), /Allow step 6 to run first/, 'commands need consent, named by step');
  assert.match(editor.querySelector('[data-start-blocked]').textContent, /Allow step 6 to run first/);
  assert.match(editor.querySelector('[data-consent-profile=""]').textContent, /Allow step 6 to run gh pr merge --squash \(program profile: default\)/);
  Object.defineProperty(editor.querySelector('[data-start-profile=""]'), 'checked', { configurable: true, value: true });
  await setValue(editor.querySelector('[data-start-profile=""]'), true, 'change');
  assert.equal(start.disabled, false);
  await click(start);
  assert.equal(api.calls.create.length, 1);
  const created = api.calls.create[0].view;
  assert.equal(created.template.name, 'build-review-ship');
  assert.deepEqual(Object.keys(created.template.params), ['issue']);
  assert.ok(created.edges.some((e) => e.from === 'approve' && e.outcome === 'rework' && e.to === 'rework'));
  assert.deepEqual(api.calls.start, [{ templateId: 'tmpl_1', group: 'webshop', params: { issue: 'JOH-412' }, authorizeProgramProfiles: [''] }]);
  assert.deepEqual(notes, [{ message: 'Started build-review-ship in webshop', error: false }]);
  assert.ok(source.refreshes >= 2, 'the run card shows on the next refresh');
  assert.equal(root.querySelector('[data-step-editor]'), null, 'the editor closes after a start');
});

test('a refused start keeps the editor, names the missing authorization, and does not save twice', async (t) => {
  const api = fakeApi({ startErrors: ['process program profiles require explicit authorization: ci'] });
  const { root, click, setValue } = await mount(t, { api });
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'nightly');
  await click(editor.querySelector('[data-add-step="command"]'));
  await setValue(steps(root)[0].querySelector('.gq-step-text'), 'make test', 'change');
  assert.ok(editor.querySelector('[data-step-start]').disabled, 'no start before consent');
  const tick = async (profile) => {
    const box = editor.querySelector(`[data-start-profile="${profile}"]`);
    Object.defineProperty(box, 'checked', { configurable: true, value: true });
    await setValue(box, true, 'change');
  };
  await tick('');
  await click(editor.querySelector('[data-step-start]'));
  assert.match(editor.querySelector('.gq-error').textContent, /authorization: ci/);
  assert.ok(editor.querySelector('[data-start-profile="ci"]'), 'the refused profile is offered');
  assert.ok(editor.querySelector('[data-step-start]').disabled, 'the refused profile needs consent too');
  await tick('ci');
  await click(editor.querySelector('[data-step-start]'));
  assert.equal(api.calls.create.length, 1, 'the unchanged steps reuse the template already created');
  assert.equal(api.calls.start.length, 2);
  assert.deepEqual(api.calls.create[0].view.template.nodes['command'].performer, { kind: 'program', run: 'make', args: ['test'] });
});

test('editing steps: who, answers and their targets, order and removal', async (t) => {
  const { root, api, click, setValue, settle } = await mount(t);
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'gate');
  await click(editor.querySelector('[data-add-step="agent"]'));
  await click(editor.querySelector('[data-add-step="you"]'));
  await click(editor.querySelector('[data-add-step="command"]'));
  let [work, decide, cmd] = steps(root);
  await setValue(work.querySelector('.gq-who-select'), 'agent:search', 'change');
  await setValue(work.querySelector('.gq-step-text'), 'Fix {{ticket}}', 'change');
  assert.ok(editor.querySelector('[data-start-param="ticket"]'), '{{ticket}} becomes a start input');
  [work, decide, cmd] = steps(root);
  const answers = decide.querySelector('.gq-answers-in input');
  assert.equal(answers.value, 'continue');
  await setValue(answers, 'ship, hold');
  await harness_blur(answers);
  await settle();
  decide = steps(root)[1];
  const selects = [...decide.querySelectorAll('.gq-then-link select')];
  assert.deepEqual(selects.map((s) => s.getAttribute('aria-label')), ['After ship', 'After hold']);
  assert.deepEqual([...selects[1].querySelectorAll('option')].map((o) => o.value), ['command', 'end'], 'targets are forward only');
  await setValue(selects[1], 'end', 'change');
  await click(steps(root)[2].querySelector('[aria-label="Move step 3 up"]'));
  assert.deepEqual(steps(root).map((el) => el.dataset.step), ['step', 'command', 'approve']);
  await click(steps(root)[1].querySelector('[aria-label="Remove step 2"]'));
  assert.deepEqual(steps(root).map((el) => el.dataset.step), ['step', 'approve']);
  await setValue(steps(root)[1].querySelector('.gq-step-text'), 'Ship it?', 'change');
  await setValue(editor.querySelector('[data-start-param="ticket"]'), 'T-1');
  await click(editor.querySelector('[data-step-save-library]'));
  const view = api.calls.create.at(-1).view;
  assert.deepEqual(view.template.nodes.step.performer, { kind: 'agent', assignee: 'search', prompt: 'Fix {{params.ticket}}' });
  assert.deepEqual(view.edges.filter((e) => e.from === 'approve').map((e) => `${e.outcome}>${e.to}`).sort(), ['hold>end', 'ship>end'],
    'a removed target retargets to what followed it');
  assert.equal(api.calls.start.length, 0, 'saving to the library starts nothing');
});

async function harness_blur(el) {
  el.dispatchEvent(new el.ownerDocument.defaultView.Event('blur'));
}

test('reuse opens the start form with the template; the graph-shaped starter hands off to the editor', async (t) => {
  const { harness, root, api, handoff, click, settle } = await mount(t);
  await click(root.querySelector('[data-reuse-template="db-migrate"]'));
  const form = root.querySelector('.gp-start-form');
  assert.ok(form, 'the start form opens');
  await harness.act(() => harness.fireEvent(form, 'submit'));
  await settle();
  assert.equal(api.calls.start.at(-1).templateId, 'db-migrate', 'the reused template is the one started');
  await click(root.querySelector('[data-quickstart-expand]'));
  await click(root.querySelector('[data-starter="plan-fan-out-merge"]'));
  const draft = handoff.takeProcessDraft();
  assert.equal(draft.name, 'plan-fan-out-merge');
  assert.equal(draft.view.template.nodes['fan-out'].type, 'parallel');
  assert.equal(handoff.takeProcessDraft(), null, 'a draft is handed off once');
});

test('a group with runs builds a new process from the start form instead of Get started', async (t) => {
  const context = {
    runs: { run_1: { runId: 'run_1', name: 'nightly', status: 'running', nodes: [{ id: 'a', kind: 'task', state: 'running' }], edges: [], scope: { kind: 'group', group: 'webshop' } } },
    groups: [{ groupId: 1, group: 'webshop', active: ['run_1'], needsYou: [], done: [], planned: [], activeCount: 1 }], agents: [], orphaned: [],
  };
  const { root, click } = await mount(t, { context });
  assert.equal(root.querySelector('[data-quickstart]'), null);
  assert.equal(root.querySelector('[data-process-new]'), null, 'no separate new-process button');
  await click(root.querySelector('[data-process-start="webshop"]'));
  await click(root.querySelector('[data-process-new="webshop"]'));
  assert.ok(root.querySelector('[data-quickstart="webshop"]'));
  assert.equal(root.querySelector('.gp-start-form'), null, 'building replaces the start form');
});

test('open in graph editor saves the steps first, then hands the stored template to the editor', async (t) => {
  const { root, api, handoff, click } = await mount(t);
  await click(root.querySelector('[data-starter="check-fix"]'));
  const editor = root.querySelector('[data-step-editor]');
  await click(editor.querySelector('[data-step-open-graph]'));
  assert.equal(api.calls.create.length, 1);
  const draft = handoff.takeProcessDraft();
  assert.equal(draft.id, 'tmpl_1');
  assert.equal(draft.name, 'check-fix');
  assert.equal(draft.view.template.id, 'tmpl_1');
  assert.equal(draft.view.currentRef, 'x@sha256:1');
  assert.equal(root.querySelector('[data-step-editor]'), null);
  assert.equal(root.querySelector('[data-quickstart]'), null, 'once a process exists, an empty group folds Get started');
  await click(root.querySelector('[data-quickstart-expand]'));
  await click(root.querySelector('[data-quickstart-graph]'));
  assert.deepEqual(handoff.takeProcessDraft(), { create: true }, 'the blank graph editor is the tab\'s own create dialog');
  assert.equal(api.calls.create.length, 1);
});

test('a starter stored for the graph editor is created once, however often it is clicked', async (t) => {
  const { root, api, harness, settle } = await mount(t);
  const starter = root.querySelector('[data-starter="plan-fan-out-merge"]');
  await harness.act(() => { harness.fireEvent(starter, 'click'); });
  await harness.act(() => { harness.fireEvent(root.querySelector('[data-starter="plan-fan-out-merge"]'), 'click'); });
  await settle();
  assert.equal(api.calls.create.length, 1);
});

test('Start sends only the inputs the steps still use', async (t) => {
  const { root, api, click, setValue } = await mount(t);
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'fix');
  await click(editor.querySelector('[data-add-step="agent"]'));
  await setValue(steps(root)[0].querySelector('.gq-step-text'), 'Fix {{issue}}', 'change');
  await setValue(editor.querySelector('[data-start-param="issue"]'), 'A-1');
  await setValue(steps(root)[0].querySelector('.gq-step-text'), 'Fix {{ticket}}', 'change');
  await setValue(editor.querySelector('[data-start-param="ticket"]'), 'T-2');
  await click(editor.querySelector('[data-step-start]'));
  assert.deepEqual(api.calls.start.at(-1).params, { ticket: 'T-2' });
});
