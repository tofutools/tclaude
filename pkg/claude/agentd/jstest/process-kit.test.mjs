import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

async function kit(t) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/process-kit.js');
  const sheet = await harness.importDashboardModule('js/process-kit-sheet.js');
  return { harness, mod, sheet };
}

test('kinds, states and scopes fold engine and editor spellings into the kit vocabulary', async (t) => {
  const { mod } = await kit(t);
  assert.equal(mod.normalizeNodeKind('task'), 'agent');
  assert.equal(mod.normalizeNodeKind('cmd'), 'command');
  assert.equal(mod.normalizeNodeKind('comp'), 'subprocess');
  assert.equal(mod.normalizeNodeKind('decision'), 'decision');
  assert.equal(mod.normalizeNodeKind('mystery'), 'agent');
  for (const [raw, want] of [['pending', 'planned'], ['ready', 'planned'], ['running', 'running'], ['done', 'done'], ['completed', 'done'], ['blocked', 'waiting'], ['failed', 'failed'], ['canceled', 'failed'], ['skipped', 'skipped'], [undefined, 'planned']]) {
    assert.equal(mod.normalizeNodeState(raw), want, `state ${raw}`);
  }
  assert.deepEqual(mod.normalizeScope({ kind: 'Agent', name: 'search-indexer' }), { kind: 'agent', name: 'search-indexer' });
  assert.deepEqual(mod.normalizeScope({ kind: 'planet' }), { kind: '', name: '' });
  assert.equal(mod.kindLabel('human'), 'Human step');
  assert.equal(mod.kindLabel('human', true), 'Mortal rune');
});

test('runSteps orders work nodes topologically, ignoring back edges and start/end', async (t) => {
  const { mod, sheet } = await kit(t);
  assert.deepEqual(mod.runSteps(sheet.SAMPLE_AGENT_RUN).map((n) => n.id), ['snapshot', 'backfill', 'verify', 'gate', 'wait', 'swap']);
  assert.deepEqual(mod.runSteps(sheet.SAMPLE_GROUP_RUN).map((n) => n.id), ['plan', 'branches', 'fanout', 'checkout', 'search', 'review', 'qa', 'ship']);
  // An unmarked cycle and dangling edges never lose nodes.
  const cyclic = { nodes: [{ id: 'a' }, { id: 'b' }, { id: 'c' }], edges: [{ from: 'a', to: 'b' }, { from: 'b', to: 'a' }, { from: 'c', to: 'ghost' }] };
  assert.deepEqual(mod.runSteps(cyclic).map((n) => n.id).sort(), ['a', 'b', 'c']);
  assert.deepEqual(mod.runSteps(null), []);
});

test('run state and the icon view prefer the most urgent state and the performer\'s own node', async (t) => {
  const { mod, sheet } = await kit(t);
  const run = sheet.SAMPLE_GROUP_RUN;
  assert.equal(mod.runState(run), 'waiting', 'a human gate outranks running work');
  assert.equal(mod.runState({ ...run, status: 'failed' }), 'failed');
  assert.equal(mod.runState({ ...run, status: 'completed' }), 'done');
  assert.equal(mod.runState({ nodes: [{ id: 'a', state: 'done' }, { id: 'b', state: 'skipped' }] }), 'done');
  assert.equal(mod.runState({ nodes: [{ id: 'a', state: 'pending' }] }), 'planned');

  const coder = mod.runIconView(run, 'checkout-ui');
  assert.equal(coder.state, 'running', 'a performer sees its own node state');
  assert.equal(coder.node.id, 'checkout');
  assert.equal(coder.badge, '4/8');
  assert.equal(coder.glyph, '◈');
  assert.match(coder.title, /release-2\.4 · blocked on human · group scope · webshop · at Implement checkout \(4\/8\)/);

  const human = mod.runIconView(run, 'human');
  assert.equal(human.state, 'waiting');
  assert.equal(human.node.id, 'review');

  const overall = mod.runIconView(run);
  assert.equal(overall.state, 'waiting');
  assert.equal(overall.node.id, 'review', 'without a performer the most urgent node is current');
  assert.equal(mod.runIconView({ ...run, current: 'ship' }).node.id, 'ship', 'an explicit current from the projection wins');
  assert.equal(mod.runIconView({ ...run, status: 'failed' }, 'checkout-ui').state, 'failed', 'a dead run never reads as live');

  const agent = mod.runIconView(sheet.SAMPLE_AGENT_RUN, 'search-indexer');
  assert.equal(agent.scope, 'agent');
  assert.equal(agent.glyph, '◉');
  assert.equal(agent.badge, '2/6');
});

