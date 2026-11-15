// The boot bar over repeated reloads (dashboard.html's inline counter), and
// what it does when a boot module is held or fails. The boot graph is ~300
// parallel module requests; dashboard.js runs only once every one of them has
// arrived, so one held request holds the whole boot.
//
//   scripts/sandbox/ui.sh smoke boot-reload
import { test, expect } from '@playwright/test';

const booted = (page) => page.waitForFunction(() => document.body && !document.body.classList.contains('dashboard-booting'), null, { timeout: 30000 });

// Every preload href with a resource-timing entry, and whether it came from
// the HTTP cache. Without the buffer fix, a cached reload overflows Chrome's
// 250-entry default before the counter registers, and entries go missing.
const preloadTimings = (page) => page.evaluate(() => {
  const entries = new Map(performance.getEntriesByType('resource').map((e) => [e.name, e]));
  const hrefs = [...document.querySelectorAll('link[rel="modulepreload"]')].map((l) => l.href);
  return {
    total: hrefs.length,
    missing: hrefs.filter((h) => !entries.has(h)),
    fromCache: hrefs.filter((h) => entries.get(h)?.transferSize === 0).length,
  };
});

test('repeated reloads boot every time, count every module and reuse the cache', async ({ page }) => {
  test.setTimeout(180000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  await page.goto('/');
  await booted(page);
  const cdp = await page.context().newCDPSession(page);
  for (const hard of [false, false, false, true, false, true, false]) {
    if (hard) {
      await cdp.send('Page.reload', { ignoreCache: true });
      await page.waitForLoadState('domcontentloaded');
    } else {
      await page.reload();
    }
    await booted(page);
    const timings = await preloadTimings(page);
    expect(timings.total).toBeGreaterThan(100);
    expect(timings.missing, 'every boot module has a timing entry for the counter').toEqual([]);
    if (!hard) expect(timings.fromCache, 'a normal reload takes the versioned boot modules from the cache').toBe(timings.total);
    await expect(page.locator('#boot-progress-reload')).toBeHidden();
  }
  expect(errors).toEqual([]);
});

test('a held boot module is named, then offered a Reload, and boot goes on when it arrives', async ({ page }) => {
  test.setTimeout(90000);
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  await page.route(/\/static\/v-[^/]+\/js\/export-progress\.js$/, async (route) => {
    await held;
    await route.continue();
  });
  await page.goto('/', { waitUntil: 'commit' });
  const count = page.locator('#boot-progress-count');
  await expect(count).toHaveText(/Waiting on export-progress\.js/, { timeout: 10000 });
  const reload = page.locator('#boot-progress-reload');
  await expect(reload).toBeVisible({ timeout: 10000 });
  // Past the 8s CSS failsafe the stuck label stays, and the dead page stays curtained.
  await page.waitForTimeout(3000);
  await expect(reload).toBeVisible();
  await expect(page.locator('header')).toBeHidden();
  release();
  await booted(page);
  await expect(page.locator('header')).toBeVisible();
});

test('a boot module that fails says so and offers a Reload', async ({ page }) => {
  test.setTimeout(90000);
  // export-progress.js is in dashboard.js's own static graph, so its failure stops boot.
  await page.route(/\/static\/v-[^/]+\/js\/export-progress\.js$/, (route) => route.abort('failed'));
  await page.goto('/', { waitUntil: 'commit' });
  await expect(page.locator('#boot-progress-count')).toHaveText(/The dashboard did not load: export-progress\.js could not be loaded/, { timeout: 10000 });
  // The graph's own import retries a failed preload: with the module still
  // refused, that fails too and the page stays stuck, offering its Reload.
  await page.waitForTimeout(2000);
  await expect(page.locator('#boot-progress-reload')).toBeVisible();
  await page.unrouteAll();
  await Promise.all([page.waitForEvent('framenavigated'), page.locator('#boot-progress-reload').click()]);
  await booted(page);
  await expect(page.locator('#boot-progress-reload')).toBeHidden();
});
