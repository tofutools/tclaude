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

async function setup(t, { snapshot = snapshotWith(), prefs = memoryPrefs(), sessionPrefs = memoryPrefs(), fetchImpl, speakerImpl = null, dictationImpl = null } = {}) {
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
    snapshot=${snap} activeTab=${activeTab} prefs=${prefs} sessionPrefs=${sessionPrefs} fetchImpl=${fetchFn}
    widgetFactory=${fakeWidgetFactory(mounted)} speakerImpl=${speakerImpl} dictationImpl=${dictationImpl} closeTerminalPanes=${(selectors) => closed.push(selectors)}
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

test('summoning the Steward posts a scoped scribe summon and attaches its terminal', async (t) => {
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
  assert.equal(post.body.slugs, undefined, 'the daemon decides a Steward\'s access');
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

test('a cue cannot point at the operator\'s own consent', async (t) => {
  const harness = await createPreactHarness(t);
  const g = await harness.importDashboardModule('js/companion-guide.js');
  const doc = harness.window.document;
  const page = doc.createElement('div');
  page.innerHTML = `<button id="keep">Approve the plan later</button>
    <div class="mail-decision"><button>Sidebar</button></div>
    <button class="access-btn approve">Grant it</button>
    <button id="save">Save draft</button>`;
  doc.body.append(page);
  t.after(() => page.remove());
  const keep = doc.getElementById('keep');
  assert.equal(g.consentControl(keep), true, 'by its words');
  assert.equal(g.consentControl(page.querySelector('.mail-decision button')), true, 'by where it is');
  assert.equal(g.consentControl(page.querySelector('.access-btn')), true);
  assert.equal(g.consentControl(doc.getElementById('save')), false);
  for (const target of ['Approve the plan later', 'Sidebar', 'Grant it']) {
    assert.equal(g.findGuideTarget(doc, target), null, target);
  }
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

test('a decision from a pinned agent is answered with a button in its companion tab', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const snapshot = snapshotWith();
  snapshot.messages = [
    { id: 7, from_agent: A, subject: 'Which layout?', body: 'Tabs are compact; a sidebar shows more.', options: ['Tabs', 'Sidebar'], recommended: 2,
      attachments: [{ id: 1, filename: 'mock.png', url: '/api/human-messages/7/attachments/1', previewable: true }] },
    { id: 8, from_agent: A, subject: 'Answered', options: ['x', 'y'], decision: 'x' },
    { id: 9, from_agent: B, subject: 'Not alpha', options: ['x', 'y'] },
    { id: 10, from_agent: A, subject: 'Just news' },
  ];
  const { view, harness, requests, controller } = await setup(t, { prefs, snapshot });
  assert.ok(harness.getByRole(view.container, 'button', { name: /alpha: needs you/ }), 'a waiting decision marks the chip');
  await harness.act(() => controller().open(A));
  const cards = view.container.querySelectorAll('.companion-decision');
  assert.equal(cards.length, 1, 'only this agent\'s unanswered decisions');
  assert.match(cards[0].textContent, /Which layout\?/);
  assert.match(cards[0].textContent, /a sidebar shows more/);
  assert.ok(cards[0].querySelector('img[src="/api/human-messages/7/attachments/1"]'), 'an attached mock is shown');
  const sidebar = harness.getByRole(cards[0], 'button', { name: /Sidebar/ });
  assert.ok(sidebar.classList.contains('companion-primary'), 'the recommended option stands out');
  await harness.input(cards[0].querySelector('input.companion-decision-note'), 'keep it narrow');
  await harness.act(async () => { sidebar.click(); await new Promise((r) => setTimeout(r, 0)); });
  const reply = requests.find((r) => r.url === '/api/human-messages/reply');
  assert.deepEqual(reply.body, { id: 7, choice: 2, body: 'keep it narrow' });
});

test('a cue can open the agent that needs the operator in the companion window', async (t) => {
  const { view, harness, snap } = await setup(t);
  assert.equal(view.container.querySelector('.companion-window'), null);
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: [{ id: 3, agent_name: 'steward', open: B }] };
    await new Promise((r) => setTimeout(r, 0));
  });
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const tab = harness.getByRole(view.container, 'tab', { name: /bravo/ });
  assert.equal(tab.getAttribute('aria-selected'), 'true', 'bravo is pinned and shown');
  assert.equal(harness.window.document.querySelector('.companion-guide-note'), null, 'an open-only cue draws nothing');
});

test('an open cue only pins the agent while the operator is typing to another tab', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, snap, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  await harness.input(view.container.querySelector('input.companion-draft'), 'half a thought');
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: [{ id: 4, agent_name: 'steward', open: B }] };
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.equal(harness.getByRole(view.container, 'tab', { name: /alpha/ }).getAttribute('aria-selected'), 'true', 'the draft keeps its agent');
  assert.equal(harness.getByRole(view.container, 'tab', { name: /bravo/ }).getAttribute('aria-selected'), 'false', 'bravo is pinned beside it');
  assert.equal(view.container.querySelector('input.companion-draft').value, 'half a thought');
});

