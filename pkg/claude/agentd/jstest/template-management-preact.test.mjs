import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';
import { comboKeys, comboPick } from './combobox-helpers.mjs';

test('template model preserves the complete replace payload and stale references', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule(
    'js/template-management-model.js',
  );
  const original = {
    name: 'force',
    per_agent_worktrees: true,
    wave_max_wait: 30,
    agents: [
      {
        name: 'dev',
        permissions: ['read'],
        role_ref: 'missing-role',
        spawn_profile: 'missing-profile',
        profile_inline: { model: 'custom' },
        wave: 2,
      },
    ],
    work_pattern: [{ send_to: 'dev', value: 'build {{task}}' }],
    process: [{ name: 'build', roles: [' dev ', ''], criteria: 'done' }],
    rhythms: [{ name: 'ping', interval: '10m', body: 'status' }],
  };
  const draft = model.templateDraft(original);
  assert.notEqual(draft.agents, original.agents);
  draft.name = ' force-2 ';
  draft.agents[0].name = ' dev2 ';
  const payload = model.templatePayload(draft);
  assert.equal(payload.name, 'force-2');
  assert.equal(payload.agents[0].name, 'dev2');
  assert.deepEqual(payload.agents[0].permissions, ['read']);
  assert.equal(payload.agents[0].profile_inline.model, 'custom');
  assert.deepEqual(payload.process[0].roles, ['dev']);
  assert.equal(payload.wave_max_wait, 30);
  assert.deepEqual(model.moveItem(['a', 'b'], 1, -1), ['b', 'a']);
});

// Saving is a full replace, so a field the editor does not show still has to
// go back as it came: a template's owner scopes (TCL-1071) survive an edit, and
// a template without them sends none (tcl-cnua7d).
test('template model sends back the owner scopes it was given, and none it was not', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/template-management-model.js');
  const owner_scopes = { 'groups.members.spawn': { spawn_profile: ['p1'] } };
  const scoped = { name: 'crew', agents: [{ name: 'lead' }], owner_scopes };
  const draft = model.templateDraft(scoped);
  draft.descr = 'edited';
  const payload = model.templatePayload(draft);
  assert.deepEqual(payload.owner_scopes, owner_scopes);
  assert.notEqual(payload.owner_scopes, owner_scopes, 'a copy, not the snapshot\'s own object');
  assert.equal('owner_scopes' in model.templatePayload(model.templateDraft({ name: 'plain', agents: [{ name: 'lead' }] })), false);
  assert.equal('owner_scopes' in model.templatePayload(model.templateDraft()), false, 'a new template has none');
});

test('template actions keep starter requests ordered and preserve mutation payloads', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }] =
    await Promise.all([
      harness.importDashboardModule('js/management-state.js'),
      harness.importDashboardModule('js/management-actions.js'),
    ]);
  const pending = [];
  const saves = [];
  let refreshes = 0;
  const templateAPI = {
    loadStarters: () => new Promise((resolve) => pending.push(resolve)),
    async saveTemplate(original, payload) {
      saves.push([original, payload]);
      return {};
    },
  };
  const state = createManagementState();
  state.updateTemplates([{ name: 'force', agents: [] }], [{ name: 'team' }]);
  const actions = createManagementActions({
    state,
    confirm: async () => true,
    notify() {},
    refresh: async () => {
      refreshes += 1;
    },
    getSnapshot: () => ({
      templates: state.templates.value,
      groups: state.templateGroups.value,
    }),
    templateAPI,
  });
  const first = actions.openTemplateStarters();
  const second = actions.openTemplateStarters();
  pending[1]([{ name: 'newer' }]);
  await second;
  pending[0]([{ name: 'stale' }]);
  await first;
  assert.equal(
    state.dialog.value.request.data[0].name,
    'newer',
    'a stale starter response cannot replace the latest request',
  );
  await actions.duplicateTemplate(
    { name: 'force', created_at: 'old', updated_at: 'old', agents: [] },
    'force-copy',
  );
  assert.equal(saves[0][0], '');
  assert.equal(saves[0][1].name, 'force-copy');
  assert.equal('created_at' in saves[0][1], false);
  assert.equal('updated_at' in saves[0][1], false);
  assert.equal(refreshes, 1);
});

