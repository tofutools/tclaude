// Message steps in the dashboard's editors (tk_9f7b4513): a task that tells
// someone and goes on. Pure layers only: the step list model, the inline
// editor's model and the run log's sentences.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  messageRecipientOptions, messageRecipientWords, newStep, setStepRecipients, setStepSubject, setStepText, setStepWho,
  stepListParams, stepText, stepWho,
} from '../dashboard/js/process-step-list.js';
import { changeKind, miniKind, newStepNode } from '../dashboard/js/process-mini-edit.js';
import { whoNow, whoOptions } from '../dashboard/js/process-quickstart.js';
import { createPreactHarness } from './preact-harness.mjs';

const empty = { steps: [], ends: [], template: {} };

test('a new message step tells you, and its text reads start inputs', () => {
  const step = newStep(empty, { kind: 'message', text: 'Ready: {{what}}' });
  assert.equal(step.node.type, 'task');
  assert.deepEqual(step.node.performer, { kind: 'message', to: ['operator'], text: 'Ready: {{params.what}}' });
  assert.deepEqual(stepWho(step), { mode: 'message', to: ['operator'] });
  assert.equal(stepText(step), 'Ready: {{params.what}}');
  const subject = setStepSubject(step, 'About {{who}}\nignored line');
  assert.equal(subject.node.performer.subject, 'About {{params.who}} ignored line', 'a subject is one line');
  assert.deepEqual(stepListParams({ steps: [subject] }), ['who', 'what'], 'subject and text both declare params');
  assert.equal(setStepSubject(subject, '  ').node.performer.subject, undefined, 'an empty subject is the default');
});

test('recipients: each at most once, at most eight, and in words', () => {
  const step = newStep(empty, { kind: 'message' });
  const many = setStepRecipients(step, ['operator', 'lead', 'lead', ' role:reviewer ', ...Array.from({ length: 9 }, (_, i) => `agent:a${i}`)]);
  assert.equal(many.node.performer.to.length, 8);
  assert.deepEqual(many.node.performer.to.slice(0, 3), ['operator', 'lead', 'role:reviewer']);
  assert.equal(setStepRecipients(step, []).node.performer.to, undefined);
  assert.deepEqual(['operator', 'lead', 'group', 'role:reviewer', 'agent:rita'].map(messageRecipientWords),
    ['you', 'the lead', 'the whole group', 'reviewer', 'rita']);
  const options = messageRecipientOptions([{ name: 'rita', role: 'reviewer' }, { name: 'ivan', role: 'implementor' }]);
  assert.deepEqual(options.map((o) => o.value),
    ['operator', 'lead', 'group', 'role:implementor', 'role:reviewer', 'agent:rita', 'agent:ivan']);
});

test('an agent step becomes a message and back, keeping its text', () => {
  const agent = { ...newStep(empty, { kind: 'agent', text: 'Build it' }) };
  agent.node = { ...agent.node, captures: ['pr'] };
  const message = setStepWho(agent, { mode: 'message' });
  assert.equal(message.node.captures, undefined, 'a message reports no outputs');
  assert.deepEqual(message.node.performer, { kind: 'message', to: ['operator'], text: 'Build it' });
  assert.equal(setStepText(message, 'Built').node.performer.text, 'Built');
  const back = setStepWho(message, { mode: 'role', role: 'reviewer' });
  assert.equal(back.node.performer.kind, 'agent');
  assert.equal(back.node.performer.to, undefined);
});

test('the Who picker offers a message on a task only, and says who it tells', () => {
  assert.ok(whoOptions([], { message: true }).some((o) => o.value === 'message'));
  assert.ok(!whoOptions([]).some((o) => o.value === 'message'));
  const step = setStepRecipients(newStep(empty, { kind: 'message' }), ['operator', 'role:reviewer']);
  assert.deepEqual(whoNow(step, []), { label: 'a message', detail: 'to you, reviewer' });
});

test('the inline editor: a message is its own kind, a decision never becomes one', () => {
  const node = newStepNode('message');
  assert.equal(miniKind(node), 'message');
  const view = {
    template: { start: 'build', nodes: {
      build: { type: 'task', name: 'Build', captures: ['pr'], performer: { kind: 'agent', prompt: 'x' } },
      choose: { type: 'decision', name: 'Choose', performer: { kind: 'human', ask: 'ok?', choices: ['yes', 'no'] } },
      end: { type: 'end' },
    } },
    edges: [{ from: '', outcome: 'start', to: 'build' }, { from: 'build', outcome: 'next', to: 'choose' },
      { from: 'choose', outcome: 'yes', to: 'end' }, { from: 'choose', outcome: 'no', to: 'end' }],
  };
  const changed = changeKind(view, 'build', 'message').view.template.nodes.build;
  assert.equal(changed.performer.kind, 'message');
  assert.deepEqual(changed.performer.to, ['operator']);
  assert.equal(changed.captures, undefined, 'a message reports no outputs');
  assert.throws(() => changeKind(view, 'choose', 'message'), /decision cannot become a message/);
  view.template.nodes.build.review = { performer: { kind: 'agent', prompt: 'look' } };
  assert.throws(() => changeKind(view, 'build', 'message'), /plan, checks or review/);
});

test('the run log says what a message sent, and to whom', async (t) => {
  const harness = await createPreactHarness(t);
  const { eventSentence } = await harness.importDashboardModule('js/process-oversight.js');
  const sent = eventSentence({ kind: 'message_sent', nodeId: 'tell', payload: {
    observation: { outcome: 'succeeded' },
    message: { subject: 'Ready: login', text: 'x', delivered: [{ to: 'operator', key: 'operator' }, { to: 'role:reviewer', key: 'conv-1', name: 'rita' }] },
  } });
  assert.equal(sent.text, 'tell sent “Ready: login” to you, rita');
  const failed = eventSentence({ kind: 'message_sent', nodeId: 'tell', payload: JSON.stringify({
    observation: { outcome: 'failed', error: 'no member of crew has role designer' },
    message: { delivered: [{ to: 'operator', key: 'operator' }] },
  }) });
  assert.equal(failed.tone, 'concern');
  assert.equal(failed.text, 'tell could not be sent: no member of crew has role designer (it reached you)');
});
