import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A run as GET /v1/process/context projects it: an agent-scoped review loop
// where agt_coder implements, a human decides, and a fix step loops back.
function contextFixture() {
  const run = {
    runId: 'run_review',
    templateRef: 'review-loop',
    name: 'review-loop',
    status: 'running',
    action: 'awaiting_decision',
    needsYou: true,
    scope: { kind: 'group', groupId: 7, group: 'webshop', owner: 'agent:agt_lead', state: 'bound' },
    startedAt: new Date(Date.now() - 95 * 60000).toISOString(),
    nodes: [
      { id: 'start', label: 'start', kind: 'start', state: 'done', performer: { kind: 'engine' } },
      { id: 'implement', label: 'Implement', kind: 'task', state: 'done', index: 1, performer: { kind: 'agent', ref: 'agt_coder' }, description: 'Write the change.' },
      { id: 'lint', label: 'Lint', kind: 'task', state: 'done', index: 2, performer: { kind: 'program', ref: 'lint' } },
      { id: 'gate', label: 'Ship it?', kind: 'decision', state: 'awaiting', index: 3, performer: { kind: 'human' }, description: 'Approve or send back.' },
      { id: 'fix', label: 'Fix', kind: 'task', state: 'pending', index: 4, performer: { kind: 'agent', ref: 'agt_coder' } },
      { id: 'end', label: 'end', kind: 'end', state: 'pending', performer: { kind: 'engine' } },
    ],
    edges: [
      { from: 'start', to: 'implement', label: 'next' },
      { from: 'start', to: 'lint', label: 'next' },
      { from: 'implement', to: 'gate', label: 'next' },
      { from: 'lint', to: 'gate', label: 'next' },
      { from: 'gate', to: 'end', label: 'approved' },
      { from: 'gate', to: 'fix', label: 'changes' },
      { from: 'fix', to: 'gate', label: 'next', back: true },
    ],
  };
  const other = { ...run, runId: 'run_other', name: 'other', nodes: run.nodes.map((node) => ({ ...node, performer: { kind: node.performer.kind } })) };
  return {
    runs: { run_review: run, run_other: other },
    groups: [],
    agents: [{ agentId: 'agt_bound', runs: [{ runId: 'run_other', nodeId: '', index: 0, total: 4, state: 'running' }] }],
    orphaned: [],
  };
}

async function overlay(t, context = contextFixture()) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/process-terminal-overlay.js');
  const requests = [];
  const restore = mod.configureProcessContextPoller({
    interval: 60_000,
    fetch: async (url, init) => {
      requests.push({ url, init });
      return { ok: true, json: async () => context };
    },
  });
  t.after(restore);
  return { harness, mod, requests };
}

test('runs for an agent come from its bound runs and the nodes it performs', async (t) => {
  const { mod } = await overlay(t);
  const context = contextFixture();
  assert.deepEqual(mod.processRunsForAgent(context, 'agt_coder').map((run) => run.runId), ['run_review']);
  assert.deepEqual(mod.processRunsForAgent(context, 'agt_bound').map((run) => run.runId), ['run_other']);
  assert.deepEqual(mod.processRunsForAgent(context, 'agt_nobody'), []);
  assert.deepEqual(mod.processRunsForAgent(null, 'agt_coder'), []);

  const around = mod.nodeNeighbourhood(context.runs.run_review, 'gate');
  assert.deepEqual(around.upstream.map((item) => item.id), ['implement', 'lint']);
  assert.deepEqual(around.downstream.map((item) => item.id), ['end', 'fix']);
  assert.deepEqual(around.outcomes, ['approved', 'changes'], 'back edges never count as outcomes');
  assert.deepEqual(mod.nodeNeighbourhood(context.runs.run_review, 'implement').siblings.map((item) => item.id), ['lint']);
  // A rework verdict looping back is still a verdict.
  const loop = { nodes: context.runs.run_review.nodes, edges: [
    { from: 'implement', to: 'gate', label: 'next' },
    { from: 'gate', to: 'end', label: 'approved' },
    { from: 'gate', to: 'implement', label: 'changes', back: true }] };
  assert.deepEqual(mod.nodeNeighbourhood(loop, 'gate').outcomes, ['approved', 'changes']);
  assert.deepEqual(mod.nodeNeighbourhood(loop, 'gate').downstream.map((item) => item.id), ['end']);

  assert.equal(mod.formatRunAge('2026-01-01T00:00:00Z', Date.parse('2026-01-01T01:35:00Z')), '1h 35m');
  assert.equal(mod.formatRunAge('', 0), '');
});

test('the pane overlay shows nothing while processes are off and polls one shared context when on', async (t) => {
  const { harness, mod, requests } = await overlay(t);
  const off = await harness.mount(harness.html`<${mod.ProcessPaneOverlay} agentId="agt_coder" enabled=${false} />`);
  assert.equal(off.container.innerHTML, '');
  assert.equal(requests.length, 0, 'a disabled overlay never fetches');
  await off.unmount();

  const on = await harness.mount(harness.html`<div>
    <${mod.ProcessPaneOverlay} agentId="agt_coder" enabled=${true} />
    <${mod.ProcessTabIcons} agentId="agt_coder" enabled=${true} />
  </div>`);
  await harness.act(() => mod.refreshProcessContext());
  assert.equal(new Set(requests.map((request) => request.url)).size, 1);
  assert.ok(requests.length <= 2, 'two subscribers share one poller');
  const icons = on.container.querySelectorAll('[data-process-overlay="agt_coder"] button.pk-run-icon');
  assert.equal(icons.length, 1);
  assert.match(icons[0].getAttribute('title'), /review-loop/);
  assert.ok(on.container.querySelector('[data-process-tab-icons] .pk-run-icon.small'), 'the tab repeats the icon in miniature');

  const toggle = on.container.querySelector('.pk-pane-overlay-toggle');
  await harness.act(() => harness.fireEvent(toggle, 'click'));
  assert.equal(on.container.querySelectorAll('[data-process-overlay] button.pk-run-icon').length, 0, 'collapsing hides the icons');
  assert.match(on.container.querySelector('.pk-pane-overlay-count').textContent, /1/);
  await harness.act(() => harness.fireEvent(toggle, 'click'));
  assert.equal(on.container.querySelectorAll('[data-process-overlay] button.pk-run-icon').length, 1);
  await on.unmount();
});

