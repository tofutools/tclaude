// Tests for the Groups › Processes quick start (mocks Q1/Q2, tcl-7p6bgt): an
// empty group offers Get started, a starter opens as an editable step list
// filled from the roster, and Start saves the steps as a template and starts
// it in the group. Driven through the real Processes perspective.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { stepListToView } from '../dashboard/js/process-step-list.js';
import { STARTERS } from '../dashboard/js/process-quickstart.js';

const GROUPS = [
  { name: 'webshop', members: [
    { conv_id: 'c1', agent_id: 'agt_checkout', title: 'checkout', role: 'dev', online: true },
    { conv_id: 'c2', agent_id: 'agt_search', title: 'search', role: 'dev', online: true },
    { conv_id: 'c3', agent_id: 'agt_review', title: 'review-bot', role: 'reviewer', online: true },
  ] },
];

const EMPTY_CONTEXT = { runs: {}, groups: [{ groupId: 1, group: 'webshop', active: [], needsYou: [], done: [], planned: [] }], agents: [], orphaned: [] };

function fakeApi({ createError = null, createErrorBody = null, startErrors = [], stored = {}, preview = null } = {}) {
  const calls = { validate: [], create: [], start: [], save: [], publish: [], pin: [], unpin: [], get: [], preview: [] };
  return {
    calls,
    ...(preview ? { resolvePreview: async (view, group) => { calls.preview.push({ view, group }); return { group, nodes: preview(view) }; } } : {}),
    getTemplate: async (id) => { calls.get.push(id); return stored[id]; },
    saveTemplate: async (id, view, options) => { calls.save.push({ id, view, options }); return { ref: `${id}@sha256:2`, sourceHash: 'h2' }; },
    publishTemplate: async (id) => { calls.publish.push(id); return { id, listed: true }; },
    pinProcess: async (group, id) => { calls.pin.push({ group, id }); return { pinned: true }; },
    unpinProcess: async (group, id) => { calls.unpin.push({ group, id }); return { pinned: false }; },
    listTemplates: async () => [{ id: 'release-train', name: 'release-train' }, { id: 'db-migrate', name: 'db-migrate' }],
    templateRequirements: async () => ({ params: [], programProfiles: [] }),
    validateTemplate: async (view) => { calls.validate.push(view); return []; },
    createTemplate: async (view, options) => {
      calls.create.push({ view, options });
      if (createError) throw Object.assign(new Error(createError), { body: createErrorBody });
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
  assert.deepEqual(api.calls.create[0].options, { listed: false }, 'a one-off start is unlisted and not saved to the group');
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

test('Save to group stores the steps unlisted and pinned to the group', async (t) => {
  const { root, api, notes, click, setValue } = await mount(t);
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'triage');
  await click(editor.querySelector('[data-add-step="agent"]'));
  const save = editor.querySelector('[data-step-save-group]');
  assert.equal(save.textContent, 'Save to webshop');
  await click(save);
  assert.deepEqual(api.calls.create.map((call) => call.options), [{ listed: false, pinGroup: 'webshop' }]);
  assert.equal(JSON.stringify(api.calls.create[0].view).includes('webshop'), false, 'the group never goes into the template');
  assert.deepEqual(api.calls.start, []);
  assert.deepEqual(notes, [{ message: 'Saved triage to webshop', error: false }]);
  assert.equal(root.querySelector('[data-step-editor]'), null);
});

test('a refused start, then Save to group, pins the template already created', async (t) => {
  const api = fakeApi({ startErrors: ['boom'] });
  const { root, click, setValue } = await mount(t, { api });
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'triage');
  await click(editor.querySelector('[data-add-step="agent"]'));
  await click(editor.querySelector('[data-step-start]'));
  assert.match(editor.querySelector('.gq-error').textContent, /boom/);
  await click(editor.querySelector('[data-step-save-group]'));
  assert.equal(api.calls.create.length, 1);
  assert.deepEqual(api.calls.pin, [{ group: 'webshop', id: 'tmpl_1' }]);
});

test('Save to library lists the steps without pinning them', async (t) => {
  const { root, api, click, setValue } = await mount(t);
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'triage');
  await click(editor.querySelector('[data-add-step="agent"]'));
  await click(editor.querySelector('[data-step-save-library]'));
  assert.deepEqual(api.calls.create.map((call) => call.options), [{ listed: true }]);
});

