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
  return { groups, ...(steward ? { steward_roster: [{ agent_id: S, name: 'steward-k3x', seat: 'active', online: true }] } : {}) };
}

// A hand-over's claim is asynchronous and starts as an act's render ends.
const settleClaim = (harness) => harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });

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

// A dictation fake: the only way text reaches the draft field, which appears
// while there is some.
function fakeDictation() {
  const fake = { callbacks: null };
  fake.impl = (cb) => { fake.callbacks = cb; return { supported: true, start: () => cb.onState(true), stop: () => cb.onState(false), abort: () => cb.onState(false) }; };
  fake.say = async (harness, view, text) => {
    await harness.act(() => view.container.querySelector('.companion-mic').click());
    await harness.act(() => fake.callbacks.onText(text));
    await harness.act(() => view.container.querySelector('.companion-mic').click());
  };
  return fake;
}

const SHOWN_WINDOW = '.companion-window:not(.companion-window-warm)';

async function setup(t, { snapshot = snapshotWith(), prefs = memoryPrefs(), sessionPrefs = memoryPrefs(), onComposeMessage = null, openMorph = null, openPermissions = null, openSudo = null, detachWindow = null, openTerminalsPane = null, openStewardHandover = null, fetchImpl, speakerImpl = null, dictationImpl = null, notify = () => {}, locks = null } = {}) {
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
    snapshot=${snap} activeTab=${activeTab} prefs=${prefs} sessionPrefs=${sessionPrefs} fetchImpl=${fetchFn} notify=${notify}
    locks=${locks} onComposeMessage=${onComposeMessage} openMorph=${openMorph} openPermissions=${openPermissions} openSudo=${openSudo} detachWindow=${detachWindow} openTerminalsPane=${openTerminalsPane} openStewardHandover=${openStewardHandover} widgetFactory=${fakeWidgetFactory(mounted)} speakerImpl=${speakerImpl} dictationImpl=${dictationImpl} closeTerminalPanes=${(selectors) => closed.push(selectors)}
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

test('a live Steward\'s title bar opens the permissions editor and the sudo dialog', async (t) => {
  const opened = [];
  const { view, harness, controller } = await setup(t, {
    snapshot: snapshotWith({ steward: true }),
    openPermissions: (conv, label) => opened.push(['permissions', conv, label]),
    openSudo: (agent) => opened.push(['sudo', agent]),
  });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.companion-steward-perms').click());
  await harness.act(() => view.container.querySelector('.companion-steward-sudo').click());
  assert.deepEqual(opened, [['permissions', 'conv-s', 'steward-k3x'], ['sudo', S]]);
});

test('a Steward that stopped keeps the access controls disabled, with the reason', async (t) => {
  const snapshot = snapshotWith({ steward: true });
  const { view, harness, controller, snap } = await setup(t, { snapshot, openPermissions: () => {}, openSudo: () => {} });
  await harness.act(() => controller().open(S));
  const stopped = structuredClone(snapshot);
  stopped.groups.find((group) => group.name === 'steward').members[0].online = false;
  await harness.act(() => { snap.value = stopped; });
  const permissions = view.container.querySelector('.companion-steward-perms');
  const sudo = view.container.querySelector('.companion-steward-sudo');
  assert.equal(permissions.disabled, true);
  assert.equal(sudo.disabled, true);
  assert.match(permissions.title, /not running/);
});

test('shutting a Steward down asks first, retires it, and offers a new one', async (t) => {
  const calls = [];
  const fetchImpl = async (url, options = {}) => { calls.push([options.method, url]); return { ok: true, text: async () => '', json: async () => ({}) }; };
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.steward-shutdown-open').click());
  const stops = () => calls.filter(([, url]) => url.endsWith('/stop'));
  assert.deepEqual(stops(), [], 'nothing happens before the confirmation');
  assert.match(view.container.querySelector('.steward-shutdown-confirm').textContent, /keeps its conversation/);
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .danger').click(); await Promise.resolve(); await Promise.resolve(); });
  assert.deepEqual(stops(), [['POST', `/api/agents/${encodeURIComponent(S)}/stop`]], 'it stops; it is not retired');
  assert.equal(calls.some(([, url]) => url.endsWith('/retire')), false);
  assert.match(view.container.querySelector('.companion-shutdown-note').textContent, /is shut down/);
  assert.match(view.container.querySelector('.companion-resume-steward').textContent, /Resume/);
  assert.match(view.container.querySelector('.companion-fresh-steward').textContent, /Start fresh/);
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); await Promise.resolve(); await Promise.resolve(); });
  assert.equal(calls.at(-1)[1], `/api/agents/${encodeURIComponent(S)}/resume`, 'Resume is the same agent');
});

test('a Steward tab × only minimizes, and the tab stays pinned', async (t) => {
  const prefs = memoryPrefs();
  const { view, harness, controller } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  const tab = view.container.querySelector('.companion-tab-close');
  assert.match(tab.title, /Minimize/);
  await harness.act(() => tab.click());
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null, 'the window went away');
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.tabs')), [S], 'still pinned');
  assert.equal(view.container.querySelector('.steward-shutdown-confirm'), null, 'nothing asked');
});

test('the window ✕ on a Steward asks what ⏻ asks, then shuts it down and closes', async (t) => {
  const calls = [];
  const fetchImpl = async (url, options = {}) => { calls.push([options.method, url]); return { ok: true, text: async () => '', json: async () => ({}) }; };
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  const close = () => view.container.querySelector('.overlay-wc-close');
  const stops = () => calls.filter(([, url]) => url.endsWith('/stop'));
  assert.match(close().title, /shut it down/);
  await harness.act(() => close().click());
  assert.match(view.container.querySelector('.steward-shutdown-confirm').textContent, /keeps its conversation/);
  assert.ok(view.container.querySelector(SHOWN_WINDOW), 'open while it asks');
  await harness.act(() => view.container.querySelector('.steward-shutdown-actions button:not(.danger)').click());
  assert.deepEqual(stops(), [], 'Cancel keeps it running');
  assert.ok(view.container.querySelector(SHOWN_WINDOW));

  await harness.act(() => close().click());
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(stops(), [['POST', `/api/agents/${encodeURIComponent(S)}/stop`]]);
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null, 'the window closed');
});

test('⏻ after a cancelled ✕ still keeps the window on the way back', async (t) => {
  const fetchImpl = async () => ({ ok: true, text: async () => '', json: async () => ({}) });
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.overlay-wc-close').click());
  await harness.act(() => view.container.querySelector('.steward-shutdown-actions button:not(.danger)').click());
  await harness.act(() => view.container.querySelector('.steward-shutdown-open').click());
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(view.container.querySelector(SHOWN_WINDOW));
  assert.match(view.container.querySelector('.companion-resume-steward').textContent, /Resume/);
});

test('a floated Steward: its window ✕ asks too, and Cancel keeps it running', async (t) => {
  const calls = [];
  const fetchImpl = async (url, options = {}) => { calls.push([options.method, url]); return { ok: true, text: async () => '', json: async () => ({}) }; };
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.companion-float').click());
  const floated = () => view.container.querySelector('.chat-window');
  const stops = () => calls.filter(([, url]) => url.endsWith('/stop'));
  assert.match(floated().querySelector('.overlay-wc-close').title, /shut it down/);
  await harness.act(() => floated().querySelector('.overlay-wc-close').click());
  assert.ok(floated(), 'still open while it asks');
  await harness.act(() => floated().querySelector('.steward-shutdown-actions button:not(.danger)').click());
  assert.ok(floated(), 'Cancel leaves it');
  assert.deepEqual(stops(), []);

  await harness.act(() => floated().querySelector('.overlay-wc-close').click());
  await harness.act(async () => { floated().querySelector('.steward-shutdown-actions .danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(stops(), [['POST', `/api/agents/${encodeURIComponent(S)}/stop`]]);
  assert.equal(floated(), null, 'the window closed');
});

test('shutting down a floated Steward leaves an open companion on Resume', async (t) => {
  const fetchImpl = async () => ({ ok: true, text: async () => '', json: async () => ({}) });
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.companion-float').click());
  await harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  assert.ok(view.container.querySelector(SHOWN_WINDOW), 'the companion is open, with no tab');
  const floated = () => view.container.querySelector('.chat-window');
  await harness.act(() => floated().querySelector('.overlay-wc-close').click());
  await harness.act(async () => { floated().querySelector('.steward-shutdown-actions .danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(floated(), null);
  assert.ok(view.container.querySelector(SHOWN_WINDOW), 'still open');
  assert.match(view.container.querySelector('.companion-resume-steward').textContent, /Resume/);
});

test('a Terminals tab floats its agent as a chat, and a parked tab goes with it', async (t) => {
  const { view, harness, controller } = await setup(t, { openTerminalsPane: () => {} });
  const tabOfAlpha = () => [...view.container.querySelectorAll('.companion-tab')].find((tab) => tab.textContent.includes('alpha'));
  await harness.act(() => controller().open(A));
  await harness.act(() => view.container.querySelector('[aria-label="Open in the Collab tab"]').click());
  assert.ok(tabOfAlpha(), 'parked in the companion while it is in the Collab tab');
  let floated = false;
  await harness.act(() => { floated = controller().float('conv-a'); });
  assert.equal(floated, true, 'a pane may hold the conv id');
  assert.ok(view.container.querySelector('.chat-window .companion-term-host'), 'the chat window has the terminal');
  assert.equal(tabOfAlpha(), undefined, 'no second place for it');
  assert.equal(controller().float('agt_nobody'), false);
});

test('an agent that never had a tab floats without touching the companion', async (t) => {
  const { view, harness, controller } = await setup(t);
  await harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  assert.ok(view.container.querySelector(SHOWN_WINDOW), 'open, with no tab');
  await harness.act(() => controller().float(A));
  assert.ok(view.container.querySelector('.chat-window'));
  assert.ok(view.container.querySelector(`.companion-window:not(.chat-window):not(.companion-window-warm)`), 'still open');
});

test('a floated agent that is no Steward still closes at once', async (t) => {
  const { view, harness, controller } = await setup(t);
  await harness.act(() => controller().open(A));
  await harness.act(() => view.container.querySelector('.companion-float').click());
  const floated = () => view.container.querySelector('.chat-window');
  assert.match(floated().querySelector('.overlay-wc-close').title, /keeps running/);
  assert.equal(floated().querySelector('.steward-shutdown-open'), null);
  await harness.act(() => floated().querySelector('.overlay-wc-close').click());
  assert.equal(floated(), null);
});

test('a refused shut-down stays on the Steward and says why', async (t) => {
  const fetchImpl = async () => ({ ok: false, status: 409, text: async () => '{"message":"nothing to retire"}', json: async () => ({}) });
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.steward-shutdown-open').click());
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .danger').click(); await new Promise((resolve) => { setTimeout(resolve, 0); }); });
  assert.match(view.container.querySelector('.steward-shutdown-error').textContent, /nothing to retire/);
  assert.equal(view.container.querySelector('.companion-shutdown-note'), null);
});

test('the access controls are the Steward\'s alone', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, controller } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }), openPermissions: () => {}, openSudo: () => {} });
  await harness.act(() => controller().open(A));
  assert.ok(harness.getByRole(view.container, 'tab', { name: /alpha/ }));
  assert.equal(view.container.querySelector('.companion-steward-perms'), null);
  assert.equal(view.container.querySelector('.companion-steward-sudo'), null);
  assert.equal(view.container.querySelector('.steward-shutdown-open'), null);
});