test('template manager and editor retain native markup, wizard variants, nested draft state, and stacking', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] =
    await Promise.all([
      harness.importDashboardModule('js/management-state.js'),
      harness.importDashboardModule('js/management-island.js'),
    ]);
  const template = {
    name: 'force',
    descr: 'ship it',
    agents: [
      {
        name: 'dev',
        role: 'builder',
        role_ref: 'reviewer',
        spawn_profile: 'fast',
        wave: 0,
      },
    ],
    work_pattern: [],
    process: [],
    rhythms: [],
  };
  const state = createManagementState();
  state.updateTemplates(
    [template],
    [{ name: 'live-force', source_template: 'force', mission: 'release\nnow' }],
  );
  state.profiles.value = [{ name: 'fast', model: 'sonnet' }];
  state.roles.value = [
    {
      name: 'reviewer',
      brief: 'Review carefully',
      permissions: ['read'],
    },
  ];
  state.openTemplateManager();
  let saved = null;
  let profileManagerOpened = false;
  let discard = false;
  let scribeResult = null;
  const scribeAsked = [];
  const actions = {
    openTemplateEditor(seed = null, options = {}) {
      state.openTemplateDialog({ kind: 'template-editor', seed, options });
    },
    openTemplateDeploy() {},
    openTemplateDuplicate() {},
    exportTemplate() {},
    removeTemplate() {},
    openTemplateFromGroup() {},
    openTemplateImport() {},
    openTemplateStarters() {},
    editTemplatesWithAgent() {},
    openManager(kind) {
      profileManagerOpened = kind === 'profiles';
      state.openManager(kind);
    },
    openProfileEditor() {},
    async saveTemplate(value) {
      saved = value;
    },
    editTemplateWithAgent(name) { scribeAsked.push(name); return scribeResult; },
  };
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  mountManagementIsland({
    host,
    state,
    actions,
    confirm: async () => true,
    confirmDiscard: async () => discard,
    openProfilePermissions() {},
    registerCleanup(fn) {
      cleanups.push(fn);
    },
  });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelectorAll('#templates-manage-modal').length, 1);
  assert.equal(
    host.querySelector('.tc-force').dataset.forceGroup,
    'live-force',
  );
  assert.equal(
    host.querySelectorAll('.tpl-word-wizard').length > 0,
    true,
    'wizard vocabulary is emitted alongside plain copy',
  );
  host.querySelector('[data-tact="edit"]').click();
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#template-editor-name').type, 'text');
  assertSameNode(
    harness.document.activeElement,
    host.querySelector('#template-editor-name'),
    'the editor autofocuses its declared initial field',
  );
  assert.ok(host.querySelector('.ta-role-ref').classList.contains('choice-combobox'));
  assert.ok(host.querySelector('.ta-profile-select').classList.contains('choice-combobox'));
  assert.match(
    host.querySelector('.ta-role-inspect').textContent,
    /Review carefully/,
  );
  assert.match(host.querySelector('.ta-role-inspect').textContent, /grantsread/);
  assert.equal(
    host.querySelector('#template-editor-wave-max-wait').type,
    'number',
  );
  host.querySelector('#template-editor-add-agent').click();
  host.querySelector('#template-editor-add-pattern').click();
  host.querySelector('#template-editor-add-phase').click();
  host.querySelector('#template-editor-add-rhythm').click();
  await harness.act(() => Promise.resolve());
  assert.equal(
    host.querySelectorAll('#template-editor-agents .template-agent-row').length,
    2,
  );
  assert.equal(host.querySelectorAll('.template-pattern-row').length, 1);
  assert.equal(host.querySelectorAll('.template-process-row').length, 1);
  assert.equal(host.querySelectorAll('.template-rhythm-row').length, 1);
  const name = host.querySelector('#template-editor-name');
  name.value = 'force-2';
  name.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('.ta-profile-manage').click();
  await harness.act(() => Promise.resolve());
  assert.equal(profileManagerOpened, true);
  assert.ok(
    host.querySelector('#template-editor-modal'),
    'opening a nested manager keeps the template draft mounted',
  );
  assert.ok(host.querySelector('#profiles-manage-modal'));
  const nestedFilter = host.querySelector('#filter-profiles');
  nestedFilter.focus();
  const tab = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(tab, 'key', { value: 'Tab' });
  harness.document.dispatchEvent(tab);
  assertSameNode(
    harness.document.activeElement,
    nestedFilter,
    'only the topmost overlay owns the Tab trap',
  );
  state.closeManager();
  await harness.act(() => Promise.resolve());
  const escape = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(escape, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(escape);
  await harness.act(() => Promise.resolve());
  assert.ok(
    host.querySelector('#template-editor-modal'),
    'discard rejection keeps the dirty editor open',
  );
  assert.ok(
    host.querySelector('#templates-manage-modal'),
    'Escape does not close the underlying manager',
  );
  state.openDialog({
    kind: 'template-starters',
    request: { phase: 'loading', data: [], error: '' },
  });
  await harness.act(() => Promise.resolve());
  assert.match(
    host.querySelector('#starters-list').textContent,
    /Loading starters/,
  );
  state.openDialog({
    kind: 'template-starters',
    request: { phase: 'error', data: [], error: 'catalog unavailable' },
  });
  await harness.act(() => Promise.resolve());
  assert.match(
    host.querySelector('#starters-error').textContent,
    /catalog unavailable/,
    'a loading-to-error request transition stays visible',
  );
  state.closeDialog();
  host.querySelector('#template-editor-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(saved.payload.name, 'force-2');
  assert.equal(saved.payload.agents.length, 2);
  // Edit with agent keeps the editor until the scribe's window opened: a
  // declined grant leaves it as it was.
  discard = true;
  const settle = async () => { for (let i = 0; i < 3; i++) await harness.act(() => Promise.resolve()); };
  host.querySelector('#scribe-editor-open').click();
  await settle();
  assert.deepEqual(scribeAsked, ['force'], 'the stored circle the scribe edits');
  assert.ok(host.querySelector('#template-editor-modal'), 'a declined summon keeps the editor');
  scribeResult = { agentId: 'agt_circle', opened: 'chat' };
  host.querySelector('#scribe-editor-open').click();
  await settle();
  assertAbsent(host.querySelector('#template-editor-modal'), 'the editor steps aside once the chat window opened');
  cleanups.reverse().forEach((fn) => fn());
});

