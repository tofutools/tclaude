import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const SNAPSHOT = {
  groups: [{ name: 'team', members: [] }, { name: 'ops', members: [] }],
  harnesses: [
    { name: 'claude', display_name: 'Claude Code', models: ['claude-opus-5-5'] },
    { name: 'codex', display_name: 'Codex', models: ['gpt-5.5'] },
    { name: 'shell', display_name: 'Shell', models: [] },
  ],
};

async function mountManagement(t, actions, descriptor) {
  const harness = await createPreactHarness(t);
  const [{ createManagementState }, { mountManagementIsland }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/management-state.js'),
    harness.importDashboardModule('js/management-island.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  dashboardState.publishLocalEdit(SNAPSHOT);
  const state = createManagementState();
  state.openDialog(descriptor);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  await harness.act(() => mountManagementIsland({
    host, state, actions,
    confirm: async () => true, confirmDiscard: async () => true, openProfilePermissions() {},
    registerCleanup: (fn) => cleanups.push(fn),
  }));
  const settle = () => harness.act(() => new Promise((resolve) => setTimeout(resolve, 400)));
  const choose = async (select, value) => {
    [...select.options].forEach((option) => option.removeAttribute('selected'));
    [...select.options].find((option) => option.value === value).setAttribute('selected', '');
    await harness.act(() => harness.fireEvent(select, 'change'));
  };
  const tick = async (input, checked) => {
    input.checked = checked;
    await harness.act(() => harness.fireEvent(input, 'change'));
  };
  // The harness tears the DOM down in its own after hook; unmount first when
  // it is still there.
  t.after(() => { if (globalThis.document) cleanups.forEach((fn) => fn()); });
  return { harness, host, state, settle, choose, tick };
}

test('PAF model builds the export, landing and query shapes the routes take', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/paf-model.js');
  assert.equal(model.queryString(model.exportParams({ agent: 'agt_1', native: false })),
    '?agent=agt_1&format=paf&history=true&no_native=true');
  assert.equal(model.queryString(model.exportParams({ agent: 'agt_1', history: false })), '?agent=agt_1&format=paf');
  assert.equal(model.queryString(model.landingParams({ harness: 'codex', members: { c1: { harness: 'claude', model: 'claude-opus-5-5' }, c2: { harness: '' } } })),
    '?harness=codex&member_harness=c1%3Dclaude&member_model=c1%3Dclaude-opus-5-5');
  assert.equal(model.queryString(model.exportParams({ agent: 'agt_1', history: false, workflows: true })), '?agent=agt_1&format=paf&workflows=own');
  assert.equal(model.rungBadge('none').text, 'config only');
  assert.match(model.lossSummary({ source_entries: 412, carried_entries: 371, items: [{ kind: 'provider_reasoning', count: 41 }] }),
    /carries 371 \/ 412 entries · losses: 41 provider reasoning/);
  assert.deepEqual(model.spawnableHarnesses(SNAPSHOT).map((h) => h.name), ['claude', 'codex'], 'shell cannot host an import');
});

test('a flagged agent export shows the findings and downloads only after acknowledging or redacting', async (t) => {
  const calls = [];
  const exportBlob = { size: 10 };
  const actions = {
    async exportAgentPAF(choices) {
      calls.push(['export', choices]);
      if (!choices.allowFlagged) {
        const error = new Error('suspected credentials');
        error.status = 422;
        error.body = { code: 'flagged_credentials', findings: [{ kind: 'github_token', count: 1, locations: ['history line 3'] }] };
        throw error;
      }
      return exportBlob;
    },
    async redactPAF(blob, opts, preview) {
      calls.push(['redact', blob === exportBlob, opts, preview]);
      return preview ? { redactions: [{ kind: 'mask_flagged', count: 1 }], findings: [] } : { size: 5 };
    },
  };
  const { harness, host, tick, settle } = await mountManagement(t, actions, { kind: 'paf-export', agent: 'agt_1', label: 'reviewer' });
  const URLs = [];
  harness.window.URL.createObjectURL = () => { URLs.push('blob:x'); return 'blob:x'; };
  harness.window.URL.revokeObjectURL = () => {};
  globalThis.URL.createObjectURL = harness.window.URL.createObjectURL;
  globalThis.URL.revokeObjectURL = () => {};
  assert.match(host.querySelector('#paf-export-title').textContent, /Export reviewer as PAF/);
  await tick(host.querySelector('#paf-export-native'), false);
  await harness.act(() => host.querySelector('#paf-export-download').click());
  await settle();
  assert.deepEqual(calls[0], ['export', { agent: 'agt_1', history: true, native: false, neutral: true, allowFlagged: false, workflows: false }]);
  assert.match(host.querySelector('#paf-findings').textContent, /github_token \(history line 3\)/);
  assert.equal(host.querySelector('#paf-export-download').disabled, true, 'download waits for the acknowledgement');
  await harness.act(() => host.querySelector('#paf-export-redact').click());
  assert.equal(host.querySelector('#paf-redact-download').disabled, true, 'redacted download waits for a preview');
  await harness.act(() => host.querySelector('#paf-redact-preview').click());
  await settle();
  assert.equal(calls.at(-1)[0], 'redact');
  assert.equal(calls.at(-1)[1], true, 'redacts the flagged export');
  assert.deepEqual(calls.at(-1)[2], { mask_flagged: true, paths_to_placeholders: true, stub_tool_results: false, drop_reasoning: false, drop_media: false });
  assert.equal(calls.at(-1)[3], true);
  assert.match(host.querySelector('#paf-redact-report').textContent, /mask flagged: 1[\s\S]*No suspected credentials remain/);
  await harness.act(() => host.querySelector('#paf-redact-download').click());
  await settle();
  assert.equal(calls.at(-1)[3], false);
  assert.equal(URLs.length, 1, 'the redacted copy was downloaded');
});

test('the import dialog previews an agent file that is not a group, then imports and starts it', async (t) => {
  const calls = [];
  const preview = {
    history: true,
    agent: { name: 'reviewer', harness: 'claude', origin: { instance: 'inst_lab', model: 'claude-opus-5-5' }, paths: { cwd: '/home/me/app' } },
    security: 'Permissions and ownership are advisory only; none will be copied.',
    warnings: [], unresolved: [{ name: 'CWD' }],
    history_landing: { source_harness: 'claude', harness: 'codex', model: 'gpt-5.5', rung: 'translated', losses: { source_entries: 10, carried_entries: 9, items: [] } },
  };
  const actions = {
    async inspectGroupImport() { throw new Error('a group import needs a group PAF'); },
    async importAgentPAF(file, params) { calls.push(params); return preview; },
  };
  const file = { name: 'reviewer.paf' };
  const { host, settle, choose, tick } = await mountManagement(t, actions, { kind: 'group-import', file });
  await settle();
  assert.match(host.querySelector('#group-import-title').textContent, /Import an agent/);
  assertAbsent(host.querySelector('#group-import-into'), 'group-only fields are hidden for an agent');
  assert.match(host.querySelector('#agent-import-preview').textContent, /From node\s*inst_lab/);
  assert.match(host.querySelector('#paf-history-landing').textContent, /translated/);
  assert.match(host.querySelector('#agent-import-unresolved').textContent, /1 recorded path/);
  const submit = host.querySelector('#group-import-submit');
  assert.equal(submit.textContent.trim(), 'Import (offline)');
  assert.equal(host.querySelector('#agent-import-start').textContent.trim(), 'Import & start');
  assert.equal(submit.disabled, true, 'needs a group and a working dir');
  assert.equal(host.querySelector('#agent-import-start').disabled, true);
  await choose(host.querySelector('#agent-import-group'), 'team');
  await tick(host.querySelector('#agent-import-keep-paths'), true);
  await settle();
  assert.equal(host.querySelector('#group-import-submit').disabled, false);
  host.querySelector('#group-import-submit').click();
  await settle();
  const applied = calls.at(-1);
  assert.equal(applied.apply, true);
  assert.equal(applied.group, 'team');
  assert.equal(applied.keep_paths, true);
  assert.equal(applied.start, 'false', 'the default lands it offline');
  host.querySelector('#agent-import-start').click();
  await settle();
  assert.equal(calls.at(-1).apply, true);
  assert.equal(calls.at(-1).start, '', 'Import & start starts it');
});

test('offline import is offered only when a conversation lands', async (t) => {
  const harness = await createPreactHarness(t);
  const { offlineImportBlocker, agentImportParams } = await harness.importDashboardModule('js/paf-dialogs.js');
  const base = { history: true, agent: { harness: 'claude' }, history_landing: { harness: 'claude' } };
  assert.equal(offlineImportBlocker(base), '');
  assert.equal(offlineImportBlocker({ ...base, history_landing: { harness: 'opencode' } }), '');
  assert.match(offlineImportBlocker({ ...base, history: false }), /No conversation lands/);
  const choices = { group: 'team', name: '', harness: '', model: '', cwd: '', keepPaths: true };
  assert.equal(agentImportParams(choices).start, '', 'a preview never sends start');
  assert.equal(agentImportParams(choices, true, false).start, 'false');
  assert.equal(agentImportParams(choices).agent, '', 'an ordinary import names no agent of the file');
  const landing = agentImportParams({ ...choices, agent: 'scout', role: ' dev ' });
  assert.deepEqual([landing.agent, landing.role], ['scout', 'dev'], 'a performer lands by its key, under the role asked for');
});

test('a group preview offers per-member harness and model and re-inspects with them', async (t) => {
  const inspected = [];
  const imported = [];
  const inspection = {
    source: 'paf', source_group: 'crew', format_version: 5, agent_count: 2, message_count: 3, conv_count: 2, missing_convs: 0,
    target_name: 'crew', target_name_valid: true, group_name_taken: false, conv_collisions: [], findings: [],
    landings: [
      { conv_id: 'c1', title: 'alice', source_harness: 'claude', harness: 'claude', rung: 'native', posture: { sandbox_implementation: 'tclaude-layer', sandbox_profiles: ['walled'], resolved_by: 'install default' } },
      { conv_id: 'c2', title: 'cody', source_harness: 'opencode', harness: 'opencode', rung: 'none', warnings: ['config only'], posture: { sandbox_implementation: 'tclaude-layer', opencode_state: 'private', refused: 'no bwrap here' } },
    ],
  };
  const actions = {
    async inspectGroupImport(file, as, landing) { inspected.push(landing); return inspection; },
    async importGroup(...args) { imported.push(args); },
    async importAgentPAF() { throw new Error('should not be tried for a group'); },
  };
  const { host, settle, choose } = await mountManagement(t, actions, { kind: 'group-import', file: { name: 'crew.paf' } });
  await settle();
  assert.match(host.querySelector('#group-import-title').textContent, /Import a group/);
  const rows = [...host.querySelectorAll('#group-import-landings tbody tr')];
  assert.deepEqual(rows.map((row) => row.dataset.member), ['alice', 'cody']);
  assert.match(rows[1].textContent, /config only/);
  assert.match(rows[0].textContent, /tclaude-layer · walled/);
  assert.match(rows[1].textContent, /no bwrap here/);
  await choose(host.querySelector('#group-import-member-c2-harness'), 'codex');
  await settle();
  assert.deepEqual(inspected.at(-1).members, { c2: { harness: 'codex', model: '' } });
  const into = host.querySelector('#group-import-into');
  into.value = '/tmp/app';
  into.dispatchEvent(new host.ownerDocument.defaultView.Event('input', { bubbles: true }));
  await settle();
  host.querySelector('#group-import-submit').click();
  await settle();
  assert.equal(imported[0][1], '/tmp/app');
  assert.deepEqual(imported[0][3].members, { c2: { harness: 'codex', model: '' } });
});

test('a group import shows its warnings on the preview and stays open on them after landing', async (t) => {
  const inspection = {
    source: 'paf', source_group: 'crew', format_version: 5, agent_count: 0, message_count: 0, conv_count: 0, missing_convs: 0,
    target_name: 'crew', target_name_valid: true, group_name_taken: false, conv_collisions: [], findings: [],
    file_warnings: ['Scheduled jobs nightly land disabled.'],
  };
  const actions = {
    async inspectGroupImport() { return inspection; },
    async importGroup() { return { summary: 'Imported group "crew"', warnings: inspection.file_warnings }; },
    async importAgentPAF() { throw new Error('should not be tried for a group'); },
  };
  const { host, settle } = await mountManagement(t, actions, { kind: 'group-import', file: { name: 'crew.paf' } });
  await settle();
  assert.match(host.querySelector('#group-import-warnings .gi-warn').textContent, /nightly land disabled/);
  const into = host.querySelector('#group-import-into');
  into.value = '/tmp/app';
  into.dispatchEvent(new host.ownerDocument.defaultView.Event('input', { bubbles: true }));
  await settle();
  host.querySelector('#group-import-submit').click();
  await settle();
  assert.match(host.querySelector('#group-import-result-summary').textContent, /Imported group "crew"/);
  assert.match(host.querySelector('#group-import-result .gi-warn').textContent, /nightly land disabled/);
  host.querySelector('#group-import-done').click();
  await settle();
  assertAbsent(host.querySelector('#group-import-result'), 'Done closes the result');
});

test('a dropped .paf opens the import dialog unless a drop zone took it or a dialog is open', async (t) => {
  const harness = await createPreactHarness(t);
  const { bindPAFDrop } = await harness.importDashboardModule('js/paf-drop.js');
  const opened = [];
  t.after(bindPAFDrop({ open: (file) => opened.push(file.name), doc: harness.document }));
  const { document, window } = harness;
  const drop = (name, { prevented = false } = {}) => {
    const event = new window.Event('drop', { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'dataTransfer', { value: { types: ['Files'], files: [{ name }] } });
    if (prevented) event.preventDefault();
    document.body.dispatchEvent(event);
  };
  drop('notes.txt');
  drop('logs.zip', { prevented: true });
  assert.deepEqual(opened, [], 'other files and handled drops are left alone');
  // The page always holds hidden dialogs; they must not stop a drop.
  const hidden = document.body.appendChild(document.createElement('div'));
  hidden.setAttribute('role', 'dialog');
  hidden.setAttribute('aria-modal', 'true');
  drop('reviewer.paf');
  assert.deepEqual(opened, ['reviewer.paf']);
  hidden.remove();
  const overlay = document.body.appendChild(document.createElement('div'));
  overlay.className = 'modal-overlay show';
  drop('other.paf');
  assert.deepEqual(opened, ['reviewer.paf'], 'an open dialog is not replaced');
  overlay.remove();
});

test('a library manager is in the URL (?manage=): opening announces it, Back closes it', async (t) => {
  const opened = [];
  const { harness, state } = await mountManagement(t, {
    openManager: async (kind) => { opened.push(kind); state.openManager(kind); },
    openTemplateManager: () => state.openTemplateManager(),
  }, null);
  const overlays = await harness.importDashboardModule('js/overlay-routes.js');
  const announced = [];
  const on = (e) => announced.push(e.detail);
  harness.document.addEventListener(overlays.OVERLAY_EVENT, on);
  t.after(() => harness.document.removeEventListener(overlays.OVERLAY_EVENT, on));
  await harness.act(() => state.openManager('roles'));
  assert.equal(overlays.overlayParams().manage, 'roles');
  assert.deepEqual(announced, [{ closed: false, replace: false }]);
  await harness.act(() => overlays.restoreOverlays({ manage: 'templates' }));
  assert.equal(state.manager.value, '');
  assert.equal(state.templateManager.value, true);
  await harness.act(() => overlays.restoreOverlays({}));
  assert.equal(overlays.overlayParams().manage, '');
  // An editor open on top keeps the manager under it.
  await harness.act(() => overlays.restoreOverlays({ manage: 'profiles' }));
  assert.deepEqual(opened, ['profiles']);
  state.dialog.value = { kind: 'unrendered-editor' };
  await harness.act(() => overlays.restoreOverlays({}));
  assert.equal(state.manager.value, 'profiles');
  state.dialog.value = null;
  await harness.act(() => state.closeManager());
});

test('a group import cut needs confirmation only for a member that lands cut', async (t) => {
  const harness = await createPreactHarness(t);
  const dialogs = await harness.importDashboardModule('js/paf-dialogs.js');
  const model = await harness.importDashboardModule('js/paf-model.js');
  const rows = [
    { conv_id: 'a', title: 'alice', rung: 'translated', losses: { needs_summary: true, dropped_percent: 60 } },
    { conv_id: 'b', title: 'bob', rung: 'translated', losses: { needs_summary: false } },
    { conv_id: 'c', title: 'carol', rung: 'none', losses: { needs_summary: true } },
  ];
  assert.deepEqual(dialogs.memberCutsToConfirm(rows).map((row) => row.title), ['alice']);
  assert.deepEqual(dialogs.memberCutsToConfirm(undefined), []);
  assert.match(model.queryString(model.landingParams({ confirmHistoryLoss: true })), /confirm_history_loss=true/);
  assert.doesNotMatch(model.queryString(model.landingParams({})), /confirm_history_loss/);
});

test('the import confirm list starts as previewed, and the import sends what stays ticked', async (t) => {
  const imported = [];
  const revealed = [];
  const inspection = {
    source: 'paf', source_group: 'crew', format_version: 5, agent_count: 1, message_count: 0, conv_count: 1, missing_convs: 0,
    target_name: 'crew', target_name_valid: true, group_name_taken: false, conv_collisions: [], findings: [],
    file_warnings: ['listed warning', 'other warning'],
    confirm: {
      digest: 'abc', can_keep: true, warnings: ['other warning'],
      items: [
        { id: 'owner:c1', kind: 'owners', label: 'owner lead', gives: 'Owns the group.', default: true },
        { id: 'env:TOKEN', kind: 'environment', label: 'environment variable TOKEN', gives: 'Set in every spawn.', revealable: true },
      ],
    },
  };
  const actions = {
    async inspectGroupImport() { return inspection; },
    async importGroup(...args) { imported.push(args); return { summary: 'Imported', warnings: [], kept: ['environment variable TOKEN'], dropped: ['owner lead'] }; },
    async importAgentPAF() { throw new Error('should not be tried for a group'); },
    async revealImportValue(file, item) { revealed.push(item); return { value: 'the-value' }; },
  };
  const { host, settle, tick } = await mountManagement(t, actions, { kind: 'group-import', file: { name: 'crew.paf' } });
  await settle();
  const owner = host.querySelector('[data-item="owner:c1"] input');
  const env = host.querySelector('[data-item="env:TOKEN"] input');
  assert.equal(owner.getAttribute('checked') !== null, true, 'owners start ticked');
  assert.equal(env.getAttribute('checked') !== null, false, 'environment starts unticked');
  assert.doesNotMatch(host.textContent, /the-value/);
  assert.deepEqual([...host.querySelectorAll('#group-import-warnings .gi-warn')].map((row) => row.textContent), ['⚠ other warning'], 'the list already shows the rest');

  host.querySelector('[data-item="env:TOKEN"] .import-confirm-reveal').click();
  await settle();
  assert.deepEqual(revealed, ['env:TOKEN']);
  assert.match(host.querySelector('[data-item="env:TOKEN"] .import-confirm-value').textContent, /the-value/);

  await tick(owner, false);
  await tick(env, true);
  const into = host.querySelector('#group-import-into');
  into.value = '/tmp/app';
  into.dispatchEvent(new host.ownerDocument.defaultView.Event('input', { bubbles: true }));
  await settle();
  host.querySelector('#group-import-submit').click();
  await settle();
  assert.deepEqual(imported[0][4], { keep: ['env:TOKEN'], keep_digest: 'abc' });
  assert.match(host.querySelector('#import-outcome-kept').textContent, /TOKEN/);
  assert.match(host.querySelector('#import-outcome-dropped').textContent, /owner lead/);
});

test('an emptied confirm list says so, and a caller who cannot keep sends nothing', async (t) => {
  const harness = await createPreactHarness(t);
  const { keepParams } = await harness.importDashboardModule('js/paf-model.js');
  const confirm = { digest: 'abc', can_keep: true, items: [{ id: 'owner:c1', default: true }] };
  assert.deepEqual(keepParams(confirm), { keep: ['owner:c1'], keep_digest: 'abc' });
  assert.deepEqual(keepParams(confirm, { 'owner:c1': false }), { keep_none: 'true' });
  assert.deepEqual(keepParams({ ...confirm, can_keep: false }), {});
  assert.deepEqual(keepParams({ ...confirm, items: [] }), {});
});
