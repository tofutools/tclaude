// js/phone-process-runs.js: process runs on a phone, read from the shared
// context fixture and decided through a fake run API.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

const FIXTURE = JSON.parse(readFileSync(new URL('./process-context-fixture.json', import.meta.url), 'utf8'));

function fakeSource(harness, context = FIXTURE) {
  const source = {
    context: harness.signals.signal(context), status: harness.signals.signal('ready'), error: harness.signals.signal(''), refreshes: 0,
  };
  source.refresh = () => { source.refreshes++; return Promise.resolve(); };
  return source;
}

function fakeApi() {
  const api = { decisions: [], holds: [] };
  api.runReads = [];
  api.eventReads = 0;
  api.getRun = async (runId) => (api.runReads.push(runId), {
    id: runId, blocked: runId === 'run_migrate' ? [{ nodeId: 'apply', attempt: 1, reason: 'migration lock held' }] : [],
    awaitingDecisions: runId === 'run_release' ? [{ nodeId: 'review', verdicts: ['approved', 'changes'], attempt: 2 }] : [],
  });
  api.events = [];
  api.listEvents = async () => { api.eventReads += 1; return { events: api.events, more: false }; };
  api.decide = async (runId, request) => { api.decisions.push([runId, request]); return { started: true }; };
  api.pause = async (runId) => {
    api.holds.push(['pause', runId]);
    if (api.refusePause) throw new Error('403 process.runs.manage required');
  };
  api.resume = async (runId) => { api.holds.push(['resume', runId]); };
  return api;
}

async function settle(harness) {
  for (let i = 0; i < 4; i++) await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
}

async function load(t) {
  const harness = await createPreactHarness(t);
  const [mod, { phonePage }] = await Promise.all([
    harness.importDashboardModule('js/phone-process-runs.js'),
    harness.importDashboardModule('js/phone-page.js'),
  ]);
  return { harness, mod, phonePage };
}

test('phoneRuns lists each run once: waiting on you, then running, then done', async (t) => {
  const { mod } = await load(t);
  const { waiting, running, done } = mod.phoneRuns(FIXTURE);
  assert.deepEqual(waiting.map((item) => item.run.runId), ['run_migrate', 'run_release']);
  assert.deepEqual(running.map((item) => item.run.runId), ['run_reindex'], 'a waiting run is not listed again as running');
  assert.deepEqual(done.map((item) => item.run.runId), ['run_hotfix']);
  assert.equal(waiting[0].where, 'frontend-squad');
  assert.deepEqual(mod.phoneRuns(null), { waiting: [], running: [], done: [] });

  const many = { runs: {}, groups: [{ group: 'g', done: [] }] };
  for (let i = 0; i < 15; i++) {
    many.runs[`r${i}`] = { runId: `r${i}`, status: 'completed', updatedAt: `2026-10-14T10:${String(i).padStart(2, '0')}:00Z` };
    many.groups[0].done.push(`r${i}`);
  }
  const recent = mod.phoneRuns(many).done.map((item) => item.run.runId);
  assert.equal(recent.length, mod.DONE_SHOWN);
  assert.equal(recent[0], 'r14', 'the newest finished run first');
});

test('runPosition names the step a run waits or works on, or how it ended', async (t) => {
  const { mod } = await load(t);
  assert.match(mod.runPosition(FIXTURE.runs.run_release), /^step \d+\/\d+ · Review gate$/);
  assert.match(mod.runPosition(FIXTURE.runs.run_reindex), /· Backfill shards$/);
  assert.match(mod.runPosition(FIXTURE.runs.run_hotfix), /^completed/);
});

