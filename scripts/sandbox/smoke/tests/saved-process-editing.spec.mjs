// A group's saved-process row while its process is open in the inline editor
// (js/groups-saved-processes.js): the "editing" mark takes the eye button's
// place and fits its slot, in the normal and the wizard skin.
// SHOTS=<dir> saves editing-row.png and editing-row-wizard.png.
//
//   scripts/sandbox/ui.sh smoke saved-process-editing
import { test, expect } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';
const TEMPLATE = 'smoke-saved-editing';

function api(page, method, url, body) {
  return page.evaluate(async ({ method, url, body }) => {
    const response = await fetch(url, {
      method, credentials: 'same-origin', headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { method, url, body });
}

const template = {
  apiVersion: 'tclaude.dev/v1alpha1', kind: 'ProcessTemplate', id: TEMPLATE, name: 'Release checklist',
  start: 'start',
  nodes: {
    start: { type: 'start', next: { next: 'build' } },
    build: { type: 'task', name: 'Build', performer: { kind: 'program', run: 'make' }, next: { next: 'done' } },
    done: { type: 'end', result: 'success' },
  },
};

test.use({ viewport: { width: 1440, height: 1000 } });

for (const skin of ['', 'wizard']) {
  test(`the editing mark fits the eye button's slot${skin ? ` (${skin})` : ''}`, async ({ page }) => {
    test.setTimeout(120_000);
    const errors = [];
    page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
    await page.goto(skin ? `/groups?${skin}=1` : '/groups');
    await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
    const saved = await api(page, 'POST', `/v1/process/templates/${TEMPLATE}`, { template, pinGroup: GROUP });
    expect(saved.status, JSON.stringify(saved.body)).toBeLessThan(300);
    try {
      const section = page.locator(`[data-fused-processes="${GROUP}"]`);
      if (!(await section.isVisible())) await page.locator(`strong[data-group-name="${GROUP}"]`).click();
      const row = section.locator(`[data-saved-process="${TEMPLATE}"]`);
      await expect(row).toBeVisible({ timeout: 30_000 });
      const eye = row.locator(`[data-saved-edit-graph="${TEMPLATE}"]`);
      const slot = await eye.boundingBox();
      await eye.click();
      const mark = row.locator(`[data-saved-editing="${TEMPLATE}"]`);
      await expect(mark).toBeVisible();
      await row.hover();
      if (process.env.SHOTS) {
        const at = await row.boundingBox();
        await page.screenshot({ path: path.join(process.env.SHOTS, `editing-row${skin ? `-${skin}` : ''}.png`),
          clip: { x: Math.max(0, at.x - 8), y: Math.max(0, at.y - 30), width: Math.min(760, at.width + 16), height: at.height + 60 } });
      }
      await expect(mark).toHaveAttribute('title', 'Editing in the graph below');
      // The mark is the eye button's size, and the buttons after it stay put.
      const box = await mark.boundingBox();
      expect(Math.abs(box.width - slot.width), `mark ${box.width}px wide, eye ${slot.width}px`).toBeLessThanOrEqual(1);
      expect(Math.abs(box.height - slot.height)).toBeLessThanOrEqual(1);
      expect(errors).toEqual([]);
    } finally {
      await api(page, 'DELETE', `/v1/process/templates/${TEMPLATE}`);
    }
  });
}
