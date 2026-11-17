// The phone's sheets share their parts (phone-ui.js SheetTitle and
// ButtonRow, phone-draft-guard.js DiscardAsk; tk_81b4849c): every sheet's
// title is the same size and weight, and their button rows are ButtonRow's.
// Opens each sheet that had its own title or row: an agent's menu, a
// decision's ⋯, a new agent, Agents' View, a message (and Discard? for it),
// Mailboxes, a gate's note, Move and Comment on a board card, and Update.
// SHOTS=<dir> saves each (BEFORE=1 skips the assertions, for main).
//
//   scripts/sandbox/ui.sh smoke phone-sheet-parts
import path from 'node:path';
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);
test.setTimeout(240_000);

const BEFORE = !!process.env.BEFORE;
const nav = (page) => page.getByRole('navigation', { name: 'Views' });
const sheet = (page) => page.locator('.phone-sheet').last();
const closeSheet = async (page) => { await page.keyboard.press('Escape'); await expect(page.locator('.phone-sheet')).toHaveCount(0); };
async function home(page) {
  await page.goto('/');
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

// The sheets, each opened from Now (or a tab) the way the operator does.
const SHEETS = [
  ['agent-menu', async (page) => { await page.locator('.phone-now-more').first().click(); }],
  ['decision', async (page) => { await page.getByRole('button', { name: 'All options, a note, or dismiss' }).first().click(); }],
  ['gate-note', async (page) => { await page.locator('[data-run-verdict]').filter({ hasText: '…' }).first().click(); }],
  ['message', async (page) => {
    await page.locator('.phone-now-more').first().click();
    await sheet(page).getByRole('button', { name: /Message/ }).first().click();
  }],
  ['new-agent', async (page) => { await nav(page).getByRole('button', { name: /Agents/ }).click(); await page.getByRole('button', { name: /^New agent in / }).first().click(); }],
  ['agents-view', async (page) => { await nav(page).getByRole('button', { name: /Agents/ }).click(); await page.locator('.phone-bar').getByRole('button', { name: /View/ }).click(); }],
  ['mailboxes', async (page) => { await nav(page).getByRole('button', { name: /Mail/ }).click(); await page.locator('.phone-bar-title-btn').click(); }],
  ['move', async (page) => { await nav(page).getByRole('button', { name: /Board/ }).click(); await page.locator('.phone-board-card').first().getByRole('button', { name: 'Move ▸' }).click(); }],
  ['comment', async (page) => { await nav(page).getByRole('button', { name: /Board/ }).click(); await page.locator('.phone-board-card').first().getByRole('button', { name: 'Comment' }).click(); }],
  ['update', async (page) => {
    await nav(page).getByRole('button', { name: /More/ }).click();
    await page.getByRole('dialog', { name: 'More' }).locator('.phone-menu-item').filter({ hasText: 'Update tclaude' }).first().click();
  }],
];

const titleStyle = (locator) => locator.evaluate((el) => {
  const s = getComputedStyle(el);
  return { size: s.fontSize, weight: s.fontWeight, color: s.color };
});

test("every sheet's title is the same, and its buttons are ButtonRow's", async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const styles = {};
  for (const [name, open] of SHEETS) {
    await home(page);
    await open(page);
    await expect(sheet(page)).toBeVisible();
    await page.waitForTimeout(500);
    if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, `${BEFORE ? 'before' : 'after'}-sheet-${name}.png`) });
    if (!BEFORE) {
      const title = sheet(page).locator('.phone-sheet-title').first();
      await expect(title, `${name} has a sheet title`).toBeVisible();
      styles[name] = await titleStyle(title);
      // A button row in a sheet is ButtonRow's: its buttons are .phone-btn.
      for (const row of await sheet(page).locator('.phone-btns').all()) {
        expect(await row.locator('button:not(.phone-btn)').count(), `${name}: a row of phone buttons`).toBe(0);
      }
    }
    if (name === 'message') {
      // Discard? is the shared question, with the focus on Keep.
      await sheet(page).locator('textarea').fill('half a thought');
      await page.keyboard.press('Escape');
      const ask = sheet(page).getByRole('alertdialog', { name: 'Discard this message?' });
      await expect(ask).toBeVisible();
      await expect(ask.getByRole('button', { name: 'Keep' })).toBeFocused();
      if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, `${BEFORE ? 'before' : 'after'}-sheet-discard.png`) });
      await ask.getByRole('button', { name: 'Discard' }).click();
      await expect(page.locator('.phone-sheet')).toHaveCount(0);
      continue;
    }
    await closeSheet(page);
  }
  if (!BEFORE) {
    for (const [name, style] of Object.entries(styles)) expect(style, name).toEqual({ size: '17px', weight: '700', color: styles['agent-menu'].color });
  }
  expect(errors).toEqual([]);
});

test('a board card\'s actions are a ButtonRow, Move ▸ opening a sheet', async ({ page }) => {
  await home(page);
  if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, `${BEFORE ? 'before' : 'after'}-now.png`) });
  await nav(page).getByRole('button', { name: /Board/ }).click();
  await expect(page.locator('.phone-board-card').first()).toBeVisible();
  if (process.env.SHOTS) await page.screenshot({ path: path.join(process.env.SHOTS, `${BEFORE ? 'before' : 'after'}-board.png`) });
  if (!BEFORE) {
    const actions = page.locator('.phone-board-card').first().locator('.phone-btns');
    await expect(actions.getByRole('button', { name: 'Move ▸' })).toHaveAttribute('aria-haspopup', 'dialog');
  }
});
