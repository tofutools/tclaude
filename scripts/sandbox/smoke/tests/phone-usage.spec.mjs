// Usage on a phone (js/phone-usage.js) in a real browser on the sandbox's
// data: the subnav is a chip row, Quota is one card per window, Costs is the
// totals per provider and model plus the top agents, a span chip reloads and
// is a history step, and the desktop views under them stay hidden.
//
//   scripts/sandbox/ui.sh smoke phone-usage
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

async function expectNoSideScroll(page, where) {
  const { scroll, width } = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth, width: document.documentElement.clientWidth,
  }));
  expect(scroll, `${where}: the page is ${scroll}px wide on a ${width}px phone`).toBeLessThanOrEqual(width);
}

function collectErrors(page) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  page.on('console', (message) => {
    if (message.type() === 'error' && !/429 \/api\/companion\/presence/.test(message.text())) errors.push(`console: ${message.text()}`);
  });
  return errors;
}

const views = (page) => page.locator('.phone-usage-chips');

test('Usage: chips, a card per quota window, and the Costs totals', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/usage');
  await booted(page);
  await expect(page.locator('.phone-bar-title')).toHaveText('Usage');
  await expect(page.locator('.usage-overview')).toBeVisible();
  await expect(page.locator('.usage-subnav')).toHaveCount(0);

  await views(page).getByRole('tab', { name: /^Quota/ }).click();
  await expect(page).toHaveURL(/\/usage\/quota$/);
  const cards = page.locator('.phone-quota-card');
  await expect(cards.first()).toBeVisible();
  expect(await cards.count()).toBeGreaterThan(0);
  await expect(cards.first().locator('.phone-quota-pct')).toHaveText(/^\d+%$/);
  await expect(cards.first().locator('.phone-quota-meta')).toContainText('sampled');
  await expect(page.locator('#usage-root')).toBeHidden();
  await expectNoSideScroll(page, 'Usage › Quota');

  await views(page).getByRole('tab', { name: /^Costs/ }).click();
  await expect(page).toHaveURL(/\/usage\/costs$/);
  const costs = page.locator('.phone-usage-costs');
  await expect(costs.locator('.phone-cost-big')).toHaveText(/^\$/);
  for (const title of ['By provider', 'By model', 'Top agents']) {
    await expect(costs.getByRole('group', { name: title }).locator('.phone-cost-row').first()).toBeVisible();
  }
  await expect(page.locator('#costs-root')).toBeHidden();
  await expectNoSideScroll(page, 'Usage › Costs');

  // A span chip is the desktop's span button: a reload and a history step.
  const total = await costs.locator('.phone-cost-big').textContent();
  await costs.locator('.phone-cost-spans').getByRole('tab', { name: 'Last 7d' }).click();
  await expect(page).toHaveURL(/\/usage\/costs\?period=7d$/);
  await expect(costs.locator('.phone-cost-spans').getByRole('tab', { name: 'Last 7d' })).toHaveAttribute('aria-selected', 'true');
  await expect(costs.getByRole('group', { name: /^Total Last 7d/ })).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/\/usage\/costs$/);
  await expect(costs.locator('.phone-cost-big')).toHaveText(total);
  expect(errors).toEqual([]);
});
