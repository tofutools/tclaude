// Each decider of a gate on its own row in the inline editor (tk_44d02f32,
// mock item 5 variant A): process-gate-deciders.js and the inspector's ▸ rows.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import {
  deciderOwnAsk, deciderSummary, gateAsk, reviewedStep, setDeciderAsk, setDeciderFallback, setDeciderNot, setGateAsk,
  stuckGate, withReviewerDefault,
} from '../dashboard/js/process-gate-deciders.js';
import { addDecider } from '../dashboard/js/process-mini-edit.js';

const ASK = 'Review {{ outputs.pr }}: approve or send back.';
const reviewer = (extra = {}) => ({ kind: 'agent', role: 'reviewer', prompt: ASK, ...extra });

function gateView(deciders, { captures = ['pr'] } = {}) {
  return {
    template: {
      id: 'tpl_1', name: 'Ship it', start: 'start',
      nodes: {
        start: { type: 'start' },
        implement: { type: 'task', name: 'implement', captures, performer: { kind: 'agent', role: 'dev', prompt: 'Build it.' } },
        review: { type: 'decision', name: 'review', agree: 'approved', deciders },
        done: { type: 'end', result: 'success' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'implement' },
      { from: 'implement', outcome: 'next', to: 'review' },
      { from: 'review', outcome: 'approved', to: 'done' },
      { from: 'review', outcome: 'changes', to: 'implement' },
    ],
    layout: { nodes: { implement: { x: 0, y: 0 }, review: { x: 160, y: 0 }, done: { x: 320, y: 0 } } },
    sourceHash: 'h1', currentRef: 'tpl_1@sha256:a',
  };
}

test('a decider has its own ask, and the gate\'s Asks changes only those without one', () => {
  let view = gateView([reviewer(), reviewer()]);
  assert.equal(gateAsk(view.template.nodes.review), ASK);
  view = setDeciderAsk(view, 'review', 1, 'Security review of {{ outputs.pr }}').view;
  assert.equal(deciderOwnAsk(view.template.nodes.review, 1), 'Security review of {{ outputs.pr }}');
  assert.equal(deciderOwnAsk(view.template.nodes.review, 0), '');
  view = setGateAsk(view, 'review', 'Review it.').view;
  assert.deepEqual(view.template.nodes.review.deciders.map((d) => d.prompt), ['Review it.', 'Security review of {{ outputs.pr }}']);
  view = setDeciderAsk(view, 'review', 1, '  ').view;
  assert.equal(view.template.nodes.review.deciders[1].prompt, 'Review it.', 'a blank own ask is the gate\'s again');
});

test('giving the first decider its own ask keeps the shared one as the Asks, and {{x}} is stored expanded', () => {
  let view = gateView([reviewer({ prompt: 'A' }), reviewer({ prompt: 'A' })]);
  view = setDeciderAsk(view, 'review', 0, 'B', 'A').view;
  const node = () => view.template.nodes.review;
  assert.equal(gateAsk(node(), 'A'), 'A', 'the tie keeps the Asks shown');
  assert.equal(deciderOwnAsk(node(), 0, 'A'), 'B');
  assert.equal(deciderOwnAsk(node(), 1, 'A'), '');
  view = setGateAsk(view, 'review', 'C', 'A').view;
  assert.deepEqual(node().deciders.map((d) => d.prompt), ['B', 'C'], 'the Asks changes the decider that follows it');
  view = setDeciderAsk(view, 'review', 0, 'Review {{ticket}}', 'C').view;
  assert.equal(node().deciders[0].prompt, 'Review {{params.ticket}}');
});

test('a decider naming one agent takes no "not"', () => {
  const view = gateView([reviewer(), reviewer({ assignee: 'rita', role: undefined })]);
  assert.equal(setDeciderNot(view, 'review', 1, ['implement']).view.template.nodes.review.deciders[1].differentFrom, undefined);
});

test('a new agent decider is kept off the agent whose work it reviews', () => {
  const view = gateView([reviewer(), reviewer()]);
  assert.equal(reviewedStep(view, 'review'), 'implement', 'it reports the output the gate reads');
  const noOutputs = gateView([reviewer({ prompt: 'Review it.' }), reviewer({ prompt: 'Review it.' })], { captures: [] });
  assert.equal(reviewedStep(noOutputs, 'review'), 'implement', 'else the agent step leading into the gate');
  const added = addDecider(view, 'review', { mode: 'role', role: 'reviewer' }).view;
  const defaulted = withReviewerDefault(added, 'review', 2).view;
  assert.deepEqual(defaulted.template.nodes.review.deciders[2].differentFrom, ['implement']);
  assert.equal(deciderSummary(defaulted, 'review', 2), 'not implement');
  // One already set is left alone, and so is a person.
  const set = setDeciderNot(added, 'review', 2, []).view;
  set.template.nodes.review.deciders[2].sameAs = 'implement';
  assert.equal(withReviewerDefault(set, 'review', 2).view, set);
});

test('a closed row sums up what is set', () => {
  let view = gateView([reviewer(), reviewer()]);
  view = setDeciderNot(view, 'review', 1, ['implement']).view;
  view = setDeciderFallback(view, 'review', 1, { action: 'spawn', profile: 'sonnet-dev' }).view;
  view = setDeciderAsk(view, 'review', 1, 'Security review').view;
  assert.equal(deciderSummary(view, 'review', 1), 'own ask · not implement · if nobody: spawn sonnet-dev');
  assert.equal(deciderSummary(view, 'review', 0), '');
  assert.deepEqual(setDeciderFallback(view, 'review', 1, null).view.template.nodes.review.deciders[1].fallback, undefined,
    'cleared, it follows the process');
});

test('the gate warns only when a vote would wait forever', () => {
  const roster = [{ name: 'dana', role: 'dev' }, { name: 'rita', role: 'reviewer' }];
  const two = gateView([reviewer(), reviewer()]);
  assert.deepEqual(stuckGate(two, 'review', roster), { deciders: 2, members: 1, noun: 'reviewer', index: 1 });
  assert.equal(stuckGate(two, 'review', [...roster, { name: 'rob', role: 'reviewer' }]), null, 'two reviewers: no warning');
  assert.equal(stuckGate(setDeciderFallback(two, 'review', 1, { action: 'spawn', profile: 'sonnet-dev' }).view, 'review', roster), null, 'the second spawns');
  assert.equal(stuckGate(two, 'review', roster, { action: 'spawn', profile: 'sonnet-dev' }), null, 'the process spawns');
  assert.equal(stuckGate(gateView([reviewer(), { kind: 'human', ask: 'OK?' }]), 'review', roster), null, 'a person votes too');
  assert.equal(stuckGate(gateView([reviewer(), { kind: 'agent', prompt: ASK }]), 'review', roster), null, 'nobody picked yet');
  assert.deepEqual(stuckGate(gateView([reviewer(), { kind: 'agent', anyone: true, prompt: ASK }, reviewer()]), 'review', roster),
    { deciders: 3, members: 2, noun: 'members who can vote', index: 2 });
  // "Not implement" takes the one dev out: anyone-but-the-builder and a reviewer both need rita.
  const notBuilder = gateView([{ kind: 'agent', anyone: true, prompt: ASK, differentFrom: ['implement'] }, reviewer()]);
  assert.deepEqual(stuckGate(notBuilder, 'review', roster), { deciders: 2, members: 1, noun: 'member who can vote', index: 1 });
  // Matched, not first-fit: anyone first does not take the only reviewer from the reviewer decider.
  assert.equal(stuckGate(gateView([{ kind: 'agent', anyone: true, prompt: ASK }, reviewer()]), 'review',
    [{ name: 'rita', role: 'reviewer' }, { name: 'dana', role: 'dev' }]), null);
  // A spawn with no profile to use waits.
  assert.ok(stuckGate(two, 'review', roster, { action: 'spawn' }, ''), 'no default spawn profile: it waits');
  assert.equal(stuckGate(two, 'review', roster, { action: 'spawn' }, 'sonnet-dev'), null);
});

test('the inspector opens a decider in place, sums up a closed one, and its warning link spawns', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-mini-editor.js');
  const saves = [];
  const api = {
    spawnProfileNames: async () => ['sonnet-dev'],
    saveTemplate: async (id, draft) => { saves.push(structuredClone(draft)); return { ref: `tpl_1@sha256:s${saves.length}`, sourceHash: `h${saves.length + 1}` }; },
  };
  const stored = gateView([reviewer({ differentFrom: ['implement'] }), reviewer()]);
  const roster = [{ name: 'dana', role: 'dev', online: true }, { name: 'rita', role: 'reviewer', online: true }];
  const mounted = await harness.mount(harness.html`<${mod.MiniGraphEditor} group=${{ name: 'webshop' }} roster=${roster}
    process=${{ templateId: 'tpl_1', name: 'Ship it', ran: true }} view=${stored} stored=${stored} api=${api}
    onClose=${() => {}} onSteps=${() => {}} />`);
  const root = mounted.container;
  await harness.act(async () => { root.querySelector('[data-process-node="review"]').click(); });
  const inspector = () => root.querySelector('[data-mini-inspector="review"]');
  assert.equal(inspector().querySelector('[data-mini-decider-summary="0"]').textContent, 'not implement');
  assert.equal(inspector().querySelector('[data-mini-decider-panel="0"]'), null, 'closed');
  assert.match(inspector().querySelector('[data-mini-stuck]').textContent, /2 agent deciders but 1 reviewer in the group: the second vote waits forever/);

  // ▸ opens decider 2 in place: its own ask, If nobody and Not.
  const toggle = inspector().querySelector('[data-mini-decider-toggle="1"]');
  assert.equal(toggle.getAttribute('aria-expanded'), 'false');
  await harness.act(async () => { toggle.click(); });
  const panel = inspector().querySelector('[data-mini-decider-panel="1"]');
  assert.ok(panel.querySelector('[data-mini-decider-ask="1"]'));
  assert.ok(panel.querySelector('[data-mini-decider-fallback="1"]'));
  const not = panel.querySelector('[data-mini-decider-not="1"] select');
  Object.defineProperty(not, 'value', { configurable: true, writable: true, value: 'implement' });
  await harness.act(async () => { harness.fireEvent(not, 'change'); });
  const ask = inspector().querySelector('[data-mini-decider-ask="1"]');
  await harness.act(async () => { ask.value = 'Security review of {{ outputs.pr }}'; harness.fireEvent(ask, 'input'); harness.fireEvent(ask, 'change'); });

  // The warning's link sets spawn on decider 2, and the warning goes.
  await harness.act(async () => { inspector().querySelector('[data-mini-stuck-fix]').click(); });
  assert.equal(inspector().querySelector('[data-mini-stuck]'), null);
  await harness.act(async () => { inspector().querySelector('[data-mini-decider-toggle="1"]').click(); });
  assert.equal(inspector().querySelector('[data-mini-decider-summary="1"]').textContent,
    'own ask · not implement · if nobody: spawn sonnet-dev');
  await harness.act(async () => { inspector().querySelector('[data-mini-save]')?.click(); });
  mounted.unmount();
});
