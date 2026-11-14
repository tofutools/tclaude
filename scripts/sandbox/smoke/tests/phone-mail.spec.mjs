// Mail on a phone (js/phone-mail-island.js) in a real browser: the mailbox
// chips and rows fit the phone, a message opens as a full-screen page whose
// back button (and the browser's Back) returns to the list, the Agents
// picker lists agent folders, and Write opens the message dialog as a bottom
// sheet. Nothing on the way scrolls the page sideways.
//
//   scripts/sandbox/ui.sh smoke phone-mail
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

test('Mail lists, opens, goes back, picks an agent folder and writes', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/');
  await booted(page);
  await page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: /Mail/ }).click();
  const chips = page.getByRole('group', { name: 'Mailboxes' });
  await expect(chips.getByRole('button', { name: /^Human/ })).toHaveAttribute('aria-pressed', 'true');
  const rows = page.locator('.phone-mail-open');
  await expect(rows.first()).toBeVisible();
  await expectNoSideScroll(page, 'the list');

  await rows.first().click();
  await expect(page.locator('.phone-mail-reader')).toBeVisible();
  await expect(page).toHaveURL(/\/messages\/human\/[^/]+$/);
  await expect(page.locator('html')).toHaveAttribute('data-phone-page', 'full');
  await expect(page.getByRole('navigation', { name: 'Views' })).toHaveCount(0);
  await expectNoSideScroll(page, 'the reader');
  await page.locator('.phone-bar-back').click();
  await expect(page).toHaveURL(/\/messages\/human$/);
  await expect(page.locator('.phone-mail-reader')).toHaveCount(0);
  await expect(page.locator('html')).not.toHaveAttribute('data-phone-page', 'full');

  await rows.first().click();
  await expect(page.locator('.phone-mail-reader')).toBeVisible();
  await page.goBack();
  await expect(page.locator('.phone-mail-reader')).toHaveCount(0);
  await expect(rows.first()).toBeVisible();

  await chips.getByRole('button', { name: /Agents/ }).click();
  await expect(page.locator('.phone-mail-agents .phone-menu-item').first()).toBeVisible();
  await expectNoSideScroll(page, 'the agents picker');
  await chips.getByRole('button', { name: /^Human/ }).click();

  await page.locator('.phone-mail-write').click();
  const dialog = page.locator('.modal-overlay.show .ui-dialog');
  await expect(dialog).toBeVisible();
  const { left, right, width } = await dialog.evaluate((el) => {
    const box = el.getBoundingClientRect();
    return { left: box.left, right: box.right, width: document.documentElement.clientWidth };
  });
  expect(left, 'the dialog is a full-width sheet').toBeLessThanOrEqual(1);
  expect(right).toBeGreaterThanOrEqual(width - 1);
  await expectNoSideScroll(page, 'the write dialog');
  expect(errors).toEqual([]);
});
