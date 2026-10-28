// Tests for the review pill that follows the CI pill on a PR badge, and the
// footer's review-aware filters. The pill must not fetch anything on hover —
// the reviewer list rides the snapshot — and must stay silent for drafts,
// closed PRs and PRs whose reviews are not known yet.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const PR = 'https://github.com/tofutools/tclaude/pull/3046';


// leave moves the pointer off a PR popover and waits out its close grace.
async function leave(harness, root) {
  const { HOVER_CLOSE_MS } = await harness.importDashboardModule('js/pr-checks-hover.js');
  await harness.act(() => { harness.fireEvent(root, 'mouseleave'); });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, HOVER_CLOSE_MS + 20)));
}

test('review state collapses decision and counts', async (t) => {
  const harness = await createPreactHarness(t);
  const { prReviewState, prReviewLabel, readyToMerge } = await harness.importDashboardModule('js/pr-review-model.js');

  assert.equal(prReviewState(null), '', 'unknown renders nothing');
  assert.equal(prReviewState({ approved: 0 }), '', 'a decision-only summary without a list is still unknown');
  assert.equal(prReviewState({ reviewers_known: true }), 'none');
  assert.equal(prReviewState({ approved: 2, reviewers_known: true }), 'approved',
    'approvals count even when the repo requires no reviews');
  assert.equal(prReviewState({ approved: 2, changes_requested: 1 }), 'changes', 'a change request wins');
  assert.equal(prReviewState({ decision: 'approved' }), 'approved');
  assert.equal(prReviewState({ decision: 'review_required', approved: 1 }), 'required');
  assert.equal(prReviewLabel({ approved: 2 }), '2');
  assert.equal(prReviewLabel({ decision: 'review_required', approved: 1 }), '', 'waiting is the icon alone');
  assert.equal(prReviewLabel({ reviewers_known: true }), '', 'no reviews is the grey clock alone');

  assert.equal(readyToMerge({ checks: { state: 'passing' }, reviews: { approved: 1 } }), true);
  assert.equal(readyToMerge({ checks: { state: 'pending' }, reviews: { approved: 1 } }), false);
  assert.equal(readyToMerge({ draft: true, checks: { state: 'passing' }, reviews: { approved: 1 } }), false);
});

test('footer filters use review state', async (t) => {
  const harness = await createPreactHarness(t);
  const { authoredOpenPRsView } = await harness.importDashboardModule('js/shell-model.js');
  const prs = {
    available: true, total: 3,
    items: [
      { number: 1, url: `${PR.replace(/3046$/, '1')}`, checks: { total: 2, passed: 2, state: 'passing' }, reviews: { approved: 1 } },
      { number: 2, url: `${PR.replace(/3046$/, '2')}`, checks: { total: 2, pending: 2, state: 'pending' }, reviews: { changes_requested: 1 } },
      { number: 3, url: `${PR.replace(/3046$/, '3')}`, checks: { total: 2, pending: 2, state: 'pending' } },
    ],
  };
  const view = authoredOpenPRsView({ authored_open_prs: prs });
  assert.equal(view.ready, 1);
  assert.deepEqual(authoredOpenPRsView({ authored_open_prs: prs }, 'ready').items.map((pr) => pr.number), [1]);
  assert.deepEqual(authoredOpenPRsView({ authored_open_prs: prs }, 'attention').items.map((pr) => pr.number), [1, 2],
    'requested changes need the author even while CI still runs');
});

test('the review pill opens a reviewer panel without fetching', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRReviewBadge } = await harness.importDashboardModule('js/pr-review-badge.js');
  const saved = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url) => { calls.push(String(url)); return { ok: true, json: async () => ({}) }; };
  t.after(() => { globalThis.fetch = saved; });

  const reviews = {
    decision: '', approved: 1, changes_requested: 1, reviewers_known: true,
    reviewers: [
      { login: 'bob', state: 'changes_requested', url: `${PR}#pullrequestreview-2`, submitted_at: '2026-10-15T09:00:00Z' },
      { login: 'alice', state: 'approved', url: 'javascript:alert(1)' },
      { login: 'coderabbitai', state: 'approved', bot: true },
    ],
  };
  const mounted = await harness.mount(harness.html`
    <span>
      <${PRReviewBadge} url=${PR} prNumber=${3046} reviews=${reviews} prState="open" />
      <${PRReviewBadge} url=${PR} prNumber=${3046} reviews=${reviews} prState="draft" />
      <${PRReviewBadge} url=${PR} prNumber=${3046} reviews=${null} prState="open" />
    </span>
  `);
  try {
    const badges = mounted.container.querySelectorAll('.rv-badge');
    assert.equal(badges.length, 1, 'drafts and unknown reviews get no pill');
    assert.ok(badges[0].classList.contains('rv-changes'));
    assert.ok(badges[0].querySelector('svg.rv-person'), 'the pill is a person icon, not a ✓ like CI');
    assert.equal(badges[0].textContent.trim(), '1', 'plus the count of change requests');
    assert.equal(badges[0].getAttribute('href'), PR);

    const root = mounted.container.querySelector('.rv-hover');
    await harness.act(() => { harness.fireEvent(root, 'mouseenter'); });
    await harness.act(async () => {});
    const panel = mounted.container.querySelector('.rv-panel');
    assert.ok(panel, 'hover opens the reviewer panel');
    const rows = panel.querySelectorAll('.ci-checks li');
    assert.equal(rows.length, 3);
    assert.match(rows[0].textContent, /bob/);
    assert.equal(rows[0].querySelector('a').getAttribute('href'), `${PR}#pullrequestreview-2`);
    assert.equal(rows[1].querySelector('a').getAttribute('href'), PR, 'a hostile review URL falls back to the PR');
    assert.match(rows[2].textContent, /bot · not counted/);
    const links = [...panel.querySelectorAll('.ci-panel-note a')].map((a) => a.getAttribute('href'));
    assert.deepEqual(links, [PR, `${PR}/files`]);
    assert.deepEqual(calls, [], 'the panel reads the snapshot; hovering costs no request');

    await leave(harness, root);
    assert.equal(mounted.container.querySelector('.rv-panel'), null);
  } finally {
    await mounted.unmount();
  }
});

