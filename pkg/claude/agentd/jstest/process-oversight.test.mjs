import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const tick = () => new Promise((r) => setTimeout(r, 0));

const NAMES = { agt_lena: 'lena', agt_alice: 'alice', agt_esme: 'esme' };
const nameOf = (id) => NAMES[id] || '';

function view(concerns = []) {
  return {
    id: 'run_ov', status: 'running',
    overseer: { ref: 'agent:agt_lena', agentId: 'agt_lena', name: 'lena', via: 'lead', escalationAgentId: 'agt_esme', escalationName: 'esme' },
    concerns,
  };
}

function fakeApi() {
  const calls = [];
  return {
    calls,
    raiseConcern: async (runId, body) => { calls.push(['raise', runId, body]); return {}; },
    markBlocked: async (runId, body) => { calls.push(['park', runId, body]); return {}; },
    cancelRun: async (runId, body) => { calls.push(['cancel', runId, body]); return {}; },
    sendBack: async (runId, body) => { calls.push(['back', runId, body]); return {}; },
    answerConcern: async (runId, id, body) => { calls.push(['answer', runId, id, body]); return {}; },
    pickConcern: async (messageId, choice, note) => { calls.push(['pick', messageId, choice, note]); return {}; },
  };
}

test('the run log says each oversight step in words, with names for agents', async (t) => {
  const harness = await createPreactHarness(t);
  const { eventSentence, overseerText, actorName } = await harness.importDashboardModule('js/process-oversight.js');
  assert.equal(overseerText(view().overseer), "lena · the group's lead · escalates to esme");
  assert.equal(overseerText({ ref: 'human:operator', via: 'starter' }), 'you (the operator) · started the run');
  assert.equal(actorName('tclaude:oversight'), 'the engine');
  const say = (event) => eventSentence(event, nameOf).text;
  assert.equal(say({ kind: 'concern_raised', actor: 'agent:agt_alice', nodeId: 'work', payload: { concern: 3, reason: 'Spec contradicts itself.\nmore' } }),
    'alice raised concern #3 at work: Spec contradicts itself.');
  assert.equal(say({ kind: 'concern_escalated', actor: 'tclaude:oversight', payload: { concern: 3, level: 'escalation', holder: 'agent:agt_esme', why: 'lena did not answer within 30m0s' } }),
    'concern #3 passed to esme (lena did not answer within 30m0s)');
  assert.equal(say({ kind: 'concern_escalated', actor: 'agent:agt_esme', payload: JSON.stringify({ concern: 3, level: 'operator' }) }),
    'esme passed concern #3 to you');
  assert.equal(say({ kind: 'concern_resolved', actor: 'human:operator', payload: { concern: 3, resolution: 'resume', note: 'go' } }),
    'you settled concern #3: resume: go');
  assert.equal(say({ kind: 'run_paused', actor: 'agent:agt_lena', payload: {} }), 'lena paused the run');
  assert.equal(say({ kind: 'node_completed', nodeId: 'work', payload: {} }), 'node completed at work');
});

