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
  setup,
  FLOATING,
  openLook,
  buttonNames,
  openWithMic,
  micCounting,
  CONTINUOUS_SNAPSHOT,
} from './companion-helpers.mjs';

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
  assert.ok(!view.container.querySelector('.companion-menu'));
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
  await press(view.container.querySelector('.board-overlay-titlebar'), 0);
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

test('each look puts the window controls in native order, and keyboard order follows the eye', async (t) => {
  const names = ['Minimize (Ctrl .)', 'Maximize', 'Close alpha'];
  for (const [look, order] of [['macos', [names[2], names[0], names[1]]], ['windows', names], ['linux', names]]) {
    const { view } = await openLook(t, look);
    const bar = view.container.querySelector('.board-overlay-titlebar');
    const group = bar.querySelector('.overlay-window-controls');
    assert.deepEqual(buttonNames(group), order, look);
    assert.ok(group.querySelectorAll('svg').length === 3, `${look} draws its symbols`);
    const all = [...bar.querySelectorAll('button')];
    const tab = bar.querySelector('[role="tab"]');
    assert.equal(all.indexOf(group.querySelector('button')) < all.indexOf(tab), look === 'macos', `${look} tab order`);
    assert.equal(bar.querySelector('.companion-controls [aria-label^="Minimize"]'), null, 'no second minimize');
    view.unmount?.();
  }
});

test('the classic look has its one minimize', async (t) => {
  const { view, harness } = await openLook(t, 'classic');
  const bar = view.container.querySelector('.board-overlay-titlebar');
  assert.equal(bar.querySelector('.overlay-window-controls'), null);
  const minimize = bar.querySelector('.board-overlay-close[aria-label="Minimize alpha"]');
  assert.equal(minimize.textContent, '–');
  assert.equal(bar.querySelector('[aria-label="Maximize"]'), null);
  await harness.act(async () => { minimize.click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(view.container.querySelector('.companion-window-warm'), 'minimized');
});

test('an unknown or missing look is the standard one, and the OS looks are opt-in', async (t) => {
  const harness = await createPreactHarness(t);
  const { windowControlsLook } = await harness.importDashboardModule('js/overlay-window-controls.js');
  assert.equal(windowControlsLook('auto', 'macos'), 'standard');
  assert.equal(windowControlsLook(undefined, 'windows'), 'standard');
  assert.equal(windowControlsLook('beos', 'linux'), 'standard');
  assert.equal(windowControlsLook('os', 'macos'), 'macos', 'Match my OS follows the browser');
  assert.equal(windowControlsLook('classic', 'macos'), 'classic');
  assert.equal(windowControlsLook('linux', 'macos'), 'linux');
});

test('the standard look is − ⇄ × on the far right, and ⇄ opens the window in the Collab tab', async (t) => {
  const opened = [];
  const store = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]), 'tclaude.dash.companion.active': A });
  const { view, harness, controller } = await setup(t, { prefs: store, snapshot: { ...snapshotWith(), window_controls: 'auto' }, openTerminalsPane: (...args) => opened.push(args) });
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const bar = view.container.querySelector('.board-overlay-titlebar');
  assert.ok(bar.classList.contains('overlay-look-standard'));
  const group = bar.querySelector('.overlay-window-controls');
  assert.equal(bar.lastElementChild, group, 'on the far right');
  assert.deepEqual([...group.querySelectorAll('button')].map((button) => button.className), ['overlay-wc-minimize', 'overlay-wc-collab', 'overlay-wc-close'], '− ⇄ ×');
  assert.ok([...group.querySelectorAll('button')].every((button) => button.querySelector('svg path')), 'each glyph drawn in its box');
  assert.deepEqual(buttonNames(group), ['Minimize (Ctrl .)', 'Open in the Collab tab', 'Close alpha']);
  for (const button of group.querySelectorAll('button')) assert.ok(button.title, `${button.className} has a tooltip`);
  await harness.act(async () => { group.querySelector('.overlay-wc-collab').click(); await new Promise((r) => setTimeout(r, 80)); });
  assert.deepEqual(opened.map(([agent]) => agent), [A], 'the cog item\'s move');
  assert.ok(!view.container.querySelector('.companion-tab[data-tab-id="chat:' + A + '"]'), 'and it left the window');
});

test('the standard look\'s middle button maximizes where the window cannot go to Collab', async (t) => {
  const harness = await createPreactHarness(t);
  const { WindowControls } = await harness.importDashboardModule('js/overlay-window-controls.js');
  let sized = 0;
  const view = await harness.mount(harness.html`<${WindowControls} look="standard" maximized=${false} closeLabel="Close x" closeTitle="Close" onMinimize=${() => {}} onMaximize=${() => { sized++; }} onClose=${() => {}} />`);
  assert.deepEqual(buttonNames(view.container), ['Minimize (Ctrl .)', 'Maximize', 'Close x'], 'always three buttons');
  await harness.act(() => view.container.querySelector('.overlay-wc-maximize').click());
  assert.equal(sized, 1);
});

