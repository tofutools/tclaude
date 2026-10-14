import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// An agent-scoped run as GET /v1/process/context projects it (mock T3):
// snapshot (program) → backfill (agent, this conversation, on its 4th loop)
// → verify (program) → decide (human) → swap (program).
function agentRun() {
  const at = (minutes) => new Date(Date.now() - minutes * 60000).toISOString();
  return {
    runId: 'run_reindex', name: 'reindex-v2', status: 'running', action: 'awaiting_performer',
    scope: { kind: 'agent', agentId: 'agt_search', owner: 'human:operator', state: 'bound' },
    startedAt: at(41),
    nodes: [
      { id: 'start', kind: 'start', state: 'done', performer: { kind: 'engine' } },
      { id: 'snapshot', label: 'Snapshot index', kind: 'task', state: 'done', performer: { kind: 'program', ref: 'safe' } },
      { id: 'backfill', label: 'Backfill shards', kind: 'task', state: 'running', performer: { kind: 'agent', ref: 'agt_search' },
        dispatch: { agentId: 'agt_search', attempt: 4, state: 'delivered', deliveredAt: at(21) } },
      { id: 'verify', label: 'Verify recall@10', kind: 'task', state: 'pending', performer: { kind: 'program', ref: 'eval' } },
      { id: 'decide', label: 'Decide', kind: 'decision', state: 'pending', performer: { kind: 'human' } },
      { id: 'swap', label: 'Swap alias', kind: 'task', state: 'pending', performer: { kind: 'program', ref: 'alias-write' } },
      { id: 'end', kind: 'end', state: 'pending', performer: { kind: 'engine' } },
    ],
    edges: [
      { from: 'start', to: 'snapshot', label: 'next' }, { from: 'snapshot', to: 'backfill', label: 'next' },
      { from: 'backfill', to: 'verify', label: 'next' }, { from: 'verify', to: 'decide', label: 'next' },
      { from: 'decide', to: 'swap', label: 'swap' }, { from: 'decide', to: 'end', label: 'ask-lead' },
      { from: 'swap', to: 'end', label: 'next' },
    ],
  };
}

function snapshot() {
  return {
    agents: [{
      agent_id: 'agt_search', conv_id: 'conv-search', title: 'search-indexer', groups: ['webshop', 'search'],
      state: { cwd: '/home/dev/git/webshop-search-index-v2', sandbox_profiles: [{ scope: 'group', name: 'dev-net' }], cost_usd: 1.234 },
    }],
    groups: [{ id: 7, name: 'webshop', members: [] }, { id: 9, name: 'search', members: [] }],
    permissions: { grants: { 'conv-search': ['self.process', 'agent.message'] } },
  };
}

async function load(t) {
  const harness = await createPreactHarness(t);
  const stepper = await harness.importDashboardModule('js/process-agent-stepper.js');
  const overlay = await harness.importDashboardModule('js/process-terminal-overlay.js');
  return { harness, stepper, overlay };
}

test('bounds come from the snapshot row of the bound agent', async (t) => {
  const { stepper } = await load(t);
  const bounds = stepper.agentRunBounds(snapshot(), 'agt_search');
  assert.equal(bounds.dir, 'webshop-search-index-v2');
  assert.equal(bounds.sandbox, 'dev-net');
  assert.deepEqual(bounds.grants, ['agent.message', 'self.process']);
  assert.equal(bounds.costUSD, 1.234);
  assert.deepEqual(bounds.groups, [{ id: 7, name: 'webshop' }, { id: 9, name: 'search' }]);
  assert.equal(stepper.agentRunBounds(snapshot(), 'agt_unknown'), null);
  assert.equal(stepper.agentRunBounds(null, 'agt_search'), null);
  assert.ok(stepper.isAgentScopedTo(agentRun(), 'agt_search'));
  assert.ok(!stepper.isAgentScopedTo(agentRun(), 'agt_other'));
});

