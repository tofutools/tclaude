import test from 'node:test';
import assert from 'node:assert/strict';
import {
  PROCESS_PORT_IN, PROCESS_PORT_OUT, processEdgePortAvailability,
  processNodePortAvailability, processNodePortAvailable, processPortUnavailableMessage,
} from '../dashboard/js/process-port-availability.js';

test('editor port availability is one closed semantic table for every node kind', () => {
  const expected = {
    start: { in: false, out: true },
    end: { in: true, out: false },
    task: { in: true, out: true },
    decision: { in: true, out: true },
    parallel: { in: true, out: true },
    wait: { in: true, out: true },
  };
  for (const [type, ports] of Object.entries(expected)) {
    assert.deepEqual(processNodePortAvailability({ type }), ports, type);
    assert.equal(processNodePortAvailable({ type }, PROCESS_PORT_IN), ports.in, `${type}/in`);
    assert.equal(processNodePortAvailable({ type }, PROCESS_PORT_OUT), ports.out, `${type}/out`);
  }
  assert.equal(processNodePortAvailable(null, 'in'), false);
  assert.equal(processNodePortAvailable({ type: 'task' }, 'sideways'), false);
});

test('edge endpoint availability names the exact unavailable semantic side', () => {
  assert.deepEqual(processEdgePortAvailability({ type: 'task' }, { type: 'end' }), {
    enabled: true, port: '', message: '',
  });
  assert.deepEqual(processEdgePortAvailability({ type: 'end' }, { type: 'task' }), {
    enabled: false, port: 'out', message: 'End nodes cannot have outgoing connections.',
  });
  assert.deepEqual(processEdgePortAvailability({ type: 'task' }, { type: 'start' }), {
    enabled: false, port: 'in', message: 'Start nodes cannot have incoming connections.',
  });
  assert.equal(processPortUnavailableMessage({ type: 'start' }, 'in'),
    'Start nodes cannot have incoming connections.');
});
