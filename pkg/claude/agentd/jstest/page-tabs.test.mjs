// Tests for live pages as Collab tabs (tcl-r4sg0c): a page an agent shows opens
// from the snapshot as an unfocused strip tab with a dot and a toast, its view
// is the header plus the sandboxed frame, it closes through the daemon (which
// tells the agent), it follows the live socket's reload and reachability
// signals, and it moves to an overlay tab without closing. The shell is the
// real mounted feature; xterm, the daemon and the socket are faked.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));

const TOKEN = '0123456789abcdef0123456789abcdef';
const ORIGIN = `http://f-${TOKEN}.ui.localhost:7777`;
const PAGE = Object.freeze({
  id: 'pg_00112233445566aa', name: 'mock', title: 'Checkout <b>mock</b>', owner: 'agt_000000000000000000000000000000aa',
  target: ':5173', kind: 'port', worktree: '/home/op/git/shop-feat', state: 'open', origin: ORIGIN, url: `${ORIGIN}/checkout`, rev: 1,
});
const GROUPS = [{ name: 'shop', members: [{ agent_id: PAGE.owner, conv_id: 'conv-ui', title: 'ui-worker', online: true, harness: 'claude' }] }];
const snap = (pages, extra = {}) => ({ agents: [], groups: GROUPS, live_pages: pages, ...extra });

function installHosts(harness) {
  const doc = harness.document;
  const nav = doc.body.appendChild(doc.createElement('nav'));
  const main = doc.body.appendChild(doc.createElement('main'));
  for (const name of ['groups', 'terminals']) {
    const button = nav.appendChild(doc.createElement('a'));
    button.dataset.tab = name;
    main.appendChild(doc.createElement('section')).id = `tab-${name}`;
  }
  const host = doc.getElementById('tab-terminals').appendChild(doc.createElement('div'));
  host.id = 'terminals-root';
  const badgeHost = nav.appendChild(doc.createElement('span'));
  badgeHost.id = 'terminals-badge-root';
  return { host, badgeHost };
}

function fakeDaemon(t) {
  const requests = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    requests.push(`${init.method || 'GET'} ${String(url)}`);
    return { ok: true, status: 200, json: async () => ({ closed: true, told_agent: true }) };
  };
  t.after(() => { globalThis.fetch = previous; });
  return requests;
}

// The shell's own fetch, which a tab's × goes through, records into the same list.
const shellFetch = (requests) => async (url, init = {}) => {
  requests.push(`${init.method || 'GET'} ${String(url)}`);
  return { ok: true, status: 200, json: async () => ({}) };
};

// A WebSocket the test drives: each instance records its URL, and say() hands
// it a message.
function fakeSockets(t, harness) {
  const sockets = [];
  class FakeSocket {
    constructor(url) { this.url = url; this.closed = false; sockets.push(this); }
    close() { this.closed = true; }
    say(message) { this.onmessage?.({ data: JSON.stringify(message) }); }
  }
  const previous = globalThis.WebSocket;
  globalThis.WebSocket = FakeSocket;
  harness.window.location = { protocol: 'http:', host: '127.0.0.1:7777', pathname: '/terminals', search: '', hash: '' };
  t.after(() => { globalThis.WebSocket = previous; });
  return sockets;
}

async function mountShell(t, first = snap([]), requests = []) {
  const harness = await createPreactHarness(t);
  const hosts = installHosts(harness);
  const sockets = fakeSockets(t, harness);
  const factory = (options) => {
    options.host.append(harness.document.createElement('textarea'));
    return { options, connect: () => Promise.resolve(true), copy: () => Promise.resolve(), fit() {}, focus() {}, setActive() {}, status: () => 'connected', dispose() {} };
  };
  const [{ mountTerminalsFeature }, controller, place, instances, { dashboardState }, { terminalShellState }, { shellState }, closeNav] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
    harness.importDashboardModule('js/page-place.js'),
    harness.importDashboardModule('js/overlay-instances.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/shell-state.js'),
    harness.importDashboardModule('js/terminal-close-nav.js'),
  ]);
  dashboardState.publishLocalEdit(first);
  closeNav.setStayOnTerminalsAfterLastClose(false);
  t.after(() => { instances.overlayWindows.value = []; });
  const cleanup = await mountTerminalsFeature({ widgetFactory: factory, fetchImpl: shellFetch(requests) });
  await harness.act(() => wait());
  let done = false;
  const stop = () => { if (!done) { done = true; cleanup(); } };
  t.after(stop);
  const setSnapshot = async (value) => { await harness.act(async () => { dashboardState.publishLocalEdit(value); await wait(); }); await harness.act(() => wait()); };
  return { harness, stop, ...hosts, sockets, controller, place, instances, state: terminalShellState, shellState, setSnapshot };
}

