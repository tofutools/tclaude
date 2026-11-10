// Tests for the Fleet node dialog's Installation view: its Start automatically
// part on the node's own service routes, and its Updates part — the status
// line and main action, and the auto-update settings below them:
// what each access level ("full", "settings", "read") can see and change,
// the PUT body, the opt-in confirmation for a package-manager method, and
// a node without the settings route.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const METHODS = [
  { id: 'release', label: 'Release download', available: true },
  { id: 'go_install', label: 'Build with go install', available: true },
  { id: 'source_checkout', label: 'Build from a checkout', available: true },
  { id: 'custom', label: 'Custom command', available: true },
  { id: 'homebrew', label: 'Let tclaude run Homebrew', available: true, package_manager: true },
  { id: 'apt', label: 'Let tclaude run apt', available: false, package_manager: true, unavailable_reason: 'needs passwordless sudo' },
  { id: 'manual', label: 'Notify me', available: true, notify_only: true },
];

function settingsView(access, overrides = {}) {
  return {
    access,
    methods: METHODS,
    detected: { method: 'release', recognized: true, reason: 'a release build' },
    redacted: access === 'full' ? [] : ['custom.command'],
    settings: {
      method: 'source_checkout',
      schedule: { frequency: 'weekly', weekday: 'sunday', window_start: '02:00', window_end: '04:00', defer_while_agents_working: true },
      update_on_start: false,
      post_update_setup: { enabled: true, flags: ['install-agent-skills'], harness: '' },
      source_checkout: { path: '~/src/tclaude', ref: '' },
      custom: { command: access === 'full' ? 'make install' : '', timeout_seconds: 1800 },
      install_dir: '~/.local/bin',
      ...overrides,
    },
  };
}

async function open(t, view, { node = { id: 'peer-1', label: 'maria-laptop' }, confirm = async () => true, save, fetchImpl } = {}) {
  const harness = await createPreactHarness(t);
  const { NodeUpdateDialog } = await harness.importDashboardModule('js/node-update.js');
  const puts = [];
  const confirms = [];
  const toasts = [];
  const actions = {
    ...(fetchImpl ? { fetchImpl } : {}),
    status: async () => ({ current_version: 'v0.42.0', latest_version: 'v0.43.1', update_available: true }),
    start: async () => { throw new Error('not in these tests'); },
    settings: async () => { if (view instanceof Error) throw view; return structuredClone(view); },
    saveSettings: async (n, body) => {
      puts.push({ node: n.id, body: structuredClone(body) });
      if (save) return save(body);
      return { ...structuredClone(view), settings: body.settings };
    },
  };
  await harness.mount(harness.html`<${NodeUpdateDialog} node=${node} actions=${actions}
    confirm=${async (o) => { confirms.push(o); return confirm(o); }} toast=${(m) => toasts.push(m)} timers=${globalThis} onClose=${() => {}} />`);
  const q = (x) => harness.document.querySelector(x);
  const flush = () => harness.act(() => new Promise((r) => setTimeout(r, 10)));
  // The settings block mounts once the status is read, then reads its own.
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline && (!q('#fleet-node-update-modal .fa-dl') || /Loading/.test(q('#fleet-node-update-modal').textContent))) await flush();
  const change = async (sel, value) => {
    const el = q(sel);
    if (el.type === 'checkbox') el.checked = value;
    else if (el.tagName === 'SELECT') {
      for (const option of el.querySelectorAll('option')) {
        option.removeAttribute('selected');
        option.selected = false;
      }
      const chosen = el.querySelector(`option[value="${value}"]`);
      chosen.setAttribute('selected', '');
      chosen.selected = true;
    } else el.value = value;
    await harness.act(() => harness.fireEvent(el, el.tagName === 'SELECT' || el.type === 'checkbox' ? 'change' : 'input'));
  };
  return { harness, q, flush, change, puts, confirms, toasts };
}

