// Tests for Config › Installation › Updates: updates.self. Its fields load and save with
// the rest of the Config form, an untouched form adds no keys, the notes
// follow the daemon's install detection, a package-manager method is opt-in
// behind a confirmation, and a tclaude-driven method next to a
// package-managed binary needs the operator's acknowledgement.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const METHODS = [
  { id: 'release', label: 'Release download', available: true },
  { id: 'go_install', label: 'Build with go install', available: true },
  { id: 'source_checkout', label: 'Build from a checkout', available: false, unavailable_reason: 'needs git — not on PATH' },
  { id: 'custom', label: 'Custom command', available: true },
  { id: 'homebrew', label: 'Let tclaude run Homebrew', available: true, package_manager: true, command: 'brew upgrade tofutools/tap/tclaude' },
  { id: 'manual', label: 'Notify me', available: true, notify_only: true },
];

const HOMEBREW = {
  method: 'manual', recognized: true, managed_by: 'homebrew', reason: 'tclaude resolves into a Homebrew Cellar',
  manual_command: 'brew upgrade tofutools/tap/tclaude', untrack_command: 'brew uninstall tclaude',
  binaries: [{ name: 'tclaude', path: '/opt/homebrew/Cellar/tclaude/0.42.0/bin/tclaude', managed_by: 'homebrew' }],
};

async function selectValue(harness, select, value) {
  const applySelection = () => {
    for (const option of select.querySelectorAll('option')) {
      option.removeAttribute('selected');
      option.selected = false;
    }
    const selected = select.querySelector(`option[value="${value}"]`);
    selected.setAttribute('selected', '');
    selected.selected = true;
  };
  applySelection();
  await harness.act(() => harness.fireEvent(select, 'change'));
}

async function mountConfig(t, config, { detected = { method: 'release', recognized: true, reason: 'a release build' }, confirm = async () => true, settings = {} } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter, { setConfigPanel }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
    harness.importDashboardModule('js/config-panel.js'),
  ]);
  setConfigPanel('installation');
  const holder = { raw: JSON.stringify(config) };
  const calls = [];
  const confirms = [];
  const fetchImpl = async (path) => {
    calls.push(path);
    if (path.startsWith('/api/node/update/settings')) {
      return { ok: true, status: 200, json: async () => ({ methods: METHODS, detected, setup_flags: [], ...settings }) };
    }
    if (path === '/api/node/update') {
      return { ok: true, status: 200, json: async () => ({ current_version: 'v0.42.0', latest_version: 'v0.42.0', update_available: false, chosen_method: 'custom' }) };
    }
    return { ok: true, status: 200, json: async () => ({ raw: holder.raw }) };
  };
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl, confirm: async (options) => { confirms.push(options); return confirm(options); },
  }} />`);
  const settle = async () => {
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve)); });
  };
  await adapter.loadConfigTab();
  // The panel reads the install detection once it is open.
  const deadline = Date.now() + 5000;
  while (!mounted.container.querySelector('#cfg-selfupdate-detected') && Date.now() < deadline) await settle();
  assert.ok(mounted.container.querySelector('#cfg-selfupdate-detected'), 'the install detection loads while the panel is open');
  const load = async (next) => {
    holder.raw = JSON.stringify(next);
    await adapter.loadConfigTab();
    await settle();
  };
  return { harness, adapter, root: mounted.container, calls, confirms, settle, load, unmount: mounted.unmount };
}

test('Updates loads every updates.self key and saves it back unchanged', async (t) => {
  const self = {
    method: 'custom',
    schedule: { frequency: 'weekly', weekday: 'sunday', window_start: '22:00', window_end: '02:00', defer_while_agents_working: true },
    update_on_start: true,
    post_update_setup: { enabled: true, flags: ['install-agent-skills', 'all-harnesses'], harness: 'codex' },
    source_checkout: { path: '~/git/tclaude', ref: 'main' },
    custom: { command: 'nix-env -u tclaude', timeout_seconds: 600 },
    install_dir: '~/bin',
    package_manager_ack: true,
    some_future_key: { kept: true },
  };
  const { adapter, root, unmount } = await mountConfig(t, { updates: { scripts: [{ name: 'x', update: 'true' }], self } });
  try {
    const value = (id) => root.querySelector(`#${id}`).value;
    assert.equal(value('cfg-selfupdate-method'), 'custom');
    assert.equal(value('cfg-selfupdate-frequency'), 'weekly');
    assert.equal(value('cfg-selfupdate-weekday'), 'sunday');
    assert.equal(value('cfg-selfupdate-window-start'), '22:00');
    assert.equal(value('cfg-selfupdate-custom-command'), 'nix-env -u tclaude');
    assert.equal(root.querySelector('#cfg-selfupdate-flag-all-harnesses').checked, true);
    assert.equal(value('cfg-selfupdate-setup-harness'), 'codex');
    assert.equal(root.querySelector('#cfg-selfupdate-custom-command').closest('.cfg-su-when').hidden, false, 'the custom command shows for the custom method');
    assert.equal(root.querySelector('#cfg-selfupdate-checkout-path').closest('.cfg-su-when').hidden, true, 'the checkout rows hide for other methods');
    assert.match(root.querySelector('#cfg-selfupdate-setup-preview').textContent,
      /tclaude setup --yes --install-agent-skills --all-harnesses --harness codex/);

    const saved = adapter.assembleConfig().updates;
    assert.deepEqual(saved.self, self, 'an untouched form saves the block as it was, unknown keys too');
    assert.deepEqual(saved.scripts, [{ name: 'x', update: 'true' }], 'sibling updates keys are kept');
  } finally {
    await unmount();
  }
});

