// Tests for Config › Installation › Start automatically: its status line and
// main action, the launcher readout, a restart that waits for
// the daemon to answer again, and installing or removing the autostart unit.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const SYSTEMD = {
  launcher: {
    kind: 'systemd-user', unit: 'tclaude-agentd.service', unit_path: '/home/u/.config/systemd/user/tclaude-agentd.service',
    argv: ['/home/u/.local/bin/tclaude', 'agentd', 'serve', '--no-tray'], process_argv: ['tclaude', 'agentd', 'serve'],
    env: [{ key: 'PATH', value: '/usr/bin' }, { key: 'HUB_TOKEN', value: '<redacted>', from: '/home/u/.config/tclaude/env' }],
    restart_policy: 'on-failure', kill_mode: 'process', managed_by_tclaude: true, restart_method: 'supervisor', warnings: [],
  },
  autostart: {
    supported: true, kind: 'systemd-user', path: '~/.config/systemd/user/tclaude-agentd.service', installed: true, managed: true, enabled: true,
    argv: ['/home/u/.local/bin/tclaude', 'agentd', 'serve'], env: [{ key: 'PATH', value: '/usr/bin' }],
    linger: { enabled: false, command: 'sudo loginctl enable-linger u' }, warnings: [],
  },
};

function daemon(initial, { getFails = 0, pendingReads = 0, launch = null, base = '/api/node' } = {}) {
  const calls = [];
  let state = structuredClone(initial);
  let failing = 0;
  let pending = 0;
  // Every answer is a fresh object, as a real response is.
  const reply = (status, body) => ({ ok: status < 400, status, json: async () => structuredClone(body) });
  const fetchImpl = async (rawPath, options = {}) => {
    // Every route is under the panel's base; anything else is a wrong node.
    if (!rawPath.startsWith(`${base}/`)) return reply(418, { error: `not under ${base}: ${rawPath}` });
    const path = `/api/node/${rawPath.slice(base.length + 1)}`;
    const body = options.body ? JSON.parse(options.body) : null;
    calls.push({ path: rawPath, method: options.method || 'GET', body });
    if ((options.method || 'GET') === 'GET') {
      // The launch profile (config-service-launch.js): absent unless given.
      if (path === '/api/node/service/launch') return launch ? reply(200, launch) : reply(404, {});
      if (failing > 0) { failing--; throw new TypeError('Failed to fetch'); }
      // The old daemon may still answer for a moment, reporting the restart.
      if (pending > 0) { pending--; return reply(200, { ...state, restart_pending: 'supervisor' }); }
      return state ? reply(200, state) : reply(404, { error: 'not found' });
    }
    if (path === '/api/node/service/restart') {
      pending = pendingReads;
      failing = getFails;
      return reply(202, { method: 'supervisor', launcher: 'systemd-user' });
    }
    if (path === '/api/node/service/autostart') {
      if (body.serve_args?.includes('--tui')) return reply(400, { code: 'invalid_arg', error: '--tui is not a serve flag for a unit' });
      if (body.action === 'uninstall') state.autostart = { ...state.autostart, installed: false, managed: false, enabled: false, argv: [] };
      else state.autostart = { ...state.autostart, installed: true, managed: true, enabled: true, argv: ['tclaude', 'agentd', 'serve', ...(body.serve_args || [])] };
      return reply(200, state);
    }
    return reply(404, {});
  };
  return { calls, fetchImpl };
}

async function setup(t, initial, { confirm = async () => true, getFails = 0, pendingReads = 0, launch = null, base = undefined } = {}) {
  const harness = await createPreactHarness(t);
  const { ServicePanel } = await harness.importDashboardModule('js/config-service.js');
  const fake = daemon(initial, { getFails, pendingReads, launch, base });
  const toasts = [];
  const confirms = [];
  const copied = [];
  const timers = { setTimeout: (fn, ms) => setTimeout(fn, ms > 1000 ? ms : 0), clearTimeout };
  const mounted = await harness.mount(harness.html`<${ServicePanel} base=${base} fetchImpl=${fake.fetchImpl}
    confirm=${async (o) => { confirms.push(o); return confirm(o); }} toast=${(m, bad) => toasts.push({ m, bad })}
    copy=${async (text) => { copied.push(text); }} timers=${timers} />`);
  const root = mounted.container;
  const until = async (check, what) => {
    const deadline = Date.now() + 5000;
    while (!check() && Date.now() < deadline) {
      await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); });
    }
    assert.ok(check(), `timed out waiting for ${what}`);
  };
  await until(() => root.querySelector('#cfg-service') && !/Loading/.test(root.textContent), 'the first read');
  const click = async (sel) => { await harness.act(() => harness.fireEvent(root.querySelector(sel), 'click')); };
  return { ...fake, harness, root, toasts, confirms, copied, until, click, unmount: mounted.unmount };
}