test('a hand-over cue on a hidden dashboard tab waits until the tab is shown', async (t) => {
  const prefs = memoryPrefs();
  const { view, harness, snap } = await setup(t, { prefs });
  const doc = harness.window.document;
  let visibility = 'hidden';
  Object.defineProperty(doc, 'visibilityState', { configurable: true, get: () => visibility });
  const show = async () => {
    visibility = 'visible';
    await harness.act(async () => { doc.dispatchEvent(new harness.window.Event('visibilitychange')); await new Promise((r) => setTimeout(r, 0)); });
  };
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: [{ id: 5, agent_name: 'steward', open: B, at: new Date().toISOString() }] };
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.equal(view.container.querySelector('.companion-window'), null, 'nothing opens while hidden');
  await show();
  assert.equal(harness.getByRole(view.container, 'tab', { name: /bravo/ }).getAttribute('aria-selected'), 'true', 'bravo opens on return');
  assert.match(prefs.store.get('tclaude.dash.companion.cue-open'), /^5@/, 'the hand-over is claimed for the other dashboard tabs');

  // A hand-over another tab already took, or one that went stale, is dropped.
  const g = await harness.importDashboardModule('js/companion-guide.js');
  assert.equal(g.claimCueOpen('5@t1', { id: 5, at: 't1' }), false);
  assert.equal(g.claimCueOpen('5@t1', { id: 6, at: 't2' }), true);
  assert.equal(g.claimCueOpen('5@t1', { id: 1, at: 't3' }), true, 'ids restart with the daemon');
  assert.equal(g.claimCueOpen(null, { id: 1 }), true);
  assert.equal(g.cueOpenDue({ at: new Date(1000).toISOString() }, 1000 + g.CUE_OPEN_PATIENCE_MS + 1), false);
  assert.equal(g.cueOpenDue({ at: new Date(1000).toISOString() }, 2000), true);
});

