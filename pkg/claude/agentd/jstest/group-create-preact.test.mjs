import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const templates = [{
  name: 'builders',
  descr: 'template descr',
  default_context: 'template context',
  agents: [{ name: 'lead', role: 'builder', is_owner: true }],
  work_pattern: [{ send_to: 'all', value: 'start' }],
}];
const groups = [{
  name: 'alpha',
  descr: 'alpha descr',
  default_cwd: '/alpha',
  default_context: 'alpha context',
  attachment_url: 'https://linear.app/acme/project/alpha',
  attachment_label: 'Alpha project',
  attachment_label_override: 'Alpha project',
  environment: [{ name: 'TEAM', value: 'alpha' }, { name: 'CACHE', value: '/alpha/cache' }],
}, {
  name: 'beta',
  descr: 'beta descr',
  default_cwd: '/beta',
  default_context: 'beta context',
}];

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function flush(harness, turns = 6) {
  await harness.act(async () => {
    for (let turn = 0; turn < turns; turn += 1) await Promise.resolve();
  });
}

function choose(select, value) {
  for (const option of select.options) {
    if (option.value === value) option.setAttribute('selected', '');
    else option.removeAttribute('selected');
  }
  Object.defineProperty(select, 'value', {
    configurable: true, writable: true, value,
  });
}

test('group-create model preserves compatible prefill and clears stale source-owned fields', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/group-create-model.js');

  const blank = model.createGroupCreateDraft({ templates, groups });
  assert.deepEqual(blank, {
    origin: 'blank', template: '', name: '', source: '', nested: false,
    parent: '', descr: '', cwd: '',
    cloneGroup: '', cloneDefaultName: '', clonePlacement: null,
    clonePreset: false,
    withAgents: false, copyOwners: false,
    cwdOrigin: '', workspaceMode: 'existing', repository: '', cloneTransport: 'ssh',
    cloneDestination: '', attachRepository: true, context: '', task: '', maxMembers: '',
    attachmentURL: '', attachmentLabel: '',
    environment: [],
  });

  let draft = model.createGroupCreateDraft({
    templates, groups, presetTemplate: 'builders',
  });
  assert.equal(draft.descr, 'template descr');
  assert.equal(draft.context, 'template context');
  draft = model.selectGroupCreateSource(draft, 'alpha', { templates, groups });
  assert.equal(draft.descr, 'alpha descr');
  assert.equal(draft.cwd, '/alpha');
  assert.equal(draft.context,
    '## Mirrored group context\n\nalpha context\n\n## Template context\n\ntemplate context');
  assert.equal(draft.attachmentURL, 'https://linear.app/acme/project/alpha');
  assert.equal(draft.attachmentLabel, 'Alpha project');
  assert.deepEqual(draft.environment, groups[0].environment);
  draft = { ...draft, nested: true };
  draft = model.selectGroupCreateSource(draft, '', { templates, groups });
  assert.equal(draft.descr, 'template descr');
  assert.equal(draft.cwd, '', 'source-owned cwd cannot leak into top-level template mode');
  assert.equal(draft.context, 'template context');
  assert.equal(draft.attachmentURL, '');
  assert.deepEqual(draft.environment, []);
  assert.equal(draft.nested, false);

  const pinned = model.createGroupCreateDraft({
    templates, groups, presetTemplate: 'builders', parentGroup: 'alpha',
  });
  assert.equal(pinned.origin, 'template',
    'an explicit template preset stays visible even when placement is prefilled');
  assert.equal(pinned.descr, 'alpha descr');
  assert.equal(pinned.cwd, '/alpha');
  assert.equal(pinned.context,
    '## Mirrored group context\n\nalpha context\n\n## Template context\n\ntemplate context');
  assert.equal(pinned.attachmentURL, 'https://linear.app/acme/project/alpha');
  assert.deepEqual(pinned.environment, groups[0].environment);
  const pinnedBlank = model.selectGroupCreateTemplate(pinned, '', {
    templates, groups, parentGroup: 'alpha',
  });
  assert.equal(pinnedBlank.descr, '');
  assert.equal(pinnedBlank.cwd, '');
  assert.equal(pinnedBlank.context, '');
  assert.deepEqual(pinnedBlank.environment, []);
  assert.equal(pinnedBlank.parent, 'alpha', 'source changes do not alter placement');
});