test('with no Steward running the empty window says where its access controls will be', async (t) => {
  const { view, harness } = await setup(t, { openPermissions: () => {}, openSudo: () => {} });
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Open the companion/ }).click());
  assert.equal(view.container.querySelector('.companion-steward-perms'), null);
  assert.match(view.container.textContent, /Once it is running, ⚙ in the title bar changes its access/);
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
  assert.equal(post.body.brief, undefined, 'the daemon writes a Steward\'s brief');
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
  const dictated = fakeDictation();
  const { view, harness, requests, controller } = await setup(t, { prefs, dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  await dictated.say(harness, view, 'what can I do here?');
  const field = view.container.querySelector('input[aria-label="Message alpha"]');
  await harness.act(async () => {
    field.closest('form').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
  const mail = requests.find((r) => r.url === '/api/operator-message');
  assert.ok(mail, 'operator mail was posted');
  assert.equal(mail.body.to, A);
  assert.match(mail.body.body, /^\[dictated\] what can I do here\?\n\n\[dashboard view: Processes \(/);
});

test('a minimized window reopens on the same terminal, and releases it after a while', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, mounted } = await setup(t, { prefs });
  const key = () => harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  const settle = () => harness.act(async () => { await Promise.resolve(); await Promise.resolve(); });
  await key();
  await settle();
  assert.equal(mounted.length, 1);
  await key();
  await settle();
  assert.ok(view.container.querySelector('.companion-window-warm'), 'hidden, not removed');
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null);
  t.mock.timers.tick(60 * 1000);
  await key();
  await settle();
  assert.ok(view.container.querySelector(SHOWN_WINDOW));
  assert.equal(mounted.length, 1, 'no new terminal');
  assert.equal(mounted[0].disposed, false);
  await key();
  await settle();
  await harness.act(async () => { t.mock.timers.tick(5 * 60 * 1000); });
  await settle();
  assert.equal(view.container.querySelector('.companion-window'), null, 'released');
  assert.ok(mounted[0].disposed);
});

test('a minimized window never attaches a terminal of its own', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]), 'tclaude.dash.companion.active': A });
  const { view, harness, mounted, snap } = await setup(t, { prefs });
  const key = () => harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  const settle = () => harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  await key();
  await settle();
  await key();
  await settle();
  const attached = mounted.length;
  assert.equal(attached, 1);
  assert.ok(view.container.querySelector('.companion-window-warm'));
  const before = snap.value;
  const active = mounted[0].options.wsPath.endsWith(A) ? A : B;
  const withoutActive = { ...before, groups: before.groups.map((g) => ({ ...g, members: g.members.filter((x) => x.agent_id !== active) })) };
  await harness.act(() => { snap.value = withoutActive; });
  await settle();
  assert.equal(mounted.length, attached, 'the next tab does not attach while hidden');
  assert.equal(view.container.querySelector('.companion-window'), null, 'the hidden window is gone');
  await harness.act(() => { snap.value = before; });
  await settle();
  assert.equal(mounted.length, attached, 'coming back does not attach either');
});

test('a click on empty window space gives focus back to the terminal', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, mounted } = await setup(t, { prefs });
  await harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const titlebar = view.container.querySelector('.companion-titlebar');
  const click = () => harness.act(() => titlebar.click());
  harness.window.document.activeElement?.blur?.();
  const before = mounted.at(-1).focused;
  await click();
  assert.equal(mounted.at(-1).focused, before + 1, 'focus returns to the terminal');
  const button = view.container.querySelector('.companion-window button');
  await harness.act(async () => { button.focus(); });
  await click();
  assert.equal(mounted.at(-1).focused, before + 1, 'focus already in the window stays where it is');
  harness.window.document.activeElement?.blur?.();
  await harness.act(() => view.container.querySelector('.companion-add').click());
  assert.equal(mounted.at(-1).focused, before + 1, 'a control keeps its click, which may open a dialog');
});

test('Ctrl+. toggles the window and unpinning the last tab closes it', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, mounted } = await setup(t, { prefs });
  const key = () => harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  await key();
  assert.ok(view.container.querySelector('.companion-window'), 'window opened');
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  await key();
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null, 'window minimized');
  assert.equal(mounted[0].disposed, false, 'a minimized window keeps its terminal attached');
  await key();
  await harness.act(() => harness.getByRole(view.container, 'button', { name: 'Unpin alpha' }).click());
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null);
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.tabs')), []);
});

test('viewportOf leaves out page scrollbars', async (t) => {
  const harness = await createPreactHarness(t);
  const o = await harness.importDashboardModule('js/overlay-window-model.js');
  const win = { innerWidth: 1280, innerHeight: 800, document: { documentElement: { clientWidth: 1265, clientHeight: 785 } } };
  assert.deepEqual(o.viewportOf(win), { width: 1265, height: 785 });
  assert.deepEqual(o.viewportOf({ innerWidth: 1000, innerHeight: 700 }), { width: 1000, height: 700 });
  assert.deepEqual(o.viewportOf(null), { width: 1280, height: 800 });
});

test('placement model: clamping, snap zones and status folding', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-model.js');
  const o = await harness.importDashboardModule('js/overlay-window-model.js');
  const vp = { width: 1200, height: 800 };
  const clamped = o.clampPlacement({ x: 5000, y: -40, w: 100, h: 5000 }, vp);
  assert.equal(clamped.w, o.OVERLAY_MIN_W);
  assert.ok(clamped.x <= vp.width - 80 && clamped.y === 0 && clamped.h <= vp.height);
  assert.equal(o.snapZone({ x: 1195, y: 300 }, vp), 'right');
  assert.equal(o.snapZone({ x: 3, y: 300 }, vp), 'left');
  assert.equal(o.snapZone({ x: 600, y: 790 }, vp), 'bottom');
  assert.equal(o.snapZone({ x: 600, y: 300 }, vp), '');
  const right = o.zoneRect('right', vp, 60);
  assert.equal(right.x + right.w, vp.width);
  assert.equal(right.y, 60);
  assert.deepEqual(o.zoneRect('max', vp, 60), { x: 0, y: 60, w: vp.width, h: vp.height - o.OVERLAY_FOOTER_ROOM - 60, snap: 'max' });
  assert.equal(o.parsePlacement(JSON.stringify({ x: 1, y: 2, w: 3, h: 4, snap: 'max' })).snap, 'max');
  const narrow = o.zoneRect('right', { width: 300, height: 800 }, 0);
  assert.deepEqual([narrow.x, narrow.w], [0, 300], 'a snapped panel never outgrows a narrow viewport');
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
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null);
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: [{ id: 3, agent_name: 'steward', open: B }] };
    await new Promise((r) => setTimeout(r, 0));
  });
  await settleClaim(harness);
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const tab = harness.getByRole(view.container, 'tab', { name: /bravo/ });
  assert.equal(tab.getAttribute('aria-selected'), 'true', 'bravo is pinned and shown');
  assert.equal(harness.window.document.querySelector('.companion-guide-note'), null, 'an open-only cue draws nothing');
});

test('an open cue only pins the agent while the operator is typing to another tab', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictated = fakeDictation();
  const { view, harness, snap, controller } = await setup(t, { prefs, dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  await dictated.say(harness, view, 'half a thought');
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: [{ id: 4, agent_name: 'steward', open: B }] };
    await new Promise((r) => setTimeout(r, 0));
  });
  await settleClaim(harness);
  assert.equal(harness.getByRole(view.container, 'tab', { name: /alpha/ }).getAttribute('aria-selected'), 'true', 'the draft keeps its agent');
  assert.equal(harness.getByRole(view.container, 'tab', { name: /bravo/ }).getAttribute('aria-selected'), 'false', 'bravo is pinned beside it');
  assert.equal(view.container.querySelector('input.companion-draft').value, 'half a thought');
});

