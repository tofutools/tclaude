// New-process draft hygiene in a real browser on the sandbox's data
// (groups-mini-editor.js, process-step-ids.js): a draft left with no steps
// by navigating away is gone; a name another process of the group has is
// flagged; a never-run draft's step takes its id from its name.
// SHOTS=<dir> saves the card.
//
//   scripts/sandbox/ui.sh smoke process-draft-hygiene
import { test, expect } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';

function api(page, method, url) {
  return page.evaluate(async ({ method, url }) => {
    const response = await fetch(url, { method, credentials: 'same-origin' });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { method, url });
}

// Drafts are not in the library listing: ask for each one.
const exists = async (page, id) => (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(id)}`)).status === 200;

async function openGroups(page) {
  await page.goto('/groups');
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
  const section = page.locator(`[data-fused-processes="${GROUP}"]`);
  if (!(await section.isVisible())) await page.locator(`strong[data-group-name="${GROUP}"]`).click();
  return section;
}

async function newDraft(page, section) {
  await section.locator(`[data-resource-add="${GROUP}"]`).first().click();
  await page.locator('[data-resource-new-process]').click();
  const editor = section.locator('[data-mini-editor]');
  await expect(editor).toBeVisible();
  return { editor, id: await editor.getAttribute('data-mini-editor') };
}

test.use({ viewport: { width: 1440, height: 900 } });

test('a zero-step draft left by navigating away is gone; duplicates flagged; ids follow names', async ({ page }) => {
  test.setTimeout(120_000);
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  const made = [];
  try {
    let section = await openGroups(page);
    // Named, no steps, left by going to another page: dropped.
    const first = await newDraft(page, section);
    made.push(first.id);
    await first.editor.locator('[data-mini-title]').fill('Hygiene check');
    await first.editor.locator('[data-mini-title]').press('Enter');
    await expect.poll(() => exists(page, first.id)).toBe(true);
    await page.goto('/agents');
    await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
    await expect.poll(() => exists(page, first.id), { timeout: 10_000 }).toBe(false);

    // A never-run draft's renamed step takes its id from its name.
    section = await openGroups(page);
    const second = await newDraft(page, section);
    made.push(second.id);
    await second.editor.locator('[data-mini-title]').fill('Hygiene steps');
    await second.editor.locator('[data-mini-title]').press('Enter');
    await second.editor.locator('[data-mini-builtin="build-review-ship"]').click();
    await second.editor.locator('[data-process-node="build"]').click();
    const name = second.editor.locator('[data-mini-inspector="build"] [data-mini-name]');
    await name.fill('Write the code');
    await name.blur();
    await expect(second.editor.locator('[data-process-node="write-the-code"]')).toBeVisible();
    await expect(second.editor.locator('[data-mini-inspector="write-the-code"]')).toBeVisible();
    await second.editor.scrollIntoViewIfNeeded();
    if (process.env.SHOTS) await second.editor.screenshot({ path: path.join(process.env.SHOTS, 'renamed.png') });
    // It has steps: Save keeps it, under the new id.
    await second.editor.locator('[data-mini-save]').click();
    await expect(section.locator('[data-mini-editor]')).toHaveCount(0);
    const saved = await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(second.id)}`);
    expect(JSON.stringify(saved.body)).toContain('"write-the-code"');

    // Its name, typed into another new draft, is flagged; ✕ then drops that one.
    const third = await newDraft(page, section);
    made.push(third.id);
    await third.editor.locator('[data-mini-title]').fill('hygiene steps');
    await expect(third.editor.locator('[data-mini-duplicate]')).toContainText('Hygiene steps');
    if (process.env.SHOTS) await third.editor.screenshot({ path: path.join(process.env.SHOTS, 'duplicate.png') });
    await third.editor.locator('[data-mini-title]').fill('Hygiene other');
    await expect(third.editor.locator('[data-mini-duplicate]')).toHaveCount(0);
    await third.editor.locator('[data-mini-close]').click();
    await expect.poll(() => exists(page, third.id), { timeout: 10_000 }).toBe(false);
    expect(await exists(page, second.id)).toBe(true);
    expect(errors).toEqual([]);
  } finally {
    for (const id of made) if (await exists(page, id)) await api(page, 'DELETE', `/v1/process/templates/${encodeURIComponent(id)}`);
  }
});
