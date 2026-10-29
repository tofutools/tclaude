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
  assert.ok(post.body.slugs.every((slug) => /\.read$/.test(slug)), `only read slugs: ${post.body.slugs}`);
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
