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
  offlineSteward,
  resumeOk,
  flush,
  chatOf,
  floated,
  flushed,
  bringOnline,
  mailable,
} from './companion-helpers.mjs';

test('with no agent showing the companion has no cog', async (t) => {
  const { view, harness } = await setup(t);
  await harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: '.', ctrlKey: true })));
  assert.ok(view.container.querySelector(SHOWN_WINDOW));
  assert.ok(!view.container.querySelector('.companion-cog-button'));
});

test('a floated Steward keeps its tools under the cog', async (t) => {
  const { view, harness, controller } = await setup(t, { snapshot: snapshotWith({ steward: true }), openPermissions: () => {}, openSudo: () => {} });
  await harness.act(() => controller().open(S));
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
  await harness.act(async () => { clear().querySelector('.ui-btn-danger').click(); await new Promise((resolve) => { setTimeout(resolve, 0); }); });
  assert.deepEqual(calls.filter(([url]) => url.endsWith('/reincarnate')), [[`/api/agents/${encodeURIComponent(A)}/reincarnate`, '{"mode":"clear"}']]);
});

test('an offline Steward in the seat offers Resume and Start fresh; with none, only a summon', async (t) => {
  const snapshot = snapshotWith({ steward: true });
  snapshot.groups[1].members[0].online = false;
  snapshot.steward_roster = [{ agent_id: S, name: 'steward-k3x', seat: 'active', online: false, last_ran: new Date(Date.now() - 2 * 3600e3).toISOString(), keeps: ['node.exec'] }];
  const first = await setup(t, { snapshot });
  const { view, harness, controller } = first;
  await harness.act(() => harness.getByRole(view.container, 'button', { name: /Open the companion/ }).click());
  assert.match(view.container.querySelector('.companion-resume-steward').textContent, /Resume/);
  assert.match(view.container.textContent, /Last ran 2h ago/);
  assert.match(view.container.textContent, /It keeps what you gave it separately: node\.exec/, 'Start fresh says what stays with the old one');
  assert.ok(view.container.querySelector('.companion-fresh-steward'));

  const fresh = await setup(t, { reloading: first, snapshot: snapshotWith() });
  await fresh.harness.act(() => fresh.harness.getByRole(fresh.view.container, 'button', { name: /Open the companion/ }).click());
  assert.ok(!fresh.view.container.querySelector('.companion-resume-steward'));
  assert.ok(!fresh.view.container.querySelector('.companion-fresh-steward'));
});

test('a resumed Steward shows it starting, not the same choice again, until it is online', async (t) => {
  const { view, harness, controller, snap } = await setup(t, { snapshot: offlineSteward(), fetchImpl: resumeOk });
  await harness.act(() => controller().open(S));
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); await flush(); });
  assert.match(view.container.textContent, /Starting the agent/);
  assert.ok(!view.container.querySelector('.companion-resume-steward'));
  assert.ok(!view.container.querySelector('.companion-fresh-steward'));
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
  assert.ok(!view.container.querySelector('.companion-fresh-steward'));
});

