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
  await expect(page.locator('#phone-shell-root > *')).toHaveCount(0);
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

  test('Agents lists agents by group, and a row opens its sheet and its actions', async ({ page }) => {
    const errors = collectErrors(page);
    await page.goto('/');
    await booted(page);
    await page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: /Agents/ }).click();
    const rows = page.locator('.phone-agent-row');
    await expect(rows.first()).toBeVisible();
    await expect(page.locator('#groups-list')).not.toBeInViewport();
    await expectNoSideScroll(page, 'Agents');
    const name = await rows.first().locator('.phone-agent-title').textContent();
    await rows.first().click();
    const sheet = page.getByRole('dialog', { name, exact: true });
    await expect(sheet).toBeVisible();
    await sheet.getByRole('button', { name: 'Restart…' }).click();
    await expect(sheet).toBeHidden();
    const confirm = page.getByRole('dialog', { name: `Restart ${name}?` });
    await expect(confirm).toBeVisible();
    await confirm.getByRole('button', { name: 'Cancel' }).click();
    await expect(confirm).toBeHidden();
    expect(errors).toEqual([]);
  });

  test('a Board task opens its dialog over the phone shell', async ({ page }) => {
    await page.goto('/');
    await booted(page);
    await page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: /Board/ }).click();
    await page.locator('.phone-task').first().click();
    await expect(page.locator('.modal-overlay').getByRole('dialog')).toBeVisible();
  });

  test('Open docks the agent terminal full screen; Back closes it where it was opened', async ({ page }) => {
    const errors = collectErrors(page);
    await page.goto('/');
    await booted(page);
    await page.getByRole('group', { name: 'Needs you' }).getByRole('button', { name: 'Open', exact: true }).first().click();
    const terminal = page.locator('.phone-term[role=dialog]');
    await expect(terminal).toBeVisible();
    await expect(terminal.locator('.xterm')).toBeVisible();
    await expect(terminal.getByRole('group', { name: 'Keys' }).getByRole('button')).toHaveCount(8);
    await expect(page).toHaveURL(/[?&]terminal=window%3A/);
    await expectNoSideScroll(page, 'Terminal');
    // The phone's Back closes the terminal, and Now is still there.
    await page.goBack();
    await expect(terminal).toBeHidden();
    await expect(page.locator('.phone-bar-title')).toHaveText('Now');
    await expect(page.locator('#terminals-root .xterm')).toHaveCount(0);
    // Forward brings it back; the ‹ closes it.
    await page.goForward();
    await expect(terminal).toBeVisible();
    await terminal.getByRole('button', { name: 'Back' }).click();
    await expect(terminal).toBeHidden();
    await expect(page).not.toHaveURL(/terminal=/);
    expect(errors).toEqual([]);
  });

  test('Settings lists the sections; one opens as a page to edit or to read', async ({ page }) => {
    const errors = collectErrors(page);
    await page.goto('/');
    await booted(page);
    await page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: /More/ }).click();
    await page.getByRole('dialog', { name: 'More' }).getByRole('button', { name: /Settings/ }).click();
    const title = page.locator('.phone-bar-title');
    await expect(title).toHaveText('Settings');
    await expect(page.getByRole('group', { name: 'This phone' })).toBeVisible();
    await expect(page.locator('#config-root')).toBeHidden();
    await expectNoSideScroll(page, 'Settings');

    await page.locator('.phone-config-row', { hasText: 'Notifications' }).click();
    await expect(title).toHaveText('Notifications');
    await expect(page.locator('#cfg-save')).toBeVisible();
    await expect(page.locator('.cfg-rail')).toBeHidden();
    await expectNoSideScroll(page, 'Notifications');
    await page.goBack();
    await expect(title).toHaveText('Settings');

    await page.locator('.phone-config-row', { hasText: 'Daemon' }).click();
    await expect(title).toHaveText('Daemon & network');
    await expect(page.getByRole('note')).toContainText('Read-only on a phone');
    await expect(page.locator('#cfg-save')).toBeHidden();
    await page.getByRole('button', { name: 'Back' }).click();
    await expect(page.getByRole('group', { name: 'This phone' })).toBeVisible();
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
    await expect(page.locator('#phone-shell-root > *')).toHaveCount(0);
    expect(await page.evaluate(() => document.documentElement.dataset.layout ?? null)).toBeNull();
    const back = page.locator('.meta-phone-view');
    await expect(back).toHaveText('Phone view');
    await Promise.all([page.waitForEvent('load'), back.click()]);
    await booted(page);
    await expect(page.locator('.phone-bar-title')).toBeVisible();
    expect(await page.evaluate(() => localStorage.getItem('tclaude.layout'))).toBe('phone');
  });
});