test('a concern with the operator offers its card\'s choices; one with an agent can be settled or passed up', async (t) => {
  const harness = await createPreactHarness(t);
  const { RunOversight } = await harness.importDashboardModule('js/process-oversight.js');
  const api = fakeApi();
  let changed = 0;
  const concerns = [
    { id: 3, nodeId: 'work', source: 'agent', raisedBy: 'agent:agt_alice', raisedName: 'alice', reason: 'CI is red.', state: 'open', level: 'operator', messageId: 41, choices: ['Resume', 'Reassign', 'Keep paused'] },
    { id: 4, source: 'agent', raisedBy: 'agent:agt_alice', raisedName: 'alice', reason: 'Docs are stale.', state: 'open', level: 'overseer', holder: 'agent:agt_lena', holderName: 'lena' },
    { id: 2, source: 'agent', raisedName: 'alice', reason: 'old', state: 'resolved', level: 'overseer', resolution: 'continue' },
  ];
  const mounted = await harness.mount(harness.html`<${RunOversight} view=${view(concerns)} runId="run_ov" api=${api} onChanged=${() => { changed += 1; }} />`);
  const root = mounted.container;
  assert.match(root.querySelector('[data-run-overseer]').textContent, /overseen by lena · the group's lead · escalates to esme/);
  assert.equal(root.querySelector('[data-run-concern-count]').textContent.trim(), '⚑ 2 open');
  assert.equal(root.querySelectorAll('[data-run-concern]').length, 2, 'a settled concern is in the log, not the strip');
  const card = root.querySelector('[data-run-concern="3"]');
  assert.match(card.querySelector('[data-concern-holder]').textContent, /waiting on you/);
  assert.deepEqual([...card.querySelectorAll('[data-concern-choice]')].map((b) => b.textContent), ['Resume', 'Reassign', 'Keep paused']);
  await harness.act(async () => {
    const note = card.querySelector('input');
    note.value = 'retries: three';
    note.dispatchEvent(new Event('input', { bubbles: true }));
    await tick();
  });
  await harness.act(async () => { card.querySelector('[data-concern-choice="Keep paused"]').click(); await tick(); });
  assert.deepEqual(api.calls.at(-1), ['pick', 41, 3, 'retries: three']);

  const agentCard = root.querySelector('[data-run-concern="4"]');
  assert.match(agentCard.querySelector('[data-concern-holder]').textContent, /with lena/);
  await harness.act(async () => { agentCard.querySelector('[data-concern-answer="escalate"]').click(); await tick(); });
  assert.deepEqual(api.calls.at(-1), ['answer', 'run_ov', 4, { action: 'escalate', note: '' }]);
  assert.equal(changed, 2);

  const cardless = await harness.mount(harness.html`<${RunOversight} view=${view([{ id: 5, source: 'agent', raisedName: 'alice', reason: 'Card gone.', state: 'open', level: 'operator' }])} runId="run_ov" api=${api} />`);
  const orphan = cardless.container.querySelector('[data-run-concern="5"]');
  assert.deepEqual([...orphan.querySelectorAll('[data-concern-answer]')].map((b) => b.dataset.concernAnswer), ['continue', 'pause'],
    'a concern whose card is gone is still settled from the run, but has nowhere further to go');
});

test('the operator raises a concern about the selected step from the strip', async (t) => {
  const harness = await createPreactHarness(t);
  const { RunOversight } = await harness.importDashboardModule('js/process-oversight.js');
  const api = fakeApi();
  const mounted = await harness.mount(harness.html`<${RunOversight} view=${view()} runId="run_ov" api=${api} nodeId="work" nodeLabel="Work" />`);
  const root = mounted.container;
  await harness.act(async () => { root.querySelector('[data-run-raise-open]').click(); await tick(); });
  await harness.act(async () => {
    const area = root.querySelector('[data-run-raise] textarea');
    area.value = 'This step waits on a vendor.';
    area.dispatchEvent(new Event('input', { bubbles: true }));
    await tick();
  });
  await harness.act(async () => { harness.getByRole(root, 'button', { name: 'Raise' }).click(); await tick(); });
  assert.deepEqual(api.calls, [['raise', 'run_ov', { reason: 'This step waits on a vendor.', nodeId: 'work' }]]);
  assert.equal(root.querySelector('[data-run-raise]'), null, 'the form closes once raised');

  const done = await harness.mount(harness.html`<${RunOversight} view=${{ ...view(), status: 'completed' }} runId="run_ov" api=${api} />`);
  assert.equal(done.container.querySelector('[data-run-raise-open]'), null, 'a finished run takes no concerns');
});

test('Messages: a run concern\'s card has no agent sender, offers its choices and no dismiss', async (t) => {
  const harness = await createPreactHarness(t);
  const { DecisionChoices } = await harness.importDashboardModule('js/mail-parts.js');
  const requests = [];
  const fetchImpl = async (url, init) => { requests.push({ url, body: JSON.parse(init.body) }); return { ok: true, status: 200, json: async () => ({}) }; };
  const toasts = [];
  const message = { id: 41, from_title: 'Process oversight', process_run: 'run_ov', options: ['Resume', 'Keep paused'], recommended: 1 };
  const view = await harness.mount(harness.html`<${DecisionChoices} message=${message} fetchImpl=${fetchImpl} toast=${(text) => toasts.push(text)} />`);
  const buttons = [...view.container.querySelectorAll('button')].map((b) => b.textContent);
  assert.deepEqual(buttons, ['Resume ★', 'Keep paused']);
  await harness.act(async () => { harness.getByRole(view.container, 'button', { name: 'Keep paused' }).click(); await tick(); });
  assert.deepEqual(requests, [{ url: '/api/human-messages/reply', body: { id: 41, choice: 2 } }]);
  assert.deepEqual(toasts, ['“Keep paused” applied to run run_ov']);

  const plain = await harness.mount(harness.html`<${DecisionChoices} message=${{ id: 5, options: ['a', 'b'] }} />`);
  assert.equal(plain.container.textContent, '', 'a decision with neither a sender nor a run has nobody to answer');
});

test('the strip parks the selected step that is out, and gives the run up, each with its why', async (t) => {
  const harness = await createPreactHarness(t);
  const { RunOversight, eventSentence } = await harness.importDashboardModule('js/process-oversight.js');
  const api = fakeApi();
  const running = { ...view(), commands: [{ nodeId: 'work', kind: 'agent', attempt: 2, parkable: true }, { nodeId: 'build', kind: 'program', attempt: 1 }, { nodeId: 'decide', kind: 'agent', attempt: 1 }] };
  const type = async (root, text) => harness.act(async () => {
    const area = root.querySelector('[data-run-steer] textarea');
    area.value = text;
    area.dispatchEvent(new Event('input', { bubbles: true }));
    await tick();
  });
  const mounted = await harness.mount(harness.html`<${RunOversight} view=${running} runId="run_ov" api=${api} nodeId="work" nodeLabel="Work" />`);
  const root = mounted.container;
  await harness.act(async () => { root.querySelector('[data-run-park-open]').click(); await tick(); });
  assert.equal(harness.getByRole(root, 'button', { name: 'Park step' }).disabled, true, 'a step is parked with a reason');
  await type(root, 'The API it needs was removed.');
  await harness.act(async () => { harness.getByRole(root, 'button', { name: 'Park step' }).click(); await tick(); });
  assert.deepEqual(api.calls.at(-1), ['park', 'run_ov', { nodeId: 'work', attempt: 2, reason: 'The API it needs was removed.' }]);

  await harness.act(async () => { root.querySelector('[data-run-cancel-open]').click(); await tick(); });
  await harness.act(async () => { harness.getByRole(root, 'button', { name: 'Cancel the run' }).click(); await tick(); });
  assert.deepEqual(api.calls.at(-1), ['cancel', 'run_ov', { note: '' }], 'the cancel note is optional');

  const program = await harness.mount(harness.html`<${RunOversight} view=${running} runId="run_ov" api=${api} nodeId="build" />`);
  assert.equal(program.container.querySelector('[data-run-park-open]'), null, 'only an agent step that is out can be parked');
  const decider = await harness.mount(harness.html`<${RunOversight} view=${running} runId="run_ov" api=${api} nodeId="decide" />`);
  assert.equal(decider.container.querySelector('[data-run-park-open]'), null, 'an agent decider is not parkable');
  const draining = await harness.mount(harness.html`<${RunOversight} view=${{ ...running, draining: true }} runId="run_ov" api=${api} nodeId="work" />`);
  assert.equal(draining.container.querySelector('[data-run-cancel-open]'), null, 'a run given up takes no more steering while it drains');
  assert.equal(draining.container.querySelector('[data-run-park-open]'), null);
  const done = await harness.mount(harness.html`<${RunOversight} view=${{ ...running, status: 'canceled' }} runId="run_ov" api=${api} nodeId="work" />`);
  assert.equal(done.container.querySelector('[data-run-cancel-open]'), null, 'an ended run is not canceled again');

  const say = (event) => eventSentence(event, nameOf).text;
  assert.equal(say({ kind: 'block_marked', actor: 'agent:agt_lena', nodeId: 'work', payload: { attempt: 2, reason: 'API gone' } }), 'lena parked work (attempt 2): API gone');
  assert.equal(say({ kind: 'run_canceled', actor: 'human:operator', payload: { note: 'superseded' } }), 'you canceled the run: superseded');
});

test('a finished step of the run\'s own graph can be sent back to, from the strip', async (t) => {
  const harness = await createPreactHarness(t);
  const { RunOversight, eventSentence } = await harness.importDashboardModule('js/process-oversight.js');
  const api = fakeApi();
  const mounted = await harness.mount(harness.html`<${RunOversight} view=${view()} runId="run_ov" api=${api} nodeId="implement" nodeLabel="Implement" nodeDone=${true} />`);
  const root = mounted.container;
  await harness.act(async () => { root.querySelector('[data-run-send-back-open]').click(); await tick(); });
  await harness.act(async () => { harness.getByRole(root, 'button', { name: 'Send back' }).click(); await tick(); });
  assert.deepEqual(api.calls.at(-1), ['back', 'run_ov', { nodeId: 'implement', note: '' }]);
  const running = await harness.mount(harness.html`<${RunOversight} view=${view()} runId="run_ov" api=${api} nodeId="implement" />`);
  assert.equal(running.container.querySelector('[data-run-send-back-open]'), null, 'only a finished step');
  const stage = await harness.mount(harness.html`<${RunOversight} view=${view()} runId="run_ov" api=${api} nodeId="work.review" nodeDone=${true} />`);
  assert.equal(stage.container.querySelector('[data-run-send-back-open]'), null, 'not a stage inside a step');
  assert.equal(eventSentence({ kind: 'sent_back', actor: 'human:operator', nodeId: 'implement', payload: { nodes: ['implement', 'review', 'decide'], note: 'spec changed' } }, nameOf).text,
    'you sent the run back to implement (2 later steps reset): spec changed');
});
