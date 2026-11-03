// Tests for the dashboard companion (tcl-6i0283): chips with a "needs you"
// state, the Steward summon, the native-terminal attach, the inbox message row
// with view context, and the pure placement/snap/status model.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import {
  A,
  B,
  S,
  snapshotWith,
  memoryPrefs,
  fakeDictation,
  SHOWN_WINDOW,
  setup,
  floated,
} from './companion-helpers.mjs';

test('with nothing pinned a single Steward chip invites the operator in', async (t) => {
  const { view, harness } = await setup(t);
  const chip = harness.getByRole(view.container, 'button', { name: /Open the companion/ });
  await harness.act(() => chip.click());
  assert.match(view.container.textContent, /Summon the Steward/);
  assert.ok(!view.container.querySelector('[role="menu"]'), 'the empty window explains itself; the add menu stays closed');
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
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .ui-btn-danger').click(); await Promise.resolve(); await Promise.resolve(); });
  assert.deepEqual(stops(), [['POST', `/api/agents/${encodeURIComponent(S)}/stop`]], 'it stops; it is not retired');
  assert.equal(calls.some(([, url]) => url.endsWith('/retire')), false);
  assert.match(view.container.querySelector('.companion-shutdown-note').textContent, /is shut down/);
  assert.match(view.container.querySelector('.companion-resume-steward').textContent, /Resume/);
  assert.match(view.container.querySelector('.companion-fresh-steward').textContent, /Start fresh/);
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); await Promise.resolve(); await Promise.resolve(); });
  assert.equal(calls.at(-1)[1], `/api/agents/${encodeURIComponent(S)}/resume`, 'Resume is the same agent');
});

test('a Steward tab – only minimizes, and the tab stays pinned', async (t) => {
  const prefs = memoryPrefs();
  const { view, harness, controller } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  const tab = view.container.querySelector('.companion-tab-minimize');
  assert.match(tab.title, /Minimize to dock/);
  await harness.act(() => tab.click());
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(!view.container.querySelector(SHOWN_WINDOW), 'the window went away');
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.tabs')), [S], 'still pinned');
  assert.ok(!view.container.querySelector('.steward-shutdown-confirm'), 'nothing asked');
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
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .ui-btn-danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(stops(), [['POST', `/api/agents/${encodeURIComponent(S)}/stop`]]);
  assert.ok(!view.container.querySelector(SHOWN_WINDOW), 'the window closed');
});

test('⏻ after a cancelled ✕ still keeps the window on the way back', async (t) => {
  const fetchImpl = async () => ({ ok: true, text: async () => '', json: async () => ({}) });
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.overlay-wc-close').click());
  await harness.act(() => view.container.querySelector('.steward-shutdown-actions button:not(.danger)').click());
  await harness.act(() => view.container.querySelector('.steward-shutdown-open').click());
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .ui-btn-danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(view.container.querySelector(SHOWN_WINDOW));
  assert.match(view.container.querySelector('.companion-resume-steward').textContent, /Resume/);
});