test('read access shows the settings as text, with the grant it lacks', async (t) => {
  const { q } = await open(t, settingsView('read'));
  assert.equal(q('#fleet-node-update-settings-access').textContent, 'read-only');
  assert.match(q('#fleet-node-update-settings').textContent, /needs the node\.update\.settings grant on maria-laptop/);
  assert.match(q('#fleet-node-update-settings-method').textContent, /Build from a checkout · ~\/src\/tclaude/);
  assert.equal(q('#fleet-node-update-settings-schedule').textContent, 'Weekly · Sunday · 02:00–04:00 · when no agent is working');
  assert.equal(q('#fleet-nus-method'), null, 'nothing to edit');
  assert.equal(q('#fleet-nus-save'), null);
});

test('settings access edits the schedule and setup, but not free text or local-only methods', async (t) => {
  const { q, change, flush, puts, toasts } = await open(t, settingsView('settings'));
  assert.equal(q('#fleet-node-update-settings-access').textContent, 'limited');
  assert.equal(q('#fleet-nus-checkout'), null, 'the checkout path is not an input');
  assert.match(q('#fleet-node-update-settings').textContent, /~\/src\/tclaude/);
  const option = (id) => q(`#fleet-nus-method option[value="${id}"]`);
  assert.ok(option('custom').hasAttribute('disabled'), 'cannot switch to custom');
  assert.ok(option('homebrew').hasAttribute('disabled'), 'cannot opt in to a package manager');
  assert.match(option('homebrew').textContent, /only on the node or by an unrestricted peer/);
  assert.ok(!option('source_checkout').hasAttribute('disabled'), 'the current method stays');
  assert.ok(!option('release').hasAttribute('disabled'));

  assert.ok(q('#fleet-nus-save').hasAttribute('disabled'), 'nothing changed yet');
  await change('#fleet-nus-frequency', 'daily');
  await change('#fleet-nus-on-start', true);
  await change('#fleet-nus-method', 'release');
  q('#fleet-nus-save').click();
  await flush();
  assert.equal(puts.length, 1);
  const sent = puts[0].body;
  assert.equal(sent.confirm_package_manager, undefined);
  assert.equal(sent.settings.method, 'release');
  assert.equal(sent.settings.schedule.frequency, 'daily');
  assert.equal(sent.settings.update_on_start, true);
  assert.equal(sent.settings.custom.command, '', 'the redacted command goes back empty, which keeps it');
  assert.deepEqual(toasts, ["Saved maria-laptop's update settings"]);
  assert.ok(q('#fleet-nus-save').hasAttribute('disabled'), 'saved: clean again');
});

test('a refusal from the node is shown and the edit is kept', async (t) => {
  const refusal = Object.assign(new Error('updates.self.window_start must be a 24-hour HH:MM local time'), { status: 400, body: { errors: ['updates.self.window_start must be a 24-hour HH:MM local time'] } });
  const { q, change, flush } = await open(t, settingsView('settings'), { save: () => { throw refusal; } });
  await change('#fleet-nus-start', '25:00');
  q('#fleet-nus-save').click();
  await flush();
  assert.match(q('#fleet-node-update-settings-error').textContent, /HH:MM/);
  assert.equal(q('#fleet-nus-start').value, '25:00');
  assert.ok(!q('#fleet-nus-save').hasAttribute('disabled'), 'still unsaved');
});

test('full access edits the checkout and the command, and opting in to a package manager confirms', async (t) => {
  let answer = false;
  const { q, change, flush, puts, confirms } = await open(t, settingsView('full'), { confirm: async () => answer });
  assert.equal(q('#fleet-nus-checkout').value, '~/src/tclaude');
  assert.ok(!q('#fleet-nus-method option[value="homebrew"]').hasAttribute('disabled'));
  assert.ok(q('#fleet-nus-method option[value="apt"]').hasAttribute('disabled'), 'unavailable on that node');
  await change('#fleet-nus-method', 'homebrew');
  q('#fleet-nus-save').click();
  await flush();
  assert.equal(confirms.length, 1);
  assert.equal(puts.length, 0, 'declined: nothing sent');
  answer = true;
  q('#fleet-nus-save').click();
  await flush();
  assert.equal(puts.length, 1);
  assert.equal(puts[0].body.confirm_package_manager, true);
  assert.equal(puts[0].body.settings.method, 'homebrew');

  await change('#fleet-nus-method', 'custom');
  assert.equal(q('#fleet-nus-command').value, 'make install');
});

