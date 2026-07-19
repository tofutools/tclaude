import test from 'node:test';
import assert from 'node:assert/strict';
import {
  openTerminalPane, openTermModal, registerTerminalShellController,
} from '../dashboard/js/terminals-tab.js';

test('invalid and canceled terminal requests do not prepare the runtime', async () => {
  const opened = [];
  let prepares = 0;
  const controller = {
    openPane(seed, options) { opened.push([seed, options]); return seed; },
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
  assert.equal(opened[0][0].initialRetry, true, 'first web-terminal attaches get bounded retry by default');
  assert.deepEqual(opened[0][1], { reveal: true }, 'plain terminal requests reveal the tab');
  await openTerminalPane({ ws: '/terminal-no-retry', initialRetry: false }, { reveal: false });
  assert.equal(opened.at(-1)[0].initialRetry, false, 'callers can explicitly opt out');
  assert.deepEqual(opened.at(-1)[1], { reveal: false }, 'background requests reach the shell controller');
  unregister();
});
