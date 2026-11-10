import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const tick = () => new Promise((r) => setTimeout(r, 0));

function recorder(response = { dismissed: 1 }) {
  const requests = [];
  const fetchImpl = async (url, init) => {
    requests.push({ url, body: JSON.parse(init.body) });
    return { ok: true, status: 200, text: async () => JSON.stringify(response) };
  };
  return { requests, fetchImpl };
}

test('dismissed decisions leave the to-decide queue and the agent\'s own list', async (t) => {
  const harness = await createPreactHarness(t);
  const { pendingDecisions, decisionsFor } = await harness.importDashboardModule('js/companion-model.js');
  const snapshot = { messages: [
    { id: 1, from_agent: 'agt_a', options: ['x', 'y'] },
    { id: 2, from_agent: 'agt_a', options: ['x', 'y'], dismissed_at: '2026-11-04T10:00:00Z', dismissed_by: 'operator' },
    { id: 3, from_agent: 'agt_a', options: ['x', 'y'], decision: 'x' },
  ] };
  assert.deepEqual(pendingDecisions(snapshot).map((m) => m.id), [1]);
  assert.deepEqual(decisionsFor(snapshot, { agentId: 'agt_a' }).map((m) => m.id), [1]);
});

test('Messages: a decision can be dismissed, and a dismissed one says by whom', async (t) => {
  const harness = await createPreactHarness(t);
  const { DecisionChoices } = await harness.importDashboardModule('js/mail-parts.js');
  const { requests, fetchImpl } = recorder();
  const toasts = [];
  let done = 0;
  const message = { id: 4, from_agent: 'agt_x', from_title: 'designer', options: ['Tabs', 'Sidebar'] };
  const view = await harness.mount(harness.html`<${DecisionChoices} message=${message} fetchImpl=${fetchImpl}
    toast=${(text, bad) => toasts.push([text, bad])} onDone=${() => { done += 1; }} />`);
  await harness.act(async () => { harness.getByRole(view.container, 'button', { name: 'Dismiss' }).click(); await tick(); });
  assert.deepEqual(requests, [{ url: '/api/human-messages/dismiss', body: { ids: [4], reason: '' } }]);
  assert.deepEqual(toasts, [['Dismissed; designer is told', false]]);
  assert.equal(done, 1);

  const remote = await harness.mount(harness.html`<${DecisionChoices} message=${message} remote=${true} />`);
  assert.equal([...remote.container.querySelectorAll('button')].some((b) => b.textContent === 'Dismiss'), false, 'no dismiss on a peer\'s decision');

  const byAgent = await harness.mount(harness.html`<${DecisionChoices} message=${{ ...message, dismissed_at: 'x', dismissed_by: 'agent', dismiss_reason: 'went with tabs' }} />`);
  assert.match(byAgent.container.textContent, /Dismissed by designer: went with tabs/);
  assert.equal(byAgent.container.querySelectorAll('button').length, 0);
  const byOperator = await harness.mount(harness.html`<${DecisionChoices} message=${{ ...message, dismissed_at: 'x', dismissed_by: 'operator' }} />`);
  assert.match(byOperator.container.textContent, /Dismissed by you/);
});

test('To decide: Dismiss all confirms with the count and sends exactly the ids on screen', async (t) => {
  const harness = await createPreactHarness(t);
  const { DismissAll } = await harness.importDashboardModule('js/companion-home.js');
  const { requests, fetchImpl } = recorder({ dismissed: 2 });
  const notes = [];
  const queue = [{ id: 7 }, { id: 9 }];
  const view = await harness.mount(harness.html`<${DismissAll} queue=${queue} fetchImpl=${fetchImpl} notify=${(text) => notes.push(text)} />`);
  await harness.act(async () => { harness.getByRole(view.container, 'button', { name: 'Dismiss all (2)' }).click(); await tick(); });
  assert.match(view.container.textContent, /Dismiss 2 decisions without answering\?/);
  assert.equal(requests.length, 0, 'nothing is sent before the confirm');
  await harness.act(async () => { harness.getByRole(view.container, 'button', { name: 'Cancel' }).click(); await tick(); });
  assert.equal(requests.length, 0);
  await harness.act(async () => { harness.getByRole(view.container, 'button', { name: 'Dismiss all (2)' }).click(); await tick(); });
  await harness.act(async () => { harness.getByRole(view.container, 'button', { name: 'Dismiss 2' }).click(); await tick(); });
  assert.deepEqual(requests, [{ url: '/api/human-messages/dismiss', body: { ids: [7, 9], reason: '' } }]);
  assert.deepEqual(notes, ['Dismissed 2 of 2 decisions; each agent is told.']);

  const empty = await harness.mount(harness.html`<${DismissAll} queue=${[]} fetchImpl=${fetchImpl} notify=${() => {}} />`);
  assert.equal(empty.container.textContent, '');
});
