// Tests for Config › Service › Launch profile (config-service-launch.js):
// the form's rows, "Use what's running now", Save with the version, a
// refused row, a conflict that keeps the edits, Clear, the comparison by its
// baseline, and the autostart install deferring to a set profile.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const URL = '/api/node/service/launch';

const SUGGESTED = {
  flags: [{ arg: '--no-tray', name: '--no-tray', secret: false }, { arg: '--api-token=<redacted>', name: '--api-token', secret: true }],
  env: [
    { key: 'ANTHROPIC_API_KEY', value: '<redacted>', secret: true, mode: 'inherit' },
    { key: 'LANG', value: 'C.UTF-8', secret: false, mode: 'set' },
    { key: 'PATH', value: '/usr/bin', secret: false, mode: 'inherit' },
  ],
};

const UNSET = {
  profile: null, suggested: SUGGESTED, missing: [], owned: true, version: 'v1', access: 'full',
  launcher: { kind: 'systemd-user', unit: 'tclaude-agentd.service', managed_by_tclaude: true },
  comparison: { state: 'unconfigured', baseline: 'none', differences: [] },
};

const SET_VIEW = {
  ...UNSET,
  profile: { flags: ['--no-tray', '--dashboard-port=8123'], env: [{ key: 'LANG', value: 'C.UTF-8' }], inherit_env: ['PATH', 'ANTHROPIC_API_KEY'] },
  version: 'v2',
  comparison: {
    state: 'differs', baseline: 'profile', differences: [
      { kind: 'flag', change: 'added', name: '--dashboard-port', after: '8123' },
      { kind: 'env', change: 'changed', name: 'LANG', before: 'en_US.UTF-8', after: 'C.UTF-8' },
      { kind: 'env', change: 'changed', name: 'ANTHROPIC_API_KEY' },
    ],
  },
};

function daemon(initial, { put = null } = {}) {
  const calls = [];
  let state = structuredClone(initial);
  const reply = (status, body) => ({ ok: status < 400, status, json: async () => structuredClone(body) });
  const fetchImpl = async (path, options = {}) => {
    const method = options.method || 'GET';
    const body = options.body ? JSON.parse(options.body) : null;
    calls.push({ path, method, body });
    if (path !== URL) return reply(404, {});
    if (method === 'GET') return reply(200, state);
    if (put) {
      const out = put(body, state);
      if (out) return reply(out.status, out.body);
    }
    state = { ...state, profile: body.profile, version: `${state.version}+` };
    return reply(200, state);
  };
  return { calls, fetchImpl, set: (next) => { state = structuredClone(next); } };
}

async function setup(t, initial, opts = {}) {
  const harness = await createPreactHarness(t);
  const { LaunchProfile } = await harness.importDashboardModule('js/config-service-launch.js');
  const fake = daemon(initial, opts);
  const toasts = [];
  const confirms = [];
  const mounted = await harness.mount(harness.html`<${LaunchProfile} fetchImpl=${fake.fetchImpl}
    confirm=${async (o) => { confirms.push(o); return true; }} toast=${(m) => toasts.push(m)} />`);
  const root = mounted.container;
  const settle = () => harness.act(async () => { await new Promise((r) => setTimeout(r, 5)); });
  const until = async (check, what) => {
    const deadline = Date.now() + 5000;
    while (!check() && Date.now() < deadline) await settle();
    assert.ok(check(), `timed out waiting for ${what}`);
  };
  await until(() => root.querySelector('#cfg-launch'), 'the first read');
  const click = async (sel) => { await harness.act(async () => { harness.fireEvent(root.querySelector(sel), 'click'); }); await settle(); };
  const type = async (el, value) => { el.value = value; await harness.act(async () => { harness.fireEvent(el, 'input'); }); };
  const puts = () => fake.calls.filter((c) => c.method === 'PUT');
  return { ...fake, harness, root, toasts, confirms, until, click, type, settle, puts, unmount: mounted.unmount };
}

