// Tests for the dashboard companion (tcl-6i0283): chips with a "needs you"
// state, the Steward summon, the native-terminal attach, the inbox message row
// with view context, and the pure placement/snap/status model.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const A = `agt_${'a'.repeat(32)}`;
const B = `agt_${'b'.repeat(32)}`;
const S = `agt_${'5'.repeat(32)}`;

function snapshotWith({ aStatus = 'idle', steward = false } = {}) {
  const groups = [{
    name: 'team', members: [
      { agent_id: A, conv_id: 'conv-a', title: 'alpha', online: true, state: { status: aStatus, status_detail: aStatus === 'awaiting_permission' ? 'Bash(rm -rf build)' : '' } },
      { agent_id: B, conv_id: 'conv-b', title: 'bravo', online: true, state: { status: 'working' } },
    ],
  }];
  if (steward) groups.push({ name: 'steward', scribe: true, members: [{ agent_id: S, conv_id: 'conv-s', title: 'steward-k3x', online: true, state: { status: 'idle' } }] });
  return { groups };
}

function memoryPrefs(initial = {}) {
  const store = new Map(Object.entries(initial));
  return { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k), store };
}

function fakeWidgetFactory(mounted) {
  return (options) => {
    const widget = {
      options, disposed: false, focused: 0,
      setActive() {}, connect: async () => { options.onStatus('connected'); },
      focus() { widget.focused += 1; }, dispose() { widget.disposed = true; },
    };
    mounted.push(widget);
    return widget;
  };
}

async function setup(t, { snapshot = snapshotWith(), prefs = memoryPrefs(), fetchImpl } = {}) {
  const harness = await createPreactHarness(t);
  const { CompanionApp } = await harness.importDashboardModule('js/companion-island.js');
  const snap = harness.signals.signal(snapshot);
  const activeTab = harness.signals.signal('processes');
  const mounted = [];
  const requests = [];
  const closed = [];
  let controller = null;
  const fetchFn = fetchImpl || (async (url, init = {}) => {
    requests.push({ url: String(url), body: init.body ? JSON.parse(init.body) : null });
    if (String(url) === '/api/scribe') return { ok: true, status: 200, json: async () => ({ agent_id: S, conv_id: 'conv-s', name: 'steward-k3x', reused: false }) };
    if (String(url).endsWith('/peek')) return { ok: true, status: 200, json: async () => ({ lines: ['● hello', 'Allow this command?'] }) };
    return { ok: true, status: 202, json: async () => ({ id: 1 }) };
  });
  const view = await harness.mount(harness.html`<${CompanionApp}
    snapshot=${snap} activeTab=${activeTab} prefs=${prefs} fetchImpl=${fetchFn}
    widgetFactory=${fakeWidgetFactory(mounted)} closeTerminalPanes=${(selectors) => closed.push(selectors)}
    registerController=${(c) => { controller = c; return () => { controller = null; }; }} />`);
  return { harness, view, snap, mounted, requests, closed, prefs, controller: () => controller };
}

test('with nothing pinned a single Steward chip invites the operator in', async (t) => {
  const { view, harness } = await setup(t);
  const chip = harness.getByRole(view.container, 'button', { name: /Open the companion/ });
  await harness.act(() => chip.click());
  assert.match(view.container.textContent, /Summon the Steward/);
  assert.equal(view.container.querySelector('[role="menu"]'), null, 'the empty window explains itself; the add menu stays closed');
});

