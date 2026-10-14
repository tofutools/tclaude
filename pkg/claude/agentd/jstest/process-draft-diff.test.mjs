// Unit tests for the proposal review's pure three-way diff and merge
// (dashboard/js/process-draft-diff.js), run with Node's built-in runner.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  diffTemplateDrafts, draftDiffCounts, decideDraftChange, decideAllDraftChanges,
  mergeDraftDecisions, sameDraftContent, overlayDraftDiff, pendingDraftChanges,
  draftChangeForNode, draftChangeForEdge,
} from '../dashboard/js/process-draft-diff.js';

function view(nodes, edges, extra = {}) {
  return {
    template: { id: 't', name: 'Flow', nodes, ...extra },
    edges: [{ from: '', outcome: 'start', to: 'a' }, ...edges],
    layout: { nodes: Object.fromEntries(Object.keys(nodes).map((id, i) => [id, { x: i * 100, y: 0 }])) },
  };
}

const base = view({
  a: { type: 'task', name: 'Build', performer: { kind: 'agent' } },
  b: { type: 'task', name: 'Test', performer: { kind: 'agent' } },
  done: { type: 'end' },
}, [{ from: 'a', outcome: 'pass', to: 'b' }, { from: 'b', outcome: 'pass', to: 'done' }]);

// The scribe inserts a review gate between b and done, renames a, and drops nothing.
const theirs = view({
  a: { type: 'task', name: 'Build it', performer: { kind: 'agent' } },
  b: { type: 'task', name: 'Test', performer: { kind: 'agent' } },
  gate: { type: 'decision', name: 'Review', performer: { kind: 'human', ask: 'ok?' } },
  done: { type: 'end' },
}, [{ from: 'a', outcome: 'pass', to: 'b' }, { from: 'b', outcome: 'pass', to: 'gate' }, { from: 'gate', outcome: 'yes', to: 'done' }]);

test('diff lists added, changed and retargeted items with dependencies', () => {
  const changes = diffTemplateDrafts({ base, ours: base, theirs });
  assert.deepEqual(changes.map((c) => `${c.op}:${c.id}`), [
    'added:node:gate', 'changed:node:a', 'added:edge:gate:yes', 'changed:edge:b:pass',
  ]);
  const retarget = changes.find((c) => c.id === 'edge:b:pass');
  assert.equal(retarget.previousTo, 'done');
  assert.deepEqual(retarget.requires, ['node:gate'], 'a retarget onto an added node needs that node');
  assert.deepEqual(changes.find((c) => c.id === 'node:a').fields, ['name']);
  assert.ok(changes.every((c) => !c.conflict));
  assert.deepEqual(draftDiffCounts(changes), {
    total: 4, added: 2, changed: 2, removed: 0, conflicts: 0, pending: 4, accepted: 0, rejected: 0,
  });
});

test('local edits that already match are skipped; differing local edits conflict', () => {
  const ours = structuredClone(base);
  ours.template.nodes.a.name = 'Build it';
  ours.template.nodes.b.name = 'Test locally';
  const theirsB = structuredClone(theirs);
  theirsB.template.nodes.b.name = 'Test remotely';
  const changes = diffTemplateDrafts({ base, ours, theirs: theirsB });
  assert.ok(!draftChangeForNode(changes, 'a'), 'ours already equals theirs for a');
  assert.equal(draftChangeForNode(changes, 'b').conflict, true);
  assert.equal(draftChangeForEdge(changes, 'b', 'pass').conflict, false);
});

