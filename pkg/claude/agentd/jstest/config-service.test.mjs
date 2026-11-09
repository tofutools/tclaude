// Tests for Config › Service: the launcher readout, a restart that waits for
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

function daemon(initial, { getFails = 0 } = {}) {
  const calls = [];
  let state = structuredClone(initial);
  let failing = 0;
  // Every answer is a fresh object, as a real response is.
  const reply = (status, body) => ({ ok: status < 400, status, json: async () => structuredClone(body) });
  const fetchImpl = async (path, options = {}) => {
    const body = options.body ? JSON.parse(options.body) : null;
    calls.push({ path, method: options.method || 'GET', body });
    if ((options.method || 'GET') === 'GET') {
      if (failing > 0) { failing--; throw new TypeError('Failed to fetch'); }
      return state ? reply(200, state) : reply(404, { error: 'not found' });
    }
    if (path === '/api/node/service/restart') {
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

async function setup(t, initial, { confirm = async () => true, getFails = 0 } = {}) {
  const harness = await createPreactHarness(t);
  const { ServicePanel } = await harness.importDashboardModule('js/config-service.js');
  const fake = daemon(initial, { getFails });
  const toasts = [];
  const confirms = [];
  const copied = [];
  const timers = { setTimeout: (fn) => setTimeout(fn, 0) };
  const mounted = await harness.mount(harness.html`<${ServicePanel} fetchImpl=${fake.fetchImpl}
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

test('restart goes through the supervisor and waits until agentd answers again', async (t) => {
  const { root, calls, confirms, toasts, until, click, unmount } = await setup(t, SYSTEMD, { getFails: 2 });
  try {
    await click('#cfg-svc-restart');
    await until(() => toasts.some((x) => x.m === 'agentd is back'), 'the daemon to come back');
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
  const { root, calls, until, click, unmount } = await setup(t, SYSTEMD, { confirm: async () => answer });
  try {
    await click('#cfg-svc-uninstall');
    await new Promise((resolve) => setTimeout(resolve, 20));
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
