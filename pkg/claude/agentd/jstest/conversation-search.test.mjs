import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function waitFor(harness, ready) {
  for (let turn = 0; turn < 50; turn += 1) {
    if (ready()) return;
    await harness.act(() => new Promise((resolve) => { setTimeout(resolve, 5); }));
  }
  assert.fail('condition never held');
}

async function mount(t, search) {
  const harness = await createPreactHarness(t);
  const { ConversationSearchPanel } = await harness.importDashboardModule('js/conversation-search.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.mount(harness.html`<${ConversationSearchPanel} search=${search} debounceMs=${5} />`, host);
  const type = async (value) => {
    const input = host.querySelector('#conversation-search-input');
    input.value = value;
    await harness.act(() => harness.fireEvent(input, 'input'));
  };
  return { harness, host, type };
}

const found = (rows, extra = {}) => ({ results: rows, scanned: rows.length, total: rows.length, truncated: false, ...extra });
const row = (conv_id, extra = {}) => ({ conv_id, title: conv_id, online: false, matches: [{ line: 1, text: `match in ${conv_id}` }], ...extra });

test('short input sends nothing, and a settled query sends one request', async (t) => {
  const calls = [];
  const { harness, host, type } = await mount(t, async (query) => { calls.push(query); return found([row('one')]); });
  await type('ab');
  await harness.act(() => new Promise((resolve) => { setTimeout(resolve, 20); }));
  assert.deepEqual(calls, [], 'below the minimum nothing reaches the daemon');
  await type('abc');
  await type('abcd');
  await waitFor(harness, () => host.querySelector('[data-conv-result="one"]'));
  assert.deepEqual(calls, ['abcd'], 'typing inside the debounce window coalesces');
  assert.match(host.querySelector('#conversation-search-status').textContent, /1 conversation found, 1 searched/);
});

test('a newer query aborts the older request and its late answer is dropped', async (t) => {
  const signals = [];
  const answers = [];
  const { harness, host, type } = await mount(t, (query, signal) => {
    signals.push(signal);
    return new Promise((resolve) => { answers.push(() => resolve(found([row(query)]))); });
  });
  await type('first');
  await waitFor(harness, () => signals.length === 1);
  await type('second');
  await waitFor(harness, () => signals.length === 2);
  assert.equal(signals[0].aborted, true);
  await harness.act(async () => { answers[1](); answers[0](); });
  await waitFor(harness, () => host.querySelector('[data-conv-result="second"]'));
  assert.equal(host.querySelector('[data-conv-result="first"]'), null);
});

test('results offer the action that fits, and a partial scan says so', async (t) => {
  const rows = [row('plain'), row('gone', { agent_id: 'a1', retired: true }), row('live', { agent_id: 'a2' })];
  const { harness, host, type } = await mount(t, async () => found(rows, { scanned: 40, total: 900, truncated: true }));
  await type('needle');
  await waitFor(harness, () => host.querySelector('[data-conv-result="live"]'));
  const action = (id) => host.querySelector(`[data-conv-result="${id}"] button`);
  assert.equal(action('plain').getAttribute('data-act'), 'promote-agent');
  assert.equal(action('gone').getAttribute('data-act'), 'promote-agent');
  assert.equal(action('gone').textContent, 'reinstate');
  assert.equal(action('live').getAttribute('data-act'), 'copy-generation-id');
  assert.match(host.querySelector('#conversation-search-status').textContent, /newest 40 of 900; older ones were not searched/);
});

test('a failed search shows its reason', async (t) => {
  const { harness, host, type } = await mount(t, async () => { throw new Error('another conversation search is running'); });
  await type('needle');
  await waitFor(harness, () => /another conversation search/.test(host.querySelector('#conversation-search-status').textContent));
});
