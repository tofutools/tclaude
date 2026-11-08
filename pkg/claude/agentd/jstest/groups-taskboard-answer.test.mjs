import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const op = { agent_id: 'operator', name: 'operator', operator: true };
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });
const question = (extra = {}) => ({
  kind: 'question', recommend: 'amber',
  options: [
    { key: 'amber', label: 'Amber', description: 'warm', preview: '★ You  amber' },
    { key: 'teal', label: 'Teal' },
    { key: 'none', label: 'No tint' },
  ],
  ...extra,
});

function board(group) {
  return {
    group, group_id: 1,
    counts: { live: 2, needs_you: 2, waiting_on_you: 2 },
    tasks: [
      row('tk_0000000a', 'Lane colour?', { assignee: op, assigned_by: 'agent:agt_builder', question: question(), waiting: true }),
      row('tk_0000000b', 'Which tables?', { assignee: op, assigned_by: 'agent:agt_builder', question: { kind: 'question', multi: true,
        options: [{ key: 'orders', label: 'Orders' }, { key: 'refunds', label: 'Refunds' }] } }),
    ],
    closed: [],
  };
}

function fakeApi() {
  const calls = [];
  const record = (verb) => async (...args) => { calls.push([verb, ...args]); return {}; };
  return { calls, answer: record('answer'), status: record('status'), stage: record('stage'), assign: record('assign'), unassign: record('unassign') };
}

async function mount(t, name, view, api = fakeApi()) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, view);
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  const source = { context: harness.signals.signal({ groups: [board(name)] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const group = { name, members: [{ agent_id: 'agt_builder', title: 'builder', online: true }] };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  return { harness, c, api, click, doc: harness.document };
}

test('a question card answers through the answer dialog: a pick and words, in one write', async (t) => {
  const { harness, c, api, click, doc } = await mount(t, 'tbq-lanes', 'lanes');
  const card = c.querySelector('.tb-card[data-task-id="tk_0000000a"]');
  assert.match(card.textContent, /❓ pick one · 3/);
  assert.match(card.textContent, /builder is waiting/);
  assert.match(c.querySelector('.tb-card[data-task-id="tk_0000000b"]').textContent, /pick any · 2/);
  await click(card.querySelector('.tb-answer-open'));
  const dialog = doc.querySelector('#taskboard-answer-modal');
  assert.ok(dialog, 'Answer… opens the dialog');
  assert.match(dialog.textContent, /asked by builder/);
  assert.match(dialog.querySelector('[data-answer-option="amber"]').textContent, /recommended/);
  assert.match(dialog.querySelector('[data-answer-option="amber"] .tb-answer-preview').textContent, /★ You {2}amber/, 'an option shows its preview');
  const send = () => dialog.querySelector('#taskboard-answer-submit');
  assert.equal(send().disabled, true, 'nothing picked or said yet');
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('[data-answer-option="teal"] input'), 'change'); });
  await harness.input(dialog.querySelector('.tb-answer-text'), 'but dimmer');
  await click(send());
  assert.deepEqual(api.calls, [['answer', 'tk_0000000a', ['teal'], 'but dimmer', 0]]);
});

test('a multi-select question takes several picks; the List answers from ⋯ Answer…', async (t) => {
  const { harness, c, api, click, doc } = await mount(t, 'tbq-list', 'list');
  const rowEl = c.querySelector('tr[data-task-id="tk_0000000b"]');
  assert.match(rowEl.textContent, /❓ pick any · 2/);
  await click(rowEl.querySelector('.tb-act-more'));
  const item = [...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].find((b) => b.textContent.trim() === 'Answer…');
  assert.ok(item, 'the ⋯ menu offers Answer…');
  await click(item);
  const dialog = doc.querySelector('#taskboard-answer-modal');
  for (const key of ['orders', 'refunds']) {
    await harness.act(async () => { harness.fireEvent(dialog.querySelector(`[data-answer-option="${key}"] input`), 'change'); });
  }
  await click(dialog.querySelector('#taskboard-answer-submit'));
  assert.deepEqual(api.calls, [['answer', 'tk_0000000b', ['orders', 'refunds'], '', 0]]);
});

test('without the assign grant no Answer is offered', async (t) => {
  const api = fakeApi();
  api.can = { assign: false };
  const { c } = await mount(t, 'tbq-noassign', 'lanes', api);
  assert.equal(c.querySelector('.tb-answer-open'), null);
  assert.match(c.querySelector('.tb-card[data-task-id="tk_0000000a"]').textContent, /❓ pick one/, 'the tag still shows');
});