test('an untouched Updates panel adds nothing to a config without updates.self', async (t) => {
  const { adapter, root, unmount } = await mountConfig(t, { log_level: 'info' });
  try {
    assert.equal(root.querySelector('#cfg-selfupdate-method').value, '', 'no method is chosen');
    assert.match(root.querySelector('#cfg-selfupdate-method-note').textContent, /No method chosen/);
    assert.equal(adapter.assembleConfig().updates, undefined, 'defaults are not written out');
  } finally {
    await unmount();
  }
});

test('a schedule edits only the keys that differ from the defaults', async (t) => {
  const { harness, adapter, root, unmount } = await mountConfig(t, {});
  try {
    await selectValue(harness, root.querySelector('#cfg-selfupdate-method'), 'release');
    await selectValue(harness, root.querySelector('#cfg-selfupdate-frequency'), 'daily');
    assert.equal(root.querySelector('#cfg-selfupdate-window-start').closest('.cfg-su-when').hidden, false, 'the window shows once scheduled');
    assert.equal(root.querySelector('#cfg-selfupdate-weekday').closest('.cfg-su-when').hidden, true, 'the day is for weekly runs only');
    assert.deepEqual(adapter.assembleConfig().updates, { self: { method: 'release', schedule: { frequency: 'daily' } } });
  } finally {
    await unmount();
  }
});

test('the method picker shows what is unavailable and why', async (t) => {
  const { root, unmount } = await mountConfig(t, {});
  try {
    const option = root.querySelector('#cfg-selfupdate-method option[value="source_checkout"]');
    assert.ok(option.hasAttribute('disabled'), 'disabled');
    assert.match(option.textContent, /needs git — not on PATH/);
    assert.match(root.querySelector('#cfg-selfupdate-detected').textContent, /Detected: Release download — a release build/);
  } finally {
    await unmount();
  }
});

test('a package-manager method is opt-in: declining the confirmation keeps the previous choice', async (t) => {
  let answer = false;
  const { harness, adapter, root, confirms, settle, unmount } = await mountConfig(t, { updates: { self: { method: 'manual' } } },
    { detected: HOMEBREW, confirm: async () => answer });
  try {
    const method = root.querySelector('#cfg-selfupdate-method');
    await selectValue(harness, method, 'homebrew');
    await settle();
    assert.equal(confirms.length, 1);
    assert.match(confirms[0].body, /brew upgrade tofutools\/tap\/tclaude/);
    assert.equal(method.value, 'manual', 'declined: back to the previous method');
    assert.equal(adapter.assembleConfig().updates.self.method, 'manual');

    answer = true;
    await selectValue(harness, method, 'homebrew');
    await settle();
    assert.equal(method.value, 'homebrew');
    assert.equal(adapter.assembleConfig().updates.self.method, 'homebrew');
  } finally {
    await unmount();
  }
});

