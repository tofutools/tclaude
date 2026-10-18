// mail-history.test.mjs — Messages' half of
// /messages/<folder>[/<message-id>][?q=&from=&page=&box=]:
// which selections push a history entry, which only correct the current one,
// and how a history location is shown. The router itself is covered by
// nav-history.test.mjs; here the URL is a fake the test moves by hand.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const MESSAGES = [1, 2, 3].map((id) => ({ id, direction: 'in', from_conv: 'conv-a', to_conv: 'human', subject: `S${id}`, body: '', created_at: '2026-07-12T00:00:00Z', read: true }));

async function setup(t) {
  const harness = await createPreactHarness(t);
  harness.document.body.innerHTML = '<nav><button data-tab="messages" class="active"></button></nav><section id="tab-messages" class="active"></section>';
  let pathname = '/messages';
  let search = '';
  const location = { get pathname() { return pathname; }, get search() { return search; } };
  for (const target of [globalThis, harness.window]) Object.defineProperty(target, 'location', { configurable: true, writable: true, value: location });
  const fetches = [];
  const fetchImpl = async (url) => {
    fetches.push(String(url));
    const body = String(url).startsWith('/api/mailboxes')
      ? { mailboxes: [{ id: 'all', kind: 'all', total: 3 }, { id: 'human', kind: 'human', total: 3 }, { id: 'conv-a', kind: 'agent', total: 3 }], agents_total: 1, selected_valid: true }
      : { messages: MESSAGES, total: 300, total_unfiltered: 300, page: Number(new URL(String(url), 'http://x').searchParams.get('page')) || 1 };
    return { ok: true, json: async () => body, text: async () => '' };
  };
  for (const target of [globalThis, harness.window]) Object.defineProperty(target, 'fetch', { configurable: true, writable: true, value: fetchImpl });
  // Stub the two seams mail.js imports so the full dashboard does not boot.
  await harness.replaceDashboardModule('js/dashboard.js', 'export let lastSnapshot = null;');
  await harness.replaceDashboardModule('js/refresh.js', 'export function confirmModal() {}\nexport function toast() {}');
  const { mailController: mail } = await harness.importDashboardModule('js/mail.js');
  const events = [];
  // Stand in for the router: a navigation or correction moves the URL.
  const { toPath, toQuery } = await harness.importDashboardModule('js/nav-history-core.js');
  const onNav = (e) => {
    events.push({ correction: !!e.detail.correction, location: e.detail.location });
    pathname = toPath(e.detail.location);
    const query = toQuery(e.detail.location);
    search = query ? `?${query}` : '';
  };
  harness.document.addEventListener('tclaude:navigated', onNav);
  const dispose = mail.initMail();
  t.after(() => { dispose(); harness.document.removeEventListener('tclaude:navigated', onNav); });
  const setPath = (p) => { [pathname, search = ''] = p.split('?'); if (search) search = `?${search}`; };
  return { mail, events, setPath, path: () => pathname + search, fetches };
}

test('clicking a folder or message pushes; other selection changes only correct the URL', async (t) => {
  const s = await setup(t);
  s.events.length = 0;
  await s.mail.selectMailbox('conv-a', { navigate: true });
  assert.deepEqual(s.events[0], { correction: false, location: { tab: 'messages', selection: 'conv-a' } });
  assert.equal(s.events.length, 1);
  s.mail.selectMessage(2, { navigate: true });
  assert.deepEqual(s.events.at(-1), { correction: false, location: { tab: 'messages', selection: 'conv-a', item: '2' } });
  assert.equal(s.path(), '/messages/conv-a/2');
  // Arrow keys (no navigate flag): the URL follows in place.
  s.events.length = 0;
  s.mail.selectMessage(3);
  assert.deepEqual(s.events, [{ correction: true, location: { tab: 'messages', selection: 'conv-a', item: '3' } }]);
  // A repaint that changes nothing records nothing.
  s.events.length = 0;
  s.mail.state.touch();
  assert.deepEqual(s.events, []);
});

test('a folder switch before the router records the tab click never rewrites the previous tab', async (t) => {
  const s = await setup(t);
  // The Messages tab is already active (bindTabs ran) but the URL is still the
  // tab the operator left: the attention jump switches folder right here.
  s.setPath('/groups');
  s.events.length = 0;
  await s.mail.selectMailbox('human');
  s.mail.selectMessage(2);
  assert.deepEqual(s.events, [], 'the /groups entry is left for the router to push past');
});