const SAVED_VIEW = {
  template: {
    id: 'tmpl_ship', name: 'ship-feature', params: { issue: { type: 'string', required: true } },
    start: 'start',
    nodes: {
      start: { type: 'start' },
      implement: { type: 'task', name: 'Implement', performer: { kind: 'agent', profile: 'dev', prompt: 'Implement {{params.issue}}.' } },
      end: { type: 'end', outcome: 'success' },
    },
  },
  edges: [{ from: 'start', outcome: 'start', to: 'implement' }, { from: 'implement', outcome: 'pass', to: 'end' }],
  layout: { nodes: {} },
  sourceHash: 'h1', listed: false,
};

const SAVED_CONTEXT = {
  ...EMPTY_CONTEXT,
  groups: [{
    ...EMPTY_CONTEXT.groups[0],
    processes: [
      { templateId: 'tmpl_ship', name: 'ship-feature', ref: 'tmpl_ship@sha256:1', listed: false, steps: 1,
        params: [{ name: 'issue', type: 'string', required: true }, { name: 'branch', type: 'string', required: false, default: 'main' }],
        programProfiles: ['ci'] },
      { templateId: 'tmpl_nightly', name: 'nightly-check', ref: 'tmpl_nightly@sha256:1', listed: true, steps: 3, params: [], programProfiles: [] },
    ],
  }],
};

test('saved processes show as chips; ▶ asks for the inputs and consent, then starts in the group', async (t) => {
  const { root, api, notes, click, setValue, harness, settle } = await mount(t, { context: SAVED_CONTEXT });
  assert.equal(root.querySelector('[data-quickstart]'), null, 'a group with saved processes is past Get started');
  assert.equal(root.querySelector('[data-quickstart-folded]'), null);
  const saved = root.querySelector('[data-saved-processes="webshop"]');
  assert.deepEqual([...saved.querySelectorAll('[data-saved-process]')].map((el) => el.dataset.savedProcess), ['tmpl_ship', 'tmpl_nightly']);
  assert.match(saved.querySelector('[data-saved-process="tmpl_nightly"]').textContent, /nightly-check\s*3 steps/);
  assert.equal(saved.querySelector('[data-saved-new]'), null, 'new processes are built from the group process buttons');
  await click(saved.querySelector('[data-saved-run="tmpl_ship"]'));
  const strip = root.querySelector('[data-saved-strip="tmpl_ship"]');
  assert.ok(strip, 'the start strip opens');
  assert.equal(strip.querySelector('[data-start-param="branch"]').value, 'main', 'a default starts out filled');
  const start = strip.querySelector('[data-saved-start]');
  assert.match(start.getAttribute('title'), /Fill issue first/);
  await setValue(strip.querySelector('[data-start-param="issue"]'), 'JOH-412');
  assert.match(start.getAttribute('title'), /Allow its commands to run first/);
  const box = strip.querySelector('[data-start-profile="ci"]');
  Object.defineProperty(box, 'checked', { configurable: true, value: true });
  await setValue(box, true, 'change');
  assert.equal(start.disabled, false);
  await harness.act(() => harness.fireEvent(strip, 'submit'));
  await settle();
  assert.deepEqual(api.calls.start, [{ templateId: 'tmpl_ship', group: 'webshop', params: { branch: 'main', issue: 'JOH-412' }, authorizeProgramProfiles: ['ci'] }]);
  assert.deepEqual(notes, [{ message: 'Started ship-feature in webshop', error: false }]);
  assert.equal(root.querySelector('[data-saved-strip]'), null, 'the strip closes after a start');
  assert.deepEqual(api.calls.create, [], 'a rerun saves nothing');
});