test('the launcher readout shows the unit, its command and its environment, secrets hidden', async (t) => {
  const { root, unmount } = await setup(t, SYSTEMD);
  try {
    assert.equal(root.querySelector('#cfg-svc-kind').textContent, 'systemd user unit');
    assert.match(root.textContent, /tclaude-agentd\.service/);
    assert.equal(root.querySelector('#cfg-svc-observed').textContent, '/home/u/.local/bin/tclaude agentd serve --no-tray', 'the unit command, not the process argv');
    assert.match(root.textContent, /HUB_TOKEN=•••/);
    assert.ok(!root.textContent.includes('<redacted>'));
    assert.match(root.textContent, /Restart=on-failure · KillMode=process/);
  } finally {
    await unmount();
  }
});

test('restart goes through the supervisor and waits until the new agentd answers', async (t) => {
  const { root, calls, confirms, toasts, until, click, unmount } = await setup(t, SYSTEMD, { pendingReads: 2, getFails: 2 });
  try {
    await click('#cfg-svc-restart');
    await until(() => toasts.some((x) => x.m === 'agentd is back'), 'the daemon to come back');
    const reads = calls.filter((c) => c.path === '/api/node/service?refresh=1');
    assert.ok(reads.length >= 5, `an answer still reporting the restart is not "back" (reads: ${reads.length})`);
    assert.match(confirms[0].body, /through systemd user unit tclaude-agentd\.service/);
    assert.ok(calls.some((c) => c.path === '/api/node/service/restart' && c.method === 'POST'));
    assert.ok(calls.filter((c) => c.path === '/api/node/service?refresh=1').length >= 3, 'it kept reading through the restart');
    assert.match(root.querySelector('#cfg-svc-restart').textContent, /Restart agentd/);
  } finally {
    await unmount();
  }
});

test('the autostart unit is compared with what runs, and rewritten with typed serve flags', async (t) => {
  const { harness, root, calls, confirms, until, click, unmount } = await setup(t, SYSTEMD);
  try {
    assert.ok(root.querySelector('#cfg-svc-differs'), 'the unit runs without --no-tray, unlike the daemon');
    assert.ok(root.querySelector('#cfg-svc-restart-hint'), 'a restart would apply the unit now');
    harness.fireEvent(root.querySelector('input[name="cfg-svc-args"][value="custom"]'), 'change');
    await harness.act(() => {});
    const args = root.querySelector('#cfg-svc-serve-args');
    args.value = '  --no-tray   --dashboard-port 7878 ';
    await harness.act(() => harness.fireEvent(args, 'input'));
    root.querySelector('#cfg-svc-keep-env').checked = false;
    await harness.act(() => harness.fireEvent(root.querySelector('#cfg-svc-keep-env'), 'change'));
    await click('#cfg-svc-install');
    await until(() => calls.some((c) => c.path === '/api/node/service/autostart'), 'the install request');
    const sent = calls.find((c) => c.path === '/api/node/service/autostart').body;
    assert.deepEqual(sent, { action: 'install', keep_env: false, serve_args: ['--no-tray', '--dashboard-port', '7878'] });
    assert.match(confirms[0].body, /nothing restarts now/);
    await until(() => root.querySelector('#cfg-svc-configured').textContent === 'tclaude agentd serve --no-tray --dashboard-port 7878', 'the new unit to show');
  } finally {
    await unmount();
  }
});

test('a refused serve flag is shown, and nothing else changes', async (t) => {
  const { harness, root, until, click, unmount } = await setup(t, SYSTEMD);
  try {
    harness.fireEvent(root.querySelector('input[name="cfg-svc-args"][value="custom"]'), 'change');
    await harness.act(() => {});
    const args = root.querySelector('#cfg-svc-serve-args');
    args.value = '--tui';
    await harness.act(() => harness.fireEvent(args, 'input'));
    await click('#cfg-svc-install');
    await until(() => root.querySelector('#cfg-svc-error'), 'the error');
    assert.match(root.querySelector('#cfg-svc-error').textContent, /--tui is not a serve flag/);
    assert.equal(root.querySelector('#cfg-svc-configured').textContent, '/home/u/.local/bin/tclaude agentd serve');
  } finally {
    await unmount();
  }
});