test('a tclaude-driven method next to a package-managed binary needs the acknowledgement', async (t) => {
  const { harness, adapter, root, confirms, unmount } = await mountConfig(t, {}, {
    detected: HOMEBREW, settings: { install_dir_shadowed: true, shadowed_by: '/opt/homebrew/bin/tclaude' },
  });
  try {
    assert.match(root.querySelector('#cfg-selfupdate-detected').textContent, /Managed by Homebrew/);
    assert.equal(root.querySelector('#cfg-selfupdate-manual-command').textContent, 'brew upgrade tofutools/tap/tclaude');
    const ack = root.querySelector('#cfg-selfupdate-pm-ack');
    assert.equal(ack.closest('.cfg-su-when').hidden, true, 'nothing to acknowledge while no method installs');

    await selectValue(harness, root.querySelector('#cfg-selfupdate-method'), 'release');
    assert.equal(confirms.length, 0, 'only package-manager methods ask to confirm');
    assert.equal(ack.closest('.cfg-su-when').hidden, false);
    assert.equal(root.querySelector('#cfg-selfupdate-untrack-command').textContent, 'brew uninstall tclaude');
    assert.match(root.querySelector('#cfg-selfupdate-shadowed').textContent, /comes after\s+\/opt\/homebrew\/bin\/tclaude/);
    assert.throws(() => adapter.assembleConfig(), /Homebrew still manages tclaude here/);

    ack.checked = true;
    await harness.act(() => harness.fireEvent(ack, 'change'));
    assert.deepEqual(adapter.assembleConfig().updates.self, { method: 'release', package_manager_ack: true });
  } finally {
    await unmount();
  }
});

test('typing an install dir asks the daemon whether it is shadowed', async (t) => {
  const { harness, root, calls, unmount } = await mountConfig(t, { updates: { self: { method: 'release', package_manager_ack: true } } }, { detected: HOMEBREW });
  try {
    const dir = root.querySelector('#cfg-selfupdate-install-dir');
    dir.value = '~/bin';
    await harness.act(() => harness.fireEvent(dir, 'input'));
    const deadline = Date.now() + 5000;
    while (!calls.includes('/api/node/update/settings?install_dir=~%2Fbin') && Date.now() < deadline) {
      await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 20)); });
    }
    assert.ok(calls.includes('/api/node/update/settings?install_dir=~%2Fbin'), `calls: ${calls.join(', ')}`);
  } finally {
    await unmount();
  }
});

test('a failed install detection says so and keeps the method picker usable', async (t) => {
  const { harness, adapter, root, unmount } = await mountConfig(t, {}, {
    detected: null, settings: { detection_error: 'os.Executable: no such file', warnings: ['tclaude-agentd is not on PATH'] },
  });
  try {
    const text = root.querySelector('#cfg-selfupdate-detected').textContent;
    assert.match(text, /Could not tell how tclaude was installed: os.Executable: no such file/);
    assert.match(text, /tclaude-agentd is not on PATH/);
    await selectValue(harness, root.querySelector('#cfg-selfupdate-method'), 'release');
    assert.deepEqual(adapter.assembleConfig().updates, { self: { method: 'release' } }, 'no acknowledgement is asked for without a detected package manager');
  } finally {
    await unmount();
  }
});

test('setup flags keep the file order and flags without a checkbox', async (t) => {
  const flags = ['all-harnesses', 'some-future-flag', 'install-agent-skills'];
  const { harness, adapter, root, unmount } = await mountConfig(t, { updates: { self: { post_update_setup: { enabled: true, flags } } } });
  try {
    assert.deepEqual(adapter.assembleConfig().updates.self.post_update_setup.flags, flags, 'an untouched list is saved as it was');
    const box = (flag) => root.querySelector(`#cfg-selfupdate-flag-${flag}`);
    box('all-harnesses').checked = false;
    await harness.act(() => harness.fireEvent(box('all-harnesses'), 'change'));
    box('absolute-paths').checked = true;
    await harness.act(() => harness.fireEvent(box('absolute-paths'), 'change'));
    assert.deepEqual(adapter.assembleConfig().updates.self.post_update_setup.flags,
      ['some-future-flag', 'install-agent-skills', 'absolute-paths'], 'unticked flags go, new ones are appended');
  } finally {
    await unmount();
  }
});

test('a schedule or harness value this build does not list is kept on save', async (t) => {
  const self = { schedule: { frequency: 'hourly', weekday: 'funday' }, post_update_setup: { harness: 'future-harness' } };
  const { adapter, root, load, unmount } = await mountConfig(t, { updates: { self } });
  try {
    assert.equal(root.querySelector('#cfg-selfupdate-frequency').value, 'hourly');
    assert.deepEqual(adapter.assembleConfig().updates.self, self);
    await load({});
    assert.equal(root.querySelectorAll('#cfg-selfupdate-frequency option[data-cfg-current]').length, 0, 'a reload drops the stale option');
    assert.equal(adapter.assembleConfig().updates, undefined);
  } finally {
    await unmount();
  }
});