test('a floated Steward: its window ✕ asks too, and Cancel keeps it running', async (t) => {
  const calls = [];
  const fetchImpl = async (url, options = {}) => { calls.push([options.method, url]); return { ok: true, text: async () => '', json: async () => ({}) }; };
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  const floated = () => view.container.querySelector('.chat-window');
  const stops = () => calls.filter(([, url]) => url.endsWith('/stop'));
  assert.match(floated().querySelector('.overlay-wc-close').title, /shut it down/);
  await harness.act(() => floated().querySelector('.overlay-wc-close').click());
  assert.ok(floated(), 'still open while it asks');
  await harness.act(() => floated().querySelector('.steward-shutdown-actions button:not(.danger)').click());
  assert.ok(floated(), 'Cancel leaves it');
  assert.deepEqual(stops(), []);

  await harness.act(() => floated().querySelector('.overlay-wc-close').click());
  await harness.act(async () => { floated().querySelector('.steward-shutdown-actions .ui-btn-danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(stops(), [['POST', `/api/agents/${encodeURIComponent(S)}/stop`]]);
  assert.equal(floated(), null, 'the window closed');
});

test('a Steward shut down from its window ✕ leaves Resume behind its chip', async (t) => {
  const fetchImpl = async () => ({ ok: true, text: async () => '', json: async () => ({}) });
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  const floated = () => view.container.querySelector('.chat-window');
  await harness.act(() => floated().querySelector('.overlay-wc-close').click());
  await harness.act(async () => { floated().querySelector('.steward-shutdown-actions .ui-btn-danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(!floated());
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Open the companion/ }).click());
  assert.match(view.container.querySelector('.companion-resume-steward').textContent, /Resume/);
});

test('moving to the Collab tab leaves no tab behind, and a Terminals tab floats as a chat', async (t) => {
  const { view, harness, controller } = await setup(t, { openTerminalsPane: () => {} });
  const tabOfAlpha = () => [...view.container.querySelectorAll('.board-overlay-titlebar .companion-tab')].find((tab) => tab.textContent.includes('alpha'));
  await harness.act(() => controller().open(A));
  await harness.act(() => view.container.querySelector('[aria-label="Open in the Collab tab"]').click());
  assert.ok(!tabOfAlpha(), 'nothing left to close by hand');
  let floated = false;
  await harness.act(() => { floated = controller().float('conv-a'); });
  assert.equal(floated, true, 'a pane may hold the conv id');
  assert.ok(view.container.querySelector('.chat-window .companion-term-host'), 'the chat window has the terminal');
  assert.equal(controller().float('agt_nobody'), false);
});

test('an agent that never had a tab floats in a window of its own', async (t) => {
  const { view, harness, controller } = await setup(t);
  await harness.act(() => controller().open(B));
  await harness.act(() => controller().float(A));
  assert.equal(view.container.querySelectorAll('.chat-window').length, 2);
});

test('reveal shows an agent where the companion holds it', async (t) => {
  const { view, harness, controller, mounted } = await setup(t, { openTerminalsPane: () => {} });
  const reveal = async (selector, options = { tabs: true }) => {
    let answer;
    await harness.act(() => { answer = controller().reveal(selector, options); });
    return answer;
  };
  assert.equal(!!await reveal(A), false, 'nowhere here yet');
  await harness.act(() => controller().open(A));
  await harness.act(() => view.container.querySelector('.chat-window [aria-label^="Minimize alpha"]').click());
  assert.equal(!!await reveal(A, { tabs: false }), false, 'one on the dock only counts when asked');
  assert.equal(!!await reveal('conv-a'), true, 'by its conv id too');
  await harness.act(() => view.container.querySelector('[aria-label="Open in the Collab tab"]').click());
  assert.equal(!!await reveal(A), false, 'gone from here: the Terminals tab holds it');
  await harness.act(() => { controller().float(A); });
  const win = () => view.container.querySelector('.chat-window');
  await harness.act(() => win().querySelector('.overlay-wc-minimize, [aria-label^="Minimize"]').click());
  assert.equal(win()?.querySelector('.companion-term-host') ?? null, null, 'minimized');
  assert.equal(!!await reveal(A), true);
  assert.ok(win().querySelector('.companion-term-host'), 'restored');
  const widget = mounted.at(-1);
  const focused = widget.focused;
  await reveal(A, { tabs: false });
  assert.ok(widget.focused > focused, 'it takes the keyboard');
  assert.equal(view.container.querySelectorAll('.chat-window').length, 1);
});

test('a detached tab has no Collab move: its way back is Bring it back here', async (t) => {
  const { view, harness, controller } = await setup(t, { detachWindow: () => ({ closed: false, focus() {}, close() {} }), openTerminalsPane: () => {} });
  await harness.act(() => controller().open(A));
  assert.ok(view.container.querySelector('[aria-label="Open in the Collab tab"]'));
  await harness.act(() => view.container.querySelector('.companion-detach').click());
  assert.ok(!view.container.querySelector('[aria-label="Open in the Collab tab"]'));
});

test('a floated agent that is no Steward still closes at once', async (t) => {
  const { view, harness, controller } = await setup(t);
  await harness.act(() => controller().open(A));
  const floated = () => view.container.querySelector('.chat-window');
  assert.match(floated().querySelector('.overlay-wc-close').title, /keeps running/);
  assert.ok(!floated().querySelector('.steward-shutdown-open'));
  await harness.act(() => floated().querySelector('.overlay-wc-close').click());
  assert.equal(floated(), null);
});

test('the window ✕ on several agents takes them all off the dock, still running, and they open again', async (t) => {
  const { view, harness, controller, requests } = await setup(t);
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(() => controller().open(A));
  await harness.act(() => controller().open(B));
  assert.deepEqual(overlays.overlayWindows.peek().map((window) => window.tabs.length), [2], 'one window, two tabs');
  const close = view.container.querySelector('.overlay-wc-close');
  assert.match(close.title, /keeps running in the background/);
  await harness.act(async () => { close.click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(overlays.overlayWindows.peek(), []);
  assert.deepEqual(overlays.minimizedOverlayTabs.peek(), [], 'no pill is left on the dock');
  assert.ok(!view.container.querySelector('.steward-shutdown-confirm'), 'nothing asked');
  assert.deepEqual(requests.filter(({ url }) => /\/(stop|retire)$/.test(url)), [], 'nothing stopped');
  await harness.act(() => controller().open(A));
  assert.ok(view.container.querySelector(`${SHOWN_WINDOW} [data-chat-agent="${A}"]`), 'it opens again from where agents open');
});

test('a tab – sends that agent to the dock, still running', async (t) => {
  const { view, harness, controller, requests } = await setup(t);
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(() => controller().open(A));
  await harness.act(() => controller().open(B));
  const tabMinimize = [...view.container.querySelectorAll('.companion-tab')].find((tab) => tab.textContent.includes('bravo')).querySelector('.companion-tab-minimize');
  assert.ok(tabMinimize.querySelector('svg path'), 'a drawn glyph, centred in its box');
  await harness.act(() => tabMinimize.click());
  assert.deepEqual(overlays.minimizedOverlayTabs.peek().map(({ tab }) => tab.id), [`chat:${B}`]);
  assert.deepEqual(overlays.overlayWindows.peek().map((window) => window.tabs.map((tab) => tab.id)), [[`chat:${A}`]]);
  assert.deepEqual(requests.filter(({ url }) => /\/(stop|retire)$/.test(url)), [], 'nothing stopped');
});

test('a tab × is the window ✕ for that tab: it leaves window and dock, still running, and stays gone on reload', async (t) => {
  const prefs = memoryPrefs();
  const { view, harness, controller, requests } = await setup(t, { prefs });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(() => controller().open(A));
  await harness.act(() => controller().open(B));
  const bravo = [...view.container.querySelectorAll('.companion-tab')].find((tab) => tab.textContent.includes('bravo'));
  const buttons = [...bravo.querySelectorAll('button:not([role="tab"])')];
  assert.deepEqual(buttons.map((button) => button.className), ['companion-tab-minimize', 'companion-tab-close'], 'the title bar\'s order: – then ×');
  const tabClose = bravo.querySelector('.companion-tab-close');
  assert.match(tabClose.title, /Detach to background/);
  assert.match(tabClose.getAttribute('aria-label'), /Detach .*to background/);
  await harness.act(async () => { tabClose.click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(overlays.overlayWindows.peek().map((window) => window.tabs.map((tab) => tab.id)), [[`chat:${A}`]], 'the other tab stays');
  assert.deepEqual(overlays.minimizedOverlayTabs.peek(), [], 'no pill is left on the dock');
  assert.ok(!view.container.querySelector('.steward-shutdown-confirm'), 'nothing asked');
  assert.deepEqual(requests.filter(({ url }) => /\/(stop|retire)$/.test(url)), [], 'nothing stopped');
  assert.ok(!JSON.parse(prefs.store.get('tclaude.dash.companion.tabs') || '[]').includes(B), 'not brought back by a reload');
});

test('a Steward tab × asks what the window ✕ asks, and the others stay', async (t) => {
  const calls = [];
  const fetchImpl = async (url, options = {}) => { calls.push([options.method, url]); return { ok: true, text: async () => '', json: async () => ({}) }; };
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(() => controller().open(S));
  await harness.act(() => controller().open(A));
  const tabsOf = () => overlays.overlayWindows.peek().map((window) => window.tabs.map((tab) => tab.id));
  const stewardTab = view.container.querySelector(`.companion-tab[data-tab-id="chat:${S}"]`);
  const tabClose = stewardTab.querySelector('.companion-tab-close');
  assert.match(tabClose.title, /shut it down, after asking/);
  await harness.act(() => tabClose.click());
  assert.match(view.container.querySelector('.steward-shutdown-confirm').textContent, /keeps its conversation/, 'the Steward asks, as the window ✕ does');
  assert.deepEqual(tabsOf(), [[`chat:${S}`, `chat:${A}`]], 'nothing left while it asks');
  assert.deepEqual(calls.filter(([, url]) => url.endsWith('/stop')), []);
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .ui-btn-danger').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(calls.filter(([, url]) => url.endsWith('/stop')), [['POST', `/api/agents/${encodeURIComponent(S)}/stop`]]);
  assert.deepEqual(tabsOf(), [[`chat:${A}`]], 'the worker stays in its window');
});

test('the window ✕ beside a running Steward takes the others off, and the Steward still asks', async (t) => {
  const calls = [];
  const fetchImpl = async (url, options = {}) => { calls.push([options.method, url]); return { ok: true, text: async () => '', json: async () => ({}) }; };
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(() => controller().open(S));
  await harness.act(() => controller().open(A));
  const tabsOf = () => overlays.overlayWindows.peek().map((window) => window.tabs.map((tab) => tab.id));
  assert.deepEqual(tabsOf(), [[`chat:${S}`, `chat:${A}`]]);
  const close = view.container.querySelector('.overlay-wc-close');
  assert.match(close.title, /Steward shuts down, after asking/);
  await harness.act(() => close.click());
  assert.deepEqual(tabsOf(), [[`chat:${S}`]], 'the worker left');
  assert.deepEqual(overlays.minimizedOverlayTabs.peek(), [], 'with no pill');
  assert.match(view.container.querySelector('.steward-shutdown-confirm').textContent, /keeps its conversation/, 'the Steward asks, as before');
  assert.deepEqual(calls.filter(([, url]) => url.endsWith('/stop')), []);
});

test('a refused shut-down stays on the Steward and says why', async (t) => {
  const fetchImpl = async () => ({ ok: false, status: 409, text: async () => '{"message":"nothing to retire"}', json: async () => ({}) });
  const { view, harness, controller } = await setup(t, { fetchImpl, snapshot: snapshotWith({ steward: true }) });
  await harness.act(() => controller().open(S));
  await harness.act(() => view.container.querySelector('.steward-shutdown-open').click());
  await harness.act(async () => { view.container.querySelector('.steward-shutdown-actions .ui-btn-danger').click(); await new Promise((resolve) => { setTimeout(resolve, 0); }); });
  assert.match(view.container.querySelector('.steward-shutdown-error').textContent, /nothing to retire/);
  assert.ok(!view.container.querySelector('.companion-shutdown-note'));
});

test('the access controls are the Steward\'s alone', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, controller } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }), openPermissions: () => {}, openSudo: () => {} });
  await harness.act(() => controller().open(A));
  assert.ok(harness.getByRole(view.container, 'tab', { name: /alpha/ }));
  assert.ok(!view.container.querySelector('.companion-steward-perms'));
  assert.ok(!view.container.querySelector('.companion-steward-sudo'));
  assert.ok(!view.container.querySelector('.steward-shutdown-open'));
});

test('with no Steward running the empty window says where its access controls will be', async (t) => {
  const { view, harness } = await setup(t, { openPermissions: () => {}, openSudo: () => {} });
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Open the companion/ }).click());
  assert.ok(!view.container.querySelector('.companion-steward-perms'));
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

