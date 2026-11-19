// A wait step in both process editors (tk_96aee1ac) in a real browser on the
// sandbox's data: a new draft from Build → review → ship gets a wait after
// Build in the inline editor, first for a time ("3 days"), then until a
// condition checked every 5 minutes with a timed-out route and backoff under
// "more". The same draft in the full editor shows the wait's inspector.
// SHOTS=<dir> saves mini-time.png, mini-check.png, full-time.png and full-check.png.
//
//   scripts/sandbox/ui.sh smoke process-wait-step
import { test, expect } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';

function api(page, method, url) {
  return page.evaluate(async ({ method, url }) => {
    const response = await fetch(url, { method, credentials: 'same-origin' });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { method, url });
}

async function savedWait(page, id) {
  const body = (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(id)}`)).body;
  return (body?.template?.nodes || body?.nodes || {}).wait?.wait;
}

test.use({ viewport: { width: 1440, height: 1000 } });

test('a wait step: for a time, then until a condition, in both editors', async ({ page }) => {
  test.setTimeout(180_000);
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
    // + after Build → ⏳ wait.
    await node('build').hover();
    await editor.locator('[data-mini-handles="build"] [data-mini-handle="after"]').click();
    await editor.locator('[data-mini-picker="after"] [data-mini-kind="wait"]').click();
    await expect(node('wait')).toBeVisible();
    // A new wait opens selected.
    const inspector = editor.locator('[data-mini-inspector="wait"]');
    await expect(inspector).toBeVisible();
    // For a time.
    const duration = inspector.locator('[data-mini-wait="duration"]');
    await duration.fill('3 days');
    await duration.blur();
    await expect(inspector.locator('.gm-wait-feedback')).toHaveText('= 3 days');
    await expect(node('wait')).toContainText('3 days');
    await page.mouse.move(0, 0);
    await shot(editor, 'mini-time.png');
    // Until a condition.
    await inspector.locator('[data-mini-wait-mode="check"]').click();
    const when = inspector.locator('[data-mini-wait="when"]');
    await when.fill('steps.build.result == succeeded');
    await when.blur();
    await inspector.locator('[data-mini-wait="maxChecks"]').fill('20');
    await inspector.locator('[data-mini-wait="maxChecks"]').blur();
    await inspector.locator('[data-mini-wait="within"]').fill('1d');
    await inspector.locator('[data-mini-wait="within"]').blur();
    await inspector.locator('[data-mini-wait="onLimit"]').selectOption('timedOut');
    await inspector.locator('[data-mini-wait-more]').click();
    await inspector.locator('[data-mini-wait="backoff"]').selectOption('exponential');
    await inspector.locator('[data-mini-wait="maxEvery"]').fill('1h');
    await inspector.locator('[data-mini-wait="maxEvery"]').blur();
    await expect(inspector.locator('[data-mini-outcome="timedOut"]')).toBeVisible();
    await expect(inspector.locator('[data-mini-wait-summary]')).toContainText('backing off ×2 up to 1 hour');
    await page.mouse.move(0, 0);
    await shot(editor, 'mini-check.png');
    // Saved as written.
    await expect.poll(async () => (await savedWait(page, made))?.check?.maxEvery, { timeout: 15_000 }).toBe('1h');
    const saved = await savedWait(page, made);
    expect(saved.check).toMatchObject({ when: 'steps.build.result == succeeded', every: '5m', maxChecks: 20, within: '1d', onLimit: 'timedOut', backoff: 'exponential' });
    // The full editor: the same wait's inspector.
    await inspector.locator('[data-mini-more]').click();
    const full = page.locator('[data-node-inspector]').first();
    const canvasNode = page.locator('.process-overlay-slot [data-node-id="wait"]').first();
    await expect(canvasNode).toBeVisible({ timeout: 30_000 });
    // The library rail beside the overlay is folded away for the pictures.
    const hide = page.getByText('Hide ›', { exact: true });
    if (await hide.isVisible()) await hide.click();
    await canvasNode.click({ timeout: 10_000 });
    await expect(page.locator('[data-node-inspector="wait"]')).toBeVisible();
    await expect(full.locator('[data-wait-summary]')).toContainText('until steps.build.result == succeeded');
    await expect(full.locator('[data-wait-more-fields]')).toBeVisible();
    // The routes the inline editor made are valid where the wait sits (in a loop).
    await expect(page.locator('.process-overlay').first().getByText(/Issues · \d+ error/)).toHaveCount(0);
    await page.mouse.move(0, 0);
    if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, 'full-check.png') });
    await full.locator('[data-wait-mode="time"]').click();
    const fullDuration = full.locator('[data-node-field="wait.duration"]');
    await fullDuration.fill('90m');
    await fullDuration.blur();
    await expect(full.locator('[data-wait-feedback="wait.duration"]')).toHaveText('= 1 hour 30 minutes');
    // A wait for a time goes on one way: its timed-out route went with the condition.
    await expect(full.locator('.process-node-edge-outcome')).toHaveText(['next']);
    await expect(page.locator('.process-overlay').first().getByText(/Issues · \d+ error/)).toHaveCount(0);
    await page.mouse.move(0, 0);
    if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, 'full-time.png') });
    expect(errors).toEqual([]);
  } finally {
    if (made && (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(made)}`)).status === 200) {
      await api(page, 'DELETE', `/v1/process/templates/${encodeURIComponent(made)}`);
    }
  }
});