test('summoning the Steward posts a read-only scoped scribe summon and attaches its terminal', async (t) => {
  const { view, harness, requests, mounted, closed } = await setup(t);
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Open the companion/ }).click());
  const summon = [...view.container.querySelectorAll('button')].find((b) => /Summon the Steward/.test(b.textContent) && b.classList.contains('companion-primary'));
  await harness.act(async () => { summon.click(); await Promise.resolve(); });
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const post = requests.find((r) => r.url === '/api/scribe');
  assert.ok(post, 'POST /api/scribe was sent');
  assert.equal(post.body.name, 'steward');
  assert.deepEqual(post.body.scope, { kind: 'steward' });
  assert.equal(post.body.background, true);
  assert.ok(!post.body.exclusive, 'the Steward keeps per-command --ask-human approvals available');
  assert.ok(post.body.slugs.every((slug) => /\.read$/.test(slug) || ['dashboard.guide', 'boards.write'].includes(slug)), `only read slugs plus pointing and drawing: ${post.body.slugs}`);
  assert.match(post.body.brief, /tclaude agent board show/);
  assert.match(post.body.brief, /--ask-human/);
  assert.deepEqual(closed.at(-1), [S], 'any Terminals-tab pane of the agent is closed first');
  // The summoned agent is not in a snapshot yet: the tab stays and says so.
  assert.match(view.container.textContent, /Starting the agent/);
  assert.ok(!/Summon the Steward/.test(view.container.querySelector('.companion-body').textContent), 'no second summon is offered while it starts');
});

test('the facade refuses an agent the snapshot does not know, so callers keep their fallback', async (t) => {
  const { controller } = await setup(t);
  assert.equal(controller().open(`agt_${'c'.repeat(32)}`), false);
  assert.equal(controller().open(A), true);
});

test('agents outside every group can be pinned', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-model.js');
  const agents = m.companionAgents({ groups: [], ungrouped: [{ agent_id: B, conv_id: 'b', title: 'solo', online: true, state: { status: 'idle' } }] });
  assert.equal(agents.get(B)?.name, 'solo');
  const body = m.composeCompanionMessage('é'.repeat(9000), 'Groups (/)');
  assert.ok(new TextEncoder().encode(body).length <= m.COMPANION_MESSAGE_MAX_BYTES);
});

test('a pinned agent awaiting permission shows "needs you" on its chip', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]) });
  const { view, harness } = await setup(t, { prefs, snapshot: snapshotWith({ aStatus: 'awaiting_permission' }) });
  const alpha = harness.getByRole(view.container, 'button', { name: /alpha: needs you/ });
  assert.ok(alpha.classList.contains('needs'));
  assert.ok(harness.getByRole(view.container, 'button', { name: /bravo: working/ }));
  assert.match(view.container.textContent, /1 companion need you/);
});

test('opening an agent attaches its native terminal stream and the window carries the prompt reason', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, mounted, controller } = await setup(t, { prefs, snapshot: snapshotWith({ aStatus: 'awaiting_permission' }) });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(mounted.length, 1);
  assert.equal(mounted[0].options.wsPath, `/api/open-window-ws/${A}`);
  assert.match(view.container.textContent, /alpha is waiting for you: Bash\(rm -rf build\)/);
  assert.ok(harness.getByRole(view.container, 'tab', { name: /alpha/ }));
});