test('miniGraphLayout lays a run out left to right with routed edges', async (t) => {
  const { mod, sheet } = await kit(t);
  const layout = mod.miniGraphLayout(sheet.SAMPLE_GROUP_RUN);
  const at = Object.fromEntries(layout.nodes.map((n) => [n.id, n]));
  assert.ok(at.start.x < at.plan.x && at.plan.x < at.branches.x && at.branches.x < at.fanout.x, 'ranks run left to right');
  assert.equal(at.checkout.x, at.search.x, 'parallel branches share a rank');
  assert.notEqual(at.checkout.y, at.search.y, 'and stack vertically');
  for (const node of layout.nodes) {
    assert.ok(node.x - node.width / 2 >= 0 && node.x + node.width / 2 <= layout.width, `${node.id} fits horizontally`);
    assert.ok(node.y - node.height / 2 >= 0 && node.y + node.height / 2 <= layout.height, `${node.id} fits vertically`);
  }
  const edge = (from, to) => layout.edges.find((e) => e.from === from && e.to === to);
  assert.equal(edge('plan', 'branches').state, 'done');
  assert.equal(edge('fanout', 'checkout').state, 'running');
  assert.equal(edge('ship', 'end').state, 'planned');
  assert.equal(edge('review', 'qa').label, 'approved');
  const gap = at.qa.x - at.qa.width / 2 - (at.review.x + at.review.width / 2);
  assert.ok(gap >= 'approved'.length * 5.4 + 20, `a labelled edge leaves room for its label (gap ${gap})`);
  assert.match(edge('checkout', 'qa').path, /^M[\d.]+,[\d.]+( L[\d.]+,[\d.]+)+$/, 'a rank-skipping edge keeps the layout\'s orthogonal route');
  for (const [x, y] of edge('checkout', 'qa').path.matchAll(/([\d.-]+),([\d.-]+)/g).map((m) => [Number(m[1]), Number(m[2])])) {
    assert.ok(x >= 0 && x <= layout.width && y >= 0 && y <= layout.height, `route point ${x},${y} is inside the bounds`);
  }

  const compact = mod.miniGraphLayout(sheet.SAMPLE_AGENT_RUN, { compact: true });
  const back = compact.edges.find((e) => e.from === 'gate' && e.to === 'backfill');
  assert.equal(back.back, true);
  assert.ok(back.labelY > Math.max(...compact.nodes.map((n) => n.y)), 'the back edge loops below the row');
  assert.ok(compact.height >= back.labelY, 'the back-edge lane is inside the bounds');
  assert.deepEqual(mod.miniGraphLayout({ nodes: [] }), { width: 0, height: 0, nodes: [], edges: [] });
});

