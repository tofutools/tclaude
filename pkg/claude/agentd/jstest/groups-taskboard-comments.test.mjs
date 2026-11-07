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
  const box = container.querySelector('.tb-comment-new');
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
  await harness.input(commentEl(4).querySelector('.tb-comment-edit-body'), 'Add the QA and release notes');
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