test('the message row queues operator mail with the dashboard view attached', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, requests, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  const field = view.container.querySelector('input[aria-label="Message alpha"]');
  await harness.input(field, 'what can I do here?');
  await harness.act(async () => {
    field.closest('form').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
  const mail = requests.find((r) => r.url === '/api/operator-message');
  assert.ok(mail, 'operator mail was posted');
  assert.equal(mail.body.to, A);
  assert.match(mail.body.body, /^what can I do here\?\n\n\[dashboard view: Processes \(/);
});

test('Ctrl+. toggles the window and unpinning the last tab closes it', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, mounted } = await setup(t, { prefs });
  const key = () => harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  await key();
  assert.ok(view.container.querySelector('.companion-window'), 'window opened');
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  await key();
  assert.equal(view.container.querySelector('.companion-window'), null, 'window minimized');
  assert.ok(mounted[0].disposed, 'minimizing disposes the terminal widget');
  await key();
  await harness.act(() => harness.getByRole(view.container, 'button', { name: 'Unpin alpha' }).click());
  assert.equal(view.container.querySelector('.companion-window'), null);
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.tabs')), []);
});

test('placement model: clamping, snap zones and status folding', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-model.js');
  const vp = { width: 1200, height: 800 };
  const clamped = m.clampPlacement({ x: 5000, y: -40, w: 100, h: 5000 }, vp);
  assert.equal(clamped.w, m.COMPANION_MIN_W);
  assert.ok(clamped.x <= vp.width - 80 && clamped.y === 0 && clamped.h <= vp.height);
  assert.equal(m.snapZone({ x: 1195, y: 300 }, vp), 'right');
  assert.equal(m.snapZone({ x: 3, y: 300 }, vp), 'left');
  assert.equal(m.snapZone({ x: 600, y: 790 }, vp), 'bottom');
  assert.equal(m.snapZone({ x: 600, y: 300 }, vp), '');
  const right = m.zoneRect('right', vp, 60);
  assert.equal(right.x + right.w, vp.width);
  assert.equal(right.y, 60);
  assert.equal(m.companionStatus({ online: true, status: 'awaiting_input' }), 'needs');
  assert.equal(m.companionStatus({ online: true, status: 'main_agent_idle' }), 'working');
  assert.equal(m.companionStatus({ online: false, status: 'working' }), 'offline');
  assert.deepEqual(m.parseTabs(JSON.stringify([A, 'nope', A, B])), [A, B]);
  assert.equal(m.nextActive([A, B, S], B), S);
  assert.equal(m.nextActive([A, B], B), A);
  assert.equal(m.composeCompanionMessage('  '), '');
  const agents = m.companionAgents(snapshotWith({ steward: true }));
  assert.equal(m.stewardAgent(agents).agentId, S);
});

test('the companion reports the operator view and plays a guidance cue once', async (t) => {
  const harness = await createPreactHarness(t);
  const { CompanionApp } = await harness.importDashboardModule('js/companion-island.js');
  const doc = harness.document;
  const toolbar = doc.body.appendChild(doc.createElement('div'));
  const save = toolbar.appendChild(doc.createElement('button'));
  save.textContent = 'Save draft';
  save.getBoundingClientRect = () => ({ left: 100, top: 50, right: 180, bottom: 74, width: 80, height: 24 });
  const other = toolbar.appendChild(doc.createElement('button'));
  other.textContent = 'Save draft and close the editor';
  other.getBoundingClientRect = () => ({ left: 200, top: 50, right: 400, bottom: 74, width: 200, height: 24 });
  const snap = harness.signals.signal({ groups: [], companion_cues: [{ id: 1, note: 'old', at: '' }] });
  const posts = [];
  const navigated = [];
  const fetchImpl = async (url, init = {}) => { posts.push({ url: String(url), body: init.body ? JSON.parse(init.body) : null }); return { ok: true, status: 204, json: async () => ({}) }; };
  const view = await harness.mount(harness.html`<${CompanionApp} snapshot=${snap} activeTab=${harness.signals.signal('groups')}
    prefs=${memoryPrefs()} fetchImpl=${fetchImpl} widgetFactory=${() => ({})} registerController=${() => () => {}}
    navigate=${async (path) => { navigated.push(path); return true; }} />`);
  const report = posts.find((p) => p.url === '/api/companion/view');
  assert.ok(report, 'the view is reported on mount');
  assert.equal(report.body.tab, 'groups');
  assert.equal(doc.querySelector('.companion-guide-note'), null, 'a cue live before the page loaded is not replayed');
  await harness.act(async () => {
    snap.value = { groups: [], companion_cues: [{ id: 1, note: 'old' }, { id: 2, agent_name: 'steward', tab: 'processes', target: 'save draft', note: 'Save once the diff looks right' }] };
    await new Promise((r) => setTimeout(r, 600));
  });
  // The cue plays from an effect that runs as that act settles; give its
  // navigation and target search time inside a second act so its render flushes.
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 600)); });
  assert.deepEqual(navigated, ['/processes']);
  const note = doc.querySelector('.companion-guide-note');
  assert.ok(note, 'the cue note is shown');
  assert.match(note.textContent, /Save once the diff looks right/);
  const ring = doc.querySelector('.companion-guide-ring');
  assert.ok(ring, 'the matched control is ringed');
  assert.match(ring.getAttribute('style'), /left:94px/, 'the exact match wins over the longer label');
  await harness.act(() => harness.getByRole(doc.body, 'button', { name: 'Dismiss' }).click());
  assert.equal(doc.querySelector('.companion-guide-note'), null);
  await view.unmount();
});