test('a saved process publishes to the library and is removed from the group after a confirm', async (t) => {
  const { root, api, click } = await mount(t, { context: SAVED_CONTEXT });
  await click(root.querySelector('[data-saved-run="tmpl_ship"]'));
  await click(root.querySelector('[data-saved-publish]'));
  assert.deepEqual(api.calls.publish, ['tmpl_ship']);
  await click(root.querySelector('[data-saved-remove]'));
  assert.deepEqual(api.calls.unpin, [], 'remove asks first');
  assert.match(root.querySelector('.gq-confirm').textContent, /not in the template library/);
  await click(root.querySelector('[data-saved-remove-confirm]'));
  assert.deepEqual(api.calls.unpin, [{ group: 'webshop', id: 'tmpl_ship' }]);
  await click(root.querySelector('[data-saved-run="tmpl_nightly"]'));
  assert.equal(root.querySelector('[data-saved-publish]'), null, 'a listed process is already published');
});

test('Edit steps reopens a saved process as a step list and saves a new version of it', async (t) => {
  const api = fakeApi({ stored: { tmpl_ship: SAVED_VIEW } });
  const { root, click, setValue } = await mount(t, { api, context: SAVED_CONTEXT });
  await click(root.querySelector('[data-saved-run="tmpl_ship"]'));
  await click(root.querySelector('[data-saved-edit]'));
  assert.deepEqual(api.calls.get, ['tmpl_ship']);
  const editor = root.querySelector('[data-step-editor-saved="tmpl_ship"]');
  assert.ok(editor, 'the saved process opens in the step-list editor');
  assert.equal(editor.querySelector('.gq-name').value, 'ship-feature');
  assert.deepEqual(steps(root).map((el) => el.dataset.step), ['implement']);
  assert.match(editor.querySelector('.gq-tag').textContent, /saved in webshop/);
  assert.equal(editor.querySelector('[data-step-save-group]').textContent, 'Save');
  await setValue(editor.querySelector('[data-start-param="issue"]'), 'A-1');
  await click(editor.querySelector('[data-step-start]'));
  assert.deepEqual(api.calls.save, [], 'starting unchanged steps writes no new version');
  assert.equal(api.calls.start.at(-1).templateId, 'tmpl_ship');
  await click(root.querySelector('[data-saved-run="tmpl_ship"]'));
  await click(root.querySelector('[data-saved-edit]'));
  const again = root.querySelector('[data-step-editor-saved="tmpl_ship"]');
  await setValue(steps(root)[0].querySelector('.gq-step-text'), 'Implement {{issue}} with tests.', 'change');
  await click(again.querySelector('[data-step-save-group]'));
  assert.equal(api.calls.save.length, 1);
  assert.equal(api.calls.save[0].id, 'tmpl_ship');
  assert.deepEqual(api.calls.save[0].options, { sourceHash: 'h1' }, 'saved against the version it opened, still pinned and unlisted');
  assert.equal(api.calls.save[0].view.template.nodes.implement.performer.prompt, 'Implement {{params.issue}} with tests.');
  assert.deepEqual(api.calls.create, []);
});

test('a saved process the step list cannot show opens as the inline graph', async (t) => {
  const graphy = { ...SAVED_VIEW, template: { ...SAVED_VIEW.template, nodes: { ...SAVED_VIEW.template.nodes, fan: { type: 'parallel' } } } };
  const api = fakeApi({ stored: { tmpl_ship: graphy } });
  const { root, click } = await mount(t, { api, context: SAVED_CONTEXT });
  await click(root.querySelector('[data-saved-run="tmpl_ship"]'));
  await click(root.querySelector('[data-saved-edit]'));
  assert.equal(root.querySelector('[data-step-editor]'), null);
  const card = root.querySelector('[data-mini-editor="tmpl_ship"]');
  assert.ok(card, 'the inline mini editor opens');
  assert.ok(card.querySelector('[data-mini-steps]').hasAttribute('disabled'), 'the step list cannot show a parallel node');
});

