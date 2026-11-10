// The Collab panel's rows drag to the Groups tab's bin (#dnd-trash): it shows
// labelled for the act while a row is dragged, a drop there asks first through
// the act's existing confirm, confirming runs it, a cancel runs nothing, and
// the drag's end hides the bin again.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createPreactHarness } from './preact-harness.mjs';

// until polls cond up to a limit, so no assertion rests on a fixed sleep.
async function until(harness, cond, what, limit = 2000) {
  const end = Date.now() + limit;
  for (;;) {
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
    if (cond()) return;
    if (Date.now() > end) assert.fail(`timed out waiting for ${what}`);
  }
}

const SNAP = {
  groups: [{ id: 1, name: 'crew' }, { id: 2, name: 'ops' }],
  agents: [{ agent_id: 'agt_a', conv_id: 'ca', title: 'alpha', groups: ['crew'], spawn_group: 'crew', online: true }],
  group_resources: [
    { kind: 'drawing_board', id: 'plan', group_id: 1, title: 'Plan', state: 'running' },
    { kind: 'page', id: 'pg1', group_id: 1, title: 'Preview', state: 'running', owner: 'agt_a' },
    { kind: 'process', id: 'review', group_id: 1, title: 'review', state: 'running' },
    { kind: 'shell', id: 'crew', group_id: 1, title: 'Group shell', state: 'running', session: 'sess-crew' },
    { kind: 'shell', id: 'ops', group_id: 2, title: 'Group shell', state: 'running', session: 'sess-ops' },
  ],
};

const BACKGROUND = [
  { kind: 'agent', key: 'agent:agt_a', agent_id: 'agt_a', tmux_session: 'tc-a', label: 'alpha', killable: true },
  { kind: 'group_shell', key: 'groupterm:crew', tmux_session: 'g-crew', killable: true },
  { kind: 'agent', key: 'sess:loose', tmux_session: 'loose', label: 'loose session', killable: true },
  { kind: 'agent', tmux_session: 'old-gen', label: 'superseded', killable: true },
];

async function mountRail(t, { confirmAnswer = true } = {}) {
  const harness = await createPreactHarness(t);
  const doc = harness.document;
  const html = await readFile(new URL('../dashboard/dashboard.html', import.meta.url), 'utf8');
  const markup = html.match(/<div id="dnd-trash"[\s\S]*?\n<\/div>/)[0];
  const holder = doc.createElement('div');
  holder.innerHTML = markup;
  const bin = holder.firstElementChild;
  doc.body.appendChild(bin);
  const [rail, bg, ops, places] = await Promise.all([
    harness.importDashboardModule('js/collab-rail.js'),
    harness.importDashboardModule('js/background-terminals.js'),
    harness.importDashboardModule('js/dnd-ops.js'),
    harness.importDashboardModule('js/resource-places.js'),
  ]);
  await harness.act(() => { places.stripPaneKeys.value = []; bg.backgroundTerminals.value = BACKGROUND; });
  const calls = [];
  const confirms = [];
  const answer = { value: confirmAnswer };
  // The real drop operations (dnd-ops.js), with the retire dialog faked: it
  // asks (the confirm) and, confirmed, retires.
  const groupOps = ops.createDndOps({
    confirm: async () => true, toast: (text, error) => calls.push(['toast', text, !!error]), refresh: async () => {},
    getSnapshot: () => SNAP,
    openRetire: async (selector, label, node) => {
      confirms.push({ title: `Retire ${label}?` });
      if (!answer.value) return { cancelled: true };
      calls.push(['retire', selector, label, node]);
      return { ok: true };
    },
    retireNeedsReconcile: () => false,
  });
  const rowContext = {
    confirm: async (options) => {
      confirms.push(options);
      if (!answer.value) return false;
      if (options.action) await options.action();
      return true;
    },
    notify: (text, error) => calls.push(['notify', text, !!error]),
    kill: async (row) => { calls.push(['kill', row.tmux_session]); return ''; },
    killShell: async (session) => { calls.push(['killShell', session]); return ''; },
    exportDrawingBoard: async () => {}, askDeleteDrawingBoard: async () => {},
    groupOps,
    api: {
      deleteDrawingBoard: async (id) => calls.push(['deleteDrawingBoard', id]),
      deleteGroupPage: async (id) => calls.push(['deleteGroupPage', id]),
    },
    source: { refresh: async () => {} },
    fetchImpl: async () => assert.fail('no network'),
  };
  const mounted = await harness.mount(harness.html`<${rail.CollabRail} panes=${[]} actions=${{}} snapshot=${{ value: SNAP }}
    notify=${rowContext.notify} rowContext=${rowContext} />`);
  t.after(() => mounted.unmount());
  const root = mounted.container;
  const transfer = { setData: () => {}, types: ['application/x-tclaude-rail-item'], dropEffect: 'none', effectAllowed: 'all' };
  const fire = (type, target) => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'dataTransfer', { value: transfer });
    target.dispatchEvent(event);
    return event;
  };
  const row = (key) => {
    const el = root.querySelector(`[data-rail-item="${key}"]`);
    assert.ok(el, `row ${key} is listed`);
    return el;
  };
  // label is the bin's label as the stylesheet would show it outside wizard
  // mode: the mode class picks the pair.
  const label = () => {
    if (bin.classList.contains('dnd-trash-kill-mode')) return bin.querySelector('.dnd-trash-label-kill').textContent;
    if (bin.classList.contains('dnd-trash-template-mode')) return bin.querySelector('.dnd-trash-label-template').textContent;
    return bin.querySelector('.dnd-trash-label-regular').textContent;
  };
  return { harness, root, bin, fire, row, label, calls, confirms, answer, bg };
}