test('the ＋ menu offers no resume or fresh start while it starts', async (t) => {
  const { view, harness, controller } = await setup(t, { snapshot: offlineSteward(), fetchImpl: resumeOk });
  await harness.act(() => controller().open(S));
  await harness.act(async () => { view.container.querySelector('.companion-resume-steward').click(); await flush(); });
  await harness.act(() => view.container.querySelector('.companion-add').click());
  assert.ok(!view.container.querySelector('.companion-menu-resume'));
  assert.ok(!view.container.querySelector('.companion-menu-fresh'));
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

test('a chat moves to a separate window, and its tab stays as the way back', async (t) => {
  const places = [];
  const { view, harness, controller, mounted } = await setup(t, { detachWindow: (agent, _label, options) => { places.push([agent, options]); return { closed: false, focus() {}, close() {} }; } });
  await harness.act(async () => { controller().float(A); await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(() => view.container.querySelector('.chat-window .companion-detach').click());
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(places.length, 1);
  assert.ok(mounted.at(-1).disposed, 'one place at a time');
  assert.ok(!view.container.querySelector('.companion-term-host'));
  assert.match(view.container.querySelector('.chat-window').textContent, /is open in its own window/);
  await harness.act(async () => { assert.equal(controller().reclaim(A), true); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(view.container.querySelector('.companion-term-host'));
  assert.equal(controller().reclaim(A), false, 'nothing is waiting any more');
});

test('a chat tab takes dictation, and keeps an unsent draft while its window shows another tab', async (t) => {
  const fake = fakeDictation();
  const { view, harness, controller } = await floated(t, { dictationImpl: fake.impl, snapshot: { ...snapshotWith() } });
  await harness.act(() => controller().open(B));
  await harness.act(() => controller().float(B));
  const windows = await harness.importDashboardModule('js/overlay-instances.js');
  t.after(() => { windows.overlayWindows.value = []; });
  const [first, second] = windows.overlayWindows.value;
  const [a, b] = [first.tabs[0].id, second.tabs[0].id];
  const show = async (id) => {
    await harness.act(async () => { windows.activateOverlayTab(id); });
    await harness.act(async () => { await new Promise((r) => setTimeout(r, 20)); });
  };
  await harness.act(async () => { windows.moveOverlayTab(b, first.id, 1); });
  await show(a);
  await fake.say(harness, view, 'half a thought');
  const draftField = () => view.container.querySelector('.chat-window .companion-draft');
  assert.equal(draftField().value, 'half a thought', 'dictation reaches the draft');
  await show(b);
  assert.equal(draftField(), null, 'the other tab has no such draft');
  await show(a);
  assert.equal(draftField().value, 'half a thought');
});

test('a chat tab has the companion\'s title bar tools', async (t) => {
  const opened = [];
  const snapshot = snapshotWith({ steward: true });
  const { view, harness } = await floated(t, {
    snapshot, agent: S, onComposeMessage: (message) => opened.push(message.agent),
    openPermissions: () => {}, openSudo: () => {},
  });
  const chat = chatOf(harness, view);
  await harness.act(() => chat.querySelector('.companion-compose').click());
  assert.deepEqual(opened, [S]);
  assert.ok(chat.querySelector('.companion-cog-button'));
});

test('an offline agent resumes in its chat tab and shows it starting', async (t) => {
  const offline = structuredClone(snapshotWith());
  offline.groups[0].members[0].online = false;
  const calls = [];
  const fetchImpl = async (url) => { calls.push(String(url)); return { ok: true, status: 200, json: async () => ({ action: 'resumed' }), text: async () => '' }; };
  const { view, harness, snap } = await floated(t, { fetchImpl });
  await harness.act(() => { snap.value = offline; });
  const chat = chatOf(harness, view);
  assert.match(chat.textContent, /alpha is offline/);
  await harness.act(async () => { chat.querySelector('.companion-primary').click(); await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(calls.some((url) => url.includes('resume')), calls.join());
  assert.match(chatOf(harness, view).textContent, /Starting the agent/);
});

test('a minimized chat tab keeps its terminal warm and hidden, and shows it again', async (t) => {
  const { view, harness, mounted } = await floated(t);
  const windows = await harness.importDashboardModule('js/overlay-instances.js');
  t.after(() => { windows.overlayWindows.value = []; });
  const attached = mounted.length;
  await harness.act(async () => { windows.minimizeOverlayWindow(windows.overlayWindows.value[0].tabs[0].id); });
  const hidden = view.container.querySelector('.chat-window.companion-window-warm');
  assert.ok(hidden, 'it stays in the page, out of sight');
  assert.ok(hidden.querySelector('.companion-term-host'));
  await harness.act(async () => { windows.openOverlayWindow(windows.overlayWindows.value[0].tabs[0]); });
  assert.ok(!view.container.querySelector('.chat-window.companion-window-warm'));
  assert.equal(mounted.length, attached, 'no second attach');
});

test('a chat tab shows who needs the operator', async (t) => {
  const { view, harness } = await floated(t, { snapshot: snapshotWith({ aStatus: 'awaiting_permission' }) });
  assert.match(view.container.querySelector('.chat-window .companion-needs-bar').textContent, /alpha is waiting for you: Bash\(rm -rf build\)/);
});

test('a chat tab has the sudo band and state, the access count, the roster and Clear context', async (t) => {
  const opened = [];
  const snapshot = {
    ...snapshotWith({ steward: true }),
    steward_elevated: { [S]: { standing: ['host.read'], inherited: [] } },
    access_requests: [{ id: 7, kind: 'grant', from_agent: B, slugs: ['host.read'], status: 'pending' }],
    steward_roster: [{ agent_id: S, seat: 'primary', online: true }, { agent_id: B, seat: 'standby', online: true }],
  };
  const { view, harness } = await floated(t, { snapshot, agent: S, openSudo: (agent) => opened.push(agent), openPermissions: () => {}, openStewardHandover: () => {} });
  const chat = chatOf(harness, view);
  assert.ok(chat.querySelector('.companion-elevated'), 'the band');
  await harness.act(() => chat.querySelector('.companion-state-sudo').click());
  assert.deepEqual(opened, [S]);
  assert.match(chat.querySelector('.companion-access').textContent, /1/);
  assert.match(chat.querySelector('.companion-state-roster').textContent, /1/);
  await harness.act(() => chat.querySelector('.companion-cog-button').click());
  assert.ok(chat.querySelector('.companion-cog-menu .companion-clear-open, .companion-cog-menu [class*="clear"]'), 'Clear context is on its cog');
});

test('voice turns on and off for an agent in a chat tab, and is saved', async (t) => {
  const prefs = memoryPrefs();
  const { view, harness } = await floated(t, { prefs, speakerImpl: { supported: true, speak() {}, cancel() {} } });
  const button = () => view.container.querySelector('.chat-window .companion-voice');
  assert.equal(button().getAttribute('aria-pressed'), 'false');
  await harness.act(() => button().click());
  assert.equal(button().getAttribute('aria-pressed'), 'true');
  assert.deepEqual(JSON.parse(prefs.getItem('tclaude.dash.companion.voice') || '[]'), [A]);
  await harness.act(() => button().click());
  assert.equal(button().getAttribute('aria-pressed'), 'false');
});

test('a hidden chat attaches nothing: not for an agent that came up, nor one that came back', async (t) => {
  const windows = (h) => h.importDashboardModule('js/overlay-instances.js');
  const live = snapshotWith();
  const { view, harness, snap, mounted } = await floated(t, { snapshot: bringOnline(live, false) });
  const w = await windows(harness);
  t.after(() => { w.overlayWindows.value = []; });
  await harness.act(async () => { w.minimizeOverlayWindow(w.overlayWindows.value[0].tabs[0].id); });
  const attached = mounted.length;
  await harness.act(() => { snap.value = live; });
  await flushed(harness);
  assert.equal(mounted.length, attached, 'minimized while starting: no terminal for a window nobody can see');
  await harness.act(async () => { w.openOverlayWindow(w.overlayWindows.value[0].tabs[0]); });
  await flushed(harness);
  assert.ok(view.container.querySelector('.chat-window .companion-term-host'));
  await harness.act(async () => { w.minimizeOverlayWindow(w.overlayWindows.value[0].tabs[0].id); });
  await harness.act(() => { snap.value = bringOnline(live, false); });
  await flushed(harness);
  await harness.act(() => { snap.value = live; });
  await flushed(harness);
  assert.ok(!view.container.querySelector('.chat-window.companion-window-warm'), 'a restart while hidden lets go of it');
});

test('a chat tab says the view it floats over, and keeps its draft and mail without a terminal', async (t) => {
  const fake = fakeDictation();
  const requests = [];
  const fetchImpl = async (url, init = {}) => { requests.push([String(url), init.body ? JSON.parse(init.body) : null]); return { ok: true, status: 202, json: async () => ({ id: 1 }) }; };
  const { view, harness, mounted } = await floated(t, { fetchImpl, dictationImpl: fake.impl, snapshot: mailable() });
  await flushed(harness);
  await fake.say(harness, view, 'what is here');
  const chat = () => view.container.querySelector('.chat-window');
  await harness.act(() => mounted.at(-1).options.onStatus('disconnected'));
  assert.equal(chat().querySelector('.companion-draft').value, 'what is here', 'a lost terminal takes no draft away');
  assert.ok(chat().querySelector('.companion-mic'));
  await harness.act(async () => { chat().querySelector('.companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true })); await new Promise((r) => setTimeout(r, 0)); });
  const sent = JSON.stringify(requests.find(([, body]) => body?.to === A));
  assert.match(sent, /what is here/);
  assert.match(sent, /Processes/i, 'the view is the one it floats over');
  assert.doesNotMatch(sent, /Terminals|Collab \(/);
});

test('voice stays on when a tab floats, and typing in a chat draft stops speech', async (t) => {
  let cancelled = 0;
  const fake = fakeDictation();
  const prefs = memoryPrefs();
  const speakerImpl = { supported: true, speak() {}, cancel() { cancelled += 1; } };
  const { view, harness, controller } = await setup(t, { prefs, speakerImpl, dictationImpl: fake.impl, snapshot: { ...snapshotWith() } });
  await harness.act(() => controller().open(A));
  await harness.act(() => view.container.querySelector('.companion-voice').click());
  await harness.act(() => controller().float(A));
  await flushed(harness);
  assert.equal(view.container.querySelector('.chat-window .companion-voice').getAttribute('aria-pressed'), 'true');
  await fake.say(harness, view, 'x');
  const before = cancelled;
  await harness.input(view.container.querySelector('.chat-window .companion-draft'), 'typed');
  assert.ok(cancelled > before);
});

test('a chat tab notes capture off, and offers Reconnect after an error', async (t) => {
  const { view, harness, mounted } = await floated(t, { dictationImpl: fakeDictation().impl, snapshot: mailable({ steward_access: { capture_off: [A] } }) });
  await flushed(harness);
  assert.ok(view.container.querySelector('.chat-window .companion-context-off'));
  await harness.act(() => mounted.at(-1).options.onStatus('error'));
  const button = [...view.container.querySelectorAll('.chat-window .companion-term-status button')].find((node) => node.textContent === 'Reconnect');
  assert.ok(button);
});

test('a hand-over waits while the operator has a draft in a chat tab', async (t) => {
  const fake = fakeDictation();
  const notes = [];
  const snapshot = { ...snapshotWith() };
  const { view, harness, snap } = await floated(t, { dictationImpl: fake.impl, snapshot, notify: (text) => notes.push(text) });
  await flushed(harness);
  await fake.say(harness, view, 'still writing');
  assert.equal(view.container.querySelector('.chat-window .companion-draft').value, 'still writing');
  await harness.act(() => { snap.value = { ...snapshot, companion_cues: [{ id: 1, open: B, agent_name: 'Steward', at: Date.now() }] }; });
  await harness.act(() => { snap.value = { ...snapshot, companion_cues: [{ id: 2, open: B, agent_name: 'Steward', at: Date.now() }] }; });
  await flushed(harness);
  assert.ok(notes.some((text) => /pinned bravo/.test(text)), notes.join('|'));
});

test('a draft delivered after its tab left stays delivered, and a failure still says so', async (t) => {
  const fake = fakeDictation();
  const notes = [];
  let answer = null;
  const fetchImpl = async (url) => {
    if (String(url) !== '/api/operator-message') return { ok: true, status: 202, json: async () => ({}) };
    return new Promise((resolve) => { answer = resolve; });
  };
  const { view, harness, controller } = await floated(t, { fetchImpl, dictationImpl: fake.impl, notify: (text) => notes.push(text), snapshot: mailable() });
  await harness.act(() => controller().open(B));
  await harness.act(() => controller().float(B));
  const windows = await harness.importDashboardModule('js/overlay-instances.js');
  t.after(() => { windows.overlayWindows.value = []; });
  const [first, second] = windows.overlayWindows.value;
  const [a, b] = [first.tabs[0].id, second.tabs[0].id];
  const show = async (id) => { await harness.act(async () => { windows.activateOverlayTab(id); }); await flushed(harness); };
  await harness.act(async () => { windows.moveOverlayTab(b, first.id, 1); });
  await show(a);
  const send = async (text) => {
    await fake.say(harness, view, text);
    await harness.act(async () => { view.container.querySelector('.chat-window .companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true })); await new Promise((r) => setTimeout(r, 0)); });
    await show(b);
  };
  await send('once only');
  await harness.act(async () => { answer({ ok: true, status: 202, json: async () => ({ id: 1 }) }); await new Promise((r) => setTimeout(r, 0)); });
  await show(a);
  assert.ok(!view.container.querySelector('.chat-window .companion-draft'), 'it does not come back to be sent twice');
  await send('this one fails');
  await harness.act(async () => { answer({ ok: false, status: 500, statusText: 'nope', json: async () => ({}) }); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(notes.some((text) => /Message to alpha failed/.test(text)), notes.join('|'));
  await show(a);
  assert.equal(view.container.querySelector('.chat-window .companion-draft').value, 'this one fails');
});

test('Unpin on the dock stops voice, as it does on a tab', async (t) => {
  const { view, harness } = await floated(t, { speakerImpl: { supported: true, speak() {}, cancel() {} } });
  const windows = await harness.importDashboardModule('js/overlay-instances.js');
  t.after(() => { windows.overlayWindows.value = []; windows.minimizedOverlayTabs.value = []; });
  await harness.act(() => view.container.querySelector('.chat-window .companion-voice').click());
  const tab = windows.overlayWindows.value[0].tabs[0];
  await harness.act(async () => { windows.minimizeOverlayTab(tab.id); });
  await flushed(harness);
  await harness.act(async () => { windows.closeOverlayTab(tab.id); });
  await flushed(harness);
  await harness.act(async () => { windows.openOverlayWindow(tab); });
  await flushed(harness);
  assert.equal(view.container.querySelector('.chat-window .companion-voice').getAttribute('aria-pressed'), 'false');
});

test('a chat tab keeps the dictated mark and the view choice across a tab switch', async (t) => {
  const fake = fakeDictation();
  const requests = [];
  const fetchImpl = async (url, init = {}) => { requests.push([String(url), init.body ? JSON.parse(init.body) : null]); return { ok: true, status: 202, json: async () => ({ id: 1 }) }; };
  const snapshot = mailable({ voice: { mark_dictated: true } });
  const { view, harness, controller } = await floated(t, { fetchImpl, dictationImpl: fake.impl, snapshot });
  await harness.act(() => controller().open(B));
  await harness.act(() => controller().float(B));
  const windows = await harness.importDashboardModule('js/overlay-instances.js');
  t.after(() => { windows.overlayWindows.value = []; });
  const [first, second] = windows.overlayWindows.value;
  const [a, b] = [first.tabs[0].id, second.tabs[0].id];
  const show = async (id) => { await harness.act(async () => { windows.activateOverlayTab(id); }); await flushed(harness); };
  await harness.act(async () => { windows.moveOverlayTab(b, first.id, 1); });
  await show(a);
  const context = () => view.container.querySelector('.chat-window .companion-context input');
  assert.equal(context().hasAttribute('checked'), true, 'view starts on');
  await harness.act(() => { context().checked = false; context().dispatchEvent(new harness.window.Event('change', { bubbles: true })); });
  await fake.say(harness, view, 'spoken words');
  await show(b);
  await show(a);
  assert.equal(context().hasAttribute('checked'), false, 'view stays off');
  await harness.act(async () => { view.container.querySelector('.chat-window .companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true })); await new Promise((r) => setTimeout(r, 0)); });
  const sent = requests.find(([url]) => url === '/api/operator-message')[1].body;
  const { markDictated } = await harness.importDashboardModule('js/companion-model.js');
  assert.equal(sent, markDictated('spoken words'), 'marked as dictated, with no view attached');
});

test('a Classic chat can be minimized, and a pill brings it back with the keyboard', async (t) => {
  const snapshot = { ...snapshotWith(), window_controls: 'classic' };
  const { view, harness, mounted } = await floated(t, { snapshot });
  const windows = await harness.importDashboardModule('js/overlay-instances.js');
  t.after(() => { windows.overlayWindows.value = []; });
  await harness.act(() => view.container.querySelector('.chat-window [aria-label="Minimize alpha"].board-overlay-close').click());
  assert.ok(view.container.querySelector('.chat-window.companion-window-warm'));
  const focused = mounted.at(-1).focused;
  const tab = windows.overlayTabs.value[0];
  await harness.act(async () => { windows.openOverlayWindow(tab); windows.focusOverlayWindow(tab.id); });
  await flushed(harness);
  assert.ok(mounted.at(-1).focused > focused);
});

test('marking up from a chat tab draws above the windows, which step aside', async (t) => {
  const { view, harness } = await floated(t, { snapshot: mailable() });
  await harness.act(() => view.container.querySelector('.chat-window .companion-annotate').click());
  await flushed(harness);
  const root = view.container.querySelector('.annotate-root');
  assert.equal(root.closest('.companion-window'), null, 'a hidden window would hide it too');
  assert.equal(harness.document.body.classList.contains('companion-annotating'), true);
});

test('a reply landing leaves the operator typing in another window', async (t) => {
  const fake = fakeDictation();
  let answer = null;
  const fetchImpl = async (url) => {
    if (String(url) !== '/api/operator-message') return { ok: true, status: 202, json: async () => ({}) };
    return new Promise((resolve) => { answer = resolve; });
  };
  const { view, harness, mounted } = await floated(t, { fetchImpl, dictationImpl: fake.impl, snapshot: mailable() });
  await fake.say(harness, view, 'over to you');
  await harness.act(async () => { view.container.querySelector('.chat-window .companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true })); await new Promise((r) => setTimeout(r, 0)); });
  const elsewhere = harness.document.createElement('textarea');
  elsewhere.className = 'companion-draft';
  harness.document.body.append(elsewhere);
  t.after(() => elsewhere.remove());
  elsewhere.focus();
  const focused = mounted.at(-1).focused;
  await harness.act(async () => { answer({ ok: true, status: 202, json: async () => ({ id: 1 }) }); await new Promise((r) => setTimeout(r, 0)); });
  await flushed(harness);
  assert.equal(harness.document.activeElement, elsewhere);
  assert.equal(mounted.at(-1).focused, focused);
  assert.ok(!view.container.querySelector('.chat-window .companion-draft'), 'and the message did go');
});

test('a reply landing leaves the operator typing on in the same field', async (t) => {
  const fake = fakeDictation();
  let answer = null;
  const fetchImpl = async (url) => {
    if (String(url) !== '/api/operator-message') return { ok: true, status: 202, json: async () => ({}) };
    return new Promise((resolve) => { answer = resolve; });
  };
  const { view, harness, mounted } = await floated(t, { fetchImpl, dictationImpl: fake.impl, snapshot: mailable() });
  await fake.say(harness, view, 'first');
  const field = () => view.container.querySelector('.chat-window .companion-draft');
  await harness.act(async () => { view.container.querySelector('.chat-window .companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true })); await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(() => { field().focus(); field().value = 'first and more'; field().dispatchEvent(new harness.window.Event('input', { bubbles: true })); });
  const focused = mounted.at(-1).focused;
  await harness.act(async () => { answer({ ok: true, status: 202, json: async () => ({ id: 1 }) }); await new Promise((r) => setTimeout(r, 0)); });
  await flushed(harness);
  assert.equal(field().value, 'first and more');
  assert.equal(harness.document.activeElement, field());
  assert.equal(mounted.at(-1).focused, focused);
});

test('a send landing with the terminal lost leaves the field for the next message', async (t) => {
  const fake = fakeDictation();
  const { view, harness, mounted } = await floated(t, { dictationImpl: fake.impl });
  await fake.say(harness, view, 'still here');
  const field = () => view.container.querySelector('.chat-window .companion-draft');
  await harness.act(async () => { field().blur(); field().focus(); mounted.at(-1).options.onStatus('disconnected'); });
  await harness.act(async () => { view.container.querySelector('.chat-window .companion-input').dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true })); await new Promise((r) => setTimeout(r, 0)); });
  await flushed(harness);
  assert.equal(field().value, '', 'it went');
  assert.equal(harness.document.activeElement, field(), 'and the keyboard stays to write the next');
});

test('a helper that only stopped gets its tab back when it returns', async (t) => {
  const P = `agt_${'d'.repeat(32)}`;
  const withScribe = (online) => {
    const snapshot = snapshotWith();
    snapshot.groups.push({ name: 'process-scribe', scribe: true, members: [{ agent_id: P, conv_id: 'conv-p', title: 'scribe', online, state: { status: 'idle' } }] });
    return snapshot;
  };
  const prefs = memoryPrefs();
  const { harness, snap } = await setup(t, { prefs, snapshot: withScribe(true) });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  const has = () => overlays.overlayTabs.value.some((tab) => tab.agent === P);
  assert.ok(has());
  await harness.act(async () => { snap.value = withScribe(false); });
  assert.equal(has(), false);
  await harness.act(async () => { snap.value = withScribe(true); });
  assert.ok(has(), 'the operator never closed it');
});

test('a tab waiting behind the Steward shows on its chip, and every kept agent comes back', async (t) => {
  const many = [...'0123467890'.slice(0, 9), 'c'].map((digit) => `agt_${digit.repeat(32)}`);
  const snapshot = snapshotWith({ steward: true, aStatus: 'awaiting_permission' });
  snapshot.groups[0].members.push(...many.map((id, i) => ({ agent_id: id, conv_id: `c${i}`, title: `n${i}`, online: true, state: { status: 'idle', harness: 'claude' } })));
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([S, A, ...many]) });
  const { view, harness } = await setup(t, { prefs, snapshot });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  assert.equal(overlays.overlayWindows.value[0].tabs.length, 12);
  const chip = harness.getByRole(view.container, 'button', { name: /alpha: .*in the window of steward-k3x/ });
  assert.ok(chip.classList.contains('needs'));
  await harness.act(() => chip.click());
  assert.match(view.container.querySelector('.chat-window').getAttribute('aria-label'), /alpha/);
  assert.equal(JSON.parse(prefs.store.get('tclaude.dash.companion.tabs')).length, 12);
});

test('decisions join the window holding the Steward, wherever it went', async (t) => {
  const snapshot = snapshotWith({ steward: true });
  snapshot.messages = [{ id: 9, from_agent: B, subject: 'Which?', options: ['x', 'y'] }];
  const { view, harness, controller } = await setup(t, { snapshot });
  await harness.act(() => controller().float(S));
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  const window = overlays.overlayWindows.value.find((candidate) => candidate.tabs.some((tab) => tab.agent === S));
  assert.ok(window.tabs.some((tab) => tab.kind === 'decide'));
  assert.ok(view.container.querySelector('[data-tab-strip] [data-tab-id="companion-decide"]'));
});

test('a Steward out of the seat keeps its pill', async (t) => {
  const S2 = `agt_${'e'.repeat(32)}`;
  const snapshot = snapshotWith({ steward: true });
  snapshot.groups[1].members.push({ agent_id: S2, conv_id: 'conv-s2', title: 'steward-old', online: true, state: { status: 'idle' } });
  snapshot.steward_roster = [{ agent_id: S, name: 'steward-k3x', seat: 'active', online: true }, { agent_id: S2, name: 'steward-old', seat: 'standby', online: true }];
  const { harness, snap } = await setup(t, { snapshot });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(async () => { overlays.closeOverlayTab(`chat:${S}`); overlays.minimizeOverlayTab(`chat:${S2}`); });
  const { BoardOverlay } = await harness.importDashboardModule('js/board-overlay.js');
  const dock = await harness.mount(harness.html`<${BoardOverlay} snapshot=${snap} />`);
  const seated = (await harness.importDashboardModule('js/companion-model.js')).stewardAgent((await harness.importDashboardModule('js/companion-model.js')).companionAgents(snapshot)).agentId;
  assert.equal(dock.container.querySelectorAll('.overlay-dock-pill').length, seated === S2 ? 0 : 1);
});

test('a prompt behind the chip shows past a decision, and a shown tab stays out of it', async (t) => {
  const snapshot = snapshotWith({ steward: true, aStatus: 'awaiting_permission' });
  snapshot.messages = [{ id: 7, from_agent: S, subject: 'Later', options: ['x', 'y'] }];
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([S, A]) });
  const { view, harness, controller } = await setup(t, { prefs, snapshot });
  assert.ok(harness.getByRole(view.container, 'button', { name: /alpha: .*in the window of steward-k3x/ }), 'the Steward only asked');
  await harness.act(() => controller().open(A));
  assert.ok(harness.getByRole(view.container, 'button', { name: /^steward-k3x: / }), 'alpha is on screen, and the chip is the Steward again');
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(async () => {
    const stewards = overlays.overlayWindows.peek().find((window) => window.tabs.some((tab) => tab.agent === S));
    overlays.openTabInWindow(stewards.id, '', { id: 'term:a', kind: 'terminal', agent: A, paneKey: 'p', ws: '/x', label: 'shell' }, { activate: false });
  });
  assert.ok(harness.getByRole(view.container, 'button', { name: /^steward-k3x: / }), 'a shell of alpha, out of sight, says nothing about its chat');
});

test('a minimized shell of the Steward keeps its pill', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([S]) });
  const { harness, snap } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }) });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  const { BoardOverlay } = await harness.importDashboardModule('js/board-overlay.js');
  const dock = await harness.mount(harness.html`<${BoardOverlay} snapshot=${snap} />`);
  await harness.act(async () => {
    overlays.openOverlayWindow({ id: 'term:s', kind: 'terminal', agent: S, paneKey: 'p', ws: '/x', label: 'shell' });
    overlays.minimizeOverlayWindow('term:s');
  });
  assert.match(dock.container.querySelector('.overlay-dock-pill').textContent, /shell/);
});