test('chips, badge and icon render the S1 vocabulary', async (t) => {
  const { harness, mod, sheet } = await kit(t);
  const opened = [];
  const mounted = await harness.mount(harness.html`<div>
    <${mod.ProcessNodeChip} node=${{ id: 'n', label: 'Node', kind: 'human', state: 'blocked' }} />
    <${mod.ProcessScopeChip} scope=${{ kind: 'agent', name: 'search-indexer' }} />
    <${mod.ProcessScopeChip} scope=${{}} />
    <${mod.ProcessRunBadge} run=${sheet.SAMPLE_GROUP_RUN} performerId="checkout-ui" onOpen=${(run, node) => opened.push([run.id, node.id])} />
    <${mod.ProcessRunIcon} run=${sheet.SAMPLE_AGENT_RUN} performerId="search-indexer" />
  </div>`);
  const root = mounted.container;
  const chip = root.querySelector('.pk-node-chip');
  assert.ok(chip.classList.contains('pk-state-waiting'));
  assert.equal(chip.querySelector('.pk-glyph').textContent, '👤');
  assert.equal(chip.getAttribute('title'), 'Human step · blocked on human');
  const scopes = root.querySelectorAll('.pk-scope');
  assert.equal(scopes.length, 1, 'an unscoped run renders no scope chip');
  assert.equal(scopes[0].textContent, '◉ agent scope · search-indexer');

  const badge = getByRole(root, 'button', { name: /release-2\.4/ });
  assert.equal(badge.querySelector('.pk-run-node').textContent, 'Implement checkout');
  assert.deepEqual([...badge.querySelectorAll('.pk-step')].map((s) => s.className.replace('pk-step', '').trim()), ['d', 'd', 'd', 'r', 'd', 'w', '', '']);
  await harness.act(() => harness.fireEvent(badge, 'click'));
  assert.deepEqual(opened, [['run-release-24', 'checkout']]);

  const icon = root.querySelector('.pk-run-icon');
  assert.ok(icon.classList.contains('pk-scope-agent') && icon.classList.contains('pk-state-running'));
  assert.equal(icon.getAttribute('role'), 'img', 'a non-interactive icon is an image, not a button');
  assert.equal(icon.querySelector('.pk-run-icon-badge').textContent, '2/6');
  await mounted.unmount();
});

test('the mini-graph draws a gate\'s caption under its diamond and outlines the nodes named as problems', async (t) => {
  const { harness, mod } = await kit(t);
  const run = {
    id: 'r', name: 'r', status: 'planned',
    nodes: [
      { id: 'build', kind: 'agent', label: 'Build', state: 'planned', sub: 'any dev' },
      { id: 'gate', kind: 'decision', label: 'Review', state: 'planned', caption: '2 deciders · all approve' },
    ],
    edges: [{ from: 'build', to: 'gate' }],
  };
  const mounted = await harness.mount(harness.html`<div>
    <${mod.ProcessMiniGraph} run=${run} problemIds=${new Set(['build'])} onNodeClick=${() => {}} />
    <${mod.ProcessMiniGraph} run=${run} compact />
  </div>`);
  const [full, compact] = mounted.container.querySelectorAll('.pk-mini-graph');
  const gate = full.querySelector('[data-process-node="gate"]');
  assert.equal(gate.querySelector('.pk-gnode-caption').textContent, '2 deciders · all approve');
  assert.match(gate.getAttribute('title'), /2 deciders · all approve/);
  assert.equal(compact.querySelector('.pk-gnode-caption'), null, 'a compact graph has no room for it');
  assert.deepEqual([...full.querySelectorAll('.pk-gnode.problem')].map((n) => n.dataset.processNode), ['build']);
  assert.match(full.querySelector('[data-process-node="build"]').getAttribute('title'), /has a problem/);
  assert.equal(compact.querySelectorAll('.pk-gnode.problem').length, 0, 'only where problems are given');
  assert.equal(full.querySelector('[data-process-retries]'), null, 'no retries, no ↻');
  await mounted.unmount();
});