test('live scribes and Stewards keep a tab by themselves, and a closed one stays closed', async (t) => {
  const P = `agt_${'d'.repeat(32)}`;
  const S2 = `agt_${'e'.repeat(32)}`;
  const withHelpers = ({ scribe = true, second = false } = {}) => {
    const snapshot = snapshotWith({ steward: true });
    snapshot.groups.push({ name: 'process-scribe', scribe: true, members: [{ agent_id: P, conv_id: 'conv-p', title: 'process-scribe-a1', online: scribe, state: { status: 'idle' } }] });
    if (second) snapshot.groups[1].members.push({ agent_id: S2, conv_id: 'conv-s2', title: 'steward-new', online: true, state: { status: 'idle' } });
    return snapshot;
  };
  const prefs = memoryPrefs();
  const { view, harness, snap, controller } = await setup(t, { prefs, snapshot: withHelpers() });
  const chip = (name) => harness.queryByRole?.(view.container, 'button', { name }) ?? [...view.container.querySelectorAll('.companion-chip')].find((b) => name.test(b.getAttribute('aria-label')));
  assert.ok(chip(/steward-k3x/), 'the Steward has a pill without being pinned');
  assert.ok(chip(/process-scribe-a1/), 'so has a scribe, whoever summoned it');
  assert.equal(chip(/bravo/), undefined, 'ordinary agents still wait to be pinned');
  assert.equal(view.container.querySelector('.companion-window'), null, 'a helper appearing does not open the window');

  // Two Stewards can be live (a changed permission set summons a fresh one).
  await harness.act(async () => { snap.value = withHelpers({ second: true }); });
  assert.ok(chip(/steward-new/), 'both Stewards are tracked');

  // Closing a helper sticks, through a poll that sees it offline and a reload.
  await harness.act(() => controller().open(P));
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Unpin process-scribe-a1/ }).click());
  const hasTab = () => [...view.container.querySelectorAll('.companion-tab')].some((tab) => /process-scribe-a1/.test(tab.textContent));
  await harness.act(async () => { snap.value = withHelpers({ scribe: false, second: true }); });
  await harness.act(async () => { snap.value = withHelpers({ second: true }); });
  assert.equal(hasTab(), false, 'closed stays closed');
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.closed')), [P], 'remembered for the next load');

  // Reopening from the add menu makes it tracked again.
  await harness.act(() => harness.getByRole(view.container, 'button', { name: 'Add a companion' }).click());
  await harness.act(() => harness.getByRole(view.container, 'menuitem', { name: /process-scribe-a1/ }).click());
  assert.equal(hasTab(), true);
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.closed')), []);

  const m = await harness.importDashboardModule('js/companion-model.js');
  const agent = (id, scribe, online) => [id, { agentId: id, scribe, online }];
  const X = `agt_${'1'.repeat(32)}`;
  const Y = `agt_${'2'.repeat(32)}`;
  const Z = `agt_${'3'.repeat(32)}`;
  const agents = new Map([agent(A, false, false), agent(X, true, false), agent(Y, true, true), agent(Z, true, true)]);
  assert.deepEqual(m.syncHelperTabs([A, X], agents, new Set([Z]), new Set()), [A, Y], 'a pin stays, an ended helper goes, a new one comes, a closed one does not');
  assert.deepEqual(m.syncHelperTabs([X], agents, new Set([Y, Z]), new Set([X])), [X], 'a tab being kept stays');
  const same = [Y, Z];
  assert.equal(m.syncHelperTabs(same, agents, new Set(), new Set()), same, 'nothing to do changes nothing');
  const full = Array.from({ length: m.COMPANION_MAX_TABS }, (_, i) => `agt_${String(i).repeat(32)}`);
  assert.deepEqual(m.syncHelperTabs(full, new Map([agent(Y, true, true)]), new Set(), new Set()), full, 'a helper never pushes out a pin');
  assert.equal(m.parseClosedHelpers(JSON.stringify(full.concat(A, B, 'junk'))).size, m.COMPANION_MAX_TABS + 2, 'every closed helper is remembered');
});

test('the window comes back open after a reload of the same browser tab', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const sessionPrefs = memoryPrefs({ 'tclaude.dash.companion.open': '1' });
  const { view, harness } = await setup(t, { prefs, sessionPrefs });
  assert.ok(view.container.querySelector('.companion-window'), 'it was open before the reload');
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Minimize/ }).click());
  assert.equal(sessionPrefs.store.get('tclaude.dash.companion.open'), '0', 'minimized stays minimized');
});

test('a reload shows this browser tab\'s own agent and takes it over from its Terminals pane', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]), 'tclaude.dash.companion.active': A });
  const sessionPrefs = memoryPrefs({ 'tclaude.dash.companion.open': '1', 'tclaude.dash.companion.active': B });
  const { view, harness, closed } = await setup(t, { prefs, sessionPrefs });
  assert.equal(harness.getByRole(view.container, 'tab', { name: /bravo/ }).getAttribute('aria-selected'), 'true', 'not what another browser tab showed last');
  assert.ok(closed.some((selectors) => selectors.includes(B)), 'its Terminals pane closes');
});

test('an agent moved to Terminals before a reload stays there', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]) });
  const sessionPrefs = memoryPrefs({ 'tclaude.dash.companion.open': '1', 'tclaude.dash.companion.active': B, 'tclaude.dash.companion.parked': JSON.stringify([B]) });
  const { closed } = await setup(t, { prefs, sessionPrefs });
  assert.equal(closed.some((selectors) => selectors.includes(B)), false);
});