const pageTab = (host) => host.querySelector(`.mux-tab[data-pane-key="page:${PAGE.id}"]`);

test('the page model keeps the token in the frame src and nowhere else', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/page-pane-model.js');
  const { normalizeSeed } = await harness.importDashboardModule('js/terminals-core.js');
  assert.equal(model.isPageId(PAGE.id), true);
  assert.equal(model.isPageId('pg_../x'), false);
  assert.deepEqual(normalizeSeed({ page: PAGE.id, label: 'x', url: PAGE.url, origin: ORIGIN }), { page: PAGE.id, label: 'x', key: `page:${PAGE.id}` },
    'a seed never carries the page URL');
  assert.equal(normalizeSeed({ page: PAGE.id, drawingBoard: 'b' }), null, 'a mixed seed is refused');
  assert.equal(normalizeSeed({ page: 'nope' }), null);
  assert.equal(model.pageFrameSrc(PAGE), PAGE.url);
  assert.equal(model.pageFrameSrc({ ...PAGE, origin: 'http://127.0.0.1:7777', url: 'http://127.0.0.1:7777/api/snapshot' }), '', 'the dashboard origin never loads in the frame');
  assert.equal(model.pageFrameSrc({ ...PAGE, url: `http://evil.example/${TOKEN}` }), '', 'a URL off the page origin loads nothing');
  // A dashboard on another machine gets a path on its own host (tcl-balzk0).
  const remote = { ...PAGE, origin: '', url: `/p/${TOKEN}/checkout?x=1` };
  assert.equal(model.pageFrameSrc(remote), remote.url);
  assert.equal(model.pageFrameSrc({ ...remote, url: `/p/${TOKEN}` }), `/p/${TOKEN}`);
  for (const url of ['/api/snapshot', `//evil.example/p/${TOKEN}/`, `https://evil.example/p/${TOKEN}/`, '/p/short/', `/p/${TOKEN}x/`, 'javascript:alert(1)', `/p/${TOKEN}/../../api/snapshot`, `/p/${TOKEN}/%2e%2e/%2e%2e/api/snapshot`, `/p/${TOKEN}/.%2E/x`]) {
    assert.equal(model.pageFrameSrc({ ...remote, url }), '', `${url} loads nothing`);
  }
  assert.match(model.pageSandboxRules(remote)[0], /\/p\/‹token›\//, 'the chip names the path, its token masked');
  assert.doesNotMatch(model.pageSandboxRules(remote).join(' '), new RegExp(TOKEN));
  assert.match(model.pageSandboxRules({ ...remote, group: 'tclaude' }).join(' '), /Shared in group tclaude: .*every member/,
    'a shared page says who can read what the operator types');
  assert.match(model.pageSandboxRules(remote).join(' '), /The agent that showed this page can read/);
  assert.equal(model.PAGE_FRAME_SANDBOX, 'allow-scripts allow-forms allow-modals');

  const first = model.pageShowEvents(null, [PAGE]);
  assert.deepEqual([first.added, first.reshown, first.initial.map((p) => p.id)], [[], [], [PAGE.id]], 'the first snapshot is no news');
  const again = model.pageShowEvents(first.next, [{ ...PAGE, rev: 2 }, { ...PAGE, id: 'pg_00112233445566bb', rev: 3 }]);
  assert.deepEqual([again.added.map((p) => p.id), again.reshown.map((p) => p.id), again.gone], [['pg_00112233445566bb'], [PAGE.id], []]);
  assert.deepEqual(model.pageShowEvents(again.next, []).gone, [PAGE.id, 'pg_00112233445566bb']);
});

