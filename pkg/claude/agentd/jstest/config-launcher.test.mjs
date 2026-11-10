// Tests for Config › Installation › App launcher (config-launcher.js): its
// status line and main action, the serve-flag controls and what an install
// sends, the launch profile overriding the flags, a launcher out of date,
// and what a peer with manage or read access may do.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const FILES = ['/home/u/.local/share/applications/tclaude.desktop', '/home/u/.local/share/tclaude/tclaude-launcher', '/home/u/.local/share/icons/hicolor/scalable/apps/tclaude.svg'];
const DEFAULTS = ['--persist-operator-token', '--dashboard-port=7878', '--no-print-human-token'];

function launcher(overrides = {}) {
  return {
    supported: true, kind: 'desktop-entry', path: FILES[0], files: FILES, installed: false, managed: false,
    // Not installed, the daemon reports zero options; installed ones override.
    current_exe: '/home/u/.local/bin/tclaude', current_version: 'v1.2.3', stale: false, options: { start_daemon: overrides.installed ? true : false },
    serve_args: DEFAULTS, serve_args_source: 'default', default_serve_args: DEFAULTS, operator_token_store: 'file',
    warnings: [], access: 'full', ...overrides,
  };
}

function daemon(initial, { base = '/api/node' } = {}) {
  const calls = [];
  let state = structuredClone(initial);
  const reply = (status, body) => ({ ok: status < 400, status, json: async () => structuredClone(body) });
  const fetchImpl = async (path, options = {}) => {
    const body = options.body ? JSON.parse(options.body) : null;
    calls.push({ path, method: options.method || 'GET', body });
    if (!path.startsWith(`${base}/launcher`)) return reply(418, { error: `not under ${base}` });
    const action = path.slice(`${base}/launcher`.length);
    if (action === '') return reply(200, state);
    if (action === '/install') {
      const args = body.serve_args === null ? DEFAULTS : body.serve_args || state.serve_args;
      state = { ...state, installed: true, managed: true, stale: false, exe: state.current_exe, options: { start_daemon: body.start_daemon },
        serve_args: args, installed_serve_args: args, serve_args_source: body.serve_args === null ? 'default' : body.serve_args ? 'launcher' : state.serve_args_source };
      return reply(200, state);
    }
    if (action === '/uninstall') {
      state = { ...state, installed: false, managed: false, installed_serve_args: undefined };
      return reply(200, state);
    }
    return reply(404, {});
  };
  return { calls, fetchImpl };
}

async function setup(t, initial, { base, idp, confirm = async () => true } = {}) {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/config-launcher.js');
  const fake = daemon(initial, { base });
  const confirms = [];
  const toasts = [];
  const mounted = await harness.mount(harness.html`<${m.LauncherPanel} base=${base} idp=${idp} fetchImpl=${fake.fetchImpl}
    confirm=${async (o) => { confirms.push(o); return confirm(o); }} toast=${(msg) => toasts.push(msg)} />`);
  const root = mounted.container;
  const p = idp || 'cfg-';
  const q = (sel) => root.querySelector(sel.replaceAll('#P-', `#${p}`));
  const until = async (check, what) => {
    const deadline = Date.now() + 5000;
    while (!check() && Date.now() < deadline) {
      await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); });
    }
    assert.ok(check(), `timed out waiting for ${what}`);
  };
  await until(() => q('#P-install-launcher-state') && !/Loading/.test(root.textContent), 'the first read');
  const click = async (sel) => { await harness.act(() => harness.fireEvent(q(sel), 'click')); };
  const tick = async (name, on) => {
    const box = root.querySelector(`[data-launcher-flag="${name}"]`);
    box.checked = on;
    await harness.act(() => harness.fireEvent(box, 'change'));
  };
  const type = async (sel, value) => {
    const el = q(sel);
    el.value = value;
    await harness.act(() => harness.fireEvent(el, 'input'));
  };
  return { ...fake, m, harness, root, q, confirms, toasts, until, click, tick, type, unmount: mounted.unmount };
}