test('the companion stays out of the page until features.collab is on', async (t) => {
  const harness = await createPreactHarness(t);
  const { CompanionGate } = await harness.importDashboardModule('js/companion-island.js');
  const snap = harness.signals.signal({ ...snapshotWith(), collab_enabled: false });
  const activeTab = harness.signals.signal('groups');
  const controllers = [];
  const view = await harness.mount(harness.html`<${CompanionGate} snapshot=${snap} activeTab=${activeTab} prefs=${memoryPrefs()} sessionPrefs=${memoryPrefs()}
    fetchImpl=${async () => ({ ok: true, status: 200, json: async () => ({}) })} widgetFactory=${fakeWidgetFactory([])} closeTerminalPanes=${() => {}}
    registerController=${(c) => { controllers.push(c); return () => {}; }} />`);
  assert.equal(view.container.querySelector('.companion-chips'), null);
  assert.equal(controllers.length, 0, 'no controller, so callers take their usual path');
  await harness.act(async () => { snap.value = { ...snapshotWith(), collab_enabled: true }; });
  assert.ok(view.container.querySelector('.companion-chips'), 'turning it on needs no reload');
  await harness.act(async () => { snap.value = { ...snapshotWith(), collab_enabled: false }; });
  assert.equal(view.container.querySelector('.companion-chips'), null);
});