test('group-create model validates and builds exact blank, template, and nested requests', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/group-create-model.js');
  const base = model.createGroupCreateDraft();
  assert.equal(model.validateGroupCreateDraft(base), 'name is required');
  assert.equal(model.validateGroupCreateDraft({ ...base, name: 'new', maxMembers: '-1' }),
    'max members must be a non-negative integer (0 = unlimited)');
  assert.equal(model.validateGroupCreateDraft({ ...base, name: 'new', maxMembers: '1x' }),
    'max members must be a non-negative integer (0 = unlimited)');
  assert.equal(model.validateGroupCreateDraft(
    { ...base, name: 'new', maxMembers: '-1' }, { templateMode: true }), '',
  'a hidden blank-group cap cannot block template instantiation');

  const blank = model.groupCreateRequest({
    ...base, name: '  new-group ', parent: 'alpha', descr: ' desc ', cwd: ' /repo ',
    context: ' context ', maxMembers: '4',
    environment: [{ name: ' TEAM ', value: 'alpha' }, { name: '', value: '' }],
    attachmentURL: ' https://linear.app/acme/project/alpha ',
    attachmentLabel: ' Alpha project ',
  }, null, 'alpha');
  assert.deepEqual(blank.body, {
    name: 'new-group', parent: 'alpha', descr: 'desc', default_cwd: '/repo',
    default_context: 'context', max_members: 4,
    environment: [{ name: 'TEAM', value: 'alpha' }],
    attachment_url: 'https://linear.app/acme/project/alpha',
    attachment_label: 'Alpha project',
  });

  const topLevel = model.groupCreateRequest({
    ...model.createGroupCreateDraft({ groups, parentGroup: 'alpha' }),
    name: 'moved-to-top', parent: '',
  }, null, 'alpha');
  assert.equal(topLevel.body.parent, '',
    'clearing an editable placement cannot fall back to the entry-point preset');

  const instantiated = model.groupCreateRequest({
    ...base, name: 'party', source: 'alpha', nested: true,
    descr: ' desc ', cwd: ' /repo ', context: ' context\n', task: ' ship\n',
    attachmentURL: 'https://linear.app/acme/project/alpha',
    attachmentLabel: 'Alpha project',
  }, templates[0]);
  assert.equal(instantiated.url, '/api/templates/builders/instantiate');
  assert.deepEqual(instantiated.body, {
    group_name: 'party', task: ' ship\n', cwd: '/repo', descr_override: 'desc',
    context_override: ' context\n', parent: 'alpha',
    environment: [],
    attachment_url: 'https://linear.app/acme/project/alpha',
    attachment_label: 'Alpha project',
  });

  const cloned = model.groupCreateRequest({
    ...base, name: 'cloned', workspaceMode: 'clone',
    repository: 'github.com/acme/repo', cloneTransport: 'https',
    cloneDestination: ' ~/git/repo ', attachRepository: true,
  }, null);
  assert.deepEqual(cloned.body, {
    name: 'cloned', parent: '', descr: '', default_cwd: '~/git/repo',
    default_context: '', environment: [], max_members: 0,
    repository_clone: {
      repository: 'github.com/acme/repo', transport: 'https',
      destination: '~/git/repo', attach: true,
    },
  });
  assert.equal(model.derivedCloneDestination('git@github.com:acme/payments.git'), '~/git/payments');

  const groupClone = model.groupCreateRequest({
    ...base, cloneGroup: 'alpha', cloneDefaultName: 'alpha-c-1',
    clonePlacement: { parent: 'root', anchor: 'alpha', before: true },
    parent: 'root',
    name: 'alpha-copy', withAgents: true, copyOwners: true,
  }, null);
  assert.deepEqual(groupClone, {
    kind: 'clone', name: 'alpha-copy', source: 'alpha',
    placement: { parent: 'root', anchor: 'alpha', before: true },
    withAgents: true, copyOwners: true,
    url: '/api/groups/alpha/clone',
    body: {
      no_clone_members: false, copy_owners: true,
      descr: '', default_cwd: '', default_context: '', max_members: 0,
      environment: [],
      attachment_url: '', attachment_label: '',
      new_name: 'alpha-copy', parent: 'root',
    },
  });
});

