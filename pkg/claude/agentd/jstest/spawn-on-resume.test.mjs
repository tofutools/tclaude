import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ROWS = [
  { field: 'group_env', label: 'Group environment', mode: 're-resolve', default: 're-resolve', modes: ['re-resolve', 'reuse'], settable: true, why: 'Group settings are kept current.' },
  { field: 'harness_config', label: 'Harness config posture', mode: 'tighten-only', default: 'explicit-wins', modes: ['explicit-wins', 'tighten-only', 'chain-wins'], settable: true, why: 'Least surprise.' },
];
const FIELDS = [
  { key: 'harness', label: 'Harness', layer: 'fixed', configurable: false },
  { key: 'approval', label: 'Approval policy', layer: 'spawn copy', configurable: false },
  { key: 'identity', label: 'Agent name and description', layer: 'spawn only', configurable: false },
  { key: 'group_env', label: 'Group environment', layer: 'group setting', configurable: true },
];
const RESOLVED = {
  group_env: { value: 'reuse', source: 'profile pinned' },
  harness_config: { value: 'tighten-only', source: 'global' },
};

const ok = (body) => async () => ({ ok: true, status: 200, json: async () => body });
const flushOf = (harness) => () => harness.act(async () => { for (let i = 0; i < 8; i += 1) await Promise.resolve(); });

test('on resume row: the summary names each row\'s source, and Edit… hands over the daemon\'s preview', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/spawn-on-resume.js');
  const urls = [];
  const edits = [];
  const fetchImpl = async (url) => { urls.push(url); return ok({ rows: ROWS, fields: FIELDS, resolved: RESOLVED })(); };
  const view = await harness.mount(harness.html`<${mod.SpawnOnResumeRow} fetchImpl=${fetchImpl} group="crew" profile="pinned"
    selection=${{ harness_config: 'chain-wins' }} onEdit=${(arg) => edits.push(arg)} />`);
  await flushOf(harness)();
  const c = view.container;
  assert.equal(urls[0], '/api/relaunch-policy?preview=1&group=crew&profile=pinned', 'the daemon resolves for the dialog\'s group and profile');
  const summary = c.querySelector('[data-on-resume-summary]').textContent;
  assert.match(summary, /Group environment → Reuse \(profile pinned\)/);
  assert.match(summary, /Harness config posture → Chain wins \(this spawn\)/);
  assert.match(c.querySelector('[data-on-resume-own]').textContent, /1 set for this spawn/);
  assert.equal(c.querySelector('table'), null, 'no inline table');
  await harness.act(async () => { c.querySelector('#agent-spawn-on-resume-edit').click(); });
  assert.deepEqual(edits, [{ rows: ROWS, fields: FIELDS, inherited: RESOLVED }]);
});

test('on resume row: says nothing when the policy cannot be read', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/spawn-on-resume.js');
  const view = await harness.mount(harness.html`<${mod.SpawnOnResumeRow} fetchImpl=${async () => ({ ok: false, status: 500, json: async () => ({}) })} />`);
  await flushOf(harness)();
  assert.equal(view.container.querySelector('#agent-spawn-on-resume-row'), null);
});

test('relaunch rows summary at defaults', async (t) => {
  const harness = await createPreactHarness(t);
  const { relaunchRowsSummary } = await harness.importDashboardModule('js/relaunch-policy-dialog.js');
  const atDefault = [{ field: 'group_env', label: 'Group environment', mode: 're-resolve', default: 're-resolve' }];
  assert.equal(relaunchRowsSummary(atDefault, {}, {}, 'this spawn'), 'Every row follows the global table at its default');
  assert.equal(relaunchRowsSummary(atDefault, { group_env: 'inherit' }, { group_env: { value: 'reuse', source: 'profile p' } }, 'this spawn'),
    'Every row follows the global table at its default', 'Follow global takes the global table over the profile');
});

async function mountDialog(t, descriptor) {
  const harness = await createPreactHarness(t);
  const { RelaunchPolicyDialog } = await harness.importDashboardModule('js/relaunch-policy-dialog.js');
  const closed = [];
  const view = await harness.mount(harness.html`<${RelaunchPolicyDialog} descriptor=${descriptor}
    state=${{ close: () => closed.push(true) }} confirmDiscard=${async () => true} />`);
  const row = (field) => view.container.querySelector(`[data-relaunch-field="${field}"]`);
  const pick = (field, mode) => harness.act(async () => { row(field).querySelector(`[data-mode="${mode}"]`).click(); });
  const effect = (field) => row(field).querySelector('[data-relaunch-effect]').textContent;
  return { harness, view, row, pick, effect, closed };
}

