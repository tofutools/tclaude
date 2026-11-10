// Tests for Config › Git & forges: the agent.git_proxy form. It loads every
// key into its row, saves only the keys it owns, and keeps apart the two
// protected_refs meanings the file distinguishes (absent: the default
// branches; []: none).

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

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
  applySelection();
}

async function mountConfig(t, config) {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
  ]);
  const holder = { raw: JSON.stringify(config) };
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl: async () => ({ ok: true, json: async () => ({ raw: holder.raw }) }),
  }} />`);
  await adapter.loadConfigTab();
  await harness.act(() => {});
  const load = async (next) => {
    holder.raw = JSON.stringify(next);
    await adapter.loadConfigTab();
    await harness.act(() => {});
  };
  return { harness, adapter, state, root: mounted.container, load, unmount: mounted.unmount };
}

const FULL = {
  allowed_remotes: ['gitlab.example.com/team/*', 'github.com/acme'],
  protected_refs: ['main', 'release/*'],
  allow_force_push: true,
  ssh_key: '~/.ssh/id_ed25519',
  github_token_file: '~/.config/tclaude/github-token',
  gitlab_hosts: { 'gitlab.example.com': 'https://gitlab.example.com:8929', 'gitlab.com': 'https://gitlab.com' },
  gitlab_token_file: '~/.config/tclaude/gitlab-token',
  some_future_key: { kept: true },
};

test('Git & forges loads every git_proxy key and saves it back unchanged', async (t) => {
  const { adapter, root, unmount } = await mountConfig(t, { agent: { clone_cooldown: '2m', git_proxy: FULL } });
  try {
    const values = (id) => [...root.querySelectorAll(`#${id} .cfg-list-row input`)].map((input) => input.value);
    assert.deepEqual(values('cfg-gitproxy-remotes'), FULL.allowed_remotes);
    assert.equal(root.querySelector('#cfg-gitproxy-protected-mode').value, 'custom');
    assert.equal(root.querySelector('#cfg-gitproxy-protected-wrap').hidden, false, 'the custom list shows');
    assert.deepEqual(values('cfg-gitproxy-protected'), ['main', 'release/*']);
    assert.equal(root.querySelector('#cfg-gitproxy-forcepush').checked, true);
    assert.equal(root.querySelector('#cfg-gitproxy-sshkey').value, '~/.ssh/id_ed25519');
    assert.equal(root.querySelector('#cfg-gitproxy-ghtoken').value, '~/.config/tclaude/github-token');
    assert.equal(root.querySelector('#cfg-gitproxy-gltoken').value, '~/.config/tclaude/gitlab-token');
    assert.deepEqual(values('cfg-gitproxy-gitlab-hosts'),
      ['gitlab.com', 'https://gitlab.com', 'gitlab.example.com', 'https://gitlab.example.com:8929'],
      'one row per instance, host then base, in a stable order');

    const saved = adapter.assembleConfig().agent;
    assert.deepEqual(saved.git_proxy, FULL, 'an untouched form saves the block as it was, unknown keys too');
    assert.equal(saved.clone_cooldown, '2m', 'sibling agent keys are kept');
  } finally {
    await unmount();
  }
});

test('Git & forges keeps protected_refs absent, empty and listed apart', async (t) => {
  const { harness, adapter, root, load, unmount } = await mountConfig(t, { agent: { git_proxy: { allowed_remotes: ['github.com/acme'] } } });
  try {
    const mode = root.querySelector('#cfg-gitproxy-protected-mode');
    const wrap = root.querySelector('#cfg-gitproxy-protected-wrap');
    assert.equal(mode.value, 'default');
    assert.equal(wrap.hidden, true, 'no list to edit while the default applies');
    assert.equal('protected_refs' in adapter.assembleConfig().agent.git_proxy, false, 'the default stays absent');

    await load({ agent: { git_proxy: { protected_refs: [] } } });
    assert.equal(mode.value, 'none', '[] reads as protection off');
    assert.deepEqual(adapter.assembleConfig().agent.git_proxy.protected_refs, []);

    await selectValue(harness, mode, 'default');
    assert.equal(adapter.assembleConfig().agent, undefined, 'back to the default, an otherwise empty block goes');

    await selectValue(harness, mode, 'custom');
    assert.equal(wrap.hidden, false);
    assert.throws(() => adapter.assembleConfig(), /Custom needs at least one branch/,
      'an empty custom list is refused rather than saved as "protect nothing"');

    await selectValue(harness, mode, 'none');
    assert.deepEqual(adapter.assembleConfig().agent.git_proxy, { protected_refs: [] });
  } finally {
    await unmount();
  }
});

