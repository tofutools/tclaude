import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const SNAPSHOT = {
  groups: [{ name: 'team', members: [{ agent_id: 'agt_rev', title: 'reviewer' }] }, { name: 'trial', members: [] }],
  harnesses: [{ name: 'claude', models: [] }, { name: 'codex', models: ['gpt-5.5'] }],
};

const LIBRARY = [
  { name: 'known-good', note: 'before the refactor', agent_name: 'reviewer', harness: 'claude', model: 'claude-opus-5-5', state: 'idle',
    created_at: new Date(Date.now() - 3600e3).toISOString(), bytes: 2_400_000, history: true, findings: [] },
  { name: 'flagged', agent_name: 'reviewer', harness: 'claude', created_at: new Date().toISOString(), bytes: 1000, history: true,
    findings: [{ kind: 'github_token', count: 1 }] },
];

async function mount(t, { descriptor = { kind: 'snapshots' }, overrides = {} } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  dashboardState.publishLocalEdit(SNAPSHOT);
  const calls = [];
  const snapshots = {
    list: async () => LIBRARY,
    policies: async () => [{ id: 'p1', target: 'reviewer', every_seconds: 3600, dest: { kind: 'peer', peer: 'inst_bob' }, peer_label: 'bob',
      agents: [{ agent: 'reviewer', last_good: new Date(Date.now() - 600e3).toISOString(), last_error: 'peer offline', failures: 2, next_due: new Date(Date.now() + 600e3).toISOString() }] }],
    create: async (body) => { calls.push(['create', body]); return { name: body.name || 'reviewer-x' }; },
    spawn: async (name, body) => { calls.push(['spawn', name, body]); return { spawned: [{ conv_id: 'c1' }] }; },
    remove: async (name) => { calls.push(['remove', name]); return {}; },
    prune: async () => ({ removed: [] }),
    exportBlob: async (name) => { calls.push(['export', name]); return { name }; },
    share: async (board, params) => {
      calls.push(['share', board, params]);
      if (params.snapshot === 'flagged' && !params.blob) {
        const e = new Error('suspected credentials'); e.status = 422; e.body = { code: 'flagged_credentials' }; throw e;
      }
      return { board, recipients: params.to, expires_at: new Date(Date.now() + 7 * 864e5).toISOString(),
        summary: { bytes: 2400, turns: 12, findings: [], redactions: [], content_digest: 'dg1' }, published: !!params.digest };
    },
    publish: async (blob, opts, digest) => {
      calls.push(['publish', opts, digest]);
      return digest ? { size: 1 } : { report: {}, summary: { bytes: 900, turns: 12, findings: [], redactions: [{ kind: 'strip_identity' }], content_digest: 'pub1' } };
    },
    ...overrides,
  };
  const actions = { snapshots, fleet: null, redactPAF: async (blob, opts, preview) => { calls.push(['redact', opts, preview]); return { redacted: true }; } };
  const fleet = {
    boards: async () => [{ id: 'brd_1', name: 'lab' }],
    boardMembers: async () => [{ instance: 'inst_bob', label: 'bob' }, { instance: 'inst_carol' }],
  };
  actions.fleet = fleet;
  const state = createManagementState();
  state.openDialog(descriptor);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  await harness.act(() => mountManagementIsland({
    host, state, actions,
    confirm: async () => true, confirmDiscard: async () => true, openProfilePermissions() {},
    registerCleanup: (fn) => cleanups.push(fn),
  }));
  t.after(() => { if (globalThis.document) cleanups.forEach((fn) => fn()); });
  // Two rounds: a fetch resolved in one round renders in the next.
  const settle = async () => {
    for (let i = 0; i < 2; i++) await harness.act(() => new Promise((resolve) => setTimeout(resolve, 20)));
  };
  const input = async (el, value, type = 'input') => { el.value = value; await harness.act(() => harness.fireEvent(el, type)); };
  const choose = async (select, value) => {
    [...select.options].forEach((option) => option.removeAttribute('selected'));
    [...select.options].find((option) => option.value === value).setAttribute('selected', '');
    await harness.act(() => harness.fireEvent(select, 'change'));
  };
  const tick = async (el, checked) => { el.checked = checked; await harness.act(() => harness.fireEvent(el, 'change')); };
  await settle();
  return { harness, host, calls, settle, input, choose, tick };
}