test('group-create state snapshots each open and invalidates closed generations', async (t) => {
  const harness = await createPreactHarness(t);
  const { createGroupCreateState } = await harness.importDashboardModule('js/group-create-state.js');
  let snapshot = { templates, groups };
  const state = createGroupCreateState({ getSnapshot: () => snapshot });
  state.open('builders', 'alpha');
  const first = state.dialog.value;
  assert.equal(first.presetTemplate, 'builders');
  assert.equal(first.parentGroup, 'alpha');
  assert.equal(state.isCurrent(first.generation), true);
  snapshot = { templates: [], groups: [] };
  assert.equal(first.templates.length, 1, 'an in-flight draft is not retargeted by poll mutation');
  state.close();
  assert.equal(state.isCurrent(first.generation), false);
  state.open();
  assert.equal(state.dialog.value.templates.length, 0, 'reopen starts from the latest snapshot');
  assert.notEqual(state.dialog.value.generation, first.generation);
  snapshot = { templates, groups: [...groups, { name: 'alpha-c-1' }] };
  state.openClone('alpha');
  assert.equal(state.dialog.value.cloneGroup, 'alpha');
  assert.equal(state.dialog.value.defaultName, 'alpha-c-2');
  assert.deepEqual(state.dialog.value.placement, {
    parent: '', anchor: 'alpha', before: false,
  });
});

test('group-create actions preserve HTTP errors, partial outcome toasts, expansion, and refresh', async (t) => {
  const harness = await createPreactHarness(t);
  const { createGroupCreateActions } = await harness.importDashboardModule('js/group-create-actions.js');
  const calls = [];
  let response = {
    ok: false, status: 403,
    text: async () => 'permission denied',
  };
  const actions = createGroupCreateActions({
    fetchImpl: async (url, options) => { calls.push(['fetch', url, options]); return response; },
    pickDirectory: async () => ({ canceled: true }),
    openTemplateManager: (options) => calls.push(['manager', options]),
    notify: (...args) => calls.push(['notify', ...args]),
    setExpanded: (name) => calls.push(['expanded', name]),
    recordInteraction: (name) => calls.push(['interaction', name]),
    refresh: () => calls.push(['refresh']),
  });
  const draft = { ...await (async () => {
    const model = await harness.importDashboardModule('js/group-create-model.js');
    return model.createGroupCreateDraft();
  })(), name: 'party' };
  await assert.rejects(() => actions.submit(draft, null), /permission denied/);

  response = {
    ok: true, status: 201,
    text: async () => JSON.stringify({
      spawned: 2, failed: 1, pattern_errors: ['owner missing'],
    }),
  };
  const result = await actions.submit(draft, templates[0]);
  actions.complete(result);
  assert.deepEqual(calls.filter(([kind]) => kind === 'notify'), [
    ['notify', 'group party: spawned 2, 1 failed — check the group', true],
    ['notify', '⚠ work pattern: 1 step not sent — owner missing', true],
  ]);
  assert.ok(calls.some((entry) => entry[0] === 'expanded' && entry[1] === 'party'));
  assert.ok(calls.some((entry) => entry[0] === 'interaction' && entry[1] === 'party'));
  assert.ok(calls.some((entry) => entry[0] === 'refresh'));
});