test('a save whose pin failed keeps the stored template, so a retry only pins it', async (t) => {
  const api = fakeApi({ createError: 'the template was saved, but pinning it to the group failed: full',
    createErrorBody: { id: 'tmpl_x', ref: 'tmpl_x@sha256:1', code: 'process_group_pin' }, stored: { tmpl_x: { sourceHash: 'hx' } } });
  const { root, click, setValue } = await mount(t, { api });
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'triage');
  await click(editor.querySelector('[data-add-step="agent"]'));
  await click(editor.querySelector('[data-step-save-group]'));
  assert.match(editor.querySelector('.gq-error').textContent, /pinning it to the group failed/);
  await click(editor.querySelector('[data-step-save-group]'));
  assert.equal(api.calls.create.length, 1, 'no second template');
  assert.deepEqual(api.calls.pin, [{ group: 'webshop', id: 'tmpl_x' }]);
});

test('who-now reads the server\'s resolve-preview; the process fallback and anyone free are pickable', async (t) => {
  const api = fakeApi({ preview: (view) => Object.fromEntries(Object.entries(view.template.nodes)
    .filter(([, node]) => node.performer?.kind === 'agent')
    .map(([id, node]) => [id, node.performer.anyone
      ? { agentId: 'agt_search', name: 'search', reason: 'anyone in the group, idle now' }
      : { spawnProfile: 'sonnet-dev', reason: 'no member has role "qa"; spawns one with profile sonnet-dev' }])) });
  const context = { ...EMPTY_CONTEXT, groups: [{ ...EMPTY_CONTEXT.groups[0], defaultSpawnProfile: 'sonnet-dev', roles: [{ role: 'qa', count: 0 }] }] };
  const { root, click, setValue, settle } = await mount(t, { api, context });
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'triage');
  await click(editor.querySelector('[data-add-step="agent"]'));
  const who = steps(root)[0].querySelector('.gq-who-select');
  const anyone = [...who.querySelectorAll('option')].find((option) => option.value === 'anyone');
  assert.equal(anyone.hasAttribute('disabled'), false, 'anyone free is a real choice');
  assert.ok([...who.querySelectorAll('option')].some((option) => option.value === 'role:qa'), 'roles come from the process context');
  await setValue(who, 'role:qa', 'change');
  const fallback = editor.querySelector('[data-process-fallback] select');
  assert.equal(editor.querySelector('[data-process-fallback]').dataset.processFallback, 'wait');
  assert.match(fallback.querySelector('option[value="spawn"]').textContent, /spawn one with profile sonnet-dev/);
  await setValue(fallback, 'spawn', 'change');
  assert.equal(editor.querySelector('[data-process-fallback]').dataset.processFallback, 'spawn');
  await new Promise((resolve) => setTimeout(resolve, 450));
  await settle();
  const last = api.calls.preview.at(-1);
  assert.equal(last.group, 'webshop');
  assert.deepEqual(last.view.template.fallback, { action: 'spawn' }, 'the fallback is the process\'s, in the template');
  assert.deepEqual(last.view.template.nodes[steps(root)[0].dataset.step].performer.role, 'qa');
  assert.match(editor.querySelector('.gq-whonow').textContent, /a new sonnet-dev · step 1/, 'who-now is the server\'s answer');
  await setValue(steps(root)[0].querySelector('.gq-who-select'), 'anyone', 'change');
  await new Promise((resolve) => setTimeout(resolve, 450));
  await settle();
  assert.match(editor.querySelector('.gq-whonow').textContent, /search · step 1/);
});