test('a kept agent that needs the operator says so on its dock pill, which peeks', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, snap, controller } = await setup(t, { prefs, snapshot: snapshotWith({ aStatus: 'awaiting_permission' }) });
  assert.match(view.container.textContent, /1 companion need you/);
  const facade = await harness.importDashboardModule('js/companion-facade.js');
  t.after(facade.registerCompanionController(controller()));
  const { BoardOverlay } = await harness.importDashboardModule('js/board-overlay.js');
  const dock = await harness.mount(harness.html`<${BoardOverlay} snapshot=${snap} />`);
  const pill = dock.container.querySelector('.overlay-dock-pill');
  assert.ok(pill.classList.contains('needs'));
  await harness.act(async () => {
    pill.querySelector('.overlay-dock-item').dispatchEvent(new harness.window.Event('mouseenter'));
    await new Promise((r) => setTimeout(r, 260));
  });
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.match(view.container.querySelector('.companion-peek').textContent, /Allow this command/);
});

test('the Steward chip stands for its window, and other windows keep their pills', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([S, A]) });
  const { view, harness, snap, controller } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }) });
  const { BoardOverlay } = await harness.importDashboardModule('js/board-overlay.js');
  const dock = await harness.mount(harness.html`<${BoardOverlay} snapshot=${snap} />`);
  assert.ok(!dock.container.querySelector('.overlay-dock-pill'), 'its window minimized holds the Steward');
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /steward-k3x: / }).click());
  assert.match(view.container.querySelector('.chat-window').getAttribute('aria-label'), /steward-k3x/);
  assert.ok(!view.container.querySelector('.companion-chip[aria-label^="steward-k3x"]'), 'the chip steps aside while it shows');
  await harness.act(() => controller().float(B));
  await harness.act(() => view.container.querySelector('section[aria-label="Chat with bravo"] [aria-label^="Minimize bravo"]').click());
  assert.match(dock.container.querySelector('.overlay-dock-pill').textContent, /bravo/);
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
  assert.ok(!view.container.querySelector(SHOWN_WINDOW));
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
  assert.ok(!view.container.querySelector('.companion-window'), 'released');
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
  assert.ok(!view.container.querySelector('.companion-window'), 'the hidden window is gone');
  await harness.act(() => { snap.value = before; });
  await settle();
  assert.equal(mounted.length, attached, 'coming back does not attach either');
});

