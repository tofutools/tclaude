import test from 'node:test';
import assert from 'node:assert/strict';
import { loadXtermRuntime } from '../dashboard/js/xterm-loader.js';

test('xterm runtime is appended only on demand and concurrent callers share it', async () => {
  const globalRef = {};
  const appended = [];
  const fetches = [];
  let authorized = false;
  const fetchImpl = async (url, options) => {
    fetches.push({ url, options });
    return authorized ? { ok: true, status: 200 } : { ok: false, status: 403 };
  };
  const documentRef = {
    createElement: () => ({ dataset: {} }),
    head: {
      appendChild(script) {
        appended.push(script);
      },
    },
  };

  assert.equal(appended.length, 0, 'importing the loader does not fetch xterm');
  assert.equal(fetches.length, 0);
  await assert.rejects(
    loadXtermRuntime({ documentRef, globalRef, fetchImpl }),
    /preflight failed \(403\)/,
  );
  assert.equal(appended.length, 0, 'a failed auth preflight never injects the script');

  authorized = true;
  const first = loadXtermRuntime({ documentRef, globalRef, fetchImpl });
  const second = loadXtermRuntime({ documentRef, globalRef, fetchImpl });
  assert.equal(first, second, 'in-flight loads are deduplicated');
  await Promise.resolve();
  assert.equal(appended.length, 1);
  assert.deepEqual(fetches.at(-1), {
    url: '/static/vendor/xterm/xterm.min.js',
    options: { method: 'HEAD', credentials: 'same-origin' },
  });
  assert.equal(appended[0].src, '/static/vendor/xterm/xterm.min.js');
  assert.equal(appended[0].dataset.tclaudeXtermRuntime, '1');

  globalRef.Terminal = function Terminal() {};
  appended[0].onload();
  assert.equal(await first, true);
  assert.equal(await loadXtermRuntime({ documentRef, globalRef, fetchImpl }), true);
  assert.equal(appended.length, 1, 'a ready runtime is never appended again');
});
