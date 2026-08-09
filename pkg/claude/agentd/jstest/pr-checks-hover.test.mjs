// Tests for the CI indicator that trails a PR badge in the Groups tab.
//
// The point of the badge is that an operator can see, without clicking into
// GitHub, whether the PR an agent just opened is green. Two properties carry
// that: the compact n/m must not count skipped jobs as outstanding work, and
// the panel must fetch only while someone is looking — the whole reason the
// per-check list is NOT on the 2s snapshot. Both are asserted here.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const PR = 'https://github.com/tofutools/tclaude/pull/2151';

function stubFetch(t, respond) {
  const calls = [];
  const saved = globalThis.fetch;
  globalThis.fetch = async (url) => {
    calls.push(String(url));
    return { ok: true, json: async () => respond(calls.length) };
  };
  t.after(() => { globalThis.fetch = saved; });
  return calls;
}

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

test('the CI badge summarizes checks and opens a panel on hover', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/pr-checks-hover.js');
  const { PRChecksBadge, checkDenominator, elapsed, summaryLine } = mod;

  await t.test('skipped checks leave the denominator', () => {
    // 12/14 must mean "twelve of the fourteen that had to run" — counting two
    // path-filtered jobs as outstanding would read as a PR still working.
    assert.equal(checkDenominator({ total: 16, passed: 12, skipped: 2, failed: 1, pending: 1 }), 14);
    assert.equal(checkDenominator(null), 0);
  });

  await t.test('a running check keeps counting up', () => {
    const now = Date.parse('2026-08-09T10:03:30Z');
    assert.equal(elapsed({ started_at: '2026-08-09T10:00:00Z' }, now), '3m 30s');
    assert.equal(
      elapsed({ started_at: '2026-08-09T10:00:00Z', completed_at: '2026-08-09T10:00:42Z' }, now),
      '42s',
      'a completed check keeps its final duration, not the age of the cache',
    );
    assert.equal(elapsed({}, now), '');
  });

  await t.test('a hostile check URL never becomes a live link', () => {
    // A commit status's target_url is set by whoever posted it, and this
    // renders inside the dashboard origin whose cookie authorizes every
    // mutating /api route. The server drops non-http(s) schemes too; neither
    // side may be the only guard.
    const { safeCheckURL } = mod;
    for (const hostile of [
      'javascript:fetch("/api/agents/x",{method:"DELETE"})',
      'JavaScript:alert(1)', 'data:text/html,<script>1</script>', 'file:///etc/passwd', '',
    ]) {
      assert.equal(safeCheckURL(hostile), '', `must refuse ${hostile}`);
    }
    assert.equal(safeCheckURL('https://github.com/o/r/runs/1'), 'https://github.com/o/r/runs/1');
  });

  await t.test('the summary line names what is outstanding', () => {
    assert.equal(
      summaryLine({ total: 14, passed: 12, failed: 1, pending: 1 }),
      '12 passed · 1 failed · 1 running',
    );
    assert.equal(summaryLine({ total: 0 }), 'no checks');
  });

  await t.test('nothing renders when the snapshot carried no checks', async () => {
    const calls = stubFetch(t, () => ({}));
    const mounted = await harness.mount(harness.html`
      <${PRChecksBadge} url=${PR} prNumber=${2151} summary=${null} />
    `);
    try {
      assert.equal(mounted.container.querySelector('.ci-badge'), null,
        'an unresolved PR keeps the bare link it always had');
      assert.deepEqual(calls, [], 'and costs no request');
    } finally {
      await mounted.unmount();
    }
  });

  await t.test('a failing badge reads red and states the counts', async () => {
    stubFetch(t, () => ({ summary: {}, checks: [], resolved: false }));
    const mounted = await harness.mount(harness.html`
      <${PRChecksBadge} url=${PR} prNumber=${2143}
        summary=${{ total: 14, passed: 12, failed: 1, pending: 1, skipped: 0, state: 'failing' }} />
    `);
    try {
      const badge = mounted.container.querySelector('.ci-badge');
      assert.ok(badge, 'expected a badge');
      assert.ok(badge.className.includes('ci-failing'), `badge class = ${badge.className}`);
      assert.equal(badge.querySelector('.ci-count').textContent, '12/14');
      assert.match(badge.getAttribute('title'), /12 passed · 1 failed · 1 running/);
      assert.equal(mounted.container.querySelector('.ci-panel'), null,
        'the panel stays closed until the operator hovers or focuses it');
    } finally {
      await mounted.unmount();
    }
  });
});

test('the panel fetches only while it is open', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRChecksBadge } = await harness.importDashboardModule('js/pr-checks-hover.js');
  const calls = stubFetch(t, () => ({
    url: PR,
    summary: { total: 2, passed: 1, failed: 1, pending: 0, skipped: 0, state: 'failing' },
    checks: [
      {
        name: 'test / go test ./...', bucket: 'fail', conclusion: 'failure', source: 'CI',
        url: 'https://github.com/o/r/runs/1',
        started_at: '2026-08-09T10:00:00Z', completed_at: '2026-08-09T10:03:12Z',
      },
      { name: 'lint / golangci-lint', bucket: 'pass', conclusion: 'success', source: 'CI' },
    ],
    resolved: true,
  }));

  const mounted = await harness.mount(harness.html`
    <${PRChecksBadge} url=${PR} prNumber=${2143}
      summary=${{ total: 2, passed: 1, failed: 1, pending: 0, skipped: 0, state: 'failing' }} />
  `);
  try {
    assert.deepEqual(calls, [], 'a badge nobody is looking at must not poll');

    const root = mounted.container.querySelector('.ci-hover');
    await harness.act(() => { harness.fireEvent(root, 'mouseenter'); });
    await flush();
    await harness.act(async () => {});

    assert.equal(calls.length, 1, 'hovering fires an immediate refresh for this PR');
    assert.match(calls[0], /^\/api\/pr-checks\?url=/);
    assert.ok(calls[0].includes(encodeURIComponent(PR)));

    const panel = mounted.container.querySelector('.ci-panel');
    assert.ok(panel, 'the panel opens on hover');
    const rows = panel.querySelectorAll('.ci-checks li');
    assert.equal(rows.length, 2);
    assert.match(rows[0].textContent, /test \/ go test/);
    assert.match(rows[0].textContent, /CI · failure/);
    assert.match(rows[0].textContent, /3m 12s/, 'each check shows how long it took');
    assert.match(panel.querySelector('.ci-panel-note a').getAttribute('href'), /\/pull\/2151\/checks$/);
    // A check whose details URL the server refused renders unlinked rather
    // than as an href the reader would trust.
    assert.equal(rows[1].querySelector('.ci-check-link'), null,
      'a check with no usable URL must not become a link');

    // Leaving closes the panel and, with it, the poll — the cost of this
    // surface is bounded by "a human is looking at exactly one PR".
    await harness.act(() => { harness.fireEvent(root, 'mouseleave'); });
    assert.equal(mounted.container.querySelector('.ci-panel'), null);
    const settled = calls.length;
    await new Promise((resolve) => setTimeout(resolve, 30));
    assert.equal(calls.length, settled, 'no polling continues after the pointer leaves');
  } finally {
    await mounted.unmount();
  }
});
