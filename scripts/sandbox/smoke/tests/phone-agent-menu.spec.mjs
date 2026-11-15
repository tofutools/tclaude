// The agent menu a touch and hold opens on a phone (js/phone-hold.js,
// js/phone-agent-menu.js), in a real browser on the sandbox's data: a hold on
// a Now row opens it without opening the terminal too, it scrolls to Retire on
// an iPhone SE with the desktop's confirm behind it, and Grant sudo… lands on
// that agent's Grant page.
//
//   scripts/sandbox/ui.sh smoke phone-agent-menu
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

// hold puts a finger on element's middle until the menu is up, then lifts
// it: a real touch (Playwright's touchscreen only taps).
async function hold(page, element) {
  // In the middle of the screen: low on an SE it would sit under the nav.
  await element.evaluate((el) => el.scrollIntoView({ block: 'center' }));
  const box = await element.boundingBox();
  const at = [{ x: box.x + box.width / 2, y: box.y + box.height / 2 }];
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: at });
  await expect(page.locator('.phone-sheet[role=dialog]')).toBeVisible();
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}

test('a hold on a Now row opens its menu, not its terminal; on an SE it scrolls to Retire', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.setViewportSize({ width: 375, height: 667 });
  await page.goto('/');
  await booted(page);
  const row = page.getByRole('group', { name: /^(Online|Active)$/ }).locator('.phone-row').first();
  const name = (await row.locator('.phone-who-name').textContent()).trim();
  await hold(page, row);
  const sheet = page.locator('.phone-sheet[role=dialog]');
  await expect(sheet).toHaveAttribute('aria-label', name);
  await expect(sheet.locator('.phone-agent-tile')).toHaveCount(4);
  await expect(page.locator('.phone-term[role=dialog]')).toHaveCount(0);
  await page.waitForTimeout(300); // the lift's click, if the browser makes one, has landed
  await expect(sheet, 'the lift neither closed it nor opened the terminal').toBeVisible();
  await expect(page.locator('.phone-term[role=dialog]')).toHaveCount(0);

  const retire = sheet.getByRole('button', { name: /^Retire…/ });
  await retire.scrollIntoViewIfNeeded();
  await expect(retire).toBeInViewport();
  await retire.click();
  const confirm = page.getByRole('dialog', { name: 'Retire this agent?' });
  await expect(confirm).toBeVisible();
  await confirm.getByRole('button', { name: 'Cancel' }).click();
  await expect(confirm).toBeHidden();

  // A browser that hit-tests the lift where it lands puts its click on the
  // scrim the menu just opened under the finger: it is swallowed.
  await row.evaluate((el) => el.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, pointerId: 9, button: 0, clientX: 1, clientY: 1 })));
  await expect(sheet).toBeVisible();
  await page.locator('.phone-sheet-scrim').evaluate((el) => el.click());
  await expect(sheet).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(sheet).toBeHidden();

  // ⋯ opens the same menu; Grant sudo… is that agent's Grant page.
  await page.getByRole('button', { name: `${name}: more` }).first().click();
  await sheet.getByRole('button', { name: 'Grant sudo…' }).click();
  await expect(page.locator('.phone-bar-title')).toHaveText(`Grant to ${name}`);
  expect(errors).toEqual([]);
});

test('Grant sudo… from a terminal\'s ⋯ closes the terminal and shows the Grant page', async ({ page }) => {
  await page.goto('/');
  await booted(page);
  const row = page.getByRole('group', { name: /^(Online|Active)$/ }).locator('.phone-row').first();
  const name = (await row.locator('.phone-who-name').textContent()).trim();
  await row.click();
  const term = page.locator('.phone-term[role=dialog]');
  await expect(term).toBeVisible();
  await term.getByRole('button', { name: `${name}: more` }).click();
  await page.locator('.phone-sheet[role=dialog]').getByRole('button', { name: 'Grant sudo…' }).click();
  await expect(term).toBeHidden();
  await expect(page.locator('.phone-bar-title')).toHaveText(`Grant to ${name}`);
});
