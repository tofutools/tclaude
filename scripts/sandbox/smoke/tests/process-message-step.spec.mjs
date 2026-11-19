// A message step in the inline process editor (tk_9f7b4513) in a real
// browser on the sandbox's data: a new draft from Build → review → ship,
// Ship's Who switched to "a message", the reviewer added beside you, a
// subject and the text typed. The graph draws it with ✉ and its sub line
// says who it tells. SHOTS=<dir> saves inspector.png, graph.png and steps.png (BEFORE=1
// opens Ship's inspector and saves inspector.png only, for main).
//
//   scripts/sandbox/ui.sh smoke process-message-step
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

test('a step becomes a message: who it tells, a subject and its text', async ({ page }) => {
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
    await node('ship').click();
    const inspector = editor.locator('[data-mini-inspector="ship"]');
    await expect(inspector).toBeVisible();
    if (process.env.BEFORE) {
      await shot(editor, 'inspector.png');
      return;
    }
    await inspector.locator('select.gq-who-select').selectOption('message');
    const to = inspector.locator('[data-mini-to="ship"]');
    await expect(to.locator('[data-recipient="operator"]')).toHaveText(/you/);
    await to.locator('select.gq-recipient-add').selectOption('role:reviewer');
    await expect(to.locator('[data-recipient="role:reviewer"]')).toHaveText(/reviewer/);
    await inspector.locator('[data-mini-subject="ship"] input').fill('Shipped: {{feature}}');
    await inspector.locator('[data-mini-text]').fill('{{feature}} is out. Thanks for the review!');
    await inspector.locator('[data-mini-text]').blur();
    await expect(node('ship').locator('.pk-kind-message')).toHaveCount(1);
    await expect(node('ship')).toContainText('✉');
    await expect(node('ship')).toContainText('to you · reviewer');
    await page.mouse.move(0, 0);
    await shot(editor, 'inspector.png');
    await inspector.locator('[data-mini-subject="ship"] input').press('Escape');
    await expect(inspector).toHaveCount(0);
    await shot(editor, 'graph.png');
    // Saved as written: the template holds a message performer.
    await expect.poll(async () => (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(made)}`)).body?.template?.nodes?.ship?.performer?.kind
      ?? (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(made)}`)).body?.nodes?.ship?.performer?.kind, { timeout: 15_000 }).toBe('message');
    // The same draft as a step list: Ship's row picks its recipients as chips.
    await editor.locator('[data-mini-steps]').click();
    const row = section.locator('[data-step="ship"]');
    await expect(row).toHaveAttribute('data-step-kind', 'message');
    await expect(row.locator('[data-recipients="operator role:reviewer"]')).toBeVisible();
    await row.scrollIntoViewIfNeeded();
    await shot(row, 'steps.png');
    expect(errors).toEqual([]);
  } finally {
    if (made && (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(made)}`)).status === 200) {
      await api(page, 'DELETE', `/v1/process/templates/${encodeURIComponent(made)}`);
    }
  }
});
