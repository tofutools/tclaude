// Selecting steps in the inline process editor and deleting them
// (groups-mini-editor.js, tk_d90d2b7a) in a real browser on the sandbox's
// data: a new draft from Build → review → ship, a box dragged on the empty
// canvas selects Build and Review, shift-click adds Ship, Delete
// asks first, and Remove takes them out as one undoable edit.
// SHOTS=<dir> saves box.png, picked.png, dialog.png and after.png
// (BEFORE=1 shift-clicks and saves picked.png only, for main).
//
//   scripts/sandbox/ui.sh smoke process-select-delete
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

test('box and shift-click select steps; Delete asks, then removes them', async ({ page }) => {
  test.setTimeout(120_000);
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  const shot = async (locator, name) => { if (process.env.SHOTS) await locator.screenshot({ path: path.join(process.env.SHOTS, name) }); };
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
    await editor.scrollIntoViewIfNeeded();
    const node = (id) => editor.locator(`[data-process-node="${id}"]`);
    const picked = () => editor.locator('.pk-gnode.selected');
    if (process.env.BEFORE) {
      await node('build').click();
      await node('ship').click({ modifiers: ['Shift'] });
      await page.mouse.move(0, 0);
      await shot(editor, 'picked.png');
      return;
    }
    // A box from the empty canvas above Build to past Review.
    const canvas = await editor.locator('[data-mini-canvas]').boundingBox();
    const build = await node('build').boundingBox({ timeout: 10_000 });
    const review = await node('review').boundingBox({ timeout: 10_000 });
    await page.mouse.move(build.x - 12, canvas.y + 6);
    await page.mouse.down();
    await page.mouse.move(review.x + review.width + 10, review.y + review.height + 8, { steps: 6 });
    await expect(editor.locator('[data-mini-marquee]')).toBeVisible();
    await shot(editor, 'box.png');
    await page.mouse.up();
    await expect(editor.locator('[data-mini-marquee]')).toHaveCount(0);
    await expect(node('build')).toHaveClass(/selected/);
    await expect(node('review')).toHaveClass(/selected/);
    await node('ship').click({ modifiers: ['Shift'] });
    await expect(picked()).toHaveCount(3);
    await expect(editor.locator('[data-mini-inspector]')).toHaveCount(0);
    await page.mouse.move(0, 0);
    await shot(editor, 'picked.png');

    await page.keyboard.press('Delete');
    const dialog = page.locator('#confirm-modal');
    await expect(page.locator('#confirm-title')).toHaveText(/^Remove 3 steps: /);
    await expect(page.locator('#confirm-body')).toContainText('stays connected');
    if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, 'dialog.png') });
    await page.locator('#confirm-ok').click();
    await expect(dialog).not.toHaveClass(/show/);
    await expect(node('build')).toHaveCount(0);
    await expect(node('review')).toHaveCount(0);
    await expect(node('ship')).toHaveCount(0);
    await expect(node('start')).toHaveCount(1);
    await shot(editor, 'after.png');
    await editor.locator('[data-mini-undo]').click();
    await expect(node('build')).toHaveCount(1);
    await expect(node('ship')).toHaveCount(1);
    expect(errors).toEqual([]);
  } finally {
    if (made && (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(made)}`)).status === 200) {
      await api(page, 'DELETE', `/v1/process/templates/${encodeURIComponent(made)}`);
    }
  }
});
