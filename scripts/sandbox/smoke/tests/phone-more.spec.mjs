// More and Settings on a phone (js/phone-more.js, js/phone-sessions.js,
// js/phone-config-island.js) in a real browser on the sandbox's data: More
// gives each entry its scope and Diagnostics its count, Collab is a list of
// the open sessions whose rows open a terminal, and Settings › Notifications
// is the simple form.
//
//   scripts/sandbox/ui.sh smoke phone-more
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

const more = (page) => page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: /More/ });
const entry = (page, name) => page.getByRole('dialog', { name: 'More' }).locator('.phone-menu-item')
  .filter({ has: page.locator('.phone-menu-label', { hasText: new RegExp(`^${name}$`) }) });

test('More: a scope per entry, the Diagnostics count, notifications and this device', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/');
  await booted(page);
  await more(page).click();
  await expect(entry(page, 'Usage').locator('.phone-scope-ro')).toHaveText('read-only');
  await expect(entry(page, 'Automations').locator('.phone-scope-simp')).toHaveText('on/off, run now');
  await expect(entry(page, 'Access').locator('.phone-scope-simp')).toHaveText('approve, revoke');
  await expect(entry(page, 'Fleet').locator('.phone-scope-ro')).toHaveText('nodes, read-only');
  await expect(entry(page, 'Diagnostics').locator('.phone-more-count').first()).toBeVisible();
  await expect(entry(page, 'Notifications').locator('.phone-menu-note')).toHaveText(/^(on|off)$/);
  await expect(entry(page, 'Desktop site').locator('.phone-menu-note')).toHaveText('this device');
  await expectNoSideScroll(page, 'More');
  // A tap beside the sheet closes it and hands focus back to More: after a
  // tap or a click, with no ring.
  await page.locator('.phone-sheet-scrim').click({ position: { x: 20, y: 20 } });
  await expect(page.getByRole('dialog', { name: 'More' })).toHaveCount(0);
  await expect(more(page)).toBeFocused();
  expect(await more(page).evaluate((el) => getComputedStyle(el).outlineStyle)).toBe('none');
  await more(page).click();
  await entry(page, 'Notifications').click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Notifications');
  await expect(page).toHaveURL(/\/config\/notifications$/);
  // ‹ goes back to where More was opened.
  await page.locator('.phone-bar-back').click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Now');
  // So does the phone's Back, and Settings from More then starts on its list.
  await more(page).click();
  await entry(page, 'Notifications').click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Notifications');
  await page.goBack();
  await expect(page.locator('.phone-bar-title')).toHaveText('Now');
  await more(page).click();
  await entry(page, 'Settings').click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Settings');
  await expect(page.locator('.phone-config-row').first()).toBeVisible();
  expect(errors).toEqual([]);
});

test('Open sessions: a plain list by group; a row opens the agent terminal', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/');
  await booted(page);
  await more(page).click();
  await entry(page, 'Open sessions').click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Open sessions');
  const rows = page.locator('.phone-session');
  await expect(rows.first()).toBeVisible();
  await expect(page.locator('.phone-sessions').getByRole('group', { name: 'webshop' })).toBeVisible();
  await expect(page.locator('#terminals-root .mux-tabbar')).toBeHidden();
  await expect(page.locator('.collab-home')).toHaveCount(0);
  await expectNoSideScroll(page, 'Open sessions');
  await rows.filter({ hasText: 'checkout-ui' }).click();
  const terminal = page.locator('.phone-term[role=dialog]');
  await expect(terminal).toBeVisible();
  await terminal.getByRole('button', { name: 'Back' }).click();
  await expect(terminal).toBeHidden();
  await expect(page.locator('.phone-bar-title')).toHaveText('Open sessions');
  expect(errors).toEqual([]);
});

test('Settings › Notifications is the simple form: no config keys, no Advanced block', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/config/notifications');
  await booted(page);
  const panel = page.locator('[data-cfg-panel="notifications"]');
  await expect(panel.getByText('Desktop notifications', { exact: true })).toBeVisible();
  await expect(panel.locator('.cfg-key').first()).toBeHidden();
  await expect(panel.getByText('Raw transition rules')).toBeHidden();
  await expect(panel.getByText('Notification command')).toBeHidden();
  await expect(panel.getByText('Human messages', { exact: true })).toBeVisible();
  await expectNoSideScroll(page, 'Notifications');
  expect(errors).toEqual([]);
});
