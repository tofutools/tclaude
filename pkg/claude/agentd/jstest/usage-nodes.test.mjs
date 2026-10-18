import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const self = { id: 'inst_self', name: 'desk', local: true, color: '#58a6ff' };
const forge = { id: 'inst_forge', name: 'forge', color: '#d29922' };
const mac = { id: 'inst_mac0', name: 'mac', color: '#a371f7' };
const lab = { id: 'inst_lab0', name: 'lab', color: '#3fb950' };
const quota = (five, seven) => ({ available: true, five_hour: { pct: five }, seven_day: { pct: seven } });

test('the fused Usage summary sums spend across nodes but never quota', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/usage-nodes.js');
  const selfUsage = { ...quota(4, 5), today_cost_usd: 1.5, total_cost_usd: 41.2 };
  const entries = {
    inst_forge: { usage: { ...quota(62, 31), today_cost_usd: 2, total_cost_usd: 27.85 }, peerView: { level: 'unrestricted' } },
    inst_mac0: { usage: { available: false }, peerView: { level: 'restricted', included: [], omitted: [{ feature: 'costs', requires: 'costs.read' }] } },
  };
  const { rows, total } = mod.fusedUsageRows([self, forge, mac, lab], selfUsage, entries);
  assert.deepEqual(rows.map((r) => [r.node.name, r.why ? 'locked' : r.peak]), [['desk', 5], ['forge', 62], ['mac', 'locked'], ['lab', 'locked']]);
  assert.equal(rows[3].pending, true, 'a peer not read yet is loading, not refused');
  assert.match(rows[2].why, /mac does not share usage or costs with you \(needs costs\.read\)/);
  assert.equal(total.nodes, 2);
  assert.equal(total.today.toFixed(2), '3.50');
  assert.equal(total.mtd.toFixed(2), '69.05');

  const mounted = await harness.mount(harness.html`<${mod.FusedUsageSummary} nodes=${[self, forge, mac]} selfUsage=${selfUsage} entries=${entries} fmt=${(v) => `$${v.toFixed(2)}`} />`);
  const text = mounted.container.querySelector('tfoot').textContent;
  assert.match(text, /Total \(2 nodes\)\$3\.50\$69\.05per node/, 'the quota column has no total');
  assert.ok(mounted.container.querySelector('tbody tr:nth-child(3) .lock-why'), 'a withheld node is locked with its reason');
  await mounted.unmount();
});