test('the pure helpers: rows, suggested, the PUT profile and its error paths', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/config-service-launch.js');
  const draft = m.profileDraft(SET_VIEW.profile);
  assert.deepEqual(draft.env.map((r) => `${r.key}:${r.mode}`), ['LANG:set', 'PATH:inherit', 'ANTHROPIC_API_KEY:inherit']);
  const { draft: fromRunning, dropped } = m.draftFromSuggested(SUGGESTED);
  assert.deepEqual(fromRunning.flags, ['--no-tray'], 'a secret flag is left out');
  assert.deepEqual(dropped, ['--api-token']);
  assert.deepEqual(fromRunning.env.map((r) => `${r.key}:${r.mode}:${r.value}`), ['ANTHROPIC_API_KEY:inherit:', 'LANG:set:C.UTF-8', 'PATH:inherit:']);
  // Empty rows are left out, and each sent entry maps back to its row.
  const { profile, paths } = m.profileOf({ flags: ['', '--no-tray'], env: [{ key: 'A', mode: 'set', value: '1' }, { key: '', mode: 'set', value: '' }, { key: 'B', mode: 'inherit', value: 'x' }] });
  assert.deepEqual(profile, { flags: ['--no-tray'], env: [{ key: 'A', value: '1' }], inherit_env: ['B'] });
  assert.equal(paths.get('profile.flags[0]'), 'f1');
  assert.equal(paths.get('profile.env[0].value'), 'e0');
  assert.equal(paths.get('profile.inherit_env[0]'), 'e2');
  const errs = m.rowErrors([{ path: 'profile.flags[0]', message: 'not a serve flag' }, { path: 'profile', message: 'too big' }], paths);
  assert.deepEqual(errs, { rows: { f1: 'not a serve flag' }, general: ['too big'] });
  // A value shows only for a non-secret key, never redacted, never to a restricted viewer.
  const secretKeys = new Set(['ANTHROPIC_API_KEY']);
  assert.equal(m.shownValue({ kind: 'env', name: 'LANG' }, 'C.UTF-8', { secretKeys, restricted: false }), 'C.UTF-8');
  assert.equal(m.shownValue({ kind: 'env', name: 'LANG' }, 'C.UTF-8', { secretKeys, restricted: true }), null);
  assert.equal(m.shownValue({ kind: 'env', name: 'ANTHROPIC_API_KEY' }, 'sk-x', { secretKeys, restricted: false }), null);
  assert.equal(m.shownValue({ kind: 'flag', name: '--x' }, '<redacted>', { secretKeys, restricted: false }), null);
  assert.equal(m.comparisonTag({ state: 'unavailable', baseline: 'profile' }).text, 'cannot compare now');
  assert.deepEqual([m.comparisonTag({ state: 'not_applied', baseline: 'profile' })].map((x) => [x.text, x.baseline]), [['started by hand', 'the profile applies to the unit started at boot']]);
  assert.equal(m.comparisonTag({ state: 'unconfigured', baseline: 'none' }).baseline, '');
});

test("Use what's running now fills the rows, leaves a secret flag out with a note, and Save sends the version", async (t) => {
  const { root, click, puts, toasts, until, unmount } = await setup(t, UNSET);
  try {
    assert.match(root.querySelector('#cfg-launch-state').textContent, /not set/);
    assert.equal(root.querySelector('#cfg-launch-clear'), null, 'nothing to clear');
    await click('#cfg-launch-use-running');
    assert.deepEqual([...root.querySelectorAll('.cfg-launch-flag')].map((el) => el.value), ['--no-tray']);
    assert.match(root.querySelector('#cfg-launch-dropped').textContent, /--api-token.*inherited environment variable/);
    const values = [...root.querySelectorAll('.cfg-launch-value')];
    assert.equal(values[0].disabled, true, 'a secret is inherited');
    assert.match(values[0].getAttribute('placeholder'), /secret/);
    assert.ok(root.querySelector('#cfg-launch-dirty'), 'unsaved changes');
    await click('#cfg-launch-save');
    await until(() => toasts.includes('Launch profile saved'), 'the save');
    assert.deepEqual(puts()[0].body, {
      profile: { flags: ['--no-tray'], env: [{ key: 'LANG', value: 'C.UTF-8' }], inherit_env: ['ANTHROPIC_API_KEY', 'PATH'] },
      version: 'v1',
    });
    assert.equal(root.querySelector('#cfg-launch-dirty'), null);
    assert.ok(root.querySelector('#cfg-launch-clear'));
  } finally {
    await unmount();
  }
});