test('a draft stays with its agent across tab switches and goes to that agent', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]) });
  const dictated = fakeDictation();
  const { view, harness, requests, controller } = await setup(t, { prefs, dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  const draft = () => view.container.querySelector('input.companion-draft');
  assert.equal(draft(), null, 'there is no text field until there is text');
  await dictated.say(harness, view, 'for alpha');
  await harness.act(() => harness.getByRole(view.container, 'tab', { name: /bravo/ }).click());
  assert.equal(draft(), null, 'bravo does not inherit the draft');
  await harness.act(() => harness.getByRole(view.container, 'tab', { name: /alpha/ }).click());
  assert.equal(draft().value, 'for alpha');
  await harness.act(async () => {
    view.container.querySelector('form.companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.equal(requests.find((r) => r.url === '/api/operator-message').body.to, A);
  assert.equal(draft(), null);
});

test('sending from the field hands focus to the terminal, and emptying it keeps it', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictated = fakeDictation();
  const { view, harness, controller, mounted } = await setup(t, { prefs, dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const draft = () => view.container.querySelector('input.companion-draft');
  await dictated.say(harness, view, 'for alpha');
  await harness.act(async () => { draft().blur(); draft().focus(); });
  await harness.act(async () => {
    draft().value = '';
    draft().dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  });
  assert.ok(draft(), 'emptying the field by hand leaves it under the caret');
  await harness.act(async () => {
    draft().value = 'for alpha';
    draft().dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  });
  const before = mounted.at(-1).focused;
  await harness.act(async () => {
    view.container.querySelector('form.companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.equal(draft(), null);
  assert.equal(mounted.at(-1).focused, before + 1, 'the keyboard is not left on the page');
});

test('a finished send leaves text typed after it started', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  let finish = null;
  const fetchImpl = (url) => (String(url) === '/api/operator-message'
    ? new Promise((resolve) => { finish = () => resolve({ ok: true, status: 202, json: async () => ({ id: 1 }) }); })
    : Promise.resolve({ ok: true, status: 200, json: async () => ({}) }));
  const dictated = fakeDictation();
  const { view, harness, controller } = await setup(t, { prefs, fetchImpl, dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  const draft = () => view.container.querySelector('input.companion-draft');
  await dictated.say(harness, view, 'first');
  await harness.act(async () => {
    view.container.querySelector('form.companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
  await harness.input(draft(), 'second');
  await harness.act(async () => { finish(); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(draft().value, 'second');
});

test('an open cue takes over a terminal that only holds focus', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, snap, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const keys = harness.window.document.createElement('textarea');
  view.container.querySelector('.companion-term-host').appendChild(keys);
  keys.focus();
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: [{ id: 4, agent_name: 'steward', open: B }] };
    await new Promise((r) => setTimeout(r, 0));
  });
  await settleClaim(harness);
  assert.equal(harness.getByRole(view.container, 'tab', { name: /bravo/ }).getAttribute('aria-selected'), 'true');
});

test('an open cue only pins the agent while the operator is typing in the terminal', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, snap, controller, mounted } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const keys = harness.window.document.createElement('textarea');
  view.container.querySelector('.companion-term-host').appendChild(keys);
  keys.focus();
  keys.dispatchEvent(new harness.window.Event('keydown', { bubbles: true }));
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: [{ id: 4, agent_name: 'steward', open: B }] };
    await new Promise((r) => setTimeout(r, 0));
  });
  await settleClaim(harness);
  assert.equal(harness.getByRole(view.container, 'tab', { name: /alpha/ }).getAttribute('aria-selected'), 'true');
  assert.equal(harness.getByRole(view.container, 'tab', { name: /bravo/ }).getAttribute('aria-selected'), 'false');
  assert.equal(mounted[0].disposed, false, 'the terminal being typed in stays');
});

test('every hand-over in a snapshot opens, and a second dashboard tab takes none of them', async (t) => {
  const prefs = memoryPrefs();
  const { view, harness, snap } = await setup(t, { prefs });
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  const cues = [{ id: 1, agent_name: 'steward', open: A, at: 't1' }, { id: 2, agent_name: 'steward', open: B, at: 't2' }];
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: cues };
    await new Promise((r) => setTimeout(r, 0));
  });
  await settleClaim(harness);
  assert.equal(harness.getByRole(view.container, 'tab', { name: /alpha/ }).getAttribute('aria-selected'), 'false');
  assert.equal(harness.getByRole(view.container, 'tab', { name: /bravo/ }).getAttribute('aria-selected'), 'true', 'the newest shows, the earlier stays pinned');
  const g = await harness.importDashboardModule('js/companion-guide.js');
  const stored = prefs.store.get('tclaude.dash.companion.cue-open');
  assert.deepEqual(cues.map((cue) => g.claimCueOpen(stored, cue)), [false, false]);
  assert.equal(g.claimCueOpen(stored, { id: 3, at: 't3' }), true);
});

test('the Web Lock lets one of two tabs take a hand-over, and a missing API leaves it to prefs', async (t) => {
  const harness = await createPreactHarness(t);
  const { claimCueLock } = await harness.importDashboardModule('js/companion-guide.js');
  const cue = { id: 7, at: 't7' };
  // The real Web Locks API, held across "tabs" that share one lock manager.
  if (globalThis.navigator?.locks) {
    const locks = globalThis.navigator.locks;
    const unique = { id: 7, at: `t7-${process.pid}-${Date.now()}` };
    assert.deepEqual(await Promise.all([claimCueLock(locks, unique), claimCueLock(locks, unique)]), [true, false]);
    assert.equal(await claimCueLock(locks, unique), false, 'a tab seeing the cue later still loses');
    assert.equal(await claimCueLock(locks, { ...unique, id: 8 }), true);
  }
  assert.equal(await claimCueLock(undefined, cue), true, 'no Web Locks: degrade to the prefs claim');
  assert.equal(await claimCueLock({ request: () => Promise.reject(new Error('SecurityError')) }, cue), true);
  assert.equal(await claimCueLock({ request: () => { throw new Error('SecurityError'); } }, cue), true);
});

test('a hand-over another tab holds opens nothing here, shown or held', async (t) => {
  const prefs = memoryPrefs();
  const held = new Set();
  const locks = { request: async (name, _options, grant) => {
    const free = !held.has(name);
    if (free) held.add(name);
    return grant(free ? { name } : null);
  } };
  const { view, harness, snap } = await setup(t, { prefs, locks });
  const settle = (cues) => harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: cues };
    await new Promise((r) => setTimeout(r, 0));
  }).then(() => settleClaim(harness));
  await settle([]);
  const taken = { id: 5, agent_name: 'steward', open: B, at: new Date().toISOString() };
  held.add(`tclaude.companion.cue-open/5@${taken.at}`);
  await settle([taken]);
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null, 'the other tab has it');
  assert.equal(prefs.store.get('tclaude.dash.companion.cue-open'), undefined);

  const doc = harness.window.document;
  let visibility = 'hidden';
  Object.defineProperty(doc, 'visibilityState', { configurable: true, get: () => visibility });
  const heldCue = { id: 6, agent_name: 'steward', open: B, at: new Date().toISOString() };
  await settle([taken, heldCue]);
  held.add(`tclaude.companion.cue-open/6@${heldCue.at}`);
  visibility = 'visible';
  await harness.act(async () => { doc.dispatchEvent(new harness.window.Event('visibilitychange')); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null, 'a held hand-over another tab took meanwhile is dropped');

  const free = { id: 7, agent_name: 'steward', open: B, at: new Date().toISOString() };
  await settle([taken, heldCue, free]);
  assert.equal(harness.getByRole(view.container, 'tab', { name: /bravo/ }).getAttribute('aria-selected'), 'true', 'a free lock opens it here');
  assert.match(prefs.store.get('tclaude.dash.companion.cue-open'), /^7@/);
});

test('a cue from a restarted daemon is new even though its id was seen', async (t) => {
  const harness = await createPreactHarness(t);
  const g = await harness.importDashboardModule('js/companion-guide.js');
  const seen = new Set();
  assert.deepEqual(g.newCues(seen, [{ id: 1, at: 't1' }], true).map((c) => c.at), ['t1']);
  assert.deepEqual(g.newCues(seen, [{ id: 1, at: 't1' }], true), []);
  assert.deepEqual(g.newCues(seen, [{ id: 1, at: 't2' }], true).map((c) => c.at), ['t2']);
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
  await settleClaim(harness);
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null, 'nothing opens while hidden');
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
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null, 'a helper appearing does not open the window');

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

test("a peer's page has no companion window but stays reachable for capture and marks", async (t) => {
  const harness = await createPreactHarness(t);
  const { CompanionGate } = await harness.importDashboardModule('js/companion-island.js');
  globalThis.__tclaudeRemoteNode = { id: 'inst_forge7' };
  t.after(() => { delete globalThis.__tclaudeRemoteNode; });
  const urls = [];
  const fetchImpl = async (url) => {
    urls.push(url);
    return { ok: true, status: 200, json: async () => ({ marks: [] }) };
  };
  const snap = harness.signals.signal({ ...snapshotWith(), collab_enabled: false, peer_view: { level: 'restricted' } });
  const view = await harness.mount(harness.html`<${CompanionGate} snapshot=${snap} activeTab=${harness.signals.signal('groups')} prefs=${memoryPrefs()} sessionPrefs=${memoryPrefs()}
    fetchImpl=${fetchImpl} widgetFactory=${fakeWidgetFactory([])} closeTerminalPanes=${() => {}} />`);
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); });
  assert.equal(view.container.querySelector('.companion-chips'), null, 'no window on a peer page');
  assert.ok(urls.includes('/api/companion/presence'), 'the tab still reports itself');
  assert.ok(urls.includes('/api/companion/view'), 'and which view it is on, so guide view is not left on a local one');
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
  assert.equal(view.container.querySelector(SHOWN_WINDOW), null, 'the window stays minimized');
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

  const secret = harness.document.body.appendChild(harness.document.createElement('div'));
  secret.innerHTML = '<p id="masked-card">Invite: <code data-capture-mask>sk-live-s3cret</code></p>';
  const code = secret.querySelector('code');
  assert.equal(m.labelFor(code), null, 'a marked secret is not named by its text');
  assert.equal(m.labelFor(secret.querySelector('#masked-card')), null, 'nor by the text around it');
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

test('free-hand strokes read as drawings, and a run of letters as handwriting', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-annotate.js');
  const scribble = [[0, 0], [40, 30], [10, 50], [60, 70], [20, 90], [90, 100]];
  assert.equal(m.classifyStroke(scribble), 'drawing', 'it wanders, so it is no arrow');
  assert.equal(m.classifyStroke([[0, 0], [60, 40], [120, 90]]), 'arrow');
  const at = () => ({ text: 'git status', role: '' });
  assert.equal(m.describeStroke(scribble, at), 'drew by hand over “git status”');
  const stroke = (x, startedAt, points) => ({ ...m.resolveStroke(points, at), points, startedAt, endedAt: startedAt + 100 });
  const line = (x) => [[x, 0], [x, 40], [x, 90]];
  const letters = [stroke(0, 0, line(0)), stroke(30, 300, line(30)), stroke(60, 600, line(60))];
  assert.deepEqual(letters.map((s) => s.kind), ['arrow', 'arrow', 'arrow'], 'alone, a tall letter looks like an arrow');
  assert.deepEqual(m.readStrokes(letters).map((s) => s.kind), ['drawing', 'drawing', 'drawing']);
  const apart = [stroke(0, 0, line(0)), stroke(30, 5000, line(30)), stroke(60, 10000, line(60))];
  assert.deepEqual(m.readStrokes(apart).map((s) => s.kind), ['arrow', 'arrow', 'arrow'], 'arrows drawn one at a time stay arrows');
  const far = [stroke(0, 0, line(0)), stroke(500, 300, line(500)), stroke(1000, 600, line(1000))];
  assert.deepEqual(m.readStrokes(far).map((s) => s.kind), ['arrow', 'arrow', 'arrow'], 'quick strokes far apart are no word');
  assert.equal(m.composeAnnotation({ lines: ['a', 'a', 'a', 'b'], image: true }),
    'The operator marked up the dashboard view they are on. The attached image shows the view with their ink on it, numbered as below:\n1–3. a\n4. b');
});

test('the ink goes onto the image as the overlay shows it', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-annotate.js');
  const calls = [];
  const ctx = new Proxy({}, {
    get: (_, name) => (...args) => calls.push([name, ...args]),
    set: (_, name, value) => { calls.push([`set ${String(name)}`, value]); return true; },
  });
  m.drawInk(ctx, [
    { color: '#ff4d4f', kind: 'drawing', points: [[10, 20], [30, 40]] },
    { color: '#4dabf7', kind: 'arrow', points: [[50, 60], [150, 60]] },
  ], 2);
  assert.deepEqual(calls.find(([name]) => name === 'scale'), ['scale', 2, 2], 'stroke points are in CSS pixels');
  assert.deepEqual(calls.filter(([name]) => name === 'set strokeStyle').map(([, c]) => c), ['#ff4d4f', '#4dabf7']);
  assert.deepEqual(calls.filter(([name]) => name === 'fillText').map(([, text]) => text), ['1', '2'], 'numbered as the lines are');
  assert.equal(calls.filter(([name]) => name === 'stroke').length, 3, 'only the arrow gets a head');
});