test('a PR popover follows hover and keyboard focus only, never a click', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRReviewBadge } = await harness.importDashboardModule('js/pr-review-badge.js');
  const { HOVER_CLOSE_MS } = await harness.importDashboardModule('js/pr-checks-hover.js');
  const reviews = { approved: 1, reviewers_known: true, reviewers: [{ login: 'alice', state: 'approved' }] };
  const mounted = await harness.mount(harness.html`
    <${PRReviewBadge} url=${PR} prNumber=${3046} reviews=${reviews} prState="open" />
  `);
  const panel = () => mounted.container.querySelector('.rv-panel');
  const wait = (ms) => harness.act(() => new Promise((resolve) => setTimeout(resolve, ms)));
  try {
    const root = mounted.container.querySelector('.rv-hover');
    const badge = mounted.container.querySelector('.rv-badge');
    let focusVisible = false;
    badge.matches = (selector) => (selector === ':focus-visible' ? focusVisible : false);

    // Clicking the link focuses it; that focus must not hold the panel open
    // once the pointer has gone (e.g. off to the link's target and back).
    await harness.act(() => { harness.fireEvent(root, 'mouseenter'); });
    await harness.act(() => { harness.fireEvent(badge, 'focusin'); });
    await harness.act(() => { harness.fireEvent(badge, 'click'); });
    assert.ok(panel(), 'hovering opens the panel');
    await leave(harness, root);
    assert.equal(panel(), null, 'a clicked, still-focused link does not pin the panel');

    // A brief slip off the badge is forgiven.
    await harness.act(() => { harness.fireEvent(root, 'mouseenter'); });
    await harness.act(() => { harness.fireEvent(root, 'mouseleave'); });
    await wait(HOVER_CLOSE_MS / 4);
    assert.ok(panel(), 'the panel survives the close grace');
    await harness.act(() => { harness.fireEvent(root, 'mouseenter'); });
    await wait(HOVER_CLOSE_MS + 20);
    assert.ok(panel(), 'returning within the grace keeps it open');

    // Leaving the page closes it even though no mouseleave ever arrives.
    await harness.act(() => { harness.fireEvent(globalThis.window, 'blur'); });
    assert.equal(panel(), null, 'the window losing focus closes the panel');

    // Keyboard focus still opens it.
    focusVisible = true;
    await harness.act(() => { harness.fireEvent(badge, 'focusin'); });
    assert.ok(panel(), 'keyboard focus opens the panel');
    await harness.act(() => { harness.fireEvent(badge, 'focusout', { relatedTarget: harness.document.body }); });
    assert.equal(panel(), null);
  } finally {
    await mounted.unmount();
  }
});

test('opening a PR popover closes the one still in its leave grace', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRReviewBadge } = await harness.importDashboardModule('js/pr-review-badge.js');
  const { HOVER_CLOSE_MS } = await harness.importDashboardModule('js/pr-checks-hover.js');
  const reviews = { approved: 1, reviewers_known: true, reviewers: [{ login: 'alice', state: 'approved' }] };
  const other = PR.replace(/3046$/, '3047');
  const mounted = await harness.mount(harness.html`
    <div>
      <${PRReviewBadge} url=${PR} prNumber=${3046} reviews=${reviews} prState="open" />
      <${PRReviewBadge} url=${other} prNumber=${3047} reviews=${reviews} prState="open" />
    </div>
  `);
  const panels = () => [...mounted.container.querySelectorAll('.rv-panel')]
    .map((panel) => panel.querySelector('.ci-panel-heading a').getAttribute('href'));
  try {
    const [first, second] = mounted.container.querySelectorAll('.rv-hover');
    await harness.act(() => { harness.fireEvent(first, 'mouseenter'); });
    await harness.act(() => { harness.fireEvent(first, 'mouseleave'); });
    assert.deepEqual(panels(), [PR], 'the first panel lingers through its grace');

    await harness.act(() => { harness.fireEvent(second, 'mouseenter'); });
    assert.deepEqual(panels(), [other], 'opening the second panel dismisses the first at once');

    await harness.act(() => { harness.fireEvent(second, 'mouseleave'); });
    assert.deepEqual(panels(), [other], 'with nothing new opening, the grace still applies');
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, HOVER_CLOSE_MS + 20)));
    assert.deepEqual(panels(), []);
  } finally {
    await mounted.unmount();
  }
});