test('deploy and group dialogs preserve native controls, collision preview, and mode payloads', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }] =
    await Promise.all([
      harness.importDashboardModule('js/management-state.js'),
      harness.importDashboardModule('js/management-island.js'),
    ]);
  const template = {
    name: 'force',
    per_agent_worktrees: false,
    default_context: 'template lore',
    agents: [{ name: 'dev' }],
  };
  const alternate = {
    ...template,
    name: 'review-force',
    default_context: 'review lore',
  };
  const group = {
    name: 'team',
    descr: 'source',
    default_context: 'group lore',
    default_cwd: '/repo',
    default_profile: 'fast',
    attachment_url: 'https://linear.app/acme/project/team',
    attachment_label: 'Team project',
    attachment_label_override: 'Team project',
    members: [{ name: 'lead', owner: true, online: true }],
  };
  const staleProfileGroup = {
    ...group,
    name: 'stale-team',
    default_profile: 'deleted-profile',
  };
  const state = createManagementState();
  state.updateTemplates([template, alternate], [group, staleProfileGroup]);
  state.profiles.value = [
    { name: 'fast', is_owner: true, permission_overrides: { read: 'grant' } },
  ];
  state.openDialog({
    kind: 'template-deploy',
    presetName: 'force',
    dropGroup: '',
  });
  const deployed = [];
  const actions = {
    async loadDeployWorktrees(repo) {
      const changedRepo = repo === '/repo-b';
      return {
        is_repo: true,
        repo_root: repo,
        has_commits: true,
        worktrees: [{ path: repo, branch: 'main', is_main: true }],
        branches: changedRepo ? ['main', 'hotfix'] : ['main', 'release'],
        default_branch: 'main',
      };
    },
    async createDeployWorktree() {
      return { path: '/repo/.worktrees/force', branch: 'force' };
    },
    async deployTemplate(...args) {
      deployed.push(args);
      state.closeDialog();
      return {};
    },
  };
  const cleanups = [];
  const host = harness.document.createElement('div');
  harness.document.body.appendChild(host);
  mountManagementIsland({
    host,
    state,
    actions,
    confirm: async () => true,
    confirmDiscard: async () => true,
    openProfilePermissions() {},
    registerCleanup(fn) {
      cleanups.push(fn);
    },
  });
  await harness.act(() => Promise.resolve());
  assertSameNode(
    harness.document.activeElement,
    host.querySelector('#template-deploy-mission'),
    'the deploy dialog honors mission autofocus instead of the first control',
  );
  assert.ok(host.querySelector('#template-deploy-template').classList.contains('choice-combobox'));
  assert.ok(host.querySelector('#template-deploy-default-profile').classList.contains('choice-combobox'));
  assert.equal(
    host.querySelector('#template-deploy-wt-per-agent').type,
    'checkbox',
  );
  assert.match(
    host.querySelector('#template-deploy-preview').textContent,
    /force-dev|dev/,
  );
  await comboPick(harness, host.querySelector('#template-deploy-default-profile'), 'fast');
  await harness.act(() => Promise.resolve());
  assert.match(
    host.querySelector('#template-deploy-preview').textContent,
    /owner/,
  );
  await comboPick(harness, host.querySelector('#template-deploy-source'), 'team');
  await comboPick(harness, host.querySelector('#template-deploy-template'), 'review-force');
  await harness.act(() => Promise.resolve());
  assert.equal(
    host.querySelector('#template-deploy-group').value,
    'review-force',
    'an untouched group suggestion follows the selected template',
  );
  assert.match(
    host.querySelector('#template-deploy-context').value,
    /group lore[\s\S]*review lore/,
    'mirrored context follows the selected template',
  );
  const mission = host.querySelector('#template-deploy-mission');
  mission.value = 'Ship the release';
  mission.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.equal(
    host.querySelector('#template-deploy-group').value,
    'ship-the-release',
  );
  host.querySelector('#template-deploy-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(deployed[0][1], 'deploy');
  assert.equal(deployed[0][2].group_name, 'ship-the-release');
  assert.equal(deployed[0][2].attachment_url, 'https://linear.app/acme/project/team');
  assert.equal(deployed[0][2].attachment_label, 'Team project');

  state.openDialog({
    kind: 'template-deploy',
    presetName: 'force',
    dropGroup: 'team',
  });
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#template-deploy-mode'));
  assert.equal(
    host.querySelector('#template-deploy-submit').textContent,
    'Create subgroup',
  );
  assert.match(
    host.querySelector('#template-deploy-context').value,
    /group lore[\s\S]*template lore/,
  );
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  await comboPick(harness, host.querySelector('#template-deploy-worktree'), '__new__');
  await comboPick(harness, host.querySelector('#template-deploy-wt-base'), 'release');
  const reinforce = host.querySelector(
    'input[name="template-deploy-mode"][value="reinforce"]',
  );
  reinforce.checked = true;
  reinforce.dispatchEvent(
    new harness.window.Event('change', { bubbles: true }),
  );
  await harness.act(() => Promise.resolve());
  assert.ok(
    host.querySelector('#template-deploy-group').classList.contains('locked'),
  );
  const subgroup = host.querySelector(
    'input[name="template-deploy-mode"][value="subgroup"]',
  );
  subgroup.checked = true;
  subgroup.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.equal(
    host.querySelector('#template-deploy-submit').disabled,
    true,
    'a worktree-backed submit waits for metadata to reload',
  );
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  assert.equal(
    host.querySelector('#template-deploy-wt-base').dataset.value,
    'release',
    'a reinforce round-trip preserves the manually selected base branch',
  );
  const repo = host.querySelector('#template-deploy-wt-repo');
  repo.value = '/repo-b';
  repo.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  const staleBase = host.querySelector('#template-deploy-wt-base');
  assert.equal(staleBase.closest('[hidden]') !== null, true);
  assert.equal((await comboKeys(harness, staleBase)).includes('release'), false,
    'changing repositories immediately removes stale branch choices');
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  const reloadedBase = host.querySelector('#template-deploy-wt-base');
  const reloadedKeys = await comboKeys(harness, reloadedBase);
  assert.ok(reloadedKeys.includes('main'), 'a changed repository loads its own default branch');
  assert.equal(reloadedKeys.includes('release'), false,
    'branches from the previous repository do not leak into the new picker');
  reinforce.checked = true;
  reinforce.dispatchEvent(
    new harness.window.Event('change', { bubbles: true }),
  );
  await harness.act(() => Promise.resolve());
  host.querySelector('#template-deploy-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(deployed[1][1], 'reinforce');
  assert.equal(deployed[1][2].group_name, 'team');

  state.openDialog({
    kind: 'template-deploy',
    presetName: 'force',
    dropGroup: 'stale-team',
  });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#template-deploy-default-profile').dataset.value, '');
  assert.match(
    host.querySelector('#template-deploy-default-profile-input').value,
    /none/,
    'a deleted group default degrades to the visible none choice',
  );
  const staleReinforce = host.querySelector(
    'input[name="template-deploy-mode"][value="reinforce"]',
  );
  staleReinforce.checked = true;
  staleReinforce.dispatchEvent(
    new harness.window.Event('change', { bubbles: true }),
  );
  await harness.act(() => Promise.resolve());
  host.querySelector('#template-deploy-submit').click();
  await harness.act(() => Promise.resolve());
  assert.equal(
    'agent_profiles' in deployed[2][2],
    false,
    'a deleted profile is never submitted invisibly',
  );

  state.openDialog({ kind: 'group-context', group: 'team', context: 'old' });
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#group-context-text').value, 'old');
  assert.ok(host.querySelector('.tpl-word-wizard'));
  cleanups.reverse().forEach((fn) => fn());
});