test('Git & forges edits GitLab instances as host → base rows', async (t) => {
  const { harness, adapter, root, unmount } = await mountConfig(t, {
    agent: { git_proxy: { gitlab_hosts: { 'gitlab.example.com': 'https://gitlab.example.com:8929' } } },
  });
  try {
    harness.fireEvent(root.querySelector('#cfg-gitproxy-gitlab-hosts .cfg-list-add'), 'click');
    await harness.act(() => {});
    const rows = root.querySelectorAll('#cfg-gitproxy-gitlab-hosts .cfg-list-row');
    assert.equal(rows.length, 2);
    assert.equal(adapter.assembleConfig().agent.git_proxy.gitlab_hosts['gitlab.example.com'], 'https://gitlab.example.com:8929',
      'a blank new row saves nothing');
    await harness.input(rows[1].querySelector('[data-role="host"]'), ' gitlab.com ');
    await harness.input(rows[1].querySelector('[data-role="base"]'), 'https://gitlab.com');
    assert.deepEqual(adapter.assembleConfig().agent.git_proxy.gitlab_hosts, {
      'gitlab.example.com': 'https://gitlab.example.com:8929', 'gitlab.com': 'https://gitlab.com',
    });

    await harness.input(rows[1].querySelector('[data-role="host"]'), 'gitlab.example.com');
    assert.throws(() => adapter.assembleConfig(), /gitlab\.example\.com is listed twice/);

    harness.fireEvent(rows[1].querySelector('.cfg-row-del'), 'click');
    await harness.act(() => {});
    harness.fireEvent(root.querySelector('#cfg-gitproxy-gitlab-hosts .cfg-row-del'), 'click');
    await harness.act(() => {});
    assert.equal(adapter.assembleConfig().agent, undefined, 'no instances and nothing else: the block goes');
  } finally {
    await unmount();
  }
});

test('Git & forges drops emptied keys and leaves the rest of git_proxy alone', async (t) => {
  const { harness, adapter, root, unmount } = await mountConfig(t, {
    agent: { git_proxy: { allowed_remotes: ['github.com/acme'], ssh_key: '~/.ssh/k', some_future_key: 1 } },
  });
  try {
    harness.fireEvent(root.querySelector('#cfg-gitproxy-remotes .cfg-row-del'), 'click');
    await harness.act(() => {});
    await harness.input(root.querySelector('#cfg-gitproxy-sshkey'), '  ');
    assert.deepEqual(adapter.assembleConfig().agent.git_proxy, { some_future_key: 1 },
      'an empty allow-list and a blank path are removed, not written empty');
  } finally {
    await unmount();
  }
});

test('Git & forges is a Config section with searchable rows', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, { CONFIG_PANELS }, { KNOWN_SUBTABS }] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-markup.js'),
    harness.importDashboardModule('js/nav-history-core.js'),
  ]);
  assert.ok(CONFIG_PANELS.some((panel) => panel.id === 'git' && panel.title === 'Git & forges'));
  assert.ok(KNOWN_SUBTABS.config.has('git'), '/config/git is a known route');

  const tab = harness.document.body.appendChild(harness.document.createElement('section'));
  tab.id = 'tab-config';
  const host = tab.appendChild(harness.document.createElement('div'));
  const state = createConfigState({ activeTab: harness.signals.signal('groups') });
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} />`, host);
  const root = mounted.container;
  assert.ok(root.querySelector('[data-cfg-panel-link="git"]'), 'the rail links the section');
  const filter = root.querySelector('#cfg-filter');
  filter.value = 'gitlab_hosts';
  await harness.act(() => harness.fireEvent(filter, 'input'));
  assert.equal(root.querySelector('#cfg-gitproxy-gitlab-hosts').closest('.cfg-row').hidden, false);
  assert.equal(root.querySelector('#cfg-gitproxy-sshkey').closest('.cfg-row').hidden, true);
  assert.equal(root.querySelector('[data-cfg-hits="git"]').textContent, '1');
  await mounted.unmount();
});
