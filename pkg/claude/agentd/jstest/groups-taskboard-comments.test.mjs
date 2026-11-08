// A task's comments and activity at the foot of the edit form
// (groups-taskboard-comments.js): the stream as the server gives it, a
// comment sent at once with a key that makes a retry safe, the author's own
// edit and delete, and the 💬 count on a row.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const task = (extra = {}) => ({ id: 'tk_00000001', group: 'crew', title: 'Release notes', body: 'draft them', stage: 'next', status: 'open',
  depends_on: [], created_by: 'human:operator', created_at: ago(60), attachments: [], comment_count: 2, ...extra });

const comment = (id, extra = {}) => ({ id, kind: 'comment', body: `comment ${id}`, author: 'human:operator', at: ago(10), ...extra });

const STREAM = [
  { id: 1, at: ago(60), actor: 'human:operator', actor_name: 'operator', kind: 'created', detail_text: 'created the task' },
  { id: 2, at: ago(50), actor: 'human:operator', actor_name: 'operator', kind: 'edited', changes: [{ field: 'title', before: 'Notes', after: 'Release notes' }] },
  { id: 3, at: ago(40), actor: 'agent:agt_dev', kind: 'comment', comment: comment(3, { body: 'Started on **the draft**', author: 'agent:agt_dev', author_name: 'dev' }) },
  { id: 4, at: ago(30), actor: 'human:operator', kind: 'comment', comment: comment(4, { body: 'Add the *QA* notes', edited_at: ago(5),
    attachments: [{ id: 9, name: 'qa.md', size: 12, content_type: 'text/markdown' }] }) },
  { id: 5, at: ago(25), actor: 'human:operator', kind: 'comment_edited', comment_id: 4, previous_body: 'Add QA' },
  { id: 6, at: ago(20), actor: 'agent:agt_dev', kind: 'comment', comment: comment(6, { kind: 'block_reason', body: 'waiting on QA', author: 'agent:agt_dev', author_name: 'dev' }) },
  { id: 7, at: ago(15), actor: 'human:operator', kind: 'comment', comment: comment(7, { body: '', deleted_at: ago(14) }) },
];

function fakeApi({ entries = STREAM, failComment = 0 } = {}) {
  const calls = [];
  let fails = failComment;
  const api = {
    calls,
    activity: async (id) => { calls.push(['activity', id]); return { id, entries }; },
    comment: async (...args) => {
      calls.push(['comment', ...args]);
      if (fails > 0) { fails -= 1; throw new Error('node offline'); }
      return { comment: comment(99) };
    },
    editComment: async (...args) => { calls.push(['editComment', ...args]); return {}; },
    deleteComment: async (...args) => { calls.push(['deleteComment', ...args]); return {}; },
    fileData: async () => ({ data: '', content_type: 'text/markdown' }),
  };
  return api;
}

async function mountActivity(t, { api = fakeApi(), me = 'human:operator', the = task() } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskActivity } = await harness.importDashboardModule('js/groups-taskboard-comments.js');
  const drafts = [];
  const { container } = await harness.mount(harness.html`<${TaskActivity} api=${api} task=${the} me=${me} onDraft=${(d) => drafts.push(d)} />`);
  const settle = async () => { for (let i = 0; i < 4; i += 1) await harness.act(() => new Promise((r) => setTimeout(r, 0))); };
  await settle();
  const click = async (el) => { await harness.act(() => harness.fireEvent(el, 'click')); await settle(); };
  const commentEl = (id) => container.querySelector(`[data-comment="${id}"]`);
  return { harness, container, api, settle, click, commentEl, drafts };
}

