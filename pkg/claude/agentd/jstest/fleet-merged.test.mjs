import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const self = { id: 'inst_self', name: 'desk', color: '#58a6ff', local: true };
const forge = { id: 'inst_forge', name: 'forge', color: '#f0883e', local: false };
const lab = { id: 'inst_lab', name: 'lab', color: '#39c5cf', local: false };
const group = (name, extra = {}) => ({ name, members: [], online: 0, ...extra });

test('merge names groups group@node, keeps parents per node, and marks stale peers', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-merged-model.js');
  const merged = m.mergeSnapshots([
    { node: self, entry: { snapshot: { group_attachments_mode: 'fixed', groups: [group('ops'), group('ops-sub', { parent: 'ops' })], agents: [{ agent_id: 'agt_1' }], links: [{ id: 7, from: 'ops', to: 'ops-sub', mode: 'send' }] }, receivedAt: 1000 } },
    { node: forge, entry: { snapshot: { groups: [group('ops')], agents: [{ agent_id: 'agt_2' }], pending: [{ label: 'stuck-1', group: 'ops' }, { label: 'loose' }], queued_spawns: [{ id: 3, group: 'ops', draft: {} }], peer_view: { included: [] } }, receivedAt: 1000 } },
    { node: lab, entry: { snapshot: { groups: [group('build')] }, receivedAt: 0, failure: { status: 502, code: 'peer_unreachable' } } },
  ], 61000);
  assert.deepEqual(merged.groups.map((g) => g.name), ['ops@desk', 'ops-sub@desk', 'ops@forge', 'build@lab']);
  assert.equal(merged.groups[1].parent, 'ops@desk');
  assert.equal(merged.group_attachments_mode, 'fixed', 'presentation settings come from this node');
  assert.equal(merged.peer_view, undefined);
  assert.deepEqual(merged.agents.map((a) => a.agent_id), ['agt_1', 'agt_2']);
  assert.deepEqual(merged.pending, [{ label: 'stuck-1', group: 'ops@forge' }], 'a pending spawn sits in its node\'s group; one without a group stays on its node');
  assert.deepEqual(merged.queued_spawns, [{ id: 3, group: 'ops@forge', own_group: 'ops', fleet_node: 'inst_forge', fleet_node_name: 'forge', draft: {} }], 'a queued spawn heads its node\'s group and names its node');
  assert.deepEqual(merged.links, [{ id: 7, from: 'ops@desk', to: 'ops-sub@desk', mode: 'send' }], 'a node\'s links join its groups under their merged names');
  const labGroup = merged.groups[3];
  assert.equal(labGroup.fleet_node.stale, true); assert.equal(labGroup.fleet_node.label, 'stale · data 1 min old');
  assert.equal(merged.groups[2].fleet_node.stale, true, 'a minute-old snapshot is stale even without a failure');
  assert.equal(merged.groups[0].fleet_node.stale, false, 'this node is never stale');
  assert.deepEqual(m.mergeSnapshots([{ node: forge, entry: null }]).fleet_nodes[0].loaded, false);
  const offline = m.mergeSnapshots([{ node: { ...forge, online: false }, entry: null }]).fleet_nodes[0];
  assert.equal(offline.label, 'offline', 'a peer the hub reports offline is not "loading…"');
  assert.equal(offline.stale, true);
  assert.equal(m.mergeSnapshots([{ node: { ...forge, online: false }, entry: { failure: { status: 502, code: 'peer_unreachable' } } }]).fleet_nodes[0].label, 'offline', 'a failed poll keeps it offline');
});

test('node names that collide (self-reported or containing @) never merge two nodes\' groups', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-merged-model.js');
  const fakeDesk = { id: 'inst_imposter9', name: 'desk', color: '#f00', local: false };
  const ab = { id: 'inst_abnode77', name: 'a@b', color: '#0f0', local: false };
  const merged = m.mergeSnapshots([
    { node: { ...self, name: 'b' }, entry: { snapshot: { groups: [group('ops@a')] }, receivedAt: 0 } },
    { node: { ...forge, name: 'desk' }, entry: { snapshot: { groups: [group('ops')] }, receivedAt: 0 } },
    { node: fakeDesk, entry: { snapshot: { groups: [group('ops')] }, receivedAt: 0 } },
    { node: ab, entry: { snapshot: { groups: [group('ops')] }, receivedAt: 0 } },
  ], 0);
  const names = merged.groups.map((g) => g.name);
  assert.equal(new Set(names).size, names.length, `unique names: ${names.join(', ')}`);
  assert.deepEqual(names, ['ops@a@b', 'ops@desk~forge', 'ops@desk~impost', 'ops@a_b']);
});

function fakeTimers() {
  const queue = []; let seq = 0;
  return { queue, setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; }, clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); } };
}

