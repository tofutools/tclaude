import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const MONTHLY = { name: 'Monthly spend', metric: 'total_usd', period: 'month', budget: 500,
  steps: [{ at: 50, action: 'notify', notify: ['operator'] }, { at: 100, action: 'wrap_up' }] };
const STANDARD = { name: 'Lifetime budget', metric: 'total_usd', period: 'lifetime', budget: 20,
  steps: [{ at: 80, action: 'notify', notify: ['agent'], repeat_every_pct: 10 }, { at: 100, action: 'wrap_up' }] };

function overviewFixture() {
  return {
    profiles: [{ id: 3, name: 'standard', descr: 'everyday', rules: [STANDARD] }],
    limits: [{ id: 7, scope: 'global', enabled: true, rule: MONTHLY }],
    limit_statuses: [{ rule_key: 'limit:7', scope: 'global', limit_id: 7, name: 'Monthly spend', metric: 'total_usd',
      period: 'month', value: 260, budget: 500, pct: 52, reached: 'notify' }],
    assignments: [{ scope: 'group', scope_id: 'g:1', scope_name: 'tclaude', profile_id: 3, profile_name: 'standard' }],
    overrides: [],
    statuses: [{ rule_key: 'profile:3:lifetime budget', scope: 'agent', agent_id: 'a1', agent_title: 'worker', profile: 'standard',
      layer: 'group', name: 'Lifetime budget', metric: 'total_usd', period: 'lifetime', value: 17, budget: 20, pct: 85, reached: 'notify' }],
    groups: ['tclaude'], spawn_profiles: ['dev'], agents: [{ id: 'a1', title: 'worker' }], windows: ['anthropic:five_hour'],
  };
}

test('usage rule drafts round-trip and catch what the form can see', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/usage-rules-model.js');
  const draft = model.ruleToDraft(STANDARD);
  assert.equal(draft.steps[0].repeat, '10%');
  assert.deepEqual(model.draftToRule(draft, 'agent'), STANDARD);
  assert.deepEqual(model.parseRepeat('30m'), { repeat_every_minutes: 30 });
  assert.deepEqual(model.parseRepeat(' 5 '), { repeat_every_pct: 5 });
  assert.throws(() => model.parseRepeat('soon'), /10% or 30m/);
  assert.throws(() => model.draftToRule({ ...draft, budget: '0' }, 'agent'), /budget must be a positive amount/);
  assert.throws(() => model.draftToRule({ ...draft, steps: [{ at: 50, action: 'notify', notify: [], repeat: '' }] }, 'agent'), /needs a recipient/);
  assert.throws(() => model.draftToRule({ ...draft, steps: [{ at: 50, action: 'block_spawn', notify: [], repeat: '' }] }, 'agent'), /limits only/);
  const windowRule = model.draftToRule({ ...model.blankRule('global'), name: '5h', metric: 'window_pct', window: 'anthropic:five_hour' }, 'global');
  assert.equal(windowRule.period, 'window');
  assert.equal(windowRule.budget, 100);
  assert.throws(() => model.draftToRule({ ...draft, metric: 'window_pct' }, 'group'), /global limit/);
  assert.equal(model.metricsFor('group').some((m) => m.id === 'window_pct'), false);
  assert.equal(model.periodsFor('global').some((p) => p.id === 'lifetime'), false);
  assert.equal(model.actionsFor('agent').some((a) => a.id === 'block_spawn'), false);
  assert.match(model.ruleSummary(MONTHLY), /^\$500 per month · Cost \(API or what-if\) — 50% notify operator, 100% wrap up$/);
  for (const preset of [...model.LIMIT_PRESETS, ...model.PROFILE_PRESETS]) {
    const scope = model.LIMIT_PRESETS.includes(preset) ? 'global' : 'agent';
    assert.doesNotThrow(() => model.draftToRule(model.ruleToDraft(preset.rule), scope), preset.id);
  }
  const overview = overviewFixture();
  assert.deepEqual(model.assignedNames(overview.assignments, 'group', 'tclaude'), ['standard']);
  assert.deepEqual(model.profileUses(overview.assignments, 3), ['group tclaude']);
  const readings = model.agentReadings({ ...overview, overrides: [{ agent_id: 'a2', exempt: true }] });
  assert.deepEqual(readings.map((r) => r.agentID), ['a1', 'a2']);
  assert.equal(readings[1].override.exempt, true);
  assert.equal(model.barTone({ pct: 50 }), 'ok');
  assert.equal(model.barTone({ pct: 85 }), 'warn');
  assert.equal(model.barTone({ pct: 60, reached: 'wrap_up' }), 'over');
});