test('the stream shows changes as lines and comments as Markdown; only the viewer\'s own comments can be edited', async (t) => {
  const { harness, container, commentEl } = await mountActivity(t);
  // The Markdown parser loads on first use: wait for it, up to a limit.
  for (let i = 0; i < 100 && !commentEl(3).querySelector('.tb-comment-body strong'); i += 1) {
    await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  }
  const changes = [...container.querySelectorAll('.tb-activity-change')].map((li) => li.textContent.replace(/\s+/g, ' ').trim());
  assert.match(changes[0], /^operator created the task/);
  assert.match(changes[1], /^operator title: Notes → Release notes/);
  assert.equal(container.querySelector('[data-activity="comment_edited"]'), null, 'an edit shows as "edited" on its comment, not as an entry');
  assert.match(commentEl(3).querySelector('.tb-comment-author').textContent, /^dev$/, 'a comment names its author by author_name');
  assert.ok(commentEl(3).querySelector('.tb-comment-body strong'), 'the body is rendered Markdown');
  assert.equal(commentEl(3).querySelector('.tb-comment-edit'), null, 'someone else\'s comment has no Edit');
  assert.ok(commentEl(4).querySelector('.tb-comment-edit') && commentEl(4).querySelector('.tb-comment-rm'), 'the viewer\'s own does');
  assert.match(commentEl(4).textContent, /edited/);
  assert.ok(commentEl(4).querySelector('[data-file="9"]'), 'a comment\'s files are tiles');
  assert.match(commentEl(6).querySelector('.tb-comment-kind').textContent, /blocked/, 'a block reason is a typed comment');
  assert.match(commentEl(7).textContent, /deleted/);
  assert.equal(commentEl(7).querySelector('.tb-comment-edit'), null, 'a deleted comment has nothing to edit');
});

test('Ctrl+Enter sends a comment at once; a failed send keeps its key, so the retry is not a second comment', async (t) => {
  const api = fakeApi({ failComment: 1 });
  const { harness, container, settle, drafts } = await mountActivity(t, { api });
  const box = container.querySelector('.tb-comment-new textarea');
  await harness.input(box, 'Ship it after QA');
  assert.equal(drafts.at(-1), true, 'the form hears a comment is being written');
  const send = async () => {
    await harness.act(() => harness.fireEvent(box, 'keydown', { key: 'Enter', ctrlKey: true }));
    await settle();
  };
  await send();
  await send();
  const sent = api.calls.filter((c) => c[0] === 'comment');
  assert.equal(sent.length, 2);
  assert.deepEqual(sent[0].slice(1, 3), ['tk_00000001', 'Ship it after QA']);
  assert.match(sent[0][3], /^dash-/);
  assert.equal(sent[1][3], sent[0][3], 'the retry carries the same key');
  assert.equal(box.value, '', 'a sent comment clears the composer');
  assert.equal(drafts.at(-1), false);
  assert.equal(api.calls.filter((c) => c[0] === 'activity').length, 2, 'the stream is read again after the comment');
  // The next comment is a new one: a fresh key.
  await harness.input(box, 'And the changelog');
  await harness.act(() => harness.fireEvent(container.querySelector('#taskboard-comment-send'), 'click'));
  await settle();
  const third = api.calls.filter((c) => c[0] === 'comment')[2];
  assert.notEqual(third[3], sent[0][3]);
});

test('the author edits and deletes a comment', async (t) => {
  const { harness, api, click, commentEl, settle } = await mountActivity(t);
  await click(commentEl(4).querySelector('.tb-comment-edit'));
  await harness.input(commentEl(4).querySelector('.tb-comment-edit-body textarea'), 'Add the QA and release notes');
  await click([...commentEl(4).querySelectorAll('button')].find((b) => b.textContent.trim() === 'Save comment'));
  assert.deepEqual(api.calls.filter((c) => c[0] === 'editComment'), [['editComment', 'tk_00000001', 4, 'Add the QA and release notes']]);
  const { shellState } = await harness.importDashboardModule('js/shell-state.js');
  await click(commentEl(4).querySelector('.tb-comment-rm'));
  assert.equal(shellState.confirmation.value?.title, 'Delete comment?');
  await harness.act(async () => { shellState.resolveConfirmation(true); });
  await settle();
  assert.deepEqual(api.calls.filter((c) => c[0] === 'deleteComment'), [['deleteComment', 'tk_00000001', 4]]);
});