test('the merged view polls peers only while shown, renders group@node, and guards controls', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js')]);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createFleetState({ activeTab, search: '' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }] });
  const snapshot = harness.signals.signal({ groups: [group('ops')], agents: [] });
  const timers = fakeTimers(); const calls = []; const toasts = []; const switched = []; const opened = [];
  const inits = [];
  const fetchImpl = async (url, init) => { calls.push(url); inits.push(init); return { ok: true, status: 200, json: async () => ({ groups: [group('build')], agents: [], peer_view: { included: ['agents.status'] } }) }; };
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" toast=${(m) => toasts.push(m)} switchNode=${(id) => switched.push(id)} openTerminal=${(o) => opened.push(o)} />`);
  assert.equal(timers.queue.length, 0, 'one node shown: polls nothing');
  await harness.act(() => { state.setFused('all'); });
  assert.equal(timers.queue.length, 1);
  await harness.act(async () => { await timers.queue.shift().fn(); });
  assert.deepEqual(calls, ['/api/peer/inst_forge/snapshot']);
  assert.equal(inits[0].headers['X-Tclaude-Poll-Ms'], '5000', 'a peer poll declares its interval, so the daemon keeps it warm');
  const text = mounted.container.textContent;
  assert.match(text, /ops@desk/); assert.match(text, /build@forge/);
  const suffix = [...mounted.container.querySelectorAll('[data-fleet-open]')].find((el) => el.textContent === '@forge');
  assert.ok(suffix, 'the @node suffix is a jump to that node');
  // Read-only: the status dot (a power control), keyboard activation and the
  // row context menu are stopped; view-only controls pass.
  const el = (html) => { const d = harness.document.createElement('div'); d.innerHTML = html; host.appendChild(d); return d.firstElementChild; };
  const dot = el('<span data-act="dot-toggle" data-agent="agt_1">●</span>');
  const chip = el('<span data-act="set-group-descr" role="button" tabindex="0">📝</span>');
  const menu = el('<button data-act="group-menu">⚙</button>');
  assert.equal(harness.fireEvent(dot, 'click').defaultPrevented, true, 'the dot wakes or shuts down agents: blocked');
  assert.equal(harness.fireEvent(chip, 'keydown', { key: 'Enter' }).defaultPrevented, true);
  assert.equal(harness.fireEvent(chip, 'contextmenu').defaultPrevented, true);
  assert.equal(harness.fireEvent(menu, 'click').defaultPrevented, false, 'menus still open');
  assert.ok(toasts.length >= 2);
  // A peer agent's terminal opens in the browser; this node's goes to its usual handler.
  const inGroup = (node, name, inner) => { const d = el(`<details data-fleet-node="${node}" data-fleet-node-name="${name}">${inner}</details>`); return d.firstElementChild; };
  const peerWin = inGroup('inst_forge', 'forge', '<button data-act="web-open-window" data-agent="agt_ada1">web window</button>');
  assert.equal(harness.fireEvent(peerWin, 'click').defaultPrevented, true);
  assert.deepEqual({ instance: opened[0].instance, agent: opened[0].agent, peerLabel: opened[0].peerLabel }, { instance: 'inst_forge', agent: 'agt_ada1', peerLabel: 'forge' });
  const localWin = inGroup('inst_self', 'desk', '<button data-act="web-open-window" data-agent="agt_me1">web window</button>');
  assert.equal(harness.fireEvent(localWin, 'click').defaultPrevented, false, "this node's rows run as on its own view");
  assert.equal(opened.length, 1);
  // Ctrl/Cmd opens a peer's terminal in the background, by click or by the
  // context-menu event macOS turns a Ctrl-click into.
  harness.fireEvent(peerWin, 'click', { ctrlKey: true });
  assert.equal(opened[1].background, true);
  assert.equal(harness.fireEvent(peerWin, 'contextmenu', { ctrlKey: true }).defaultPrevented, true);
  assert.equal(opened[2]?.background, true, 'a Ctrl-click context menu still opens it');
  assert.equal(opened[0].background, undefined, 'a plain click reveals it');
  await harness.act(() => { activeTab.value = 'usage'; });
  assert.equal(timers.queue.length, 0, 'leaving the view cancels polling');
  await mounted.unmount(); state.dispose();
});

