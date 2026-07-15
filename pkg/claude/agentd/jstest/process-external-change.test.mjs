import test from 'node:test';
import assert from 'node:assert/strict';
import {
  CHANGE_SUMMARY_LIMITS, NO_EXTERNAL_CHANGE, attachExternalReview, keepExternalChange, reconcileExternalChange,
  summarizeTemplateChange, templateHeadFromEditView, templateHeadSignature,
} from '../dashboard/js/process-external-change.js';

const pending = (kind, ref, sourceHash) => ({ kind, ref, sourceHash, actor: '', authoredAt: '' });

const observe = (previous, loadedRef, currentRef, dirty = false, loadedSourceHash = 'source-a', currentSourceHash = 'source-b') => reconcileExternalChange(previous, {
  loadedRef, loadedSourceHash, currentRef, currentSourceHash, dirty,
});

test('template head signatures are stable across ordering and include set changes', () => {
  const a = templateHeadSignature([{ id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }, { id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-a' }]);
  const b = templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-a' }, { id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }]);
  assert.equal(a, b);
  assert.notEqual(a, templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-a' }]));
  assert.notEqual(a, templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:3', sourceHash: 'source-c' }, { id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }]));
  assert.notEqual(a, templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-new' }, { id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }]));
  assert.notEqual(a, templateHeadSignature([{ id: 'alpha', ref: 'a@sha256:1', sourceHash: 'source-a', actor: 'agent:agt_changed' }, { id: 'beta', ref: 'b@sha256:2', sourceHash: 'source-b' }]));
});

test('external generation comparison ignores blanks and the loaded head', () => {
  assert.equal(observe(NO_EXTERNAL_CHANGE, '', 'release@sha256:new'), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', ''), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', false, '', 'source-b'), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', false, 'source-a', ''), NO_EXTERNAL_CHANGE);
  assert.equal(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', false, 'source-a', 'source-a'), NO_EXTERNAL_CHANGE);
});

test('a moved head distinguishes clean and dirty buffers', () => {
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b'), {
    ...pending('clean', 'release@sha256:b', 'source-b'),
  });
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b', true), {
    ...pending('dirty', 'release@sha256:b', 'source-b'),
  });
});

test('a source-only generation change distinguishes clean and dirty buffers', () => {
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', false, 'source-a', 'source-b'), {
    ...pending('clean', 'release@sha256:a', 'source-b'),
  });
  assert.deepEqual(observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:a', true, 'source-a', 'source-b'), {
    ...pending('dirty', 'release@sha256:a', 'source-b'),
  });
});

test('banner state follows buffer dirtiness while the same external head is pending', () => {
  const clean = observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b');
  clean.review = { summary: { addedNodes: ['review'] } };
  const dirty = observe(clean, 'release@sha256:a', 'release@sha256:b', true);
  assert.deepEqual(dirty, { ...pending('dirty', 'release@sha256:b', 'source-b'), review: clean.review });
  assert.deepEqual(observe(dirty, 'release@sha256:a', 'release@sha256:b'), {
    ...pending('clean', 'release@sha256:b', 'source-b'), review: clean.review,
  });
});

test('Keep editing dismisses only the observed ref and a later head resurfaces', () => {
  const dirty = observe(NO_EXTERNAL_CHANGE, 'release@sha256:a', 'release@sha256:b', true);
  const kept = keepExternalChange(dirty);
  assert.deepEqual(kept, pending('kept', 'release@sha256:b', 'source-b'));
  assert.equal(observe(kept, 'release@sha256:a', 'release@sha256:b', true), kept);
  assert.deepEqual(observe(kept, 'release@sha256:a', 'release@sha256:c', true), {
    ...pending('dirty', 'release@sha256:c', 'source-b'),
  });
  assert.deepEqual(observe(kept, 'release@sha256:a', 'release@sha256:b', true, 'source-a', 'source-c'), {
    ...pending('dirty', 'release@sha256:b', 'source-c'),
  });
  assert.equal(observe(kept, 'release@sha256:b', 'release@sha256:b', true, 'source-b', 'source-b'), NO_EXTERNAL_CHANGE);
});

test('exact generation attribution never falls back to another source-only save', () => {
  const view = {
    currentRef: 'release@sha256:b', sourceHash: 'source-new',
    authorship: [
      { ref: 'release@sha256:b', sourceHash: 'source-old', actor: 'agent:agt_old', authoredAt: '2026-07-15T01:00:00Z' },
      { ref: 'release@sha256:b', sourceHash: 'source-new', actor: 'agent:agt_new', authoredAt: '2026-07-15T02:00:00Z' },
    ],
  };
  assert.deepEqual(templateHeadFromEditView(view), {
    ref: 'release@sha256:b', sourceHash: 'source-new', actor: 'agent:agt_new', authoredAt: '2026-07-15T02:00:00Z',
  });
  assert.deepEqual(templateHeadFromEditView({ ...view, sourceHash: 'source-unknown' }), {
    ref: 'release@sha256:b', sourceHash: 'source-unknown', actor: '', authoredAt: '',
  }, 'unknown attribution stays unknown');
});