test('a history location opens its folder and message without recording a navigation', async (t) => {
  const s = await setup(t);
  s.setPath('/messages/human/2');
  s.events.length = 0;
  await s.mail.restoreLocation({ tab: 'messages', selection: 'human', item: '2' });
  assert.equal(s.mail.state.data.selected, 'human');
  assert.equal(s.mail.state.data.selectedMsgId, 2);
  assert.deepEqual(s.events, []);
  // Back to the folder alone closes the reader.
  s.setPath('/messages/human');
  await s.mail.restoreLocation({ tab: 'messages', selection: 'human' });
  assert.equal(s.mail.state.data.selectedMsgId, null);
  assert.deepEqual(s.events, []);
  // A message that is gone leaves the folder, and the URL says so.
  s.setPath('/messages/conv-a/99');
  await s.mail.restoreLocation({ tab: 'messages', selection: 'conv-a', item: '99' });
  assert.equal(s.mail.state.data.selected, 'conv-a');
  assert.deepEqual(s.events, [{ correction: true, location: { tab: 'messages', selection: 'conv-a' } }]);
  // A bare /messages keeps the open folder and names it.
  s.events.length = 0;
  s.setPath('/messages');
  await s.mail.restoreLocation({ tab: 'messages' });
  assert.deepEqual(s.events, [{ correction: true, location: { tab: 'messages', selection: 'conv-a' } }]);
});

test('a page step pushes; a search edit pushes once, then corrects as it is typed', async (t) => {
  const s = await setup(t);
  await s.mail.selectMailbox('conv-a', { navigate: true });
  s.events.length = 0;
  s.mail.goToPage(2);
  assert.deepEqual(s.events, [{ correction: false, location: { tab: 'messages', selection: 'conv-a', params: { page: '2' } } }]);
  s.events.length = 0;
  s.mail.setMessageQuery('d');
  s.mail.setMessageQuery('de');
  s.mail.setMessageQuery('dep');
  assert.deepEqual(s.events.map((e) => e.correction), [false, true, true]);
  assert.equal(s.path(), '/messages/conv-a?q=dep', 'the search starts over on page 1');
  // The folder filter is another edit: its first keystroke pushes again.
  s.events.length = 0;
  s.mail.setBoxQuery('o');
  s.mail.setBoxQuery('op');
  assert.deepEqual(s.events.map((e) => e.correction), [false, true]);
  assert.equal(s.path(), '/messages/conv-a?q=dep&box=op');
  // A click ends the edit: the next keystroke pushes once more.
  s.mail.selectMessage(2, { navigate: true });
  s.events.length = 0;
  s.mail.setMessageQuery('depl');
  assert.equal(s.events[0].correction, false);
});

test('a history location restores its search, sender, page and folder filter', async (t) => {
  const s = await setup(t);
  s.setPath('/messages/human/2?q=boom&from=agt_x&page=3&box=ops');
  s.fetches.length = 0;
  s.events.length = 0;
  await s.mail.restoreLocation({ tab: 'messages', selection: 'human', item: '2', params: { q: 'boom', from: 'agt_x', page: '3', box: 'ops' } });
  const data = s.mail.state.data;
  assert.equal(data.messageQuery, 'boom');
  assert.equal(data.senderFilter, 'agt_x');
  assert.equal(data.page, 3);
  assert.equal(data.boxQuery, 'ops');
  assert.equal(data.selectedMsgId, 2);
  const fetched = new URL(s.fetches.find((u) => u.startsWith('/api/mailbox?')), 'http://x').searchParams;
  assert.equal(fetched.get('q'), 'boom');
  assert.equal(fetched.get('sender'), 'agt_x');
  assert.equal(fetched.get('page'), '3');
  assert.equal(new URL(s.fetches.find((u) => u.startsWith('/api/mailboxes')), 'http://x').searchParams.get('q'), 'ops');
  assert.deepEqual(s.events, []);
  // Back to the same folder without them clears all four.
  s.setPath('/messages/human');
  await s.mail.restoreLocation({ tab: 'messages', selection: 'human' });
  assert.equal(data.messageQuery, '');
  assert.equal(data.senderFilter, '');
  assert.equal(data.page, 1);
  assert.equal(data.boxQuery, '');
  assert.deepEqual(s.events, []);
});