test('a peer row runs what the peer grants through its own handlers or peer routes; this node\'s rows act directly; unshared controls are stopped', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js')]);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createFleetState({ activeTab, search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }] });
  const snapshot = harness.signals.signal({ groups: [group('ops')], agents: [] });
  const timers = fakeTimers(); const toasts = []; const calls = []; const confirms = [];
  const spawns = [];
  const peerSnap = { groups: [group('build', { members: [{ agent_id: 'agt_ada1', title: 'ada' }] })], agents: [], peer_view: { included: ['agents.status', 'lifecycle.resume', 'spawn', 'messaging'], agents: { agt_ada1: ['groups.members.resume'] } } };
  const fetchImpl = async () => ({ ok: true, status: 200, json: async () => peerSnap });
  const peerActions = (node) => ({
    resume: async (id) => { calls.push([node, 'resume', id]); return {}; },
    message: async (to, o) => { calls.push([node, 'message', to, o.body]); return {}; },
  });
  const confirm = async (o) => { confirms.push(o); return o.action(); };
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" toast=${(m) => toasts.push(m)} switchNode=${() => {}} peerActions=${peerActions} confirm=${confirm} openSpawn=${async (o) => { spawns.push(o); }} />`);
  await harness.act(async () => { await timers.queue.shift().fn(); });
  const doc = harness.document;
  const inGroup = (node, name, inner) => { const d = doc.createElement('div'); d.innerHTML = `<details data-fleet-node="${node}" data-fleet-node-name="${name}">${inner}</details>`; host.appendChild(d); return d.firstElementChild.firstElementChild; };
  // A stopped agent's dot wakes it on forge: the click reaches the regular
  // row handler (which addresses forge), not a peer dialog.
  const dot = inGroup('inst_forge', 'forge', '<span data-act="dot-toggle" data-online="0" data-agent="agt_ada1" data-label="ada">○</span>');
  assert.equal(harness.fireEvent(dot, 'click').defaultPrevented, false);
  assert.equal(doc.querySelector('#peer-action-modal'), null);
  // Spawn and message carry the peer's own group name, not group@node.
  const spawn = inGroup('inst_forge', 'forge', '<button data-act="spawn-agent" data-group="build@forge">+</button>');
  await harness.act(() => harness.fireEvent(spawn, 'click'));
  const opened = spawns.shift();
  assert.equal(opened.nodeId, 'inst_forge');
  assert.equal(opened.node, 'forge');
  assert.equal(opened.groupName, 'build', 'the regular spawn dialog opens on forge\'s own group name');
  assert.deepEqual(opened.snapshot.groups.map((g) => g.name), ['build'], 'and on forge\'s snapshot, not this node\'s');
  const msg = inGroup('inst_forge', 'forge', `<button data-act="message-new" data-prefill='{"targetMode":"group","groupName":"build@forge"}'>✉</button>`);
  await harness.act(() => harness.fireEvent(msg, 'click'));
  const body = doc.querySelector('#peer-message-body');
  body.value = 'hello';
  await harness.act(() => harness.fireEvent(body, 'input'));
  await harness.act(() => doc.querySelector('#peer-message-send').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  assert.deepEqual(calls.shift(), ['inst_forge', 'message', 'agt_ada1', 'hello']);
  // Unshared (stop) is stopped with a toast; this node's rows reach their own handlers.
  const before = toasts.length;
  const running = inGroup('inst_forge', 'forge', '<span data-act="dot-toggle" data-online="1" data-agent="agt_ada1">●</span>');
  const local = inGroup('inst_self', 'desk', '<span data-act="dot-toggle" data-online="0" data-agent="agt_me1">○</span>');
  const localSpawn = inGroup('inst_self', 'desk', '<button data-act="spawn-agent" data-group="ops@desk">+</button>');
  assert.equal(harness.fireEvent(running, 'click').defaultPrevented, true);
  assert.match(toasts.at(-1), /forge does not share this action with you \(needs groups\.members\.stop\)/);
  // Hover or focus puts the same reason on it, disabled for assistive tech;
  // what runs from here keeps its own title.
  harness.fireEvent(running, 'pointerover');
  assert.match(running.getAttribute('title'), /forge does not share this action with you \(needs groups\.members\.stop\)/);
  assert.equal(running.getAttribute('aria-disabled'), 'true');
  harness.fireEvent(dot, 'focusin');
  assert.equal(dot.getAttribute('aria-disabled'), null, 'a granted wake is not disabled');
  assert.equal(harness.fireEvent(local, 'click').defaultPrevented, false);
  assert.equal(harness.fireEvent(localSpawn, 'click').defaultPrevented, false);
  assert.equal(doc.querySelector('#peer-action-modal'), null);
  assert.equal(toasts.length, before + 1);
  assert.equal(calls.length, 0);
  // The greying is generated from the same rules, per node.
  const css = doc.querySelector('#fleet-merged-limits').textContent;
  const forgeRule = css.split('\n').find((r) => r.includes('details[data-fleet-node="inst_forge"]'));
  assert.match(forgeRule, /\[data-act="dot-toggle"\]:not\(\[data-online="1"\]\)\[data-agent="agt_ada1"\]/, 'wake is granted on ada');
  assert.match(forgeRule, /\[data-act="spawn-agent"\]/);
  assert.match(forgeRule, /\[data-act="jump"\]/, 'its terminal opens here');
  assert.doesNotMatch(forgeRule, /\[data-act="dot-toggle"\]\[data-online="1"\]|\[data-act="restart"\]/, 'stop and restart are not granted');
  assert.ok(!css.includes('details[data-fleet-node="inst_self"]'), 'this node\'s own groups act directly here: nothing greyed');
  await mounted.unmount(); state.dispose();
});

test('fused Groups shows each node\'s triggers in its own groups, read from nodes that let it', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js')]);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createFleetState({ activeTab, search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }, { instance_id: 'inst_lab', label: 'lab', trusted: true, online: true }] });
  const snapshot = harness.signals.signal({ groups: [group('ops')], agents: [], triggers_enabled: true });
  const rules = {
    self: [{ id: 1, name: 'desk-global', scope: 'global', source: 'pr.opened', enabled: true }],
    forge: [{ id: 1, name: 'forge-ops', scope: 'group', group: 'ops', source: 'pr.merged', enabled: true }],
  };
  const peers = {
    forge: { groups: [group('ops')], agents: [], triggers_enabled: true, peer_view: { level: 'unrestricted' } },
    lab: { groups: [group('qa')], agents: [], triggers_enabled: true, peer_view: { included: [] } },
  };
  const asked = [];
  const fetchImpl = async (url) => {
    const node = url.includes('inst_forge') ? 'forge' : url.includes('inst_lab') ? 'lab' : 'self';
    if (url.includes('/firings')) return { ok: true, status: 200, json: async () => ({ firings: [] }) };
    if (/\/triggers(\?|$)/.test(url)) { asked.push(node); return { ok: true, status: 200, json: async () => ({ triggers: rules[node] || [] }) }; }
    return { ok: true, status: 200, json: async () => peers[node] };
  };
  const timers = fakeTimers();
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" toast=${() => {}} switchNode=${() => {}} />`, host);
  for (const first of timers.queue.splice(0)) await harness.act(async () => { await first.fn(); });
  await harness.act(() => new Promise((r) => setTimeout(r, 5)));
  const names = (key) => [...(mounted.container.querySelector(`details[data-group-key="${key}"]`)?.querySelectorAll('.group-triggers-section .rowname') || [])].map((n) => n.textContent);
  assert.deepEqual(names('ops@desk'), ['desk-global'], 'this node\'s global rule, not forge\'s group rule of the same name');
  assert.deepEqual(names('ops@forge'), ['forge-ops'], 'forge\'s rule on its own ops');
  assert.deepEqual(names('qa@lab'), [], 'a restricted peer\'s triggers are not read');
  assert.deepEqual(asked.sort(), ['forge', 'self'], 'each node is read once; lab is never asked');
  await mounted.unmount(); state.dispose();
});

