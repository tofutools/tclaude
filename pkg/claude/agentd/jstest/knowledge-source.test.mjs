// Tests for the source download's question and progress: what the operator is
// told, that each answer is sent once, and that a failure shows.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const VIEW = {
  state: 'unavailable', ask: true, policy: 'ask', version: 'v1.2.3', origin: 'github.com/tofutools/tclaude',
  download_bytes: 18 << 20, stored_bytes: 28 << 20, detail: 'not downloaded',
};

async function setup(t, view, { collab = true, reply = VIEW, polled = null } = {}) {
  const harness = await createPreactHarness(t);
  const { KnowledgeSourceGate } = await harness.importDashboardModule('js/knowledge-source-island.js');
  const snap = harness.signals.signal({ collab_enabled: collab, knowledge_source: view });
  const posts = [];
  const timers = [];
  const fetchImpl = async (url, init) => {
    if (init?.method === 'POST') {
      posts.push(JSON.parse(init.body));
      return { ok: true, json: async () => reply };
    }
    return { ok: true, json: async () => polled };
  };
  const mounted = await harness.mount(harness.html`<${KnowledgeSourceGate} snapshot=${snap} fetchImpl=${fetchImpl}
    setTimer=${(fn) => { timers.push(fn); return timers.length; }} clearTimer=${() => {}} />`);
  return { harness, host: mounted.container, posts, timers, snap };
}

test('the question says what, how big and from where, and sends the answer', async (t) => {
  const { harness, host, posts } = await setup(t, VIEW, { reply: { ...VIEW, ask: false, state: 'fetching', received: 9 << 20 } });
  const card = host.querySelector('.knowledge-source-card');
  assert.match(card.textContent, /tclaude's source/);
  assert.match(card.textContent, /About 18 MiB from github\.com\/tofutools\/tclaude, about 28 MiB on disk/);
  assert.match(card.textContent, /Config/);
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Yes, every new version' }).click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(posts, [{ action: 'each_version' }]);
  assert.equal(Number(host.querySelector('progress').getAttribute('value')), 9 << 20, 'the bar takes over from the question');
  assert.match(host.textContent, /9 MiB of about 18 MiB/);
});

test('a running download follows its own poll and a failure stays until dismissed', async (t) => {
  const failed = { ...VIEW, ask: false, state: 'failed', detail: 'abc is not on GitHub' };
  const { harness, host, posts, timers } = await setup(t, { ...VIEW, ask: false, state: 'fetching', received: 1 << 20 }, { polled: failed });
  assert.ok(host.querySelector('progress'));
  assert.equal(timers.length, 1, 'polls only while downloading');
  await harness.act(async () => { await timers[0](); });
  const alert = harness.getByRole(host, 'alert');
  assert.match(alert.textContent, /abc is not on GitHub/);
  await harness.act(async () => { harness.getByRole(host, 'button', { name: 'Try again' }).click(); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(posts, [{ action: 'once' }]);
});

test('nothing shows once answered', async (t) => {
  const { host } = await setup(t, { ...VIEW, ask: false });
  assert.equal(host.querySelector('.knowledge-source-card'), null);
});

test('nothing shows with Collab off', async (t) => {
  const { host } = await setup(t, VIEW, { collab: false });
  assert.equal(host.querySelector('.knowledge-source-card'), null);
});