test('accepting pulls in requirements; rejecting pushes out dependants', () => {
  const changes = diffTemplateDrafts({ base, ours: base, theirs });
  let { decisions, affected } = decideDraftChange(changes, {}, 'edge:b:pass', 'accept');
  assert.deepEqual(affected.sort(), ['edge:b:pass', 'node:gate']);
  ({ decisions, affected } = decideDraftChange(changes, decisions, 'node:gate', 'reject'));
  assert.deepEqual(affected.sort(), ['edge:b:pass', 'edge:gate:yes', 'node:gate']);
  assert.equal(decisions['edge:b:pass'], 'reject');
  assert.deepEqual(pendingDraftChanges(changes, decisions).map((c) => c.id), ['node:a']);
  ({ decisions } = decideDraftChange(changes, decisions, 'node:gate', 'pending'));
  assert.equal(decisions['node:gate'], undefined, 'undo returns only that change to pending');
});

test('accept-all skips conflicts unless explicitly included', () => {
  const ours = structuredClone(base);
  ours.template.nodes.a.name = 'Mine';
  const changes = diffTemplateDrafts({ base, ours, theirs });
  const decisions = decideAllDraftChanges(changes, {}, 'accept');
  assert.equal(decisions['node:a'], undefined);
  assert.equal(decideAllDraftChanges(changes, {}, 'accept', { includeConflicts: true })['node:a'], 'accept');
});

test('merge applies exactly the accepted changes and keeps the rest of ours', () => {
  const ours = structuredClone(base);
  ours.layout.nodes.a = { x: 999, y: 999 };
  const changes = diffTemplateDrafts({ base, ours, theirs });
  let { decisions } = decideDraftChange(changes, {}, 'edge:b:pass', 'accept');
  decisions = decideDraftChange(changes, decisions, 'edge:gate:yes', 'accept').decisions;
  decisions = decideDraftChange(changes, decisions, 'node:a', 'reject').decisions;
  const merged = mergeDraftDecisions({ ours, theirs, changes, decisions });
  assert.equal(merged.template.nodes.a.name, 'Build', 'rejected rename stays ours');
  assert.equal(merged.template.nodes.gate.type, 'decision');
  assert.deepEqual(merged.layout.nodes.gate, theirs.layout.nodes.gate, 'an accepted node adopts its proposed position');
  assert.deepEqual(merged.layout.nodes.a, { x: 999, y: 999 }, 'ours keeps its own positions');
  assert.deepEqual(merged.edges.map((e) => `${e.from}:${e.outcome}>${e.to}`).sort(),
    [':start>a', 'a:pass>b', 'b:pass>gate', 'gate:yes>done']);
  assert.equal(merged.template.start, 'a');
  assert.ok(!sameDraftContent(merged, theirs));
  const all = mergeDraftDecisions({ ours: base, theirs, changes, decisions: decideAllDraftChanges(changes, {}, 'accept') });
  assert.ok(sameDraftContent(all, { ...theirs, layout: { nodes: { ...base.layout.nodes, gate: theirs.layout.nodes.gate } } }));
});

test('removing a node requires its edge changes and drops its connectors', () => {
  const smaller = view({ a: base.template.nodes.a, done: { type: 'end' } }, [{ from: 'a', outcome: 'pass', to: 'done' }]);
  const changes = diffTemplateDrafts({ base, ours: base, theirs: smaller });
  const removal = draftChangeForNode(changes, 'b');
  assert.equal(removal.op, 'removed');
  assert.deepEqual(removal.requires.sort(), ['edge:a:pass', 'edge:b:pass']);
  const { decisions } = decideDraftChange(changes, {}, 'node:b', 'accept');
  const merged = mergeDraftDecisions({ ours: base, theirs: smaller, changes, decisions });
  assert.equal(merged.template.nodes.b, undefined);
  assert.deepEqual(merged.edges.map((e) => `${e.from}:${e.outcome}>${e.to}`).sort(), [':start>a', 'a:pass>done']);
});