test('the Runs list opens a run, keeps the context fresh, and says what is desktop only', async (t) => {
  const { harness, mod } = await load(t);
  const source = fakeSource(harness);
  const opened = [];
  const mounted = await harness.mount(harness.html`<${mod.RunsList} source=${source} onOpen=${(id) => opened.push(id)} />`);
  const root = mounted.container;
  const sections = [...root.querySelectorAll('.phone-run-section')].map((section) => section.getAttribute('aria-label'));
  assert.deepEqual(sections, ['Waiting on you', 'Running', 'Done']);
  assert.equal(root.querySelector('.phone-run-row.waiting[data-run="run_release"] .phone-pill').textContent, 'Decide');
  assert.ok(root.querySelector('[data-run="run_release"] .phone-pill-key'), 'Decide is amber');
  assert.equal(root.querySelector('[data-run="run_reindex"] .phone-pill').textContent, 'Running');
  assert.equal(root.querySelector('[data-run="run_hotfix"] .phone-pill').textContent, 'Done');
  assert.ok(root.querySelector('.phone-desktop-only').textContent.includes('desktop only'));
  assert.equal(source.refreshes, 1, 'read once on mount');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:snapshot')));
  assert.equal(source.refreshes, 2, 'and again on each snapshot poll');
  await harness.act(() => root.querySelector('.phone-run-row[data-run="run_release"] .phone-run-open').click());
  assert.deepEqual(opened, ['run_release']);
});

test('a gate card on Now approves in one tap, and asks for a note before changes', async (t) => {
  const { harness, mod } = await load(t);
  const { PhoneSheetHost, closePhoneSheet } = await harness.importDashboardModule('js/phone-sheet.js');
  t.after(() => closePhoneSheet());
  const source = fakeSource(harness);
  const api = fakeApi();
  const item = mod.gateItems(FIXTURE).find((entry) => entry.run.runId === 'run_release');
  const mounted = await harness.mount(harness.html`<div><${mod.GateCard} item=${item} api=${api} source=${source} /><${PhoneSheetHost} /></div>`);
  await settle(harness);
  const card = mounted.container.querySelector('.phone-gate-card');
  assert.match(card.querySelector('.phone-gate-title').textContent, /Review gate/);
  assert.equal(card.querySelector('textarea'), null, 'no note box on the card');

  // Request changes: a sheet takes the note first.
  await harness.act(() => card.querySelector('[data-run-verdict="changes"]').click());
  const sheet = mounted.container.querySelector('[role="dialog"]');
  assert.ok(sheet, 'the note sheet opens');
  await harness.input(sheet.querySelector('textarea'), 'fix the copy');
  await harness.act(() => harness.getByRole(sheet, 'button', { name: 'Request changes' }).click());
  await settle(harness);
  assert.deepEqual(api.decisions, [['run_release', { nodeId: 'review', verdict: 'changes', evidence: 'fix the copy', attempt: 2 }]]);
  assert.equal(mounted.container.querySelector('[role="dialog"]'), null, 'the sheet closes once it is sent');
  assert.match(card.querySelector('.phone-gate-done').textContent, /Recorded: Request changes/);
  assert.equal(source.refreshes, 1, 'the context is read again after the verdict');
});

test('approve on a gate card goes in one tap, without a note', async (t) => {
  const { harness, mod } = await load(t);
  const api = fakeApi();
  const item = mod.gateItems(FIXTURE).find((entry) => entry.run.runId === 'run_release');
  const mounted = await harness.mount(harness.html`<${mod.GateCard} item=${item} api=${api} source=${fakeSource(harness)} />`);
  await settle(harness);
  await harness.act(() => mounted.container.querySelector('[data-run-verdict="approved"]').click());
  await settle(harness);
  assert.deepEqual(api.decisions, [['run_release', { nodeId: 'review', verdict: 'approved', evidence: '', attempt: 2 }]]);
});

test('the run page tops the steps with the gate, marks your step, and pauses from the top bar', async (t) => {
  const { harness, mod, phonePage } = await load(t);
  const source = fakeSource(harness);
  const api = fakeApi();
  const backs = [];
  const mounted = await harness.mount(harness.html`<${mod.RunPage} runId="run_release" api=${api} source=${source} back=${() => backs.push(1)} />`);
  await settle(harness);
  const root = mounted.container;
  assert.ok(root.querySelector('.phone-box-key.phone-run-gate .phone-gate [data-run-verdict="approved"]'), 'the gate is on top, in an amber card');
  assert.ok(root.querySelector('[data-run-verdict="approved"]').classList.contains('phone-btn-ok'), 'Approve is green');
  assert.ok(root.querySelector('[data-run-verdict="changes"]').classList.contains('phone-btn-no'), 'request changes is red');
  assert.equal(root.querySelector('.phone-run-gate').getAttribute('aria-label'), 'Gate: Review gate');
  const mine = root.querySelector('.phone-run-step.mine');
  assert.match(mine.textContent, /Review gate/);
  assert.match(mine.textContent, /you/);
  assert.equal(phonePage.value.title, 'release-2.4');
  assert.equal(phonePage.value.actions[0].icon, 'Pause');
  await harness.act(() => phonePage.value.actions[0].onClick());
  await settle(harness);
  assert.deepEqual(api.holds, [['pause', 'run_release']]);
  phonePage.value.onBack();
  assert.deepEqual(backs, [1]);
});