test('a markup sends the view as an image, and says so when it cannot', async (t) => {
  const harness = await createPreactHarness(t);
  const tools = await harness.importDashboardModule('js/dictation-tools.js');
  const run = async ({ render, target = {}, uploadOk = true, imageOk = true, strokes = [{ color: '#fff', kind: 'drawing', line: 'drew by hand over “x”', points: [[1, 1], [9, 9]] }] }) => {
    const calls = [];
    const notes = [];
    const fetchImpl = async (url, init) => {
      calls.push({ url, init });
      if (url === '/api/spawn-attachments') return { ok: uploadOk, status: uploadOk ? 200 : 500, statusText: 'x', json: async () => (uploadOk ? { token: 'tok' } : {}) };
      if (!imageOk && JSON.parse(init.body).attachment_token) return { ok: false, status: 500, statusText: 'x', json: async () => ({ error: 'attachments total too large' }) };
      return { ok: true, status: 202, json: async () => ({}) };
    };
    let hook;
    function Probe() { hook = tools.useAnnotation({ win: harness.window, notify: (text, error) => notes.push({ text, error }), viewNow: () => null, render }); return null; }
    const view = await harness.mount(harness.html`<${Probe} />`);
    await harness.act(() => hook.start({ agentId: 'a1', name: 'alpha', fetchImpl, ...target }));
    await harness.act(() => hook.overlay.props.onSend({ note: '', lines: strokes.map((s) => s.line), strokes }));
    await view.unmount();
    const mail = calls.findLast((c) => c.url === '/api/operator-message');
    return { calls, notes, mail: mail && JSON.parse(mail.init.body) };
  };
  const blob = new Blob(['png']);
  const sent = await run({ render: async () => blob });
  const upload = sent.calls.find((c) => c.url === '/api/spawn-attachments');
  assert.equal(upload.init.body.get('file').name, 'markup.png');
  assert.equal(sent.mail.attachment_token, 'tok');
  assert.match(sent.mail.body, /attached image shows the view/);
  assert.deepEqual(sent.notes, [{ text: 'Sent your markup to alpha.', error: false }]);

  const failed = await run({ render: async () => { throw new Error('too big'); } });
  assert.equal(failed.mail.attachment_token, undefined);
  assert.doesNotMatch(failed.mail.body, /attached image/, 'the agent is not told of an image it did not get');
  assert.match(failed.notes[0].text, /as text: the image failed: too big/);
  assert.equal(failed.notes[0].error, true);

  const refused = await run({ render: async () => blob, uploadOk: false });
  assert.equal(refused.mail.attachment_token, undefined);
  assert.doesNotMatch(refused.mail.body, /attached image/);

  const tooBig = await run({ render: async () => blob, imageOk: false });
  assert.equal(tooBig.mail.attachment_token, undefined, 'a refused image does not cost the operator the lines');
  assert.doesNotMatch(tooBig.mail.body, /attached image/);
  assert.match(tooBig.notes[0].text, /as text: the image failed: attachments total too large/);

  let rendered = 0;
  const remote = await run({ render: async () => { rendered += 1; return blob; }, target: { remote: true } });
  assert.equal(rendered, 0, 'a peer gets no image of this page');
  assert.equal(remote.mail.attachment_token, undefined);
  assert.match(remote.notes[0].text, /another node gets no image/);

  const noteOnly = await run({ render: async () => { rendered += 1; return blob; }, strokes: [] });
  assert.equal(rendered, 0, 'with nothing drawn, no image of the view leaves');
  void noteOnly;
});

test('a mark on a terminal is named by its line, and the overlay stays out of the image', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, requests, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  const doc = harness.window.document;
  const term = doc.createElement('div');
  term.className = 'xterm';
  term.innerHTML = '<div class="xterm-screen"></div><div class="xterm-rows"><div>first line</div><div>   </div></div>';
  doc.body.appendChild(term);
  const [first, blank] = term.querySelector('.xterm-rows').children;
  first.getBoundingClientRect = () => ({ top: 0, bottom: 50 });
  blank.getBoundingClientRect = () => ({ top: 50, bottom: 100 });
  doc.elementsFromPoint = () => [term.querySelector('.xterm-screen')];
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Mark up this page for alpha/ }).click());
  assert.ok(view.container.querySelector('.annotate-root').hasAttribute('data-capture-skip'));
  const svg = view.container.querySelector('svg.annotate-canvas');
  const fire = (type, x, y) => {
    const camel = { pointerdown: 'PointerDown', pointermove: 'PointerMove', pointerup: 'PointerUp' }[type];
    harness.fireEvent(svg, `on${type}` in svg ? type : camel, { button: 0, pointerId: 1, clientX: x, clientY: y });
  };
  await harness.act(() => {
    fire('pointerdown', 10, 10);
    for (const [x, y] of [[30, 30], [10, 40], [30, 20], [14, 30]]) fire('pointermove', x, y);
    fire('pointerup', 14, 30);
  });
  await harness.act(async () => {
    view.container.querySelector('form.annotate-bar').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await new Promise((r) => setTimeout(r, 20));
  });
  const mail = requests.find((r) => r.url === '/api/operator-message' && r.body.subject === '[annotation]');
  assert.match(mail.body.body, /1\. .*“first line”/);
});

test('markup too long to send whole stays open and tells the operator', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const notes = [];
  const { view, harness, requests, controller } = await setup(t, { prefs, notify: (text, error) => notes.push({ text, error }) });
  await harness.act(() => controller().open(A));
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Mark up this page for alpha/ }).click());
  const m = await harness.importDashboardModule('js/companion-model.js');
  const doc = harness.window.document;
  const target = doc.createElement('button');
  doc.body.appendChild(target);
  // Each its own line: alike ones share one.
  let label = 0;
  doc.elementsFromPoint = () => {
    target.textContent = `${label += 1}`.padEnd(70, 'x');
    return [target];
  };
  const svg = view.container.querySelector('svg.annotate-canvas');
  const fire = (type, x, y) => {
    const camel = { pointerdown: 'PointerDown', pointermove: 'PointerMove', pointerup: 'PointerUp' }[type];
    harness.fireEvent(svg, `on${type}` in svg ? type : camel, { button: 0, pointerId: 1, clientX: x, clientY: y });
  };
  await harness.act(() => {
    for (let i = 0; i < 120; i++) {
      fire('pointerdown', 10, 10);
      fire('pointermove', 15, 15);
      fire('pointerup', 15, 15);
    }
  });
  await harness.act(async () => {
    view.container.querySelector('form.annotate-bar').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.equal(requests.find((r) => r.url === '/api/operator-message'), undefined, 'nothing clipped goes out');
  assert.match(notes.at(-1).text, /too long/);
  assert.equal(notes.at(-1).error, true);
  assert.ok(view.container.querySelector('svg.annotate-canvas'), 'the layer stays open');
  assert.equal(m.companionMessageFits('x'.repeat(m.COMPANION_MESSAGE_MAX)), true);
  assert.equal(m.companionMessageFits('x'.repeat(m.COMPANION_MESSAGE_MAX + 1)), false);
  assert.equal(m.companionMessageFits('é'.repeat(m.COMPANION_MESSAGE_MAX)), false, 'the byte cap counts too');
});