test('a click on empty window space gives focus back to the terminal', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, mounted } = await setup(t, { prefs });
  await harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const titlebar = view.container.querySelector('.board-overlay-titlebar');
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

test('Ctrl+. toggles the window, and a tab × sends it to the dock', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const { view, harness, mounted } = await setup(t, { prefs });
  const key = () => harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  await key();
  assert.ok(view.container.querySelector('.companion-window'), 'window opened');
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  await key();
  assert.ok(!view.container.querySelector(SHOWN_WINDOW), 'window minimized');
  assert.equal(mounted[0].disposed, false, 'a minimized window keeps its terminal attached');
  await key();
  await harness.act(() => view.container.querySelector('.companion-tab-minimize').click());
  assert.ok(!view.container.querySelector(SHOWN_WINDOW));
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.tabs')), [A], 'a tab × stops nothing, and the agent waits on the dock');
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(async () => { overlays.closeOverlayTab(`chat:${A}`); });
  assert.deepEqual(JSON.parse(prefs.store.get('tclaude.dash.companion.tabs')), []);
});

test('no window is home: an agent joins the chats used last, or gets a window of its own', async (t) => {
  const { harness, controller } = await setup(t);
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  const windowsOf = () => overlays.overlayWindows.peek().map((window) => window.tabs.map((tab) => tab.id));
  await harness.act(() => controller().open(A));
  await harness.act(() => controller().open(B));
  assert.deepEqual(windowsOf(), [[`chat:${A}`, `chat:${B}`]], 'B joins the chat the operator was in');
  await harness.act(async () => {
    overlays.tearOverlayTabOut(`chat:${B}`, null);
    overlays.closeOverlayTab(`chat:${A}`);
  });
  await harness.act(() => controller().open(A));
  assert.deepEqual(windowsOf(), [[`chat:${B}`, `chat:${A}`]], 'the window B tore out to is as good as any');
  await harness.act(async () => {
    overlays.closeOverlayTab(`chat:${A}`);
    overlays.closeOverlayTab(`chat:${B}`);
    overlays.openTabInWindow('', '', { id: 'board-1', kind: 'board', label: 'plan' });
  });
  await harness.act(() => controller().open(A));
  assert.deepEqual(windowsOf(), [['board-1'], [`chat:${A}`]], 'with no chat up, it takes a window of its own');
});

test('Ctrl+. opens the Steward past a worker\'s window, never minimizing that', async (t) => {
  const { view, harness, controller } = await setup(t, { snapshot: snapshotWith({ steward: true }) });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(() => controller().open(A));
  await harness.act(async () => { overlays.closeOverlayTab(`chat:${S}`); });
  const key = () => harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  await key();
  const shown = overlays.overlayWindows.peek().filter((window) => !window.minimized).map((window) => window.active);
  assert.ok(shown.includes(`chat:${S}`), 'the Steward shows');
  assert.ok(view.container.querySelector(`[data-chat-agent="${S}"]`), 'its chat is drawn');
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
