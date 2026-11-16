// The inline process editor and an agent saving the same draft
// (dashboard/js/groups-mini-live.js and its use in groups-mini-editor.js).

import test from 'node:test';
import assert from 'node:assert/strict';
import { installFastClock } from './fast-clock.mjs';
import { createPreactHarness } from './preact-harness.mjs';
import { changeList, inlineHelpNote, mergeIncoming } from '../dashboard/js/groups-mini-live.js';

installFastClock(0.05);

function view(nodes, { name = 'Ship it', hash = 'h1', rev = 'a' } = {}) {
  const ids = Object.keys(nodes);
  const edges = [{ from: '', outcome: 'start', to: 'start' }];
  for (let i = 0; i < ids.length - 1; i += 1) {
    if (nodes[ids[i]].type !== 'end') edges.push({ from: ids[i], outcome: 'next', to: ids[i + 1] });
  }
  return {
    template: { id: 'tpl_1', name, start: 'start', nodes: structuredClone(nodes) },
    edges,
    layout: { nodes: Object.fromEntries(ids.map((id, i) => [id, { x: i * 160, y: 0 }])) },
    sourceHash: hash, currentRef: `tpl_1@sha256:${rev}`,
  };
}

const agentStep = (prompt, extra = {}) => ({ type: 'task', performer: { kind: 'agent', prompt, role: 'dev' }, ...extra });
const BASE_NODES = { start: { type: 'start' }, build: agentStep('Build it'), done: { type: 'end', result: 'success' } };

test('a version that changes other steps merges cleanly; one that changes the same step waits for a choice', () => {
  const base = view(BASE_NODES);
  const theirs = view({ ...BASE_NODES, build: agentStep('Build it', { name: 'Build' }) }, { hash: 'h2', rev: 'b' });
  const clean = mergeIncoming({ base, ours: base, theirs });
  assert.equal(clean.conflicts.length, 0);
  assert.equal(clean.merged.template.nodes.build.name, 'Build');
  assert.ok(clean.clean, 'nothing local: the draft is exactly theirs');

  const ours = view({ ...BASE_NODES, build: agentStep('Build it well') });
  const both = mergeIncoming({ base, ours, theirs });
  assert.equal(both.merged, null, 'both changed build: no silent pick');
  assert.deepEqual(both.conflicts.map((change) => change.nodeId), ['build']);
  assert.equal(mergeIncoming({ base, ours, theirs, choice: 'theirs' }).merged.template.nodes.build.name, 'Build');
  const mine = mergeIncoming({ base, ours, theirs, choice: 'mine' });
  assert.equal(mine.merged.template.nodes.build.performer.prompt, 'Build it well');
  assert.equal(mine.clean, false, 'keeping mine leaves something to save');
});

test('the operator\'s edit elsewhere survives the agent\'s new step', () => {
  const base = view(BASE_NODES);
  const ours = view({ ...BASE_NODES, build: agentStep('Build it with tests') });
  const theirs = view({ start: { type: 'start' }, build: agentStep('Build it'), review: agentStep('Review it'), done: { type: 'end', result: 'success' } }, { hash: 'h2', rev: 'b' });
  const result = mergeIncoming({ base, ours, theirs });
  assert.equal(result.conflicts.length, 0);
  assert.ok(result.merged.template.nodes.review, 'their new step lands');
  assert.equal(result.merged.template.nodes.build.performer.prompt, 'Build it with tests', 'and my edit stays');
  assert.equal(result.clean, false);
  assert.match(changeList(result.changes), /\+ review/);
});

test('the agent brief says the operator watches and that a moved hash is their edit', () => {
  const note = inlineHelpNote('webshop', 'Ship it');
  assert.match(note, /inline editor of group webshop/);
  assert.match(note, /moved sourceHash is usually their own edit/);
  assert.match(note, /Never revert what they changed/);
});

async function mountEditor(t, api, props = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-mini-editor.js');
  const stored = view(BASE_NODES);
  const mounted = await harness.mount(harness.html`<${mod.MiniGraphEditor} group=${{ name: 'webshop' }} roster=${[]}
    process=${{ templateId: 'tpl_1', name: 'Ship it' }} view=${stored} stored=${stored} api=${api} draft=${true}
    onClose=${() => {}} onSteps=${() => {}} ...${props} />`);
  return { harness, root: mounted.container };
}

async function waitFor(harness, condition, message) {
  for (let attempt = 0; attempt < 400; attempt += 1) {
    if (condition()) return;
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 20)));
  }
  assert.ok(condition(), message);
}

