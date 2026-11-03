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
  settleClaim,
  memoryPrefs,
  fakeWidgetFactory,
  fakeDictation,
  SHOWN_WINDOW,
  setup,
} from './companion-helpers.mjs';

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
  assert.match(view.container.textContent, /1 companion need you/, 'a pending approval counts');
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
  assert.match(view.container.textContent, /1 companion need you/, 'a waiting decision counts');
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
  assert.ok(!view.container.querySelector(SHOWN_WINDOW));
  await harness.act(async () => { snap.value = { ...snap.value, companion_cues: [] }; });
  await harness.act(async () => {
    snap.value = { ...snap.value, companion_cues: [{ id: 3, agent_name: 'steward', open: B }] };
    await new Promise((r) => setTimeout(r, 0));
  });
  await settleClaim(harness);
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const tab = harness.getByRole(view.container, 'tab', { name: /bravo/ });
  assert.equal(tab.getAttribute('aria-selected'), 'true', 'bravo is pinned and shown');
  assert.ok(!harness.window.document.querySelector('.companion-guide-note'), 'an open-only cue draws nothing');
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
  assert.ok(!draft());
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
  assert.ok(!draft());
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
  assert.ok(!view.container.querySelector(SHOWN_WINDOW), 'the other tab has it');
  assert.equal(prefs.store.get('tclaude.dash.companion.cue-open'), undefined);

  const doc = harness.window.document;
  let visibility = 'hidden';
  Object.defineProperty(doc, 'visibilityState', { configurable: true, get: () => visibility });
  const heldCue = { id: 6, agent_name: 'steward', open: B, at: new Date().toISOString() };
  await settle([taken, heldCue]);
  held.add(`tclaude.companion.cue-open/6@${heldCue.at}`);
  visibility = 'visible';
  await harness.act(async () => { doc.dispatchEvent(new harness.window.Event('visibilitychange')); await new Promise((r) => setTimeout(r, 0)); });
  assert.ok(!view.container.querySelector(SHOWN_WINDOW), 'a held hand-over another tab took meanwhile is dropped');

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
  assert.ok(!view.container.querySelector(SHOWN_WINDOW), 'nothing opens while hidden');
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
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  const tabbed = (name) => overlays.overlayTabs.value.some((tab) => name.test(tab.label));
  const chip = (name) => harness.queryByRole?.(view.container, 'button', { name }) ?? [...view.container.querySelectorAll('.companion-chip')].find((b) => name.test(b.getAttribute('aria-label')));
  assert.ok(chip(/steward-k3x/), 'the Steward has a pill without being pinned');
  assert.ok(tabbed(/process-scribe-a1/), 'a scribe has a tab, whoever summoned it');
  assert.equal(tabbed(/bravo/), false, 'ordinary agents still wait to be pinned');
  assert.ok(!view.container.querySelector(SHOWN_WINDOW), 'a helper appearing does not open the window');

  // Two Stewards can be live (a changed permission set summons a fresh one).
  await harness.act(async () => { snap.value = withHelpers({ second: true }); });
  assert.ok(tabbed(/steward-new/), 'both Stewards are tracked');

  // Closing a helper sticks, through a poll that sees it offline and a reload.
  await harness.act(() => controller().open(P));
  await harness.act(async () => { overlays.closeOverlayTab(`chat:${P}`); });
  const hasTab = () => tabbed(/process-scribe-a1/);
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
  const { view } = await setup(t, { prefs, sessionPrefs });
  assert.ok(view.container.querySelector(SHOWN_WINDOW), 'it was open before the reload');
});

test('what a main build saved comes back as the home window, placement included', async (t) => {
  const placement = { x: 40, y: 50, w: 640, h: 420, snap: '' };
  const prefs = memoryPrefs({
    'tclaude.dash.companion.tabs': JSON.stringify([S, A]), 'tclaude.dash.companion.active': A,
    'tclaude.dash.companion.placement': JSON.stringify(placement),
    'tclaude.dash.companion.voice': JSON.stringify([A]),
    'tclaude.dash.companion.chip-spot': 'top-left',
  });
  const sessionPrefs = memoryPrefs({ 'tclaude.dash.companion.open': '1', 'tclaude.dash.companion.active': A });
  const speech = { supported: true, speak() {}, cancel() {} };
  const { view, harness } = await setup(t, { prefs, sessionPrefs, speakerImpl: speech, snapshot: snapshotWith({ steward: true }) });
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const [window] = overlays.overlayWindows.value;
  assert.equal(overlays.overlayWindows.value.length, 1);
  assert.equal(window.place, 'tclaude.dash.companion.placement');
  assert.deepEqual(window.tabs.map((tab) => tab.agent), [S, A]);
  assert.equal(window.active, `chat:${A}`);
  assert.equal(window.minimized, false);
  const frame = view.container.querySelector('.chat-window');
  assert.deepEqual([frame.style.left, frame.style.top, frame.style.width, frame.style.height], ['40px', '50px', '640px', '420px']);
  assert.equal(frame.querySelector('.companion-voice').getAttribute('aria-pressed'), 'true', 'voice stays on');
  assert.equal(sessionPrefs.store.get('tclaude.dash.companion.seeded'), '1');
});

test('a dashboard tab new to the agents kept at hand has them minimized, once', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]) });
  const sessionPrefs = memoryPrefs();
  const first = await setup(t, { prefs, sessionPrefs });
  const { view, harness } = first;
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  assert.ok(!view.container.querySelector(SHOWN_WINDOW));
  assert.equal(overlays.overlayWindows.value[0].tabs.length, 2);
  await setup(t, { prefs, sessionPrefs, reloading: first });
  assert.deepEqual(overlays.overlayTabs.value, [], 'a reload leaves that to the saved windows');
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