test('guide target matching prefers exact, then prefix, then shorter text', async (t) => {
  const harness = await createPreactHarness(t);
  const g = await harness.importDashboardModule('js/companion-guide.js');
  assert.ok(g.guideMatchScore({ text: 'Save draft' }, 'save draft') > g.guideMatchScore({ text: 'Save draft now' }, 'save draft'));
  assert.ok(g.guideMatchScore({ text: 'Save draft now' }, 'save draft') > g.guideMatchScore({ text: 'Then save draft' }, 'save draft'));
  assert.equal(g.guideMatchScore({ text: 'Validate' }, 'save'), 0);
  assert.ok(g.guideMatchScore({ text: '', label: 'Open the companion' }, 'companion') > 0);
  assert.equal(g.cuePath({ tab: 'usage' }), '/usage');
  assert.equal(g.cuePath({ tab: 'usage', path: '/usage/costs' }), '/usage/costs');
  const seen = new Set();
  assert.deepEqual(g.newCues(seen, [{ id: 3 }], false), []);
  assert.deepEqual(g.newCues(seen, [{ id: 3 }, { id: 5 }, { id: 4 }], true).map((c) => c.id), [4, 5]);
});

test('a pending sudo plan from a pinned agent is decided in its companion tab', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const snapshot = snapshotWith();
  snapshot.access_requests = [
    { id: 'req-1', perm: 'sudo.groups.create,profiles.manage', agent_id: A, conv_id: 'conv-a', body: 'Plan: 1. create groups api, web\n2. add profile dev-default', status: 'pending', deadline: new Date(Date.now() + 90000).toISOString() },
    { id: 'req-2', perm: 'groups.create', agent_id: B, status: 'pending' },
    { id: 'req-4', perm: 'groups.members.stop', agent_id: A, status: 'pending', target_group: 'api', target_conv_title: 'builder', target_conv_id: 'conv-x', path: '/v1/groups/api/stop' },
    { id: 'req-3', perm: 'groups.create', agent_id: A, status: 'approved' },
  ];
  const { view, harness, requests, controller } = await setup(t, { prefs, snapshot });
  assert.ok(harness.getByRole(view.container, 'button', { name: /alpha: needs you/ }), 'a pending approval marks the chip');
  await harness.act(() => controller().open(A));
  const card = view.container.querySelector('.companion-approval');
  assert.ok(card, 'the plan card is shown in the tab');
  assert.equal(view.container.querySelectorAll('.companion-approval').length, 2, 'only this agent\'s pending requests');
  const command = view.container.querySelectorAll('.companion-approval')[1];
  assert.match(command.textContent, /Group\s*api/, 'the target group is shown');
  assert.match(command.textContent, /Target\s*builder/, 'the target agent is shown');
  assert.match(command.textContent, /\/v1\/groups\/api\/stop/, 'the endpoint shows when there is no body');
  assert.match(card.textContent, /Temporary permissions for a plan/);
  assert.match(card.textContent, /groups\.create/);
  assert.match(card.textContent, /create groups api, web/);
  await harness.act(async () => {
    harness.getByRole(card, 'button', { name: 'Approve plan' }).click();
    await new Promise((r) => setTimeout(r, 0));
  });
  const decision = requests.find((r) => r.url === '/api/access-requests/req-1/decision');
  assert.ok(decision, 'the decision was posted');
  assert.deepEqual(decision.body, { decision: 'approve' });
});