test('Tab stays inside the annotation layer', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Mark up this page for alpha/ }).click());
  const doc = harness.window.document;
  const buttons = [...view.container.querySelectorAll('.annotate-bar button:not([disabled])')];
  const first = buttons[0];
  const last = buttons[buttons.length - 1];
  const tab = (shiftKey) => harness.fireEvent(doc.body, 'keydown', { key: 'Tab', shiftKey, bubbles: true, cancelable: true });
  last.focus();
  tab(false);
  assert.equal(doc.activeElement, first, 'Tab wraps from the last control to the first');
  tab(true);
  assert.equal(doc.activeElement, last, 'Shift+Tab wraps back');
  doc.body.focus();
  first.blur?.();
  tab(false);
  assert.equal(doc.activeElement, first, 'focus that escaped comes back');
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
  const keys = h.window.document.createElement('textarea');
  view.container.querySelector('.companion-term-host').appendChild(keys);
  const press = (init) => keys.dispatchEvent(Object.assign(new h.window.Event('keydown', { bubbles: true }), init));
  const before = cancelled;
  press({ key: 'Shift' });
  press({ key: 'Tab', altKey: true });
  assert.equal(cancelled, before, 'a modifier or a window shortcut is not typing');
  press({ key: 'a' });
  assert.equal(cancelled, before + 1, 'typing in the terminal stops speech');
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
  assert.match(mail.body.body, /^\[dictated\] what can I do here\n\n\[dashboard view:/);
  const voice = await harness.importDashboardModule('js/companion-voice.js');
  assert.equal(voice.sendPhraseRest('fix the bug send now', 'send now'), 'fix the bug');
  assert.equal(voice.sendPhraseRest('Fix the bug, Send Now.', 'send now'), 'Fix the bug');
  assert.equal(voice.sendPhraseRest('send, now!', 'send now'), '');
  assert.equal(voice.sendPhraseRest('send now and then stop', 'send now'), null, 'mid-sentence is plain text');
  assert.equal(voice.sendPhraseRest('send', 'send now'), null, 'the bare word no longer sends');
  assert.equal(voice.sendPhraseRest('resend now', 'send now'), null, 'whole words only');
  assert.equal(voice.sendPhraseRest('skicka nu', ' Skicka  nu! '), '');
  assert.equal(voice.sendPhraseRest('a.b', 'a+b'), null, 'the phrase is text, not a pattern');
  assert.equal(voice.sendPhraseRest('done over-and-out', 'over-and-out'), 'done');
  assert.equal(voice.sendPhraseRest('send now', ''), null, 'an empty phrase is off');
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

test('elevated access shows on the agent, and End ends only its sudo', async (t) => {
  const snapshot = snapshotWith({ steward: true });
  const prefs = memoryPrefs();
  const calls = [];
  const fetchImpl = async (url, options = {}) => {
    calls.push({ url: String(url), method: options.method, body: options.body });
    return { ok: true, status: 200, json: async () => ({}), text: async () => '' };
  };
  const { view, harness, snap, controller } = await setup(t, { snapshot, prefs, fetchImpl });
  const m = await harness.importDashboardModule('js/companion-model.js');
  const steward = [...m.companionAgents(snapshot).values()].find((agent) => agent.steward);
  assert.equal(m.elevatedAccess(snapshot, steward), null, 'nothing beyond the usual');
  await harness.act(() => controller().open(steward.agentId));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(async () => {
    snap.value = {
      ...snap.value,
      sudo: [{ id: 7, agent_id: steward.agentId, slug: 'agent.spawn', remaining_seconds: 700 }, { id: 8, agent_id: 'agt_other', slug: 'node.exec', remaining_seconds: 5 }],
      steward_elevated: { [steward.agentId]: { standing: ['host.read'], inherited: ['groups.admin'] } },
    };
  });
  const band = view.container.querySelector('.companion-elevated');
  assert.match(band.textContent, /agent\.spawn 12 min left/);
  assert.match(band.textContent, /host\.read standing/);
  assert.match(band.textContent, /groups\.admin from a group/);
  assert.doesNotMatch(band.textContent, /node\.exec/, 'another agent\'s grant');
  await harness.act(async () => {
    band.querySelector('button').click();
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.deepEqual(calls.map((call) => `${call.method} ${call.url}`).filter((line) => line.includes('/api/sudo') || line.includes('/api/permissions')),
    [`DELETE /api/sudo?agent_id=${encodeURIComponent(steward.agentId)}`], 'End touches sudo and nothing permanent');

  await harness.act(async () => { snap.value = { ...snap.value, sudo: [{ id: 7, agent_id: steward.agentId, slug: 'agent.spawn', remaining_seconds: 700, carried: true }, { id: 9, agent_id: steward.agentId, slug: 'agent.stop', remaining_seconds: 700 }] }; });
  const stored = view.container.querySelector('.companion-elevated');
  assert.match(stored.textContent, /agent\.spawn 12 min left, all future Stewards: ending it ends it for them too/);
  assert.doesNotMatch(stored.textContent, /agent\.stop 12 min left, all/, 'an instance grant says nothing about Stewards');
  assert.match(stored.querySelector('button').title, /stored for future Stewards/);
  const deletes = () => calls.filter((call) => call.method === 'DELETE').length;
  const before = deletes();
  await harness.act(async () => { stored.querySelector('button').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(deletes(), before + 1);
  assert.equal(calls.filter((call) => call.url === '/api/permissions').length, 0);

  await harness.act(async () => { snap.value = { ...snap.value, sudo: [] }; });
  const left = view.container.querySelector('.companion-elevated');
  assert.match(left.textContent, /host\.read standing/);
  assert.equal(left.querySelector('button'), null, 'no temporary sudo, nothing to end');
});

test('a click outside closes the add menu, and the + button still toggles it', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const plus = view.container.querySelector('.companion-add');
  await harness.act(async () => { plus.click(); await new Promise((r) => setTimeout(r, 0)); });
  const menu = view.container.querySelector('.companion-menu');
  assert.ok(menu);
  const press = (node) => harness.act(async () => {
    node.dispatchEvent(new harness.window.Event('pointerdown', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
  await press(menu);
  await press(plus);
  assert.ok(view.container.querySelector('.companion-menu'), 'the menu and its button do not count as outside');
  await press(harness.window.document.body);
  assert.equal(view.container.querySelector('.companion-menu'), null);
});

test('a reopened window fits the viewport and a larger browser restores its saved size', async (t) => {
  const saved = { x: 900, y: 500, w: 700, h: 400, snap: '' };
  const prefs = memoryPrefs({
    'tclaude.dash.companion.tabs': JSON.stringify([A]),
    'tclaude.dash.companion.placement': JSON.stringify(saved),
  });
  const { view, harness, controller } = await setup(t, { prefs });
  const root = harness.window.document.documentElement;
  const viewport = (width, height) => {
    Object.defineProperty(root, 'clientWidth', { configurable: true, value: width });
    Object.defineProperty(root, 'clientHeight', { configurable: true, value: height });
  };
  viewport(1000, 600);
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const frame = () => view.container.querySelector('.companion-window');
  const box = () => ['left', 'top', 'width', 'height'].map((key) => parseFloat(frame().style[key]));
  let [x, y, w, h] = box();
  assert.deepEqual([w, h], [700, 400], 'moved in at full size');
  assert.ok(x + w <= 1000 && y + h <= 600 && x >= 0 && y >= 0);
  viewport(800, 400);
  await harness.act(async () => {
    harness.window.dispatchEvent(new harness.window.Event('resize'));
    await new Promise((r) => setTimeout(r, 0));
  });
  [x, y, w, h] = box();
  assert.ok(w <= 700 && x + w <= 800 && h < 400 && y >= 0, 'shrunk only as far as it must be');
  assert.deepEqual(JSON.parse(prefs.getItem('tclaude.dash.companion.placement')), saved, 'and the saved place is untouched');
  const press = (node, dx) => harness.act(async () => {
    const event = (type, x) => Object.assign(new harness.window.Event(type, { bubbles: true }), { button: 0, pointerId: 1, clientX: x, clientY: 10 });
    node.dispatchEvent(event('pointerdown', 100));
    if (dx) node.dispatchEvent(event('pointermove', 100 + dx));
    node.dispatchEvent(event('pointerup', 100 + dx));
    await new Promise((r) => setTimeout(r, 0));
  });
  await press(view.container.querySelector('.companion-titlebar'), 0);
  await press(view.container.querySelector('.window-resize-zone.e'), 0);
  assert.deepEqual(JSON.parse(prefs.getItem('tclaude.dash.companion.placement')), saved, 'a click saves nothing');
  viewport(1800, 1000);
  await harness.act(async () => {
    harness.window.dispatchEvent(new harness.window.Event('resize'));
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.deepEqual(box(), [900, 500, 700, 400], 'a larger browser brings it all back');
  viewport(800, 400);
  await harness.act(async () => {
    harness.window.dispatchEvent(new harness.window.Event('resize'));
    await new Promise((r) => setTimeout(r, 0));
  });
  await press(view.container.querySelector('.window-resize-zone.e'), -20);
  assert.equal(JSON.parse(prefs.getItem('tclaude.dash.companion.placement')).h, 400, 'a side drag keeps the saved height');
});

const FLOATING = { x: 300, y: 100, w: 500, h: 400, snap: '' };

async function openLook(t, look, { prefs, snapshot = snapshotWith() } = {}) {
  const store = prefs || memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]), 'tclaude.dash.companion.active': A });
  const ctx = await setup(t, { prefs: store, snapshot: { ...snapshot, window_controls: look } });
  await ctx.harness.act(() => ctx.controller().open(A));
  await ctx.harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  return ctx;
}
const buttonNames = (root) => [...root.querySelectorAll('button')].map((button) => button.getAttribute('aria-label'));

test('each look puts the window controls in native order, and keyboard order follows the eye', async (t) => {
  const names = ['Minimize (Ctrl+.)', 'Maximize', 'Close alpha'];
  for (const [look, order] of [['macos', [names[2], names[0], names[1]]], ['windows', names], ['linux', names]]) {
    const { view } = await openLook(t, look);
    const bar = view.container.querySelector('.companion-titlebar');
    const group = bar.querySelector('.overlay-window-controls');
    assert.deepEqual(buttonNames(group), order, look);
    assert.ok(group.querySelectorAll('svg').length === 3, `${look} draws its symbols`);
    const all = [...bar.querySelectorAll('button')];
    const tab = bar.querySelector('[role="tab"]');
    assert.equal(all.indexOf(group.querySelector('button')) < all.indexOf(tab), look === 'macos', `${look} tab order`);
    assert.equal(bar.querySelector('.companion-grip') === null, look === 'macos', `${look} grip`);
    assert.equal(bar.querySelector('.companion-controls [aria-label^="Minimize"]'), null, 'no second minimize');
    view.unmount?.();
  }
});

test('the classic look is the bar as it always was, with its one minimize', async (t) => {
  const { view, harness } = await openLook(t, 'classic');
  const bar = view.container.querySelector('.companion-titlebar');
  assert.equal(bar.querySelector('.overlay-window-controls'), null);
  const minimize = bar.querySelector('.companion-controls [aria-label="Minimize (Ctrl+.)"]');
  assert.equal(minimize.textContent, '–');
  assert.equal(bar.querySelector('[aria-label="Maximize"]'), null);
  await harness.act(async () => { minimize.click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(view.container.querySelector('.companion-window-warm'), 'minimized');
});

test('an unknown or missing look follows the browser OS', async (t) => {
  const harness = await createPreactHarness(t);
  const { windowControlsLook } = await harness.importDashboardModule('js/overlay-window-controls.js');
  assert.equal(windowControlsLook('auto', 'macos'), 'macos');
  assert.equal(windowControlsLook(undefined, 'windows'), 'windows');
  assert.equal(windowControlsLook('beos', 'linux'), 'linux');
  assert.equal(windowControlsLook('classic', 'macos'), 'classic');
  assert.equal(windowControlsLook('linux', 'macos'), 'linux');
});

test('minimize and close are their own actions, and close leaves the agent running', async (t) => {
  const { view, harness, requests } = await openLook(t, 'windows');
  const bar = () => view.container.querySelector('.companion-titlebar');
  await harness.act(() => bar().querySelector('.overlay-wc-close').click());
  assert.deepEqual([...bar().querySelectorAll('[role="tab"]')].map((tab) => tab.title.split(':')[0]), ['bravo'], 'the tab is gone');
  assert.equal(view.container.querySelector('.companion-window-warm'), null, 'the window stays for the other tab');
  assert.deepEqual(requests.filter((request) => request.url.includes(A)), [], 'nothing was asked of the agent');
  await harness.act(async () => { bar().querySelector('.overlay-wc-minimize').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(view.container.querySelector('.companion-window-warm'), 'minimized');
  assert.equal(view.container.querySelectorAll('[role="tab"]').length, 1, 'and minimize closed no tab');
});

test('maximize fills the room, survives a reload, and restores what it replaced', async (t) => {
  const pref = 'tclaude.dash.companion.placement';
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]), [pref]: JSON.stringify(FLOATING) });
  const { view, harness } = await openLook(t, 'linux', { prefs });
  const frame = () => view.container.querySelector('.companion-window');
  const size = () => view.container.querySelector('.overlay-wc-maximize');
  await harness.act(() => size().click());
  assert.ok(frame().classList.contains('snapped-max'));
  assert.equal(size().getAttribute('aria-label'), 'Restore');
  assert.equal(view.container.querySelector('.overlay-window-edge'), null, 'a maximized window has no free edge to pull');
  assert.equal(view.container.querySelector('.companion-resize-frame'), null);
  assert.deepEqual(JSON.parse(prefs.getItem(pref)), { ...FLOATING, snap: 'max' }, 'the floating place is kept to come back to');
  await harness.act(() => size().click());
  assert.equal(frame().classList.contains('snapped'), false);
  assert.equal(size().getAttribute('aria-label'), 'Maximize');
  assert.deepEqual(JSON.parse(prefs.getItem(pref)), FLOATING);
  prefs.setItem(pref, JSON.stringify({ ...FLOATING, snap: 'left' }));
  view.unmount?.();
  const again = await openLook(t, 'linux', { prefs });
  const frame2 = () => again.view.container.querySelector('.companion-window');
  const size2 = () => frame2().querySelector('.overlay-wc-maximize');
  assert.ok(frame2().classList.contains('snapped-left'));
  await again.harness.act(() => size2().click());
  await again.harness.act(() => size2().click());
  assert.ok(frame2().classList.contains('snapped-left'), 'restored to the side it was snapped to');
});

test('restore brings back the panel it replaced, across a reload and at its resized width', async (t) => {
  const pref = 'tclaude.dash.companion.placement';
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]), [pref]: JSON.stringify({ ...FLOATING, snap: 'left' }) });
  const first = await openLook(t, 'linux', { prefs });
  const frame = () => first.view.container.querySelector('.companion-window');
  const size = () => frame().querySelector('.overlay-wc-maximize');
  const pull = (dx) => first.harness.act(async () => {
    const handle = first.view.container.querySelector('.overlay-window-edge');
    const event = (type, x) => Object.assign(new first.harness.window.Event(type, { bubbles: true, cancelable: true }), { button: 0, pointerId: 1, clientX: x, clientY: 200 });
    handle.dispatchEvent(event('pointerdown', 500));
    handle.dispatchEvent(event('pointermove', 500 + dx));
    handle.dispatchEvent(event('pointerup', 500 + dx));
    await new Promise((r) => setTimeout(r, 0));
  });
  await pull(30);
  const pulled = frame().style.width;
  await first.harness.act(() => size().click());
  await first.harness.act(() => size().click());
  assert.ok(frame().classList.contains('snapped-left'));
  assert.equal(frame().style.width, pulled, 'the resized panel comes back at its size');
  await first.harness.act(() => size().click());
  assert.equal(JSON.parse(prefs.getItem(pref)).restoreSnap, 'left');
  const reloaded = await openLook(t, 'linux', { prefs });
  const frame2 = () => reloaded.view.container.querySelectorAll('.companion-window')[0];
  assert.ok(frame2().classList.contains('snapped-max'));
  await reloaded.harness.act(() => frame2().querySelector('.overlay-wc-maximize').click());
  assert.ok(frame2().classList.contains('snapped-left'), 'the edge survives a reload');
  assert.equal(frame2().style.width, pulled, 'and so does the width');
  assert.equal(JSON.parse(prefs.getItem(pref)).restoreSnap, undefined);
});

test('snap model: corners, saved panel sizes and free edges', async (t) => {
  const harness = await createPreactHarness(t);
  const o = await harness.importDashboardModule('js/overlay-window-model.js');
  const vp = { width: 1200, height: 800 };
  const bottom = vp.height - o.OVERLAY_FOOTER_ROOM;
  assert.equal(o.snapZone({ x: 3, y: 70 }, vp, 60), 'top-left');
  assert.equal(o.snapZone({ x: 1195, y: 70 }, vp, 60), 'top-right');
  assert.equal(o.snapZone({ x: 3, y: 760 }, vp, 60), 'bottom-left');
  assert.equal(o.snapZone({ x: 1100 + 60, y: 790 }, vp, 60), 'bottom-right', 'along the bottom edge too');
  assert.equal(o.snapZone({ x: 3, y: 400 }, vp, 60), 'left');
  // The reach follows the viewport, between its limits.
  assert.deepEqual(Object.values(o.snapReach({ width: 1920 })).map(Math.round), [58, 211]);
  assert.deepEqual(o.snapReach({ width: 800 }), { edge: 40, corner: 120 });
  assert.deepEqual(o.snapReach({ width: 3840 }), { edge: 80, corner: 240 });
  const wide = { width: 1920, height: 1080 };
  assert.equal(o.snapZone({ x: 50, y: 500 }, wide, 60), 'left');
  assert.equal(o.snapZone({ x: 60, y: 500 }, wide, 60), '');
  assert.equal(o.snapZone({ x: 3, y: 265 }, wide, 60), 'top-left');
  assert.equal(o.snapZone({ x: 3, y: 280 }, wide, 60), 'left');
  assert.equal(o.snapZone({ x: 200, y: 1040 }, wide, 60), 'bottom-left');
  assert.equal(o.snapZone({ x: 220, y: 1040 }, wide, 60), 'bottom');
  const short = { width: 900, height: 400 };
  assert.equal(o.snapZone({ x: 3, y: 200 }, short, 60), 'left', 'a short side keeps its middle third');
  assert.equal(o.snapZone({ x: 3, y: 100 }, short, 60), 'top-left');
  const quarter = o.zoneRect('top-right', vp, 60);
  assert.deepEqual(quarter, { x: 1200 - 504, y: 60, w: 504, h: Math.round((bottom - 60) / 2), snap: 'top-right' });
  const low = o.zoneRect('bottom-left', vp, 60);
  assert.equal(low.y + low.h, bottom, 'a bottom corner sits on the footer');
  assert.equal(o.zoneRect('left', vp, 60, { w: 700, h: 300 }).w, 700, 'a saved width');
  assert.equal(o.zoneRect('left', vp, 60, { w: 700, h: 300 }).h, bottom - 60, 'a side panel stays full height');
  assert.equal(o.zoneRect('right', { width: 500, height: 800 }, 0, { w: 700, h: 300 }).w, 500, 'a saved size stays within the viewport');
  assert.equal(o.parsePlacement(JSON.stringify({ x: 1, y: 2, w: 3, h: 4, snap: 'top-left' })).snap, 'top-left');
  assert.equal(o.parsePlacement(JSON.stringify({ x: 1, y: 2, w: 3, h: 4, snap: 'max', restoreSnap: 'bottom-right' })).restoreSnap, 'bottom-right');
  const panels = { left: { w: 700, h: 300 }, junk: { w: 1, h: 2 }, right: { w: 'x', h: 2 } };
  assert.deepEqual(o.parsePlacement(JSON.stringify({ x: 1, y: 2, w: 3, h: 4, panels })).panels, { left: { w: 700, h: 300 } });
  assert.equal(o.parsePlacement(JSON.stringify({ x: 1, y: 2, w: 3, h: 4, panels: 'nope' })).panels, undefined);
  assert.deepEqual(o.freeEdges('bottom-right'), ['w', 'n', 'nw']);
  assert.deepEqual(o.freeEdges('max'), []);
  assert.deepEqual(o.freeEdges(''), []);
  const start = o.zoneRect('bottom-right', vp, 60);
  const pulled = o.snappedResize(start, 'nw', -40, -30, vp, 60);
  assert.deepEqual([pulled.x + pulled.w, pulled.y + pulled.h], [1200, bottom], 'the far corner stays put');
  assert.deepEqual([pulled.w, pulled.h], [start.w + 40, start.h + 30]);
  const huge = o.snappedResize(o.zoneRect('top-left', vp, 60), 's', 0, 5000, vp, 60);
  assert.equal(huge.y + huge.h, bottom, 'a drag cannot run past the footer');
  assert.equal(o.snappedResize(o.zoneRect('right', vp, 60), 'w', 5000, 0, vp, 60).x, 1200 - o.OVERLAY_MIN_W, 'nor under the minimum width');
});

test('a mouse press hands focus back to the terminal, and the keyboard stays on the control', async (t) => {
  const { view, harness, mounted } = await openLook(t, 'windows');
  const size = () => view.container.querySelector('.overlay-wc-maximize');
  const before = mounted.at(-1).focused;
  await harness.act(() => size().click());
  assert.equal(mounted.at(-1).focused, before, 'Enter on the control keeps focus for the next key');
  const press = (node) => harness.act(() => node.dispatchEvent(Object.assign(new harness.window.Event('click', { bubbles: true, cancelable: true }), { detail: 1 })));
  await press(size());
  assert.equal(mounted.at(-1).focused, before + 1, 'after maximize or restore');
  await press(view.container.querySelector('.overlay-wc-close'));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.notEqual(view.container.ownerDocument.activeElement?.className, 'overlay-wc-close', 'the next key cannot close the next tab');
});

test('a click on the bar leaves a snapped window alone, and only a real move tears it off', async (t) => {
  const pref = 'tclaude.dash.companion.placement';
  const stored = JSON.stringify({ ...FLOATING, snap: 'max' });
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]), [pref]: stored });
  const { view, harness } = await openLook(t, 'linux', { prefs });
  const frame = () => view.container.querySelector('.companion-window');
  const bar = view.container.querySelector('.companion-grip');
  const press = (dx) => harness.act(async () => {
    const event = (type, x) => Object.assign(new harness.window.Event(type, { bubbles: true, cancelable: true }), { button: 0, pointerId: 1, clientX: x, clientY: 200 });
    bar.dispatchEvent(event('pointerdown', 400));
    if (dx) bar.dispatchEvent(event('pointermove', 400 + dx));
    bar.dispatchEvent(event('pointerup', 400 + dx));
    await new Promise((r) => setTimeout(r, 0));
  });
  await press(0);
  await press(2);
  assert.ok(frame().classList.contains('snapped-max'), 'a click, or a hand that shook, keeps it');
  assert.equal(prefs.getItem(pref), stored, 'and saves nothing');
  await press(40);
  assert.equal(frame().classList.contains('snapped'), false);
});