test('with stewards opted out the companion keeps its windows but has no Steward chips', async (t) => {
  const harness = await createPreactHarness(t);
  const { CompanionGate } = await harness.importDashboardModule('js/companion-island.js');
  const snap = harness.signals.signal({ ...snapshotWith(), stewards_disabled: true });
  const activeTab = harness.signals.signal('groups');
  const controllers = [];
  const view = await harness.mount(harness.html`<${CompanionGate} snapshot=${snap} activeTab=${activeTab} prefs=${memoryPrefs()} sessionPrefs=${memoryPrefs()}
    fetchImpl=${async () => ({ ok: true, status: 200, json: async () => ({}) })} widgetFactory=${fakeWidgetFactory([])} closeTerminalPanes=${() => {}}
    registerController=${(c) => { controllers.push(c); return () => {}; }} />`);
  assert.ok(!view.container.querySelector('.companion-chips'));
  assert.equal(controllers.length, 1, 'still the overlay host: boards, pages and terminals can float');
  await harness.act(async () => { snap.value = snapshotWith(); });
  assert.ok(view.container.querySelector('.companion-chips'), 'turning it on needs no reload');
  await harness.act(async () => { snap.value = { ...snapshotWith(), stewards_disabled: true }; });
  assert.ok(!view.container.querySelector('.companion-chips'));
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
  const snap = harness.signals.signal({ ...snapshotWith(), peer_view: { level: 'restricted' } });
  const view = await harness.mount(harness.html`<${CompanionGate} snapshot=${snap} activeTab=${harness.signals.signal('groups')} prefs=${memoryPrefs()} sessionPrefs=${memoryPrefs()}
    fetchImpl=${fetchImpl} widgetFactory=${fakeWidgetFactory([])} closeTerminalPanes=${() => {}} />`);
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); });
  assert.ok(!view.container.querySelector('.companion-chips'), 'no window on a peer page');
  assert.ok(urls.includes('/api/companion/presence'), 'the tab still reports itself');
  assert.ok(urls.includes('/api/companion/view'), 'and which view it is on, so guide view is not left on a local one');
});

test('a permission request opens in a right-hand panel from its pill, and the panel closes when answered', async (t) => {
  const prefs = memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([S]) });
  const deadline = new Date(Date.now() + 120000).toISOString();
  const snapshot = {
    ...snapshotWith({ steward: true }),
    access_requests: [
      { id: 'r2', perm: 'agent.spawn', agent_id: B, conv_title: 'bravo', status: 'pending', deadline: new Date(Date.now() + 200000).toISOString() },
      { id: 'r1', perm: 'sudo.groups.create', agent_id: S, status: 'pending', deadline },
      { id: 'r3', perm: 'x', agent_id: S, status: 'approved' },
      { id: 'r4', perm: 'y', agent_id: S, status: 'pending', proposal: {} },
      { id: 'r5', perm: 'y', agent_id: B, status: 'pending', origin_peer: 'node-2' },
    ],
  };
  const { view, harness, snap, requests } = await setup(t, { prefs, snapshot });
  const { AccessPanelGate } = await harness.importDashboardModule('js/access-panel-island.js');
  const panelView = await harness.mount(harness.html`<${AccessPanelGate} snapshot=${snap} fetchImpl=${async (url, init) => { requests.push({ url: String(url), body: JSON.parse(init.body) }); return { ok: true, status: 200 }; }} notify=${() => {}} />`);
  const panelHost = panelView.container;
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(panelHost.querySelector('.access-panel'), null, 'closed until asked for');

  const chip = harness.getByRole(view.container, 'button', { name: /steward-k3x: / });
  assert.match(chip.textContent, /approval/);
  await harness.act(() => chip.click());
  assert.ok(!view.container.querySelector(SHOWN_WINDOW), 'the window stays minimized');
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
  const snapshot = { access_requests: [{ id: 'r', perm: 'x', agent_id: A, status: 'pending' }, { id: 'c', perm: 'y', conv_id: 'conv-b', status: 'pending' }] };
  assert.equal(paneApprovals({ seed: { agent: A } }, snapshot).length, 1);
  assert.equal(paneApprovals({ seed: { agent: 'conv-b' } }, snapshot).length, 1, 'a pane seeded with a conv id');
  assert.equal(paneApprovals({ seed: { agent: 'conv-a' } }, { access_requests: [{ id: 'r', perm: 'x', agent_id: A, conv_id: 'conv-a', status: 'pending' }] }).length, 1, 'a conv-seeded pane finds a request naming its agent');
  assert.equal(paneApprovals({ seed: { agent: B } }, snapshot).length, 0);
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
  assert.ok(!view.container.querySelector('form.companion-input'), 'no message row while deciding');
  await harness.act(async () => { harness.getByRole(cards[1], 'button', { name: 'Sidebar' }).click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(requests.find((r) => r.url === '/api/human-messages/reply').body, { id: 11, choice: 2, body: '' });
  // Once everything is answered the queue ends, and a later decision only
  // brings the pill back instead of covering the window.
  await harness.act(async () => { snap.value = { ...snap.value, messages: [] }; });
  await harness.act(async () => { snap.value = { ...snap.value, messages: [{ id: 13, from_agent: C, subject: 'Again?', options: ['x', 'y'] }] }; });
  assert.ok(!view.container.querySelector('.companion-decide-queue'));
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
  assert.ok(!view.container.querySelector('svg.annotate-canvas'), 'the layer closes after sending');
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