test('on a linked node\'s board, the viewer\'s comments are this node\'s', async (t) => {
  const harness = await createPreactHarness(t);
  const { myAuthor } = await harness.importDashboardModule('js/groups-taskboard-comments.js');
  assert.equal(myAuthor('', { self: 'inst_mine', peerPage: false }), 'human:operator', 'this node\'s own board');
  assert.equal(myAuthor('inst_other', { self: 'inst_mine', peerPage: false }), 'peer:inst_mine', 'a linked node\'s board in the fused view');
  assert.equal(myAuthor('', { self: 'inst_mine', peerPage: true }), 'peer:inst_mine', 'a peer\'s own page');
  assert.equal(myAuthor('inst_other', { self: '', peerPage: false }), '', 'without knowing this node, nothing is offered');
});

test('a comment with files goes as multipart, to a linked node through its staged upload', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTaskboardApi } = await harness.importDashboardModule('js/taskboard-actions.js');
  const sent = [];
  const api = createTaskboardApi({
    fetchImpl: async (url, init) => { sent.push(['fetch', url, init]); return new Response(JSON.stringify({ comment: { id: 1 } }), { status: 201 }); },
    upload: async (path, init) => { sent.push(['upload', path, init]); return new Response(JSON.stringify({ comment: { id: 2 } }), { status: 201 }); },
  });
  await api.comment('tk_00000001', 'see the shot', 'dash-k1');
  const file = new Blob(['png'], { type: 'image/png' });
  await api.comment('tk_00000001', 'with a shot', 'dash-k2', [{ file, name: 'shot.png' }]);
  assert.equal(sent[0][0], 'fetch');
  assert.match(sent[0][1], /\/v1\/taskboard\/tasks\/tk_00000001\/comments$/);
  assert.deepEqual(JSON.parse(sent[0][2].body), { body: 'see the shot', key: 'dash-k1' });
  assert.equal(sent[1][0], 'upload');
  const form = sent[1][2].body;
  assert.deepEqual(JSON.parse(await form.get('comment').text()), { body: 'with a shot', key: 'dash-k2' });
  assert.equal(form.getAll('file').length, 1);
  assert.equal(form.get('file').name, 'shot.png');
});

test('a row shows its 💬 count, which opens the edit form at the comments', async (t) => {
  const harness = await createPreactHarness(t);
  const { CommentCount } = await harness.importDashboardModule('js/groups-taskboard-comments.js');
  let opened = 0;
  const { container } = await harness.mount(harness.html`<div>
    <${CommentCount} task=${task({ comment_count: 3 })} onOpen=${() => { opened += 1; }} />
    <${CommentCount} task=${task({ comment_count: 0 })} onOpen=${() => {}} />
  </div>`);
  const counts = container.querySelectorAll('.tb-comment-count');
  assert.equal(counts.length, 1, 'no comments, no count');
  assert.match(counts[0].textContent, /💬 3/);
  await harness.act(() => harness.fireEvent(counts[0], 'click'));
  assert.equal(opened, 1);
});

test('a comment over 16 KiB of UTF-8 is held back before it is sent, though it fits maxLength in characters', async (t) => {
  const api = fakeApi();
  const { harness, container, settle } = await mountActivity(t, { api });
  const box = container.querySelector('.tb-comment-new textarea');
  await harness.input(box, '漢'.repeat(6000)); // 6,000 characters, 18,000 bytes
  assert.equal(container.querySelector('#taskboard-comment-send').disabled, true);
  assert.match(container.querySelector('.tb-comment-too-long').textContent, /over 16 KiB/);
  await harness.act(() => harness.fireEvent(box, 'keydown', { key: 'Enter', ctrlKey: true }));
  await settle();
  assert.equal(api.calls.filter((c) => c[0] === 'comment').length, 0, 'nothing is sent the server would refuse');
});

