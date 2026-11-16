// Who › a new agent… with its spawn profile (groups-mini-editor.js,
// groups-quickstart.js) in a real browser on the sandbox's data, on the
// desktop and on a phone's desktop site. A new draft's first agent step
// picks "a new agent…" and a profile beside it: the graph card says
// "a new agent · <profile>", If nobody steps aside, and the saved template
// has the spawn fallback. The Steps view's If nobody line picks a spawn and
// its profile the same way. SHOTS=<dir> saves the views (SHOTS_ONLY=1 stops
// after the first, for a before shot on main).
//
//   scripts/sandbox/ui.sh smoke process-spawn-profile
import { test, expect, devices } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';

function api(page, method, url) {
  return page.evaluate(async ({ method, url }) => {
    const response = await fetch(url, { method, credentials: 'same-origin' });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { method, url });
}

async function shot(page, name) {
  if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, `${name}.png`) });
}

async function run(page, prefix, url) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  const made = [];
  await page.goto(url);
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
  const section = page.locator(`[data-fused-processes="${GROUP}"]`);
  if (!(await section.isVisible())) await page.locator(`strong[data-group-name="${GROUP}"]`).click();
  await expect(section).toBeVisible();
  try {
    await section.locator(`[data-resource-add="${GROUP}"]`).first().click();
    await page.locator('[data-resource-new-process]').click();
    const editor = section.locator('[data-mini-editor]');
    await expect(editor).toBeVisible();
    const id = await editor.getAttribute('data-mini-editor');
    made.push(id);
    await page.keyboard.type('Spawned review');
    await page.keyboard.press('Enter');
    await editor.locator('[data-process-node="mini-first-step"]').click();
    await page.locator('[data-mini-picker] [data-mini-kind="agent"]').click();
    const inspector = editor.locator('[data-mini-inspector]');
    await expect(inspector).toBeVisible();
    await inspector.scrollIntoViewIfNeeded();
    await shot(page, `${prefix}-inspector`);
    if (process.env.SHOTS_ONLY) return;

    const profiles = ((await api(page, 'GET', '/api/spawn-profiles')).body || []).map((p) => p.name).filter(Boolean);
    await inspector.locator('.gq-who-select').selectOption('spawn');
    const profile = inspector.locator('.gq-profile-select');
    await expect(profile).toBeVisible();
    await expect(inspector.locator('[data-mini-fallback]')).toHaveCount(0);
    const pick = profiles.at(-1) || '';
    if (pick) await profile.selectOption(pick);
    const node = editor.locator('[data-process-node]').filter({ hasText: 'a new agent ·' });
    await expect(node).toHaveCount(1);
    await expect.poll(async () => (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(id)}`)).body?.template?.nodes,
      { timeout: 10_000 }).toEqual(expect.objectContaining({}));
    await expect.poll(async () => {
      const nodes = (await api(page, 'GET', `/v1/process/templates/${encodeURIComponent(id)}`)).body?.template?.nodes || {};
      return Object.values(nodes).find((n) => n.performer?.kind === 'agent')?.performer?.fallback;
    }, { timeout: 10_000 }).toEqual(pick ? { action: 'spawn', profile: pick } : { action: 'spawn' });
    await inspector.scrollIntoViewIfNeeded();
    await shot(page, `${prefix}-new-agent`);

    // The Steps view: a new agent's row, and the process's If nobody.
    await editor.locator('[data-mini-steps]').click();
    const list = section.locator(`[data-step-editor-saved="${id}"]`);
    await expect(list).toBeVisible();
    await expect(list.locator('.gq-profile-select')).toHaveCount(1);
    await list.locator('[data-add-step="agent"]').click();
    await list.locator('.gq-step').nth(1).locator('.gq-who-select').selectOption('anyone');
    await list.locator('[data-process-fallback] .gq-fallback-select').selectOption('spawn');
    await expect(list.locator('[data-process-fallback] .gq-profile-select')).toBeVisible();
    await list.scrollIntoViewIfNeeded();
    await shot(page, `${prefix}-steps`);
    expect(errors).toEqual([]);
  } finally {
    const left = ((await api(page, 'GET', '/v1/process/templates')).body?.templates || []).map((row) => row.id);
    for (const id of made.filter((one) => left.includes(one))) await api(page, 'DELETE', `/v1/process/templates/${encodeURIComponent(id)}`);
  }
}

test.describe('desktop', () => {
  test.use({ viewport: { width: 1440, height: 900 } });
  test('Who › a new agent picks its profile, on the desktop', async ({ page }) => { test.setTimeout(120_000); await run(page, 'desktop', '/groups'); });
});

test.describe('phone', () => {
  const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
  test.use(IPHONE);
  test('Who › a new agent picks its profile, on a phone\'s desktop site', async ({ page }) => { test.setTimeout(120_000); await run(page, 'phone', '/groups?layout=desktop'); });
});