test('graph and canonical-source review is concise and attaches only to its requested generation', () => {
  const before = {
    template: { id: 'release', name: 'Release', nodes: { start: { type: 'start' }, old: { type: 'task' } } },
    edges: [{ from: 'start', outcome: 'pass', to: 'old' }], source: 'id: release\nname: Release\nold: true\n',
  };
  const after = {
    template: { id: 'release', name: 'Release v2', nodes: { start: { type: 'start' }, review: { type: 'task' } } },
    edges: [{ from: 'start', outcome: 'pass', to: 'review' }], source: 'id: release\nname: Release v2\nreview: true\n',
    currentRef: 'release@sha256:new', sourceHash: 'source-new',
  };
  const summary = summarizeTemplateChange(before, after);
  assert.deepEqual(summary.addedNodes, ['review']);
  assert.deepEqual(summary.removedNodes, ['old']);
  assert.equal(summary.addedEdges, 1); assert.equal(summary.removedEdges, 1);
  assert.equal(summary.metadataChanged, true);
  assert.equal(summary.source.firstLine, 2);

  const latest = pending('dirty', 'release@sha256:new', 'source-new');
  assert.ok(attachExternalReview(latest, after, before).review);
  const stale = { ...after, currentRef: 'release@sha256:old', sourceHash: 'source-old' };
  assert.equal(attachExternalReview(latest, stale, before), latest, 'stale response cannot replace the latest review target');
});

test('a local edge/start save followed by a canonical source/layout-only save has no false semantic change', () => {
  const before = {
    template: {
      id: 'release', start: 'stale-start',
      nodes: {
        start: { type: 'start', next: { pass: 'stale-target' } },
        done: { type: 'end' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'start' },
      { from: 'start', outcome: 'pass', to: 'done' },
    ],
    layout: { nodes: { start: { x: 10, y: 10 } } },
  };
  const after = {
    template: {
      id: 'release', start: 'start',
      nodes: {
        start: { type: 'start', next: { pass: 'done' } },
        done: { type: 'end' },
      },
    },
    edges: structuredClone(before.edges),
    layout: { nodes: { start: { x: 200, y: 300 } } },
    source: 'id: release\nstart: start\n',
  };
  const summary = summarizeTemplateChange(before, after);
  assert.equal(summary.metadataChanged, false);
  assert.equal(summary.changedNodeCount, 0);
  assert.equal(summary.addedEdges, 0);
  assert.equal(summary.removedEdges, 0);

  const topologyChange = summarizeTemplateChange(after, {
    ...after,
    template: { ...after.template, start: 'done' },
    edges: [
      { from: '', outcome: 'start', to: 'done' },
      { from: 'start', outcome: 'pass', to: 'done' },
    ],
  });
  assert.equal(topologyChange.metadataChanged, false, 'derived start stays out of settings comparison');
  assert.equal(topologyChange.addedEdges, 1, 'the new exact start pseudo-edge is reported');
  assert.equal(topologyChange.removedEdges, 1, 'the replaced exact start pseudo-edge is reported');
});

test('change summaries retain exact totals while bounding node ids and source previews', () => {
  const count = CHANGE_SUMMARY_LIMITS.nodeIDs + 9;
  const afterNodes = Object.fromEntries(Array.from({ length: count }, (_, index) => [`added-${String(index).padStart(3, '0')}`, { type: 'task' }]));
  const longASCII = 'x'.repeat(CHANGE_SUMMARY_LIMITS.sourceCharactersPerLine + 100);
  const longUTF8 = '界'.repeat(CHANGE_SUMMARY_LIMITS.sourceBytesPerLine);
  const changedLines = Array.from({ length: CHANGE_SUMMARY_LIMITS.sourceLinesPerSide + 3 }, (_, index) => `${index}-${index % 2 ? longASCII : longUTF8}`);
  const summary = summarizeTemplateChange(
    { template: { id: 'bounded', nodes: {} }, edges: [], source: 'id: bounded\nold\n' },
    { template: { id: 'bounded', nodes: afterNodes }, edges: [], source: `id: bounded\n${changedLines.join('\n')}\n` },
  );

  assert.equal(summary.addedNodeCount, count);
  assert.equal(summary.addedNodes.length, CHANGE_SUMMARY_LIMITS.nodeIDs);
  assert.equal(summary.addedNodesTruncated, true);
  assert.ok(summary.source.before.length <= CHANGE_SUMMARY_LIMITS.sourceLinesPerSide);
  assert.ok(summary.source.after.length <= CHANGE_SUMMARY_LIMITS.sourceLinesPerSide);
  assert.equal(summary.source.truncated, true);
  assert.equal(summary.source.truncation.lines, true);
  assert.equal(summary.source.truncation.characters, true);
  assert.equal(summary.source.truncation.bytes, true);
  const preview = [...summary.source.before, ...summary.source.after];
  assert.ok(preview.reduce((total, line) => total + line.length, 0) <= CHANGE_SUMMARY_LIMITS.sourceCharacters);
  assert.ok(preview.reduce((total, line) => total + new TextEncoder().encode(line).length, 0) <= CHANGE_SUMMARY_LIMITS.sourceBytes);
  assert.ok(preview.every((line) => line.length <= CHANGE_SUMMARY_LIMITS.sourceCharactersPerLine));
  assert.ok(preview.every((line) => new TextEncoder().encode(line).length <= CHANGE_SUMMARY_LIMITS.sourceBytesPerLine));
});
