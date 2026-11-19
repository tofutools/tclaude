// Waiting wait steps in the run views (tk_96aee1ac) in a real browser on the
// sandbox's data: a run of webshop with two branches, one waiting three days
// and one waiting until a condition holds, checked every 5 minutes. The
// desktop run focus counts both down with Skip wait and Check now; Check now
// makes the next check; the phone's run card and run sheet say the same.
// SHOTS=<dir> saves desktop.png, phone-card.png and phone-sheet.png.
//
//   scripts/sandbox/ui.sh smoke process-run-waits
import { test, expect, devices } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';
const TEMPLATE = 'smoke-run-waits';
const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];

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
  apiVersion: 'tclaude.dev/v1alpha1', kind: 'ProcessTemplate', id: TEMPLATE, name: 'Launch window',
  start: 'start',
  params: { approved: { type: 'boolean', default: 'false' } },
  nodes: {
    start: { type: 'start', next: { next: 'fork' } },
    fork: { type: 'parallel', next: { cool: 'cool_off', ready: 'store_ready' } },
    cool_off: { type: 'wait', name: 'Cool off', wait: { duration: '3 days' }, next: { next: 'join' } },
    store_ready: {
      type: 'wait', name: 'Store approval',
      wait: { check: { when: 'params.approved == true', every: '5m', backoff: 'exponential', maxEvery: '1h', maxChecks: 20, within: '1d', onLimit: 'timedOut' } },
      next: { met: 'join', timedOut: 'late' },
    },
    join: { type: 'end', join: 'all', result: 'success', name: 'Launched' },
    late: { type: 'end', result: 'failure', name: 'Not approved in time' },
  },
};

async function startRun(page) {
  await api(page, 'DELETE', `/v1/process/templates/${TEMPLATE}`);
  const saved = await api(page, 'POST', `/v1/process/templates/${TEMPLATE}`, { template });
  expect(saved.status, JSON.stringify(saved.body)).toBeLessThan(300);
  const run = await api(page, 'POST', '/v1/process/runs', { templateId: TEMPLATE, group: GROUP, params: { approved: 'false' }, authorizeProgramProfiles: [] });
  expect(run.status, JSON.stringify(run.body)).toBe(201);
  return run.body?.run?.id || run.body?.id;
}

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

const shot = async (target, name) => { if (process.env.SHOTS) await target.screenshot({ path: path.join(process.env.SHOTS, name) }); };

test.describe('desktop', () => {
  test.use({ viewport: { width: 1440, height: 1000 } });
  test('a run counts its waits down, and Check now makes the next check', async ({ page }) => {
    test.setTimeout(120_000);
    const errors = [];
    page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
    await page.goto('/groups');
    await booted(page);
    const runId = await startRun(page);
    try {
      await page.goto(`/groups/runs/${GROUP}/${runId}`);
      await booted(page);
      const cool = page.locator('[data-run-wait="cool_off"]');
      const ready = page.locator('[data-run-wait="store_ready"]');
      await expect(cool.locator('[data-run-wait-status]')).toContainText(/2d 23h left · until /, { timeout: 30_000 });
      await expect(ready.locator('[data-run-wait-status]')).toContainText(/check 2 of 20 in 4m \d\ds · last: not yet · times out /);
      await expect(cool.locator('[data-run-wait-check-now]')).toHaveCount(0);
      await expect(cool.locator('[data-run-wait-skip]')).toBeVisible();
      // Check now: the second check is made, and the third waits 10 minutes (backing off ×2).
      await ready.locator('[data-run-wait-check-now]').click();
      await expect(ready.locator('[data-run-wait-status]')).toContainText(/check 3 of 20 in (9m|10m)/, { timeout: 30_000 });
      await page.mouse.move(0, 0);
      await shot(page, 'desktop.png');
      expect(errors).toEqual([]);
    } finally {
      await api(page, 'POST', `/v1/process/runs/${runId}/cancel`, {});
    }
  });
});

test.describe('phone', () => {
  test.use(IPHONE);
  test('the run card and run sheet say how long each wait has left', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto('/groups');
    await booted(page);
    const runId = await startRun(page);
    try {
      await page.goto('/automations/processes');
      await booted(page);
      const card = page.locator(`[data-run="${runId}"]`);
      await expect(card).toBeVisible({ timeout: 30_000 });
      const lines = card.locator('[data-run-wait-line]');
      await expect(lines.filter({ hasText: /^⏳ Cool off: 2d 23h left$/ })).toHaveCount(1);
      await expect(lines.filter({ hasText: /^↻ Store approval: check 2\/20, next in 4m \d\ds$/ })).toHaveCount(1);
      await card.scrollIntoViewIfNeeded();
      await shot(card, 'phone-card.png');
      await card.locator('.phone-run-open').click();
      const ready = page.locator('[data-run-wait="store_ready"]');
      await expect(ready).toBeVisible({ timeout: 30_000 });
      await ready.scrollIntoViewIfNeeded();
      await shot(page, 'phone-sheet.png');
    } finally {
      await api(page, 'POST', `/v1/process/runs/${runId}/cancel`, {});
      await api(page, 'DELETE', `/v1/process/templates/${TEMPLATE}`);
    }
  });
});
