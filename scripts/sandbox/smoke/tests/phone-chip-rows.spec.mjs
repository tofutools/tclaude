// Every phone chip row is the same one (phone-ui.js ChipRow, phone.css
// .phone-chips; tk_81b4849c): on Now, Agents, Board, Mail, Access, Usage,
// Automations and Diagnostics the chips sit the same distance under the
// bar, are the same size with the same inset and gap, have the same space
// under the row, and the picked chip scrolled into sight stops inside the
// gutter, not under the screen's edge. Automations' page starts at the
// gutter like every other. SHOTS=<dir> saves the top of each view (BEFORE=1
// skips the assertions, for main).
//
//   scripts/sandbox/ui.sh smoke phone-chip-rows
import path from 'node:path';
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);
test.setTimeout(180_000);

const BEFORE = !!process.env.BEFORE;
const nav = (page) => page.getByRole('navigation', { name: 'Views' });
const tab = (name) => async (page) => { await nav(page).getByRole('button', { name }).click(); };
const more = (name) => async (page) => {
  await nav(page).getByRole('button', { name: /More/ }).click();
  await page.getByRole('dialog', { name: 'More' }).locator('.phone-menu-item')
    .filter({ has: page.locator('.phone-menu-label', { hasText: new RegExp(`^${name}$`) }) }).first().click();
};
const VIEWS = [
  ['Now', tab(/Now/)], ['Agents', tab(/Agents/)], ['Board', tab(/Board/)], ['Mail', tab(/Mail/)],
  ['Access', more('Access')], ['Usage', more('Usage')], ['Automations', more('Automations')], ['Diagnostics', more('Diagnostics')],
];

// measure is the view's first visible chip row, as numbers.
const measure = (page) => page.evaluate(() => {
  const shown = (el) => el.getClientRects().length > 0 && getComputedStyle(el).visibility !== 'hidden';
  // Main's Mail drew its own row (.phone-mail-chips), for the before shot.
  const row = [...document.querySelectorAll('.phone-chips')].find(shown) || [...document.querySelectorAll('.phone-mail-chips')].find(shown);
  if (!row) return null;
  const chips = [...row.querySelectorAll('.ui-tab, .phone-mail-chip')].filter(shown);
  const first = chips[0];
  const picked = row.querySelector('[aria-selected="true"], [aria-pressed="true"]');
  const style = getComputedStyle(first);
  return {
    top: Math.round(first.getBoundingClientRect().top - document.querySelector('.phone-bar').getBoundingClientRect().bottom),
    height: Math.round(first.getBoundingClientRect().height),
    padding: style.paddingLeft,
    font: style.fontSize,
    gap: Math.round(chips[1].getBoundingClientRect().left - first.getBoundingClientRect().right),
    under: getComputedStyle(row).marginBottom,
    pickedLeft: picked ? Math.round(picked.getBoundingClientRect().left) : null,
  };
});

test('one chip row on every view: the same place, size, inset, gap and space under it', async ({ page }) => {
  const seen = {};
  for (const [name, go] of VIEWS) {
    await page.goto('/');
    await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
    await go(page);
    await expect.poll(() => measure(page), { message: `${name} has a chip row` }).not.toBeNull();
    await page.waitForTimeout(700);
    if (process.env.SHOTS) {
      await page.screenshot({ path: path.join(process.env.SHOTS, `${BEFORE ? 'before' : 'after'}-chips-${name}.png`), clip: { x: 0, y: 0, width: 390, height: 260 } });
    }
    seen[name] = await measure(page);
    if (name === 'Automations' && !BEFORE) {
      // The page under the chips starts at the gutter, as every page does.
      const left = await page.locator('.phone-auto-search').first().evaluate((el) => Math.round(el.getBoundingClientRect().left));
      expect(left, 'Automations starts at the gutter').toBe(16);
    }
  }
  if (BEFORE) return;
  for (const [name, m] of Object.entries(seen)) {
    expect({ name, ...m, pickedLeft: undefined }, name).toEqual({ name, ...seen.Now, pickedLeft: undefined });
    if (m.pickedLeft !== null) expect(m.pickedLeft, `${name}: the picked chip is inside the gutter`).toBeGreaterThanOrEqual(16);
  }
  expect(seen.Now).toMatchObject({ top: 12, height: 32, padding: '12px', gap: 8, under: '10px' });
});