test('the mini-graph marks "you are here", and nodes become buttons only with a handler', async (t) => {
  const { harness, mod, sheet } = await kit(t);
  const clicked = [];
  const mounted = await harness.mount(harness.html`<div>
    <${mod.ProcessMiniGraph} run=${sheet.SAMPLE_GROUP_RUN} performerId="checkout-ui" onNodeClick=${(node) => clicked.push(node.id)} />
    <${mod.ProcessMiniGraph} run=${sheet.SAMPLE_AGENT_RUN} compact />
    <${mod.ProcessMiniGraph} run=${sheet.SAMPLE_AGENT_RUN} compact />
    <${mod.ProcessPerformers} performers=${[{ id: 'p', label: 'lint', kind: 'program' }]} />
  </div>`);
  assert.ok(mounted.container.querySelector('.pk-avatars > .pk-avatar.command'), 'program performers read as commands');
  const [interactive, readonly] = mounted.container.querySelectorAll('.pk-mini-graph');
  assert.equal(interactive.querySelectorAll('[data-process-node]').length, 10);
  assert.deepEqual([...interactive.querySelectorAll('.pk-gnode.here')].map((n) => n.dataset.processNode), ['checkout']);
  assert.equal(interactive.querySelectorAll('path.pk-edge').length, 10);
  const review = getByRole(interactive, 'button', { name: /Review gate · Human step · blocked on human/ });
  await harness.act(() => harness.fireEvent(review, 'click'));
  assert.deepEqual(clicked, ['review']);
  const avatar = interactive.querySelector('[data-process-node="checkout"] .pk-avatar');
  assert.equal(avatar.textContent, 'C');

  assert.ok(readonly.classList.contains('compact'));
  assert.equal(readonly.querySelectorAll('button').length, 0);
  assert.equal(readonly.querySelectorAll('.pk-gnode-sub').length, 0, 'compact graphs drop sub-lines');
  assert.ok(readonly.querySelector('path.pk-edge.back'));
  const markerIDs = [...mounted.container.querySelectorAll('marker')].map((m) => m.id);
  assert.equal(new Set(markerIDs).size, markerIDs.length, 'arrow markers are unique per graph instance, even for the same run');
  await mounted.unmount();
});

test('the S1 sheet mounts every section', async (t) => {
  const { harness, sheet } = await kit(t);
  const mounted = await harness.mount(harness.html`<${sheet.ProcessKitSheet} wizard=${true} />`);
  const root = mounted.container.querySelector('[data-process-kit-sheet]');
  assert.equal(root.querySelectorAll('.pk-sheet-box').length, 6);
  assert.equal(root.querySelectorAll('.pk-mini-graph').length, 4);
  assert.match(root.textContent, /Hero rune/);
  assert.match(root.textContent, /hero-bound · search-indexer/);
  await mounted.unmount();
});