test('a refused row is marked by its path', async (t) => {
  const put = (body) => (body.profile?.flags?.includes('--tui')
    ? { status: 400, body: { error: 'refused', errors: [{ path: 'profile.flags[2]', code: 'invalid_flag', message: '--tui is not a serve flag' }] } }
    : null);
  const { harness, root, click, type, until, unmount } = await setup(t, SET_VIEW, { put });
  try {
    await click('#cfg-launch-add-flag');
    await type(root.querySelectorAll('.cfg-launch-flag')[2], '--tui');
    await click('#cfg-launch-add-env');
    const keys = root.querySelectorAll('.cfg-launch-key');
    const last = keys.length - 1;
    await type(keys[last], 'HTTPS_PROXY');
    const mode = root.querySelectorAll('.cfg-launch-mode')[last];
    for (const o of mode.querySelectorAll('option')) {
      const on = o.getAttribute('value') === 'inherit';
      o.selected = on;
      if (on) o.setAttribute('selected', ''); else o.removeAttribute('selected');
    }
    await harness.act(async () => { harness.fireEvent(mode, 'change'); });
    assert.equal(root.querySelector('[data-launch-missing="HTTPS_PROXY"]'), null, 'a name just typed waits for the daemon\'s word');
    await click('#cfg-launch-save');
    await until(() => root.querySelector('[data-launch-row="f2"] .cfg-launch-err'), 'the row error');
    assert.match(root.querySelector('[data-launch-row="f2"] .cfg-launch-err').textContent, /--tui is not a serve flag/);
    assert.ok(root.querySelector('[data-launch-row="f2"] .cfg-launch-flag').className.includes('is-invalid'));
    assert.ok(root.querySelector('#cfg-launch-dirty'), 'the edits stay');
  } finally {
    await unmount();
  }
});

test('a stale version keeps the edits over the current view; Clear asks and sends null', async (t) => {
  let stale = true;
  const put = (body, state) => {
    if (stale && body.version === 'v2') { stale = false; return { status: 409, body: { error: 'changed', code: 'launch_profile_conflict', current: { ...state, version: 'v3' } } }; }
    return null;
  };
  const { root, click, type, puts, confirms, until, unmount } = await setup(t, SET_VIEW, { put });
  try {
    await type(root.querySelectorAll('.cfg-launch-flag')[1], '--dashboard-port=9000');
    await click('#cfg-launch-save');
    await until(() => root.querySelector('#cfg-launch-note'), 'the conflict note');
    assert.match(root.querySelector('#cfg-launch-note').textContent, /changed elsewhere/);
    assert.equal(root.querySelectorAll('.cfg-launch-flag')[1].value, '--dashboard-port=9000', 'the edit is kept');
    await click('#cfg-launch-save');
    await until(() => puts().length === 2 && !root.querySelector('#cfg-launch-dirty'), 'the second save');
    assert.equal(puts()[1].body.version, 'v3', 'saved over the version it was shown');
    await click('#cfg-launch-clear');
    await until(() => puts().length === 3, 'the clear');
    assert.equal(confirms[0].title, 'Clear the launch profile?');
    assert.deepEqual(puts()[2].body.profile, null);
  } finally {
    await unmount();
  }
});

