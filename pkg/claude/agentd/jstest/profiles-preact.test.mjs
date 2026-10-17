import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const capture = (id, extra = {}) => ({
  id,
  source: 'dashboard',
  status: 'done',
  seconds: 30,
  trace_seconds: 5,
  started_at: '2026-10-17T10:00:00Z',
  headline: { cpu_available: true, self_cpu_percent: 12.5, child_cpu_percent: 3.25, subprocesses_per_sec: 4 },
  files: [
    { name: 'cpu.pprof', kind: 'cpu', bytes: 100 },
    { name: 'heap.pprof', kind: 'heap', bytes: 100 },
    { name: 'mutex.pprof', kind: 'mutex', bytes: 0 },
    { name: 'trace.out', bytes: 100 },
    { name: 'summary.txt', bytes: 100 },
  ],
  ...extra,
});

const listing = (captures, running = '') => ({
  captures,
  running,
  max_seconds: 600,
  defaults: { seconds: 30, trace_seconds: 5 },
});

async function loadModules(harness) {
  const [{ createProfilesState }, { createProfilesActions, validateProfileDraft }, { ProfilesApp }] = await Promise.all([
    harness.importDashboardModule('js/profiles-state.js'),
    harness.importDashboardModule('js/profiles-actions.js'),
    harness.importDashboardModule('js/profiles-island.js'),
  ]);
  return { createProfilesState, createProfilesActions, validateProfileDraft, ProfilesApp };
}

test('Profile dialog defaults start a capture and validation errors stay in the dialog', async (t) => {
  const harness = await createPreactHarness(t);
  const { createProfilesState, createProfilesActions, validateProfileDraft } = await loadModules(harness);
  const state = createProfilesState({ activeTab: harness.signals.signal('activity'), activitySubtab: harness.signals.signal('performance') });
  const calls = [];
  const actions = createProfilesActions({
    state,
    fetchImpl: async (url, options = {}) => {
      calls.push({ url, options });
      if (options.method === 'POST') return { ok: true, json: async () => capture('20261017-100000-abcdef', { status: 'running' }) };
      return { ok: true, json: async () => listing([], '20261017-100000-abcdef') };
    },
  });
  assert.equal(await actions.load(), true);
  state.openModal();
  assert.deepEqual(
    { seconds: state.modal.value.seconds, trace: state.modal.value.trace_seconds },
    { seconds: '30', trace: '5' },
    'the dialog opens on the server defaults so Enter alone starts a sensible capture',
  );
  assert.equal(await actions.start(), true);
  const post = calls.find((call) => call.options.method === 'POST');
  assert.equal(post.url, '/api/profiles');
  assert.deepEqual(JSON.parse(post.options.body), { seconds: 30, trace_seconds: 5, label: '' });
  assert.equal(state.modal.value, null, 'a started capture closes the dialog');
  assert.equal(state.view.value.running, '20261017-100000-abcdef');

  assert.match(validateProfileDraft({ seconds: '0', trace_seconds: '5' }, 600).error, /between 1 and 600/);
  assert.match(validateProfileDraft({ seconds: '10', trace_seconds: '-1' }, 600).error, /whole number/);
  assert.deepEqual(validateProfileDraft({ seconds: '10', trace_seconds: '99', label: '  idle ' }, 600).body,
    { seconds: 10, trace_seconds: 10, label: 'idle' }, 'the trace never outlasts the CPU window');

  const busyState = createProfilesState({ activeTab: harness.signals.signal('activity') });
  const busy = createProfilesActions({
    state: busyState,
    fetchImpl: async () => ({ ok: false, status: 409, text: async () => 'a profile capture is already running\n' }),
  });
  busyState.openModal();
  assert.equal(await busy.start(), false);
  assert.equal(busyState.modal.value.error, 'a profile capture is already running');
  assert.equal(busyState.modal.value.submitting, false);
});

