import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function response(status, body) {
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => body === undefined ? '' : JSON.stringify(body),
  };
}

test('message/reply actions preserve wire payloads and warn on partial backpressure', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogActions } = await harness.importDashboardModule('js/message-access-dialog-actions.js');
  const calls = [];
  const notices = [];
  let refreshes = 0;
  const fetchImpl = async (url, options) => {
    calls.push({ url, method: options.method, body: JSON.parse(options.body) });
    if (url === '/api/message') return response(200, {
      via_group: 'team',
      recipients: [{ queued: true }, { queued: false, queue_full: true }, { queued: false, error: 'insert failed' }],
    });
    return response(200, { queued: true });
  };
  const actions = createMessageAccessDialogActions({
    fetchImpl, notify: (message) => notices.push(message), refresh: async () => { refreshes++; },
  });
  const message = { from: 'agt_sender', to: 'group:team', subject: 's', body: 'b', role: 'dev', members: ['agt_a'] };
  await actions.sendMessage(message);
  await actions.replyHuman({ id: 17, body: 'answer', label: 'worker' });
  assert.deepEqual(calls, [
    { url: '/api/message', method: 'POST', body: message },
    { url: '/api/human-messages/reply', method: 'POST', body: { id: 17, body: 'answer' } },
  ]);
  assert.equal(notices[0], 'message saved for 1 recipient; 1 not queued (target backlog full); 1 not queued (delivery error)');
  assert.equal(notices[1], 'reply queued for worker');
  assert.equal(refreshes, 1);
});

test('action errors retain status, code, and server message for component retry gates', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogActions } = await harness.importDashboardModule('js/message-access-dialog-actions.js');
  const actions = createMessageAccessDialogActions({
    fetchImpl: async () => response(409, { error: 'agent went offline', code: 'offline' }),
  });
  await assert.rejects(
    actions.replyHuman({ id: 1, body: 'x', label: 'worker' }),
    (error) => error.message === 'agent went offline' && error.status === 409 && error.code === 'offline',
  );
});

test('operator message uploads its frozen attachment batch before posting the target payload', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogActions } = await harness.importDashboardModule('js/message-access-dialog-actions.js');
  const calls = [];
  const actions = createMessageAccessDialogActions({
    fetchImpl: async (url, options) => {
      calls.push({ url, options });
      return url === '/api/spawn-attachments'
        ? response(200, { token: 'batch-token' })
        : response(200, { id: 7 });
    },
  });
  const file = new Blob(['proof']);
  Object.defineProperty(file, 'name', { value: 'proof.txt' });
  await actions.sendOperatorMessage(Object.freeze({
    to: 'agt_worker', subject: 'evidence', body: '', files: Object.freeze([file]),
  }));
  assert.deepEqual(calls.map((call) => call.url), ['/api/spawn-attachments', '/api/operator-message']);
  assert.equal(calls[0].options.body.get('file').name, 'proof.txt');
  assert.deepEqual(JSON.parse(calls[1].options.body), {
    to: 'agt_worker', subject: 'evidence', body: '', attachment_token: 'batch-token',
  });
});

test('human reply uploads attachments before posting the reply token', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogActions } = await harness.importDashboardModule('js/message-access-dialog-actions.js');
  const calls = [];
  const actions = createMessageAccessDialogActions({
    fetchImpl: async (url, options) => {
      calls.push({ url, options });
      return url === '/api/spawn-attachments'
        ? response(200, { token: 'reply-batch' })
        : response(200, { queued: true });
    },
  });
  const file = new Blob(['screenshot']);
  Object.defineProperty(file, 'name', { value: 'shot.png' });
  await actions.replyHuman({ id: 17, body: '', files: [file], label: 'worker' });
  assert.deepEqual(calls.map((call) => call.url), ['/api/spawn-attachments', '/api/human-messages/reply']);
  assert.equal(calls[0].options.body.get('file').name, 'shot.png');
  assert.deepEqual(JSON.parse(calls[1].options.body), {
    id: 17, body: '', attachment_token: 'reply-batch',
  });
});

test('all-live announcement skips attachments and reports partial fan-out', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogActions } = await harness.importDashboardModule('js/message-access-dialog-actions.js');
  const calls = [];
  const notices = [];
  const actions = createMessageAccessDialogActions({
    fetchImpl: async (url, options) => {
      calls.push({ url, body: JSON.parse(options.body) });
      return response(200, { recipients: [{ queued: true }, { queued: false, queue_full: true }] });
    },
    notify: (message) => notices.push(message),
  });
  await actions.sendOperatorMessage(Object.freeze({
    to: '', subject: 'heads up', body: 'deploy now', files: Object.freeze([]), allLive: true,
  }));
  assert.deepEqual(calls, [{
    url: '/api/operator-message',
    body: { to: '', subject: 'heads up', body: 'deploy now', all_live: true },
  }]);
  assert.deepEqual(notices, ['announcement saved for 1 live agent; 1 not queued']);
});