test('the read model projection (GET /v1/process/context) renders without an adapter', async (t) => {
  const { harness, mod } = await kit(t);
  const run = {
    runId: 'run_7', templateId: 'tpl_release', status: 'running', action: 'awaiting_decision', needsYou: true,
    scope: { kind: 'group', groupId: 'grp_1', group: 'webshop', owner: 'lead', state: 'bound' },
    nodes: [
      { id: 'start', kind: 'start', performer: { kind: 'engine', ref: '' }, state: 'done' },
      { id: 'build', label: 'Build', kind: 'task', performer: { kind: 'program', ref: '' }, state: 'done' },
      { id: 'impl', label: 'Implement', kind: 'task', performer: { kind: 'agent', ref: 'agt_c' }, state: 'running' },
      { id: 'triage', kind: 'task', performer: { kind: 'agent', ref: 'unassigned' }, state: 'ready' },
      { id: 'approve', label: 'Approve', kind: 'task', performer: { kind: 'human', ref: '' }, state: 'awaiting' },
      { id: 'merge', label: 'Merge', kind: 'join', state: 'pending' },
      { id: 'end', kind: 'end', state: 'pending' },
    ],
    edges: [
      { from: 'start', to: 'build' }, { from: 'build', to: 'impl' }, { from: 'build', to: 'triage' },
      { from: 'impl', to: 'approve' }, { from: 'triage', to: 'merge' }, { from: 'approve', to: 'merge', label: 'approved' },
      { from: 'approve', to: 'impl', label: 'changes', back: true }, { from: 'merge', to: 'end' },
    ],
    current: ['impl', 'triage', 'approve'],
  };
  assert.equal(mod.runID(run), 'run_7');
  assert.equal(mod.runName(run), 'tpl_release', 'templateId names a run without a name');
  assert.deepEqual(mod.normalizeScope(run.scope), { kind: 'group', name: 'webshop' });
  assert.deepEqual(mod.normalizeScope({ kind: 'agent', agentId: 'agt_s' }), { kind: 'agent', name: 'agt_s' });
  assert.deepEqual(mod.normalizeScope({ kind: 'none' }), { kind: '', name: '' });
  const steps = mod.runSteps(run);
  assert.deepEqual(steps.map((n) => [n.id, n.kind, n.state]), [
    ['build', 'command', 'done'], ['impl', 'agent', 'running'], ['triage', 'agent', 'planned'],
    ['approve', 'human', 'waiting'], ['merge', 'parallel', 'planned'],
  ]);
  assert.equal(steps.find((n) => n.id === 'triage').label, 'triage', 'a node without a label shows its id');
  assert.deepEqual(steps.find((n) => n.id === 'impl').performers, [{ id: 'agt_c', label: 'agt_c', kind: 'agent' }]);
  assert.deepEqual(steps.find((n) => n.id === 'triage').performers, [], 'unassigned slots draw no avatar');
  assert.equal(mod.currentRunNode(run).id, 'approve', 'among several current nodes the human gate wins');
  const view = mod.runIconView(run, 'agt_c');
  assert.equal(view.node.id, 'impl');
  assert.equal(view.badge, '2/5');

  const mounted = await harness.mount(harness.html`<div>
    <${mod.ProcessRunBadge} run=${run} performerId="agt_c" />
    <${mod.ProcessMiniGraph} run=${run} performerId="agt_c" />
  </div>`);
  assert.equal(mounted.container.querySelector('.pk-run-badge').dataset.processRun, 'run_7');
  assert.equal(mounted.container.querySelector('.pk-run-name').textContent, 'tpl_release');
  assert.ok(mounted.container.querySelector('[data-process-node="impl"].here'));
  assert.ok(mounted.container.querySelector('path.pk-edge.back'));
  await mounted.unmount();
});

test('a bounded loop draws amber with its bound, and its body nodes say their round', async (t) => {
  const { harness, mod } = await kit(t);
  const loopRun = (round) => ({
    id: 'r', name: 'review loop', status: 'running',
    nodes: [
      { id: 'start', kind: 'start', label: 'start', state: 'done' },
      { id: 'write', kind: 'agent', label: 'Write', state: round ? 'running' : 'planned', ...(round ? { round, rounds: 3 } : {}) },
      { id: 'review', kind: 'decision', label: 'Review', state: 'planned', ...(round ? { round, rounds: 3 } : {}) },
      { id: 'end', kind: 'end', label: 'end', state: 'planned' },
    ],
    edges: [
      { from: 'start', to: 'write' }, { from: 'write', to: 'review' },
      { from: 'review', to: 'end', label: 'approved' },
      { from: 'review', to: 'write', label: 'changes', back: true, maxIterations: 3 },
    ],
  });
  assert.equal(mod.loopLabel({ label: 'changes', maxIterations: 3 }), 'changes · ≤3×');
  assert.equal(mod.loopLabel({ label: 'changes' }), 'changes', 'an unbounded edge keeps its label');
  assert.equal(mod.nodeRound({ round: 2, rounds: 3 }), '2/3');
  assert.equal(mod.nodeRound({}), '');
  const planned = mod.miniGraphLayout(loopRun(0)).edges.find((e) => e.loop);
  assert.deepEqual([planned.back, planned.label], [true, 'changes · ≤3×']);
  assert.equal(mod.miniGraphLayout(loopRun(2)).edges.find((e) => e.loop).label, 'changes · round 2 of 3');

  const mounted = await harness.mount(harness.html`<${mod.ProcessMiniGraph} run=${loopRun(2)} onNodeClick=${() => {}} />`);
  const graph = mounted.container;
  assert.ok(graph.querySelector('path.pk-edge.back.loop'));
  assert.equal(graph.querySelector('.pk-elabel.loop').textContent, '↺ changes · round 2 of 3');
  assert.deepEqual([...graph.querySelectorAll('[data-process-round]')].map((b) => b.closest('[data-process-node]').dataset.processNode), ['write'],
    'only the active body node wears the badge');
  assert.equal(graph.querySelector('.pk-round').textContent, '↺ 2/3');
  assert.match(getByRole(graph, 'button', { name: /^Review ·/ }).getAttribute('title'), /round 2 of 3$/, 'idle body nodes say their round');
  await mounted.unmount();
  const parked = loopRun(2);
  parked.nodes = parked.nodes.map((n) => ({ ...n, state: n.id === 'review' ? 'waiting' : n.id === 'write' ? 'done' : n.state }));
  const gate = await harness.mount(harness.html`<${mod.ProcessMiniGraph} run=${parked} />`);
  assert.equal(gate.container.querySelector('[data-process-node="review"] .pk-round')?.textContent, '↺ 2/3', 'a decision parked on its verdict wears the badge');
  await gate.unmount();
});

