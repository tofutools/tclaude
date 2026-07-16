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
      owner: 'agt_owner', interval: '5m', cron_expr: '',
    },
  }, 'an untouched solo target is omitted from PATCH');

  const retargeted = { ...draft, target: { ...draft.target, target: 'agt_next' } };
  assert.deepEqual(model.buildCronMutation(edit, retargeted).payload, {
    name: 'daily', body: 'Report', subject: 'Status', enabled: true,
    run_immediately: false,
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
      enabled: false, run_immediately: false, cron_expr: '@daily', role: 'dev',
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
});
