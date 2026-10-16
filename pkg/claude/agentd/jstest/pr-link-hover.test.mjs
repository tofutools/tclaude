// Tests for the PR link's hover card: hovering the #1234 link fetches
// /api/pr-details, re-polls only while the daemon reports a read in flight,
// and renders the title, state, branches, diffstat and description excerpt.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const PR = 'https://github.com/tofutools/tclaude/pull/3090';

test('diff blocks split five ways with a block for each non-zero side', async (t) => {
  const harness = await createPreactHarness(t);
  const { diffBlocks } = await harness.importDashboardModule('js/pr-link-hover.js');
  assert.deepEqual(diffBlocks(0, 0), { add: 0, del: 0 });
  assert.deepEqual(diffBlocks(412, 88), { add: 4, del: 1 });
  assert.deepEqual(diffBlocks(1000, 1), { add: 4, del: 1 }, 'a lone deletion still shows');
  assert.deepEqual(diffBlocks(1, 1000), { add: 1, del: 4 }, 'a lone addition still shows');
  assert.deepEqual(diffBlocks(37, 0), { add: 5, del: 0 });
});

test('hovering the PR link opens a details card', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRLinkHover } = await harness.importDashboardModule('js/pr-link-hover.js');
  const saved = globalThis.fetch;
  const calls = [];
  const details = {
    title: 'Dock: remember collapsed sections', state: 'draft', author: 'alice',
    head_ref: 'dock-collapse', base_ref: 'main', excerpt: 'The dock forgot collapsed sections.',
    additions: 1204, deletions: 3, changed_files: 1, commits: 2,
    created_at: new Date(Date.now() - 3 * 3600e3).toISOString(),
    fetched_at: new Date().toISOString(),
  };
  const replies = [
    { url: PR, resolved: false, refreshing: true },
    { url: PR, resolved: true, refreshing: false, details },
  ];
  globalThis.fetch = async (url) => {
    calls.push(String(url));
    const reply = replies[Math.min(calls.length - 1, replies.length - 1)];
    return { ok: true, json: async () => reply };
  };
  t.after(() => { globalThis.fetch = saved; });

  const mounted = await harness.mount(harness.html`
    <span><${PRLinkHover} url=${PR} prNumber=${3090} className="pr-state-draft" label="#3090"
      describe="Draft pull request #3090" /></span>
  `);
  try {
    const link = mounted.container.querySelector('a.pr-link');
    assert.ok(link.classList.contains('pr-state-draft'), 'the state colour class is kept');
    assert.equal(link.textContent, '#3090');
    assert.equal(link.getAttribute('href'), PR);
    assert.equal(link.getAttribute('title'), null, 'no native tooltip competing with the card');
    assert.deepEqual(calls, [], 'nothing is fetched until a human hovers');

    const root = mounted.container.querySelector('.prd-hover');
    await harness.act(() => { harness.fireEvent(root, 'mouseenter'); });
    await harness.act(async () => {});
    let panel = mounted.container.querySelector('.prd-panel');
    assert.ok(panel, 'hover opens the card');
    assert.equal(calls[0], `/api/pr-details?url=${encodeURIComponent(PR)}`);
    assert.match(panel.textContent, /Fetching pull request/);

    // The daemon said a read was in flight; the card asks again and paints it.
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 800)));
    await harness.act(async () => {});
    panel = mounted.container.querySelector('.prd-panel');
    assert.equal(calls.length, 2);
    assert.match(panel.querySelector('.prd-title').textContent, /Dock: remember collapsed sections\s+#3090/);
    assert.equal(panel.querySelector('.prd-state').textContent, 'Draft');
    assert.match(panel.querySelector('.prd-meta').textContent, /alice · opened 3h ago/);
    assert.equal(panel.querySelector('.prd-branches').textContent, 'dock-collapse → main');
    assert.equal(panel.querySelector('.prd-add').textContent, '+1,204');
    assert.equal(panel.querySelector('.prd-del').textContent, '−3');
    assert.equal(panel.querySelector('.prd-counts').textContent, '1 file · 2 commits');
    assert.equal(panel.querySelectorAll('.prd-bar i.a').length, 4);
    assert.equal(panel.querySelectorAll('.prd-bar i.d').length, 1);
    assert.equal(panel.querySelector('.prd-excerpt').textContent, 'The dock forgot collapsed sections.');
    const links = [...panel.querySelectorAll('.ci-panel-note a')].map((a) => a.getAttribute('href'));
    assert.deepEqual(links, [PR, `${PR}/files`]);

    // A resolved card stops polling.
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 800)));
    assert.equal(calls.length, 2, 'a resolved card does not keep polling');

    await harness.act(() => { harness.fireEvent(root, 'mouseleave'); });
    assert.equal(mounted.container.querySelector('.prd-panel'), null);
  } finally {
    await mounted.unmount();
  }
});

test('only GitHub pull requests get a card; other forges keep the plain link', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRLinkHover, githubPRBase } = await harness.importDashboardModule('js/pr-link-hover.js');
  assert.equal(githubPRBase(`${PR}/files?w=1#diff`), PR);
  assert.equal(githubPRBase('https://gitlab.com/a/b/-/merge_requests/3'), '');
  assert.equal(githubPRBase('javascript:alert(1)'), '');

  const other = 'https://gitlab.com/a/b/-/merge_requests/3';
  const mounted = await harness.mount(harness.html`
    <span><${PRLinkHover} url=${other} prNumber=${3} className="pr-state-open" label="#3" describe="MR" /></span>
  `);
  try {
    assert.equal(mounted.container.querySelector('.prd-hover'), null, 'no hover root, so no card');
    const link = mounted.container.querySelector('a.pr-link');
    assert.equal(link.getAttribute('href'), other);
    assert.equal(link.getAttribute('title'), 'MR');
  } finally {
    await mounted.unmount();
  }
});