test('accepted reply and sudo mutations do not await snapshot refresh before completion', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogActions } = await harness.importDashboardModule('js/message-access-dialog-actions.js');
  let refreshes = 0;
  let sudoBody = null;
  const actions = createMessageAccessDialogActions({
    fetchImpl: async (url, options) => {
      if (url === '/api/sudo') {
        sudoBody = JSON.parse(options.body);
        return response(200, { agent_id: 'agt_worker', grants: [{ id: 1 }] });
      }
      return response(200, { queued: true });
    },
    refresh: () => {
      refreshes++;
      return { then() { throw new Error('accepted mutation awaited snapshot refresh'); } };
    },
  });
  await actions.replyHuman({ id: 1, body: 'answer', label: 'worker' });
  await actions.grantSudo({ agentID: 'agt_worker', slugs: ['self.rename'], duration: '5m', reason: '' });
  assert.equal(refreshes, 2);
  assert.deepEqual(sudoBody, { agent_id: 'agt_worker', slugs: ['self.rename'], duration: '5m', reason: '' });
});

test('permission actions use mode-specific payloads and buffered saves strip defaults', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogActions } = await harness.importDashboardModule('js/message-access-dialog-actions.js');
  const calls = [];
  const notices = [];
  let buffered = null;
  let roleBuffered = null;
  const actions = createMessageAccessDialogActions({
    fetchImpl: async (url, options) => {
      calls.push({ url, body: JSON.parse(options.body) });
      return response(200, {});
    },
    notify: (message) => notices.push(message),
    words: (plain, wizard) => wizard,
  });
  await actions.savePermissions(
    { mode: 'group', group: 'team' },
    { 'groups.members.spawn': 'grant', 'agent.send': 'default' },
    { 'groups.members.spawn': { group: ['team'] } },
  );
  await actions.savePermissions(
    { mode: 'buffer', onSave: async (value) => { buffered = value; } },
    { 'groups.members.spawn': 'grant', 'agent.send': 'deny', 'self.rename': 'default' },
    { 'groups.members.spawn': { group: ['team'] } },
  );
  await actions.savePermissions(
    { mode: 'buffer', grantOnly: true, onSave: async (value) => { roleBuffered = value; } },
    { 'future.permission': 'grant', 'agent.send': 'deny' },
    { 'future.permission': { future_dimension: ['narrow'] } },
  );
  assert.deepEqual(calls, [{ url: '/api/groups/team', body: {
    permissions: [{ slug: 'groups.members.spawn', scope: { group: ['team'] } }],
  } }]);
  assert.equal(notices[0], 'team: 1 party boon bound');
  assert.deepEqual(buffered, {
    'groups.members.spawn': { effect: 'grant', scope: { group: ['team'] } },
    'agent.send': 'deny',
  });
  assert.deepEqual(roleBuffered, {
    'future.permission': { effect: 'grant', scope: { future_dimension: ['narrow'] } },
  }, 'grant-only blueprints keep canonical scopes and cannot emit denies');
});

test('group permission saves carry owner_scopes only when the editor touched it', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMessageAccessDialogActions } = await harness.importDashboardModule('js/message-access-dialog-actions.js');
  const calls = [];
  const actions = createMessageAccessDialogActions({
    fetchImpl: async (url, options) => {
      calls.push(JSON.parse(options.body));
      return response(200, {});
    },
    notify: () => {},
    words: (plain) => plain,
  });
  // Not edited: the field must be ABSENT, so the daemon leaves the stored
  // narrowing alone. Sending the box's current value would clear a narrowing
  // this build could not decode into it.
  await actions.savePermissions({ mode: 'group', group: 'team' }, { 'groups.members.spawn': 'grant' }, {}, null);
  // Edited to a real map, and edited to {} — the deliberate clear. `{}` is
  // meaningful, which is why the action tests against null and not truthiness.
  const narrowing = { 'groups.members.spawn': { spawn_profile: ['reviewer'] } };
  await actions.savePermissions({ mode: 'group', group: 'team' }, { 'groups.members.spawn': 'grant' }, {}, narrowing);
  await actions.savePermissions({ mode: 'group', group: 'team' }, { 'groups.members.spawn': 'grant' }, {}, {});
  // A present-but-empty per-grant scope is the explicit clear arm. Omitting
  // the slug from the scopes map is the legacy "leave its scope alone" arm.
  await actions.savePermissions({ mode: 'group', group: 'team' }, { 'groups.members.spawn': 'grant' }, { 'groups.members.spawn': {} }, null);
  assert.deepEqual(calls, [
    { permissions: ['groups.members.spawn'] },
    { permissions: ['groups.members.spawn'], owner_scopes: narrowing },
    { permissions: ['groups.members.spawn'], owner_scopes: {} },
    { permissions: [{ slug: 'groups.members.spawn', scope: {} }] },
  ]);
});
