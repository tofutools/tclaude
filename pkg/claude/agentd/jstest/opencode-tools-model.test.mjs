import test from 'node:test';
import assert from 'node:assert/strict';
import {
  composeOpenCodeTools, openCodeToolsBase, parseOpenCodeTools, withOpenCodeToolsBase,
} from '../dashboard/js/opencode-tools-model.js';

test('the base is the part before the first entry', () => {
  assert.equal(openCodeToolsBase('all,question=deny'), 'all');
  assert.equal(openCodeToolsBase('allow'), 'allow');
  assert.equal(openCodeToolsBase(''), '');
});

test('entries parse over their defaults', () => {
  assert.deepEqual(parseOpenCodeTools('all'), { base: 'all', entries: { question: 'allow', plan: 'allow', env: 'deny' } });
  assert.deepEqual(parseOpenCodeTools('all,plan=deny,env=allow').entries, { question: 'allow', plan: 'deny', env: 'allow' });
});

test('the canonical spelling matches the Go side: defaults dropped, fixed order', () => {
  assert.equal(composeOpenCodeTools('all', { env: 'allow', question: 'deny' }), 'all,question=deny,env=allow');
  assert.equal(composeOpenCodeTools('all', { question: 'allow', plan: 'allow', env: 'deny' }), 'all');
  assert.equal(composeOpenCodeTools('allow', { question: 'deny' }), 'allow', 'entries only follow all');
});

test('changing the base keeps the entries only while it is all', () => {
  assert.equal(withOpenCodeToolsBase('all,question=deny', 'deny'), 'deny');
  assert.equal(withOpenCodeToolsBase('allow', 'all'), 'all');
});