test('a shown page opens unfocused with a dot and a toast, and its view is the header and the sandboxed frame', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, host, state, shellState } = env;
  await harness.act(() => env.controller.openWebWindowPane('agt_000000000000000000000000000000cc', 'other'));
  await harness.act(() => wait());
  const activeBefore = state.activeKey.peek();

  await env.setSnapshot(snap([PAGE]));
  const tab = pageTab(host);
  assert.ok(tab, 'the page opened as a strip tab');
  assert.equal(state.activeKey.peek(), activeBefore, 'unfocused: the tab showing stays');
  assert.equal(tab.dataset.agentStatus, 'page');
  assert.ok(tab.querySelector('.mux-tab-unseen'), 'marked as not seen yet');
  assert.equal(tab.querySelector('.mux-tab-label').textContent, PAGE.title, 'the title is the tab label, as text');
  const toast = shellState.toast.peek();
  assert.equal(toast.message, `ui-worker showed ${PAGE.title}`);
  assert.equal(toast.action.label, 'Open');

  await harness.act(async () => { toast.action.run(); await wait(); });
  assert.equal(state.activeKey.peek(), `page:${PAGE.id}`, 'Open shows the page');
  assert.equal(pageTab(host).querySelector('.mux-tab-unseen'), null, 'and it is seen');

  const pane = host.querySelector(`[data-page-pane="${PAGE.id}"]`);
  const frame = pane.querySelector('iframe.page-frame');
  assert.equal(frame.getAttribute('src'), PAGE.url);
  assert.equal(frame.getAttribute('sandbox') ?? frame.sandbox?.value ?? frame.sandbox, 'allow-scripts allow-forms allow-modals');
  assert.equal(frame.getAttribute('allow') ?? frame.allow, '');
  assert.equal(frame.getAttribute('referrerpolicy'), 'no-referrer');
  assert.ok(frame.hasAttribute('credentialless') || frame.credentialless === true, 'credentialless');
  const head = pane.querySelector('.page-head');
  assert.equal(head.querySelector('.page-head-title').textContent, PAGE.title);
  assert.equal(head.querySelector('b'), null, 'nothing the agent wrote renders as HTML');
  assert.equal(head.querySelector('.page-head-sender').textContent, 'ui-worker');
  assert.equal(head.querySelector('.page-head-target').textContent, ':5173');
  assert.equal(head.querySelector('.page-head-worktree').textContent, 'shop-feat');
  const chip = head.querySelector('.page-sandbox-chip');
  assert.match(chip.getAttribute('title'), /own origin: http:\/\/f-‹token›\.ui\.localhost:7777/, 'the origin, its token masked');
  assert.match(chip.getAttribute('title'), /The agent that showed this page can read what you type in it\./);
  const labels = [...head.querySelectorAll('button')].map((b) => b.textContent);
  assert.deepEqual(labels.filter((l) => ['Reload', 'Open in overlay', 'Pop out', 'Close'].includes(l)), ['Reload', 'Open in overlay', 'Pop out', 'Close']);
  // The lock chip is a .ui-tag that toggles (data-ui-exempt, resource-lock.js).
  for (const button of head.querySelectorAll('button:not([data-ui-exempt])')) assert.ok(button.classList.contains('ui-btn'), `${button.textContent} is a ui-btn`);
  assert.equal(head.querySelector('[data-resource-lock]').textContent, '✎ agents can edit', 'the page wears its agent lock');
  assert.equal(host.innerHTML.split(TOKEN).length, 2, 'the token is in the frame src only');

  await harness.act(async () => { harness.fireEvent([...head.querySelectorAll('button')].find((b) => b.textContent === 'Close'), 'click'); await wait(); });
  assert.ok(requests.includes(`DELETE /api/pages/${PAGE.id}`), 'Close asks the daemon, which tells the agent');
  assert.equal(pageTab(host), null);
  env.stop();
});

test('the live socket reloads the frame, and an unreachable source offers Retry', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, host, sockets } = env;
  await env.setSnapshot(snap([PAGE]));
  await harness.act(async () => { env.place.revealPage(PAGE.id, PAGE.title); await wait(); });
  const live = sockets.filter((s) => s.url === `ws://127.0.0.1:7777/api/pages/${PAGE.id}/live`);
  assert.equal(live.length, 1, 'one live socket for the view');
  const frameOf = () => host.querySelector(`[data-page-pane="${PAGE.id}"] iframe`);
  const before = frameOf();
  await harness.act(async () => { live[0].say({ type: 'reload' }); await wait(); });
  assert.notEqual(frameOf(), before, 'a reload signal loads the frame again');

  await harness.act(async () => { live[0].say({ type: 'unreachable' }); await wait(); });
  const down = host.querySelector('.page-unreachable');
  assert.ok(down, 'an unreachable source says so');
  await harness.act(async () => { harness.fireEvent(down.querySelector('button'), 'click'); await wait(); });
  assert.ok(live[0].closed, 'Retry drops the socket');
  const again = sockets.filter((s) => s.url === live[0].url && !s.closed);
  assert.equal(again.length, 1, 'and asks again on a new one');
  await harness.act(async () => { again[0].say({ type: 'reachable' }); await wait(); });
  assert.equal(host.querySelector('.page-unreachable'), null);
  env.stop();
});