test('relaunch policy dialog: Default, Follow global and the modes, with what each comes to', async (t) => {
  const saved = [];
  const { view, row, pick, effect, closed, harness } = await mountDialog(t, {
    scope: 'spawn', rows: ROWS, fields: FIELDS, inherited: RESOLVED, selection: {}, label: 'worker', onSave: (kept) => saved.push(kept),
  });
  assert.match(effect('group_env'), /Reuse · profile pinned/, 'Default shows what the profiles give');
  assert.match(row('group_env').querySelector('[data-mode="default"]').getAttribute('title'), /Reuse \(profile pinned\)/);
  assert.ok(row('group_env').querySelector('[data-mode="inherit"]'), 'a spawn can follow the global table');
  assert.equal(row('harness_config').querySelectorAll('[data-mode]').length, 5, 'Default, Follow global and three modes');
  assert.equal(view.container.querySelector('#relaunch-policy-banner'), null, 'no intro banner: it is behind the title\'s [?]');
  assert.match(view.container.querySelector('#relaunch-policy-help-hint').textContent, /Follow global takes the global table/);
  assert.equal(row('group_env').querySelector('.perm-row-desc'), null, 'no row description under the label');
  assert.equal(row('group_env').querySelector('#relaunch-policy-help-group_env-hint').textContent, 'Group settings are kept current.');
  assert.equal(row('group_env').querySelector('.perm-row-slug > span[title]').getAttribute('title'), 'Group settings are kept current.');
  assert.equal(view.container.querySelector('#relaunch-policy-layers').hasAttribute('open'), false, 'the other inputs start collapsed');
  assert.equal(view.container.querySelector('[data-relaunch-layers-summary]').textContent, '1 copied at spawn · 1 fixed');

  await pick('group_env', 'inherit');
  assert.match(effect('group_env'), /Re-resolve · global/, 'Follow global shows what that resolves to');
  await pick('harness_config', 'chain-wins');
  assert.match(effect('harness_config'), /Chain wins · this spawn/);
  await pick('group_env', 'default');
  assert.match(effect('group_env'), /Reuse · profile pinned/);
  assert.equal(view.container.querySelectorAll('[data-relaunch-layer]').length, 2, 'the other inputs, by layer, spawn-only left out');

  await harness.act(async () => { view.container.querySelector('#relaunch-policy-submit').click(); });
  assert.deepEqual(saved, [{ harness_config: 'chain-wins' }], 'sparse: Default is an absent row');
  assert.equal(closed.length, 1);
});

test('relaunch policy dialog for a profile: no Follow global, sources are this profile or global', async (t) => {
  const { row, pick, effect } = await mountDialog(t, { scope: 'profile', rows: ROWS, fields: [], selection: { group_env: 'reuse' } });
  assert.equal(row('group_env').querySelector('[data-mode="inherit"]'), null);
  assert.match(effect('group_env'), /Reuse · this profile/);
  await pick('group_env', 'default');
  assert.match(effect('group_env'), /Re-resolve · global/);
});

test('relaunch policy dialog: all default keeps rows the editor does not show', async (t) => {
  const saved = [];
  const { view, harness } = await mountDialog(t, { scope: 'profile', rows: ROWS, fields: [],
    selection: { group_env: 'reuse', future_row: 'reuse' }, onSave: (kept) => saved.push(kept) });
  await harness.act(async () => { view.container.querySelector('#relaunch-policy-reset').click(); });
  await harness.act(async () => { view.container.querySelector('#relaunch-policy-submit').click(); });
  assert.deepEqual(saved, [{ future_row: 'reuse' }]);
});

test('agent-spawn model sends this spawn\'s relaunch rows, and Save as profile leaves Follow global out', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const groups = [{ name: 'crew', default_cwd: '/repo' }];
  const harnesses = [{ name: 'claude', display_name: 'Claude Code', models: ['sonnet'], effort_levels: [] }];
  const context = { groups, harnesses, userDefaultModel: '', normalizeNames: true };
  const base = { ...model.createSpawnDraft({ groups, harnesses, groupName: 'crew' }), name: 'worker' };
  assert.deepEqual(base.relaunchPolicy, {});
  assert.equal('relaunch_policy' in model.buildSpawnRequest(base, context, null, []).body, false);
  const draft = { ...base, relaunchPolicy: { group_env: 'inherit', harness_config: 'tighten-only' } };
  assert.deepEqual(model.buildSpawnRequest(draft, context, null, []).body.relaunch_policy, { group_env: 'inherit', harness_config: 'tighten-only' });
  assert.equal(model.spawnDraftIsDirty(draft, base), true);
  assert.deepEqual(model.spawnProfileSeed(draft, context).relaunch_policy, { harness_config: 'tighten-only' });
  const fromProfile = { ...draft, profileRelaunchPolicy: { group_env: 'reuse', sandbox_profile_contents: 'reuse' } };
  assert.deepEqual(model.spawnProfileSeed(fromProfile, context).relaunch_policy, { sandbox_profile_contents: 'reuse', harness_config: 'tighten-only' },
    'the selected profile\'s rows are carried too, under this spawn\'s own; Follow global drops the profile\'s row');
  const profile = { name: 'pinned', relaunch_policy: { group_env: 'reuse' } };
  const applied = model.applySpawnProfile({ ...base, profile: 'pinned' }, profile, context);
  assert.deepEqual(applied.profileRelaunchPolicy, { group_env: 'reuse' });
  assert.equal('relaunch_policy' in model.buildSpawnRequest(applied, context, null, []).body, false, 'the daemon reads the profile\'s rows itself');
});
