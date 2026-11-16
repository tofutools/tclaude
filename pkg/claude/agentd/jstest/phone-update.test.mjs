// js/phone-update.js: the phone's Update tclaude sheet, driven through what
// the operator sees and taps, over node-update.js's own status and job
// handling with a fake daemon: not configured, notify-only, ready, the
// confirm, a run (restarting, then back), the result, a failure, a refused
// start, and Back held while it runs. Also the ☰ More entry and its gate.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// fakeDaemon answers the update routes from `status`; start makes a job that
// next() moves along. down makes the status read fail (agentd restarting).
function fakeDaemon(status) {
  const daemon = { status, calls: [], down: false, refuse: null };
  daemon.actions = {
    fetchImpl: async () => ({ ok: true, json: async () => ({}) }),
    status: async () => { if (daemon.down) throw Object.assign(new Error('agentd is restarting'), { status: 0 }); return daemon.status; },
    start: async (node, action, version) => {
      daemon.calls.push([action, version || '']);
      if (daemon.refuse) throw Object.assign(new Error(daemon.refuse.message), daemon.refuse);
      const job = { id: `job-${daemon.calls.length}`, action, version: version || '', from_version: status.current_version, state: 'running', phase: 'downloading', warnings: [] };
      daemon.status = { ...daemon.status, job };
      return job;
    },
  };
  return daemon;
}

// manualTimers runs the update poll by hand: flush() runs what is due.
function manualTimers() {
  let due = [];
  return {
    setTimeout: (fn) => { due.push(fn); return due.length; },
    clearTimeout: () => {},
    flush() { const now = due; due = []; for (const fn of now) fn(); },
  };
}

const READY = { current_version: 'v1.4.0', latest_version: 'v1.5.0', update_available: true, chosen_method: 'release', warnings: [] };