test('a page leaves when the snapshot drops it, floats without closing, and a tab × closes it', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t, snap([]), requests);
  const { harness, host, instances } = env;
  await env.setSnapshot(snap([PAGE]));
  assert.ok(pageTab(host));
  await env.setSnapshot(snap([]));
  assert.equal(pageTab(host), null, 'a page closed elsewhere leaves its tab');
  assert.deepEqual(requests.filter((r) => r.startsWith('DELETE')), []);

  await env.setSnapshot(snap([PAGE]));
  await harness.act(async () => { harness.fireEvent(pageTab(host), 'click'); await wait(); });
  const float = [...host.querySelectorAll(`[data-page-pane="${PAGE.id}"] .page-head button`)].find((b) => b.textContent === 'Open in overlay');
  await harness.act(async () => { harness.fireEvent(float, 'click'); await wait(); });
  assert.equal(pageTab(host), null, 'the strip tab left');
  assert.deepEqual(instances.overlayTabs.peek().filter((tab) => tab.kind === 'page').map((tab) => tab.page), [PAGE.id], 'for an overlay tab');
  assert.deepEqual(requests.filter((r) => r.startsWith('DELETE')), [], 'moving is not closing');

  await harness.act(async () => { await env.place.returnPageToTab(PAGE.id); await wait(); });
  assert.ok(pageTab(host), 'back to a strip tab');
  assert.equal(instances.overlayTabs.peek().filter((tab) => tab.kind === 'page').length, 0);
  await harness.act(async () => { harness.fireEvent(pageTab(host).querySelector('.mux-tab-close'), 'click'); await wait(); });
  assert.ok(requests.includes(`DELETE /api/pages/${PAGE.id}`), 'the tab × closes the page');
  env.stop();
});

test('pages already open when the dashboard loads open quietly', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, snap([PAGE]));
  const { harness, host, shellState } = env;
  await harness.act(() => wait());
  assert.ok(pageTab(host), 'a page with no place opens');
  assert.equal(pageTab(host).querySelector('.mux-tab-unseen'), null, 'no news: no dot');
  assert.notEqual(shellState.toast.peek().action?.label, 'Open', 'and no toast');
  env.stop();
});

test('a close that fails gives the page its tab back, and a restored overlay tab of a closed page goes', async (t) => {
  const requests = [];
  const env = await mountShell(t, snap([]), requests);
  const { harness, host, instances } = env;
  const failing = async (url, init = {}) => { requests.push(`${init.method || 'GET'} ${String(url)}`); return { ok: false, status: 500, json: async () => ({}) }; };
  await env.setSnapshot(snap([PAGE]));
  await harness.act(async () => { await env.place.closeLivePage(PAGE.id, { fetchImpl: failing }); await wait(); });
  assert.ok(requests.includes(`DELETE /api/pages/${PAGE.id}`));
  env.state.removePanes([`page:${PAGE.id}`]);
  await env.setSnapshot(snap([{ ...PAGE }]));
  assert.ok(pageTab(host), 'the page is still open, so it has a tab again');

  await harness.act(async () => { env.place.openPageOverlay('pg_00112233445566cc', 'gone page'); await wait(); });
  await env.setSnapshot(snap([PAGE]));
  assert.deepEqual(instances.overlayTabs.peek().filter((tab) => tab.kind === 'page').map((tab) => tab.page), [], 'a page the snapshot does not list leaves its overlay tab');
  env.stop();
});

// tcl-20ihu3: a page the operator creates from a group's ＋▾ opens focused
// once the snapshot lists it, with no "showed" toast and no unseen dot; the
// group's name stands where an agent's would.
test('a page this dashboard created opens focused, unannounced, and names its group', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, host, state, shellState } = env;
  const page = { ...PAGE, id: 'pg_00112233445566bb', name: 'sketch', title: 'sketch', owner: '', group_owned: true, group: 'shop', kind: 'scratch', target: 'scratch' };
  env.place.expectPage(page.id);
  const toastBefore = shellState.toast.peek();
  await env.setSnapshot(snap([]));
  const tabOf = () => host.querySelector(`.mux-tab[data-pane-key="page:${page.id}"]`);
  assert.equal(tabOf(), null, 'an older snapshot without it opens nothing');
  await env.setSnapshot(snap([page]));
  const tab = tabOf();
  assert.ok(tab, 'it opens when the snapshot lists it');
  assert.equal(state.activeKey.peek(), `page:${page.id}`, 'focused');
  assert.equal(tab.querySelector('.mux-tab-unseen'), null, 'no unseen dot');
  assert.equal(shellState.toast.peek(), toastBefore, 'no "showed" toast');
  await harness.act(() => wait());
  assert.match(host.querySelector(`[data-page-pane="${page.id}"] .page-head-from`).textContent, /from group shop/);
  env.stop();
});
