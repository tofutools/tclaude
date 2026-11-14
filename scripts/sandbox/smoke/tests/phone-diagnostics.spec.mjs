// Diagnostics, Plugins and Fleet on a phone (js/phone-diagnostics.js,
// js/phone-plugins.js, js/phone-fleet.js) in a real browser: each fits the
// phone, Audit and Logs are two-line rows that open, Doctor leads the
// sub-tabs, Performance keeps its endpoint cards, a plugin is a card with its
// steps, and Fleet is reached from More and changes nothing.
//
//   scripts/sandbox/ui.sh smoke phone-diagnostics
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

test('Diagnostics: Doctor first, Audit and Logs as rows, Performance as cards', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/diagnostics/audit');
  await booted(page);
  const subtabs = page.locator('#tab-diagnostics .activity-subtab');
  const first = await subtabs.evaluateAll((tabs) => tabs.slice().sort((a, b) => a.getBoundingClientRect().left - b.getBoundingClientRect().left)[0].dataset.subtab);
  expect(first).toBe('doctor');

  const row = page.locator('#audit-root .phone-diag-row').first();
  await expect(row).toBeVisible();
  await expect(page.locator('#audit-root table')).toHaveCount(0);
  await row.locator('summary').click();
  await expect(row.locator('.phone-diag-detail')).toBeVisible();
  await page.getByRole('button', { name: 'Failed only' }).click();
  await expect(page.getByRole('button', { name: 'Failed only' })).toHaveAttribute('aria-pressed', 'true');
  await expectNoSideScroll(page, 'Diagnostics › Audit');

  await subtabs.filter({ hasText: 'Logs' }).click();
  await expect(page).toHaveURL(/\/diagnostics\/logs$/);
  await expect(page.locator('#logs-root .phone-diag-row').first()).toBeVisible();
  await expectNoSideScroll(page, 'Diagnostics › Logs');

  await subtabs.filter({ hasText: 'Performance' }).click();
  await expect(page.locator('.debug-card').first()).toBeVisible();
  await expect(page.locator('#profiles-root')).toBeHidden();
  await expectNoSideScroll(page, 'Diagnostics › Performance');

  await subtabs.filter({ hasText: 'Doctor' }).click();
  await expect(page.locator('.doctor-row').first()).toBeVisible();
  await expectNoSideScroll(page, 'Diagnostics › Doctor');
  expect(errors).toEqual([]);
});

test('Plugins: a card per plugin with its steps; Fleet from More, read-only', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/plugins');
  await booted(page);
  const card = page.locator('.phone-plugin').first();
  await expect(card).toBeVisible();
  await expect(card.getByRole('switch').or(card.locator('input[type=checkbox]'))).toHaveCount(1);
  await expect(card.locator('.phone-plugin-step').first()).toBeVisible();
  await expect(page.locator('.phone-desktop-only:visible')).toContainText('desktop only');
  await expect(page.locator('.plugin-catalog-card')).toHaveCount(0);
  await expectNoSideScroll(page, 'Plugins');

  await page.getByRole('button', { name: 'More' }).click();
  await page.locator('.phone-menu-item').filter({ hasText: 'Fleet' }).click();
  await expect(page).toHaveURL(/\/fleet-admin$/);
  await expect(page.locator('.phone-bar-title')).toHaveText('Fleet');
  await expect(page.locator('.phone-fleet')).toBeVisible();
  await expect(page.locator('.phone-fleet').getByRole('button')).toHaveText(['Desktop site ›']);
  await expectNoSideScroll(page, 'Fleet');
  expect(errors).toEqual([]);
});
