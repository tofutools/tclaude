// js/phone-task-links.js: the phone task page's Refs, After, Epic and run
// sections, which show what the desktop task view shows (a PR's state, CI
// and reviews; a tracker issue's status; dependencies; the epic; the run).
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';

const PR = { kind: 'github', label: '#1482', url: 'https://github.com/acme/webshop/pull/1482',
  pr: { number: 1482, state: 'open', title: 'Checkout: one-page flow', checks: { total: 5, passed: 4, failed: 1 }, reviews: { decision: 'changes_requested' } } };
const ISSUE = { kind: 'linear', label: 'WEB-88', url: 'https://linear.app/acme/issue/WEB-88', tracker: { status: 'In Review', title: 'Redesign the checkout' } };
const UNLINKED = { kind: 'awb', label: 'awb-12', link_note: 'no awb base URL on this node' };

function board() {
  return {
    group: 'webshop',
    columns: [],
    tasks: [
      { id: 'tk_epic', kind: 'epic', title: 'Checkout v2', status: 'open', stage: 'next', progress: { total: 3, done: 1, in_progress: 1 } },
      { id: 'tk_page', title: 'Checkout page', status: 'open', stage: 'next', parent_id: 'tk_epic',
        refs: [PR, ISSUE, UNLINKED], depends_on: ['tk_api', 'tk_done', 'tk_gone'] },
      { id: 'tk_api', title: 'Payments API', status: 'open', stage: 'next', parent_id: 'tk_epic', assignee: { agent_id: AGENT, name: 'payments' } },
    ],
    closed: [{ id: 'tk_done', title: 'Address form', status: 'done', parent_id: 'tk_epic', closed_at: '2026-11-14T00:00:00Z' }],
  };
}

test('a ref row says what the desktop chip says: PR state, CI and review; an issue its tracker and status', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/phone-task-links.js');
  const pr = m.refRow(PR);
  assert.deepEqual([pr.kind, pr.label, pr.title, pr.state, pr.ci, pr.ciText, pr.review], ['pr', 'PR #1482', 'Checkout: one-page flow', 'open', 'failing', '✗ 4/5', 'changes requested']);
  assert.equal(pr.reviewTitle, 'GitHub: changes requested');
  const issue = m.refRow(ISSUE);
  assert.deepEqual([issue.kind, issue.kindName, issue.label, issue.state, issue.stateTone, issue.title], ['issue', 'Linear', 'WEB-88', 'In Review', 'ask', 'Redesign the checkout']);
  const unlinked = m.refRow(UNLINKED);
  assert.equal(unlinked.url, '', 'no link to open');
  assert.equal(unlinked.note, 'not linked: no awb base URL on this node');
  assert.equal(m.refRow({ kind: 'url', label: 'spec', url: 'javascript:alert(1)' }).url, '', 'only http(s) is a link');
});

test('After lists the dependencies with where each stands; Epic finds the parent or lists an epic\'s tasks', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/phone-task-links.js');
  const entry = board();
  const page = entry.tasks[1];
  assert.deepEqual(m.depRows(page, entry).map((row) => [row.title, row.state.text, row.open]), [
    ['Payments API', 'started', true], ['Address form', 'done', true], ['tk_gone', 'not on the board', false]]);
  assert.deepEqual(m.epicOfTask(page, entry), { id: 'tk_epic', title: 'Checkout v2', progress: entry.tasks[0].progress });
  assert.equal(m.epicOfTask({ parent_id: 'tk_old' }, entry).gone, true);
  assert.equal(m.epicOfTask(entry.tasks[0], entry), null, 'an epic is in none');
  assert.deepEqual(m.childRows([entry.tasks[2], entry.closed[0], { id: 'x', title: 'Stuck', status: 'blocked' }]).map((row) => [row.title, row.state.text]),
    [['Payments API', 'in progress'], ['Address form', 'done'], ['Stuck', 'blocked']]);
});