test("another node's group saves as a template on that node and opens no editor here", async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { createManagementActions }, nodeAPI] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-actions.js'),
    harness.importDashboardModule('js/node-api.js'),
  ]);
  const node = 'inst_abcdefghijklmnop';
  t.after(nodeAPI.setNodeSnapshotSource((id) => (id === node
    ? { groups: [{ name: 'crew' }], templates: [{ name: 'crew-tpl' }] } : null)));
  const calls = [];
  let refreshes = 0;
  const state = createManagementState();
  state.updateTemplates([{ name: 'local-tpl', agents: [] }], []);
  const actions = createManagementActions({
    state, confirm: async () => true, notify() {}, refresh: async () => { refreshes += 1; },
    getSnapshot: () => ({ groups: [{ name: 'here' }], templates: state.templates.value }),
    templateAPI: { async fromGroup(body, target) { calls.push([body, target]); return { name: body.template_name, agents: [] }; } },
  });
  actions.openTemplateFromGroup('crew', node);
  assert.deepEqual(state.dialog.value.groups, ['crew'], "the node's groups, not the page's");
  assert.deepEqual(state.dialog.value.templates, [{ name: 'crew-tpl' }]);
  await actions.snapshotTemplateFromGroup('crew', 'crew-tpl', true, node);
  assert.deepEqual(calls, [[{ group: 'crew', template_name: 'crew-tpl', update: true }, node]]);
  assert.equal(state.templateDialog.value, null, "this node's editor would save here");
  assert.equal(refreshes, 0);
  actions.openTemplateFromGroup('here');
  assert.deepEqual(state.dialog.value.groups, ['here']);
  assert.equal(state.dialog.value.node, undefined);
});
