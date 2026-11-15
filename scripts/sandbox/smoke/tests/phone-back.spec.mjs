// Back on a phone (js/phone-history.js) in a real browser: it closes what is
// on top (a sheet, a page pushed inside a tab) and returns to the view the
// operator came from, Now and Board included. Closing on screen leaves no
// extra step behind.
//
//   scripts/sandbox/ui.sh smoke phone-back
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

const nav = (page, label) => page.locator('.phone-nav-item', { hasText: label }).click();
const title = (page) => page.locator('.phone-bar-title');

test('Back returns to Now and Board, and closes sheets', async ({ page }) => {
  await page.goto('/');
  await booted(page);
  await expect(title(page)).toHaveText('Now');
  await nav(page, 'Mail');
  await expect(title(page)).toHaveText(/^Mail/);
  await page.goBack();
  await expect(title(page)).toHaveText('Now');

  await nav(page, 'Board');
  await expect(title(page)).toHaveText(/^Board/);
  await nav(page, 'Mail');
  await nav(page, 'More');
  await expect(page.getByRole('dialog', { name: 'More' })).toBeVisible();
  await page.goBack();
  await expect(page.getByRole('dialog', { name: 'More' })).toHaveCount(0);
  await expect(title(page)).toHaveText(/^Mail/);
  await page.goBack();
  await expect(title(page)).toHaveText(/^Board/);

  await nav(page, 'Agents');
  await page.locator('.phone-agent-title').first().click();
  const sheet = page.locator('.phone-sheet');
  await expect(sheet).toBeVisible();
  await page.goBack();
  await expect(sheet).toHaveCount(0);
  await expect(title(page)).toHaveText('Agents');
});

test('a page inside a tab is a step of its own, and its ‹ leaves no extra step', async ({ page }) => {
  await page.goto('/messages');
  await booted(page);
  await nav(page, 'More');
  await page.locator('.phone-sheet button', { hasText: 'Automations' }).click();
  await expect(title(page)).toHaveText('Automations');
  await page.locator('.phone-auto-open').first().click();
  await expect(page.locator('.phone-bar-back')).toBeVisible();
  await page.goBack();
  await expect(page.locator('.phone-bar-back')).toHaveCount(0);
  await expect(title(page)).toHaveText('Automations');

  await page.locator('.phone-auto-open').first().click();
  await page.locator('.phone-bar-back').click();
  await expect(title(page)).toHaveText('Automations');
  await page.goBack();
  await expect(title(page)).toHaveText(/^Mail/);
});

test('a More entry picked over Now: Back returns to Now, and the address shows the view', async ({ page }) => {
  await page.goto('/');
  await booted(page);
  await expect(title(page)).toHaveText('Now');
  for (const entry of ['Automations', 'Fleet']) {
    await nav(page, 'More');
    await page.locator('.phone-sheet button', { hasText: entry }).click();
    await expect(title(page)).toHaveText(entry);
    await expect(page).not.toHaveURL(/[?&](own|phone)=/);
    await page.goBack();
    await expect(title(page)).toHaveText('Now');
  }
});