test('a double click on the title bar\'s empty space maximizes and restores, in the standard look too', async (t) => {
  const { view, harness } = await openLook(t, 'auto');
  const section = () => view.container.querySelector('.chat-window');
  const bar = () => view.container.querySelector('.board-overlay-titlebar');
  const dbl = (target) => harness.act(() => target.dispatchEvent(new harness.window.Event('dblclick', { bubbles: true })));
  await dbl(bar());
  assert.match(section().className, /snapped-max/);
  await dbl(bar().querySelector('[role="tab"]'));
  assert.match(section().className, /snapped-max/, 'not on a tab');
  await dbl(bar());
  assert.doesNotMatch(section().className, /snapped-max/);
});

test('minimize and close are their own actions, and close leaves the agent running', async (t) => {
  const { view, harness, requests } = await openLook(t, 'windows');
  const bar = () => view.container.querySelector('.board-overlay-titlebar');
  await harness.act(() => bar().querySelector('.companion-tab-minimize').click());
  assert.deepEqual([...bar().querySelectorAll('[role="tab"]')].map((tab) => tab.title.split(':')[0]), ['bravo'], 'the tab went to the dock');
  assert.ok(!view.container.querySelector('.companion-window-warm'), 'the window stays for the other tab');
  await harness.act(async () => { bar().querySelector('.overlay-wc-minimize').click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(view.container.querySelector('.companion-window-warm'), 'minimized');
  assert.equal(view.container.querySelectorAll('[role="tab"]').length, 1, 'and minimize closed no tab');
  assert.deepEqual(requests.filter((request) => request.url.includes(A)), [], 'nothing was asked of the agent');
});

test('maximize fills the room, survives a reload, and restores what it replaced', async (t) => {
  const pref = 'tclaude.dash.companion.placement';
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]), [pref]: JSON.stringify(FLOATING) });
  const first = await openLook(t, 'linux', { prefs });
  const { view, harness } = first;
  const frame = () => view.container.querySelector('.companion-window');
  const size = () => view.container.querySelector('.overlay-wc-maximize');
  await harness.act(() => size().click());
  assert.ok(frame().classList.contains('snapped-max'));
  assert.equal(size().getAttribute('aria-label'), 'Restore');
  assert.ok(!view.container.querySelector('.overlay-window-edge'), 'a maximized window has no free edge to pull');
  assert.ok(!view.container.querySelector('.companion-resize-frame'));
  assert.deepEqual(JSON.parse(prefs.getItem(pref)), { ...FLOATING, snap: 'max' }, 'the floating place is kept to come back to');
  await harness.act(() => size().click());
  assert.equal(frame().classList.contains('snapped'), false);
  assert.equal(size().getAttribute('aria-label'), 'Maximize');
  assert.deepEqual(JSON.parse(prefs.getItem(pref)), FLOATING);
  prefs.setItem(pref, JSON.stringify({ ...FLOATING, snap: 'left' }));
  const again = await openLook(t, 'linux', { prefs, reloading: first });
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
  const reloaded = await openLook(t, 'linux', { prefs, reloading: first });
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
  const bar = view.container.querySelector('.board-overlay-titlebar');
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
  assert.ok(!view.container.querySelector('.companion-window .window-resize-zone'), 'outside the window, whose clip would cut them');
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
  assert.ok(!view.container.querySelector('form.companion-input'), 'no bar at rest');
  const order = [...view.container.querySelectorAll('.companion-floating, .companion-term-host')];
  assert.equal(order[0], tools, 'reachable by Tab before the terminal');

  await harness.act(() => view.container.querySelector('.companion-mic').click());
  assert.ok(view.container.querySelector('form.companion-input.listening input.companion-draft'));
  assert.ok(view.container.querySelector('form.companion-input button[type="submit"]'));
  assert.ok(tools.classList.contains('listening'), 'never dimmed while the mic is open');
});

test('a browser without speech recognition gets a disabled mic whose tooltip says so', async (t) => {
  const { mic, note, container } = await openWithMic(t, { supported: false, secure: true });
  assert.equal(mic.disabled, true);
  assert.match(mic.title, /no speech recognition/);
  assert.equal(note, null);
  assert.ok(!container.querySelector('.companion-context'), 'view only serves dictation');
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
  assert.ok(body.classList.contains('has-cards'));
  await harness.act(() => view.container.querySelector('.companion-mic').click());
  assert.ok(view.container.querySelector('.companion-approval'));
  assert.ok(view.container.querySelector('.companion-body > .companion-input'), 'the field joins the cards, whose padding clears it');
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
  assert.ok(!view.container.querySelector('.companion-menu'));
  assert.equal(harness.document.activeElement, plus);
});

test('the Steward phrase opens its window and only then dictates there', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A]) });
  const dictated = micCounting();
  const { view, harness, controller } = await setup(t, { prefs, snapshot: snapshotWith({ steward: true }), dictationImpl: dictated.impl });
  await harness.act(() => controller().open(A));
  dictated.reservations.length = 0;
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
  assert.equal(dictated.reservations[0], true, 'the door holds it until the Steward has it');
  assert.equal(dictated.reservations.at(-1), false);
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
  await harness.act(() => controller().open(B));
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
  assert.ok(!view.container.querySelector('.companion-state-roster'));
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