test('the Updates part says whether tclaude updates itself, and its action edits the form', async (t) => {
  const { harness, adapter, root, load, unmount } = await mountConfig(t, {});
  try {
    const status = () => root.querySelector('#cfg-install-updates-status').textContent;
    const state = () => root.querySelector('#cfg-install-updates-state').getAttribute('data-state');
    assert.equal(root.querySelector('#cfg-selfupdate-on-start-svc'), null, 'update on start is shown once');
    assert.equal(state(), 'off');
    assert.match(status(), /No update method is chosen/);
    assert.ok(root.querySelector('#cfg-install-updates-choose'), 'not set up: the main action picks a method');
    assert.equal(root.querySelector('#cfg-install-updates-body').hasAttribute('data-collapsed'), false, 'not set up: the detail is open');

    await load({ updates: { self: { method: 'release' } } });
    assert.equal(state(), 'off');
    assert.match(status(), /method is Release download, but no schedule/);
    await harness.act(() => root.querySelector('#cfg-install-updates-on').click());
    assert.equal(root.querySelector('#cfg-selfupdate-frequency').value, 'daily', 'Update daily edits the form');
    assert.deepEqual(adapter.assembleConfig().updates.self, { method: 'release', schedule: { frequency: 'daily' } });
    assert.equal(state(), 'on');
    assert.match(status(), /Updates itself with Release download: daily 03:00–05:00/);
    assert.equal(root.querySelector('#cfg-install-updates-body').hasAttribute('data-collapsed'), false, 'an edit never folds the detail away');

    await load({ updates: { self: { method: 'release', update_on_start: true, schedule: { frequency: 'weekly', weekday: 'friday' } } } });
    assert.match(status(), /every Friday 03:00–05:00 and when agentd starts/);
    assert.equal(root.querySelector('#cfg-install-updates-body').hasAttribute('data-collapsed'), true, 'set up: the detail folds');
    await harness.act(() => root.querySelector('#cfg-install-updates-toggle').click());
    assert.equal(root.querySelector('#cfg-install-updates-body').hasAttribute('data-collapsed'), false, 'Show details opens it');
    await harness.act(() => root.querySelector('#cfg-install-updates-off').click());
    assert.equal(root.querySelector('#cfg-selfupdate-on-start').checked, false);
    assert.equal(adapter.assembleConfig().updates.self.method, 'release', 'Turn off keeps the method');
    assert.equal(adapter.assembleConfig().updates.self.update_on_start, undefined);
    assert.equal(adapter.assembleConfig().updates.self.schedule.frequency, 'off');
    assert.equal(state(), 'off');

    // A method that cannot run here needs attention, and never folds as healthy.
    await load({ updates: { self: { method: 'source_checkout', schedule: { frequency: 'daily' } } } });
    const deadline = Date.now() + 5000;
    while (state() !== 'warn' && Date.now() < deadline) await harness.act(async () => { await new Promise((r) => setTimeout(r, 5)); });
    assert.equal(state(), 'warn');
    assert.match(status(), /Build from a checkout cannot run here: needs git/);
    assert.equal(root.querySelector('#cfg-install-updates-body').hasAttribute('data-collapsed'), false);
  } finally {
    await unmount();
  }
});

test('Try it acts on the saved method: a method edit asks to save first, a schedule edit does not', async (t) => {
  const { harness, root, calls, settle, unmount } = await mountConfig(t, { updates: { self: { method: 'custom', custom: { command: 'make install' } } } },
    { settings: { settings: { method: 'custom', custom: { command: 'make install' } } } });
  try {
    const deadline = Date.now() + 5000;
    while (Date.now() < deadline && /Reading/.test(root.querySelector('#cfg-update-now-version')?.textContent || 'Reading')) await settle();
    assert.ok(calls.includes('/api/node/update'), 'the row reads the version through the Config fetch');
    const note = () => root.querySelector('#cfg-update-now-note').textContent;
    assert.match(note(), /saved method \(Custom command\)/);
    assert.ok(!root.querySelector('#cfg-update-now-apply').hasAttribute('disabled'));

    await selectValue(harness, root.querySelector('#cfg-selfupdate-frequency'), 'daily');
    assert.ok(!root.querySelector('#cfg-update-now-apply').hasAttribute('disabled'), 'a schedule edit does not change what Update now runs');

    const command = root.querySelector('#cfg-selfupdate-custom-command');
    assert.ok(command.classList.contains('ui-textarea'), 'the command box takes the theme field styling');
    command.value = 'git pull && go install .';
    await harness.act(() => harness.fireEvent(command, 'input'));
    assert.match(note(), /save changes first/);
    assert.ok(root.querySelector('#cfg-update-now-apply').hasAttribute('disabled'));
    assert.ok(root.querySelector('#cfg-update-now-test').hasAttribute('disabled'));

    command.value = 'make install';
    await harness.act(() => harness.fireEvent(command, 'input'));
    assert.match(note(), /saved method/, 'putting it back is not an edit');
  } finally {
    unmount();
  }
});
