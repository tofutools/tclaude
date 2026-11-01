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
  assert.equal(panel.querySelector('.gq-ask-in').disabled, false, 'the flow can be described in words');
  assert.ok(panel.querySelector('[data-quickstart-draft] button[type="submit"]').disabled, 'an empty description cannot be sent');
});

test('describing the flow saves a draft pinned to the group and hands it to the editor with a scribe request', async (t) => {
  const { harness, root, api, handoff, setValue, settle } = await mount(t);
  const form = root.querySelector('[data-quickstart-draft]');
  await setValue(form.querySelector('.gq-ask-in'), '  Build it,\n then review   and ship  ');
  assert.equal(form.querySelector('button[type="submit"]').disabled, false);
  await harness.act(() => harness.fireEvent(form, 'submit'));
  await settle();
  assert.equal(api.calls.create.length, 1);
  assert.deepEqual(api.calls.create[0].options, { listed: false, pinGroup: 'webshop' }, 'the draft is saved to the group, not the library');
  assert.equal(api.calls.create[0].view.template.name, 'Build it,');
  const draft = handoff.takeProcessDraft();
  assert.equal(draft.id, 'tmpl_1');
  assert.equal(draft.view.template.id, 'tmpl_1');
  assert.match(draft.scribe.prompt, /^Build it,\n then review   and ship/);
  assert.match(draft.scribe.prompt, /agent group "webshop"/);
  assert.match(draft.scribe.prompt, /- review-bot \(role: reviewer\)/);
  assert.match(draft.scribe.prompt, /do not start it/);
});

test('the scribe draft request and name are bounded', async (t) => {
  const harness = await createPreactHarness(t);
  const { scribeDraftName, scribeDraftRequest } = await harness.importDashboardModule('js/groups-quickstart.js');
  assert.equal(scribeDraftName('  \n '), 'untitled process');
  assert.equal(scribeDraftName('a  b\nsecond line'), 'a b');
  assert.equal(scribeDraftName('x'.repeat(100)).length, 60);
  assert.match(scribeDraftRequest('flow', 'g', []), /no members yet/);
  const roster = Array.from({ length: 400 }, (_, i) => ({ name: `agent-${i}`, role: 'dev' }));
  const request = scribeDraftRequest('flow', 'g', roster);
  assert.ok(request.length <= 2000, `request fits the scribe prompt (${request.length})`);
  assert.match(request, /…and \d+ more/);
});

