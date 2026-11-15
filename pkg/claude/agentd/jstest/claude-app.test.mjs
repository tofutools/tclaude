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

function fakeWorld({ statuses, blocked = false, onFails = false }) {
  const calls = [];
  const toasts = [];
  const opened = [];
  const tab = { closed: false, location: { href: '' }, document: { title: '', body: { textContent: '' } }, close() { this.closed = true; } };
  const win = { open: (...args) => { opened.push(args); return blocked ? null : tab; } };
  let i = 0;
  const fetchImpl = async (url, init) => {
    const { intent } = JSON.parse(init.body);
    calls.push(intent);
    if (intent === 'on' && onFails) return { ok: false, status: 503, text: async () => 'no tmux', json: async () => ({}) };
    const body = intent === 'status' ? statuses[Math.min(i++, statuses.length - 1)] : { action: 'enabled' };
    return { ok: true, status: 200, text: async () => '', json: async () => body };
  };
  let clock = 0;
  return {
    calls, toasts, opened, tab,
    opts: {
      conv: 'agt_1', label: 'cc', win, fetchImpl,
      notify: (...args) => toasts.push(args),
      sleep: async (ms) => { clock += ms; }, now: () => clock,
    },
  };
}

test('turn on & open: the tab opens in the tap, then follows the link once it shows', async () => {
  const w = fakeWorld({ statuses: [{}, {}, { session_url: LINK }] });
  const pending = openInClaudeApp({ ...w.opts, turnOn: true });
  assert.equal(w.opened.length, 1, 'the tab is opened before anything is awaited');
  const result = await pending;
  assert.deepEqual(result, { ok: true, url: LINK });
  assert.deepEqual(w.calls, ['on', 'status', 'status', 'status']);
  assert.equal(w.tab.location.href, LINK);
  assert.match(w.toasts[0][0], /Turning on Remote Access for cc/);
});

test('no link within the wait: the tab closes and a toast says so', async () => {
  const w = fakeWorld({ statuses: [{}] });
  const result = await openInClaudeApp({ ...w.opts, turnOn: true, timeoutMs: 3000, intervalMs: 1000 });
  assert.equal(result.ok, false);
  assert.equal(w.tab.closed, true);
  const [message, error] = w.toasts.at(-1);
  assert.equal(error, true);
  assert.match(message, /Remote Access did not come up for cc within 3 s/);
  assert.ok(w.calls.filter((c) => c === 'status').length <= 4, 'it stops at the deadline');

  const failing = fakeWorld({ statuses: [{}], onFails: true });
  await openInClaudeApp({ ...failing.opts, turnOn: true });
  assert.match(failing.toasts.at(-1)[0], /did not come up for cc: no tmux/);
  assert.deepEqual(failing.calls, ['on']);
});

test('a blocked tab: a second tap from the toast opens the link', async () => {
  const w = fakeWorld({ statuses: [{ session_url: LINK }], blocked: true });
  const result = await openInClaudeApp({ ...w.opts });
  assert.deepEqual(result, { ok: true, url: LINK, deferred: true });
  assert.deepEqual(w.calls, ['status'], 'Remote Access already on: no switch');
  const [, , options] = w.toasts.at(-1);
  assert.equal(options.action.label, 'Open in Claude app ↗');
  options.action.run();
  assert.deepEqual(w.opened.at(-1), [LINK, '_blank', 'noopener']);
});
