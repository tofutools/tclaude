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
      elapsed({ started_at: '2026-08-09T10:00:00Z', completed_at: '0001-01-01T00:00:00Z' }, now),
      '3m 30s',
      'GitHub zero-time means the check is still running',
    );
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

  await t.test('the badge links to the build behind its state', () => {
    const { badgeHref } = mod;
    const run = 'https://github.com/o/r/actions/runs/31333654230';
    assert.equal(badgeHref({ state: 'failing', run_url: run }, PR), run,
      'a red badge goes straight to the failing build');
    // No run could be named (external CI app only) — the PR's checks tab is
    // still a useful landing place, so the badge is never a dead pill.
    assert.equal(badgeHref({ state: 'passing' }, PR), `${PR}/checks`);
    assert.equal(badgeHref({ run_url: 'javascript:alert(1)' }, PR), `${PR}/checks`,
      'a hostile run_url must not become the click target');
    // The fallback branch is guarded too, so neither path can produce a
    // scheme the dashboard would execute.
    assert.equal(badgeHref({}, 'javascript:alert(1)'), '');
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
        summary=${{ total: 14, passed: 12, failed: 1, pending: 1, skipped: 0, state: 'failing',
                    run_url: 'https://github.com/o/r/actions/runs/9' }} />
    `);
    try {
      const badge = mounted.container.querySelector('a.ci-badge');
      assert.ok(badge, 'the badge must be a link so clicking it opens the build');
      assert.equal(badge.getAttribute('href'), 'https://github.com/o/r/actions/runs/9');
      assert.equal(badge.getAttribute('target'), '_blank');
      assert.ok(badge.className.includes('ci-failing'), `badge class = ${badge.className}`);
      assert.equal(badge.querySelector('.ci-count').textContent, '12/14');
      assert.match(badge.getAttribute('aria-label'), /12 passed · 1 failed · 1 running/);
      assert.match(badge.getAttribute('aria-label'), /Opens the build/,
        'the label must say where activating the link goes');
      assert.equal(badge.getAttribute('aria-haspopup'), null,
        'pressing the link navigates, so it must not promise a popup');
      // The native title tooltip would fire on the same hover that opens the
      // panel and land on top of it; aria-label carries the text instead.
      assert.equal(badge.getAttribute('title'), null);
      assert.equal(mounted.container.querySelector('.ci-panel'), null,
        'the panel stays closed until the operator hovers or focuses it');
    } finally {
      await mounted.unmount();
    }
  });
});

test('the panel is placed against the usable area, flipping above when that is where the room is', async (t) => {
  const harness = await createPreactHarness(t);
  const { placePRChecksPanel } = await harness.importDashboardModule('js/pr-checks-hover.js');
  // area is the viewport minus fixed chrome; these cases use the whole thing
  // except where the footer/dock case says otherwise.
  const area = { top: 0, left: 0, right: 1200, bottom: 800, width: 1200, height: 800 };
  const panel = { width: 360 };
  const anchorAt = (top, left = 400) => ({ top, bottom: top + 16, left, right: left + 46 });

  await t.test('below by default, even with plenty of room above', () => {
    // A badge halfway down a tall viewport has room both ways; downward is
    // where the eye expects the panel, so it must not flip opportunistically.
    const placed = placePRChecksPanel({ anchor: anchorAt(300), panel, area });
    assert.equal(placed.placement, 'below');
    assert.equal(placed.top, 323, 'hangs just under the badge');
    assert.equal(placed.maxHeight, 320);
  });

  await t.test('above when the badge sits low and there is more room up there', () => {
    // The reported case: a row near the bottom of a long table.
    const placed = placePRChecksPanel({ anchor: anchorAt(700), panel, area });
    assert.equal(placed.placement, 'above');
    assert.ok(placed.top >= 8, 'stays inside the usable area');
    assert.ok(placed.top + placed.maxHeight <= 700 - 7, 'clears the badge');
  });

  await t.test('the side does not depend on how much content is loaded', () => {
    // The panel opens showing "Fetching checks…" and grows to a full list a
    // moment later. Deciding the side on the rendered height would flip the
    // panel to the other side mid-hover — yanking it out from under a pointer
    // already moving toward it, which unmounts it and aborts the poll. The
    // decision uses the panel's maximum height, so it is stable for the whole
    // hover no matter what arrives.
    const low = anchorAt(560);
    const first = placePRChecksPanel({ anchor: low, panel, area });
    const afterContentGrew = placePRChecksPanel({ anchor: low, panel, area });
    assert.equal(first.placement, afterContentGrew.placement);
    assert.equal(first.placement, 'above', 'a full list would not have fit below');
    assert.deepEqual(first, afterContentGrew, 'placement is a function of geometry alone');
  });

  await t.test('a cramped side gets a scrollable panel, not an off-screen one', () => {
    const short = { top: 0, left: 0, right: 1200, bottom: 400, width: 1200, height: 400 };
    const placed = placePRChecksPanel({ anchor: anchorAt(330), panel, area: short });
    assert.equal(placed.placement, 'above');
    assert.ok(placed.top >= 8, 'never above the top edge');
    assert.ok(placed.top + placed.maxHeight <= short.bottom - 8,
      'and never past the bottom edge either');
  });

  await t.test('a viewport too short for the minimum still keeps the panel on screen', () => {
    // The min-height floor can exceed the room available; the position is
    // clamped rather than trusted, so the panel overlaps the badge instead of
    // leaving the screen.
    const tiny = { top: 0, left: 0, right: 800, bottom: 180, width: 800, height: 180 };
    const placed = placePRChecksPanel({ anchor: anchorAt(150), panel, area: tiny });
    assert.ok(placed.top >= 8 && placed.top + placed.maxHeight <= tiny.bottom - 8,
      `panel at ${placed.top}+${placed.maxHeight} must stay inside a ${tiny.bottom}px area`);
  });

  await t.test('fixed chrome owning an edge is excluded, not painted over', () => {
    // The footer bar and the agent dock paint ABOVE the panel, so a panel
    // merely "inside the viewport" can have its last rows and its GitHub link
    // hidden behind them.
    const inset = { top: 0, left: 0, right: 1200 - 264, bottom: 800 - 29, width: 936, height: 771 };
    const belowFooter = placePRChecksPanel({ anchor: anchorAt(430), panel, area: inset });
    assert.ok(belowFooter.top + belowFooter.maxHeight <= inset.bottom - 8,
      'the panel must clear the footer bar');
    const nearDock = placePRChecksPanel({ anchor: anchorAt(200, 900), panel, area: inset });
    assert.equal(nearDock.left, inset.right - 360 - 8, 'and stay clear of the dock rail');
  });

  await t.test('horizontal placement is clamped into the usable area', () => {
    const right = placePRChecksPanel({ anchor: anchorAt(100, 1150), panel, area });
    assert.equal(right.left, 1200 - 360 - 8, 'a badge near the right edge pulls the panel back');
    const left = placePRChecksPanel({ anchor: anchorAt(100, -40), panel, area });
    assert.equal(left.left, 8, 'and never goes off the left edge');
  });

  await t.test('the hover bridge spans both boxes, whichever side wins', () => {
    // A clamped panel can sit well to the side of its badge; a bridge that
    // only covered the badge's own width would let a diagonal travel fall
    // outside the hover root and close the panel.
    const placed = placePRChecksPanel({ anchor: anchorAt(100, 1150), panel, area });
    const { bridge } = placed;
    assert.equal(bridge.left, Math.min(placed.left, 1150));
    assert.equal(bridge.left + bridge.width, Math.max(placed.left + 360, 1196));
    assert.equal(bridge.top, 116, 'covers the gap under the badge');
    assert.equal(bridge.height, placed.top - 116);

    const above = placePRChecksPanel({ anchor: anchorAt(700), panel, area });
    assert.equal(above.bridge.top, above.top + above.maxHeight, 'and above it when flipped');
    assert.equal(above.bridge.height, 700 - (above.top + above.maxHeight));
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
    const heading = panel.querySelector('.ci-panel-heading a');
    assert.equal(heading.getAttribute('href'), mounted.container.querySelector('.ci-badge').getAttribute('href'),
      'the popover title uses the same CI summary target as the badge');
    assert.equal(heading.getAttribute('target'), '_blank');
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

test('a running check ticks locally between polls', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRChecksBadge } = await harness.importDashboardModule('js/pr-checks-hover.js');
  stubFetch(t, () => ({
    summary: { total: 1, passed: 0, failed: 0, pending: 1, skipped: 0, state: 'pending' },
    checks: [{
      name: 'test', bucket: 'pending', conclusion: 'in progress', source: 'CI',
      started_at: '2026-08-09T10:00:00Z', completed_at: '0001-01-01T00:00:00Z',
    }],
    resolved: true,
  }));

  let now = Date.parse('2026-08-09T10:03:30Z');
  const savedNow = Date.now;
  const savedSetInterval = globalThis.setInterval;
  const savedClearInterval = globalThis.clearInterval;
  const intervals = new Map();
  let nextInterval = 1;
  Date.now = () => now;
  globalThis.setInterval = (callback, milliseconds) => {
    assert.equal(milliseconds, 1000, 'the elapsed clock updates once per second');
    const id = nextInterval++;
    intervals.set(id, callback);
    return id;
  };
  globalThis.clearInterval = (id) => intervals.delete(id);
  t.after(() => {
    Date.now = savedNow;
    globalThis.setInterval = savedSetInterval;
    globalThis.clearInterval = savedClearInterval;
  });

  const mounted = await harness.mount(harness.html`
    <${PRChecksBadge} url=${PR} prNumber=${2166}
      summary=${{ total: 1, passed: 0, failed: 0, pending: 1, skipped: 0, state: 'pending' }} />
  `);
  try {
    const root = mounted.container.querySelector('.ci-hover');
    await harness.act(() => { harness.fireEvent(root, 'mouseenter'); });
    await flush();
    await harness.act(async () => {});

    const time = () => mounted.container.querySelector('.ci-check-time')?.textContent;
    assert.equal(time(), '3m 30s');
    // Preact deliberately schedules passive effects after paint; let that
    // queue flush before inspecting the interval it installed.
    await new Promise((resolve) => setTimeout(resolve, 120));
    await harness.act(async () => {});
    assert.equal(intervals.size, 1, 'one local clock runs while a started check is pending');

    now += 1000;
    await harness.act(() => { [...intervals.values()][0](); });
    assert.equal(time(), '3m 31s', 'the display advances without waiting for another fetch');

    await harness.act(() => { harness.fireEvent(root, 'mouseleave'); });
    assert.equal(intervals.size, 0, 'closing the popover stops its local clock');
  } finally {
    await mounted.unmount();
  }
});

// LinkeDOM performs no layout, so every other component test in this file
// exercises the pre-measurement branch (the panel renders hidden). Without a
// stub, a regression that left the panel permanently invisible — or that
// stopped registering the scroll listeners keeping it attached — would pass
// unnoticed. Stub the two layout reads the component makes and assert the
// wiring itself.
test('the measured panel is positioned and follows the viewport', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRChecksBadge } = await harness.importDashboardModule('js/pr-checks-hover.js');
  stubFetch(t, () => ({ summary: {}, checks: [], resolved: false }));

  const doc = globalThis.document;
  const win = doc.defaultView || globalThis.window;
  const listeners = [];
  const savedAdd = win.addEventListener;
  const savedRemove = win.removeEventListener;
  win.addEventListener = (type, fn, opts) => { listeners.push(type); return savedAdd?.call(win, type, fn, opts); };
  win.removeEventListener = (type, fn, opts) => {
    const i = listeners.indexOf(type);
    if (i >= 0) listeners.splice(i, 1);
    return savedRemove?.call(win, type, fn, opts);
  };
  // A badge low on an 800px viewport: the panel belongs above it.
  const savedRect = harness.ElementPrototype?.getBoundingClientRect;
  const proto = harness.ElementPrototype || doc.body.constructor.prototype;
  proto.getBoundingClientRect = function rect() {
    if (this.classList?.contains('ci-panel')) return { top: 0, left: 0, width: 360, height: 320, right: 360, bottom: 320 };
    return { top: 700, bottom: 716, left: 400, right: 446, width: 46, height: 16 };
  };
  Object.defineProperty(doc.documentElement, 'clientWidth', { value: 1200, configurable: true });
  Object.defineProperty(doc.documentElement, 'clientHeight', { value: 800, configurable: true });
  t.after(() => {
    win.addEventListener = savedAdd;
    win.removeEventListener = savedRemove;
    if (savedRect) proto.getBoundingClientRect = savedRect; else delete proto.getBoundingClientRect;
  });

  const mounted = await harness.mount(harness.html`
    <${PRChecksBadge} url=${PR} prNumber=${2151}
      summary=${{ total: 4, passed: 4, skipped: 0, failed: 0, pending: 0, state: 'passing' }} />
  `);
  try {
    const root = mounted.container.querySelector('.ci-hover');
    await harness.act(() => { harness.fireEvent(root, 'mouseenter'); });
    await flush();
    await harness.act(async () => {});

    const panel = mounted.container.querySelector('.ci-panel');
    assert.ok(panel, 'the panel opens');
    const style = panel.getAttribute('style') || '';
    assert.match(style, /top:\d+px/, `panel must be positioned, got style=${style}`);
    assert.match(style, /left:\d+px/);
    assert.match(style, /max-height:\d+px/);
    assert.doesNotMatch(style, /visibility:hidden/, 'a measured panel must be visible');
    assert.ok(root.className.includes('ci-place-above'),
      `a badge at 700px of an 800px viewport belongs above it, got ${root.className}`);
    assert.ok(mounted.container.querySelector('.ci-bridge'), 'the hover bridge is rendered');

    assert.ok(listeners.includes('scroll'), 'scroll is watched while open');
    assert.ok(listeners.includes('resize'), 'resize is watched while open');

    await harness.act(() => { harness.fireEvent(root, 'mouseleave'); });
    assert.equal(mounted.container.querySelector('.ci-panel'), null);
    assert.ok(!listeners.includes('scroll'), 'and both are dropped on close');
    assert.ok(!listeners.includes('resize'));
  } finally {
    await mounted.unmount();
  }
});
