// The phone shell (js/phone-shell-island.js) in a real browser: a phone lands
// on Now, the bottom nav and the More sheet reach every view without the page
// scrolling sideways, a deep link lands on its tab, and Desktop site / Phone
// view switch between the layouts. A desktop browser gets none of it.
//
//   scripts/sandbox/ui.sh smoke phone-shell
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

// The page never scrolls sideways: a wide tab scrolls inside <main>.
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

test('a desktop browser has no phone shell', async ({ page }) => {
  await page.goto('/');
  await booted(page);
  await expect(page.locator('#phone-shell-root')).toHaveCount(0);
  await expect(page.locator('body > header')).toBeVisible();
  await expect(page.locator('body > nav [data-tab="groups"]')).toBeVisible();
  await expect(page.locator('.meta-phone-view')).toHaveCount(0);
});

test.describe('on a phone', () => {
  test.use(IPHONE);

  test('lands on Now and reaches every view from the bottom nav and More', async ({ page }) => {
    const errors = collectErrors(page);
    await page.goto('/');
    await booted(page);
    const nav = page.getByRole('navigation', { name: 'Views' });
    const title = page.locator('.phone-bar-title');
    await expect(title).toHaveText('Now');
    await expect(nav.getByRole('button', { name: /Now/ })).toHaveAttribute('aria-current', 'page');
    await expect(page.getByRole('group', { name: 'Needs you' })).toBeVisible();
    await expect(page.locator('body > header')).toBeHidden();
    await expect(page.locator('#agent-dock')).toBeHidden();
    await expectNoSideScroll(page, 'Now');

    for (const [slot, heading, section] of [['Agents', 'Agents', '#tab-groups'], ['Mail', 'Mail', '#tab-messages']]) {
      await nav.getByRole('button', { name: new RegExp(slot) }).click();
      await expect(title).toHaveText(heading);
      await expect(page.locator(section)).toBeVisible();
      await expectNoSideScroll(page, slot);
    }

    await nav.getByRole('button', { name: /Board/ }).click();
    await expect(title).toHaveText('Board');
    await expect(page.locator('#tab-messages')).toBeHidden();
    await expect(page.getByRole('tablist', { name: 'Group' })).toBeVisible();
    await expectNoSideScroll(page, 'Board');

    await nav.getByRole('button', { name: /More/ }).click();
    const sheet = page.getByRole('dialog', { name: 'More' });
    await expect(sheet).toBeVisible();
    await sheet.getByRole('button', { name: /Settings/ }).click();
    await expect(sheet).toBeHidden();
    await expect(title).toHaveText('Settings');
    await expect(page.locator('#tab-config')).toBeVisible();
    await expect(nav.getByRole('button', { name: /More/ })).toHaveAttribute('aria-current', 'page');
    await expectNoSideScroll(page, 'Settings');

    await nav.getByRole('button', { name: /Now/ }).click();
    await expect(title).toHaveText('Now');
    expect(errors).toEqual([]);
  });

  test('a deep link lands on its tab inside the shell', async ({ page }) => {
    await page.goto('/messages');
    await booted(page);
    await expect(page.locator('.phone-bar-title')).toHaveText('Mail');
    await expect(page.locator('#tab-messages')).toBeVisible();
  });

  test('Desktop site shows the desktop page, and Phone view comes back', async ({ page }) => {
    await page.goto('/');
    await booted(page);
    await page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: /More/ }).click();
    // Both switches reload the page: wait for the new one before reading it,
    // or the old page (already booted) answers.
    await Promise.all([
      page.waitForEvent('load'),
      page.getByRole('dialog', { name: 'More' }).getByRole('button', { name: /Desktop site/ }).click(),
    ]);
    await booted(page);
    await expect(page.locator('#phone-shell-root')).toHaveCount(0);
    expect(await page.evaluate(() => document.documentElement.dataset.layout ?? null)).toBeNull();
    const back = page.locator('.meta-phone-view');
    await expect(back).toHaveText('Phone view');
    await Promise.all([page.waitForEvent('load'), back.click()]);
    await booted(page);
    await expect(page.locator('.phone-bar-title')).toBeVisible();
    expect(await page.evaluate(() => localStorage.getItem('tclaude.layout'))).toBe('phone');
  });
});
