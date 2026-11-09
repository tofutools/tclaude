// The local node's auto-update settings in the Fleet node update dialog
// (js/node-update-settings.js), against the real daemon's
// /api/node/update/settings: the block loads, an edit enables Save, Save
// writes updates.self and the block comes back clean. No page errors.
//
//   scripts/sandbox/ui.sh smoke node-update-settings
import { test, expect } from '@playwright/test';

let pageErrors = [];
test.beforeEach(({ page }) => {
  pageErrors = [];
  page.on('pageerror', (err) => pageErrors.push(err.message));
});
test.afterEach(() => {
  expect(pageErrors, 'page errors').toEqual([]);
});

test('the local node update dialog edits and saves its auto-update settings', async ({ page }) => {
  await page.goto('/fleet-admin/setup');
  await page.locator('#fleet-node-update-open').click();
  const block = page.locator('#fleet-node-update-settings');
  await expect(block.getByText('Auto-update settings')).toBeVisible();
  const save = page.locator('#fleet-nus-save');
  await expect(save).toBeDisabled();

  await page.locator('#fleet-nus-frequency').selectOption('daily');
  await page.locator('#fleet-nus-on-start').check();
  await expect(save).toBeEnabled();
  await expect(page.locator('#fleet-nus-unchosen')).toBeVisible();
  await save.click();
  await expect(save).toBeDisabled();
  await expect(page.locator('#fleet-node-update-settings-error')).toHaveCount(0);

  // The saved settings are what the daemon now reports.
  const view = await page.evaluate(async () => (await fetch('/api/node/update/settings', { credentials: 'same-origin' })).json());
  expect(view.settings.schedule.frequency).toBe('daily');
  expect(view.settings.update_on_start).toBe(true);
});