const MANAGED = { method: 'manual', recognized: true, managed_by: 'homebrew', untrack_command: 'brew uninstall tclaude' };

test('a package-managed node warns a limited peer, which cannot acknowledge it', async (t) => {
  const limited = await open(t, { ...settingsView('settings', { method: 'release' }), detected: MANAGED });
  assert.match(limited.q('#fleet-node-update-settings-pm').textContent, /Homebrew still owns tclaude on maria-laptop; untrack it there with brew uninstall tclaude.*not acknowledged/);
  assert.equal(limited.q('#fleet-nus-ack'), null);
});

test('full access acknowledges a package-managed node', async (t) => {
  const full = await open(t, { ...settingsView('full', { method: 'release' }), detected: MANAGED });
  assert.ok(full.q('#fleet-nus-ack'));
  await full.change('#fleet-nus-ack', true);
  full.q('#fleet-nus-save').click();
  await full.flush();
  assert.equal(full.puts[0].body.settings.package_manager_ack, true);
});

test('setup options toggle flags and the harness', async (t) => {
  const { q, change, flush, puts } = await open(t, settingsView('full'), { node: { id: 'self', label: 'box', local: true } });
  assert.match(q('#fleet-node-update-settings-access').textContent, /stored on this node/);
  q('#fleet-nus-setup-edit').click();
  await flush();
  await change('[data-nus-flag="all-harnesses"]', true);
  await change('[data-nus-flag="install-agent-skills"]', false);
  await change('#fleet-nus-harness', 'codex');
  q('#fleet-nus-save').click();
  await flush();
  assert.deepEqual(puts[0].body.settings.post_update_setup, { enabled: true, flags: ['all-harnesses'], harness: 'codex' });
  assert.match(q('#fleet-node-update-settings').textContent, /tclaude update config set …/, 'the local CLI has no --node');
  assert.equal(q('#fleet-nus-unchosen'), null, 'a method is chosen');
  await change('#fleet-nus-method', '');
  assert.match(q('#fleet-nus-unchosen').textContent, /No method chosen/, 'a schedule without a method is called out');
});

test('a node without the settings route shows no settings block', async (t) => {
  const { q } = await open(t, Object.assign(new Error('not found'), { status: 404 }));
  assert.ok(q('#fleet-node-update-modal'));
  assert.equal(q('#fleet-node-update-settings'), null);
});

test('a peer on an older build that keeps the settings to itself shows no block', async (t) => {
  const { q } = await open(t, Object.assign(new Error('local only'), { status: 403, code: 'peer_view_local_only' }));
  assert.ok(q('#fleet-node-update-modal'));
  assert.equal(q('#fleet-node-update-settings'), null);
});

test('ticking a box on and off again is not an edit', async (t) => {
  const { q, change } = await open(t, settingsView('settings'));
  await change('#fleet-nus-on-start', true);
  assert.ok(!q('#fleet-nus-save').hasAttribute('disabled'));
  await change('#fleet-nus-on-start', false);
  assert.ok(q('#fleet-nus-save').hasAttribute('disabled'), 'false is the absent default');
});

test('a limited peer can switch a custom-command node back to manual, keeping what it cannot see', async (t) => {
  const { q, change, flush, puts } = await open(t, settingsView('settings', { method: 'custom', package_manager_ack: true }));
  assert.match(q('#fleet-node-update-settings').textContent, /hidden from peers/);
  assert.equal(q('#fleet-nus-command'), null);
  assert.ok(!q('#fleet-nus-method option[value="manual"]').hasAttribute('disabled'));
  await change('#fleet-nus-method', 'manual');
  q('#fleet-nus-save').click();
  await flush();
  const sent = puts[0].body.settings;
  assert.equal(sent.method, 'manual');
  assert.equal(sent.custom.command, '', 'the hidden command goes back empty');
  assert.equal(sent.custom.timeout_seconds, 1800, 'and the timeout unchanged');
  assert.deepEqual(sent.source_checkout, { path: '~/src/tclaude', ref: '' });
  assert.equal(sent.package_manager_ack, true, 'the acknowledgement is left as it was');
});

