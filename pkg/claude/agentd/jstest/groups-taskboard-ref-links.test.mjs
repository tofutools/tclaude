// A task's tracker refs (groups-taskboard-card.js OneRef): each shows its
// tracker's badge and its id as a link that opens in a new tab, and a ref
// its node could not link says why in its tooltip.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const task = (refs) => ({ id: 'tk_00000001', title: 't', ref: refs[0], refs });

test('tracker refs are badged links that open in a new tab', async (t) => {
  const harness = await createPreactHarness(t);
  const { RefChip } = await harness.importDashboardModule('js/groups-taskboard-card.js');
  const refs = [
    { kind: 'awb', label: 'tcl-8ab4sp', url: 'https://awb.example/#/issues/tcl-8ab4sp', local: true, id: 'tcl-8ab4sp', tracker: { status: 'open', title: 'Runner seam' } },
    { kind: 'linear', label: 'LIN-12', url: 'https://linear.app/acme/issue/LIN-12' },
    { kind: 'github', label: 'owner/repo#7', url: 'https://github.com/owner/repo/issues/7' },
  ];
  await harness.mount(harness.html`<div id="card"><${RefChip} task=${task(refs)} /></div>`);
  const card = harness.document.querySelector('#card');
  const issues = [...card.querySelectorAll('.tb-ref-issue')];
  assert.equal(issues.length, 3);
  assert.deepEqual(issues.map((el) => el.querySelector('.tb-ref-kind').textContent), ['awb', 'Linear', 'GitHub']);
  for (const [i, el] of issues.entries()) {
    const a = el.querySelector('a.tb-ref');
    assert.ok(a, `ref ${i} is a link`);
    assert.equal(a.getAttribute('href'), refs[i].url);
    assert.equal(a.getAttribute('target'), '_blank');
    assert.match(a.getAttribute('rel'), /noopener/);
  }
  assert.equal(issues[0].querySelector('.tb-ref-status').textContent, 'open', 'the status where known');
  assert.match(issues[0].querySelector('a').getAttribute('title'), /Runner seam/);
});

test('a ref its node could not link says why', async (t) => {
  const harness = await createPreactHarness(t);
  const { RefChip } = await harness.importDashboardModule('js/groups-taskboard-card.js');
  const refs = [
    { kind: 'awb', label: 'tcl-8ab4sp', link_note: 'no awb server URL is configured' },
    { kind: 'linear', label: 'LIN-12' },
  ];
  await harness.mount(harness.html`<div id="card"><${RefChip} task=${task(refs)} /></div>`);
  const card = harness.document.querySelector('#card');
  assert.equal(card.querySelector('a'), null, 'nothing to follow');
  const [awb, lin] = [...card.querySelectorAll('.tb-ref-unlinked')];
  assert.match(awb.getAttribute('title'), /not linked: no awb server URL is configured/);
  assert.match(lin.getAttribute('title'), /not linked$/, 'a node from before link notes still says not linked');
});
