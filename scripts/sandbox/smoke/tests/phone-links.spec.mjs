// Links into views a phone does not show the desktop way (js/phone-desktop-only.js,
// js/phone-process-runs.js, js/drawing-board-island.js) in a real browser on
// the sandbox's data: the Fleet map and the Route map say they are desktop
// only, a Groups run link opens the run in Automations, and a drawing board
// opens read-only, fitted, with no tools.
//
//   scripts/sandbox/ui.sh smoke phone-links
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

function collectErrors(page) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  page.on('console', (message) => {
    if (message.type() === 'error' && !/429 \/api\/companion\/presence/.test(message.text())) errors.push(`console: ${message.text()}`);
  });
  return errors;
}

const desktopPage = (page) => page.locator('.phone-desktop-page:visible');

test('the Fleet map and the Route map say they are desktop only, with a way on', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/map');
  await booted(page);
  await expect(page.locator('.phone-bar-title')).toHaveText('Fleet map');
  await expect(desktopPage(page)).toContainText('Fleet map is desktop only');
  await desktopPage(page).getByRole('button', { name: 'Show fleet' }).click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Fleet');
  await expect(desktopPage(page)).toHaveCount(0);

  await page.goto('/groups/routes');
  await booted(page);
  await expect(page.locator('.phone-bar-title')).toHaveText('Route map');
  await expect(desktopPage(page)).toContainText('Route map is desktop only');
  await desktopPage(page).getByRole('button', { name: 'Show agents' }).click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Agents');

  // The bar's ‹ leads the same way, as in the mock.
  await page.goto('/groups/routes');
  await booted(page);
  await page.getByRole('button', { name: 'Back: Show agents' }).click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Agents');
  expect(errors).toEqual([]);
});

test('a Groups run link opens the run in Automations, and Back goes to the Runs', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/groups/runs/webshop/run_release_24');
  await booted(page);
  await expect(page.locator('.phone-bar-page .phone-bar-title')).toHaveText('Release train');
  await expect(page).toHaveURL(/\/automations\/processes/);
  await page.locator('.phone-bar-back').click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Automations');
  await expect(page.locator('.phone-run-open').first()).toBeVisible();
  // Agents is Agents, not the run link again (the hidden Groups island's
  // single-run focus is cleared).
  await page.locator('.phone-nav').getByRole('button', { name: /Agents/ }).click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Agents');
  await expect(page).toHaveURL(/\/groups$|\/$/);
  expect(errors).toEqual([]);
});

test('a drawing board opens read-only and fitted', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/drawing-boards/search-architecture');
  await booted(page);
  const editor = page.locator('.drawing-board-editor:visible');
  await expect(editor.locator('.drawing-board-title')).toHaveText('Search architecture');
  await expect(editor.locator('.drawing-board-swatch')).toHaveCount(0);
  await expect(editor.getByRole('button', { name: 'File' })).toHaveCount(0);
  await expect(editor.locator('.phone-desktop-only')).toContainText('desktop only');
  await expect(editor.locator('.drawing-board-zoom')).not.toHaveText('100%');
  const { scroll, width } = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, width: document.documentElement.clientWidth }));
  expect(scroll).toBeLessThanOrEqual(width);
  expect(errors).toEqual([]);
});
