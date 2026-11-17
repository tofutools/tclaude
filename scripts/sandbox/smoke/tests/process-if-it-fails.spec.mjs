// The inline process editor's "If it fails" row (groups-mini-editor.js
// FailureRow) in a real browser on the sandbox's data: a new draft from
// Build → review → ship, its Ship step set to retry 2×, then hold it for me.
// The row shows the N× box only for retry, the node wears ↻2, and the draft
// saves retry.maxAttempts 3. SHOTS=<dir> saves the card (BEFORE=1 skips the
// new assertions, for main).
//
//   scripts/sandbox/ui.sh smoke process-if-it-fails
import { test, expect } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';

function api(page, method, url) {
  return page.evaluate(async ({ method, url }) => {
    const response = await fetch(url, { method, credentials: 'same-origin' });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { method, url });
}

test.use({ viewport: { width: 1440, height: 1000 } });

test('If it fails: retry N×, then hold it for me; ↻N on the node', async ({ page }) => {
  test.setTimeout(120_000);
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  let made = '';
  await page.goto('/groups');
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
  const section = page.locator(`[data-fused-processes="${GROUP}"]`);
  if (!(await section.isVisible())) await page.locator(`strong[data-group-name="${GROUP}"]`).click();
  try {
    await section.locator(`[data-resource-add="${GROUP}"]`).first().click();
    await page.locator('[data-resource-new-process]').click();
    const editor = section.locator('[data-mini-editor]');
    await expect(editor).toBeVisible();
    made = await editor.getAttribute('data-mini-editor');
    await editor.locator('[data-mini-builtin="build-review-ship"]').click();
    await editor.locator('[data-process-node="ship"]').click();
    const inspector = editor.locator('[data-mini-inspector="ship"]');
    await expect(inspector).toBeVisible();
    if (!process.env.BEFORE) {
      const row = inspector.locator('[data-mini-failure="ship"]');
      await expect(row.locator('select')).toHaveValue('fail');
      await expect(row.locator('[data-mini-retries]')).toHaveCount(0);
      await row.locator('select').selectOption('retry');
      await expect(row.locator('[data-mini-retries]')).toHaveValue('2');
      await expect(editor.locator('[data-process-node="ship"] [data-process-retries]')).toHaveText('↻2');
    }
    await editor.scrollIntoViewIfNeeded();
    if (process.env.SHOTS) await editor.screenshot({ path: path.join(process.env.SHOTS, 'card.png') });
    if (!process.env.BEFORE) {
      await editor.locator('[data-mini-save]').click();
      await expect(section.locator('[data-mini-editor]')).toHaveCount(0);
      const saved = await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(made)}`);
      expect(JSON.stringify(saved.body)).toMatch(/"retry":\{"maxAttempts":3\}/);
    }
    expect(errors).toEqual([]);
  } finally {
    if (made && (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(made)}`)).status === 200) {
      await api(page, 'DELETE', `/v1/process/templates/${encodeURIComponent(made)}`);
    }
  }
});