test('annotation strokes are classified and named by what is under them', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-annotate.js');
  const circle = Array.from({ length: 16 }, (_, i) => [100 + 40 * Math.cos(i / 15 * 2 * Math.PI), 100 + 20 * Math.sin(i / 15 * 2 * Math.PI)]);
  assert.equal(m.classifyStroke(circle), 'circle');
  assert.equal(m.classifyStroke([[0, 50], [40, 52], [90, 49], [120, 51]]), 'underline');
  assert.equal(m.classifyStroke([[0, 0], [60, 40], [120, 90]]), 'arrow');
  assert.equal(m.classifyStroke([[0, 0], [5, 6], [2, 3]]), 'mark');
  const at = (x, y) => (x > 50 ? { text: 'Review', role: 'button' } : { text: 'Intake', role: '' });
  assert.equal(m.describeStroke(circle, at), 'circled “Review” (button)');
  assert.equal(m.describeStroke([[10, 0], [60, 40], [120, 90]], at), 'drew an arrow from “Intake” to “Review” (button)');
  assert.equal(m.composeAnnotation({ note: 'why?', lines: ['circled “Review” (button)'] }),
    'why?\n\nThe operator marked up the dashboard view they are on:\n1. circled “Review” (button)');
  // A flat stroke that ends on a different control is a horizontal arrow.
  const flat = [[0, 50], [60, 52], [120, 50]];
  const onButton = (x, y) => (x > 100 ? { text: 'Send', role: 'button' } : { text: 'Hello world', role: '' });
  assert.equal(m.resolveStroke(flat, onButton).kind, 'arrow');
  assert.equal(m.resolveStroke(flat, () => ({ text: 'Hello world', role: '' })).kind, 'underline');
  // Field values never leave the page.
  const doc = harness.window.document;
  const input = doc.createElement('input');
  input.type = 'password'; input.value = 'hunter2'; input.setAttribute('placeholder', 'API token');
  doc.body.appendChild(input);
  assert.deepEqual(m.labelFor(input), { text: 'API token', role: 'field' });
});

test('marking up the page sends the agent a numbered description of the marks', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, requests, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  const doc = harness.window.document;
  const target = doc.createElement('button');
  target.textContent = 'Approve plan';
  doc.body.appendChild(target);
  doc.elementsFromPoint = () => [target];
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Mark up this page for alpha/ }).click());
  assert.ok(doc.body.classList.contains('companion-annotating'), 'the window steps aside while drawing');
  const svg = view.container.querySelector('svg.annotate-canvas');
  const fire = (type, x, y) => {
    const camel = { pointerdown: 'PointerDown', pointermove: 'PointerMove', pointerup: 'PointerUp' }[type];
    harness.fireEvent(svg, `on${type}` in svg ? type : camel, { button: 0, pointerId: 1, clientX: x, clientY: y });
  };
  await harness.act(() => {
    fire('pointerdown', 140, 100);
    for (let i = 1; i <= 15; i++) fire('pointermove', 100 + 40 * Math.cos(i / 15 * 2 * Math.PI), 100 + 20 * Math.sin(i / 15 * 2 * Math.PI));
    fire('pointerup', 140, 100);
  });
  await harness.input(view.container.querySelector('textarea[aria-label="Note"]'), 'why is this greyed out?');
  await harness.act(async () => {
    view.container.querySelector('form.annotate-bar').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
  const mail = requests.find((r) => r.url === '/api/operator-message' && r.body.subject === '[annotation]');
  assert.ok(mail, 'the markup was mailed');
  assert.equal(mail.body.to, A);
  assert.match(mail.body.body, /^why is this greyed out\?\n\nThe operator marked up the dashboard view they are on:\n1\. circled “Approve plan” \(button\)\n\n\[dashboard view: Processes \(/);
  assert.equal(view.container.querySelector('svg.annotate-canvas'), null, 'the layer closes after sending');
  assert.ok(!doc.body.classList.contains('companion-annotating'));
});