test('the Snapshots view lists the library and resilience status, and spawns from a snapshot', async (t) => {
  const { host, calls, settle, choose, input } = await mount(t);
  const rows = [...host.querySelectorAll('#snapshots-list tbody tr')];
  assert.deepEqual(rows.map((row) => row.dataset.snapshot), ['known-good', 'flagged']);
  assert.match(rows[0].textContent, /before the refactor[\s\S]*claude · claude-opus-5-5[\s\S]*2\.3 MB/);
  assert.match(rows[1].querySelector('.snap-acts').textContent, /⚠ 1/);
  assert.match(host.querySelector('#snapshot-policies').textContent, /reviewer[\s\S]*1h[\s\S]*bob[\s\S]*error ×2/);
  rows[0].querySelector('[data-snap="spawn"]').click();
  await settle();
  assert.equal(host.querySelector('#snapshot-spawn-submit').disabled, true, 'needs a group');
  await choose(host.querySelector('#snapshot-spawn-group'), 'trial');
  await input(host.querySelector('#snapshot-spawn-count'), '3');
  await choose(host.querySelector('#snapshot-spawn-harness'), 'codex');
  host.querySelector('#snapshot-spawn-submit').click();
  await settle();
  assert.deepEqual(calls.at(-1), ['spawn', 'known-good', { group: 'trial', count: 3, name: '', cwd: '', harness: 'codex', model: '' }]);
  assert.match(host.querySelector('#snapshots-notice').textContent, /Spawned 1 agent from known-good into trial/);
});

test('an agent ⚙ snapshot opens the create panel with the agent preset', async (t) => {
  const { host, calls, settle, input } = await mount(t, { descriptor: { kind: 'snapshots', agent: 'agt_rev' } });
  assert.ok(host.querySelector('#snapshot-create'), 'the create panel is open');
  await input(host.querySelector('#snapshot-create-name'), 'pre-merge');
  host.querySelector('#snapshot-create-submit').click();
  await settle();
  assert.deepEqual(calls.at(-1), ['create', { agent: 'agt_rev', name: 'pre-merge', note: '', expires: '', no_history: false }]);
});

test('sharing on a Board reviews first and needs the typed confirmation; a flagged snapshot shares a redacted copy', async (t) => {
  const { host, calls, settle, tick, input } = await mount(t);
  const flaggedRow = host.querySelector('[data-snapshot="flagged"]');
  flaggedRow.querySelector('[data-snap="share"]').click();
  await settle();
  await tick(host.querySelector('#snapshot-share-to [data-instance="inst_bob"]'), true);
  host.querySelector('#snapshot-share-review-btn').click();
  await settle();
  assert.ok(host.querySelector('#snapshot-share-flagged'), 'a flagged snapshot cannot go on a board as is');
  host.querySelector('#snapshot-share-redact').click();
  await settle();
  assert.deepEqual(calls.find((c) => c[0] === 'redact'), ['redact', { mask_flagged: true, paths_to_placeholders: true }, false]);
  assert.match(host.querySelector('#snapshot-share-review').textContent, /a redacted copy[\s\S]*12 turns[\s\S]*To: inst_bob/);
  const submit = host.querySelector('#snapshot-share-submit');
  assert.equal(submit.disabled, true, 'waits for the typed confirmation');
  await input(host.querySelector('#snapshot-share-confirm'), 'share');
  host.querySelector('#snapshot-share-submit').click();
  await settle();
  const published = calls.at(-1);
  assert.equal(published[0], 'share');
  assert.equal(published[2].digest, 'dg1');
  assert.deepEqual(published[2].blob, { redacted: true }, 'the redacted copy is what is shared');
  assert.deepEqual(published[2].to, ['inst_bob']);
});

test('publishing reviews the public copy and downloads it only with consent', async (t) => {
  const { harness, host, calls, settle, tick } = await mount(t);
  globalThis.URL.createObjectURL = () => 'blob:x';
  globalThis.URL.revokeObjectURL = () => {};
  harness.window.URL.createObjectURL = globalThis.URL.createObjectURL;
  host.querySelector('[data-snapshot="known-good"] [data-snap="publish"]').click();
  await settle();
  await tick(host.querySelector('#snapshot-publish-reasoning'), true);
  host.querySelector('#snapshot-publish-review-btn').click();
  await settle();
  assert.deepEqual(calls.at(-1), ['publish', { stub_tool_results: false, drop_media: false, drop_reasoning: true }, undefined]);
  assert.match(host.querySelector('#snapshot-publish-review').textContent, /strip identity/);
  assert.equal(host.querySelector('#snapshot-publish-submit').disabled, true);
  await tick(host.querySelector('#snapshot-publish-ack'), true);
  host.querySelector('#snapshot-publish-submit').click();
  await settle();
  assert.equal(calls.at(-1)[2], 'pub1', 'publishes exactly the reviewed digest');
});