const KINDS = [
  { key: 'bg::tc-a', what: 'an agent', label: 'Retire', ran: [['retire', 'ca', 'alpha', '']] },
  { key: 'drawing_board:plan', what: 'a drawing board', label: 'Delete', ran: [['deleteDrawingBoard', 'plan'], ['notify', 'Deleted Plan', false]] },
  { key: 'page:pg1', what: 'a page', label: 'Delete', ran: [['deleteGroupPage', 'pg1'], ['notify', 'Deleted Preview', false]] },
  { key: 'shell:crew', what: 'a group shell with its session', label: 'Kill', ran: [['kill', 'g-crew'], ['notify', 'Killed crew shell', false]] },
  { key: 'shell:ops', what: 'a running shell open nowhere', label: 'Kill', ran: [['killShell', 'sess-ops'], ['notify', 'Killed ops shell', false]] },
  { key: 'bg::loose', what: 'a background session', label: 'Kill', ran: [['kill', 'loose'], ['notify', 'Killed loose session', false]] },
  { key: 'bg::old-gen', what: 'an Other session', label: 'Kill', ran: [['kill', 'old-gen'], ['notify', 'Killed superseded', false]] },
];

for (const kind of KINDS) {
  test(`the Collab bin: ${kind.what} shows it as ${kind.label}, asks on a drop, and confirmed runs the existing act`, async (t) => {
    const { harness, bin, fire, row, label, calls, confirms } = await mountRail(t);
    const source = row(kind.key);
    assert.ok(source.hasAttribute('draggable') && source.getAttribute('draggable') !== 'false', 'the row drags');
    assert.equal(bin.classList.contains('show'), false, 'hidden before the drag');
    fire('dragstart', source);
    assert.ok(bin.classList.contains('show'), 'the bin shows while the row is dragged');
    assert.equal(label(), kind.label);
    assert.equal(fire('dragover', bin).defaultPrevented, true, 'the bin takes the drop');
    assert.ok(bin.classList.contains('dnd-drop-over'), 'and is armed');
    fire('drop', bin);
    assert.equal(bin.classList.contains('show'), false, 'the drop hides the bin');
    assert.equal(bin.classList.contains('dnd-drop-over'), false);
    await until(harness, () => calls.length >= kind.ran.length, 'the act to run');
    assert.equal(confirms.length, 1, 'it asked first');
    assert.deepEqual(calls, kind.ran);
    fire('dragend', source);
    assert.equal(bin.className, '', 'no mode class is left behind');
  });
}

test('the Collab bin: cancelling the confirm runs nothing', async (t) => {
  const { harness, bin, fire, row, calls, confirms } = await mountRail(t, { confirmAnswer: false });
  for (const [index, kind] of KINDS.entries()) {
    const source = row(kind.key);
    fire('dragstart', source);
    fire('dragover', bin);
    fire('drop', bin);
    fire('dragend', source);
    await until(harness, () => confirms.length === index + 1, `the confirm for ${kind.what}`);
  }
  assert.deepEqual(calls, [], 'none ran');
  assert.equal(bin.classList.contains('show'), false);
});