test('a hand-written unit is read-only, and lingering is offered as a command only', async (t) => {
  const handWritten = structuredClone(SYSTEMD);
  handWritten.autostart.managed = false;
  const { harness, root, copied, unmount } = await setup(t, handWritten);
  try {
    assert.match(root.querySelector('#cfg-svc-autostart-state').textContent, /installed by hand/);
    assert.equal(root.querySelector('#cfg-svc-install'), null);
    assert.equal(root.querySelector('#cfg-svc-uninstall'), null);
    assert.equal(root.querySelector('#cfg-svc-linger').textContent, 'sudo loginctl enable-linger u');
    const copy = [...root.querySelectorAll('button')].find((b) => b.textContent === 'Copy');
    await harness.act(() => harness.fireEvent(copy, 'click'));
    assert.deepEqual(copied, ['sudo loginctl enable-linger u']);
  } finally {
    await unmount();
  }
});

test('removing the unit asks first; declining sends nothing', async (t) => {
  let answer = false;
  const { harness, root, calls, confirms, until, click, unmount } = await setup(t, SYSTEMD, { confirm: async () => answer });
  try {
    await click('#cfg-svc-uninstall');
    await until(() => confirms.length === 1, 'the question');
    await harness.act(async () => { await Promise.resolve(); });
    assert.ok(!calls.some((c) => c.path === '/api/node/service/autostart'), 'declined: no request');
    answer = true;
    await click('#cfg-svc-uninstall');
    await until(() => /not installed/.test(root.querySelector('#cfg-svc-autostart-state').textContent), 'the unit to be gone');
    assert.deepEqual(calls.find((c) => c.path === '/api/node/service/autostart').body, { action: 'uninstall' });
  } finally {
    await unmount();
  }
});

test('a daemon without the service routes says so; one without autostart shows only the launcher', async (t) => {
  const absent = await setup(t, null);
  try {
    assert.match(absent.root.textContent, /does not report how it was started yet/);
  } finally {
    await absent.unmount();
  }
  const { root, unmount } = await setup(t, { launcher: { kind: 'tmux', process_argv: ['tclaude', 'agentd', 'serve'], restart_method: 'exec', warnings: [] } });
  try {
    assert.equal(root.querySelector('#cfg-svc-kind').textContent, 'a tmux pane');
    assert.equal(root.querySelector('#cfg-svc-observed').textContent, 'tclaude agentd serve');
    assert.ok(root.querySelector('#cfg-svc-autostart-absent'));
  } finally {
    await unmount();
  }
});

test('custom serve flags left empty cannot be installed', async (t) => {
  const { harness, root, unmount } = await setup(t, SYSTEMD);
  try {
    harness.fireEvent(root.querySelector('input[name="cfg-svc-args"][value="custom"]'), 'change');
    await harness.act(() => {});
    assert.ok(root.querySelector('#cfg-svc-install').hasAttribute('disabled'), 'nothing typed: Rewrite is off');
    const args = root.querySelector('#cfg-svc-serve-args');
    args.value = '--no-tray';
    await harness.act(() => harness.fireEvent(args, 'input'));
    assert.ok(!root.querySelector('#cfg-svc-install').hasAttribute('disabled'));
  } finally {
    await unmount();
  }
});

test('the unit and the daemon differ only by flags, not the program path', async (t) => {
  const shell = structuredClone(SYSTEMD);
  shell.launcher = { kind: 'shell', process_argv: ['tclaude', 'agentd', 'serve'], restart_method: 'exec', warnings: [] };
  const { root, unmount } = await setup(t, shell);
  try {
    assert.equal(root.querySelector('#cfg-svc-differs'), null, '/home/u/.local/bin/tclaude and tclaude run the same flags');
    assert.ok(!/(^|\s)0(\s|$)/.test(root.querySelector('#cfg-svc-configured').parentElement.textContent));
  } finally {
    await unmount();
  }
});