test('a starter opens as a filled step list; Start saves it and starts it in the group', async (t) => {
  const { root, api, notes, source, click, setValue } = await mount(t);
  await click(root.querySelector('[data-starter="build-review-ship"]'));
  const editor = root.querySelector('[data-step-editor="webshop"]');
  assert.ok(editor, 'the step-list editor opens in the group body');
  assert.equal(root.querySelector('[data-quickstart]'), null);
  assert.equal(editor.querySelector('.gq-name').value, 'build-review-ship');
  assert.deepEqual(steps(root).map((el) => el.dataset.step), ['implement', 'review', 'approve', 'merge']);
  assert.deepEqual(steps(root).map((el) => el.querySelector('.gq-who-select').dataset.who),
    ['role:dev', 'role:reviewer', 'you', 'command']);
  assert.match(steps(root)[0].querySelector('.gq-res').textContent, /idle \(any dev\)/);
  assert.equal(steps(root)[0].querySelector('.gq-step-text').value, 'Implement {{issue}} on a branch and open a PR. When it comes back for changes, address the notes on the same PR.', 'prompts read the {{name}} shorthand');
  const flow = editor.querySelector('[data-step-flow]');
  assert.deepEqual([...flow.querySelectorAll('[data-flow-row]')].map((el) => el.dataset.flowRow),
    ['start', 'implement', 'review', 'approve', 'merge', 'end', 'stopped'], 'the preview reads top to bottom');
  assert.equal(flow.querySelector('[data-flow-row="approve"] .gq-flow-answers').textContent, 'changes → ↺ back to 1 (max 3 rounds, then hold)hold → stop (canceled)');
  assert.equal(flow.querySelectorAll('.gq-flow-rails path:not(.head)').length, 2, 'the changes loop and hold jump on rails');
  assert.deepEqual([...editor.querySelectorAll('[data-start-param]')].map((el) => el.dataset.startParam), ['issue']);
  const start = editor.querySelector('[data-step-start]');
  assert.ok(start.disabled, 'a required start input blocks Start');
  assert.match(start.getAttribute('title'), /Fill issue first/);
  await setValue(editor.querySelector('[data-start-param="issue"]'), 'JOH-412');
  assert.match(start.getAttribute('title'), /Allow step 4 to run first/, 'commands need consent, named by step');
  assert.match(editor.querySelector('[data-start-blocked]').textContent, /Allow step 4 to run first/);
  assert.match(editor.querySelector('[data-consent-profile=""]').textContent, /Allow step 4 to run gh pr merge --squash \(program profile: default\)/);
  Object.defineProperty(editor.querySelector('[data-start-profile=""]'), 'checked', { configurable: true, value: true });
  await setValue(editor.querySelector('[data-start-profile=""]'), true, 'change');
  assert.equal(start.disabled, false);
  await click(start);
  assert.equal(api.calls.create.length, 1);
  assert.deepEqual(api.calls.create[0].options, { listed: false }, 'a one-off start is unlisted and not saved to the group');
  const created = api.calls.create[0].view;
  assert.equal(created.template.name, 'build-review-ship');
  assert.deepEqual(Object.keys(created.template.params), ['issue']);
  assert.ok(created.edges.some((e) => e.from === 'approve' && e.outcome === 'changes' && e.to === 'implement'));
  assert.deepEqual(created.template.nodes.approve.loops, { changes: { maxIterations: 3, onExhausted: 'hold' } }, 'changes is a bounded loop');
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
  const options = [...selects[1].querySelectorAll('option')];
  assert.deepEqual(options.map((o) => o.value), ['command', 'end', 'step'], 'later steps, the ends, and earlier steps to go back to');
  assert.match(options[2].textContent, /^↺ back to 1 · /);
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

test('an answer that goes back is a bounded loop with its rounds and what follows', async (t) => {
  const { root, api, click, setValue, settle } = await mount(t);
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'review-loop');
  await click(editor.querySelector('[data-add-step="agent"]'));
  await click(editor.querySelector('[data-add-step="you"]'));
  let decide = steps(root)[1];
  const answers = decide.querySelector('.gq-answers-in input');
  await setValue(answers, 'approved, changes, escalate');
  await harness_blur(answers);
  await settle();
  decide = steps(root)[1];
  await setValue(decide.querySelector('[aria-label="After changes"]'), 'step', 'change');
  assert.ok(steps(root)[1].querySelector('[data-loop="changes"]'), 'going back shows the loop controls');
  // Sending it forward again drops the bound; going back again starts afresh.
  await setValue(steps(root)[1].querySelector('[aria-label="After changes"]'), 'end', 'change');
  assert.equal(steps(root)[1].querySelector('[data-loop="changes"]'), null);
  await setValue(steps(root)[1].querySelector('[aria-label="After changes"]'), 'step', 'change');
  decide = steps(root)[1];
  const loop = decide.querySelector('[data-loop="changes"]');
  assert.ok(loop, 'going back shows the loop controls');
  const rounds = loop.querySelector('.gq-loop-rounds');
  assert.equal(rounds.value, '3', 'the default round limit');
  await setValue(rounds, '150', 'change');
  assert.equal(steps(root)[1].querySelector('.gq-loop-rounds').value, '3', 'an out-of-range count shows the kept one again');
  await setValue(steps(root)[1].querySelector('.gq-loop-rounds'), '4', 'change');
  await setValue(steps(root)[1].querySelector('[aria-label="When changes has used its rounds"]'), 'escalate', 'change');
  await click(editor.querySelector('[data-step-save-library]'));
  const view = api.calls.create.at(-1).view;
  assert.deepEqual(view.template.nodes.approve.loops, { changes: { maxIterations: 4, onExhausted: 'escalate' } });
  assert.ok(view.edges.some((e) => e.from === 'approve' && e.outcome === 'changes' && e.to === 'step'));
});

test('an agent step can decide: its verdicts loop like a person\'s answers', async (t) => {
  const { root, api, click, setValue } = await mount(t);
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'agent-review-loop');
  await click(editor.querySelector('[data-add-step="agent"]'));
  await click(editor.querySelector('[data-add-step="agent"]'));
  assert.equal(steps(root)[0].querySelector('.gq-answers'), null, 'a plain agent step has no verdicts');
  await click(steps(root)[1].querySelector('[data-step-decides]'));
  const review = () => steps(root)[1];
  const [work, id] = steps(root).map((el) => el.dataset.step);
  assert.equal(review().querySelector('[data-step-decides]'), null);
  assert.equal(review().querySelector('.gq-answers-in input').value, 'approve, changes');
  await setValue(review().querySelector('[aria-label="After changes"]'), work, 'change');
  assert.equal(review().querySelector('[data-loop="changes"] .gq-loop-rounds').value, '3', 'sending work back is a bounded loop');
  await setValue(review().querySelector('.gq-who-select'), 'you', 'change');
  assert.equal(review().querySelector('.gq-answers-in input').value, 'approve, changes', 'a person takes over the verdicts as answers');
  await setValue(review().querySelector('.gq-who-select'), 'agent:review-bot', 'change');
  assert.ok(review().querySelector('[data-loop="changes"]'), 'and an agent takes them back, loop and all');
  await click(editor.querySelector('[data-step-save-library]'));
  const view = api.calls.create.at(-1).view;
  const node = view.template.nodes[id];
  assert.equal(node.type, 'decision');
  assert.equal(node.performer.kind, 'agent');
  assert.equal(node.performer.choices, undefined, 'an agent decider names its verdicts only as outcomes');
  assert.deepEqual(node.loops, { changes: { maxIterations: 3 } });
  assert.equal(view.edges.find((e) => e.from === id && e.outcome === 'changes').to, work);
  assert.ok(view.edges.some((e) => e.from === id && e.outcome === 'approve'));
});

