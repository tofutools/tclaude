// Tests for the templates manager's circle scribe (tcl-i5vaex): both "Edit
// with agent" buttons start it through Get help's shared summon, so the
// operator confirms the templates.manage grant and it opens in its regular
// overlay chat window.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [{ summonTemplateScribe }, facade] = await Promise.all([
    harness.importDashboardModule('js/template-scribe.js'),
    harness.importDashboardModule('js/companion-facade.js'),
  ]);
  const opened = [];
  t.after(facade.registerCompanionController({ open: (agentId, options) => { opened.push({ agentId, ...options }); return true; } }));
  const calls = [];
  const fetchImpl = async (path, init) => {
    calls.push({ path, body: JSON.parse(init.body) });
    const body = { agent_id: 'agt_circle', conv_id: 'conv-circle', name: 'circle-scribe-1a2b3c4d', reused: false };
    return { ok: true, status: 200, text: async () => JSON.stringify(body) };
  };
  const asked = []; const toasts = [];
  const options = (answer = true) => ({
    fetchImpl, confirm: async (request) => { asked.push(request); return answer; }, toast: (message, error) => toasts.push({ message, error: !!error }),
  });
  return { summonTemplateScribe, opened, calls, asked, toasts, options };
}

for (const [label, request, brief, what] of [
  ['the library button', { scope: 'library' }, /editing this daemon’s tclaude group templates/, /edit your circles/],
  ['the editor button on a named circle', { scope: 'template', name: 'release-crew' }, /named "release-crew"/, /edit the circle "release-crew"/],
  ['the editor button on a new circle', { scope: 'template' }, /creating a new tclaude group template/, /create a new circle/],
]) {
  test(`${label} opens the circle scribe in its chat window after the grant is confirmed`, async (t) => {
    const { summonTemplateScribe, opened, calls, asked, toasts, options } = await setup(t);
    const result = await summonTemplateScribe(request, options());
    assert.equal(asked.length, 1);
    assert.equal(asked[0].title, 'Grant circle-scribe access?');
    assert.match(asked[0].body, what);
    assert.match(asked[0].body, /templates\.manage/);
    assert.deepEqual(calls.map((call) => call.path), ['/api/scribe']);
    assert.equal(calls[0].body.name, 'circle-scribe');
    assert.deepEqual(calls[0].body.slugs, ['templates.manage']);
    assert.match(calls[0].body.brief, brief);
    assert.equal(calls[0].body.background, true, 'no terminal of its own');
    assert.deepEqual(opened, [{ agentId: 'agt_circle', starting: true }], 'a regular chat window');
    assert.equal(result.agentId, 'agt_circle');
    assert.match(toasts.at(-1).message, /summoned scribe circle-scribe-1a2b3c4d/);
  });
}

test('a declined grant starts nothing', async (t) => {
  const { summonTemplateScribe, opened, calls, options } = await setup(t);
  assert.equal(await summonTemplateScribe({ scope: 'library' }, options(false)), null);
  assert.deepEqual(calls, []);
  assert.deepEqual(opened, []);
});
