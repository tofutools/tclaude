import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const dashboardStub = `
  export const lastSnapshot = {
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
    links: [],
  };
  export function sudoBadge() { return ''; }
  export function setLastSnapshot() {}
  export function webTerminalDefault() { return false; }
`;

test('real group renderer moves activity to the leaf-most visible headers', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', dashboardStub);
  await harness.replaceDashboardModule('js/refresh.js', `
    export const sudoByConv = {};
    export let hoveredGroupKey = null;
  `);
  await harness.replaceDashboardModule('js/prefs.js', `
    const values = new Map();
    export const dashPrefs = {
      getItem: (key) => values.get(key) ?? null,
      setItem: (key, value) => values.set(key, String(value)),
      removeItem: (key) => values.delete(key),
    };
  `);

  const [{ renderGroups }, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/render.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  const member = (conv_id) => ({
    conv_id, agent_id: `agt_${conv_id}`, title: conv_id, online: true,
    state: { status: 'working' },
  });
  const groups = [
    { name: 'root', members: [member('root')], online: 1 },
    { name: 'child', parent: 'root', members: [member('child')], online: 1 },
    { name: 'grandchild', parent: 'child', members: [member('grandchild')], online: 1 },
  ];
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const paint = () => { host.innerHTML = renderGroups(groups); };
  const activity = (name) => {
    const details = host.querySelector(`details[data-group-key="${name}"]`);
    const summary = details.querySelector(':scope > summary');
    return summary.querySelector('.ga-regular .actbot')?.getAttribute('aria-label') || '';
  };

  paint();
  assert.equal(activity('root'), '3 working', 'the folded root owns its hidden subtree');

  dashPrefs.setItem('tclaude.dash.group.root', '1');
  paint();
  assert.equal(activity('root'), '1 working');
  assert.equal(activity('child'), '2 working', 'the folded visible child owns its hidden grandchild');

  dashPrefs.setItem('tclaude.dash.group.child', '1');
  paint();
  assert.equal(activity('root'), '1 working');
  assert.equal(activity('child'), '1 working');
  assert.equal(activity('grandchild'), '1 working');

  const pending = [{
    name: 'pending-root', members: [], online: 0,
    pending: [{ label: 'blocked-spawn', group: 'pending-root', online: true }],
  }];
  host.innerHTML = renderGroups(pending);
  assert.equal(host.querySelector('details[data-group-key="pending-root"]').hasAttribute('open'), true,
    'a pending group defaults open without a preference');
  dashPrefs.setItem('tclaude.dash.group.pending-root', '0');
  host.innerHTML = renderGroups(pending);
  assert.equal(host.querySelector('details[data-group-key="pending-root"]').hasAttribute('open'), false,
    'an explicit fold overrides pending-group default-open behavior');
});

test('production disclosure binder persists an intentional fold as zero', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', dashboardStub);
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response('{}', {
    status: 200, headers: { 'Content-Type': 'application/json' },
  });
  t.after(() => { globalThis.fetch = previousFetch; });

  const [{ bindDetailsPersistence, bindGroupTitleToggle }, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/refresh.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  harness.document.body.innerHTML = `
    <div id="groups-list">
      <details data-group-key="pending-root" open>
        <summary><strong class="group-name">pending-root</strong></summary>
      </details>
    </div>`;
  const details = harness.document.querySelector('details');
  const title = details.querySelector('.group-name');
  bindDetailsPersistence();
  bindGroupTitleToggle();

  harness.fireEvent(title, 'click', { detail: 1 });
  details.open = false;
  harness.fireEvent(details, 'toggle');

  assert.equal(dashPrefs.getItem('tclaude.dash.group.pending-root'), '0');

  harness.fireEvent(details, 'toggle');
  assert.equal(dashPrefs.getItem('tclaude.dash.group.pending-root'), '0',
    'a later reconciliation toggle preserves the explicit fold');

  const recreated = harness.document.createElement('details');
  recreated.setAttribute('data-group-key', 'ordinary-default-closed');
  harness.document.querySelector('#groups-list').append(recreated);
  harness.fireEvent(recreated, 'toggle');
  assert.equal(dashPrefs.getItem('tclaude.dash.group.ordinary-default-closed'), null,
    'reconciliation noise does not turn default-closed groups into explicit folds');
});
