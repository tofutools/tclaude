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
  assert.match(dialog.querySelector('.tb-answer-preview').textContent, /★ You {2}amber/, 'the preview follows the recommended option first');
  const send = () => dialog.querySelector('#taskboard-answer-submit');
  assert.equal(send().disabled, true, 'nothing picked or said yet');
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('[data-answer-option="teal"] input'), 'change'); });
  await harness.input(dialog.querySelector('.tb-answer-text'), 'but dimmer');
  await click(send());
  assert.deepEqual(api.calls, [['answer', 'tk_0000000a', ['teal'], 'but dimmer']]);
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
  assert.deepEqual(api.calls, [['answer', 'tk_0000000b', ['orders', 'refunds'], '']]);
});

test('without the assign grant no Answer is offered', async (t) => {
  const api = fakeApi();
  api.can = { assign: false };
  const { c } = await mount(t, 'tbq-noassign', 'lanes', api);
  assert.equal(c.querySelector('.tb-answer-open'), null);
  assert.match(c.querySelector('.tb-card[data-task-id="tk_0000000a"]').textContent, /❓ pick one/, 'the tag still shows');
});