test('limitsCSS greys per node; a peer page keeps this node\'s terminals refused and skips odd node IDs', async (t) => {
  const harness = await createPreactHarness(t);
  const island = await harness.importDashboardModule('js/fleet-merged-island.js');
  const css = island.limitsCSS([
    { id: 'inst_self', peerView: null },
    { id: 'inst_forge', peerView: { included: ['agents.status'], agents: { agt_a: ['groups.members.stop', 'groups.members.resume'] } } },
    { id: 'inst_x"]{}*{', peerView: null },
  ], { self: 'inst_self', home: '' });
  const lines = css.split('\n');
  assert.equal(lines.length, 3, 'outside-any-group, this node, forge; the odd ID gets no rule');
  assert.match(lines[0], /\[data-act\]:not\(details\[data-fleet-node\] \[data-act\]\):not\(.*\[data-act="send-elsewhere"\]/);
  assert.doesNotMatch(lines[1], /"jump"/, 'on a peer page this node\'s terminal is not opened from here');
  assert.match(lines[2], /\[data-act="dot-toggle"\]\[data-online="1"\]\[data-agent="agt_a"\]/);
  assert.match(lines[2], /\[data-act="restart"\]\[data-agent="agt_a"\]/);
  for (const l of lines) assert.match(l, /\{ opacity: \.45; cursor: not-allowed; \}$/);
});

test('limitsCSS greys exactly the controls a click refuses', async (t) => {
  const harness = await createPreactHarness(t);
  const island = await harness.importDashboardModule('js/fleet-merged-island.js');
  const css = island.limitsCSS([
    { id: 'inst_self', peerView: null },
    { id: 'inst_forge', peerView: { included: ['agents.status', 'spawn'], agents: { agt_a: ['groups.members.resume'] } } },
  ], { self: 'inst_self', home: 'inst_self' });
  const doc = harness.document;
  doc.body.innerHTML = `<div id="fleetview-fused-groups-root"><div class="fleet-merged">
    <details data-fleet-node="inst_self"><summary><b id="s-jump" data-act="jump"></b><b id="s-spawn" data-act="spawn-agent"></b><b id="s-menu" data-act="group-menu"></b></summary>
      <details data-fleet-node="inst_self"><summary><b id="s-sub-jump" data-act="web-open-window"></b></summary></details></details>
    <details data-fleet-node="inst_forge"><summary><b id="f-jump" data-act="jump"></b><b id="f-spawn" data-act="spawn-agent"></b><b id="f-wake" data-act="dot-toggle" data-online="0" data-agent="agt_a"></b>
      <b id="f-stop" data-act="dot-toggle" data-online="1" data-agent="agt_a"></b><b id="f-restart" data-act="restart" data-agent="agt_a"></b><b id="f-send" data-act="send-elsewhere"></b></summary></details>
    <b id="loose" data-act="jump"></b><b id="loose-send" data-act="send-elsewhere"></b></div></div>`;
  const selectors = css.split('\n').map((r) => r.replace(/ \{[^}]*\}$/, ''));
  const greyed = (id) => selectors.some((sel) => doc.getElementById(id).matches(sel));
  for (const id of ['s-jump', 's-spawn', 's-menu', 's-sub-jump', 'f-jump', 'f-spawn', 'f-wake', 'f-send', 'loose-send']) assert.equal(greyed(id), false, `${id} is usable`);
  for (const id of ['f-stop', 'f-restart', 'loose']) assert.equal(greyed(id), true, `${id} is refused`);
});

test('limitsCSS greys an unrestricted peer\'s controls that only its own dashboard runs', async (t) => {
  const harness = await createPreactHarness(t);
  const island = await harness.importDashboardModule('js/fleet-merged-island.js');
  const css = island.limitsCSS([
    { id: 'inst_self', peerView: null },
    { id: 'inst_forge', peerView: { level: 'unrestricted' } },
  ], { self: 'inst_self', home: 'inst_self' });
  const doc = harness.document;
  doc.body.innerHTML = `<div id="fleetview-fused-groups-root"><div class="fleet-merged">
    <details data-fleet-node="inst_forge"><summary><b id="sub" data-act="create-subgroup" data-group="build@forge"></b><b id="pend" data-act="delete-pending" data-group="build@forge"></b>
      <b id="restart" data-act="restart" data-agent="agt_a"></b><b id="export" data-act="export-group" data-group="build@forge"></b><b id="links" data-act="links-manage"></b></summary></details></div></div>`;
  const selectors = css.split('\n').map((r) => r.replace(/ \{[^}]*\}$/, ''));
  const greyed = (id) => selectors.some((sel) => doc.getElementById(id).matches(sel));
  for (const id of ['sub', 'pend', 'restart', 'export']) assert.equal(greyed(id), false, `${id} runs on forge from here`);
  assert.equal(greyed('links'), true, "links are forge's own dashboard's, and look it");
});