test('the Collab bin: dragend without a drop hides it; a drop elsewhere does not run the act; a process row shows none', async (t) => {
  const { harness, bin, fire, row, calls, confirms, root } = await mountRail(t);
  const source = row('drawing_board:plan');
  fire('dragstart', source);
  assert.ok(bin.classList.contains('show') && bin.classList.contains('dnd-trash-template-mode'));
  fire('dragover', bin);
  assert.ok(bin.classList.contains('dnd-drop-over'));
  fire('dragover', root);
  assert.equal(bin.classList.contains('dnd-drop-over'), false, 'leaving the bin disarms it');
  fire('dragend', source);
  assert.equal(bin.classList.contains('show'), false, 'a cancelled drag hides the bin');
  assert.equal(bin.className, '');

  // An Other session cannot be opened: elsewhere its drop is nothing.
  const other = row('bg::old-gen');
  fire('dragstart', other);
  assert.equal(fire('dragover', root.ownerDocument.body).defaultPrevented, false, 'only the bin takes an Other session');
  fire('drop', root.ownerDocument.body);
  fire('dragend', other);
  assert.equal(bin.classList.contains('show'), false);

  fire('dragstart', row('process:review'));
  assert.equal(bin.classList.contains('show'), false, 'the bin means nothing for a process');
  fire('dragend', row('process:review'));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.deepEqual(calls, []);
  assert.equal(confirms.length, 0);
});

test('the Collab bin: another node\'s agent retires there only as dnd-ops allows, by its stable ID', async (t) => {
  const harness = await createPreactHarness(t);
  const bin = await harness.importDashboardModule('js/collab-rail-bin.js');
  const calls = [];
  const fakeOps = (allow) => ({
    context: (where) => { calls.push(['context', where]); return { where }; },
    allowed: (ctx, gesture, agent) => { calls.push(['allowed', gesture, agent]); return allow; },
    refused: (ctx, gesture) => calls.push(['refused', gesture]),
    retire: async (payload, ctx) => calls.push(['retire', payload, ctx.where]),
  });
  const peer = { key: 'bg:peer-b:tc-p', kind: 'terminal', title: 'beta', group: '',
    row: { node: { id: 'peer-b', name: 'desk' }, agent_id: 'agt_p', tmux_session: 'tc-p', key: 'peer:peer-b:agt_p', killable: true },
    status: { agent: { agent_id: 'agt_p', title: 'beta' } } };
  const refused = bin.railBinAction(peer, { groupOps: fakeOps(false) });
  assert.equal(refused.act, 'retire', 'an agent row retires, even with a session to kill');
  await refused.run();
  assert.deepEqual(calls.splice(0), [['context', { node: 'peer-b', name: 'desk' }], ['allowed', 'retire', 'agt_p'], ['refused', 'retire']]);
  await bin.railBinAction(peer, { groupOps: fakeOps(true) }).run();
  assert.deepEqual(calls.at(-1), ['retire', { conv: 'agt_p', agent: 'agt_p', label: 'beta' }, { node: 'peer-b', name: 'desk' }]);
  const remote = { key: 'pane:r', kind: 'terminal', title: 'gamma', pane: { seed: { remote: { peer: 'peer-c', peerLabel: 'lab', agent: 'agt_g' } } },
    status: { agent: { title: 'gamma' } } };
  calls.length = 0;
  await bin.railBinAction(remote, { groupOps: fakeOps(true) }).run();
  assert.deepEqual(calls.at(-1), ['retire', { conv: 'agt_g', agent: 'agt_g', label: 'gamma' }, { node: 'peer-c', name: 'lab' }]);
  const peerBoard = { key: 'drawing_board:x', kind: 'drawing_board', title: 'X', group: 'crew', resource: { kind: 'drawing_board', id: 'x', peer: 'peer-b' } };
  assert.equal(bin.railBinAction(peerBoard, {}), null, "a peer's drawing board is deleted on its own node");
  const loosePage = { key: 'page:p9', kind: 'page', title: 'P', group: '', view: 'open-tab', paneKey: 'page:p9' };
  assert.equal(bin.railBinAction(loosePage, {}), null, 'a page open here with no group row has nothing to delete it by');
});

test('the bin\'s Kill label: both voices in the shell, shown only in kill mode', async () => {
  const html = await readFile(new URL('../dashboard/dashboard.html', import.meta.url), 'utf8');
  assert.match(html, /<span class="dnd-trash-label-kill">Kill<\/span><span class="dnd-trash-label-kill-wizard">Dispel<\/span>/);
  const css = await readFile(new URL('../dashboard/dashboard.css', import.meta.url), 'utf8');
  assert.match(css, /\.dnd-trash-label-kill, \.dnd-trash-label-kill-wizard \{ display: none; \}/);
  assert.match(css, /#dnd-trash\.dnd-trash-kill-mode \.dnd-trash-label-kill \{ display: inline; \}/);
  assert.match(css, /body\.wizard #dnd-trash\.dnd-trash-kill-mode \.dnd-trash-label-kill-wizard \{ display: inline; \}/);
});