async function mountDialog(t, { tab = '' } = {}) {
  const harness = await createPreactHarness(t);
  const { UsageRulesDialog } = await harness.importDashboardModule('js/usage-rules.js');
  const calls = [];
  const record = (name) => async (...args) => { calls.push([name, ...args]); return {}; };
  const api = {
    overview: async () => overviewFixture(),
    createLimit: record('createLimit'), updateLimit: record('updateLimit'), deleteLimit: record('deleteLimit'),
    createProfile: record('createProfile'), updateProfile: record('updateProfile'), deleteProfile: record('deleteProfile'),
    assign: record('assign'), adjust: record('adjust'),
  };
  let closed = false;
  const mounted = await harness.mount(harness.html`<${UsageRulesDialog} descriptor=${{ kind: 'usage-rules', tab }} api=${api}
    onClose=${() => { closed = true; }} confirmDiscard=${async () => true} confirm=${async () => true}/>`);
  const flush = async () => { for (let turn = 0; turn < 8; turn++) await harness.act(() => Promise.resolve()); };
  await flush();
  const host = mounted.container;
  const click = async (el) => { assert.ok(el, 'missing element'); await harness.act(() => { harness.fireEvent(el, 'click'); }); await flush(); };
  const button = (text) => [...host.querySelectorAll('button')].find((b) => b.textContent.trim() === text);
  const select = async (label, value, scope = host) => {
    const field = scope.querySelector(`[aria-label="${label}"]`);
    assert.ok(field, `missing ${label}`);
    Object.defineProperty(field, 'value', { configurable: true, writable: true, value });
    await harness.act(() => { harness.fireEvent(field, 'change'); });
    await flush();
  };
  return { harness, host, mounted, calls, click, button, select, flush, closed: () => closed };
}

test('limits tab shows readings and saves a preset limit', async (t) => {
  const { host, mounted, calls, click, button, harness, flush } = await mountDialog(t);
  const card = host.querySelector('[data-limit-id="7"]');
  assert.match(card.textContent, /Monthly spend/);
  assert.match(card.textContent, /\$260 \/ \$500 · 52%/);
  assert.ok(card.querySelector('.usage-rule-reading.warn'));
  await click(button('＋ Daily spend cap'));
  assert.ok(host.querySelector('#usage-limit-editor'));
  assert.equal(host.querySelector('[aria-label="Rule name"]').value, 'Daily spend');
  await click(host.querySelector('#usage-limit-save'));
  const [name, body] = calls.find(([n]) => n === 'createLimit');
  assert.equal(name, 'createLimit');
  assert.equal(body.scope, 'global');
  assert.equal(body.rule.period, 'day');
  assert.deepEqual(body.rule.steps.map((s) => s.action), ['notify', 'notify', 'block_spawn']);
  assert.equal(host.querySelector('#usage-limit-editor'), null, 'the editor closes after saving');
  const enabled = host.querySelector('[data-limit-id="7"] .usage-rule-enabled input');
  await harness.act(() => { harness.fireEvent(enabled, 'change'); });
  await flush();
  const toggle = calls.find(([n]) => n === 'updateLimit');
  assert.equal(toggle[1], 7);
  assert.equal(toggle[2].enabled, false);
  await mounted.unmount();
});

test('assignments save per layer and agents can be snoozed or extended', async (t) => {
  const { host, mounted, calls, click, button, select, harness, flush } = await mountDialog(t, { tab: 'assignments' });
  assert.match(host.textContent, /Global default/);
  await select('Add usage profile to All agents', 'standard');
  assert.deepEqual(calls.at(-1), ['assign', 'global', '', ['standard']]);
  await click(host.querySelector('[aria-label="Remove standard from tclaude"]'));
  assert.deepEqual(calls.at(-1), ['assign', 'group', 'tclaude', []]);
  await click(host.querySelector('[data-usage-rules-tab="agents"]'));
  const agent = host.querySelector('[data-agent-id="a1"]');
  assert.match(agent.textContent, /worker/);
  assert.match(agent.textContent, /\$17\.00 \/ \$20\.00 · 85%/);
  await click(button('snooze 1h'));
  assert.deepEqual(calls.at(-1), ['adjust', 'a1', { snooze_minutes: 60 }]);
  await click(button('extend…'));
  const amount = host.querySelector('[aria-label="Extra budget"]');
  amount.value = '15';
  await harness.act(() => { harness.fireEvent(amount, 'input'); });
  await flush();
  await click(button('set'));
  assert.deepEqual(calls.at(-1), ['adjust', 'a1', { rule_key: 'profile:3:lifetime budget', extra_budget: 15 }]);
  await mounted.unmount();
});

test('profile editor builds the rules body', async (t) => {
  const { host, mounted, calls, click, button, harness, flush } = await mountDialog(t, { tab: 'profiles' });
  const card = host.querySelector('[data-profile-id="3"]');
  assert.match(card.textContent, /group tclaude/);
  await click([...card.querySelectorAll('button')].find((b) => b.textContent === 'edit'));
  const descr = host.querySelector('[aria-label="Usage profile description"]');
  descr.value = 'tighter';
  await harness.act(() => { harness.fireEvent(descr, 'input'); });
  await flush();
  await click(host.querySelector('#usage-profile-save'));
  const [, id, body] = calls.find(([n]) => n === 'updateProfile');
  assert.equal(id, 3);
  assert.equal(body.descr, 'tighter');
  assert.deepEqual(body.rules, [STANDARD]);
  await click(button('＋ Blank profile'));
  await click(host.querySelector('#usage-profile-save'));
  assert.match(host.querySelector('[role="alert"]').textContent, /give the profile a name/);
  await mounted.unmount();
});