test('dragging an agent onto another node\'s group opens the move; a same-node drop is the Groups drag-and-drop\'s', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js')]);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createFleetState({ activeTab, search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }, { instance_id: 'inst_lab', label: 'lab', trusted: true, online: true }] });
  const snapshot = harness.signals.signal({ groups: [group('builders', { members: [{ agent_id: 'agt_b1', conv_id: 'conv_b1', title: 'builder-1' }] }), group('other')], agents: [] });
  const peerSnap = { forge: { groups: [group('reviewers', { members: [{ agent_id: 'agt_r1', conv_id: 'conv_r1', title: 'reviewer-1' }] })], agents: [],
    peer_view: { level: 'restricted', included: ['agents.status'], agents: { agt_r1: ['agent.move'] } } }, lab: { groups: [group('qa')], agents: [] } };
  const fetchImpl = async (url) => ({ ok: true, status: 200, json: async () => (url.includes('inst_forge') ? peerSnap.forge : peerSnap.lab) });
  const timers = fakeTimers(); const toasts = [];
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const moveActions = { moveAgent: async () => ({ move_id: 'mv', disposition: 'pending_acceptance' }), moveDetail: async () => ({}) };
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" toast=${(m) => toasts.push(m)} switchNode=${() => {}} moveActions=${moveActions} />`, host);
  for (const first of timers.queue.splice(0)) await harness.act(async () => { await first.fn(); });
  const q = (s) => mounted.container.querySelector(s);
  const groupEl = (key) => q(`details[data-group-key="${key}"]`);
  const row = (agent) => q(`tr.dnd-draggable[data-dnd-agent="${agent}"]`);
  const drag = async (from, to) => {
    assert.equal(harness.fireEvent(from, 'dragstart').defaultPrevented, false, 'an agent row drags');
    const over = harness.fireEvent(to.querySelector('summary'), 'dragover');
    await harness.act(() => harness.fireEvent(to.querySelector('summary'), 'drop'));
    return over;
  };
  assert.ok(row('agt_b1') && groupEl('reviewers@forge'), 'rows and groups render');
  assert.equal(harness.fireEvent(groupEl('builders@desk').querySelector('summary'), 'dragstart').defaultPrevented, false, "this node's group headers drag (group-reorder.js)");
  assert.equal(harness.fireEvent(groupEl('reviewers@forge').querySelector('summary'), 'dragstart').defaultPrevented, true, 'a peer trusting this node restricted keeps its groups');
  // A pending or queued spawn row drags to the bin (dnd.js) on a node this
  // one acts on.
  const spawnRow = (key) => {
    const table = harness.document.createElement('table');
    table.innerHTML = '<tbody><tr class="dnd-draggable" data-dnd-pending="1" data-dnd-queued="7"><td>q</td></tr></tbody>';
    groupEl(key).appendChild(table);
    return table.querySelector('tr');
  };
  assert.equal(harness.fireEvent(spawnRow('builders@desk'), 'dragstart').defaultPrevented, false, "this node's queued spawn drags to the bin");
  assert.equal(harness.fireEvent(spawnRow('reviewers@forge'), 'dragstart').defaultPrevented, true, 'a restricted peer\'s does not');
  // dnd.js's pending Ctrl/Cmd clone hover must not finish under a move
  // between nodes this view takes.
  const dndOps = await harness.importDashboardModule('js/dnd-ops.js');
  let cancelled = 0;
  dndOps.dndGesture.cancelClone = () => { cancelled += 1; };
  const over = await drag(row('agt_b1'), groupEl('reviewers@forge'));
  assert.ok(cancelled >= 2, 'its dragover and drop cancel the clone hover');
  assert.equal(over.defaultPrevented, true, 'another node\'s group accepts the drop');
  const dialog = harness.document.querySelector('#launch-target');
  assert.ok(dialog, 'the drop opens the launch-target dialog');
  assert.match(dialog.textContent, /Move builder-1 to…/);
  assert.match(dialog.textContent, /Opened by dragging builder-1 onto reviewers@forge/);
  assert.match(dialog.textContent, /from.*desk · builders.*to.*forge · reviewers/, 'node and group are pre-filled from the drop');
  assert.equal(harness.document.activeElement?.id, 'fleetview-move-drop-ok', 'Enter confirms the move');
  await harness.act(() => harness.document.querySelector('#fleetview-move-drop-cancel').click());
  await drag(row('agt_r1'), groupEl('qa@lab'));
  assert.match(toasts.at(-1), /Neither forge nor lab is this node: move reviewer-1 from forge's dashboard/);
  // A drop on the agent's own node is the Groups drag-and-drop's (dnd.js):
  // this view lets it through, saying nothing.
  const before = toasts.length;
  const same = await drag(row('agt_b1'), groupEl('other@desk'));
  assert.equal(same.defaultPrevented, false, "this view leaves a same-node drop to dnd.js");
  assert.equal(toasts.length, before);
  await drag(row('agt_r1'), groupEl('builders@desk'));
  assert.match(toasts.at(-1), /forge does not let you bring reviewer-1 here \(needs groups\.members\.retire\)/, 'the drop names the missing grant');
  assert.equal(harness.document.querySelector('#launch-target'), null);
  await mounted.unmount(); state.dispose();
});

test('an unrestricted peer\'s agent dropped on a third node opens the move it sends on', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js')]);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createFleetState({ activeTab, search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }, { instance_id: 'inst_lab', label: 'lab', trusted: true, online: true }] });
  const snapshot = harness.signals.signal({ groups: [group('builders', { members: [{ agent_id: 'agt_b1', conv_id: 'conv_b1', title: 'builder-1' }] }), group('other')], agents: [] });
  const peerSnap = { forge: { peer_view: { level: 'unrestricted' }, groups: [group('reviewers', { members: [{ agent_id: 'agt_r1', conv_id: 'conv_r1', title: 'reviewer-1' }] })], agents: [] }, lab: { groups: [group('qa')], agents: [] } };
  const fetchImpl = async (url) => ({ ok: true, status: 200, json: async () => (url.includes('inst_forge') ? peerSnap.forge : peerSnap.lab) });
  const timers = fakeTimers(); const toasts = [];
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const moveActions = { moveAgent: async () => ({ move_id: 'mv', disposition: 'pending_acceptance' }), moveDetail: async () => ({}) };
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" toast=${(m) => toasts.push(m)} switchNode=${() => {}} moveActions=${moveActions} />`, host);
  for (const first of timers.queue.splice(0)) await harness.act(async () => { await first.fn(); });
  const q = (s) => mounted.container.querySelector(s);
  const groupEl = (key) => q(`details[data-group-key="${key}"]`);
  const row = (agent) => q(`tr.dnd-draggable[data-dnd-agent="${agent}"]`);
  const drag = async (from, to) => {
    assert.equal(harness.fireEvent(from, 'dragstart').defaultPrevented, false, 'an agent row drags');
    const over = harness.fireEvent(to.querySelector('summary'), 'dragover');
    await harness.act(() => harness.fireEvent(to.querySelector('summary'), 'drop'));
    return over;
  };
  const over = await drag(row('agt_r1'), groupEl('qa@lab'));
  assert.equal(over.defaultPrevented, true, 'the third node\'s group accepts the drop');
  const dialog = harness.document.querySelector('#launch-target');
  assert.ok(dialog, 'the drop opens the launch-target dialog');
  assert.match(dialog.textContent, /forge sends reviewer-1 .*to group qa on lab/);
  assert.match(dialog.textContent, /from.*forge · reviewers.*to.*lab · qa/);
  assert.equal(toasts.length, 0);
  await mounted.unmount(); state.dispose();
});