test('the popover shows the agent\'s node, what is around it, and acts on the run', async (t) => {
  const { harness, mod } = await overlay(t);
  const composed = [];
  const decided = [];
  const groups = [];
  const actions = {
    onComposeMessage: (seed) => composed.push(seed),
    openGroups: (run) => groups.push(run.runId),
    decide: (run, nodeId, verdict) => decided.push([run.runId, nodeId, verdict]),
  };
  // From the human's point of view the run waits on the decision gate.
  const context = contextFixture();
  const run = context.runs.run_review;
  const mounted = await harness.mount(harness.html`<${mod.ProcessRunPopover} run=${run} agentId="agt_coder" actions=${actions} onClose=${() => {}} />`);
  const root = mounted.container;
  assert.ok(root.querySelector('[data-process-popover="run_review"]'));
  assert.ok(root.querySelector('.pk-mini-graph'), 'the run graph is drawn');
  assert.ok(root.querySelector('[data-process-scope="group"]'));
  assert.match(root.textContent, /running 1h 3\dm/);
  // agt_coder's current node is the planned fix step (its active work is done).
  assert.match(root.querySelector('.pk-run-popover-title').textContent, /Fix/);
  assert.match(root.textContent, /↑\s*Ship it\?/);

  const buttons = [...root.querySelectorAll('.pk-run-popover-actions button')];
  const byText = (pattern) => buttons.find((button) => pattern.test(button.textContent));
  await harness.act(() => harness.fireEvent(byText(/open in Groups/), 'click'));
  await harness.act(() => harness.fireEvent(byText(/message owner/), 'click'));
  assert.deepEqual(groups, ['run_review']);
  assert.deepEqual(composed, [{ agent: 'agt_lead', label: 'agt_lead' }]);
  assert.equal(byText(/reassign/).disabled, true);
  assert.equal(byText(/mark outcome/).disabled, true, 'a non-decision node cannot be decided');
  await mounted.unmount();

  // Seen from the decision node, the human can pick a verdict.
  const atGate = { ...run, nodes: run.nodes.map((node) => (node.id === 'gate' ? { ...node, performer: { kind: 'human', ref: 'human' } } : node)) };
  const gate = await harness.mount(harness.html`<${mod.ProcessRunPopover} run=${atGate} agentId="human" actions=${actions} onClose=${() => {}} />`);
  const verdicts = [...gate.container.querySelectorAll('.pk-run-popover-verdict')];
  assert.deepEqual(verdicts.map((button) => button.textContent.trim()), ['✓ approved (as human)', '✓ changes (as human)']);
  await harness.act(() => harness.fireEvent(verdicts[1], 'click'));
  assert.deepEqual(decided, [['run_review', 'gate', 'changes']]);
  assert.match(gate.container.textContent, /Approve or send back\./, 'the node description is the contract');
  assert.match(gate.container.textContent, /This agent's node/);
  await gate.unmount();

  // A refused decision is shown, not swallowed.
  const refusing = { ...actions, decide: async () => { throw new Error('decision is stale'); } };
  const refused = await harness.mount(harness.html`<${mod.ProcessRunPopover} run=${atGate} agentId="human" actions=${refusing} onClose=${() => {}} />`);
  await harness.act(async () => { harness.fireEvent(refused.container.querySelector('.pk-run-popover-verdict'), 'click'); });
  await harness.act(async () => {});
  assert.match(refused.container.querySelector('.pk-run-popover-error').textContent, /decision is stale/);
  await refused.unmount();

  // A run bound to an agent that performs none of its nodes names the run's
  // position, not "this agent's node".
  const bound = await harness.mount(harness.html`<${mod.ProcessRunPopover} run=${run} agentId="agt_bound" actions=${actions} onClose=${() => {}} />`);
  assert.match(bound.container.querySelector('.pk-run-popover-kicker').textContent, /The run is at/);
  await bound.unmount();
});

test('production actions post the decision and refresh the context', async (t) => {
  const { mod, requests } = await overlay(t);
  const posts = [];
  let reply = { ok: true };
  const actions = mod.processOverlayActions({ fetch: async (url, init) => { posts.push({ url, init }); return reply; } });
  await actions.decide({ runId: 'run/1' }, 'gate', 'approved');
  assert.equal(posts[0].url, '/v1/process/runs/run%2F1/decide');
  assert.equal(posts[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(posts[0].init.body), { nodeId: 'gate', verdict: 'approved' });
  assert.equal(requests.at(-1).url, '/v1/process/context');
  reply = { ok: false, status: 409, json: async () => ({ code: 'process_run_stale', error: 'decision already made' }) };
  await assert.rejects(actions.decide({ runId: 'r' }, 'gate', 'approved'), /decision already made/);
});