test('the Updates part says what the node does, and its main action edits the settings to save', async (t) => {
  const { q, flush, puts } = await open(t, settingsView('settings', { method: 'release', schedule: { frequency: 'off' }, update_on_start: false }));
  const state = () => q('#fleet-install-updates-state').getAttribute('data-state');
  assert.equal(state(), 'off');
  assert.match(q('#fleet-install-updates-status').textContent, /The method is Release download, but no schedule or start-up update is on/);
  assert.equal(q('#fleet-install-updates-body').hasAttribute('data-collapsed'), false, 'not set up: the settings are open');
  q('#fleet-install-updates-on').click();
  await flush();
  assert.equal(state(), 'on');
  assert.match(q('#fleet-install-updates-status').textContent, /Updates itself with Release download: daily/);
  assert.ok(q('#fleet-install-updates-unsaved'), 'the edit is not saved yet');
  assert.ok(q('#fleet-nus-start'), 'the settings below follow: the time window shows');
  q('#fleet-nus-save').click();
  await flush();
  assert.equal(puts[0].body.settings.schedule.frequency, 'daily');
  assert.equal(q('#fleet-install-updates-unsaved'), null);
  assert.equal(q('#fleet-install-updates-body').hasAttribute('data-collapsed'), true, 'saved and set up: the settings fold');
  q('#fleet-install-updates-off').click();
  await flush();
  assert.equal(state(), 'off');
  assert.equal(q('#fleet-nus-start'), null, 'the schedule is off below too');
});

test('read access shows the Updates status with no action', async (t) => {
  const { q } = await open(t, settingsView('read'));
  assert.equal(q('#fleet-install-updates-state').getAttribute('data-state'), 'on');
  assert.match(q('#fleet-install-updates-status').textContent, /Updates itself with Build from a checkout: every Sunday 02:00–04:00/);
  for (const id of ['choose', 'on', 'off']) assert.equal(q(`#fleet-install-updates-${id}`), null, `no ${id} action`);
});

test('a peer\'s Start automatically part reads the peer\'s service routes, with ids of its own', async (t) => {
  const paths = [];
  const service = {
    access: 'manage',
    launcher: { kind: 'shell', process_argv: ['tclaude', 'agentd', 'serve'], restart_method: 'self', warnings: [] },
    autostart: { supported: true, kind: 'systemd-user', path: '~/.config/systemd/user/tclaude-agentd.service', installed: false, managed: false, enabled: false },
  };
  const fetchImpl = async (path, options = {}) => {
    paths.push(`${options.method || 'GET'} ${path}`);
    if (path.endsWith('/service/launch')) return { ok: false, status: 404, json: async () => ({}) };
    return { ok: true, status: 200, json: async () => structuredClone(service) };
  };
  const { q, flush } = await open(t, settingsView('settings'), { fetchImpl });
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline && !q('#fleet-install-service-on')) await flush();
  assert.match(q('#fleet-install-service-status').textContent, /Not started automatically/);
  assert.ok(paths.includes('GET /api/peer/peer-1/node/service?refresh=1'), paths.join(', '));
  assert.ok(paths.includes('GET /api/peer/peer-1/node/service/launch'), paths.join(', '));
  assert.ok(paths.every((p) => p.includes('/api/peer/peer-1/node/')), 'nothing reaches this node');
  assert.ok(q('#fleet-svc-restart'), 'manage access restarts');
  assert.equal(q('#cfg-svc-restart'), null, 'no Config ids in the dialog');
  assert.match(q('#fleet-node-update-modal-title, #fleet-node-update-title').textContent, /Installation on maria-laptop/);
});
