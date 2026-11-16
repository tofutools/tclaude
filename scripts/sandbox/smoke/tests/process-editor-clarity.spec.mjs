// The inline process editor's problems line and gate caption
// (groups-mini-editor.js, process-kit.js) in a real browser on the
// sandbox's data. A new draft from Build → review → ship gets a second
// decider on its review (a gate), and build loses its instruction: the
// problems line names Build it and a click selects it, build is
// outlined, and the gate says "2 deciders · all …" under its diamond.
// SHOTS=<dir> saves the card (BEFORE=1 skips the new assertions, for main).
//
//   scripts/sandbox/ui.sh smoke process-editor-clarity
import { test, expect } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';

function api(page, method, url) {
  return page.evaluate(async ({ method, url }) => {
    const response = await fetch(url, { method, credentials: 'same-origin' });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { method, url });
}

test.use({ viewport: { width: 1440, height: 900 } });

test('the problems line names and selects its step; a gate says who decides', async ({ page }) => {
  test.setTimeout(120_000);
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  const made = [];
  await page.goto('/groups');
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
  const section = page.locator(`[data-fused-processes="${GROUP}"]`);
  if (!(await section.isVisible())) await page.locator(`strong[data-group-name="${GROUP}"]`).click();
  try {
    await section.locator(`[data-resource-add="${GROUP}"]`).first().click();
    await page.locator('[data-resource-new-process]').click();
    const editor = section.locator('[data-mini-editor]');
    await expect(editor).toBeVisible();
    made.push(await editor.getAttribute('data-mini-editor'));
    await editor.locator('[data-mini-builtin="build-review-ship"]').click();
    await expect(editor.locator('[data-process-node="review"]')).toBeVisible();
    // A second decider makes the review a gate.
    await editor.locator('[data-process-node="review"]').click();
    await editor.locator('[data-mini-inspector="review"] [data-mini-add-decider]').click();
    await editor.locator('[data-mini-inspector="review"] [aria-label="Close"], [data-mini-inspector="review"] .gm-row .gq-link:has-text("done")').first().click();
    // build without an instruction is a problem.
    await editor.locator('[data-process-node="build"]').click();
    const text = editor.locator('[data-mini-inspector="build"] [data-mini-text]');
    await text.fill('');
    await text.blur();
    await editor.locator('[data-mini-inspector="build"] .gq-link:has-text("done")').click();
    await expect(editor.locator('[data-mini-problems]')).toBeVisible({ timeout: 10_000 });
    await editor.scrollIntoViewIfNeeded();
    if (process.env.SHOTS) await editor.screenshot({ path: path.join(process.env.SHOTS, 'card.png') });
    if (!process.env.BEFORE) {
      await expect(editor.locator('[data-process-node="review"] .pk-gnode-caption')).toHaveText(/^2 deciders · all /);
      await expect(editor.locator('.pk-gnode.problem')).toHaveCount(1);
      await expect(editor.locator('[data-process-node="build"]')).toHaveClass(/problem/);
      await editor.locator('[data-mini-problem-step="build"]').click();
      await expect(editor.locator('[data-mini-inspector="build"]')).toBeVisible();
      if (process.env.SHOTS) await editor.screenshot({ path: path.join(process.env.SHOTS, 'card-selected.png') });
    }
    expect(errors).toEqual([]);
  } finally {
    const left = ((await api(page, 'GET', '/v1/process/templates')).body?.templates || []).map((row) => row.id);
    for (const id of made.filter((one) => left.includes(one))) await api(page, 'DELETE', `/v1/process/templates/${encodeURIComponent(id)}`);
  }
});