async function setup(t, status = READY) {
  const harness = await createPreactHarness(t);
  const [{ UpdateSheet }, sheet] = await Promise.all([
    harness.importDashboardModule('js/phone-update.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  const daemon = fakeDaemon(status);
  const timers = manualTimers();
  const toasts = [];
  const navigated = [];
  const mounted = await harness.mount(harness.html`<${sheet.PhoneSheetHost} />`);
  await harness.act(() => sheet.openPhoneSheet({ label: 'Update tclaude', Body: UpdateSheet, props: {
    actions: daemon.actions, timers, toast: (text, error) => toasts.push([text, !!error]), openDesktop: () => navigated.push('/config/updates'),
    node: { id: 'self', label: 'this node', local: true },
  } }));
  const settle = async () => { for (let i = 0; i < 4; i += 1) await harness.act(async () => { await Promise.resolve(); }); };
  await settle();
  const c = mounted.container;
  const view = () => c.querySelector('.phone-update')?.dataset.update || null;
  const button = (label) => [...c.querySelectorAll('.phone-update button')].find((b) => b.textContent.trim().startsWith(label));
  const tap = async (el) => { await harness.act(() => el.click()); await settle(); };
  const poll = async () => { await harness.act(() => timers.flush()); await settle(); };
  return { harness, c, sheet, daemon, timers, toasts, navigated, view, button, tap, poll, settle };
}

test('ready: the version, the saved method and what is available; Update asks first, Cancel goes back', async (t) => {
  const { c, view, button, tap, daemon } = await setup(t);
  assert.equal(view(), 'ready');
  const facts = [...c.querySelectorAll('.phone-update-facts dd')].map((d) => d.textContent.trim());
  assert.deepEqual(facts, ['v1.4.0', 'Release download', 'v1.5.0 update available']);
  await tap(button('Update…'));
  assert.equal(view(), 'confirm');
  assert.equal(c.querySelector('.phone-update-title').textContent, 'Update and restart agentd?');
  assert.match(c.querySelector('.phone-update-body').textContent, /^This node downloads and verifies the official tclaude v1\.5\.0 release/, 'what the method does, from the desktop\'s own confirm');
  await tap(button('Cancel'));
  assert.equal(view(), 'ready');
  assert.deepEqual(daemon.calls, [], 'nothing ran');
});

test('a run: its phase, agentd restarting (not answering), then back on the new version', async (t) => {
  const { c, view, button, tap, poll, daemon } = await setup(t);
  await tap(button('Update…'));
  await tap(button('Update'));
  assert.deepEqual(daemon.calls, [['apply', '']], 'no version pinned: the saved method decides, as Update now does');
  assert.equal(view(), 'running');
  assert.match(c.querySelector('.phone-update-phase').textContent, /downloading/);
  // agentd goes down to restart: the reads fail and are retried.
  daemon.down = true;
  daemon.status = { ...daemon.status, job: { ...daemon.status.job, state: 'restarting', phase: 'restarting' } };
  await poll();
  assert.equal(view(), 'running');
  assert.match(c.querySelector('.phone-update-phase').textContent, /agentd is restarting/);
  // Back, on the new version.
  daemon.down = false;
  daemon.status = { ...daemon.status, current_version: 'v1.5.0', update_available: false, job: { ...daemon.status.job, version: 'v1.5.0', state: 'succeeded', phase: 'restarted', restart_required: true, output: 'installed' } };
  await poll();
  assert.equal(view(), 'done');
  assert.equal(c.querySelector('.phone-update-ok').textContent, '✓ Updated to v1.5.0. agentd is back.');
  assert.ok(button('Close'));
});

test('a failed run says so, with the job\'s error and output', async (t) => {
  const { c, view, button, tap, poll, daemon } = await setup(t);
  await tap(button('Update…'));
  await tap(button('Update'));
  daemon.status = { ...daemon.status, job: { ...daemon.status.job, state: 'failed', phase: 'verifying', error: 'checksum mismatch', output: 'sha256 differs' } };
  await poll();
  assert.equal(view(), 'failed');
  assert.match(c.querySelector('.phone-update-fail').textContent, /The update failed/);
  assert.match(c.querySelector('.phone-update-job').textContent, /checksum mismatch/);
  assert.match(c.querySelector('.phone-update-job pre').textContent, /sha256 differs/);
});

test('a refused start toasts why and shows no old job as its result', async (t) => {
  const old = { id: 'job-old', action: 'apply', state: 'succeeded', phase: 'restarted', version: 'v1.3.0', warnings: [] };
  const { view, button, tap, toasts, daemon } = await setup(t, { ...READY, job: old });
  assert.equal(view(), 'ready', 'an earlier job is history, not this sheet\'s');
  daemon.refuse = { message: 'already updating', code: 'update_busy', status: 409 };
  await tap(button('Update…'));
  await tap(button('Update'));
  assert.deepEqual(toasts.at(-1), ['this node is already updating', true]);
  assert.equal(view(), 'ready');
});

test('Back, a swipe or Escape is held while it runs, and says why; closes once done', async (t) => {
  const { harness, c, sheet, view, button, tap, poll, daemon, toasts } = await setup(t);
  await tap(button('Update…'));
  await harness.act(() => sheet.requestClosePhoneSheet());
  assert.equal(view(), 'ready', 'Back from the confirm goes back to the sheet');
  await tap(button('Update…'));
  await tap(button('Update'));
  await harness.act(() => sheet.requestClosePhoneSheet());
  assert.ok(c.querySelector('.phone-sheet'), 'held while it runs');
  assert.match(toasts.at(-1)[0], /The update is running/);
  daemon.status = { ...daemon.status, job: { ...daemon.status.job, state: 'succeeded', phase: 'complete' } };
  await poll();
  await harness.act(() => sheet.requestClosePhoneSheet());
  assert.equal(c.querySelector('.phone-sheet'), null, 'done: Back closes it');
});

test('a run that installs nothing new says already up to date', async (t) => {
  const { c, view, button, tap, poll, daemon } = await setup(t, { ...READY, latest_version: 'v1.4.0', update_available: false });
  await tap(button('Update…'));
  await tap(button('Update'));
  daemon.status = { ...daemon.status, job: { ...daemon.status.job, version: 'v1.4.0', state: 'succeeded', phase: 'complete', restart_required: false } };
  await poll();
  assert.equal(view(), 'done');
  assert.equal(c.querySelector('.phone-update-ok').textContent, '✓ Already up to date (v1.4.0): nothing changed.');
});

test('an install already running when the sheet opens: its progress, then its result stays', async (t) => {
  const running = { id: 'job-cron', action: 'apply', state: 'running', phase: 'building', from_version: 'v1.4.0', warnings: [] };
  const { c, view, poll, daemon } = await setup(t, { ...READY, job: running });
  assert.equal(view(), 'running');
  daemon.status = { ...daemon.status, current_version: 'v1.5.0', job: { ...running, version: 'v1.5.0', state: 'succeeded', phase: 'restarted', restart_required: true } };
  await poll();
  assert.equal(view(), 'done', 'not back to the start: the result shows');
  assert.match(c.querySelector('.phone-update-ok').textContent, /Updated to v1\.5\.0/);
});

test('a rollback running says so', async (t) => {
  const { c, view } = await setup(t, { ...READY, job: { id: 'job-rb', action: 'rollback', state: 'rolling_back', phase: 'rolling_back', warnings: [] } });
  assert.equal(view(), 'running');
  assert.match(c.querySelector('.phone-update-state').textContent, /Rolling back/);
});

test('Check now holds nothing: it is no install', async (t) => {
  const { harness, c, sheet, view, button, tap, daemon } = await setup(t);
  await tap(button('Check now'));
  assert.deepEqual(daemon.calls, [['check', '']]);
  assert.equal(view(), 'ready', 'a check is not shown as an update');
  await harness.act(() => sheet.requestClosePhoneSheet());
  assert.equal(c.querySelector('.phone-sheet'), null, 'Back closes during a check');
});

test('agentd never coming back does not trap the sheet: Close is offered once it stops answering', async (t) => {
  const { harness, c, sheet, view, button, tap, poll, daemon } = await setup(t);
  await tap(button('Update…'));
  await tap(button('Update'));
  daemon.down = true;
  await poll();
  assert.equal(view(), 'running');
  assert.match(c.querySelector('.phone-update').textContent, /You can close this: the update goes on at the node/);
  assert.ok(button('Close'));
  await harness.act(() => sheet.requestClosePhoneSheet());
  assert.equal(c.querySelector('.phone-sheet'), null, 'Back closes it now');
});

test('no method chosen, or one that only notifies: no run, a way to Config › Updates on the desktop', async (t) => {
  for (const [method, words] of [['', /No update method is chosen/], ['manual', /only notifies/]]) {
    await t.test(method || 'none', async (t2) => {
      const { c, view, button, tap, navigated, daemon } = await setup(t2, { ...READY, chosen_method: method });
      assert.equal(view(), 'unset');
      assert.match(c.querySelector('.phone-update-note').textContent, words);
      assert.equal(button('Update…'), undefined, 'nothing to run');
      await tap(button('Config › Updates on the desktop site'));
      assert.deepEqual(navigated, ['/config/updates']);
      assert.deepEqual(daemon.calls, []);
    });
  }
});

test('a node that refuses the read says why', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ UpdateSheet }, sheet] = await Promise.all([
    harness.importDashboardModule('js/phone-update.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  const actions = { status: async () => { throw Object.assign(new Error('forbidden'), { status: 403 }); }, start: async () => null };
  const mounted = await harness.mount(harness.html`<${sheet.PhoneSheetHost} />`);
  await harness.act(() => sheet.openPhoneSheet({ label: 'Update tclaude', Body: UpdateSheet, props: { actions, node: { id: 'p1', label: 'p1', local: true } } }));
  for (let i = 0; i < 4; i += 1) await harness.act(async () => { await Promise.resolve(); });
  assert.match(mounted.container.querySelector('.phone-update[data-update="error-read"] [role=alert]').textContent, /does not let you manage its updates \(needs node\.update\)/);
});

test('☰ More: Update tclaude opens the sheet; on a peer that does not share node.update it is off and says why', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ MoreMenu }, update, sheet] = await Promise.all([
    harness.importDashboardModule('js/phone-more.js'),
    harness.importDashboardModule('js/phone-update.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  const { signal } = harness.signals;
  const state = { snapshot: signal({}) };
  const actions = { tabAvailable: () => true, navigate: () => {}, showTab: () => {} };
  const mounted = await harness.mount(harness.html`<${MoreMenu} state=${state} actions=${actions} close=${() => {}} doctor=${{ counts: signal({}) }} />`);
  const entry = () => [...mounted.container.querySelectorAll('.phone-menu-item')].find((b) => b.querySelector('.phone-menu-label').textContent === 'Update tclaude');
  assert.equal(entry().disabled, false);
  await harness.act(() => entry().click());
  assert.equal(sheet.phoneSheet.value.label, 'Update tclaude');
  assert.equal(sheet.phoneSheet.value.Body, update.UpdateSheet);
  await harness.act(() => sheet.closePhoneSheet());
  // A peer's page: node.update omitted there.
  await harness.act(() => { state.snapshot.value = { peer_view: { level: 'scoped', included: ['groups'], omitted: [{ feature: 'node.update' }] } }; });
  assert.equal(entry().disabled, true);
  assert.equal(entry().querySelector('.phone-menu-note').textContent, 'needs node.update');
  assert.equal(update.canUpdateHere({ peer_view: { level: 'unrestricted' } }), true);
  assert.equal(update.canUpdateHere({ peer_view: { level: 'scoped', included: ['node.update'] } }), true);
  assert.equal(update.canUpdateHere({}), true, 'this node\'s own page');
});