test('with a launch profile set, the unit is written from it: no serve flags or env choice, install sends neither', async (t) => {
  const launch = {
    profile: { flags: ['--no-tray'], env: [], inherit_env: ['PATH'] }, suggested: { flags: [], env: [{ key: 'PATH', value: '/usr/bin' }] },
    owned: true, version: 'v1', access: 'full', comparison: { state: 'identical', baseline: 'profile', differences: [] },
  };
  const { root, calls, confirms, until, click, unmount } = await setup(t, SYSTEMD, { launch });
  try {
    await until(() => root.querySelector('#cfg-svc-by-profile'), 'the launch profile read');
    assert.equal(root.querySelector('#cfg-svc-serve-args'), null, 'no serve flags of its own');
    assert.equal(root.querySelector('#cfg-svc-keep-env'), null, 'no env choice of its own');
    await click('#cfg-svc-install');
    await until(() => calls.some((c) => c.path === '/api/node/service/autostart'), 'the install');
    assert.deepEqual(calls.find((c) => c.path === '/api/node/service/autostart').body, { action: 'install' });
    assert.match(confirms[0].body, /launch profile's flags and environment/);
  } finally {
    await unmount();
  }
});

test('the launch profile reads again after an install, so its comparison is of what runs now', async (t) => {
  const launch = { profile: null, suggested: { flags: [], env: [] }, owned: true, version: 'v1', access: 'full', comparison: { state: 'unconfigured', baseline: 'none', differences: [] } };
  const { root, calls, until, click, unmount } = await setup(t, SYSTEMD, { launch });
  try {
    await until(() => root.querySelector('#cfg-launch'), 'the launch profile read');
    const reads = () => calls.filter((c) => c.path === '/api/node/service/launch').length;
    const before = reads();
    await click('#cfg-svc-install');
    await until(() => reads() > before, 'the launch profile read again');
  } finally {
    await unmount();
  }
});

const SHELL = {
  launcher: { kind: 'shell', process_argv: ['tclaude', 'agentd', 'serve'], restart_method: 'self', warnings: [] },
  autostart: { supported: true, kind: 'systemd-user', path: '~/.config/systemd/user/tclaude-agentd.service', installed: false, managed: false, enabled: false,
    linger: { enabled: true } },
};

test('not set up: the status says agentd stops at logout, and Start on login is right there', async (t) => {
  const { root, calls, confirms, click, until, unmount } = await setup(t, SHELL);
  try {
    assert.equal(root.querySelector('#cfg-install-service-state').getAttribute('data-state'), 'off');
    assert.match(root.querySelector('#cfg-install-service-status').textContent, /Not started automatically\. agentd runs under a shell; it stops when you log out or reboot/);
    assert.equal(root.querySelector('#cfg-install-service-body').hasAttribute('data-collapsed'), false, 'not set up: the detail is open');
    await click('#cfg-install-service-on');
    await until(() => root.querySelector('#cfg-install-service-state').getAttribute('data-state') === 'on', 'the unit to be installed');
    assert.equal(confirms[0].title, 'Start agentd on login?');
    assert.deepEqual(calls.find((c) => c.method === 'POST').body, { action: 'install', keep_env: true });
    assert.ok(root.querySelector('#cfg-install-service-off'), 'set up: Remove is the main action');
    assert.equal(root.querySelector('#cfg-install-service-body').hasAttribute('data-collapsed'), false, 'acting never folds the detail away');
  } finally {
    await unmount();
  }
});

test('set up and healthy: the status names the unit and the detail folds', async (t) => {
  const healthy = structuredClone(SYSTEMD);
  healthy.autostart.argv = healthy.launcher.argv;
  const { root, click, unmount } = await setup(t, healthy);
  try {
    assert.equal(root.querySelector('#cfg-install-service-state').getAttribute('data-state'), 'on');
    assert.match(root.querySelector('#cfg-install-service-status').textContent,
      /Starts on login with the systemd user unit ~\/\.config\/systemd\/user\/tclaude-agentd\.service, written by tclaude\. It stops when you log out/);
    assert.equal(root.querySelector('#cfg-install-service-body').hasAttribute('data-collapsed'), true);
    await click('#cfg-install-service-toggle');
    assert.equal(root.querySelector('#cfg-install-service-body').hasAttribute('data-collapsed'), false);
    assert.equal(root.querySelector('#cfg-install-service-toggle').getAttribute('aria-expanded'), 'true');
  } finally {
    await unmount();
  }
});

test('a unit whose command differs from what runs needs attention', async (t) => {
  const { root, unmount } = await setup(t, SYSTEMD);
  try {
    assert.equal(root.querySelector('#cfg-install-service-state').getAttribute('data-state'), 'warn');
    assert.match(root.querySelector('#cfg-install-service-status').textContent, /its command differs from what agentd runs with now/);
    assert.equal(root.querySelector('#cfg-install-service-body').hasAttribute('data-collapsed'), false);
  } finally {
    await unmount();
  }
});

test('a peer node is read under its base, and read access shows no buttons', async (t) => {
  const base = '/api/peer/p1/node';
  const { root, calls, unmount } = await setup(t, { ...SHELL, access: 'read' }, { base });
  try {
    assert.ok(calls.length > 0 && calls.every((c) => c.path.startsWith(`${base}/service`)), 'every read is under the peer base');
    assert.ok(calls.some((c) => c.path === `${base}/service/launch`), 'the launch profile too');
    assert.match(root.querySelector('#cfg-install-service-status').textContent, /Not started automatically/);
    for (const id of ['#cfg-install-service-on', '#cfg-svc-install', '#cfg-svc-restart']) assert.equal(root.querySelector(id), null, `${id} is hidden`);
    assert.ok(root.querySelector('#cfg-svc-readonly'));
  } finally {
    await unmount();
  }
});
