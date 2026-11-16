// Tests for "Open in Claude app ↗" (claude-app.js): the offer per agent
// state, and the tap flow that switches Remote Access on and opens the link.

import test from 'node:test';
import assert from 'node:assert/strict';
import { claudeAppOffer, claudeAppURL, openInClaudeApp } from '../dashboard/js/claude-app.js';

const LINK = 'https://claude.ai/code/session_01abc';

test('only the exact claude.ai/code origin is a session link', () => {
  assert.equal(claudeAppURL({ remote_control_url: LINK }), LINK);
  for (const bad of ['https://claude.ai.evil.example/code/x', 'http://claude.ai/code/x', 'https://claude.ai/settings', '', undefined, 7]) {
    assert.equal(claudeAppURL({ remote_control_url: bad }), '', String(bad));
  }
});

test('the offer: open a known link anywhere; get one only where this page may switch it', () => {
  const all = { online: true, canRemote: true, canToggle: true };
  assert.deepEqual(claudeAppOffer({ remote_control_url: LINK }, {}), { kind: 'open', url: LINK, label: 'Open in Claude app ↗' });
  assert.deepEqual(claudeAppOffer({}, all), { kind: 'get', turnOn: true, label: 'Turn on Remote Access & open' });
  assert.deepEqual(claudeAppOffer({ remote_control: true }, all), { kind: 'get', turnOn: false, label: 'Open in Claude app ↗' });
  for (const off of ['online', 'canRemote', 'canToggle']) {
    assert.equal(claudeAppOffer({}, { ...all, [off]: false }), null, off);
  }
});

function fakeWorld({ statuses, onFails = false }) {
  const calls = [];
  const toasts = [];
  const opened = [];
  const win = { open: (...args) => { opened.push(args); return null; } };
  let i = 0;
  const fetchImpl = async (url, init) => {
    const { intent } = JSON.parse(init.body);
    calls.push(intent);
    if (intent === 'on' && onFails) return { ok: false, status: 503, text: async () => 'no tmux', json: async () => ({}) };
    const body = intent === 'link' ? statuses[Math.min(i++, statuses.length - 1)] : { action: 'enabled' };
    return { ok: true, status: 200, text: async () => '', json: async () => body };
  };
  let clock = 0;
  return {
    calls, toasts, opened,
    opts: {
      conv: 'agt_1', label: 'cc', win, fetchImpl,
      notify: (...args) => toasts.push(args),
      sleep: async (ms) => { clock += ms; }, now: () => clock,
    },
  };
}

test('turn on & open: switches it on, waits for the link, and a tap on the toast opens it', async () => {
  const w = fakeWorld({ statuses: [{}, {}, { session_url: LINK }] });
  const result = await openInClaudeApp({ ...w.opts, turnOn: true });
  assert.deepEqual(result, { ok: true, url: LINK });
  assert.deepEqual(w.calls, ['on', 'link', 'link', 'link']);
  assert.match(w.toasts[0][0], /Turning on Remote Access for cc/);
  assert.equal(w.opened.length, 0, 'nothing opens without a tap: a universal link needs one');
  const [message, error, options] = w.toasts.at(-1);
  assert.equal(message, 'Remote Access is on for cc.');
  assert.equal(error, false);
  assert.equal(options.action.label, 'Open in Claude app ↗');
  options.action.run();
  assert.deepEqual(w.opened, [[LINK, '_blank', 'noopener']]);
});

test('on but not read yet: no switch, just the read', async () => {
  const w = fakeWorld({ statuses: [{ session_url: LINK }] });
  assert.deepEqual(await openInClaudeApp({ ...w.opts }), { ok: true, url: LINK });
  assert.deepEqual(w.calls, ['link']);
  assert.match(w.toasts[0][0], /Finding cc's Remote Access link/);
});

test('no link within the wait, or a refused switch: a toast says so', async () => {
  const w = fakeWorld({ statuses: [{}] });
  const result = await openInClaudeApp({ ...w.opts, turnOn: true, timeoutMs: 3000, intervalMs: 1000 });
  assert.equal(result.ok, false);
  const [message, error, options] = w.toasts.at(-1);
  assert.equal(error, true);
  assert.equal(options, undefined);
  assert.match(message, /Remote Access did not come up for cc within 3 s/);
  assert.ok(w.calls.filter((c) => c === 'link').length <= 4, 'it stops at the deadline');

  const failing = fakeWorld({ statuses: [{}], onFails: true });
  await openInClaudeApp({ ...failing.opts, turnOn: true });
  assert.match(failing.toasts.at(-1)[0], /did not come up for cc: no tmux/);
  assert.deepEqual(failing.calls, ['on']);
});