// A question on an agent's task (tk_8b2c86f7) is a card of its own in the
// operator's lane, and the task stays in its agent's lane.
for (const [by, youRow, agentRow] of [
  ['agent', '[data-lane-row="operator"]', '[data-lane-row="agt_builder"]'],
  ['triage', '[data-board-row-head="operator"]', '[data-board-row-head="agents"]'],
]) test(`a question on an agent's task is a card in the operator's row (lanes by ${by}); Answer… answers that question`, async (t) => {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.view.tbq-on-task', 'lanes');
  dashPrefs.setItem('tclaude.dash.groups.fused.taskboard.tbq-on-task', '1');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.lanes-by.tbq-on-task', by);
  const work = row('tk_0000000c', 'Lane tint', { assignee: { agent_id: 'agt_builder', name: 'builder' }, assigned_by: 'human:operator',
    questions: [
      { kind: 'question', n: 1, title: 'Old one', asked_by: 'agent:agt_builder', state: 'answered', answer: { picked: [], text: 'ok', answered_by: 'human:operator' } },
      { ...question(), n: 2, title: 'Lane colour?', asked_by: 'agent:agt_builder', state: 'open', waiting: true },
    ], open_questions: 1 });
  const source = { context: harness.signals.signal({ groups: [{ group: 'tbq-on-task', group_id: 1, counts: { live: 1, waiting_on_you: 1 }, tasks: [work], closed: [] }] }),
    status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const group = { name: 'tbq-on-task', members: [{ agent_id: 'agt_builder', title: 'builder', online: true }] };
  const api = fakeApi();
  const { container: c } = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const card = c.querySelector(`${youRow} .tb-question-card[data-question="tk_0000000c#2"]`);
  assert.ok(card, 'the open question is a card in the operator lane');
  assert.match(card.textContent, /builder asks on[\s\S]*Lane tint[\s\S]*Lane colour\?[\s\S]*Q2 · pick one · 3[\s\S]*waiting/);
  assert.equal(c.querySelectorAll('.tb-question-card').length, 1, 'an answered question has no card');
  assert.ok(c.querySelector(`${agentRow} .tb-card[data-task-id="tk_0000000c"]`), 'the task stays in its agent\'s lane');
  assert.match(c.querySelector(`${agentRow} .tb-card[data-task-id="tk_0000000c"]`).textContent, /❓ 1 asked you/);
  await harness.act(async () => { harness.fireEvent(card.querySelector('.tb-answer-open'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  const dialog = harness.document.querySelector('#taskboard-answer-modal');
  assert.match(dialog.textContent, /tk_0000000c · Q2 on “Lane tint”/);
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('[data-answer-option="teal"] input'), 'change'); });
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('#taskboard-answer-submit'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['answer', 'tk_0000000c', ['teal'], '', 2]]);
});

// A question task's context is its brief, which the board's row leaves out:
// the dialog reads the task for it, renders it, and shows the files asked
// with it (ask-operator --attach) as tiles that open the attachment viewer.
test('a question task\'s dialog reads its brief, renders it, and shows its files', async (t) => {
  const harness = await createPreactHarness(t);
  const { AnswerDialog } = await harness.importDashboardModule('js/groups-taskboard-answer.js');
  const files = [{ id: 7, name: 'mock.png', size: 68, content_type: 'image/png', image: true, previewable: true, sha256: 'a'.repeat(64) },
    { id: 8, name: 'notes.md', size: 40, content_type: 'text/markdown', markdown: true, sha256: 'b'.repeat(64) }];
  const the = row('tk_0000000a', 'Lane colour?', { assignee: op, question: question(), attachments: files });
  const reads = [];
  const api = {
    task: async (id) => { reads.push(id); return { ...the, body: 'Which **tint**? See ![mock](mock.png)' }; },
    fileData: async () => ({ name: 'mock.png', data: 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', content_type: 'image/png' }),
  };
  await harness.mount(harness.html`<${AnswerDialog} task=${the} api=${api} askedBy="builder" onSubmit=${async () => true} onClose=${() => {}} />`);
  const dialog = harness.document.querySelector('#taskboard-answer-modal');
  for (let i = 0; i < 100 && !dialog.querySelector('.tb-answer-context strong'); i += 1) {
    await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  }
  assert.deepEqual(reads, ['tk_0000000a'], 'the brief is read once');
  assert.ok(dialog.querySelector('.tb-answer-context strong'), 'the brief is rendered Markdown');
  assert.match(dialog.querySelector('.tb-answer-files').textContent, /Attached · 2/);
  assert.ok(dialog.querySelector('.tb-answer-files [data-file="7"]') && dialog.querySelector('.tb-answer-files [data-file="8"]'), 'each file is a tile');
  assert.match(dialog.querySelector('.tb-answer-rec-line').textContent, /builder recommends Amber/);
  assert.equal(dialog.querySelectorAll('.tb-answer-preview').length, 1, 'an option with a preview shows it beside its label');
  assert.equal(harness.document.activeElement, dialog.querySelector('[data-answer-option="amber"] input'), 'focus starts on the recommended option, not a file tile');
  assert.equal(dialog.querySelector('[data-answer-option="amber"] label .tb-answer-preview'), null, 'a preview is outside the option\'s label: selecting its text does not pick it');
  await harness.act(() => dialog.querySelector('.tb-answer-files [data-file="7"] .tb-file-open').click());
  for (let i = 0; i < 5; i += 1) await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  assert.equal(harness.document.querySelector('.image-preview-overlay h2')?.textContent, 'mock.png', 'a tile opens the attachment viewer');
});