test('fused Groups shows only the ticked nodes; on a peer\'s page that peer is the page snapshot and this node is read directly', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js')]);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createFleetState({ activeTab, search: '?node=inst_forge&nodes=inst_self,inst_forge' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }, { instance_id: 'inst_lab', label: 'lab', trusted: true, online: true }] });
  // The page shows forge (?node=): its snapshot is the page's.
  const snapshot = harness.signals.signal({ groups: [group('build')], agents: [] });
  const timers = fakeTimers(); const proxied = []; const local = [];
  const fetchImpl = async (url) => { proxied.push(url); return { ok: true, status: 200, json: async () => ({ groups: [], agents: [] }) }; };
  const localFetch = async (url) => { local.push(url); return { ok: true, status: 200, json: async () => ({ groups: [group('ops')], agents: [] }) }; };
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} localFetch=${localFetch} timers=${timers} remote="inst_forge" />`);
  assert.equal(timers.queue.length, 1, 'only this node is read: forge is the page, lab is not ticked');
  await harness.act(async () => { await timers.queue.shift().fn(); });
  assert.deepEqual(local, ['/api/snapshot']); assert.deepEqual(proxied, []);
  const text = mounted.container.textContent;
  assert.match(text, /ops@desk/); assert.match(text, /build@forge/); assert.doesNotMatch(text, /@lab/);
  await mounted.unmount(); state.dispose();
});

test('a fused peer keeps its last groups on a failed read, and loses them with trust', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js')]);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }] });
  const timers = fakeTimers();
  let reply = { ok: true, status: 200, json: async () => ({ groups: [group('build')], agents: [] }) };
  const fetchImpl = async () => reply;
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${harness.signals.signal({ groups: [group('ops')], agents: [] })} fetchImpl=${fetchImpl} timers=${timers} remote="" />`);
  const text = () => mounted.container.textContent;
  await harness.act(async () => { await timers.queue.shift().fn(); });
  assert.match(text(), /build@forge/);
  reply = { ok: false, status: 502, json: async () => ({ code: 'peer_unreachable', reason: 'peer_offline' }) };
  await harness.act(async () => { await timers.queue.shift().fn(); });
  assert.match(text(), /build@forge/, 'an unreachable peer keeps its last rows');
  reply = { ok: false, status: 403, json: async () => ({ code: 'not_trusted' }) };
  await harness.act(async () => { await timers.queue.shift().fn(); });
  assert.doesNotMatch(text(), /build@forge/, 'lost trust drops what the peer shared');
  assert.match(text(), /ops@desk/);
  await mounted.unmount(); state.dispose();
});

test('a wide fused view keeps at most four snapshot reads in flight', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js')]);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '?nodes=all' });
  const ids = ['a', 'b', 'c', 'd', 'e', 'f'].map((x) => `inst_${x}0000`);
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: ids.map((id) => ({ instance_id: id, label: id.slice(5, 6), trusted: true, online: true })) });
  const timers = fakeTimers(); let open = 0; let peak = 0; const release = [];
  const fetchImpl = () => { open += 1; peak = Math.max(peak, open); return new Promise((r) => release.push(() => { open -= 1; r({ ok: true, status: 200, json: async () => ({ groups: [], agents: [] }) }); })); };
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${harness.signals.signal({ groups: [], agents: [] })} fetchImpl=${fetchImpl} timers=${timers} remote="" />`);
  const ticks = timers.queue.splice(0);
  ticks.forEach((q) => { q.fn(); });
  assert.equal(peak, island.MAX_IN_FLIGHT);
  assert.equal(timers.queue.length, ids.length - island.MAX_IN_FLIGHT, 'the rest wait their turn');
  await harness.act(async () => { release.splice(0).forEach((r) => r()); await new Promise((r) => setTimeout(r, 5)); });
  await mounted.unmount(); state.dispose();
});

test("this node's groups run the Groups actions under their own names; a peer's do nothing", async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island, rows] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js'), harness.importDashboardModule('js/fleet-merged-model.js')]);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }] });
  const timers = fakeTimers(); const calls = [];
  const ops = group('ops');
  const live = { openAddMember: (g) => { calls.push(['add', g]); }, renameGroup: async (g, v) => { calls.push(['rename', g.name, v]); } };
  const fetchImpl = async () => ({ ok: true, status: 200, json: async () => ({ groups: [group('build')], agents: [], peer_view: { included: [] } }) });
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${harness.signals.signal({ groups: [ops], agents: [] })} fetchImpl=${fetchImpl} timers=${timers} remote="" groupsActions=${() => live} />`);
  await harness.act(async () => { await timers.queue.shift().fn(); });
  const homes = [...mounted.container.querySelectorAll('details[data-fleet-home]')].map((d) => d.dataset.groupKey);
  assert.deepEqual(homes, ['ops@desk'], "only this node's groups are home");

  const routed = island.homeActions(() => live, (g) => (g.name === 'ops@desk' ? ops : null));
  routed.openAddMember({ name: 'ops@desk', fleet_node: { id: 'inst_self', home: true } });
  await routed.renameGroup({ name: 'build@forge', fleet_node: { id: 'inst_forge' } }, 'x');
  assert.deepEqual(calls, [['add', ops]], "a home group's action reaches this node with its own group; a peer's is a no-op");

  assert.deepEqual(rows.ownGroupData({ act: 'spawn-agent', group: 'ops@desk', label: 'ops@desk', agent: 'agt_1', prefill: '{"targetMode":"group","groupName":"ops@desk","scopeGroup":"ops@desk"}' }, 'desk'),
    { act: 'spawn-agent', group: 'ops', label: 'ops', agent: 'agt_1', prefill: '{"targetMode":"group","groupName":"ops","scopeGroup":"ops"}' });
  await mounted.unmount(); state.dispose();
});