// SHIP_VIEW is the build-review-ship starter stored as tmpl_ship.
const SHIP_VIEW = (() => {
  const view = stepListToView(STARTERS[0].build([{ name: 'checkout', id: 'a1', role: 'dev', online: true }, { name: 'review-bot', id: 'a3', role: 'reviewer', online: true }]).list);
  return { ...view, template: { ...view.template, id: 'tmpl_ship', name: 'ship-feature' }, sourceHash: 'h1', listed: false };
})();

async function openMini(t) {
  const api = fakeApi({ stored: { tmpl_ship: SHIP_VIEW } });
  const mounted = await mount(t, { api, context: SAVED_CONTEXT });
  await mounted.click(mounted.root.querySelector('[data-saved-edit-graph="tmpl_ship"]'));
  const card = () => mounted.root.querySelector('[data-mini-editor="tmpl_ship"]');
  const node = (id) => card().querySelector(`[data-process-node="${id}"]`);
  const fire = async (el, type, init = {}) => { await mounted.harness.act(() => mounted.harness.fireEvent(el, type, init)); await mounted.settle(); };
  return { ...mounted, api, card, node, fire };
}

test('✎ opens a saved process in place as a graph of the same steps', async (t) => {
  const { root, api, card, node } = await openMini(t);
  assert.deepEqual(api.calls.get, ['tmpl_ship']);
  assert.ok(card(), 'the mini editor opens in the group body');
  assert.ok(root.querySelector('[data-saved-editing="tmpl_ship"]'), 'the chip says it is being edited');
  for (const id of ['start', 'implement', 'review', 'approve', 'rework', 'final-say', 'merge', 'end']) assert.ok(node(id), `node ${id}`);
  assert.match(node('implement').textContent, /any dev/, 'a step says who does it');
  assert.ok(card().querySelector('[data-mini-save]').hasAttribute('disabled'), 'nothing to save yet');
  assert.equal(card().querySelector('[data-mini-changes]'), null);
});

test('hover handles add a step after; undo takes it back', async (t) => {
  const { card, node, click, fire } = await openMini(t);
  await fire(node('implement'), 'mouseenter');
  const handles = card().querySelector('[data-mini-handles="implement"]');
  assert.ok(handles, 'hovering shows the handles');
  assert.ok(handles.querySelector('[data-mini-handle="branch"]').hasAttribute('disabled'), 'a task does not branch');
  await click(handles.querySelector('[data-mini-handle="after"]'));
  await click(card().querySelector('[data-mini-picker="after"] [data-mini-kind="command"]'));
  assert.ok(node('command'), 'the new command step is drawn');
  assert.ok(card().querySelector('[data-mini-inspector="command"]'), 'and opened in the inspector');
  assert.match(card().querySelector('[data-mini-changes]').textContent, /added “Command”/);
  await click(card().querySelector('[data-mini-undo]'));
  assert.equal(node('command'), null);
});

test('right-click structure: branch a "you" step, and delete names what goes with it', async (t) => {
  const { card, node, click, fire } = await openMini(t);
  await fire(node('approve'), 'contextmenu');
  const menu = card().querySelector('.gm-menu');
  assert.ok(menu, 'right-click opens the structure menu');
  assert.match(menu.querySelector('[data-mini-menu="delete"]').textContent, /Delete step \(and Rework, Final say, only reached through it\)/);
  assert.ok(menu.querySelector('[data-mini-menu="before"] [data-mini-kind="agent"]'));
  await click(menu.querySelector('[data-mini-menu="branch"] [data-mini-kind="agent"]'));
  assert.ok(node('new-step'), 'the branch step is drawn');
  const inspector = card().querySelector('[data-mini-inspector="new-step"]');
  assert.ok(inspector);
  await fire(node('approve'), 'contextmenu');
  await click(card().querySelector('.gm-menu [data-mini-menu="delete"]'));
  assert.equal(node('approve'), null);
  assert.equal(node('rework'), null, 'the steps only it led to went too');
});

