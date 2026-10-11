import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const self = { id: 'inst_a', name: 'node-a', color: '#58a6ff', local: true };
const fleet = {
  self,
  peers: [
    { id: 'inst_forge', name: 'forge', color: '#f0883e', online: true },
    { id: 'inst_ci', name: 'ci', color: '#39c5cf', online: false },
  ],
};
const STATUS = { remote: [{ peer: 'inst_forge', groups: [{ name: 'reviewers', caps: ['agents_receive', 'teleport_receive'] }, { name: 'quiet', caps: [] }] }] };
const AGENT = { id: 'agt_b1', conv: 'conv_b1', name: 'builder-1', node: 'inst_a', nodeName: 'node-a', group: 'builders' };

function fakeTimers() {
  const queue = []; let seq = 0;
  return { queue, setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; }, clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); } };
}

test('modes, targets and what carries over say what is possible and why not', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/launch-target-dialog.js');
  assert.equal(m.TELEPORT_ROUTES, true, 'the operator teleport routes have shipped');
  assert.deepEqual(m.modeState('teleport', { local: true, teleport: false }), { ok: false, why: 'Needs the operator teleport routes, which this dashboard does not have yet.' });
  assert.equal(m.modeState('teleport', { local: true }).ok, true);
  assert.equal(m.modeState('morph', { local: true }).ok, true);
  assert.match(m.modeState('morph', { local: false, source: 'forge' }).why, /forge's dashboard/);
  assert.equal(m.modeState('move', { local: false }).ok, true, 'a peer agent can be pulled here');
  assert.equal(m.modeState('home', { local: true, presence: 'visiting', teleport: true }).ok, true, 'send home from where it runs');
  assert.equal(m.modeState('home', { local: false, presence: 'away', teleport: true }).ok, true, 'recall from its home placeholder');
  assert.equal(m.modeState('home', { local: true, presence: '', teleport: true }).why, 'It is at home.');

  const source = { node: 'inst_a', name: 'node-a' };
  const targets = m.nodeTargets({ mode: 'move', fleet, self, source, status: STATUS });
  const by = Object.fromEntries(targets.map((n) => [n.id, n]));
  assert.match(by.inst_a.why, /already here/);
  assert.deepEqual(by.inst_forge.groups, ['reviewers'], 'only groups that receive agents are offered');
  assert.equal(by.inst_ci.why, 'offline', 'an unavailable node stays listed with the reason');
  assert.equal(m.nodeTargets({ mode: 'clone', fleet, self, source, status: STATUS })[0].ok, true, 'clone here is fine');
  assert.match(m.nodeTargets({ mode: 'clone', fleet, self, source, status: STATUS, teleport: false })[1].why, /teleport routes/);
  assert.equal(m.nodeTargets({ mode: 'clone', fleet, self, source, status: STATUS })[1].ok, true, 'a peer clone goes through teleport');
  const pull = m.nodeTargets({ mode: 'move', fleet, self, source: { node: 'inst_forge', name: 'forge' }, status: STATUS, localGroups: ['builders'] });
  assert.deepEqual(pull.find((n) => n.id === 'inst_a').groups, ['builders'], 'a peer agent is pulled into this node\'s groups');

  assert.deepEqual(m.carries('teleport', { node: 'forge', home: 'node-a' }).map((r) => r[1]), ['History', 'Identity', 'Permissions', 'Here']);
  const morph = m.carries('morph', { morph: { history: { source: 'claude', target: 'codex', source_entries: 40, carried_entries: 31, items: [{ kind: 'tool_call', count: 9 }] }, after: { sandbox: 'workspace-write' } } });
  assert.match(morph[1][2], /translated claude → codex: 31 of 40 entries carried \(tool_call ×9\)/);
  assert.match(morph[2][2], /workspace-write/, 'an older daemon names only the harness mode');
  const layered = m.carries('morph', { morph: { sandbox: { tclaude: 'off', harness: 'codex danger-full-access, not applied', unsandboxed: true, warning: 'no sandbox confines the agent after this morph' } } });
  assert.deepEqual(layered.slice(2).map((row) => row[1]), ['tclaude sandbox', 'Harness sandbox', 'Unsandboxed'], 'a warning only when nothing confines it');
  assert.match(m.carries('morph', {})[1][2], /same conversation/);
});

