import assert from 'node:assert/strict';
import test from 'node:test';

import {
  createPaletteWindowOperator,
  webWindowTargets,
} from '../dashboard/js/palette-window-ops.js';

const agents = [
  { online: true, agent_id: 'agt_alpha', conv_id: 'conv-alpha', title: 'Alpha' },
  { online: true, conv_id: 'conv-beta', title: '' },
  { online: false, agent_id: 'agt_offline', conv_id: 'conv-offline', title: 'Offline' },
  { online: true, agent_id: 'agt_duplicate', conv_id: 'conv-alpha', title: 'Duplicate' },
  { online: true, agent_id: 'agt_missing_conv', title: 'Incomplete' },
];

test('web focus opens each running agent in the terminal shell without native HTTP', async () => {
  const requests = [];
  const opened = [];
  const notices = [];
  const run = createPaletteWindowOperator({
    fetchImpl: async (...args) => {
      requests.push(args);
      throw new Error('native window request must not run');
    },
    notify: (...args) => notices.push(args),
    openWebWindowPane: (...args) => opened.push(args),
    closeTerminalsForWindowOp: () => {},
  });
  const targets = webWindowTargets(agents);
  const result = await run(
    { direction: 'focus', scope: 'all' },
    'focus all windows',
    { webTerminal: true, targets },
  );

  assert.deepEqual(targets, [
    { selector: 'agt_alpha', label: 'Alpha' },
    { selector: 'conv-beta', label: 'conv-bet' },
  ]);
  assert.deepEqual(opened, [
    ['agt_alpha', 'Alpha'],
    ['conv-beta', 'conv-bet'],
  ]);
  assert.deepEqual(requests, []);
  assert.deepEqual(notices, [['focus all windows: 2 focused']]);
  assert.deepEqual(result, {
    direction: 'focus', scope: 'all', targeted: 2, focused: 2, terminal: 'web',
  });
});

test('hide with web terminals still detaches through agentd and closes returned panes', async () => {
  const requests = [];
  const closed = [];
  const notices = [];
  const response = {
    direction: 'unfocus', scope: 'all', targeted: 2,
    detached: 1, no_window: 1, failed: 0,
    agents: [
      { agent_id: 'agt_alpha', conv_id: 'conv-alpha', outcome: 'detached' },
      { agent_id: 'agt_beta', conv_id: 'conv-beta', outcome: 'no_window' },
    ],
  };
  const run = createPaletteWindowOperator({
    fetchImpl: async (...args) => {
      requests.push(args);
      return new Response(JSON.stringify(response), { status: 200 });
    },
    notify: (...args) => notices.push(args),
    openWebWindowPane: () => {
      throw new Error('hide must not open a pane');
    },
    closeTerminalsForWindowOp: (outcomes) => closed.push(outcomes),
  });
  const result = await run(
    { direction: 'unfocus', scope: 'all' },
    'hide all windows',
    { webTerminal: true, targets: webWindowTargets(agents) },
  );

  assert.equal(requests.length, 1);
  assert.equal(requests[0][0], '/api/agent-windows');
  assert.deepEqual(JSON.parse(requests[0][1].body), {
    direction: 'unfocus', scope: 'all',
  });
  assert.deepEqual(closed, [response.agents]);
  assert.deepEqual(notices, [['hide all windows: 1 detached', false]]);
  assert.deepEqual(result, response);
});
