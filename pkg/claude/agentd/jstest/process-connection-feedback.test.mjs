import test from 'node:test';
import assert from 'node:assert/strict';
import { ProcessEditModel } from '../dashboard/js/process-edit-model.js';
import {
  prepareProcessConnectionFeedback, resolveProcessConnectionFeedback,
} from '../dashboard/js/process-connection-feedback.js';

function model(config = {}) {
  return new ProcessEditModel({
    template: {
      id: 'feedback', start: 'begin', nodes: {
        begin: { type: 'start', name: 'Begin' },
        build: { type: 'task', name: 'Build' },
        review: { type: 'decision', name: 'Review' },
        ship: { type: 'end', name: 'Ship' },
      },
    },
    edges: [{ from: '', outcome: 'start', to: 'begin' }],
    layout: { nodes: {} },
  }, config);
}

const source = (candidate, phase = 'target') => ({
  phase, source: { nodeId: 'build', port: 'out' }, candidate,
});
const inputSource = (candidate) => ({
  phase: 'target', source: { nodeId: 'build', port: 'in' }, candidate,
});

test('connection feedback is the complete non-mutating editor preflight table', () => {
  const current = model();
  const before = current.saveBody();
  const cases = [
    ['input source', { phase: 'source', source: { nodeId: 'build', port: 'in' } }, 'available', /predecessor/],
    ['output source', source(null, 'source'), 'available', /successor/],
    ['impossible start input', { phase: 'source', source: { nodeId: 'begin', port: 'in' } }, 'disabled', /cannot have incoming/],
    ['impossible end output', { phase: 'source', source: { nodeId: 'ship', port: 'out' } }, 'disabled', /cannot have outgoing/],
    ['output to input', source({ nodeId: 'review', port: 'in' }), 'valid', /Build to Review/],
    ['output to output preserves accepted direction', source({ nodeId: 'review', port: 'out' }), 'valid', /Build to Review/],
    ['output to node body', source({ nodeId: 'review' }), 'valid', /Build to Review/],
    ['input to output reverses direction', inputSource({ nodeId: 'review', port: 'out' }), 'valid', /Review to Build/],
    ['input to node body reverses direction', inputSource({ nodeId: 'review' }), 'valid', /Review to Build/],
    ['input to input', inputSource({ nodeId: 'review', port: 'in' }), 'invalid', /output port/],
    ['same source port', source({ nodeId: 'build', port: 'out' }), 'source', /successor/],
    ['own body', source({ nodeId: 'build' }), 'invalid', /different node/],
    ['own other port', source({ nodeId: 'build', port: 'in' }), 'invalid', /Self-loop/],
    ['end as predecessor', inputSource({ nodeId: 'ship', port: 'out' }), 'invalid', /cannot have outgoing/],
    ['start as successor port', source({ nodeId: 'begin', port: 'out' }), 'invalid', /cannot have incoming/],
    ['start as successor body', source({ nodeId: 'begin' }), 'invalid', /cannot have incoming/],
    ['empty canvas insertion', source({ emptyCanvas: true }), 'valid', /new successor/],
    ['non-action surface', source({}), 'neutral', /^$/],
  ];
  for (const [name, request, state, message] of cases) {
    const feedback = resolveProcessConnectionFeedback(current, request);
    assert.equal(feedback.state, state, name);
    assert.match(feedback.message, message, name);
  }
  assert.deepEqual(resolveProcessConnectionFeedback(current, source({ nodeId: 'review' })), {
    state: 'valid', enabled: true, message: 'Drop to connect Build to Review.', from: 'build', to: 'review',
  });
  assert.deepEqual(current.saveBody(), before, 'preflight never mutates graph or save state');
});

test('connection feedback includes insertion and edge editability gates', () => {
  assert.deepEqual(resolveProcessConnectionFeedback(model({ canInsert: false }), source({ emptyCanvas: true })), {
    state: 'invalid', enabled: false, message: 'Adding connected nodes is not allowed in this view.',
  });
  assert.deepEqual(resolveProcessConnectionFeedback(model({ edgeEditable: () => false }), source({ nodeId: 'review' })), {
    state: 'invalid', enabled: false, message: 'This connection is read-only in this view.',
  });
});

test('prepared target feedback bounds supported-scale edge work to one O(E + N) pass', () => {
  const nodes = {};
  const edges = [];
  for (let index = 0; index < 2048; index += 1) {
    const id = `node-${index}`;
    nodes[id] = { type: 'task', name: id };
    edges.push({ from: id, outcome: 'pass', to: `node-${(index + 1) % 2048}` });
    edges.push({ from: id, outcome: 'pass-2', to: `node-${(index + 2) % 2048}` });
  }
  const current = new ProcessEditModel({ template: { nodes }, edges, layout: { nodes: {} } });
  let edgeVisits = 0;
  const storedEdges = current.edges;
  current.edges = new Proxy(storedEdges, {
    get(target, property, receiver) {
      if (property !== Symbol.iterator) return Reflect.get(target, property, receiver);
      return function* countedEdges() {
        for (const edge of target) {
          edgeVisits += 1;
          yield edge;
        }
      };
    },
  });
  let fallbackScans = 0;
  const originalFreeOutcome = current.freeOutcome.bind(current);
  current.freeOutcome = (...args) => { fallbackScans += 1; return originalFreeOutcome(...args); };

  const prepared = prepareProcessConnectionFeedback(current);
  assert.equal(edgeVisits, 4096, 'preparation visits each supported-bound edge exactly once');
  assert.equal(prepared.freeOutcome('node-1'), 'pass-3', 'prepared outcome matches the model rule');
  for (let index = 1; index < 2048; index += 1) {
    const feedback = resolveProcessConnectionFeedback(current, {
      phase: 'target', source: { nodeId: 'node-0', port: 'in' },
      candidate: { nodeId: `node-${index}` },
    }, prepared);
    assert.equal(feedback.state, 'valid');
  }
  assert.equal(edgeVisits, 4096, 'all node/body/port resolutions reuse the one edge index');
  assert.equal(fallbackScans, 0, 'prepared presentation never falls back to per-target edge scans');
  assert.equal(current.freeOutcome('node-1'), prepared.freeOutcome('node-1'),
    'prepared and commit-time outcome selection stay equivalent');
});
