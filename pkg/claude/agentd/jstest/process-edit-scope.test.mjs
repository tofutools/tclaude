// Unit tests for subprocess scopes in the process edit model (E1): drilling
// into an inline ▣ body, editing it, ports, undo across scopes, and the
// root-folded save body.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ProcessEditModel, scopeEdges, foldBody, processOutline, blankSubprocessBody,
} from '../dashboard/js/process-edit-model.js';

function view() {
  return {
    template: {
      id: 'release', name: 'Release', start: 'begin', params: { branch: { type: 'string' } },
      nodes: {
        begin: { type: 'start' },
        qa: {
          type: 'subprocess', name: 'QA suite',
          subprocess: {
            params: { target: { type: 'string' } },
            start: 'in',
            nodes: {
              in: { type: 'start', next: { pass: 'unit' } },
              unit: { type: 'task', name: 'Unit', performer: { kind: 'program', run: 'make' }, next: { pass: 'ok', fail: 'bad' } },
              ok: { type: 'end', result: 'success' },
              bad: { type: 'end', result: 'failure' },
            },
            inputs: { target: '{{ params.branch }}' },
            outputs: { ok: 'pass', bad: 'fail' },
          },
        },
        ship: { type: 'end', result: 'success' },
        stop: { type: 'end', result: 'failure' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'begin' },
      { from: 'begin', outcome: 'pass', to: 'qa' },
      { from: 'qa', outcome: 'pass', to: 'ship' },
      { from: 'qa', outcome: 'fail', to: 'stop' },
    ],
    layout: { nodes: { begin: { x: 0, y: 0 }, qa: { x: 100, y: 0 }, 'qa/unit': { x: 40, y: 50 } } },
    currentRef: 'release@sha256:1', sourceHash: 'h1', semanticHash: 's1',
  };
}

test('next maps normalize to edges and fold back losslessly', () => {
  const body = view().template.nodes.qa.subprocess;
  const edges = scopeEdges(body);
  assert.deepEqual(edges.map((e) => `${e.from}:${e.outcome}>${e.to}`), [':start>in', 'in:pass>unit', 'unit:fail>bad', 'unit:pass>ok']);
  const nodes = Object.fromEntries(Object.entries(body.nodes).map(([id, { next, ...rest }]) => [id, rest]));
  assert.deepEqual(foldBody(body, { nodes, params: body.params }, edges), body);
});

test('entering projects the body; edits fold back into the root save body', () => {
  const model = new ProcessEditModel(view());
  assert.ok(model.canEnterScope('qa'));
  model.enterScope('qa');
  assert.deepEqual(model.scopePath(), ['qa']);
  assert.deepEqual(model.scopeTrail().map((s) => s.label), ['Release', 'QA suite']);
  assert.deepEqual(Object.keys(model.template.nodes).sort(), ['bad', 'in', 'ok', 'unit']);
  assert.equal(model.template.nodes.unit.next, undefined, 'scope nodes carry no next');
  assert.deepEqual(model.layout.nodes, { unit: { x: 40, y: 50 } }, 'namespaced layout is projected');
  assert.equal(model.dirty, false, 'entering is navigation, not an edit');

  const lint = model.addNode('task', { x: 200, y: 60, id: 'lint', name: 'Lint' });
  model.setEdgeTarget('in', 'pass', lint);
  model.addEdge(lint, 'pass', 'unit');
  assert.ok(model.dirty);

  const saved = model.saveBody();
  assert.equal(saved.template.id, 'release');
  const body = saved.template.nodes.qa.subprocess;
  assert.deepEqual(body.nodes.in.next, { pass: 'lint' });
  assert.deepEqual(body.nodes.lint.next, { pass: 'unit' });
  assert.deepEqual(body.inputs, { target: '{{ params.branch }}' }, 'ports survive the fold');
  assert.deepEqual(saved.layout.nodes['qa/lint'], { x: 200, y: 60 });
  assert.ok(saved.edges.some((e) => e.from === 'qa' && e.outcome === 'pass'), 'root edges are the root\'s');

  assert.equal(model.exitScope(), 'qa');
  assert.ok(model.template.nodes.qa.subprocess.nodes.lint);
  assert.deepEqual(model.saveBody(), saved, 'exiting folds the same content');
});

test('undo is one timeline across scopes and keeps the open scope', () => {
  const model = new ProcessEditModel(view());
  model.enterScope('qa');
  model.renameNode('unit', 'Unit tests');
  model.exitScope();
  model.renameNode('ship', 'Ship it');
  model.enterScope('qa');
  model.undo();
  assert.deepEqual(model.scopePath(), ['qa'], 'undo keeps the drill-in');
  assert.equal(model.template.nodes.unit.name, 'Unit tests');
  model.undo();
  assert.equal(model.template.nodes.unit.name, 'Unit');
  assert.equal(model.dirty, false);
  model.redo();
  model.redo();
  model.exitScope();
  assert.equal(model.template.nodes.ship.name, 'Ship it');
});

test('ports: inputs bind body params, outputs map ends onto parent outcomes', () => {
  const model = new ProcessEditModel(view());
  model.enterScope('qa');
  const ports = model.scopePorts();
  assert.deepEqual([ports.params, ports.parentParams, ports.outcomes, ports.ends], [['target'], ['branch'], ['fail', 'pass'], ['bad', 'ok']]);
  assert.equal(model.setScopeOutput('bad', 'pass'), true);
  assert.equal(model.setScopeInput('target', ''), true, 'empty unbinds');
  assert.deepEqual(model.scopePorts().outputs, { ok: 'pass', bad: 'pass' });
  assert.deepEqual(model.scopePorts().inputs, {});
  const saved = model.saveBody().template.nodes.qa.subprocess;
  assert.equal(saved.inputs, undefined);
  model.undo();
  assert.deepEqual(model.scopePorts().inputs, { target: '{{ params.branch }}' });
});

test('new ▣ nodes get a runnable body; deleting one drops its body layout', () => {
  const model = new ProcessEditModel(view());
  const id = model.addNode('subprocess', { x: 1, y: 1 });
  assert.deepEqual(model.node(id).subprocess, blankSubprocessBody());
  assert.ok(model.canEnterScope(id));
  model.deleteNode('qa');
  assert.equal(model.layout.nodes['qa/unit'], undefined);
});

test('a template reference is not entered; extraction swaps the body for a ref', () => {
  const model = new ProcessEditModel(view());
  model.enterScope('qa');
  const extracted = model.extractableBody('QA suite');
  assert.equal(extracted.template.name, 'QA suite');
  assert.equal(extracted.template.id, undefined);
  assert.deepEqual(extracted.template.params, { target: { type: 'string' } });
  assert.ok(extracted.edges.some((e) => e.from === '' && e.to === 'in'));
  model.exitScope();
  model.replaceSubprocessWithRef('qa', 'qa-suite');
  assert.deepEqual(model.node('qa').subprocess, {
    templateRef: 'qa-suite', inputs: { target: '{{ params.branch }}' }, outputs: { ok: 'pass', bad: 'fail' },
  });
  assert.equal(model.layout.nodes['qa/unit'], undefined);
  assert.equal(model.canEnterScope('qa'), false);
  assert.deepEqual(processOutline(model.template).find((e) => e.id === 'qa'), { id: 'qa', name: 'QA suite', type: 'subprocess', ref: 'qa-suite' });
});

test('outline nests inline bodies and template settings stay at the root', () => {
  const model = new ProcessEditModel(view());
  model.enterScope('qa');
  const qa = model.outline().find((e) => e.id === 'qa');
  assert.deepEqual(qa.children.map((c) => c.id), ['bad', 'in', 'ok', 'unit']);
  assert.throws(() => model.setTemplateMeta({ name: 'x' }), /root process/);
});

test('nested-body diagnostics badge inside the scope and roll up onto the ▣ node at the root', async () => {
  const { mapDiagnostics } = await import('../dashboard/js/process-validation.js');
  const diagnostics = [
    { scope: 'node', targetId: 'qa/unit', severity: 'error', code: 'missing_performer', message: 'm1' },
    { scope: 'edge', targetId: 'qa/unit:pass', severity: 'warning', code: 'w', message: 'm2' },
    { scope: 'node', targetId: 'ship', severity: 'warning', code: 'w', message: 'm3' },
  ];
  const model = new ProcessEditModel(view());
  let mapped = mapDiagnostics(diagnostics, model);
  assert.deepEqual(mapped.nodes.get('qa'), { error: 1, warning: 1 }, 'the root badges the ▣ node for its body');
  assert.deepEqual(mapped.nodes.get('ship'), { error: 0, warning: 1 });
  assert.deepEqual(mapped.entries.find((e) => e.code === 'missing_performer').location, { path: ['qa'], node: 'unit' });

  model.enterScope('qa');
  mapped = mapDiagnostics(diagnostics, model);
  assert.deepEqual(mapped.nodes.get('unit'), { error: 1, warning: 0 });
  assert.equal(mapped.edges.size, 1, 'the body edge badges inside the scope');
  assert.equal(mapped.nodes.get('ship'), undefined, 'a parent node is not mistaken for a body node');
  assert.equal(mapped.entries.find((e) => e.targetId === 'ship').scope, 'template');
});
