import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function snapshot() {
  return {
    agents: [{
      agent_id: 'agt_target', conv_id: 'conv-target', title: 'target',
      online: true, memberships: [],
    }],
    groups: [{ id: 7, name: 'alpha', members: [] }],
  };
}

test('standing-order dialog creates explicit session-boundary mutations and prevents double submit', async (t) => {
  const harness = await createPreactHarness(t);
  const { StandingOrderDialog } = await harness.importDashboardModule(
    'js/jobs-standing-order-dialog-island.js',
  );
  let resolveSave;
  const saves = [];
  let closes = 0;
  const actions = {
    saveStandingOrder: (mutation) => {
      saves.push(mutation);
      return new Promise((resolve) => { resolveSave = resolve; });
    },
    closeStandingOrderDialog: () => { closes += 1; },
  };
  const mounted = await harness.mount(harness.html`<${StandingOrderDialog}
    descriptor=${{
      kind: 'create',
      prefill: {
        name: 'pr-early', target: 'agt_target', summary: 'Push the PR early.',
      },
    }}
    snapshot=${snapshot()} actions=${actions} confirmDiscard=${async () => true} />`);

  const selectedMode = mounted.container.querySelector(
    'input[name="standing-order-source-mode"][value="selected"]',
  );
  await harness.act(() => harness.fireEvent(selectedMode, 'change'));
  const compact = mounted.container.querySelector('#standing-order-sources input[value="compact"]');
  compact.checked = true;
  await harness.act(() => harness.fireEvent(compact, 'change'));
  const submit = mounted.container.querySelector('#standing-order-submit');
  await harness.act(() => {
    harness.fireEvent(submit, 'click');
    harness.fireEvent(submit, 'click');
  });
  assert.equal(saves.length, 1, 'save is single-flight');
  assert.deepEqual(saves[0], {
    path: '/api/standing-orders', method: 'POST',
    payload: {
      name: 'pr-early', target: 'agt_target', role: '',
      summary: 'Push the PR early.', trigger_event: 'session.start',
      sources: ['compact'], match_field: '', match_regex: '',
      timing: 'same-continuation', cadence: 'always',
      cooldown_seconds: 0, enabled: true,
    },
  });
  assert.equal(submit.disabled, true);
  await harness.act(() => resolveSave({ id: 1 }));
  assert.equal(closes, 1);
  await mounted.unmount();
});

test('standing-order dialog authors a prompt RE2 condition', async (t) => {
  const harness = await createPreactHarness(t);
  const { StandingOrderDialog } = await harness.importDashboardModule(
    'js/jobs-standing-order-dialog-island.js',
  );
  const saves = [];
  const mounted = await harness.mount(harness.html`<${StandingOrderDialog}
    descriptor=${{
      kind: 'create',
      prefill: {
        name: 'deploy-prompt', target: 'agt_target', summary: 'Use the release checklist.',
      },
    }}
    snapshot=${snapshot()} actions=${{
      saveStandingOrder: async (mutation) => { saves.push(mutation); },
      closeStandingOrderDialog: () => {},
    }} confirmDiscard=${async () => true} />`);

  const trigger = mounted.container.querySelector('#standing-order-trigger');
  Object.defineProperty(trigger, 'value', {
    configurable: true, writable: true, value: 'user.prompt',
  });
  await harness.act(() => harness.fireEvent(trigger, 'change'));
  assert.equal(mounted.container.querySelector('#standing-order-sources'), null);
  assert.match(mounted.container.textContent, /OpenCode is shown as unsupported/);

  const field = mounted.container.querySelector('#standing-order-match-field');
  Object.defineProperty(field, 'value', {
    configurable: true, writable: true, value: 'prompt',
  });
  await harness.act(() => harness.fireEvent(field, 'change'));
  const expression = mounted.container.querySelector('#standing-order-match-regex');
  await harness.input(expression, '(?i)\\bdeploy\\b');

  await harness.act(() => harness.fireEvent(
    mounted.container.querySelector('#standing-order-submit'), 'click',
  ));
  assert.equal(saves.length, 1);
  assert.equal(saves[0].payload.trigger_event, 'user.prompt');
  assert.deepEqual(saves[0].payload.sources, []);
  assert.equal(saves[0].payload.match_field, 'prompt');
  assert.equal(saves[0].payload.match_regex, '(?i)\\bdeploy\\b');
  await mounted.unmount();
});

test('standing-order edit shows retirement state and sends the stored row version', async (t) => {
  const harness = await createPreactHarness(t);
  const { StandingOrderDialog } = await harness.importDashboardModule(
    'js/jobs-standing-order-dialog-island.js',
  );
  const saves = [];
  const order = {
    id: 4, name: 'retired-order', revision: 6, row_version: 9, enabled: false,
    disabled_reason: 'group-retired',
    target: { kind: 'group', group_name: 'alpha', role: 'reviewer' },
    summary: 'Keep context.', trigger: { sources: [] },
    timing: 'next-turn', cadence: 'once-per-generation',
  };
  const model = await harness.importDashboardModule('js/jobs-dialog-model.js');
  const mounted = await harness.mount(harness.html`<${StandingOrderDialog}
    descriptor=${{ kind: 'edit', id: 4, order, prefill: model.standingOrderToPrefill(order) }}
    snapshot=${snapshot()} actions=${{
      saveStandingOrder: async (mutation) => { saves.push(mutation); },
      closeStandingOrderDialog: () => {},
    }} confirmDiscard=${async () => true} />`);
  assert.match(mounted.container.textContent, /disabled automatically: group-retired/);
  await harness.act(() => harness.fireEvent(
    mounted.container.querySelector('#standing-order-submit'), 'click',
  ));
  assert.equal(saves[0].payload.row_version, 9);
  assert.equal(saves[0].payload.revision, undefined);
  assert.equal(saves[0].payload.target, 'group:alpha');
  assert.equal(saves[0].payload.enabled, false);
  await mounted.unmount();
});
