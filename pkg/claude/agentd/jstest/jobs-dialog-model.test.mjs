import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('cron dialog model preserves create, edit, duplicate, and validation contracts', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/jobs-dialog-model.js');
  const job = {
    id: 7, name: 'daily', owner_agent: 'agt_owner', target_kind: 'conv',
    target_agent: 'agt_target', interval_seconds: 300, subject: 'Status', body: 'Report', enabled: true,
  };
  const edit = {
    kind: 'edit', id: 7, originalTarget: 'agt_target', originalExpr: '',
  };
  const draft = model.createCronDraft(model.cronJobToPrefill(job));
  assert.equal(draft.interval, '5m');
  assert.equal(model.validateCronDraft(edit, draft), null);
  assert.deepEqual(model.buildCronMutation(edit, draft), {
    path: '/api/cron/7', method: 'PATCH',
    payload: {
      name: 'daily', body: 'Report', subject: 'Status', enabled: true,
      run_immediately: false,
      queue_when_offline: false,
      action_kind: 'message', spawn_profile: '', spawn_roles: [],
      spawn_name_template: '', spawn_instruction_template: '',
      spawn_concurrency_policy: 'Forbid', spawn_max_live_workers: 1,
      spawn_worker_deadline_seconds: 0,
      owner: 'agt_owner', interval: '5m', cron_expr: '',
    },
  }, 'an untouched solo target is omitted from PATCH');

  const retargeted = { ...draft, target: { ...draft.target, target: 'agt_next' } };
  assert.deepEqual(model.buildCronMutation(edit, retargeted).payload, {
    name: 'daily', body: 'Report', subject: 'Status', enabled: true,
    run_immediately: false,
    queue_when_offline: false,
    action_kind: 'message', spawn_profile: '', spawn_roles: [],
    spawn_name_template: '', spawn_instruction_template: '',
    spawn_concurrency_policy: 'Forbid', spawn_max_live_workers: 1,
    spawn_worker_deadline_seconds: 0,
    owner: 'agt_owner', interval: '5m', cron_expr: '', target: 'agt_next', group_id: 0,
  });

  const groupCreate = model.createCronDraft({
    name: 'fanout', targetMode: 'group', groupName: 'alpha', role: 'dev',
    cronExpr: '@daily', body: 'Standup', enabled: false,
  });
  assert.deepEqual(model.buildCronMutation({ kind: 'create' }, groupCreate), {
    path: '/api/cron', method: 'POST',
    payload: {
      name: 'fanout', target: 'group:alpha', subject: '', body: 'Standup',
      enabled: false, run_immediately: false, queue_when_offline: false,
      action_kind: 'message', cron_expr: '@daily', role: 'dev',
    },
  });

  const duplicate = model.cronJobToPrefill(job, { duplicate: true });
  assert.equal(duplicate.name, 'daily-copy');
  assert.equal(model.buildCronMutation(
    { kind: 'duplicate' }, model.createCronDraft(duplicate),
  ).method, 'POST', 'duplicates create a new row without the source id');

  const immediate = model.createCronDraft({
    target: 'agt_target', interval: '5m', body: 'now', runImmediately: true,
  });
  assert.equal(model.buildCronMutation({ kind: 'create' }, immediate).payload.run_immediately, true);
  assert.equal(model.buildCronMutation({ kind: 'create' }, immediate).payload.queue_when_offline, false);
  immediate.enabled = false;
  assert.equal(model.validateCronDraft({ kind: 'create' }, immediate).code, 'immediate-disabled');

  const missing = model.createCronDraft({ targetMode: 'group', groupName: '', body: '' });
  assert.equal(model.validateCronDraft({ kind: 'create' }, missing).code, 'group-target');
  const expressionEdit = model.createCronDraft({ target: 'agt_target', cronExpr: '@daily', body: 'x' });
  expressionEdit.scheduleMode = 'interval';
  expressionEdit.interval = '';
  assert.equal(model.validateCronDraft({ kind: 'edit', originalExpr: '@daily' }, expressionEdit).code, 'edit-interval');
  assert.equal(model.cronDraftDirty(draft, model.createCronDraft(model.cronJobToPrefill(job))), false);
  assert.equal(model.cronDraftDirty({ ...draft, body: 'changed' }, draft), true);

  const spawn = model.createCronDraft({
    name: 'night-review', owner: 'agt_owner', targetMode: 'group', groupName: 'alpha',
    interval: '1h', actionKind: 'spawn', spawnProfile: 'reviewer',
    spawnRoles: ['reviewer'], spawnNameTemplate: 'review-{{fire_time}}',
    spawnInstructionTemplate: 'Review the queue at {{fire_time}}',
    spawnConcurrencyPolicy: 'Allow', spawnMaxLiveWorkers: 2,
    spawnWorkerDeadlineSeconds: 1800,
  });
  assert.equal(model.validateCronDraft({ kind: 'create' }, spawn), null);
  assert.deepEqual(model.buildCronMutation({ kind: 'create' }, spawn), {
    path: '/api/cron', method: 'POST', payload: {
      name: 'night-review', target: 'group:alpha', subject: '', body: '', enabled: true,
      run_immediately: false, queue_when_offline: false, action_kind: 'spawn',
      spawn_profile: 'reviewer', spawn_roles: ['reviewer'],
      spawn_name_template: 'review-{{fire_time}}',
      spawn_instruction_template: 'Review the queue at {{fire_time}}',
      spawn_concurrency_policy: 'Allow', spawn_max_live_workers: 2,
      spawn_worker_deadline_seconds: 1800, interval: '1h', owner: 'agt_owner',
    },
  });
  spawn.target.mode = 'solo';
  assert.equal(model.validateCronDraft({ kind: 'create' }, spawn).code, 'spawn-group');
  spawn.target.mode = 'group';
  spawn.spawn.roles = [];
  assert.deepEqual(model.buildCronMutation({ kind: 'edit', id: 7 }, spawn).payload.spawn_roles, [],
    'an explicit empty role array clears the replacement set');

  const spawnEdit = model.createCronDraft(model.cronJobToPrefill({
    id: 12, name: 'existing-spawn', owner_agent: 'agt_owner', target_kind: 'group', group_name: 'alpha',
    interval_seconds: 900, action_kind: 'spawn', spawn_profile: 'review-kit', spawn_roles: ['reviewer'],
    spawn_name_template: 'worker-{{fire_time}}', spawn_instruction_template: 'Review at {{fire_time}}',
    spawn_concurrency_policy: 'Replace', spawn_max_live_workers: 1,
    spawn_worker_deadline_seconds: 900, enabled: true,
  }));
  assert.equal(spawnEdit.actionKind, 'spawn');
  assert.equal(spawnEdit.target.mode, 'group');
  assert.equal(spawnEdit.spawn.profile, 'review-kit');
  assert.deepEqual(spawnEdit.spawn.roles, ['reviewer']);
  const spawnPatch = model.buildCronMutation({ kind: 'edit', id: 12, originalExpr: '' }, spawnEdit);
  assert.equal(spawnPatch.method, 'PATCH');
  assert.equal(spawnPatch.payload.action_kind, 'spawn');
  assert.equal(spawnPatch.payload.spawn_concurrency_policy, 'Replace');
  assert.equal(spawnPatch.payload.target, 'group:alpha');
});