test('a failed re-read keeps the stream and says it may be stale', async (t) => {
  const api = fakeApi();
  let fail = false;
  const read = api.activity;
  api.activity = async (id) => { if (fail) throw new Error('node offline'); return read(id); };
  const { harness, container, settle } = await mountActivity(t, { api });
  fail = true;
  await harness.input(container.querySelector('.tb-comment-new textarea'), 'one more');
  await harness.act(() => harness.fireEvent(container.querySelector('#taskboard-comment-send'), 'click'));
  await settle();
  assert.ok(container.querySelector('[data-comment="3"]'), 'the stream stays');
  assert.match(container.querySelector('.tb-activity-stale').textContent, /could not be read again \(node offline\)/);
});

test('an edit that sends the same text keeps the key: trailing spaces after a lost answer are no second comment', async (t) => {
  const api = fakeApi({ failComment: 1 });
  const { harness, container, settle } = await mountActivity(t, { api });
  const box = container.querySelector('.tb-comment-new textarea');
  await harness.input(box, 'Ship it');
  await harness.act(() => harness.fireEvent(box, 'keydown', { key: 'Enter', ctrlKey: true }));
  await settle();
  await harness.input(box, 'Ship it  ');
  await harness.act(() => harness.fireEvent(box, 'keydown', { key: 'Enter', ctrlKey: true }));
  await settle();
  const keys = api.calls.filter((c) => c[0] === 'comment').map((c) => c[3]);
  assert.equal(keys.length, 2);
  assert.equal(keys[1], keys[0], 'the same comment, the same key');
});

test('the viewer\'s author follows this node\'s id arriving after the first paint', async (t) => {
  const harness = await createPreactHarness(t);
  const { myAuthor } = await harness.importDashboardModule('js/groups-taskboard-comments.js');
  const { setSelfNode } = await harness.importDashboardModule('js/node-api.js');
  setSelfNode('');
  t.after(() => setSelfNode(''));
  function Who() { return harness.html`<span class="who">${myAuthor('inst_other1', { peerPage: false })}</span>`; }
  const { container } = await harness.mount(harness.html`<${Who} />`);
  assert.equal(container.querySelector('.who').textContent, '', 'unknown at first: no Edit offered');
  await harness.act(() => setSelfNode('inst_mine1'));
  assert.equal(container.querySelector('.who').textContent, 'peer:inst_mine1', 'and the render follows once it is known');
});

// Questions in the activity (tk_8b2c86f7): a question is an entry with its
// options, its answer shows right under it (not at its own place), and an
// open one is answered inline.
const QUESTION_OPTS = [{ key: 'amber', label: 'Amber' }, { key: 'teal', label: 'Teal' }];
const QA_STREAM = [
  { id: 10, at: ago(30), actor: 'agent:agt_dev', kind: 'comment', comment: comment(10, { kind: 'question', body: 'The lane needs a tint.', author: 'agent:agt_dev', author_name: 'dev',
    question: { kind: 'question', n: 1, title: 'Lane colour?', options: QUESTION_OPTS, recommend: 'amber', asked_by: 'agent:agt_dev', state: 'answered',
      answer: { picked: ['amber'], text: 'a touch lighter', answered_by: 'human:operator', answered_at: ago(20) } } }) },
  { id: 11, at: ago(25), actor: 'agent:agt_dev', kind: 'comment', comment: comment(11, { body: 'thanks', author: 'agent:agt_dev', author_name: 'dev' }) },
  { id: 12, at: ago(20), actor: 'human:operator', kind: 'comment', comment: comment(12, { kind: 'answer', body: 'Answer: Amber; a touch lighter', answers: 1 }) },
  { id: 13, at: ago(10), actor: 'agent:agt_dev', kind: 'comment', comment: comment(13, { kind: 'question', body: '', author: 'agent:agt_dev', author_name: 'dev',
    question: { kind: 'question', n: 2, title: 'Behind a flag?', options: [{ key: 'flag', label: 'Behind a flag' }, { key: 'on', label: 'On by default' }], asked_by: 'agent:agt_dev', state: 'open' } }) },
];

