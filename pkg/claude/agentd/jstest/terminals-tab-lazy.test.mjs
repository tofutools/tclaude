import test from 'node:test';
import assert from 'node:assert/strict';
import {
  openTerminalPane, openTermModal, registerTerminalShellController,
} from '../dashboard/js/terminals-tab.js';

test('invalid and canceled terminal requests do not prepare the runtime', async () => {
  const opened = [];
  let prepares = 0;
  const controller = {
    openPane(seed) { opened.push(seed); return seed; },
    openModal(options) { opened.push(options); return options; },
  };
  const unregister = registerTerminalShellController(controller, async () => { prepares += 1; });

  assert.equal(await openTerminalPane(null), null);
  assert.equal(await openTerminalPane(Promise.resolve(null)), null);
  assert.equal(openTermModal({ wsPath: 'https://elsewhere.test/socket' }), null);
  assert.equal(prepares, 0);
  assert.deepEqual(opened, []);

  await openTerminalPane({ ws: '/terminal', label: 'terminal' });
  assert.equal(prepares, 1);
  assert.equal(opened.length, 1);
  assert.equal(opened[0].initialRetry, true, 'first web-terminal attaches get bounded retry by default');
  await openTerminalPane({ ws: '/terminal-no-retry', initialRetry: false });
  assert.equal(opened.at(-1).initialRetry, false, 'callers can explicitly opt out');
  unregister();
});