test('the inspector edits a step; Save writes a new version against the opened hash', async (t) => {
  const { api, card, node, click, setValue, notes, root } = await openMini(t);
  await click(node('review'));
  const inspector = card().querySelector('[data-mini-inspector="review"]');
  assert.ok(inspector, 'click opens the compact inspector');
  await setValue(inspector.querySelector('[data-mini-name]'), 'Security review', 'change');
  await setValue(card().querySelector('[data-mini-inspector="review"] [data-mini-text]'), 'Check {{issue}} for security holes.', 'change');
  assert.match(node('review').textContent, /Security review/);
  await click(card().querySelector('[data-mini-save]'));
  assert.equal(api.calls.save.length, 1);
  const [{ id, view, options }] = api.calls.save;
  assert.equal(id, 'tmpl_ship');
  assert.deepEqual(options, { sourceHash: 'h1' });
  assert.equal(view.template.nodes.review.name, 'Security review');
  assert.equal(view.template.nodes.review.performer.prompt, 'Check {{params.issue}} for security holes.');
  assert.match(notes.at(-1).message, /Saved ship-feature as a new version/);
  assert.equal(root.querySelector('[data-mini-editor]'), null, 'the card closes after a save');
});

test('☰ Steps and ◈ Graph switch views over the same unsaved draft', async (t) => {
  const { api, root, card, node, click, fire } = await openMini(t);
  await fire(node('merge'), 'contextmenu');
  await click(card().querySelector('.gm-menu [data-mini-menu="before"] [data-mini-kind="you"]'));
  await click(card().querySelector('[data-mini-steps]'));
  const editor = root.querySelector('[data-step-editor-saved="tmpl_ship"]');
  assert.ok(editor, 'the step list shows the draft');
  assert.ok(steps(root).some((el) => el.dataset.step === 'decide'), 'with the step added in the graph');
  await click(editor.querySelector('[data-mini-graph]'));
  assert.ok(node('decide'), 'and back in the graph');
  assert.ok(card().querySelector('[data-mini-changes]'), 'still unsaved');
  await click(card().querySelector('[data-mini-cancel]'));
  await click(card().querySelector('[data-mini-discard]'));
  assert.equal(root.querySelector('[data-mini-editor]'), null);
  assert.deepEqual(api.calls.save, []);
});

test('⤢ full editor saves a dirty draft first, then hands the template over', async (t) => {
  const { api, card, node, click, fire, handoff } = await openMini(t);
  await fire(node('review'), 'contextmenu');
  await click(card().querySelector('.gm-menu [data-mini-menu="duplicate"]'));
  await click(card().querySelector('[data-mini-full]'));
  assert.equal(api.calls.save.length, 1);
  assert.deepEqual(handoff.takeProcessDraft(), { id: 'tmpl_ship', name: 'ship-feature' });
});

test('dragging the + handle into empty space picks a kind for a step from it', async (t) => {
  const { card, node, click, fire } = await openMini(t);
  await fire(node('approve'), 'mouseenter');
  const plus = card().querySelector('[data-mini-handles="approve"] [data-mini-handle="after"]');
  const canvas = card().querySelector('[data-mini-canvas]');
  await fire(plus, 'pointerdown', { clientX: 100, clientY: 40 });
  await fire(canvas, 'pointermove', { clientX: 220, clientY: 160 });
  assert.ok(card().querySelector('.gm-ghost'), 'a ghost follows the drag');
  await fire(canvas, 'pointerup', { clientX: 220, clientY: 160 });
  const picker = card().querySelector('[data-mini-picker]');
  assert.equal(picker.dataset.miniPicker, 'branch', 'from a decision, the new step is a branch');
  await click(picker.querySelector('[data-mini-kind="agent"]'));
  assert.ok(node('new-step'));
});
