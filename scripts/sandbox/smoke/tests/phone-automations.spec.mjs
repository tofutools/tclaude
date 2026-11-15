// Automations on a phone (js/phone-automations-island.js and
// js/phone-process-runs.js) in a real browser: a cron job's switch turns it
// off and on, ▶ asks before it fires, a job opens as its own page, and the
// seeded release-train run waits on the operator both on Now and under Runs,
// where approving it moves the run on. Nothing scrolls the page sideways.
//
//   scripts/sandbox/ui.sh smoke phone-automations
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

const chips = (page) => page.locator('.phone-auto-chips:visible').getByRole('tablist', { name: 'Automation views' });

test('cron jobs switch, run behind a confirm, and open as a page', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/automations/cron-jobs');
  await booted(page);
  const row = page.locator('.phone-auto-row:visible', { hasText: 'standup-ping' });
  await expect(row).toBeVisible();
  await expectNoSideScroll(page, 'the cron list');

  const toggle = row.getByRole('switch');
  await expect(toggle).toBeChecked();
  // The switch moves at once; wait for the server's answer before reloading,
  // so the reload proves it was stored.
  const stored = page.waitForResponse((response) => /\/api\/cron\/[^/]+\/disable/.test(response.url()) && response.ok());
  await toggle.click();
  await expect(toggle).not.toBeChecked();
  await stored;
  await page.reload();
  await booted(page);
  await expect(page.locator('.phone-auto-row:visible', { hasText: 'standup-ping' }).getByRole('switch')).not.toBeChecked();
  await page.locator('.phone-auto-row:visible', { hasText: 'standup-ping' }).getByRole('switch').click();
  await expect(page.locator('.phone-auto-row:visible', { hasText: 'standup-ping' }).getByRole('switch')).toBeChecked();

  await page.locator('.phone-auto-row:visible', { hasText: 'morning-plan' }).getByRole('button', { name: /Run morning-plan now/ }).click();
  const confirm = page.getByRole('dialog');
  await expect(confirm).toContainText('Fire this cron job now?');
  await confirm.getByRole('button', { name: 'Cancel' }).click();
  await expect(confirm).toHaveCount(0);

  await page.locator('.phone-auto-row:visible', { hasText: 'morning-plan' }).locator('.phone-auto-open').click();
  await expect(page.locator('.phone-bar-title')).toHaveText('morning-plan');
  await expect(page.locator('.phone-auto-facts')).toContainText('Schedule');
  await expect(page.locator('.phone-auto-page')).toContainText('Recent runs');
  await expectNoSideScroll(page, 'the cron page');
  await page.locator('.phone-bar-back').click();
  await expect(page.locator('.phone-auto-row:visible', { hasText: 'morning-plan' })).toBeVisible();
  expect(errors).toEqual([]);
});

test('a run waiting on the operator shows on Now and is approved from Runs', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/');
  await booted(page);
  const gate = page.locator('.phone-gate-card', { hasText: 'Release train' });
  await expect(gate).toBeVisible();
  await expect(gate.getByRole('button', { name: /approve/i })).toBeVisible();

  await page.goto('/automations/processes');
  await booted(page);
  await expect(chips(page).getByRole('tab', { name: /Runs/ })).toHaveAttribute('aria-selected', 'true');
  const run = page.getByRole('region', { name: 'Waiting on you' }).locator('.phone-run-open', { hasText: 'Release train' });
  await expect(run).toBeVisible();
  await expect(page.locator('.phone-desktop-only:visible')).toContainText('desktop only');
  await expectNoSideScroll(page, 'the runs list');

  await run.click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Release train');
  // The gate is Now's card, amber, with how long it has waited.
  await expect(page.locator('.phone-run-gate.phone-box-key')).toContainText('Waiting on you');
  await expect(page.locator('.phone-run-step.mine')).toContainText('Checks pass?');
  await expectNoSideScroll(page, 'the run page');
  await page.locator('.phone-run-page').getByRole('button', { name: /approve/i }).click();
  // The verdict lets the run go on: the gate is done and no longer yours.
  await expect(page.locator('.phone-run-step', { hasText: 'Checks pass?' })).toHaveClass(/s-done/, { timeout: 15_000 });
  await expect(page.locator('.phone-run-step.mine')).toHaveCount(0);
  await expect(page.locator('.phone-run-page .phone-run-gate')).toHaveCount(0);
  expect(errors).toEqual([]);
});

// chipInSight reports whether a chip lies wholly inside its row's visible
// part (the row scrolls sideways).
async function chipInSight(chip) {
  return chip.evaluate((el) => {
    const row = el.closest('.phone-chips').getBoundingClientRect();
    const box = el.getBoundingClientRect();
    return box.left >= row.left - 1 && box.right <= row.right + 1;
  });
}

test('the Runs chip stays Automations, picked and in sight (B7)', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/automations');
  await booted(page);
  await expect(page.locator('.phone-bar-title')).toHaveText('Automations');
  await chips(page).getByRole('tab', { name: /Runs/ }).click();
  const runs = chips(page).getByRole('tab', { name: /Runs/ });
  await expect(runs).toHaveAttribute('aria-selected', 'true');
  await expect(page.locator('.phone-bar-title')).toHaveText('Automations');
  await expect.poll(() => chipInSight(runs)).toBe(true);
  // The same on a deep link.
  await page.goto('/automations/processes/templates');
  await booted(page);
  await expect(page.locator('.phone-bar-title')).toHaveText('Automations');
  await expect(runs).toHaveAttribute('aria-selected', 'true');
  await expect.poll(() => chipInSight(runs)).toBe(true);
  expect(errors).toEqual([]);
});