test('overlay paints proposals, removals and a retarget ghost for the graph', () => {
  const changes = diffTemplateDrafts({ base, ours: base, theirs });
  const graph = {
    nodes: Object.keys(base.template.nodes).map((id) => ({ id, type: base.template.nodes[id].type, label: id })),
    edges: base.edges.filter((e) => e.from).map((e) => ({ id: `${e.from}:${e.outcome}`, from: e.from, to: e.to, outcome: e.outcome })),
  };
  const out = overlayDraftDiff(graph, { theirs, changes, decisions: {}, cursor: 'node:gate' });
  const gate = out.nodes.find((n) => n.id === 'gate');
  assert.equal(gate.proposed, true);
  assert.deepEqual([gate.diff.op, gate.diff.current, gate.diff.tag], ['added', true, '+ added']);
  assert.equal(out.nodes.find((n) => n.id === 'a').label, 'Build it', 'a pending rename previews the proposed label');
  const ghost = out.edges.find((e) => e.ghostOf === 'edge:b:pass');
  assert.equal(ghost.to, 'done');
  assert.equal(ghost.diff.ghost, true);
  assert.ok(out.edges.some((e) => e.proposed && e.from === 'b' && e.to === 'gate'));
  assert.equal(graph.nodes.length, 3, 'the input graph is not mutated');
});

// Review findings: a removal that would drop a local edge, and keep-mine
// through a dependency.
const wxy = view({
  W: { type: 'task', name: 'W' }, X: { type: 'task', name: 'X' }, Y: { type: 'task', name: 'Y' },
}, [{ from: 'W', outcome: 'ok', to: 'X' }, { from: 'Y', outcome: 'ok', to: 'W' }]);

test('keep-mine in accept-all never pulls a conflict in through a dependency', () => {
  const ours = structuredClone(wxy);
  ours.edges.find((e) => e.from === 'W').to = 'Y';
  const theirs = structuredClone(wxy);
  delete theirs.template.nodes.X;
  theirs.edges = theirs.edges.filter((e) => e.from !== 'W');
  const changes = diffTemplateDrafts({ base: wxy, ours, theirs });
  assert.equal(changes.find((c) => c.id === 'edge:W:ok').conflict, true);
  const kept = decideAllDraftChanges(changes, {}, 'accept');
  assert.equal(kept['edge:W:ok'], undefined, 'the conflict is not accepted through node:X\'s closure');
  assert.equal(kept['node:X'], undefined);
  let decisions = decideDraftChange(changes, {}, 'edge:W:ok', 'reject').decisions;
  decisions = decideAllDraftChanges(changes, decisions, 'accept');
  assert.equal(decisions['node:X'], 'reject', 'a change requiring a rejected conflict is rejected with it');
  const merged = mergeDraftDecisions({ ours, theirs, changes, decisions });
  assert.equal(merged.edges.find((e) => e.from === 'W')?.to, 'Y', 'the human\'s edge survives');
});

test('removing a node a local retarget points at is a conflict', () => {
  const ours = structuredClone(wxy);
  ours.edges.find((e) => e.from === 'Y').to = 'X';
  const theirs = structuredClone(wxy);
  delete theirs.template.nodes.X;
  theirs.edges = theirs.edges.filter((e) => e.to !== 'X');
  const changes = diffTemplateDrafts({ base: wxy, ours, theirs });
  assert.equal(draftChangeForNode(changes, 'X').conflict, true);
});

test('an accepted edge to a node deleted locally is a conflict and restores the node', () => {
  const ours = structuredClone(wxy);
  delete ours.template.nodes.X;
  ours.edges = ours.edges.filter((e) => e.to !== 'X');
  const theirs = structuredClone(wxy);
  theirs.edges.push({ from: 'Y', outcome: 'again', to: 'X' });
  const changes = diffTemplateDrafts({ base: wxy, ours, theirs });
  const added = draftChangeForEdge(changes, 'Y', 'again');
  assert.equal(added.conflict, true);
  const merged = mergeDraftDecisions({ ours, theirs, changes, decisions: decideDraftChange(changes, {}, added.id, 'accept').decisions });
  assert.ok(merged.template.nodes.X, 'taking the edge brings its endpoint back');
  assert.ok(merged.edges.some((e) => e.from === 'Y' && e.outcome === 'again' && e.to === 'X'));
});