test('serve flags split into the controls and back, keeping flags it has no control for', async (t) => {
  const harness = await createPreactHarness(t);
  const { flagsDraft, draftArgs } = await harness.importDashboardModule('js/config-launcher.js');
  const d = flagsDraft(['--dashboard-bind=127.0.0.1', '--dashboard-port=9000', '--no-tray', '--persist-operator-token']);
  assert.deepEqual(d, { on: { '--dashboard-port': true, '--no-tray': true, '--persist-operator-token': true }, port: '9000', other: '--dashboard-bind=127.0.0.1' });
  assert.deepEqual(draftArgs(d), ['--persist-operator-token', '--dashboard-port=9000', '--no-tray', '--dashboard-bind=127.0.0.1']);
  assert.deepEqual(draftArgs({ on: { '--dashboard-port': true }, port: '', other: '' }), ['--dashboard-port=7878'], 'an empty port is the default');
});

test('not installed: the status says so and Add to app menu… installs, naming the files it writes', async (t) => {
  const { q, calls, confirms, click, until, unmount } = await setup(t, launcher());
  try {
    assert.equal(q('#P-install-launcher-state').getAttribute('data-state'), 'off');
    assert.match(q('#P-install-launcher-status').textContent, /Not in the app menu\. A launcher would open the dashboard from the desktop, starting agentd first/);
    assert.equal(q('#P-install-launcher-body').hasAttribute('data-collapsed'), false);
    assert.equal(q('#P-launcher-args').textContent, 'agentd serve --persist-operator-token --dashboard-port=7878 --no-print-human-token');
    assert.match(q('#P-launcher-source').textContent, /tclaude's defaults/);
    await click('#P-install-launcher-on');
    await until(() => q('#P-install-launcher-state').getAttribute('data-state') === 'on', 'the install');
    assert.equal(confirms[0].title, 'Add tclaude to the app menu?');
    for (const f of FILES) assert.ok(confirms[0].body.includes(f), `the confirm names ${f}`);
    assert.deepEqual(calls.find((c) => c.method === 'POST').body, { start_daemon: true }, 'unchanged flags are not sent');
    assert.ok(q('#P-install-launcher-off'), 'installed: Remove is offered');
  } finally {
    await unmount();
  }
});

test('changed flags are sent with the install, and the defaults can be restored', async (t) => {
  const { q, calls, click, tick, type, until, unmount } = await setup(t, launcher({ installed: true, managed: true, exe: '/home/u/.local/bin/tclaude', installed_serve_args: DEFAULTS }));
  try {
    assert.equal(q('#P-install-launcher-body').hasAttribute('data-collapsed'), true, 'installed and healthy: the detail folds');
    await tick('--no-print-human-token', false);
    await tick('--no-tray', true);
    await type('#P-launcher-port', '9000');
    await type('#P-launcher-other', '--dashboard-bind=127.0.0.1');
    assert.equal(q('#P-launcher-args').textContent, 'agentd serve --persist-operator-token --dashboard-port=9000 --no-tray --dashboard-bind=127.0.0.1');
    await click('#P-launcher-apply');
    await until(() => calls.some((c) => c.method === 'POST'), 'the install');
    assert.deepEqual(calls.find((c) => c.method === 'POST').body,
      { start_daemon: true, serve_args: ['--persist-operator-token', '--dashboard-port=9000', '--no-tray', '--dashboard-bind=127.0.0.1'] });
    await until(() => q('#P-launcher-defaults'), 'the launcher setting');
    assert.match(q('#P-launcher-source').textContent, /the launcher's own setting/);
    await click('#P-launcher-defaults');
    await until(() => calls.filter((c) => c.method === 'POST').length === 2, 'the second install');
    assert.deepEqual(calls.filter((c) => c.method === 'POST')[1].body, { start_daemon: true, serve_args: null });
  } finally {
    await unmount();
  }
});

test('with a launch profile set the part says the profile decides the flags', async (t) => {
  const { q, unmount } = await setup(t, launcher({ serve_args_source: 'profile', serve_args: ['--no-tray'] }));
  try {
    assert.match(q('#P-launcher-source').textContent, /from the launch profile, which overrides/);
    assert.ok(q('#P-launcher-by-profile'));
  } finally {
    await unmount();
  }
});

test('a launcher out of date needs attention, and Reinstall… rewrites it', async (t) => {
  const { q, calls, click, until, unmount } = await setup(t, launcher({ installed: true, managed: true, stale: true, stale_reason: 'it runs /old/tclaude', exe: '/old/tclaude' }));
  try {
    assert.equal(q('#P-install-launcher-state').getAttribute('data-state'), 'warn');
    assert.match(q('#P-install-launcher-status').textContent, /out of date: it runs \/old\/tclaude/);
    await click('#P-install-launcher-reinstall');
    await until(() => q('#P-install-launcher-state').getAttribute('data-state') === 'on', 'the reinstall');
    assert.ok(calls.some((c) => c.method === 'POST' && c.path === '/api/node/launcher/install'));
  } finally {
    await unmount();
  }
});

test('a hand-made launcher is left alone', async (t) => {
  const { q, unmount } = await setup(t, launcher({ installed: true, managed: false }));
  try {
    assert.match(q('#P-install-launcher-status').textContent, /which tclaude did not write/);
    for (const id of ['#P-install-launcher-on', '#P-install-launcher-off', '#P-launcher-apply']) assert.equal(q(id), null, `${id} is not offered`);
  } finally {
    await unmount();
  }
});

test('a peer is read under its base; manage access sets only the known flags, read access nothing', async (t) => {
  const base = '/api/peer/p1/node';
  const manage = await setup(t, launcher({ access: 'manage' }), { base, idp: 'fleet-' });
  try {
    assert.ok(manage.calls.every((c) => c.path.startsWith(`${base}/launcher`)));
    assert.ok(manage.q('#P-install-launcher-on'), 'manage access installs');
    assert.equal(manage.q('#P-launcher-other'), null, 'no free-form flags for a peer without full access');
    assert.equal(manage.root.querySelector('#cfg-install-launcher'), null, 'its own ids');
  } finally {
    await manage.unmount();
  }
  const locked = await setup(t, launcher({ access: 'manage', installed: true, managed: true, serve_args: ['--no-tray', '--dashboard-bind=127.0.0.1'], serve_args_source: 'launcher' }), { base, idp: 'fleet-' });
  try {
    assert.ok(locked.q('#P-launcher-other-locked'), 'flags it may not send are named');
    await locked.tick('--persist-operator-token', true);
    assert.ok(locked.q('#P-launcher-apply').disabled, 'a change the node would refuse cannot be sent');
  } finally {
    await locked.unmount();
  }
  const read = await setup(t, launcher({ access: 'read' }), { base, idp: 'fleet-' });
  try {
    assert.ok(read.q('#P-launcher-readonly'));
    for (const id of ['#P-install-launcher-on', '#P-launcher-apply']) assert.equal(read.q(id), null, `${id} is hidden`);
    assert.ok(read.root.querySelector('[data-launcher-flag="--no-tray"]').disabled, 'the flags show, disabled');
  } finally {
    await read.unmount();
  }
});

test('an unsupported OS says so', async (t) => {
  const unsupported = await setup(t, { supported: false, warnings: [], access: 'full' });
  try {
    assert.match(unsupported.q('#P-install-launcher-status').textContent, /Linux and macOS only/);
    assert.equal(unsupported.q('#P-launcher-apply'), null);
  } finally {
    await unsupported.unmount();
  }
});