// fusedGroups mounts the fused view with its own Groups toolbar state and a
// process context covering this node's groups only.
async function mountFusedLens(t, { focus = 'agents', query = '' } = {}) {
  const harness = await createPreactHarness(t);
  const [stateMod, island, groupsStateMod, islandMod] = await Promise.all([
    harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js'),
    harness.importDashboardModule('js/groups-state.js'), harness.importDashboardModule('js/groups-island.js'),
  ]);
  const values = new Map([['tclaude.dash.groups.lens', focus]]);
  const prefs = { getItem: (k) => values.get(k) ?? null, setItem: (k, v) => values.set(k, String(v)), removeItem: (k) => values.delete(k) };
  const groups = groupsStateMod.createGroupsState({ prefs, resetOffsets: () => {}, reorder: (g) => g,
    columns: { list: () => [{ key: 'id', label: 'ID' }], hidden: () => false, setHidden: () => {}, deviationCount: () => 0 } });
  groups.initialize();
  if (query) groups.setQuery(query);
  // Both nodes have a group with the same id: only the name scoping may tell them apart.
  const home = { processes_enabled: true, groups: [group('frontend-squad', { id: 'grp_fe', members: [{ agent_id: 'agt_fe_lead', conv_id: 'c1', title: 'fe-lead', online: true }] })], agents: [] };
  groups.publish(home);
  const fleet = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '?nodes=all' });
  fleet.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }] });
  const peerSnap = { groups: [group('frontend-squad', { id: 'grp_fe', members: [{ agent_id: 'agt_far1', conv_id: 'cf', title: 'far-away', online: true }] }), group('ops')], agents: [] };
  const fetchImpl = async () => ({ ok: true, status: 200, json: async () => peerSnap });
  const fixture = JSON.parse((await import('node:fs')).readFileSync(new URL('./process-context-fixture.json', import.meta.url), 'utf8'));
  const source = { context: harness.signals.signal(fixture), status: harness.signals.signal('ready'), error: harness.signals.signal(''), refresh: () => Promise.resolve() };
  const timers = fakeTimers();
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const toolbar = harness.document.createElement('div'); harness.document.body.appendChild(toolbar);
  const bar = await harness.mount(harness.html`<${islandMod.GroupsControls} state=${groups} actions=${{ refresh: async () => {} }} />`, toolbar);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${fleet} host=${host} snapshot=${groups.snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" groups=${groups} processSource=${source} />`, host);
  for (const first of timers.queue.splice(0)) await harness.act(async () => { await first.fn(); });
  t.after(async () => { await mounted.unmount(); await bar.unmount(); fleet.dispose(); });
  return { harness, groups, root: mounted.container, toolbar: bar.container };
}

test('fused Groups is driven by the Groups toolbar: its filter narrows every node, and the counts are the fused ones', async (t) => {
  const { harness, groups, root, toolbar } = await mountFusedLens(t);
  const keys = () => [...root.querySelectorAll('details[data-group-key]')].map((d) => d.dataset.groupKey);
  assert.deepEqual(keys(), ['frontend-squad@desk', 'frontend-squad@forge', 'ops@forge']);
  await harness.act(() => { groups.setQuery('far-away'); });
  assert.deepEqual(keys(), ['frontend-squad@forge'], 'the filter matches a peer group by its member');
  assert.equal(toolbar.querySelector('#filter-groups-count').textContent, '1 / 3', 'the toolbar counts the fused listing');
  assert.equal(toolbar.querySelector('#filter-groups-ungrouped'), null, "this node's virtual groups are not offered while fused");
  assert.ok(toolbar.querySelector('#filter-groups-offline'));
  assert.ok(toolbar.querySelector('[data-groups-focus="processes"]'), 'the lens switch is there');
});

test('fused Processes shows this node\'s runs on its groups, and says a peer\'s are not shared', async (t) => {
  const { root } = await mountFusedLens(t, { focus: 'processes' });
  const home = root.querySelector('details[data-group-key="frontend-squad@desk"]');
  const peer = root.querySelector('details[data-group-key="frontend-squad@forge"]');
  assert.deepEqual([...home.querySelectorAll('[data-process-run-card]')].map((c) => c.dataset.processRunCard), ['run_migrate', 'run_release', 'run_reindex'],
    "a home group's runs are read under its own name");
  assert.ok(home.querySelector('[data-process-group="frontend-squad"]'), 'the runs body acts on the own group');
  assert.equal(peer.querySelectorAll('[data-process-run-card]').length, 0, "a peer group with the same name and id gets none of this node's runs");
  assert.match(peer.querySelector('[data-process-unshared]').textContent, /Process runs on forge are not shared across nodes/);
  assert.ok(home.querySelector('summary .gpi-graphs'), 'the home header shows its process graphs');
  assert.equal(peer.querySelector('summary .gpi-graphs'), null, 'the peer header shows none');
});

test('fused Both keeps a peer group\'s member table beside the not-shared note', async (t) => {
  const { root } = await mountFusedLens(t, { focus: 'both' });
  const peer = root.querySelector('details[data-group-key="frontend-squad@forge"]');
  assert.ok(peer.querySelector('[data-fused-section="agents"]'), 'the Agents section is there');
  assert.match(peer.textContent, /far-away/);
  assert.ok(peer.querySelector('[data-process-unshared]'));
  const home = root.querySelector('details[data-group-key="frontend-squad@desk"]');
  assert.ok(home.querySelector('[data-fused-processes="frontend-squad"]'), "this node's group has its Processes section");
});