test('dragging by the bar works around the controls, and pressing a control starts no drag', async (t) => {
  const pref = 'tclaude.dash.companion.placement';
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]), [pref]: JSON.stringify({ ...FLOATING, snap: 'max' }) });
  const { view, harness } = await openLook(t, 'macos', { prefs });
  const frame = () => view.container.querySelector('.companion-window');
  const press = (node) => harness.act(async () => {
    const event = (type, x) => Object.assign(new harness.window.Event(type, { bubbles: true, cancelable: true }), { button: 0, pointerId: 1, clientX: x, clientY: 200 });
    node.dispatchEvent(event('pointerdown', 400));
    node.dispatchEvent(event('pointermove', 420));
    node.dispatchEvent(event('pointerup', 420));
    await new Promise((r) => setTimeout(r, 0));
  });
  const group = () => view.container.querySelector('.overlay-window-controls');
  await press(group().querySelector('button'));
  await press(group().querySelector('svg'));
  assert.ok(frame().classList.contains('snapped-max'), 'still maximized: a control is no handle');
  await press(group());
  assert.equal(frame().classList.contains('snapped'), false, 'the gaps between the lights drag, and tear the window off');
  assert.equal(JSON.parse(prefs.getItem(pref)).snap, '');
});

test('every edge and corner of a floating window resizes it, and the opposite side stays put', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const zones = [...view.container.querySelectorAll('.companion-resize-frame .window-resize-zone')].map((node) => node.className.split(' ').pop());
  assert.deepEqual(zones.sort(), ['e', 'n', 'ne', 'nw', 's', 'se', 'sw', 'w']);
  assert.equal(view.container.querySelector('.companion-window .window-resize-zone'), null, 'outside the window, whose clip would cut them');
  const frame = view.container.querySelector('.companion-window');
  const box = () => ['left', 'top', 'width', 'height'].map((key) => parseFloat(frame.style[key]));
  const drag = (zone, dx, dy) => harness.act(async () => {
    const handle = view.container.querySelector(`.window-resize-zone.${zone}`);
    const event = (type, x, y) => Object.assign(new harness.window.Event(type, { bubbles: true }), { button: 0, pointerId: 1, clientX: x, clientY: y });
    handle.dispatchEvent(event('pointerdown', 300, 300));
    handle.dispatchEvent(event('pointermove', 300 + dx, 300 + dy));
    handle.dispatchEvent(event('pointerup', 300 + dx, 300 + dy));
    await new Promise((r) => setTimeout(r, 0));
  });
  const [x0, y0, w0, h0] = box();
  await drag('nw', -20, -10);
  assert.deepEqual(box(), [x0 - 20, y0 - 10, w0 + 20, h0 + 10]);
  await drag('nw', 0, -5000);
  const [, top, , height] = box();
  assert.equal(top, 0, 'the top edge stops at the viewport');
  assert.equal(top + height, y0 + h0, 'and the bottom edge stays');
  const [x1, y1, w1, h1] = box();
  await drag('w', -15, 40);
  assert.deepEqual(box(), [x1 - 15, y1, w1 + 15, h1], 'a left edge moves only the left side');
  assert.equal(view.container.querySelector('.companion-resize-frame').style.left, frame.style.left, 'and the frame follows the window');
});

test('the window gives the launch dialog what a row does, so Clone and Recall home work', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-model.js');
  const { launchAgent, modeState } = await harness.importDashboardModule('js/launch-target-dialog.js');
  const home = 'node-home';
  const here = 'node-here';
  const fleet = { self: { id: here, name: 'here' }, peers: [{ id: home, name: 'home box' }] };
  const presence = { state: 'here', home_instance: home, current_instance: here };
  const agents = m.companionAgents({ groups: [{ name: 'crew', members: [{
    agent_id: A, conv_id: 'conv-a', title: 'alpha', online: true, federation_presence: presence,
    state: { status: 'idle', cwd: '/work/alpha', harness: 'codex' },
  }] }] });
  const data = m.companionLaunchData(agents.get(A), fleet);
  assert.deepEqual(data, {
    agent: A, conv: 'conv-a', label: 'alpha', harness: 'codex', cwd: '/work/alpha', group: 'crew',
    presence: 'visiting', homeNode: home, homeName: 'home box',
  });
  assert.equal(m.companionLaunchData(m.companionAgents({ ungrouped: [{ agent_id: B, conv_id: 'b', title: 'solo' }] }).get(B), null).presence, '');
  const agent = launchAgent(data, fleet.self);
  assert.equal(agent.cwd, '/work/alpha');
  assert.equal(modeState('home', { local: true, presence: agent.presence, homeIsSelf: agent.homeNode === here }).ok, true);
});

test('Ctrl/Cmd+M and the title bar message the active agent; morph opens from the window', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const seeds = [];
  const morphs = [];
  const { view, harness, controller } = await setup(t, { prefs, onComposeMessage: (seed) => seeds.push(seed), openMorph: (agent) => morphs.push(agent) });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const host = view.container.querySelector('.companion-term-host');
  const press = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.assign(press, { key: 'm', code: 'KeyM', ctrlKey: true });
  await harness.act(async () => { host.dispatchEvent(press); });
  assert.equal(seeds[0]?.agent, A);
  assert.equal(press.defaultPrevented, true);
  await harness.act(async () => { view.container.querySelector('.companion-compose').click(); });
  assert.equal(seeds.length, 2);
  await harness.act(async () => { view.container.querySelector('.companion-morph').click(); });
  assert.equal(morphs[0].agentId, A);
});