test('Profiles island lists captures with pprof viewer links and submits the dialog form', async (t) => {
  const harness = await createPreactHarness(t);
  const { createProfilesState, ProfilesApp } = await loadModules(harness);
  const state = createProfilesState({ activeTab: harness.signals.signal('activity'), activitySubtab: harness.signals.signal('performance') });
  const token = state.beginRequest();
  state.commitRequest(token, listing([capture('20261017-100000-abcdef', { label: 'idle fleet' })]));

  let starts = 0;
  const removed = [];
  const shared = [];
  const actions = {
    load: () => Promise.resolve(true),
    start: () => { starts++; return Promise.resolve(true); },
    remove: (row) => { removed.push(row.id); return Promise.resolve(true); },
    share: (row) => { shared.push(row.id); return Promise.resolve(true); },
    cancel() {},
  };
  const timers = [];
  const mounted = await harness.mount(harness.html`<${ProfilesApp} state=${state} actions=${actions}
    setTimeoutImpl=${(callback, delay) => { timers.push(delay); return timers.length; }} clearTimeoutImpl=${() => {}} />`);

  const row = mounted.container.querySelector('tr[data-key="profile-20261017-100000-abcdef"]');
  assert.ok(row);
  const hrefs = [...row.querySelectorAll('a')].map((link) => link.getAttribute('href'));
  assert.deepEqual(hrefs, [
    '/api/profiles/20261017-100000-abcdef/files/summary.txt',
    '/api/profiles/20261017-100000-abcdef/ui/cpu/',
    '/api/profiles/20261017-100000-abcdef/ui/heap/',
    '/api/profiles/20261017-100000-abcdef/files/trace.out?download=1',
    '/api/profiles/20261017-100000-abcdef/archive',
  ], 'empty profiles get no viewer link; the whole capture downloads as one zip');
  assert.equal(row.querySelector('a[href$="/ui/cpu/"]').getAttribute('target'), '_blank');
  assert.match(row.textContent, /13%.*3\.3%/, "agentd and child CPU render rounded");
  assert.match(row.textContent, /idle fleet/);

  await harness.act(() => harness.fireEvent(getByRole(row, 'button', { name: /Delete capture/ }), 'click'));
  assert.deepEqual(removed, ['20261017-100000-abcdef']);
  await harness.act(() => harness.fireEvent(getByRole(row, 'button', { name: /Share capture/ }), 'click'));
  assert.deepEqual(shared, ['20261017-100000-abcdef']);

  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: /Profile daemon/ }), 'click'));
  const form = mounted.container.querySelector('#profile-modal form');
  assert.ok(form, 'the button opens the capture dialog');
  assert.equal(mounted.container.querySelector('#profile-modal-seconds').value, '30');
  await harness.act(() => harness.fireEvent(form, 'submit'));
  assert.equal(starts, 1, 'submitting the form (Enter in any field) starts the capture');

  await mounted.unmount();
});

test('Sharing a capture picks an agent and posts it to the share endpoint', async (t) => {
  const harness = await createPreactHarness(t);
  const { createProfilesState, createProfilesActions } = await loadModules(harness);
  const state = createProfilesState({ activeTab: harness.signals.signal('activity') });
  const calls = [];
  const picks = [];
  const actions = createProfilesActions({
    state,
    pickAgent: async (options) => { picks.push(options); return picks.length === 1 ? 'agent-123' : ''; },
    fetchImpl: async (url, options = {}) => {
      calls.push({ url, options });
      return { ok: true, status: 202, json: async () => ({ title: 'perf-reader', files: 9, skipped: ['trace.out'] }) };
    },
  });
  const row = capture('20261017-100000-abcdef');
  assert.equal(await actions.share(row), true);
  assert.equal(picks[0].identity, 'agent');
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, '/api/profiles/20261017-100000-abcdef/share');
  assert.deepEqual(JSON.parse(calls[0].options.body), { to: 'agent-123' });
  assert.match(state.view.value.notice, /Sent 20261017-100000-abcdef to perf-reader as 9 attached files \(too large to attach: trace\.out\)/);

  assert.equal(await actions.share(row), false, 'dismissing the picker sends nothing');
  assert.equal(calls.length, 1);
});