test('a run names its template version short, and keeps the full ref to copy', async (t) => {
  const { harness, mod } = await kit(t);
  const ref = 'a510b3f2c9e84d41@sha256:4f2a9c0e1b7d';
  assert.deepEqual(mod.templateVersionLabel({ templateRef: ref, templateVersion: 3 }), { text: 'v3 · 4f2a', ref, hashed: true });
  assert.deepEqual(mod.templateVersionLabel({ templateRef: ref }), { text: '4f2a', ref, hashed: true }, 'an unknown number shows the hash alone');
  assert.deepEqual(mod.templateVersionLabel({ templateRef: 'release-train v7' }), { text: 'release-train v7', ref: 'release-train v7', hashed: false });
  const copied = [];
  globalThis.navigator.clipboard = { writeText: async (text) => { copied.push(text); } };
  t.after(() => { delete globalThis.navigator.clipboard; });
  const { container } = await harness.mount(harness.html`<span><${mod.ProcessTemplateVersion} run=${{ templateRef: ref, templateVersion: 3 }} /></span>`);
  assert.equal(container.textContent, 'version v3 · 4f2a');
  const button = container.querySelector('[data-process-template-version]');
  assert.match(button.title, new RegExp(ref.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
  await harness.act(async () => { button.click(); await new Promise((resolve) => setTimeout(resolve, 0)); });
  assert.deepEqual(copied, [ref]);
});

// tracksRun is two reviewer tracks into a sign-off join: track a approved,
// track b on its second round.
const tracksRun = () => ({
  id: 'run-tracks',
  nodes: [
    { id: 'review-a', label: 'Review', kind: 'decision', state: 'done', round: 1, rounds: 3 },
    { id: 'review-b', label: 'Review 2', kind: 'decision', state: 'running', round: 2, rounds: 3, performer: { kind: 'agent' } },
    { id: 'fix-b', label: 'Fix 2', kind: 'task', state: 'done', round: 2, rounds: 3, performer: { kind: 'agent' } },
    { id: 'signoff', label: 'Sign off', kind: 'task', state: 'pending', performer: { kind: 'agent' } },
  ],
  edges: [
    { from: 'review-a', to: 'signoff', label: 'approved', state: 'arrived' },
    { from: 'review-b', to: 'signoff', label: 'approved', state: 'unresolved' },
    { from: 'review-b', to: 'fix-b', label: 'changes', back: true, maxIterations: 3, state: 'unresolved' },
    { from: 'fix-b', to: 'review-b', label: 'next', state: 'arrived' },
  ],
});

test('a join some tracks reached says which it still waits for', async (t) => {
  const { harness, mod } = await kit(t);
  assert.deepEqual([...mod.joinWaiting(tracksRun())], [['signoff', ['Review 2']]]);
  const allIn = tracksRun();
  allIn.edges[1].state = 'arrived';
  assert.equal(mod.joinWaiting(allIn).size, 0, 'every track arrived');
  const none = tracksRun();
  none.edges[0].state = 'unresolved';
  assert.equal(mod.joinWaiting(none).size, 0, 'no track arrived yet');
  assert.equal(mod.joinWaiting({ nodes: [{ id: 'x', state: 'pending' }], edges: [{ from: 'y', to: 'x' }] }).size, 0, 'a template has no edge states');
  const asking = tracksRun();
  asking.nodes.push({ id: 'ask-b', label: 'Review 2: rounds used up', kind: 'decision', state: 'pending', performer: { kind: 'human' } });
  asking.edges.push({ from: 'review-b', to: 'ask-b', label: 'ask-you', state: 'unresolved' },
    { from: 'ask-b', to: 'signoff', label: 'count as approved', state: 'unresolved' });
  assert.deepEqual([...mod.joinWaiting(asking)], [['signoff', ['Review 2']]], 'a track\'s "ask me" is named as its track');

  const mounted = await harness.mount(harness.html`<${mod.ProcessMiniGraph} run=${tracksRun()} onNodeClick=${() => {}} />`);
  const wait = mounted.container.querySelector('[data-process-join-waiting]');
  assert.equal(wait.closest('[data-process-node]').dataset.processNode, 'signoff');
  assert.equal(wait.textContent, '⧗ waiting for Review 2');
  assert.match(wait.closest('[data-process-node]').getAttribute('title'), /waiting for Review 2/);
  assert.equal(mounted.container.querySelector('[data-process-node="review-b"] [data-process-round]').textContent, '↺ 2/3', 'the track says its round');
});

test('a rule decision says what it chose, in plain words and with what it read', async (t) => {
  const { harness, mod } = await kit(t);
  const rule = {
    cases: [{ when: 'steps.ci.result == failed', then: 'fix' }, { when: 'params.merge == yes and steps.ci.result == succeeded', then: 'merge' }],
    else: 'hold',
  };
  const run = (choice) => ({
    id: 'run-rule',
    nodes: [
      { id: 'ci', label: 'CI', kind: 'task', state: 'done', performer: { kind: 'program' } },
      { id: 'gate', label: 'Merge?', kind: 'decision', state: choice ? 'done' : 'pending', performer: { kind: 'rule' }, rule: { ...rule, choice } },
    ],
    edges: [{ from: 'ci', to: 'gate', label: 'next', state: 'arrived' }],
  });
  const decided = await harness.mount(harness.html`<${mod.ProcessMiniGraph} run=${run({ case: 1, outcome: 'merge', read: { 'params.merge': 'yes', 'steps.ci.result': 'succeeded' } })} />`);
  const gate = decided.container.querySelector('[data-process-node="gate"]');
  assert.equal(gate.querySelector('[data-process-rule]').textContent, '⚙ merge');
  assert.match(gate.getAttribute('title'), /decided by its rule: merge is yes and CI succeeded → merge \(read merge = yes · CI = succeeded\)/);
  const waiting = await harness.mount(harness.html`<${mod.ProcessMiniGraph} run=${run(null)} />`);
  assert.equal(waiting.container.querySelector('[data-process-node="gate"] [data-process-rule]').textContent, '⚙ rule');
  assert.match(waiting.container.querySelector('[data-process-node="gate"]').getAttribute('title'), /CI failed → fix · merge is yes and CI succeeded → merge · otherwise → hold/);

  const { eventSentence } = await harness.importDashboardModule('js/process-oversight.js');
  const said = eventSentence({ kind: 'decision_recorded', actor: 'rule', nodeId: 'gate',
    payload: { verdict: 'fix', rule: { case: 0, when: 'steps.ci.result == failed', read: { 'steps.ci.result': 'failed' } } } }).text;
  assert.equal(said, 'the rule chose fix at gate: ci failed (read ci = failed)');
  const otherwise = eventSentence({ kind: 'decision_recorded', actor: 'rule', payload: { verdict: 'hold', rule: { case: -1 } } }).text;
  assert.equal(otherwise, 'the rule chose hold: no case held');
});