async function mountDialog(t, props) {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/launch-target-dialog.js');
  const timers = fakeTimers(); let closed = 0; const toasts = [];
  const mounted = await harness.mount(harness.html`<${m.LaunchTargetDialog} agent=${AGENT} self=${self} fleet=${fleet} timers=${timers} now=${() => 0}
    onClose=${() => { closed += 1; }} toast=${(text, error) => toasts.push(error ? `error: ${text}` : text)} ...${props} />`);
  const q = (s) => mounted.container.querySelector(s) || harness.document.querySelector(s);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 5)));
  const click = async (el) => { await harness.act(() => el.click()); await settle(); };
  const setValue = (el, value) => Object.defineProperty(el, 'value', { configurable: true, writable: true, value });
  const change = async (el, value) => { setValue(el, value); await harness.act(() => harness.fireEvent(el, 'change')); await settle(); };
  await settle();
  return { harness, q, click, change, settle, setValue, timers, toasts, closed: () => closed };
}

test('move from the ⚙ menu: pick a node, then a receiving group, then confirm', async (t) => {
  const sent = [];
  const s = await mountDialog(t, {
    initialMode: 'move', teleport: false,
    api: { status: async () => STATUS },
    actions: { moveAgent: async (b) => { sent.push(b); return { move_id: 'mv_1', disposition: 'pending_acceptance' }; }, moveDetail: async () => ({}) },
  });
  assert.match(s.q('#launch-target').textContent, /Move builder-1 to…/);
  assert.equal(s.q('[data-lt-mode="teleport"]').disabled, true, 'teleport is greyed on a dashboard without its routes');
  assert.equal(s.q('[data-lt-node="inst_a"]').disabled, true);
  assert.equal(s.q('[data-lt-node="inst_ci"]').disabled, true);
  await s.click(s.q('[data-lt-node="inst_forge"]'));
  await s.change(s.q('#lt-group'), 'reviewers');
  assert.match(s.q('#launch-target').textContent, /to.*forge · reviewers/);
  assert.match(s.q('[data-lt-carries]').textContent, /What carries over to forge.*History/);
  await s.click(s.q('#skynet-move-drop-ok'));
  assert.deepEqual(sent, [{ agent: 'agt_b1', peer: 'inst_forge', group: 'reviewers', direct_if_allowed: true }]);
  assert.ok(s.q('[data-move-drop-state="waiting"]'));
});

test('morph previews the change with a dry run, then applies it at the next idle turn', async (t) => {
  const calls = [];
  const api = {
    status: async () => ({}),
    profiles: async () => [{ name: 'codex-high', harness: 'codex', model: 'gpt-5', effort: 'high' }, { name: 'off', disabled: true }],
    morph: async (id, body) => {
      calls.push([id, body]);
      if (body.dry_run) return { result: 'dry_run', before: { harness: 'claude', model: 'opus' }, after: { harness: 'codex', model: 'gpt-5', effort: 'high', sandbox: 'danger-full-access' }, history: { source: 'claude', target: 'codex', source_entries: 10, carried_entries: 8, context_window_tokens: 32000, context_window_source: "unknown model, assumed 32k", budget_tokens: 19200, items: [] },
        sandbox: { implementation: 'tclaude-layer', tclaude: 'on (profile strict), unchanged', harness: 'codex danger-full-access, stood down inside the tclaude sandbox' } };
      return { result: 'pending', detail: '' };
    },
  };
  const s = await mountDialog(t, { initialMode: 'morph', api, actions: {} });
  const select = s.q('#lt-morph-profile');
  assert.deepEqual([...select.options].map((o) => o.getAttribute('value')), ['', 'codex-high'], 'disabled profiles are not offered');
  assert.equal(s.q('#lt-ok').disabled, true, 'nothing to apply before a preview');
  await s.change(select, 'codex-high');
  assert.deepEqual(calls[0], ['conv_b1', { profile: 'codex-high', dry_run: true }]);
  assert.match(s.q('.lt-morph-preview').textContent, /was claude · opus.*now codex · gpt-5 · effort high/);
  assert.match(s.q('[data-lt-carries]').textContent, /translated claude → codex: 8 of 10/);
  assert.match(s.q('[data-lt-carries]').textContent, /window 32000.*unknown model, assumed 32k.*budget 19200/);
  const carried = s.q('[data-lt-carries]').textContent;
  assert.match(carried, /tclaude sandbox\s*on \(profile strict\), unchanged/, 'tclaude\'s own sandbox is named, so "danger-full-access" does not read as off');
  assert.match(carried, /Harness sandbox\s*codex danger-full-access, stood down inside the tclaude sandbox/);
  assert.doesNotMatch(carried, /Unsandboxed/, 'no warning while tclaude\'s sandbox wraps it');
  assert.equal(s.q('#lt-ok').textContent, 'Morph at next idle turn');
  await s.click(s.q('#lt-ok'));
  assert.deepEqual(calls[1], ['conv_b1', { profile: 'codex-high' }]);
  assert.equal(s.closed(), 1, 'a submitted morph closes the dialog');
  assert.match(s.toasts[0], /morphs to codex-high when it next goes idle/);
});

