// The phone's Now view (js/phone-now.js, js/phone-approval.js) in a real
// browser on the sandbox's data: the usage strip opens Usage, an access
// request is a card with Deny / +5 min / Approve, a decision's ⋯ opens the
// whole decision, and the agents at work are rows that open their terminal.
//
//   scripts/sandbox/ui.sh smoke phone-now
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

test('Now shows what needs you as cards, and its strip and rows go where they say', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.goto('/');
  await booted(page);
  const needs = page.getByRole('group', { name: 'Needs you' });
  const request = needs.locator('.phone-box-key').first();
  await expect(request).toBeVisible();
  await expect(request.locator('.phone-btns .ui-btn')).toHaveText(['Deny', '+5 min', /^Approve/]);
  await expect(request.locator('.phone-pill-key')).toHaveText(/^(\d+:\d\d|\d+h \d\dm)$/);

  const decision = needs.locator('.phone-box-ask').first();
  await decision.getByRole('button', { name: 'All options, a note, or dismiss' }).click();
  const sheet = page.locator('.phone-sheet[role=dialog]');
  await expect(sheet.getByRole('textbox', { name: 'Note to send with your choice' })).toBeVisible();
  await expect(sheet.getByRole('button', { name: 'Dismiss' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(sheet).toBeHidden();

  // No side scroll: the usage strip fits (B6).
  const { scroll, width } = await page.evaluate(() => ({ scroll: document.querySelector('.phone-view').scrollWidth, width: document.querySelector('.phone-view').clientWidth }));
  expect(scroll).toBeLessThanOrEqual(width);

  await page.getByRole('group', { name: 'Working' }).locator('.phone-row').first().click();
  await expect(page.locator('.phone-term[role=dialog]')).toBeVisible();
  await page.goBack();
  await expect(page.locator('.phone-term[role=dialog]')).toBeHidden();

  await page.getByRole('button', { name: /^Usage: .*Open Usage$/ }).click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Usage');
  expect(errors).toEqual([]);
});