test('an agent\'s save shows up in the open inline editor, says who, and undo takes it out and saves', async (t) => {
  let head = view(BASE_NODES);
  const saves = [];
  const api = {
    getTemplateHead: async () => structuredClone(head),
    saveTemplate: async (id, draft, meta) => {
      saves.push({ draft: structuredClone(draft), sourceHash: meta.sourceHash });
      if (meta.sourceHash !== head.sourceHash) throw new Error('process template head changed');
      head = { ...structuredClone(draft), sourceHash: `h${saves.length + 10}`, currentRef: `tpl_1@sha256:s${saves.length}` };
      return { ref: head.currentRef, sourceHash: head.sourceHash };
    },
  };
  const { harness, root } = await mountEditor(t, api);
  assert.ok(root.querySelector('.gm-get-help'), 'the inline editor offers ✨ With an agent');
  assert.match(root.querySelector('.gm-get-help .get-help-toggle').textContent, /With an agent/);

  const scribe = 'agt_0123456789abcdef0123456789abcdef';
  const store = await harness.importDashboardModule('js/snapshot-store.js');
  store.dashboardState.snapshot.value = { ...(store.dashboardState.snapshot.value || {}), agents: [{ agent_id: scribe, title: 'process-scribe', online: true }] };
  head = {
    ...view({ ...BASE_NODES, build: agentStep('Build it', { name: 'Build the thing' }) }, { hash: 'h2', rev: 'b' }),
    authorship: [{ ref: 'tpl_1@sha256:b', sourceHash: 'h2', actor: `agent:${scribe}`, authoredAt: '2026-11-16T02:00:00Z' }],
  };
  await waitFor(harness, () => root.querySelector('[data-mini-notice]'), 'the agent\'s version is merged in');
  assert.match(root.querySelector('[data-process-node="build"]').textContent, /Build the thing/, 'live on the canvas');
  assert.match(root.querySelector('[data-mini-notice]').textContent, /agent process-scribe changed Build the thing \(build\)/, 'it names the agent that saved');
  assert.equal(saves.length, 0, 'exactly theirs: nothing to save');

  await harness.act(async () => { root.querySelector('[data-mini-notice-undo]').click(); });
  assert.doesNotMatch(root.querySelector('[data-process-node="build"]').textContent, /Build the thing/, 'undo takes it out');
  await waitFor(harness, () => saves.length === 1, 'and the undo saves, over their version');
  assert.equal(saves[0].sourceHash, 'h2', 'against the agent\'s version, not a stale one');
  assert.equal(saves[0].draft.template.nodes.build.name, undefined);
});

test('an autosave refused because an agent saved first merges their version and saves again', async (t) => {
  let head = view(BASE_NODES);
  const saves = [];
  const api = {
    getTemplateHead: async () => structuredClone(head),
    saveTemplate: async (id, draft, meta) => {
      saves.push({ draft: structuredClone(draft), sourceHash: meta.sourceHash });
      if (meta.sourceHash !== head.sourceHash) throw new Error('process template head changed');
      head = { ...structuredClone(draft), sourceHash: `h${saves.length + 10}`, currentRef: `tpl_1@sha256:s${saves.length}` };
      return { ref: head.currentRef, sourceHash: head.sourceHash };
    },
  };
  const { harness, root } = await mountEditor(t, api);
  // The agent saves a new step; before the editor's next read the operator renames the process.
  head = view({ start: { type: 'start' }, build: agentStep('Build it'), review: agentStep('Review it'), done: { type: 'end', result: 'success' } }, { hash: 'h2', rev: 'b' });
  const title = root.querySelector('[data-mini-title]');
  await harness.act(async () => { title.value = 'Ship it fast'; harness.fireEvent(title, 'change'); });
  await waitFor(harness, () => saves.some((save) => save.sourceHash === 'h2'), 'it merges and saves on top of the agent\'s version');
  const last = saves.at(-1);
  assert.equal(last.draft.template.name, 'Ship it fast', 'my rename is kept');
  assert.ok(last.draft.template.nodes.review, 'and their step too');
  assert.equal(root.querySelector('[data-mini-autosave="failed"]'), null, 'never stuck as not saved');
});

test('when both change the same step the operator picks, and their other changes merge either way', async (t) => {
  let head = view(BASE_NODES);
  const api = {
    getTemplateHead: async () => structuredClone(head),
    saveTemplate: async (id, draft, meta) => {
      if (meta.sourceHash !== head.sourceHash) throw new Error('process template head changed');
      head = { ...structuredClone(draft), sourceHash: 'h9', currentRef: 'tpl_1@sha256:s9' };
      return { ref: head.currentRef, sourceHash: head.sourceHash };
    },
  };
  const { harness, root } = await mountEditor(t, api);
  head = view({ ...BASE_NODES, build: agentStep('Build it fast') }, { hash: 'h2', rev: 'b' });
  // The operator changes the same step's prompt before the merge lands.
  await harness.act(async () => { root.querySelector('[data-process-node="build"]').click(); });
  const asks = root.querySelector('.gm-card textarea');
  await harness.act(async () => { asks.value = 'Build it carefully'; harness.fireEvent(asks, 'input'); harness.fireEvent(asks, 'change'); });
  await waitFor(harness, () => root.querySelector('[data-mini-incoming]'), 'a conflict asks');
  assert.match(root.querySelector('[data-mini-incoming]').textContent, /also changes what you changed: build/);
  await harness.act(async () => { root.querySelector('[data-mini-keep-mine]').click(); });
  await waitFor(harness, () => head.sourceHash === 'h9', 'mine is saved over theirs');
  assert.equal(head.template.nodes.build.performer.prompt, 'Build it carefully');
});
