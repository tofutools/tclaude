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
  assert.equal(root.querySelectorAll('.pk-sheet-box').length, 5);
  assert.equal(root.querySelectorAll('.pk-mini-graph').length, 2);
  assert.match(root.textContent, /Familiar rune/);
  assert.match(root.textContent, /familiar-bound · search-indexer/);
  await mounted.unmount();
});