async function mountPage(t, id, { runs = {} } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, pageMod, taskPage, procMod] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-page.js'),
    harness.importDashboardModule('js/phone-task-page.js'),
    harness.importDashboardModule('js/process-context.js'),
  ]);
  const { signal } = harness.signals;
  const state = createDashboardState();
  state.beginRequest();
  state.commitRequest(1, { groups: [{ name: 'webshop', members: [{ agent_id: AGENT, conv_id: 'c1', title: 'payments', online: true, state: { status: 'working' } }] }] });
  procMod.processContext.context.value = { runs };
  const source = { attach: () => () => {}, status: signal('ready'), error: signal(''), context: signal({ groups: [board()] }), refresh: () => Promise.resolve() };
  const children = [board().tasks[2], board().closed[0]];
  const api = {
    task: async (tid) => ({ ...board().tasks.find((task) => task.id === tid), body: '', children: tid === 'tk_epic' ? children : undefined }),
    activity: async () => ({ events: [] }),
  };
  const opened = [];
  t.after(() => { pageMod.phonePage.value = null; procMod.processContext.context.value = null; });
  const mounted = await harness.mount(harness.html`<${taskPage.TaskPage} id=${id} group="webshop" source=${source} state=${state} api=${api}
    onClose=${() => {}} onMove=${() => {}} onOpenTask=${(tid) => opened.push(tid)} />`);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  return { harness, c: mounted.container, opened };
}

test("a task's page shows its refs, epic and dependencies, and a tap opens one", async (t) => {
  const { harness, c, opened } = await mountPage(t, 'tk_page');
  const sections = [...c.querySelectorAll('.phone-section')].map((s) => s.getAttribute('aria-label'));
  assert.deepEqual(sections, ['Brief', 'Refs', 'Epic', 'After', 'Activity']);
  const refs = [...c.querySelectorAll('[data-task-ref]')];
  assert.deepEqual(refs.map((li) => li.getAttribute('data-task-ref')), ['pr', 'issue', 'issue']);
  assert.match(refs[0].textContent.replace(/\s+/g, ' '), /PR #1482.*Checkout: one-page flow.*open.*✗ 4\/5.*changes requested/);
  assert.equal(refs[0].querySelector('a').getAttribute('href'), PR.url);
  assert.match(refs[1].textContent.replace(/\s+/g, ' '), /Linear WEB-88.*Redesign the checkout.*In Review/);
  assert.equal(refs[2].querySelector('a'), null, 'an unlinked ref is not a link');
  assert.match(c.querySelector('[data-task-epic="tk_epic"]').textContent, /Checkout v2.*1\/3 done/);
  await harness.act(() => c.querySelector('[data-task-epic="tk_epic"] button').click());
  await harness.act(() => c.querySelector('[data-task-dep="tk_api"] button').click());
  assert.deepEqual(opened, ['tk_epic', 'tk_api']);
  assert.equal(c.querySelector('[data-task-dep="tk_gone"] button'), null, 'a dependency the board no longer lists does not open');
});

test("an epic's page shows its progress and its tasks", async (t) => {
  const { harness, c, opened } = await mountPage(t, 'tk_epic');
  assert.match(c.querySelector('[data-task-epic-progress]').textContent, /1\/3 done · 1 in progress/);
  assert.deepEqual([...c.querySelectorAll('[data-task-child]')].map((li) => [li.querySelector('.phone-task-row-title').textContent, li.querySelector('.phone-pill').textContent]),
    [['Payments API', 'in progress'], ['Address form', 'done']]);
  await harness.act(() => c.querySelector('[data-task-child="tk_done"] button').click());
  assert.deepEqual(opened, ['tk_done']);
  assert.equal(c.querySelector('[data-task-run]'), null, 'no run, no line');
});

test("a task with a process run shows its run line, as on its board card", async (t) => {
  const run = { runId: 'run_1', name: 'Ship it', status: 'running', scope: { task: 'tk_page' }, startedAt: '2026-11-14T00:00:00Z' };
  const withRun = await mountPage(t, 'tk_page', { runs: { run_1: run } });
  assert.ok(withRun.c.querySelector('[data-task-run="run_1"]'), 'the run line, as on its card');
});