test('group-create actions remember clone transport in dashboard preferences', async (t) => {
  const harness = await createPreactHarness(t);
  const { createGroupCreateActions } = await harness.importDashboardModule('js/group-create-actions.js');
  const stored = new Map([['tclaude.dash.group-create.clone-transport', 'https']]);
  const actions = createGroupCreateActions({ prefs: {
    getItem: (key) => stored.get(key) ?? null,
    setItem: (key, value) => stored.set(key, value),
  } });
  assert.equal(actions.cloneTransport(), 'https');
  actions.rememberCloneTransport('ssh');
  assert.equal(stored.get('tclaude.dash.group-create.clone-transport'), 'ssh');
});

async function mountGroupCreate(
  t,
  actionOverrides = {},
  snapshot = { templates, groups },
  confirmDiscard = async () => false,
) {
  const harness = await createPreactHarness(t);
  const [{ GroupCreateApp }, { createGroupCreateState }] = await Promise.all([
    harness.importDashboardModule('js/group-create-island.js'),
    harness.importDashboardModule('js/group-create-state.js'),
  ]);
  const state = createGroupCreateState({ getSnapshot: () => snapshot });
  const calls = [];
  const actions = {
    submit: async () => ({ kind: 'blank', name: 'new-group', response: {} }),
    complete: (...args) => calls.push(['complete', ...args]),
    loadTemplates: async () => templates,
    pickDirectory: async () => ({ canceled: true }),
    openTemplateManager: (onClose) => calls.push(['manager', onClose]),
    cloneTransport: () => 'ssh',
    rememberCloneTransport: (value) => calls.push(['transport', value]),
    ...actionOverrides,
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${GroupCreateApp}
    state=${state} actions=${actions} confirmDiscard=${confirmDiscard}
    words=${(plain) => plain}
  />`, host);
  return { harness, host, state, actions, calls, cleanup: mounted.unmount };
}

test('Preact group-create owner renders preset/mirror/pinned paths and reconciles manager close', async (t) => {
  const mounted = await mountGroupCreate(t);
  const { harness, host, state, calls } = mounted;
  state.open('builders');
  await flush(harness);
  assert.ok(host.querySelector('#group-create-modal'));
  assert.equal(host.querySelector('#group-create-descr').value, 'template descr');
  assert.match(host.querySelector('#group-create-template-preview').textContent, /‹group›-lead/);
  assert.equal(harness.document.activeElement.id, 'group-create-name');

  const source = host.querySelector('#group-create-source');
  choose(source, 'alpha');
  await harness.act(() => harness.fireEvent(source, 'change'));
  assert.equal(host.querySelector('#group-create-cwd').value, '/alpha');
  assert.match(host.querySelector('#group-create-context').value, /Mirrored group context/);
  host.querySelector('#group-create-manage-templates').click();
  assert.equal(calls[0][0], 'manager');

  mounted.actions.loadTemplates = async () => [];
  calls[0][1]();
  await flush(harness);
  assert.equal(host.querySelector('#group-create-cwd').value, '',
    'deleted selection reconciles all source-owned fields to blank mode');
  assert.equal(host.querySelector('#group-create-task-row').hidden, true);

  state.close();
  state.open('builders', 'alpha');
  await flush(harness);
  assert.match(host.querySelector('#group-create-title').textContent, /Create group/);
  assert.equal(host.querySelector('.group-create-origin-options .selected').textContent, 'Template');
  assert.equal(host.querySelector('#group-create-placement').dataset.currentValue, 'alpha');
  assert.equal(host.querySelector('#group-create-descr').value, 'alpha descr');
  assert.equal(host.querySelector('#group-create-cwd').value, '/alpha');
  assert.equal(host.querySelector('#group-create-source-row').hidden, false);
  await mounted.cleanup();
});

test('subgroup entry metadata only prefills later Group and Template choices', async (t) => {
  const mounted = await mountGroupCreate(t);
  const { harness, host, state } = mounted;
  state.open('', 'alpha');
  await flush(harness);

  const groupSource = host.querySelector('#group-create-group-source');
  choose(groupSource, 'beta');
  await harness.act(() => harness.fireEvent(groupSource, 'change'));
  choose(host.querySelector('#group-create-placement'), '');
  await harness.act(() => harness.fireEvent(host.querySelector('#group-create-placement'), 'change'));

  [...host.querySelectorAll('.group-create-origin-options button')][2].click();
  await flush(harness);
  assert.match(host.querySelector('#group-create-source-summary').textContent, /Prefilled from beta/);
  assert.equal(host.querySelector('#group-create-cwd').value, '/beta');
  assert.equal(host.querySelector('#group-create-placement').dataset.currentValue, '');
  assert.equal(host.querySelector('#group-create-source-row').hidden, false,
    'the entry route cannot permanently pin template mirroring');
  await mounted.cleanup();
});

test('Preact group-create switches Blank, Group, and Template in one visible form', async (t) => {
  const mounted = await mountGroupCreate(t);
  const { harness, host, state } = mounted;
  state.open();
  await flush(harness);
  const form = host.querySelector('#group-create-modal');
  const origins = [...form.querySelectorAll('.group-create-origin-options button')];
  assert.deepEqual(origins.map((button) => button.textContent), ['Blank', 'Group', 'Template']);
  assert.equal(origins[0].getAttribute('aria-pressed'), 'true');
  assert.equal(form.querySelector('#group-create-group-source'), null);
  const nameNode = form.querySelector('#group-create-name');

  origins[1].click();
  await flush(harness);
  assert.equal(form.querySelector('#group-create-group-source').dataset.currentValue, 'alpha');
  assert.match(form.querySelector('#group-create-source-summary').textContent, /Alpha project/);
  assert.equal(form.querySelector('#group-create-name').isSameNode(nameNode),
  true, 'switching sources keeps the same form mounted');

  [...form.querySelectorAll('.group-create-origin-options button')][2].click();
  await flush(harness);
  assert.equal(form.querySelector('#group-create-template').dataset.currentValue, 'builders');
  assert.equal(form.querySelector('#group-create-group-source'), null);
  assert.equal(form.querySelector('#group-create-placement').dataset.currentValue, '');
  await mounted.cleanup();
});

test('Preact group-create clone workspace derives destination and remembers transport', async (t) => {
  const mounted = await mountGroupCreate(t);
  const { harness, host, state, calls } = mounted;
  state.open();
  await flush(harness);
  const buttons = [...host.querySelectorAll('.group-create-workspace-modes button')];
  buttons[1].click();
  await flush(harness);
  await harness.input(host.querySelector('#group-create-repository'), 'git@github.com:acme/payments.git');
  assert.equal(host.querySelector('#group-create-clone-destination').value, '~/git/payments');
  const transports = [...host.querySelectorAll('.group-create-clone-transport button')];
  transports[1].click();
  await flush(harness);
  assert.ok(calls.some(([kind, value]) => kind === 'transport' && value === 'https'));
  assert.equal(host.querySelector('#group-create-submit').textContent, 'Create group');
  await mounted.cleanup();
});

test('Preact group-create owns clone mode and makes inherited attachment visible', async (t) => {
  let submitted;
  const mounted = await mountGroupCreate(t, {
    submit: async (draft, template) => {
      submitted = { draft, template };
      return {
        kind: 'clone', name: draft.name, source: draft.cloneGroup,
        withAgents: draft.withAgents, copyOwners: draft.copyOwners,
        placement: draft.clonePlacement, response: { group: draft.name, members: [] },
      };
    },
  });
  const { harness, host, state } = mounted;
  state.openClone('alpha');
  await flush(harness);
  assert.match(host.querySelector('#group-create-title').textContent, /Create group/);
  assert.equal(host.querySelector('.group-create-origin-options .selected').textContent, 'Group');
  assert.equal(host.querySelector('#group-create-name').value, 'alpha-c-1');
  assert.equal(host.querySelector('details'), null, 'settings are never collapsed');
  assert.equal(host.querySelector('#group-create-descr').value, 'alpha descr');
  assert.equal(host.querySelector('#group-create-cwd').value, '/alpha');
  assert.equal(host.querySelector('#group-create-context').value, 'alpha context');
  assert.equal(host.querySelector('#group-create-attachment-url').value,
    'https://linear.app/acme/project/alpha');
  assert.equal(host.querySelector('#group-create-attachment-label').value, 'Alpha project');
  const environmentRows = [...host.querySelectorAll('#group-create-environment-row .sbx-environment-row')];
  assert.deepEqual(environmentRows.map((row) => [
    row.querySelector('.sbx-env-name').value,
    row.querySelector('.sbx-env-value').value,
  ]), [['TEAM', 'alpha'], ['CACHE', '/alpha/cache']]);
  await harness.input(host.querySelector('#group-create-context'), 'edited clone context');
  await harness.input(host.querySelector('#group-create-attachment-label'), 'Alpha tracker');
  await harness.input(environmentRows[0].querySelector('.sbx-env-value'), 'clone');
  assert.equal(host.querySelector('#group-create-name').hasAttribute('data-select-on-focus'), true);
  assert.match(host.querySelector('#group-create-source-summary').textContent, /Alpha project/);
  assert.match(host.querySelector('#group-create-source-summary').textContent, /attachment \/ link/);
  assert.equal(host.querySelector('#group-create-template'), null);
  const withAgents = host.querySelector('#group-create-with-agents');
  const copyOwners = host.querySelector('#group-create-copy-owners');
  withAgents.checked = true;
  copyOwners.checked = true;
  await harness.act(() => {
    harness.fireEvent(withAgents, 'change');
    harness.fireEvent(copyOwners, 'change');
  });
  host.querySelector('#group-create-submit').click();
  await flush(harness);
  assert.equal(submitted.template, null);
  assert.equal(submitted.draft.cloneGroup, 'alpha');
  assert.equal(submitted.draft.context, 'edited clone context');
  assert.equal(submitted.draft.attachmentLabel, 'Alpha tracker');
  assert.deepEqual(submitted.draft.environment, [
    { name: 'TEAM', value: 'clone' }, { name: 'CACHE', value: '/alpha/cache' },
  ]);
  assert.equal(submitted.draft.withAgents, true);
  assert.equal(submitted.draft.copyOwners, true);
  assertAbsent(host.querySelector('#group-create-modal'));
  await mounted.cleanup();
});

test('leaving a clone preset does not silently restore clone semantics', async (t) => {
  const mounted = await mountGroupCreate(t);
  const { harness, host, state } = mounted;
  state.openClone('alpha');
  await flush(harness);

  let origins = [...host.querySelectorAll('.group-create-origin-options button')];
  origins[0].click();
  await flush(harness);
  origins = [...host.querySelectorAll('.group-create-origin-options button')];
  origins[1].click();
  await flush(harness);

  assert.equal(host.querySelector('#group-create-with-agents'), null);
  const model = await harness.importDashboardModule('js/group-create-model.js');
  const groupSource = host.querySelector('#group-create-group-source');
  choose(groupSource, 'beta');
  await harness.act(() => harness.fireEvent(groupSource, 'change'));
  await harness.input(host.querySelector('#group-create-name'), 'beta-prefill');

  let submitted;
  mounted.actions.submit = async (draft, template) => {
    submitted = model.groupCreateRequest(draft, template);
    return { kind: 'blank', name: draft.name, response: {} };
  };
  host.querySelector('#group-create-submit').click();
  await flush(harness);
  assert.equal(submitted.kind, 'blank');
  assert.equal(submitted.body.descr, 'beta descr');
  await mounted.cleanup();
});

test('Preact group-create synchronously blocks duplicate submit, blocks busy close, and retries errors', async (t) => {
  const first = deferred();
  let attempts = 0;
  const mounted = await mountGroupCreate(t, {
    submit: async () => {
      attempts += 1;
      if (attempts === 1) return first.promise;
      return { kind: 'blank', name: 'retry', response: {} };
    },
  });
  const { harness, host, state } = mounted;
  state.open();
  await flush(harness);
  await harness.input(host.querySelector('#group-create-name'), 'retry');
  const submit = host.querySelector('#group-create-submit');
  submit.click();
  submit.click();
  assert.equal(attempts, 1, 'the synchronous ref lock wins before state publication');
  host.querySelector('#group-create-cancel').click();
  await flush(harness);
  assert.ok(host.querySelector('#group-create-modal'), 'busy request cannot be dismissed');
  first.reject(new Error('temporary failure'));
  await flush(harness);
  assert.match(host.querySelector('#group-create-error').textContent, /temporary failure/);
  assert.equal(host.querySelector('#group-create-submit').disabled, false);
  host.querySelector('#group-create-submit').click();
  await flush(harness);
  assert.equal(attempts, 2);
  assertAbsent(host.querySelector('#group-create-modal'));
  await mounted.cleanup();
});

test('Preact group-create guards directory and template returns across close/reopen generations', async (t) => {
  const directory = deferred();
  const rescan = deferred();
  let managerClose;
  const mounted = await mountGroupCreate(t, {
    pickDirectory: async () => directory.promise,
    loadTemplates: async () => rescan.promise,
    openTemplateManager: (onClose) => { managerClose = onClose; },
  });
  const { harness, host, state } = mounted;
  state.open('builders');
  await flush(harness);
  host.querySelector('#group-create-cwd-browse').click();
  host.querySelector('#group-create-manage-templates').click();
  managerClose();
  state.close();
  state.open();
  await flush(harness);
  directory.resolve({ path: '/stale' });
  rescan.resolve([]);
  await flush(harness);
  assert.equal(host.querySelector('#group-create-cwd').value, '');
  assert.equal(host.querySelector('#group-create-task-row').hidden, true,
    'old manager rescan cannot change the reopened generation');
  await mounted.cleanup();
});

test('Preact group-create ignores composing Enter and submits a plain field Enter', async (t) => {
  let attempts = 0;
  const mounted = await mountGroupCreate(t, {
    submit: async () => {
      attempts += 1;
      return { kind: 'blank', name: 'ime', response: {} };
    },
  });
  const { harness, host, state } = mounted;
  state.open();
  await flush(harness);
  const name = host.querySelector('#group-create-name');
  await harness.input(name, 'ime');
  harness.fireEvent(name, 'keydown', { key: 'Enter', isComposing: true, keyCode: 229 });
  await flush(harness);
  assert.equal(attempts, 0);
  harness.fireEvent(name, 'keydown', { key: 'Enter', isComposing: false, keyCode: 13 });
  await flush(harness);
  assert.equal(attempts, 1);
  await mounted.cleanup();
});

test('dirty group-create Cancel, backdrop, and Escape all honor rejected and accepted discard confirmation', async (t) => {
  let allowDiscard = false;
  let confirmations = 0;
  const mounted = await mountGroupCreate(t, {}, { templates, groups }, async () => {
    confirmations += 1;
    return allowDiscard;
  });
  const { harness, host, state } = mounted;

  const openDirty = async () => {
    state.open();
    await flush(harness);
    await harness.input(host.querySelector('#group-create-name'), 'dirty draft');
  };
  const expectRejectedThenAccepted = async (dismiss) => {
    allowDiscard = false;
    await dismiss();
    await flush(harness);
    assert.ok(host.querySelector('#group-create-modal'), 'rejected discard keeps the draft mounted');
    allowDiscard = true;
    await dismiss();
    await flush(harness);
    assertAbsent(host.querySelector('#group-create-modal'), 'accepted discard closes the draft');
  };

  await openDirty();
  await expectRejectedThenAccepted(async () => {
    host.querySelector('#group-create-cancel').click();
  });

  await openDirty();
  await expectRejectedThenAccepted(async () => {
    harness.fireEvent(host.querySelector('#group-create-modal'), 'mousedown');
  });

  await openDirty();
  await expectRejectedThenAccepted(async () => {
    harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  });

  assert.equal(confirmations, 6, 'every dirty dismissal routes through the shared confirmation');
  await mounted.cleanup();
});