test('a detached agent keeps its window until the operator brings it back', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]) });
  const opened = { closed: false, close() { this.closed = true; } };
  let result = null;
  const calls = [];
  const places = [];
  const { view, harness, controller, mounted } = await setup(t, { prefs, detachWindow: (agent, _label, options) => { calls.push(agent); places.push(options); return result; } });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  Object.assign(harness.window, { screenX: 100, screenY: 50, outerWidth: 1216, innerWidth: 1200, outerHeight: 900, innerHeight: 800 });
  view.container.querySelector('.companion-window').getBoundingClientRect = () => ({ left: 300, top: 40, width: 420, height: 360 });
  const detach = () => harness.act(async () => {
    view.container.querySelector('.companion-detach').click();
    await new Promise((r) => setTimeout(r, 0));
  });
  await detach();
  assert.equal(mounted[0].disposed, false, 'blocked: the terminal stays');
  result = opened;
  await detach();
  assert.deepEqual(calls, [A, A]);
  assert.deepEqual(places[1].at, { x: 408, y: 190 }, 'it opens over the companion window, not at the browser corner');
  assert.deepEqual(places[1].size, { width: 420, height: 360 });
  assert.equal(mounted[0].disposed, true, 'the window lets go of the terminal');
  assert.match(view.container.textContent, /open in its own window/);

  await harness.act(() => harness.getByRole(view.container, 'tab', { name: /bravo/ }).click());
  await harness.act(() => harness.getByRole(view.container, 'tab', { name: /alpha/ }).click());
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.match(view.container.textContent, /open in its own window/, 'a tab click does not take it back');

  await harness.act(async () => {
    harness.getByRole(view.container, 'button', { name: 'Bring it back here' }).click();
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.doesNotMatch(view.container.textContent, /open in its own window/);
});

test('the tools float over the terminal, and the text field shows only while there is something to say', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictation = fakeDictation();
  const { view, harness, controller } = await setup(t, { prefs, dictationImpl: dictation.impl });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const tools = view.container.querySelector('.companion-floating');
  assert.ok(tools.querySelector('.companion-mic') && tools.querySelector('.companion-annotate') && tools.querySelector('.companion-context'));
  assert.equal(view.container.querySelector('form.companion-input'), null, 'no bar at rest');
  const order = [...view.container.querySelectorAll('.companion-floating, .companion-term-host')];
  assert.equal(order[0], tools, 'reachable by Tab before the terminal');

  await harness.act(() => view.container.querySelector('.companion-mic').click());
  assert.ok(view.container.querySelector('form.companion-input.listening input.companion-draft'));
  assert.ok(view.container.querySelector('form.companion-input button[type="submit"]'));
  assert.ok(tools.classList.contains('listening'), 'never dimmed while the mic is open');
});

async function openWithMic(t, { supported, secure }) {
  const dictationImpl = () => ({ supported, start() {}, stop() {}, abort() {} });
  const { view, harness, controller } = await setup(t, { prefs: memoryPrefs(), dictationImpl });
  harness.window.isSecureContext = secure;
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  return { mic: view.container.querySelector('.companion-mic'), note: view.container.querySelector('.companion-mic-note'), container: view.container };
}

test('a browser without speech recognition gets a disabled mic whose tooltip says so', async (t) => {
  const { mic, note, container } = await openWithMic(t, { supported: false, secure: true });
  assert.equal(mic.disabled, true);
  assert.match(mic.title, /no speech recognition/);
  assert.equal(note, null);
  assert.equal(container.querySelector('.companion-context'), null, 'view only serves dictation');
});

test('a plain http page gets a disabled mic whose tooltip says what it needs', async (t) => {
  const { mic, note } = await openWithMic(t, { supported: true, secure: false });
  assert.equal(mic.disabled, true);
  assert.equal(note, null);
  assert.match(mic.title, /tailscale serve/);
});

test('a secure page keeps a working mic with no note', async (t) => {
  const { mic, note, container } = await openWithMic(t, { supported: true, secure: true });
  assert.equal(mic.disabled, false);
  assert.equal(note, null);
  assert.ok(container.querySelector('.companion-context'));
});

test('the draft strip leaves room for an approval, and the status pill clears the tools', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictation = fakeDictation();
  const snapshot = snapshotWith();
  snapshot.access_requests = [{ id: 'r1', perm: 'groups.create', agent_id: A, conv_id: 'conv-a', status: 'pending' }];
  const { view, harness, controller } = await setup(t, { prefs, snapshot, dictationImpl: dictation.impl });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const body = view.container.querySelector('.companion-body');
  assert.equal(body.classList.contains('clear-strip'), false);
  await harness.act(() => view.container.querySelector('.companion-mic').click());
  assert.ok(view.container.querySelector('.companion-approval'));
  assert.ok(body.classList.contains('clear-strip'));
  const anchor = view.container.querySelector('.companion-floating-anchor');
  assert.ok(anchor.querySelector('.companion-floating'), 'the pill is placed from the same anchor as the tools');
});

test('the + menu opens with the cursor in its search field, and Escape goes back to +', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, controller } = await setup(t, { prefs });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const plus = view.container.querySelector('.companion-add');
  await harness.act(async () => { plus.click(); await new Promise((r) => setTimeout(r, 0)); });
  const filter = view.container.querySelector('.companion-menu-filter');
  assert.equal(harness.document.activeElement, filter);
  await harness.act(() => harness.fireEvent(filter, 'keydown', { key: 'Escape' }));
  assert.equal(view.container.querySelector('.companion-menu'), null);
  assert.equal(harness.document.activeElement, plus);
});

// ---- the Steward phrase ----

function micCounting() {
  const fake = fakeDictation();
  const impl = fake.impl;
  fake.starts = [];
  fake.impl = (cb) => {
    const made = impl(cb);
    fake.reservations = [];
    return { ...made, keep: (on) => fake.reservations.push(on), start: (options) => { fake.starts.push(options); made.start(); } };
  };
  return fake;
}

test('the Steward phrase opens its window and only then dictates there', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictated = micCounting();
  const { view, harness, controller } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }), dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  await harness.act(() => view.container.querySelector('input.companion-draft')?.focus?.());
  const wake = await harness.importDashboardModule('js/voice-wake.js');
  assert.equal(wake.stewardRefusal(), '');
  await harness.act(() => wake.openStewardDoor('check the logs'));
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  assert.equal(dictated.starts.length, 1);
  assert.equal(dictated.reservations[0], true, 'the mic is held through the switch');
  assert.equal(dictated.reservations.at(-1), false);
  const field = view.container.querySelector('input.companion-draft');
  assert.equal(field.getAttribute('aria-label'), 'Message steward-k3x');
  assert.equal(field.value, 'check the logs');
  await harness.act(() => dictated.callbacks.onText('and the rest'));
  assert.equal(view.container.querySelector('input.companion-draft').value, 'check the logs and the rest');
  await harness.act(() => controller().open(A));
  assert.ok(!view.container.querySelector('input.companion-draft')?.value, 'no word landed on the agent that had the focus');
});

test('with no Steward running its phrase summons nothing', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictated = micCounting();
  const { harness, requests, controller } = await setup(t, { prefs, dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  const wake = await harness.importDashboardModule('js/voice-wake.js');
  assert.match(wake.stewardRefusal(), /isn’t running/);
  assert.deepEqual(requests.filter((r) => String(r.url).includes('scribe')), []);
  assert.equal(dictated.starts.length, 0);
});

const CONTINUOUS_SNAPSHOT = () => ({
  ...snapshotWith({ steward: true }),
  harnesses: [{ name: 'claude', can_mail_mid_turn: true }],
  voice: { wake_enabled: true, continuous_enabled: true, phrases: [{ action: 'send', phrase: 'send now' }, { action: 'steward', phrase: 'hey stewie' }, { action: 'stop', phrase: 'stop listening' }] },
});

test('the Steward phrase said on the Steward starts a clean draft', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictated = micCounting();
  const { view, harness, controller } = await setup(t, { prefs, snapshot: CONTINUOUS_SNAPSHOT(), dictationImpl: dictated.impl });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.companion-mic').click());
  await harness.act(async () => { dictated.callbacks.onSend('first'); await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(() => new Promise((r) => setTimeout(r, 260)));
  await harness.act(() => dictated.callbacks.onText('hey stewie check the'));
  await harness.act(() => {
    dictated.callbacks.onText('', { interim: '', settled: '' });
    dictated.callbacks.onSteward('check the logs');
  });
  assert.equal(view.container.querySelector('input.companion-draft').value, 'check the logs');
});

test('the mic stays held through the switch out of a loop on another tab', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictated = micCounting();
  const { view, harness, controller } = await setup(t, { prefs, snapshot: CONTINUOUS_SNAPSHOT(), dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  await harness.act(() => view.container.querySelector('.companion-mic').click());
  await harness.act(async () => { dictated.callbacks.onSend('first'); await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(() => new Promise((r) => setTimeout(r, 260)));
  dictated.reservations.length = 0;
  await harness.act(async () => { dictated.callbacks.onSteward('take over'); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(dictated.reservations.filter((on) => on === false).length, 0, 'never let go on the way across');
  const field = view.container.querySelector('input.companion-draft');
  assert.equal(field.getAttribute('aria-label'), 'Message steward-k3x');
  assert.equal(field.value, 'take over');
  assert.ok(view.container.querySelector('.companion-continuous'), 'continuous came along');
});

test('a Steward in its own window gets focus and keeps its terminal', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictated = micCounting();
  const opened = { closed: false, focused: 0, close() { this.closed = true; }, focus() { this.focused += 1; } };
  const { view, harness, controller, closed } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }), dictationImpl: dictated.impl, detachWindow: () => opened });
  await harness.act(() => controller().open(S));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(async () => {
    view.container.querySelector('.companion-detach').click();
    await new Promise((r) => setTimeout(r, 0));
  });
  await harness.act(() => controller().open(A));
  const wake = await harness.importDashboardModule('js/voice-wake.js');
  const before = (closed || []).length;
  assert.match(wake.stewardRefusal(), /own window/);
  assert.equal(opened.focused, 1);
  assert.equal(opened.closed, false);
  assert.equal((closed || []).length, before, 'nothing took its terminal');
  assert.equal(dictated.starts.length, 0);
});

test('the title bar keeps voice, Message and ⚙; the rest is under the cog', async (t) => {
  const { view, harness, controller } = await setup(t, { onComposeMessage: () => {}, detachWindow: () => null, openMorph: () => {}, openTerminalsPane: () => {} });
  await harness.act(() => controller().open(A));
  const bar = view.container.querySelector('.companion-controls');
  const outside = (selector) => [...bar.querySelectorAll(selector)].filter((node) => !node.closest('.companion-cog-menu'));
  assert.equal(outside('.companion-compose').length, 1);
  assert.equal(outside('.companion-cog-button').length, 1);
  for (const moved of ['.companion-float', '.companion-detach', '.companion-morph', '.companion-clear-open']) {
    assert.equal(outside(moved).length, 0, moved);
    assert.ok(bar.querySelector(`.companion-cog-menu ${moved}`), moved);
  }
  const menu = bar.querySelector('.companion-cog-menu');
  assert.equal(menu.classList.contains('open'), false);
  await harness.act(() => bar.querySelector('.companion-cog-button').click());
  assert.equal(menu.classList.contains('open'), true);
  await harness.act(() => { const filter = menu.querySelector('.action-menu-filter'); filter.value = 'morph'; filter.dispatchEvent(new harness.window.Event('input', { bubbles: true })); });
  const shown = [...menu.querySelectorAll('[role="menuitem"]')].filter((item) => !item.hasAttribute('data-menu-filtered-out')).map((item) => item.querySelector('.companion-cog-label').textContent);
  assert.deepEqual(shown, ['Morph']);
});

test('a Steward\'s cog has access and the shut-down, which asks first; a plain agent\'s does not', async (t) => {
  const calls = [];
  const fetchImpl = async (url, options = {}) => { calls.push([options.method, url]); return { ok: true, text: async () => '', json: async () => ({}) }; };
  const opened = [];
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }), openSudo: (...args) => opened.push(args), openPermissions: () => {} });
  await harness.act(() => controller().open(S));
  const bar = () => view.container.querySelector('.companion-controls');
  assert.ok(bar().querySelector('.companion-cog-menu .companion-steward-perms'));
  await harness.act(() => bar().querySelector('.companion-cog-button').click());
  await harness.act(() => bar().querySelector('.companion-steward-sudo').click());
  assert.equal(opened.length, 1);
  assert.equal(bar().querySelector('.companion-cog-menu').classList.contains('open'), false, 'choosing closes the menu');
  await harness.act(() => bar().querySelector('.companion-cog-menu .steward-shutdown-open').click());
  assert.ok(bar().querySelector('.steward-shutdown-confirm'), 'it asks');
  assert.deepEqual(calls.filter(([, url]) => url.endsWith('/stop')), []);
  await harness.act(() => controller().open(A));
  assert.equal(bar().querySelector('.companion-steward-sudo'), null);
  assert.equal(bar().querySelector('.steward-shutdown-open'), null);
});