test('a finished run offers no pause, and a run gone from the context says so', async (t) => {
  const { harness, mod, phonePage } = await load(t);
  const source = fakeSource(harness);
  const api = fakeApi();
  const done = await harness.mount(harness.html`<${mod.RunPage} runId="run_hotfix" api=${api} source=${source} back=${() => {}} />`);
  await settle(harness);
  assert.deepEqual(phonePage.value.actions, []);
  assert.equal(done.container.querySelector('.phone-gate'), null);
  const gone = await harness.mount(harness.html`<${mod.RunPage} runId="run_nope" api=${api} source=${source} back=${() => {}} />`);
  assert.match(gone.container.textContent, /no longer listed/);
});

test('runs outside the group indexes are listed too, by their agent', async (t) => {
  const { mod } = await load(t);
  const context = {
    runs: {
      run_solo: { runId: 'run_solo', name: 'solo', status: 'running', needsYou: true, action: 'awaiting_decision',
        scope: { kind: 'agent', agentId: 'agt_x' }, nodes: [] },
      run_lost: { runId: 'run_lost', name: 'lost', status: 'running', needsYou: true, action: 'orphaned', scope: { kind: 'group' }, nodes: [] },
    },
    groups: [],
  };
  const { waiting } = mod.phoneRuns(context, (id) => (id === 'agt_x' ? 'xavier' : ''));
  assert.deepEqual(waiting.map((item) => [item.run.runId, item.where]), [['run_solo', 'agent xavier'], ['run_lost', 'no group']]);
  assert.deepEqual(mod.gateItems(context).map((item) => item.run.runId), ['run_solo'], 'only the gate goes to Now');
});

test('Now gets only runs waiting on a verdict; Runs tags the rest as needing you', async (t) => {
  const { harness, mod } = await load(t);
  assert.deepEqual(mod.gateItems(FIXTURE).map((item) => item.run.runId), ['run_release'], 'the blocked run has no gate to answer');
  const mounted = await harness.mount(harness.html`<${mod.RunsList} source=${fakeSource(harness)} onOpen=${() => {}} />`);
  const tag = (id) => mounted.container.querySelector(`.phone-run-row[data-run="${id}"] .phone-pill`).textContent;
  assert.equal(tag('run_release'), 'Decide');
  assert.equal(tag('run_migrate'), 'Needs you');
});

test('a run blocked on the operator says why and sends the fix to the desktop', async (t) => {
  const { harness, mod } = await load(t);
  const mounted = await harness.mount(harness.html`<${mod.RunPage} runId="run_migrate" api=${fakeApi()} source=${fakeSource(harness)} back=${() => {}} />`);
  await settle(harness);
  const wait = mounted.container.querySelector('.phone-run-wait');
  assert.match(wait.textContent, /A branch is blocked\. migration lock held\./);
  assert.match(wait.textContent, /desktop only/);
  assert.equal(mounted.container.querySelector('.phone-gate'), null);
});

test('a refused pause is shown, not swallowed', async (t) => {
  const { harness, mod, phonePage } = await load(t);
  const api = fakeApi();
  api.refusePause = true;
  const mounted = await harness.mount(harness.html`<${mod.RunPage} runId="run_release" api=${api} source=${fakeSource(harness)} back=${() => {}} />`);
  await settle(harness);
  await harness.act(() => phonePage.value.actions[0].onClick());
  await settle(harness);
  assert.match(mounted.container.querySelector('.ui-error-box[role="alert"]').textContent, /Could not pause the run: 403/);
});

