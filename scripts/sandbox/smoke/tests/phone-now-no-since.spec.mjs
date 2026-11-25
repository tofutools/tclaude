// The phone's Now view has no "Since you looked" section (js/phone-now.js),
// even for a viewer whose browser kept a look from before, and it drops
// that stored look. The Agents top bar has no ⏻: power is in ☰ More. The look planted here says every agent was working, so
// a version with the section would list each idle one as finished.
// SHOTS=<dir> saves the page (BEFORE=1 skips the new assertions, for main).
//
//   scripts/sandbox/ui.sh smoke phone-now-no-since
import { test, expect, devices } from '@playwright/test';
import path from 'node:path';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

const KEY = 'tclaude.phone.now.looked';

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

test('Now shows no Since you looked section, and forgets a stored look', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.goto('/');
  await booted(page);
  await expect(page.getByRole('group', { name: /^(Online|Active)$/ })).toBeVisible();
  // A look in which every agent online was working: the one a version with
  // the section stored on this first visit, else one built from the rows.
  // Planted as the next page starts: leaving this one may store its own.
  const planted = await page.evaluate((key) => {
    let look = null;
    try { look = JSON.parse(localStorage.getItem(key) || 'null'); } catch { look = null; }
    const ids = look ? Object.keys(look) : [];
    return JSON.stringify(Object.fromEntries((ids.length ? ids : ['agt_planted']).map((id) => [id, { s: 'working', pr: 0, prs: '', ci: '' }])));
  }, KEY);
  await page.addInitScript(({ key, value }) => {
    if (!sessionStorage.getItem('planted')) { localStorage.setItem(key, value); sessionStorage.setItem('planted', '1'); }
  }, { key: KEY, value: planted });
  await page.reload();
  await booted(page);
  await expect(page.getByRole('group', { name: /^(Online|Active)$/ })).toBeVisible();
  // Scrolled to where the section sat: over the agent list.
  const since = page.getByRole('group', { name: 'Since you looked' });
  const online = page.getByRole('group', { name: /^(Online|Active)$/ });
  await ((await since.count()) ? since : online).evaluate((el) => el.scrollIntoView({ block: 'start' }));
  if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, 'now.png') });
  if (!process.env.BEFORE) {
    await expect(page.getByRole('group', { name: 'Since you looked' })).toHaveCount(0);
    await expect(page.getByText('Mark seen')).toHaveCount(0);
    expect(await page.evaluate((key) => localStorage.getItem(key), KEY)).toBeNull();
  }
  await page.locator('.phone-nav').getByRole('button', { name: /Groups/ }).first().click();
  await expect(page.locator('.phone-bar')).toBeVisible();
  if (process.env.SHOTS) await page.locator('.phone-bar').screenshot({ path: path.join(process.env.SHOTS, 'agents-bar.png') });
  if (!process.env.BEFORE) {
    await expect(page.locator('.phone-bar [aria-label="Power"]')).toHaveCount(0);
    await page.locator('.phone-nav').getByRole('button', { name: /More/ }).first().click();
    await expect(page.locator('.phone-sheet').getByRole('button', { name: /Shut down all/ })).toBeVisible();
  }
  expect(errors).toEqual([]);
});