test('what needs the operator shows in the bar only while true', async (t) => {
  const snapshot = { ...snapshotWith({ steward: true }), steward_roster: [{ agent_id: S, name: 's', seat: 'active', online: true }] };
  const { view, harness, controller, snap } = await setup(t, { snapshot, openStewardHandover: () => {} });
  await harness.act(() => controller().open(S));
  assert.equal(view.container.querySelector('.companion-state-roster'), null);
  await harness.act(() => { snap.value = { ...snapshot, steward_roster: [...snapshot.steward_roster, { agent_id: B, name: 'b', seat: 'standby', online: true }] }; });
  assert.match(view.container.querySelector('.companion-state-roster').textContent, /✦ 1/);
});

test('the second click of a double-click on a cog item stops there', async (t) => {
  const harness = await createPreactHarness(t);
  const { dropDoubleClick } = await harness.importDashboardModule('js/companion-cog.js');
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let listener = null;
  const doc = { addEventListener: (_, next) => { listener = next; }, removeEventListener: () => { listener = null; } };
  dropDoubleClick(doc);
  let stopped = 0;
  const click = (detail) => listener({ detail, preventDefault() {}, stopPropagation() { stopped += 1; } });
  click(1);
  assert.equal(stopped, 0, 'an ordinary click goes through');
  click(2);
  assert.equal(stopped, 1);
  t.mock.timers.tick(501);
  assert.equal(listener, null, 'it stops listening');
});

test('with no agent showing the companion has no cog', async (t) => {
  const { view, harness } = await setup(t);
  await harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  assert.ok(view.container.querySelector(SHOWN_WINDOW));
  assert.equal(view.container.querySelector('.companion-cog-button'), null);
});

test('a floated Steward keeps its tools under the cog', async (t) => {
  const { view, harness, controller } = await setup(t, { snapshot: snapshotWith({ steward: true }), openPermissions: () => {}, openSudo: () => {} });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.companion-float').click());
  const win = () => view.container.querySelector('.chat-window');
  assert.ok(win().querySelector('.companion-cog-menu .companion-steward-sudo'));
  await harness.act(() => win().querySelector('.companion-cog-button').click());
  await harness.act(() => win().querySelector('.companion-cog-menu .steward-shutdown-open').click());
  assert.ok(win().querySelector('.steward-shutdown-confirm'));
});

test('Clear context asks first, for any agent that is not a shell', async (t) => {
  const calls = [];
  const fetchImpl = async (url, options = {}) => { calls.push([url, options.body]); return { ok: true, text: async () => '', json: async () => ({}) }; };
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, controller } = await setup(t, { fetchImpl, prefs, snapshot: snapshotWith() });
  await harness.act(() => controller().open(A));
  const clear = () => view.container.querySelector('.companion-clear-context');
  assert.ok(clear());
  await harness.act(() => view.container.querySelector('.companion-clear-open').click());
  assert.match(clear().querySelector('.steward-shutdown-confirm').textContent, /same identity and permissions/);
  assert.deepEqual(calls.filter(([url]) => url.endsWith('/reincarnate')), [], 'nothing happens before the confirmation');
  await harness.act(async () => { clear().querySelector('.danger').click(); await new Promise((resolve) => { setTimeout(resolve, 0); }); });
  assert.deepEqual(calls.filter(([url]) => url.endsWith('/reincarnate')), [[`/api/agents/${encodeURIComponent(A)}/reincarnate`, '{"mode":"clear"}']]);
});

test('an offline Steward in the seat offers Resume and Start fresh; with none, only a summon', async (t) => {
  const snapshot = snapshotWith({ steward: true });
  snapshot.groups[1].members[0].online = false;
  snapshot.steward_roster = [{ agent_id: S, name: 'steward-k3x', seat: 'active', online: false, last_ran: new Date(Date.now() - 2 * 3600e3).toISOString(), keeps: ['node.exec'] }];
  const { view, harness, controller } = await setup(t, { snapshot });
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Open the companion/ }).click());
  assert.match(view.container.querySelector('.companion-resume-steward').textContent, /Resume/);
  assert.match(view.container.textContent, /Last ran 2h ago/);
  assert.match(view.container.textContent, /It keeps what you gave it separately: node\.exec/, 'Start fresh says what stays with the old one');
  assert.ok(view.container.querySelector('.companion-fresh-steward'));

  const fresh = await setup(t, { snapshot: snapshotWith() });
  await fresh.harness.act(() => fresh.harness.getByRole(fresh.view.container, 'button', { name: /Open the companion/ }).click());
  assert.equal(fresh.view.container.querySelector('.companion-resume-steward'), null);
  assert.equal(fresh.view.container.querySelector('.companion-fresh-steward'), null);
});

const offlineSteward = () => {
  const snapshot = snapshotWith({ steward: true });
  const member = snapshot.groups.flatMap((group) => group.members).find((candidate) => candidate.agent_id === S);
  return { ...snapshot, steward_roster: [{ agent_id: S, name: 'steward-k3x', seat: 'active', online: false, resumable: true }], groups: snapshot.groups.map((group) => ({ ...group, members: group.members.map((one) => (one === member ? { ...one, online: false } : one)) })) };
};
const resumeOk = async () => ({ ok: true, json: async () => ({ action: 'resumed' }), text: async () => '' });
const flush = async () => { await Promise.resolve(); await Promise.resolve(); await new Promise((r) => setTimeout(r, 0)); };

test('a resumed Steward shows it starting, not the same choice again, until it is online', async (t) => {
  const { view, harness, controller, snap } = await setup(t, { snapshot: offlineSteward(), fetchImpl: resumeOk });
  await harness.act(() => controller().open(S));
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); await flush(); });
  assert.match(view.container.textContent, /Starting the agent/);
  assert.equal(view.container.querySelector('.companion-resume-steward'), null);
  assert.equal(view.container.querySelector('.companion-fresh-steward'), null);
  await harness.act(() => { snap.value = snapshotWith({ steward: true }); });
  assert.doesNotMatch(view.container.textContent, /Starting the agent/);
  assert.ok(view.container.querySelector('.companion-term-host'), 'the real terminal');
});

test('a plain agent\'s Resume shows it starting too', async (t) => {
  const offline = { ...snapshotWith(), groups: snapshotWith().groups.map((group) => ({ ...group, members: group.members.map((one) => (one.agent_id === A ? { ...one, online: false } : one)) })) };
  const { view, harness, controller } = await setup(t, { snapshot: offline, fetchImpl: resumeOk });
  await harness.act(() => controller().open(A));
  const resume = [...view.container.querySelectorAll('.companion-primary')].find((button) => button.textContent === 'Resume');
  await harness.act(async () => { resume.click(); await flush(); });
  assert.match(view.container.textContent, /Starting the agent/);
});

test('a resume that never comes up falls back to the offline choice', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const { view, harness, controller } = await setup(t, { snapshot: offlineSteward(), fetchImpl: resumeOk });
  const settle = () => harness.act(async () => { for (let i = 0; i < 6; i += 1) await Promise.resolve(); });
  await harness.act(() => controller().open(S));
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); });
  await settle();
  assert.match(view.container.textContent, /Starting the agent/);
  await harness.act(async () => { t.mock.timers.tick(30001); });
  await settle();
  assert.doesNotMatch(view.container.textContent, /Starting the agent/);
  assert.ok(view.container.querySelector('.companion-resume-steward'), 'the way back is open again');
});

test('minimizing while it starts keeps the tab, and offers no second start', async (t) => {
  const { view, harness, controller, snap } = await setup(t, { snapshot: offlineSteward(), fetchImpl: resumeOk });
  await harness.act(() => controller().open(S));
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); await flush(); });
  const ctrlDot = () => harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  await ctrlDot();
  await harness.act(() => { snap.value = { ...offlineSteward() }; });
  await ctrlDot();
  assert.match(view.container.textContent, /Starting the agent/);
  assert.equal(view.container.querySelector('.companion-fresh-steward'), null);
});

test('the ＋ menu offers no resume or fresh start while it starts', async (t) => {
  const { view, harness, controller } = await setup(t, { snapshot: offlineSteward(), fetchImpl: resumeOk });
  await harness.act(() => controller().open(S));
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); await flush(); });
  await harness.act(() => view.container.querySelector('.companion-add').click());
  assert.equal(view.container.querySelector('.companion-menu-resume'), null);
  assert.equal(view.container.querySelector('.companion-menu-fresh'), null);
});

test('a resume that fails stays on the offline choice', async (t) => {
  const fetchImpl = async () => ({ ok: true, json: async () => ({ action: 'error', detail: 'no pane' }), text: async () => '' });
  const { view, harness, controller } = await setup(t, { snapshot: offlineSteward(), fetchImpl });
  await harness.act(() => controller().open(S));
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); await flush(); });
  assert.doesNotMatch(view.container.textContent, /Starting the agent/);
  assert.ok(view.container.querySelector('.companion-resume-steward'));
});

test('a failed resume is reported and leaves the way back open', async (t) => {
  const { resumeAgent } = await (await import('./preact-harness.mjs')).createPreactHarness(t).then((harness) => harness.importDashboardModule('js/companion-approval.js'));
  const answer = (body) => async () => ({ ok: true, json: async () => body });
  await resumeAgent(answer({ action: 'resumed' }), S);
  await assert.rejects(resumeAgent(answer({ action: 'error', detail: 'no pane' }), S), /no pane/);
  await assert.rejects(resumeAgent(answer({ action: 'error:missing_cwd', detail: '/gone' }), S), (error) => error.recoverable === true);
});
