// Ask the steward in the process editor's ✨ With an agent menu
// (get-help-menu.js) in a real browser on the sandbox's data, which has a
// Steward: on the desktop and on a phone's desktop site, the menu lists it
// first, above New helper and Ask an existing agent…. Nothing is asked: the
// draft is closed untouched, so it is not kept. SHOTS=<dir> saves the open
// menu (BEFORE=1 skips the assertions, for a shot on main).
//
//   scripts/sandbox/ui.sh smoke process-help-steward
import { test, expect, devices } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';

async function run(page, prefix, url) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.goto(url);
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
  const section = page.locator(`[data-fused-processes="${GROUP}"]`);
  if (!(await section.isVisible())) await page.locator(`strong[data-group-name="${GROUP}"]`).click();
  await section.locator(`[data-resource-add="${GROUP}"]`).first().click();
  await page.locator('[data-resource-new-process]').click();
  const editor = section.locator('[data-mini-editor]');
  await expect(editor).toBeVisible();
  await editor.locator('.gm-get-help .get-help-toggle').click();
  const menu = editor.locator('.get-help-menu');
  await expect(menu).toBeVisible();
  await menu.scrollIntoViewIfNeeded();
  if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, `${prefix}-menu.png`) });
  if (!process.env.BEFORE) {
    await expect(menu.locator('[data-get-help]')).toHaveText([/^Ask the steward/, 'New helper', 'Ask an existing agent…']);
  }
  await page.keyboard.press('Escape');
  await editor.locator('[data-mini-close]').click();
  await expect(editor).toHaveCount(0);
  expect(errors).toEqual([]);
}

test.describe('desktop', () => {
  test.use({ viewport: { width: 1440, height: 900 } });
  test('With an agent lists Ask the steward first, on the desktop', async ({ page }) => { test.setTimeout(120_000); await run(page, 'desktop', '/groups'); });
});

test.describe('phone', () => {
  const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
  test.use(IPHONE);
  test('With an agent lists Ask the steward first, on a phone\'s desktop site', async ({ page }) => { test.setTimeout(120_000); await run(page, 'phone', '/groups?layout=desktop'); });
});
