// ＋ Agent on the phone's agent headers (js/phone-agents-island.js,
// js/phone-now.js) in a real browser: each group header on Agents has it
// before Board ›, and it opens the short spawn sheet on that group; Now's
// Active head has it before All groups ›, opening the sheet with no group
// picked (the first).
//
//   scripts/sandbox/ui.sh smoke phone-agent-add
//
// With SHOTS_DIR set it saves the views for the before/after composite.
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

async function shot(page, name) {
  if (process.env.SHOTS_DIR) await page.screenshot({ path: `${process.env.SHOTS_DIR}/${name}.png` });
}

test('＋ Agent on Agents\' group headers and Now\'s Active head opens the spawn sheet', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.goto('/');
  await booted(page);
  // Now's Active head, with the Shells head under it for comparison.
  await page.evaluate(() => {
    const head = document.querySelector('.phone-section[aria-label="Active"], .phone-section[aria-label="Online"]');
    head.scrollIntoView({ block: 'start' });
    // Clear of the top bar.
    let box = head.parentElement;
    while (box && box.scrollHeight <= box.clientHeight) box = box.parentElement;
    if (box) box.scrollTop -= 70; else window.scrollBy(0, -70);
  });
  await shot(page, 'now');
  const nav = page.getByRole('navigation', { name: 'Views' });
  await nav.getByRole('button', { name: /Groups/ }).click();
  await expect(page.locator('.phone-agents-head').first()).toBeVisible();
  await shot(page, 'agents');
  if (process.env.SHOTS_ONLY) return;

  const head = page.locator('.phone-agents-head', { has: page.locator('[data-agent-add]') }).first();
  const group = (await head.locator('.phone-agents-name').textContent()).trim();
  const add = head.getByRole('button', { name: `New agent in ${group}` });
  await expect(add).toHaveText('＋ Agent');
  await expect(add.locator('xpath=following-sibling::button[1]')).toHaveText('Board ›');
  await add.click();
  const sheet = page.locator('.phone-sheet[role=dialog]');
  await expect(sheet).toHaveAttribute('aria-label', 'New agent');
  await expect(sheet.locator('select').first()).toHaveValue(group);
  await shot(page, 'agents-sheet');
  await page.keyboard.press('Escape');
  await expect(sheet).toBeHidden();

  await nav.getByRole('button', { name: /Now/ }).click();
  const nowAdd = page.locator('.phone-section[aria-label="Active"], .phone-section[aria-label="Online"]').locator('[data-agent-add]');
  await expect(nowAdd).toHaveText('＋ Agent');
  await nowAdd.click();
  await expect(sheet).toHaveAttribute('aria-label', 'New agent');
  await page.keyboard.press('Escape');
  expect(errors).toEqual([]);
});