test('stepper rows mark turns in this conversation and warn before leaving the bounds', async (t) => {
  const { stepper } = await load(t);
  const rows = stepper.stepperRows(agentRun(), 'agt_search');
  assert.deepEqual(rows.map((row) => row.id), ['snapshot', 'backfill', 'verify', 'decide', 'swap']);
  const [snap, backfill, verify, decide] = rows;
  assert.equal(snap.kind, 'command');
  assert.match(snap.detail.join(), /ran on the host · profile safe/);
  assert.equal(snap.warning, '', 'a finished step no longer warns');
  assert.ok(backfill.here);
  assert.match(backfill.detail.join(' · '), /turn injected at \d\d:\d\d · loop ×4/);
  assert.equal(backfill.time, '21m');
  assert.match(verify.warning, /runs on the host, outside this agent's sandbox \(program profile eval\)/);
  assert.deepEqual(verify.detail, [], 'the warning already says where it runs');
  assert.equal(decide.detail.join(), 'swap · ask-lead');
  assert.match(decide.warning, /human decision/);

  // An agent node delivered to someone else is not this conversation.
  const run = agentRun();
  run.nodes[2].dispatch = { agentId: 'agt_other', attempt: 1, state: 'delivered', deliveredAt: run.startedAt };
  const elsewhere = stepper.stepperRows(run, 'agt_search')[1];
  assert.ok(!elsewhere.here);
  assert.match(elsewhere.detail.join(' · '), /performed by agt_other/);
});

test('an agent-scoped run opens as the stepper, toggles to the graph, and promotes', async (t) => {
  const { harness, stepper, overlay } = await load(t);
  const promoted = [];
  const actions = { promote: async (run, group) => { promoted.push([run.runId, group]); }, openGroups: () => {} };
  const bounds = stepper.agentRunBounds(snapshot(), 'agt_search');
  const mounted = await harness.mount(harness.html`<${overlay.ProcessRunPopover} run=${agentRun()} agentId="agt_search"
    actions=${actions} bounds=${bounds} onClose=${() => {}} />`);
  const root = mounted.container;
  assert.ok(root.querySelector('[data-process-stepper]'), 'the stepper replaces the graph');
  assert.ok(!root.querySelector('.pk-mini-graph'));
  assert.match(root.querySelector('[data-process-bounds]').textContent, /webshop-search-index-v2.*dev-net.*2 grants.*\$ 1\.23 spent.*survives reincarnate/s);
  assert.match(root.querySelector('[data-process-step="backfill"]').textContent, /← this conversation/);
  assert.ok(root.querySelector('[data-process-step="verify"] .pk-stepper-step-warning'));

  const button = (pattern) => [...root.querySelectorAll('.pk-run-popover-actions button')].find((b) => pattern.test(b.textContent));
  assert.equal(button(/pause/).disabled, true);
  const select = root.querySelector('.pk-stepper-group');
  assert.ok(select, 'two groups offer a choice');
  for (const option of select.querySelectorAll('option')) option.selected = option.value === 'search';
  await harness.act(() => harness.fireEvent(select, 'change'));
  await harness.act(async () => { harness.fireEvent(button(/promote/), 'click'); });
  assert.deepEqual(promoted, [['run_reindex', 'search']]);

  await harness.act(() => harness.fireEvent(button(/graph view/), 'click'));
  assert.ok(root.querySelector('.pk-mini-graph'), 'graph view is the T2 popover');
  const back = [...root.querySelectorAll('.pk-run-popover-actions button')].find((b) => /stepper view/.test(b.textContent));
  await harness.act(() => harness.fireEvent(back, 'click'));
  assert.ok(root.querySelector('[data-process-stepper]'));
  await mounted.unmount();

  // No bounds and no groups: the strip says so and promotion is disabled.
  const bare = await harness.mount(harness.html`<${stepper.ProcessAgentStepper} run=${agentRun()} agentId="agt_search" actions=${actions} />`);
  assert.match(bare.container.textContent, /bounds unavailable/);
  const promote = [...bare.container.querySelectorAll('button')].find((b) => /promote/.test(b.textContent));
  assert.equal(promote.disabled, true);
  await bare.unmount();

  // A refused promotion is shown.
  const refusing = { promote: async () => { throw new Error('the agent is not a member of that group'); } };
  const refused = await harness.mount(harness.html`<${stepper.ProcessAgentStepper} run=${agentRun()} agentId="agt_search" actions=${refusing} bounds=${bounds} />`);
  await harness.act(async () => { harness.fireEvent([...refused.container.querySelectorAll('button')].find((b) => /promote/.test(b.textContent)), 'click'); });
  await harness.act(async () => {});
  assert.match(refused.container.querySelector('.pk-run-popover-error').textContent, /not a member/);
  await refused.unmount();
});

test('production promote posts a rescope to the chosen group', async (t) => {
  const { overlay } = await load(t);
  const restore = overlay.configureProcessContextPoller({ fetch: async () => ({ ok: true, json: async () => ({}) }) });
  t.after(restore);
  const posts = [];
  const actions = overlay.processOverlayActions({ fetch: async (url, init) => { posts.push({ url, init }); return { ok: true }; } });
  await actions.promote({ runId: 'run_reindex' }, 'webshop');
  assert.equal(posts[0].url, '/v1/process/runs/run_reindex/rescope');
  assert.deepEqual(JSON.parse(posts[0].init.body), { group: 'webshop' });
});