test('a question shows its answer right under it, and an open one is answered inline', async (t) => {
  const api = fakeApi({ entries: QA_STREAM });
  api.answer = async (...args) => { api.calls.push(['answer', ...args]); return {}; };
  const { harness, container, click, commentEl } = await mountActivity(t, { api });
  const q1 = commentEl(10);
  assert.match(q1.querySelector('.tb-question-title').textContent, /Lane colour\?/);
  assert.match(q1.querySelector('.tb-question-picked').textContent, /Amber/, 'the picked option is marked');
  assert.match(q1.querySelector('.tb-question-answer').textContent, /answered Q1[\s\S]*Picked Amber — “a touch lighter”/);
  assert.equal(commentEl(12), null, 'the answer entry is not shown again at its own place');
  const order = [...container.querySelectorAll('.tb-activity-list > li')].map((li) => li.dataset.comment);
  assert.deepEqual(order, ['10', '11', '13']);
  assert.equal(q1.querySelector('.tb-answer-open'), null, 'an answered question offers no Answer');

  const q2 = commentEl(13);
  await click(q2.querySelector('.tb-answer-open'));
  await harness.act(async () => { harness.fireEvent(q2.querySelector('[data-answer-option="on"] input'), 'change'); });
  await click(q2.querySelector('.tb-question-send'));
  assert.deepEqual(api.calls.filter((c) => c[0] === 'answer'), [['answer', 'tk_00000001', ['on'], '', 2]]);
});

test('the asker may withdraw its own open question', async (t) => {
  const api = fakeApi({ entries: QA_STREAM });
  const { commentEl } = await mountActivity(t, { api, me: 'agent:agt_dev' });
  assert.equal(commentEl(10).querySelector('.tb-question-withdraw'), null, 'an answered question stays');
  assert.ok(commentEl(13).querySelector('.tb-question-withdraw'), 'an open one can be withdrawn');
});

test('activity lines read as the board does: a ref change shows the new ref, a new task its lane', async (t) => {
  const harness = await createPreactHarness(t);
  {
    const { changeText } = await harness.importDashboardModule('js/groups-taskboard-comments.js');
    const refEdit = { kind: 'edited', detail: 'kind, url, label', changes: [
      { field: 'ref_kind', after: 'github_pr' },
      { field: 'ref_url', after: 'https://github.com/tofutools/tclaude/pull/4226' },
      { field: 'ref_label', after: 'tofutools/tclaude#4226' }] };
    assert.equal(changeText(refEdit), 'ref → tofutools/tclaude#4226');
    assert.equal(changeText({ kind: 'edited', detail: 'url', changes: [{ field: 'ref_url', before: 'https://x.test/1', after: 'https://x.test/2' }] }),
      'ref → https://x.test/2');
    assert.equal(changeText({ kind: 'edited', detail: 'url, label', changes: [{ field: 'ref_url', before: 'u' }, { field: 'ref_label', before: 'l' }] }), 'cleared ref');
    assert.equal(changeText({ kind: 'edited', detail: 'title, label', changes: [{ field: 'title', before: 'a', after: 'b' }, { field: 'ref_label', after: 'L' }] }),
      'title: a → b · ref → L');
    assert.equal(changeText({ kind: 'created', detail: 'in idea' }), 'created in Ideas');
    assert.equal(changeText({ kind: 'created', detail: 'in next, ref tk#1, cloned' }), 'created in Next, ref tk#1, cloned');
    assert.equal(changeText({ kind: 'created', detail: 'in Review' }), 'created in Review');
  }
});