test('a permission request opens in a right-hand panel from its pill, and the panel closes when answered', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const deadline = new Date(Date.now() + 120000).toISOString();
  const snapshot = {
    ...snapshotWith(), collab_enabled: true,
    access_requests: [
      { id: 'r2', perm: 'agent.spawn', agent_id: B, conv_title: 'bravo', status: 'pending', deadline: new Date(Date.now() + 200000).toISOString() },
      { id: 'r1', perm: 'sudo.groups.create', agent_id: A, status: 'pending', deadline },
      { id: 'r3', perm: 'x', agent_id: A, status: 'approved' },
      { id: 'r4', perm: 'y', agent_id: A, status: 'pending', proposal: {} },
      { id: 'r5', perm: 'y', agent_id: B, status: 'pending', origin_peer: 'node-2' },
    ],
  };
  const { view, harness, snap, requests } = await setup(t, { prefs, snapshot });
  const { AccessPanelGate } = await harness.importDashboardModule('js/access-panel-island.js');
  const panelView = await harness.mount(harness.html`<${AccessPanelGate} snapshot=${snap} fetchImpl=${async (url, init) => { requests.push({ url: String(url), body: JSON.parse(init.body) }); return { ok: true, status: 200 }; }} notify=${() => {}} />`);
  const panelHost = panelView.container;
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(panelHost.querySelector('.access-panel'), null, 'closed until asked for');

  const chip = harness.getByRole(view.container, 'button', { name: /alpha: / });
  assert.match(chip.textContent, /approval/);
  await harness.act(() => chip.click());
  assert.equal(view.container.querySelector('.companion-window'), null, 'the window stays minimized');
  const cards = panelHost.querySelectorAll('.companion-approval');
  assert.equal(cards.length, 2, 'what one card can answer, from every agent');
  assert.match(panelHost.textContent, /2 requests need a closer look in Messages/, 'a peer\'s request or a proposal is sent on, never approved blind');
  assert.match(cards[0].textContent, /groups\.create/, 'the agent asked about comes first');
  assert.match(cards[1].textContent, /bravo/, 'each names its agent');
  assert.ok(harness.getByRole(view.container, 'button', { name: /1 permission request from other agents/ }), 'bravo has no pill, so a pill says it is waiting');

  await harness.act(async () => { harness.getByRole(cards[0], 'button', { name: /Approve/ }).click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(requests.find((r) => r.url.includes('/decision')).body, { decision: 'approve' });
  await harness.act(async () => { snap.value = { ...snap.value, access_requests: [] }; });
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(panelHost.querySelector('.access-panel'), null, 'it closes when nothing is left');
});

test('a Terminals tab knows its agent is asking for permission', async (t) => {
  const harness = await createPreactHarness(t);
  const { paneApprovals } = await harness.importDashboardModule('js/terminal-shell-island.js');
  const snapshot = { collab_enabled: true, access_requests: [{ id: 'r', perm: 'x', agent_id: A, status: 'pending' }, { id: 'c', perm: 'y', conv_id: 'conv-b', status: 'pending' }] };
  assert.equal(paneApprovals({ seed: { agent: A } }, snapshot).length, 1);
  assert.equal(paneApprovals({ seed: { agent: 'conv-b' } }, snapshot).length, 1, 'a pane seeded with a conv id');
  assert.equal(paneApprovals({ seed: { agent: 'conv-a' } }, { collab_enabled: true, access_requests: [{ id: 'r', perm: 'x', agent_id: A, conv_id: 'conv-a', status: 'pending' }] }).length, 1, 'a conv-seeded pane finds a request naming its agent');
  assert.equal(paneApprovals({ seed: { agent: B } }, snapshot).length, 0);
  assert.equal(paneApprovals({ seed: { agent: A } }, { ...snapshot, collab_enabled: false }).length, 0, 'the panel is part of Collab');
});

test('decisions from every agent wait in one "to decide" queue', async (t) => {
  const C = `agt_${'c'.repeat(32)}`;
  const snapshot = snapshotWith();
  snapshot.messages = [
    { id: 11, from_agent: C, from_title: 'web-builder', subject: 'Which layout?', options: ['Tabs', 'Sidebar'] },
    { id: 10, from_agent: A, subject: 'Loop back?', options: ['Yes', 'No'], recommended: 1 },
    { id: 12, from_agent: B, subject: 'Done', options: ['a', 'b'], decision: 'a' },
  ];
  const { view, harness, requests, snap } = await setup(t, { snapshot });
  const pill = harness.getByRole(view.container, 'button', { name: /2 decisions to make/ });
  await harness.act(() => pill.click());
  const queue = view.container.querySelector('.companion-decide-queue');
  assert.ok(queue, 'the queue opens');
  const cards = queue.querySelectorAll('.companion-decision');
  assert.equal(cards.length, 2, 'unanswered decisions only, from pinned and unpinned agents');
  assert.match(cards[0].textContent, /Loop back\?.*alpha/s, 'oldest first, named by its agent');
  assert.match(cards[1].textContent, /web-builder/);
  assert.equal(view.container.querySelector('form.companion-input'), null, 'no message row while deciding');
  await harness.act(async () => { harness.getByRole(cards[1], 'button', { name: 'Sidebar' }).click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(requests.find((r) => r.url === '/api/human-messages/reply').body, { id: 11, choice: 2, body: '' });
  // Once everything is answered the queue ends, and a later decision only
  // brings the pill back instead of covering the window.
  await harness.act(async () => { snap.value = { ...snap.value, messages: [] }; });
  await harness.act(async () => { snap.value = { ...snap.value, messages: [{ id: 13, from_agent: C, subject: 'Again?', options: ['x', 'y'] }] }; });
  assert.equal(view.container.querySelector('.companion-decide-queue'), null);
});

test('decision speech names the question and the options', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-model.js');
  assert.equal(m.decisionSpeech({ name: 'alpha' }, { subject: 'Which layout?', options: ['Tabs', 'Sidebar'], recommended: 2 }),
    'alpha asks you to decide: Which layout? 2 options: Tabs; Sidebar, which it recommends.');
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

test('the Steward tab offers one-click setup starters that send with the view attached', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([S]) });
  const { view, harness, requests, controller } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  const starter = harness.getByRole(view.container, 'button', { name: 'Set up my repos' });
  await harness.act(async () => { starter.click(); await new Promise((r) => setTimeout(r, 0)); });
  const mail = requests.find((r) => r.url === '/api/operator-message');
  assert.equal(mail.body.to, S);
  assert.match(mail.body.body, /^Help me set up a group for each of my repos.*\n\n\[dashboard view: Processes \(/s);
  // Typing hides the starters; other agents never show them.
  await harness.input(view.container.querySelector('input.companion-draft'), 'x');
  assert.equal(view.container.querySelector('.companion-starters'), null);
});

test('voice output speaks a reply when the turn ends and says when the agent needs you', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const spoken = [];
  let cancelled = 0;
  const speakerImpl = { supported: true, speak: (text) => spoken.push(text), cancel: () => { cancelled += 1; } };
  const speechReads = [];
  const fetchImpl = async (url) => {
    if (String(url).includes('/speech')) {
      speechReads.push(String(url));
      const primed = String(url).endsWith('since=9000000000000000000');
      return { ok: true, status: 200, json: async () => ({ now: 1000 + speechReads.length, items: primed ? [] : [{ text: 'Let me look.', at_ns: 2001 }, { text: 'You have **three** profiles.', at_ns: 2002 }] }) };
    }
    return { ok: true, status: 202, json: async () => ({}) };
  };
  const { view, harness: h, snap, controller } = await setup(t, { prefs, fetchImpl, speakerImpl, snapshot: snapshotWith({ aStatus: 'working' }) });
  const voice = await h.importDashboardModule('js/companion-voice.js');
  assert.equal(voice.speakableText('## Done\n\nI **renamed** `build` to compile:\n```sh\nmake\n```\nSee https://x.y/z.'),
    'Done. I renamed build to compile: (code omitted). See a link');
  assert.equal(voice.speakableText('Edited foo_bar_baz.go, run `go test ./...`'), 'Edited foo_bar_baz.go, run go test ./...');
  assert.match(voice.speakableText('One. '.repeat(400), 50), /The rest is in the window\.$/);
  await h.act(() => controller().open(A));
  const toggle = h.getByRole(view.container, 'button', { name: /Speak alpha's replies aloud/ });
  await h.act(async () => { toggle.click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(toggle.getAttribute('aria-pressed'), 'true');
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.voice')), [A]);
  assert.ok(speechReads[0].endsWith('since=9000000000000000000'), 'turning voice on primes the cursor, speaking nothing old');
  await h.act(async () => { snap.value = snapshotWith({ aStatus: 'idle' }); await new Promise((r) => setTimeout(r, 0)); });
  await h.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(spoken, ['You have three profiles.'], 'the turn\'s last text is spoken');
  assert.match(speechReads.at(-1), /since=1001$/, 'the read starts at the primed cursor');
  assert.equal(prefs.store.get('tclaude.dash.companion.voice.said.' + A), '2002', 'the reply is claimed, so other dashboard tabs stay quiet');
  await h.act(async () => { snap.value = snapshotWith({ aStatus: 'awaiting_permission' }); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(spoken.at(-1), 'alpha needs you: Bash(rm -rf build)');
  await h.input(view.container.querySelector('input.companion-draft'), 'stop');
  assert.ok(cancelled >= 1, 'typing stops speech');
});

test('voice speaks a further decision from an agent already waiting on the operator', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]), 'tclaude.dash.companion.voice': JSON.stringify([A]) });
  const spoken = [];
  const speakerImpl = { supported: true, speak: (text) => spoken.push(text), cancel() {} };
  const first = { id: 7, from_agent: A, subject: 'Which layout?', options: ['Tabs', 'Sidebar'] };
  const withMessages = (messages) => ({ ...snapshotWith({ aStatus: 'awaiting_permission' }), messages });
  const { harness, snap } = await setup(t, { prefs, speakerImpl, snapshot: withMessages([first]) });
  assert.deepEqual(spoken, [], 'the first sight is silent');
  await harness.act(async () => { snap.value = withMessages([first, { id: 9, from_agent: A, subject: 'Ship today?', options: ['Yes', 'No'], recommended: 1 }]); });
  assert.deepEqual(spoken, ['alpha asks you to decide: Ship today? 2 options: Yes, which it recommends; No.']);
  assert.equal(prefs.store.get('tclaude.dash.companion.voice.decision.' + A), '9', 'claimed, so other dashboard tabs stay quiet');
  await harness.act(async () => { snap.value = withMessages([first, { id: 9, from_agent: A, subject: 'Ship today?', options: ['Yes', 'No'] }]); });
  assert.equal(spoken.length, 1, 'said once');

  const voice = await harness.importDashboardModule('js/companion-voice.js');
  assert.deepEqual(voice.decisionArrivals(new Map([[A, 7], [B, 0]]), new Map([[A, 9], [B, 0], ['new', 4]])), [A]);
});

test('dictation fills the draft and sends when the operator ends with "send"', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  let callbacks = null;
  let started = 0;
  const dictationImpl = (cb) => { callbacks = cb; return { supported: true, start: () => { started += 1; cb.onState(true); }, stop: () => cb.onState(false) }; };
  let failNext = false;
  const requests = [];
  const fetchImpl = async (url, init = {}) => {
    requests.push({ url: String(url), body: init.body ? JSON.parse(init.body) : null });
    if (failNext) return { ok: false, status: 502, statusText: 'Bad Gateway', json: async () => ({ message: 'agent unreachable' }) };
    return { ok: true, status: 202, json: async () => ({ id: 1 }) };
  };
  const { view, harness, controller } = await setup(t, { prefs, dictationImpl, fetchImpl });
  await harness.act(() => controller().open(A));
  const mic = harness.getByRole(view.container, 'button', { name: /Dictate a message to alpha/ });
  await harness.act(() => mic.click());
  assert.equal(started, 1);
  assert.equal(mic.getAttribute('aria-pressed'), 'true');
  await harness.act(() => callbacks.onText('what can I do'));
  assert.equal(view.container.querySelector('input.companion-draft').value, 'what can I do');
  await harness.act(async () => { callbacks.onSend('what can I do here'); await new Promise((r) => setTimeout(r, 0)); });
  const mail = requests.find((r) => r.url === '/api/operator-message');
  assert.match(mail.body.body, /^what can I do here\n\n\[dashboard view:/);
  const voice = await harness.importDashboardModule('js/companion-voice.js');
  assert.equal(voice.isSendCommand('send'), true);
  assert.equal(voice.isSendCommand(' Send it. '), true);
  assert.equal(voice.isSendCommand('I want to send'), false, 'a pause after the verb is not the command');
  assert.equal(voice.isSendCommand("don't send"), false);
  // A failed send keeps the dictated draft.
  failNext = true;
  await harness.act(() => mic.click());
  await harness.act(() => callbacks.onText('keep me'));
  await harness.act(async () => { callbacks.onSend('keep me'); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(view.container.querySelector('input.companion-draft').value, 'keep me');
});

test('an agent cue draws marks on the operator view and names the ones it cannot find', async (t) => {
  const harness = await createPreactHarness(t);
  const { CompanionApp } = await harness.importDashboardModule('js/companion-island.js');
  const guide = await harness.importDashboardModule('js/companion-guide.js');
  const doc = harness.document;
  const box = (text, left) => {
    const el = doc.body.appendChild(doc.createElement('button'));
    el.textContent = text;
    el.getBoundingClientRect = () => ({ left, top: 100, right: left + 80, bottom: 124, width: 80, height: 24 });
    return el;
  };
  box('Review', 100);
  box('Rework', 400);
  const snap = harness.signals.signal({ groups: [], companion_cues: [] });
  const fetchImpl = async () => ({ ok: true, status: 204, json: async () => ({}) });
  const view = await harness.mount(harness.html`<${CompanionApp} snapshot=${snap} activeTab=${harness.signals.signal('groups')}
    prefs=${memoryPrefs()} fetchImpl=${fetchImpl} widgetFactory=${() => ({})} registerController=${() => () => {}} />`);
  await harness.act(async () => {
    snap.value = { groups: [], companion_cues: [{ id: 7, agent_name: 'steward', note: 'Rework never loops back', marks: [
      { kind: 'circle', target: 'Review' }, { kind: 'arrow', from: 'Rework', target: 'Review' }, { kind: 'underline', target: 'Nowhere' },
    ] }] };
    await new Promise((r) => setTimeout(r, 50));
  });
  // A mark that stays missing does not hold up the rest for the full timeout.
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 2000)); });
  const paths = [...doc.querySelectorAll('.companion-guide-marks path')];
  assert.deepEqual(paths.map((p) => p.getAttribute('class')), ['mark-circle', 'mark-arrow']);
  assert.match(doc.querySelector('.companion-guide-note').textContent, /Couldn't find “Nowhere”/);
  const rect = { left: 100, top: 100, width: 80, height: 24 };
  assert.match(guide.markPath('underline', rect), /^M96\.0,129\.0 Q/);
  assert.equal(guide.markPath('arrow', rect, null), '', 'an arrow needs both ends');
  // Loose finding: an exact graph node beats a partial control, and the
  // innermost element of equal text wins over its row.
  const sized = (el) => { el.getBoundingClientRect = () => ({ left: 0, top: 0, right: 10, bottom: 10, width: 10, height: 10 }); return el; };
  const row = sized(doc.body.appendChild(doc.createElement('div')));
  const cell = sized(row.appendChild(doc.createElement('span')));
  cell.textContent = 'Merge';
  sized(doc.body.appendChild(doc.createElement('button'))).textContent = 'Merge now';
  assert.equal(guide.findGuideTarget(doc, 'Merge', { loose: true }), cell);
  assert.equal(guide.findGuideTarget(doc, 'Merge').textContent, 'Merge now', 'strict finding stays on controls');
  await view.unmount();
});

test('the Steward goes by the name the operator gave it', async () => {
  const { companionAgents } = await import('../dashboard/js/companion-model.js');
  const agents = companionAgents({
    steward_name: 'Jeeves',
    groups: [
      { name: 'steward', scribe: true, members: [{ agent_id: A, title: 'jeeves-1234', online: true }] },
      { name: 'team', members: [{ agent_id: B, title: 'worker', online: true }] },
    ],
  });
  assert.equal(agents.get(A).name, 'Jeeves');
  assert.equal(agents.get(B).name, 'worker', 'only the Steward');
});