test('a refused morph preview says why and keeps the button off', async (t) => {
  const api = { status: async () => ({}), profiles: async () => [{ name: 'gem' }], morph: async () => { const e = new Error('x'); e.body = { error: 'gemini has no reasoning effort' }; throw e; } };
  const s = await mountDialog(t, { initialMode: 'morph', api, actions: {} });
  await s.change(s.q('#lt-morph-profile'), 'gem');
  assert.match(s.q('#launch-target [role=alert]').textContent, /no reasoning effort/);
  assert.equal(s.q('#lt-ok').disabled, true);
});

test('clone here hands over to the clone dialog', async (t) => {
  let handed = 0;
  const s = await mountDialog(t, { initialMode: 'clone', target: { node: 'inst_a', name: 'node-a', group: '' }, api: { status: async () => STATUS }, actions: {}, onLocalClone: () => { handed += 1; } });
  await s.click(s.q('[data-lt-node="inst_a"]'));
  assert.match(s.q('#launch-target').textContent, /next step picks its working directory/);
  await s.click(s.q('#lt-ok'));
  assert.equal(handed, 1);
});

test('with the teleport routes, teleport follows the move until its copy runs', async (t) => {
  const sent = [];
  const s = await mountDialog(t, {
    initialMode: 'teleport', teleport: true,
    api: { status: async () => STATUS, teleport: async (id, b) => { sent.push([id, b]); return { move_id: 'mv_9', teleport_state: 'awaiting_running_confirmation' }; } },
    actions: { moveDetail: async () => ({ state: 'confirmed', disposition: 'landed', target_agent: 'agt_b1' }) },
  });
  await s.click(s.q('[data-lt-node="inst_forge"]'));
  await s.change(s.q('#lt-group'), 'reviewers');
  assert.match(s.q('.lt-consequence').textContent, /leaves at once: it keeps running here until its copy runs there/, 'no promise of waiting for idle');
  const n = s.q('#lt-note'); s.setValue(n, 'review 1.1'); await s.harness.act(() => s.harness.fireEvent(n, 'input'));
  await s.click(s.q('#lt-ok'));
  assert.deepEqual(sent, [['conv_b1', { peer: 'inst_forge', group: 'reviewers', note: 'review 1.1' }]]);
  assert.equal(s.closed(), 1, 'a sent teleport closes the dialog');
  assert.match(s.toasts[0], /Teleport to forge: sent/);
  await s.harness.act(async () => { await s.timers.queue.shift().fn(); }); await s.settle();
  assert.match(s.toasts[1], /builder-1 runs on forge/, 'the outcome still arrives once the dialog is gone');
});

test('recall from its home placeholder sends it straight home', async (t) => {
  const recalled = [];
  const r = await mountDialog(t, {
    initialMode: 'home', teleport: true,
    agent: { ...AGENT, node: 'inst_forge', nodeName: 'forge', presence: 'away', homeNode: 'inst_a', homeName: 'node-a' },
    api: { status: async () => ({}), recall: async (id) => { recalled.push(id); return { state: 'returning' }; } },
    actions: {},
  });
  assert.match(r.q('#launch-target-title').textContent, /Recall builder-1 home to node-a\?/);
  assert.match(r.q('.lt-consequence').textContent, /straight home to node-a, not back through the nodes it visited/);
  await r.click(r.q('#lt-ok'));
  assert.deepEqual(recalled, ['agt_b1']);
  assert.match(r.toasts[0], /returning to its paused copy on node-a/, 'a paused-backup return has no move record to follow');
  assert.equal(r.closed(), 1);
});

test('a peer agent opens on a mode it can use, and a sent move locks the target', async (t) => {
  const peerAgent = { ...AGENT, node: 'inst_forge', nodeName: 'forge' };
  const s = await mountDialog(t, {
    agent: peerAgent, initialMode: 'morph', localGroups: ['builders'],
    api: { status: async () => ({}) },
    peerActions: () => ({ moveDirect: async () => ({ move_id: 'mv_p', disposition: 'checking' }) }),
    actions: { moveDetail: async () => ({ state: 'awaiting_running', disposition: 'checking' }) },
  });
  assert.equal(s.q('[data-lt-mode="move"]').getAttribute('aria-selected'), 'true', 'morph is not possible from here, so it opens on Move');
  assert.equal(s.q('#lt-morph-profile'), null);
  assert.equal(s.q('[data-lt-mode="morph"]').disabled, true);
  await s.click(s.q('[data-lt-node="inst_a"]'));
  await s.change(s.q('#lt-group'), 'builders');
  await s.click(s.q('#skynet-move-drop-ok'));
  assert.equal(s.q('#lt-group').disabled, true, 'the group is frozen once the move is sent');
  assert.equal(s.q('[data-lt-node="inst_forge"]').disabled, true);
  assert.equal(s.q('[data-lt-mode="clone"]').disabled, true);
});
