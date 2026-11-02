// A worker an external steward started says so on its row (tcl-blr0mp), next
// to the harness line it would show anyway: the operator should be able to
// tell at a glance who started it, without losing what the row normally says.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function harnessLine(t) {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { HarnessLine } = await harness.importDashboardModule('js/groups-member-table.js');
  return async (member) => {
    const mounted = await harness.mount(harness.html`<${HarnessLine} member=${member} />`);
    const text = mounted.container.textContent;
    await mounted.unmount();
    return text;
  };
}

test('a worker names the external steward that started it', async (t) => {
  const render = await harnessLine(t);
  const state = { harness: 'claude', model: 'Opus 4.8 (1M context)' };
  const plain = await render({ conv_id: 'c1', online: true, state });
  assert.doesNotMatch(plain, /Started by/);
  const worker = await render({ conv_id: 'c1', online: true, state, spawned_by_external_steward: 'assistant' });
  assert.match(worker, /Started by assistant/);
  assert.ok(worker.startsWith(plain), 'the usual harness line stays');
});