test('standing-order dialog model preserves stable targets, explicit any-source semantics, and row-version CAS', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/jobs-dialog-model.js');
  const order = {
    id: 9, name: 'pr-early', revision: 3, row_version: 5, enabled: false,
    target: { kind: 'group', group_id: 7, group_name: 'alpha', role: 'reviewer' },
    summary: 'Push the PR early.',
    trigger: { event: 'session.start', sources: ['compact', 'resume'] },
    timing: 'same-continuation', cadence: 'once-per-generation',
    cooldown_seconds: 90, debounce_seconds: 0,
  };
  const draft = model.createStandingOrderDraft(model.standingOrderToPrefill(order));
  assert.equal(draft.target.mode, 'group');
  assert.equal(draft.target.groupName, 'alpha');
  assert.equal(draft.sourceMode, 'selected');
  assert.equal(model.validateStandingOrderDraft({ kind: 'edit' }, draft), null);
  assert.deepEqual(model.buildStandingOrderMutation({ kind: 'edit', id: 9 }, draft), {
    path: '/api/standing-orders/9', method: 'PATCH',
    payload: {
      name: 'pr-early', row_version: 5,
      target: 'group:alpha', role: 'reviewer',
      summary: 'Push the PR early.', trigger_event: 'session.start',
      hook_selectors: [],
      sources: ['compact', 'resume'], match_field: '', match_regex: '',
      timing: 'same-continuation',
      cadence: 'once-per-generation', cooldown_seconds: 90, debounce_seconds: 0, enabled: false,
    },
  });

  const any = model.createStandingOrderDraft({
    name: 'all-boundaries', target: 'agt_target', summary: 'Remember.',
  });
  assert.equal(any.sourceMode, 'any');
  assert.deepEqual(model.buildStandingOrderMutation({ kind: 'create' }, any), {
    path: '/api/standing-orders', method: 'POST',
    payload: {
      name: 'all-boundaries', target: 'agt_target', role: '', summary: 'Remember.',
      trigger_event: 'session.start', hook_selectors: [],
      sources: [], match_field: '', match_regex: '',
      timing: 'same-continuation',
      cadence: 'always', cooldown_seconds: 0, debounce_seconds: 0, enabled: true,
    },
  });
  any.sourceMode = 'selected';
  assert.equal(model.validateStandingOrderDraft({ kind: 'create' }, any).code, 'sources');
  any.sourceMode = 'any';
  any.cooldownSeconds = -1;
  assert.equal(model.validateStandingOrderDraft({ kind: 'create' }, any).code, 'cooldown');
  any.cooldownSeconds = 0;
  any.debounceSeconds = 5;
  assert.equal(model.validateStandingOrderDraft({ kind: 'create' }, any).code, 'debounce-timing');
  any.timing = 'next-turn';
  assert.equal(model.validateStandingOrderDraft({ kind: 'create' }, any), null);
  assert.equal(model.standingOrderDraftDirty(draft,
    model.createStandingOrderDraft(model.standingOrderToPrefill(order))), false);

  const global = model.createStandingOrderDraft(model.standingOrderToPrefill({
    name: 'global-order', target: { kind: 'global' }, summary: 'For everyone.',
    trigger: { event: 'session.start', sources: [] },
    timing: 'same-continuation', cadence: 'always', enabled: true,
  }));
  assert.equal(global.target.mode, 'global');
  assert.equal(model.validateStandingOrderDraft({ kind: 'create' }, global), null);
  assert.equal(
    model.buildStandingOrderMutation({ kind: 'create' }, global).payload.target,
    'global',
  );

  const prompt = model.createStandingOrderDraft({
    name: 'deploy-prompt', target: 'agt_target', summary: 'Use the release checklist.',
    triggerEvent: 'user.prompt', matchField: 'prompt', matchRegex: '(?i)\\bdeploy\\b',
  });
  assert.equal(model.validateStandingOrderDraft({ kind: 'create' }, prompt), null);
  assert.deepEqual(model.buildStandingOrderMutation({ kind: 'create' }, prompt).payload, {
    name: 'deploy-prompt', target: 'agt_target', role: '',
    summary: 'Use the release checklist.', trigger_event: 'user.prompt',
    hook_selectors: [],
    sources: [], match_field: 'prompt', match_regex: '(?i)\\bdeploy\\b',
    timing: 'same-continuation', cadence: 'always', cooldown_seconds: 0,
    debounce_seconds: 0, enabled: true,
  });
  prompt.matchRegex = ' deploy ';
  assert.equal(model.buildStandingOrderMutation({ kind: 'create' }, prompt).payload.match_regex,
    ' deploy ', 'regex whitespace is meaningful and must be preserved');
  prompt.matchRegex = '(?=deploy)';
  assert.equal(model.validateStandingOrderDraft({ kind: 'create' }, prompt).code, 'match-regex-re2');
});