test('fused view lets this node\'s process drags through, the run focus\'s performer dock included', async (t) => {
  const { harness, root } = await mountFusedLens(t, { focus: 'processes' });
  const add = (inner) => { const d = harness.document.createElement('div'); d.innerHTML = inner; root.appendChild(d); return d; };
  const dock = add('<div data-run-dock><span draggable="true" data-run-dock-agent="agt_fe_lead">fe</span></div>').querySelector('[data-run-dock-agent]');
  assert.equal(harness.fireEvent(dock, 'dragstart').defaultPrevented, false, 'a performer drags onto a step');
  const peer = add('<details data-fleet-node="inst_forge"><span draggable="true" data-saved-process-drag="tpl">p</span></details>').querySelector('span');
  assert.equal(harness.fireEvent(peer, 'dragstart').defaultPrevented, true, "a peer group's drags stay stopped");
});

test("another node's group controls run through actions bound to that node, as far as its grants reach", async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const [stateMod, island, nodeApi] = await Promise.all([harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-merged-island.js'), harness.importDashboardModule('js/node-api.js')]);
  // homeActions hands another node's group to its own actions, under its own name.
  const calls = [];
  const forgeActions = { patchGroup: async (g, field, value) => { calls.push(['forge', g.name, field, value]); } };
  const routed = island.homeActions(() => ({}), () => null, (node) => (node.id === 'inst_forge' ? forgeActions : null));
  await routed.patchGroup({ name: 'build@forge', fleet_node: { id: 'inst_forge', name: 'forge' } }, 'max_members', 3);
  await routed.patchGroup({ name: 'x@lab', fleet_node: { id: 'inst_lab', name: 'lab' } }, 'max_members', 3);
  assert.deepEqual(calls, [['forge', 'build', 'max_members', 3]], 'a node without actions is a no-op');
  // An agent's own action (a rename) names its node through the member.
  forgeActions.renameAgent = async (m, title) => { calls.push(['forge-rename', m, title]); };
  await routed.renameAgent({ agent_id: 'agt_1', title: 'old', fleet_node: { id: 'inst_forge', name: 'forge' } }, 'new');
  assert.deepEqual(calls.pop(), ['forge-rename', { agent_id: 'agt_1', title: 'old', fleet_node: undefined }, 'new']);

  // createNodeGroupsActions sends that node's requests through its route and reads it again.
  const requests = []; const changed = [];
  t.after(nodeApi.onNodeChanged((node) => changed.push(node)));
  const actions = island.createNodeGroupsActions({
    node: 'inst_forge', notify: () => {},
    fetchImpl: async (url, init) => { requests.push([url, init?.method, init?.body]); return { ok: true, status: 200, json: async () => [{ name: 'fast' }], text: async () => '' }; },
  });
  await actions.patchGroup({ name: 'build' }, 'default_cwd', '/srv/build');
  assert.deepEqual(requests.pop(), ['/api/groups/build', 'PATCH', '{"default_cwd":"/srv/build"}']);
  assert.deepEqual(changed, ['inst_forge'], 'the fused view reads forge again now');
  assert.deepEqual((await actions.groupProfileChoices('profile', { name: 'build' })).map((c) => c.value), ['fast'], "forge's own profiles");
  assert.equal(requests.pop()[0], '/api/spawn-profiles');
  assert.equal(typeof actions.openAddMember, 'function', "every Groups action is bound to the node; the grant table decides what a peer's rows run");
  assert.equal(actions.then, undefined, 'the actions are never mistaken for a promise');
  // A peer's pins and folds are this browser's view: kept under the listed name.
  const pins = [];
  const viewRouted = island.homeActions(() => ({ toggleQuickPin: (g) => pins.push(g.name) }), () => null, () => forgeActions);
  viewRouted.toggleQuickPin({ name: 'build@forge', fleet_node: { id: 'inst_forge', name: 'forge' } });
  assert.deepEqual(pins, ['build@forge']);

  // The click guard lets a granted control through and stops the rest; this node's always pass.
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '?nodes=all' });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }] });
  const timers = fakeTimers(); const toasts = [];
  const peerSnap = { groups: [group('build'), group('ops')], agents: [], peer_view: { level: 'restricted', included: ['agents.status', 'groups.settings'], groups: { build: ['groups.settings'] } } };
  // On forge's page forge is the page snapshot; this node is read directly.
  const localFetch = async () => ({ ok: true, status: 200, json: async () => ({ groups: [group('mine')], agents: [] }) });
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${harness.signals.signal(peerSnap)} fetchImpl=${localFetch} localFetch=${localFetch} timers=${timers} remote="inst_forge" toast=${(m) => toasts.push(m)} switchNode=${() => {}} />`);
  await harness.act(() => { state.setFused('all'); });
  const doc = harness.document;
  const inGroup = (node, name, inner) => { const d = doc.createElement('div'); d.innerHTML = `<details data-fleet-node="${node}" data-fleet-node-name="${name}">${inner}</details>`; host.appendChild(d); return d.firstElementChild.firstElementChild; };
  const granted = inGroup('inst_forge', 'forge', '<span data-act="set-group-max-members" data-group="build@forge">👥</span>');
  const notGranted = inGroup('inst_forge', 'forge', '<span data-act="set-group-max-members" data-group="ops@forge">👥</span>');
  const mine = inGroup('inst_self', 'desk', '<span data-act="set-group-dir" data-group="mine@desk"><span data-act="group-web-term" data-group="mine@desk">~</span></span>');
  assert.equal(harness.fireEvent(granted, 'click').defaultPrevented, false, "forge's grant covers build");
  assert.equal(harness.fireEvent(notGranted, 'click').defaultPrevented, true, 'but not ops');
  assert.equal(harness.fireEvent(mine.firstElementChild, 'click').defaultPrevented, false, "on forge's page this node's own group settings still run");
  await mounted.unmount(); state.dispose();
});