test('an agent decider with no verdicts is a plain agent step again', async (t) => {
  const { root, click, setValue, settle } = await mount(t);
  await click(root.querySelector('[data-quickstart-blank]'));
  await click(root.querySelector('[data-step-editor] [data-add-step="agent"]'));
  await click(steps(root)[0].querySelector('[data-step-decides]'));
  const verdicts = steps(root)[0].querySelector('.gq-answers-in input');
  await setValue(verdicts, '');
  await harness_blur(verdicts);
  await settle();
  assert.equal(steps(root)[0].querySelector('.gq-answers'), null);
  assert.ok(steps(root)[0].querySelector('[data-step-decides]'));
});

test('an agent decider\'s approve follows the list as steps are added after it', async (t) => {
  const { root, api, click, setValue } = await mount(t);
  await click(root.querySelector('[data-quickstart-blank]'));
  const editor = root.querySelector('[data-step-editor]');
  await setValue(editor.querySelector('.gq-name'), 'decide-then-more');
  await click(editor.querySelector('[data-add-step="agent"]'));
  await click(steps(root)[0].querySelector('[data-step-decides]'));
  await click(editor.querySelector('[data-add-step="agent"]'));
  const [decider, after] = steps(root).map((el) => el.dataset.step);
  await click(editor.querySelector('[data-step-save-library]'));
  const view = api.calls.create.at(-1)?.view;
  assert.ok(view, 'the draft saves');
  assert.equal(view.edges.find((e) => e.from === decider && e.outcome === 'approve').to, after, 'approve continues to the new step');
  assert.equal(view.template.nodes[decider].loops, undefined);
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
  assert.equal(root.querySelector('[data-saved-strip] [data-process-action="export"], .gq-strip [data-process-action="export"]').dataset.id, 'tmpl_ship');
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

test('a saved chip\'s × opens the removal confirmation directly; keep backs out', async (t) => {
  const { root, api, click } = await mount(t, { context: SAVED_CONTEXT });
  await click(root.querySelector('[data-saved-remove-chip="tmpl_nightly"]'));
  assert.deepEqual(api.calls.unpin, [], 'remove asks first');
  assert.match(root.querySelector('.gq-confirm').textContent, /stays in the template library/);
  await click([...root.querySelectorAll('.gq-confirm .gq-link')].find((el) => el.textContent === 'keep'));
  assert.equal(root.querySelector('.gq-confirm'), null, 'keep backs out');
  await click(root.querySelector('[data-saved-remove-chip="tmpl_nightly"]'));
  assert.ok(root.querySelector('.gq-confirm'), '× asks again after keep');
  await click(root.querySelector('[data-saved-remove-confirm]'));
  assert.deepEqual(api.calls.unpin, [{ group: 'webshop', id: 'tmpl_nightly' }]);
  await click(root.querySelector('[data-saved-remove-chip="tmpl_ship"]'));
  await click(root.querySelector('[data-saved-run="tmpl_ship"]'));
  assert.equal(root.querySelector('.gq-confirm'), null, '▶ switches the open strip to its start form');
  assert.ok(root.querySelector('[data-saved-strip="tmpl_ship"]'));
});

test('× then keep on an open start form keeps what was typed', async (t) => {
  const { root, api, click, setValue } = await mount(t, { context: SAVED_CONTEXT });
  await click(root.querySelector('[data-saved-run="tmpl_ship"]'));
  await setValue(root.querySelector('[data-start-param="issue"]'), 'JOH-1');
  await click(root.querySelector('[data-saved-remove-chip="tmpl_ship"]'));
  assert.ok(root.querySelector('.gq-confirm'));
  await click([...root.querySelectorAll('.gq-confirm .gq-link')].find((el) => el.textContent === 'keep'));
  assert.equal(root.querySelector('[data-start-param="issue"]').value, 'JOH-1');
  assert.deepEqual(api.calls.unpin, []);
});

test('dragging a saved chip onto the bin removes it from the group', async (t) => {
  const { harness, root, api, notes, settle } = await mount(t, { context: SAVED_CONTEXT });
  const doc = harness.window.document;
  const trash = doc.createElement('div');
  trash.id = 'dnd-trash';
  doc.body.appendChild(trash);
  t.after(() => trash.remove());
  const chip = root.querySelector('[data-saved-process-drag="tmpl_nightly"]');
  assert.equal(chip.draggable, true);
  const transfer = { data: {}, setData(type, value) { this.data[type] = value; }, get types() { return Object.keys(this.data); } };
  harness.fireEvent(chip, 'dragstart', { dataTransfer: transfer, target: chip });
  assert.equal(transfer.data['application/x-tclaude-saved-process'], 'tmpl_nightly');
  assert.equal(transfer.data['text/plain'], undefined, 'text/plain is withheld so dnd.js ignores this drag');
  assert.ok(trash.classList.contains('dnd-trash-unpin-mode'), 'the bin speaks the remove voice');
  await harness.act(() => harness.fireEvent(trash, 'drop', { dataTransfer: transfer, target: trash }));
  await settle();
  assert.deepEqual(api.calls.unpin, [{ group: 'webshop', id: 'tmpl_nightly' }], 'a listed process unpins without asking');
  assert.deepEqual(notes, [{ message: 'Removed nightly-check from webshop', error: false }]);
  assert.equal(trash.classList.contains('show'), false, 'the bin is torn down after the drop');
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
  assert.ok(root.querySelector('[data-saved-edit-graph="tmpl_nightly"]').hasAttribute('disabled'), 'another process waits until this editor closes');
  for (const id of ['start', 'implement', 'review', 'approve', 'merge', 'end', 'stopped']) assert.ok(node(id), `node ${id}`);
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
  assert.equal(menu.querySelector('[data-mini-menu="delete"]').textContent.trim(), 'Delete step', 'no other step goes with it yet');
  assert.ok(menu.querySelector('[data-mini-menu="before"] [data-mini-kind="agent"]'));
  await click(menu.querySelector('[data-mini-menu="branch"] [data-mini-kind="agent"]'));
  assert.ok(node('new-step'), 'the branch step is drawn');
  const inspector = card().querySelector('[data-mini-inspector="new-step"]');
  assert.ok(inspector);
  await fire(node('approve'), 'contextmenu');
  assert.match(card().querySelector('.gm-menu [data-mini-menu="delete"]').textContent, /Delete step \(and New step, only reached through it\)/);
  await click(card().querySelector('.gm-menu [data-mini-menu="delete"]'));
  assert.equal(node('approve'), null);
  assert.equal(node('new-step'), null, 'the steps only it led to went too');
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

test('◈ Graph after the step list saved a version continues from that version', async (t) => {
  const api = fakeApi({ stored: { tmpl_ship: SHIP_VIEW }, startErrors: ['process program profiles require explicit authorization: ci'] });
  const { root, click, setValue, settle, harness } = await mount(t, { api, context: SAVED_CONTEXT });
  await click(root.querySelector('[data-saved-run="tmpl_ship"]'));
  await click(root.querySelector('[data-saved-edit]'));
  const editor = root.querySelector('[data-step-editor-saved="tmpl_ship"]');
  await setValue(steps(root)[0].querySelector('.gq-step-text'), 'Implement {{issue}} carefully.', 'change');
  await setValue(editor.querySelector('[data-start-param="issue"]'), 'A-1');
  const box = editor.querySelector('[data-start-profile=""]');
  Object.defineProperty(box, 'checked', { configurable: true, value: true });
  await setValue(box, true, 'change');
  await click(editor.querySelector('[data-step-start]'));
  assert.equal(api.calls.save.length, 1, 'the start saved a version, then was refused');
  await click(editor.querySelector('[data-mini-graph]'));
  const card = root.querySelector('[data-mini-editor="tmpl_ship"]');
  assert.ok(card);
  const review = card.querySelector('[data-process-node="review"]');
  await harness.act(() => harness.fireEvent(review, 'contextmenu'));
  await settle();
  await click(card.querySelector('.gm-menu [data-mini-menu="duplicate"]'));
  await click(card.querySelector('[data-mini-save]'));
  assert.deepEqual(api.calls.save[1].options, { sourceHash: 'h2' }, 'saved against the version the step list wrote');
});

function stubImportFetch(t, { calls = [] } = {}) {
  const previous = globalThis.fetch;
  globalThis.fetch = async (url) => {
    calls.push(String(url));
    return { ok: true, status: 200, json: async () => ({ title: 'Ship', root: 'ship@sha256:aa', rootId: 'ship', params: [], roles: [], programs: [], assignees: [], humans: [], signatures: [],
      templates: [{ ref: 'ship@sha256:aa', id: 'ship', head: true, status: 'new' }], updates: 0, roleMap: [], reviewDigest: 'd1' }) };
  };
  t.after(() => { globalThis.fetch = previous; });
  return calls;
}

const pwfFile = (name) => ({ name, size: 3, arrayBuffer: async () => new Uint8Array([1, 2, 3]).buffer });

async function dropFile(harness, target, file) {
  const event = new globalThis.Event('drop', { bubbles: true, cancelable: true });
  Object.defineProperty(event, 'dataTransfer', { value: { types: ['Files'], files: [file] } });
  await harness.act(() => { target.dispatchEvent(event); });
  return event;
}

test('Get started imports a .pwf into the group, through the shared review', async (t) => {
  const fetched = stubImportFetch(t);
  const imports = () => fetched.filter((url) => url.startsWith('/v1/process/import'));
  const { root, click, settle, harness } = await mount(t);
  let picked = 0;
  const input = root.querySelector('[data-process-group="webshop"] [data-pwf-file]');
  input.addEventListener('click', () => { picked += 1; });
  await click(root.querySelector('[data-quickstart-import]'));
  assert.equal(picked, 1, 'the panel opens the group\'s file picker');
  Object.defineProperty(input, 'files', { configurable: true, value: [pwfFile('ship.pwf')] });
  await harness.act(() => harness.fireEvent(input, 'change'));
  await settle();
  assert.equal(imports().length, 1);
  assert.ok(imports()[0].includes('group=webshop'), imports()[0]);
  assert.equal(imports().some((url) => url.includes('apply=1')), false, 'nothing is stored before the review');
  assert.ok(harness.document.querySelector('[data-pwf-group]') || root.querySelector('[data-pwf-group]'), 'the review opens');
});

test('dropping a .pwf on a group reviews it for that group; other files fall through', async (t) => {
  const fetched = stubImportFetch(t);
  const imports = () => fetched.filter((url) => url.startsWith('/v1/process/import'));
  const { root, settle, harness } = await mount(t);
  // The page always holds hidden dialogs; they must not stop a drop.
  const idle = harness.document.body.appendChild(harness.document.createElement('div'));
  idle.setAttribute('role', 'dialog');
  idle.setAttribute('aria-modal', 'true');
  t.after(() => idle.remove());
  const group = root.querySelector('[data-process-group="webshop"]');
  const other = await dropFile(harness, group, pwfFile('agent.paf'));
  await settle();
  assert.equal(other.defaultPrevented, false, 'a .paf stays with the page drop');
  assert.equal(imports().length, 0);
  const dropped = await dropFile(harness, group, pwfFile('ship.pwf'));
  await settle();
  assert.equal(dropped.defaultPrevented, true);
  assert.equal(imports().length, 1);
  assert.ok(imports()[0].includes('group=webshop'), imports()[0]);
});