test('the comparison says its baseline; values only for keys that are not secret; a unit tclaude did not write; read access', async (t) => {
  const { root, unmount } = await setup(t, SET_VIEW);
  try {
    assert.equal(root.querySelector('#cfg-launch-cmp').textContent, 'differs · 3');
    assert.equal(root.querySelector('#cfg-launch-baseline').textContent, 'compared with the profile');
    const row = (key) => [...root.querySelector(`[data-launch-diff="${key}"]`).querySelectorAll('td')].map((td) => td.textContent);
    assert.deepEqual(row('env:LANG'), ['env', 'changed', 'LANG', 'en_US.UTF-8', 'C.UTF-8']);
    assert.deepEqual(row('env:ANTHROPIC_API_KEY'), ['env', 'changed', 'ANTHROPIC_API_KEY', '—', '—']);
    assert.deepEqual(row('flag:--dashboard-port'), ['flag', 'added', '--dashboard-port', '—', '8123']);
  } finally {
    await unmount();
  }
  const unowned = await setup(t, { ...UNSET, owned: false, comparison: { state: 'identical', baseline: 'unit', differences: [] } });
  try {
    assert.equal(unowned.root.querySelector('#cfg-launch-baseline').textContent, 'compared with the unit');
    assert.match(unowned.root.querySelector('#cfg-launch-unowned').textContent, /tclaude-agentd\.service was not written by tclaude.*the unit's own command/);
  } finally {
    await unowned.unmount();
  }
  const peer = await setup(t, { ...SET_VIEW, access: 'read', redacted: ['profile.env', 'suggested.env'], profile: { ...SET_VIEW.profile, env: [{ key: 'LANG' }] } });
  try {
    assert.equal(peer.root.querySelector('.cfg-launch-flag'), null, 'no form');
    assert.match(peer.root.textContent, /LANG.*PATH \(inherit\).*values hidden/);
    assert.match(peer.root.querySelector('#cfg-launch-readonly').textContent, /node\.update\.settings/);
    assert.deepEqual([...peer.root.querySelector('[data-launch-diff="env:LANG"]').querySelectorAll('td')].map((td) => td.textContent).slice(3), ['—', '—'], 'keys only for a restricted peer');
  } finally {
    await peer.unmount();
  }
});

test('a viewer shown keys only cannot edit, so set values are never saved back empty', async (t) => {
  const view = { ...SET_VIEW, access: 'manage', redacted: ['profile.env', 'suggested.env'], profile: { ...SET_VIEW.profile, env: [{ key: 'LANG' }] } };
  const { root, puts, unmount } = await setup(t, view);
  try {
    assert.equal(root.querySelector('.cfg-launch-value'), null, 'no value boxes to save empty');
    assert.equal(root.querySelector('#cfg-launch-save'), null);
    assert.match(root.querySelector('#cfg-launch-readonly').textContent, /values are hidden here/);
    assert.equal(puts().length, 0);
  } finally {
    await unmount();
  }
});

test('a refused variable value lands on its row; removing a row clears the placed errors', async (t) => {
  const put = () => ({ status: 400, body: { error: 'refused', errors: [{ path: 'profile.env[0].value', code: 'redacted_value', message: 'not a value' }, { path: 'profile.inherit_env[1]', code: 'bad', message: 'bad name' }] } });
  const { root, click, type, until, unmount } = await setup(t, SET_VIEW, { put });
  try {
    await type(root.querySelectorAll('.cfg-launch-value')[0], 'C');
    await click('#cfg-launch-save');
    await until(() => root.querySelector('[data-launch-row="e0"] .cfg-launch-err'), 'the value error');
    assert.match(root.querySelector('[data-launch-row="e0"] .cfg-launch-err').textContent, /not a value/);
    assert.match(root.querySelector('[data-launch-row="e2"] .cfg-launch-err').textContent, /bad name/, 'inherit_env[1] is the third row');
    await click('[data-launch-row="e0"] .cfg-launch-remove');
    assert.equal(root.querySelector('.cfg-launch-err'), null, 'no error left on a shifted row');
  } finally {
    await unmount();
  }
});

test('a conflict without a usable view reads again, edits kept; a refused clear says so', async (t) => {
  let n = 0;
  const put = () => { n += 1; return { status: 409, body: { error: 'changed', code: 'launch_profile_conflict' } }; };
  const { root, click, type, calls, until, unmount } = await setup(t, SET_VIEW, { put });
  try {
    await type(root.querySelectorAll('.cfg-launch-flag')[0], '--quiet');
    const before = calls.filter((c) => c.method === 'GET').length;
    await click('#cfg-launch-save');
    await until(() => root.querySelector('#cfg-launch-note'), 'the note');
    assert.ok(calls.filter((c) => c.method === 'GET').length > before, 'read the current view again');
    assert.equal(root.querySelectorAll('.cfg-launch-flag')[0].value, '--quiet', 'the edit is kept');
    await click('#cfg-launch-discard');
    await click('#cfg-launch-clear');
    await until(() => /not cleared/.test(root.querySelector('#cfg-launch-note')?.textContent || ''), 'the clear note');
    assert.equal(n, 2);
  } finally {
    await unmount();
  }
});

test('an inherited name the daemon reports missing is warned under its row; the daemon\'s warnings show', async (t) => {
  const view = { ...SET_VIEW, missing: ['ANTHROPIC_API_KEY'], warnings: ['Left out --api-token: a secret.'] };
  const { root, unmount } = await setup(t, view);
  try {
    assert.match(root.querySelector('[data-launch-missing="ANTHROPIC_API_KEY"]').textContent, /no ANTHROPIC_API_KEY now/);
    assert.equal(root.querySelector('[data-launch-missing="PATH"]'), null);
    assert.match(root.querySelector('[data-launch-warning]').textContent, /Left out --api-token/);
  } finally {
    await unmount();
  }
});