test('a gate card reads the run view only, never its event history', async (t) => {
  const { harness, mod } = await load(t);
  const api = fakeApi();
  const item = mod.gateItems(FIXTURE)[0];
  await harness.mount(harness.html`<${mod.GateCard} item=${item} api=${api} source=${fakeSource(harness)} />`);
  await settle(harness);
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:snapshot')));
  await settle(harness);
  assert.deepEqual(api.runReads, ['run_release', 'run_release'], 'read on mount and on the poll');
  assert.equal(api.eventReads, 0);
});

test("the run page's gate says how long it has waited, and a finished step who did it and when", async (t) => {
  const { harness, mod } = await load(t);
  const api = fakeApi();
  const ago = (min) => new Date(Date.now() - min * 60000).toISOString();
  const steps = FIXTURE.runs.run_release.nodes;
  const done = steps.find((node) => node.state === 'done' && node.kind !== 'start' && node.kind !== 'end');
  api.events = [
    ...(done ? [{ sequence: 1, nodeId: done.id, kind: 'node_completed', occurredAt: ago(30) }] : []),
    { sequence: 2, nodeId: 'review', kind: 'decision_awaited', occurredAt: ago(12) },
  ];
  const mounted = await harness.mount(harness.html`<${mod.RunPage} runId="run_release" api=${api} source=${fakeSource(harness)} back=${() => {}} />`);
  await settle(harness);
  assert.equal(mounted.container.querySelector('.phone-run-gate .phone-gate-since').textContent, 'Waiting on you · 12m ago');
  if (done) {
    const row = [...mounted.container.querySelectorAll('.phone-run-step.s-done')].find((li) => li.textContent.includes(done.label));
    assert.match(row.querySelector('.phone-run-step-meta').textContent, /30m ago$/);
  }
});

test('verdicts are said the mock\'s way: no glyph, capitalised', async (t) => {
  const { mod } = await load(t);
  assert.equal(mod.verdictLabel('approved'), 'Approve');
  assert.equal(mod.verdictLabel('changes'), 'Request changes');
  assert.equal(mod.verdictLabel('ship-it'), 'ship-it', 'a verdict of its own is said as written');
  assert.equal(mod.verdictLabel('+1'), '+1');
  assert.equal(mod.verdictLabel('-1'), '-1');
});

test('runPill: what a run row wears', async (t) => {
  const { mod } = await load(t);
  assert.deepEqual(mod.runPill({ action: 'awaiting_decision' }, true), { text: 'Decide', tone: 'key' });
  assert.deepEqual(mod.runPill({ action: 'blocked' }, true), { text: 'Needs you', tone: 'err' });
  assert.deepEqual(mod.runPill({ status: 'running', paused: { by: 'x' } }), { text: 'Paused', tone: 'idle' });
  assert.deepEqual(mod.runPill({ status: 'running' }), { text: 'Running', tone: 'work' });
  assert.deepEqual(mod.runPill({ status: 'failed' }), { text: 'Failed', tone: 'err' });
  assert.deepEqual(mod.runPill({ status: 'canceled' }), { text: 'Cancelled', tone: 'off' });
  assert.deepEqual(mod.runPill({ status: 'completed' }), { text: 'Done', tone: 'idle' });
});

test('a verdict of its own is a plain button, neither green nor red', async (t) => {
  const { harness, mod } = await load(t);
  const api = fakeApi();
  api.getRun = async (runId) => ({ id: runId, blocked: [], awaitingDecisions: [{ nodeId: 'review', verdicts: ['ship', 'hold'], attempt: 1 }] });
  const mounted = await harness.mount(harness.html`<${mod.RunPage} runId="run_release" api=${api} source=${fakeSource(harness)} back=${() => {}} />`);
  await settle(harness);
  for (const verdict of ['ship', 'hold']) {
    const button = mounted.container.querySelector(`[data-run-verdict="${verdict}"]`);
    assert.ok(!button.classList.contains('phone-btn-no') && !button.classList.contains('phone-btn-ok'), verdict);
    assert.equal(button.textContent, `${verdict}…`);
  }
});
