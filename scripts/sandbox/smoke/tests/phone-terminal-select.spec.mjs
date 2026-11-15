// A phone terminal's text (js/phone-terminal-text.js) in a real browser with
// real touches: a tap on a URL opens its sheet and Open link opens it; a
// still hold on a word opens Select with that word selected, and Copy
// selection copies it; the strip's Copy and ⋯ open Select too.
//
//   scripts/sandbox/ui.sh smoke phone-terminal-select
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

// at is the client point of the text's middle character, on the last row
// that shows it.
async function at(page, text, offset = Math.floor(text.length / 2)) {
  return page.evaluate(([text, offset]) => {
    const rows = [...document.querySelectorAll('.phone-term .xterm-rows > div')];
    const row = rows.findLast((d) => d.textContent.includes(text));
    if (!row) return null;
    const r = row.getBoundingClientRect();
    const screen = document.querySelector('.phone-term .xterm-screen').getBoundingClientRect();
    const cols = Math.round(screen.width / (r.width / Math.max(1, row.textContent.length)));
    const cell = screen.width / (row.childElementCount && cols ? cols : 1);
    const measure = document.querySelector('.phone-term .xterm-char-measure-element');
    const w = measure ? measure.getBoundingClientRect().width / measure.textContent.length : cell;
    return { x: screen.left + w * (row.textContent.indexOf(text) + offset) + w / 2, y: r.top + r.height / 2 };
  }, [text, offset]);
}

async function touch(page, point, ms) {
  if (!point) throw new Error('not on screen');
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [point] });
  await page.waitForTimeout(ms);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}

test('a tap on a URL opens its sheet; a hold selects the word; Copy copies it', async ({ page, context }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.goto('/');
  await booted(page);
  await page.getByRole('group', { name: /^(Online|Active)$/ }).locator('.phone-row').first().click();
  const term = page.locator('.phone-term[role=dialog]');
  await expect(term).toBeVisible();
  // The sandbox's pane shows what is typed into it, once its start-up log
  // has gone quiet (unchanged for a second).
  const screen = () => page.evaluate(() => [...document.querySelectorAll('.phone-term .xterm-rows > div')].map((d) => d.textContent).join('\n'));
  await expect(async () => {
    const before = await screen();
    await page.waitForTimeout(1000);
    expect(await screen()).toBe(before);
  }).toPass({ timeout: 30_000 });
  await page.locator('.phone-term .xterm-helper-textarea').focus();
  // Typed again if a late log line broke it up.
  const typed = async () => {
    if (await at(page, 'example.com') && await at(page, 'zebra')) return;
    await page.keyboard.type('see https://example.com/docs zebra ', { delay: 5 });
    await expect.poll(() => at(page, 'zebra')).not.toBeNull();
  };
  await typed();
  await page.evaluate(() => { window.__opened = []; window.open = (url) => { window.__opened.push(url); return null; }; });

  // The sandbox's pane keeps logging, so a row can move between measuring it
  // and touching it: measure and touch again until the gesture lands.
  const sheet = page.locator('.phone-sheet[role=dialog]');
  await expect(async () => {
    await typed();
    await touch(page, await at(page, 'example.com'), 60);
    await expect(sheet.locator('.phone-term-link-target')).toHaveText('https://example.com/docs', { timeout: 1000 });
  }).toPass({ timeout: 15_000 });
  await sheet.getByRole('button', { name: 'Open link ›' }).click();
  await expect.poll(() => page.evaluate(() => window.__opened)).toEqual(['https://example.com/docs']);

  const select = page.getByRole('dialog', { name: 'Select text' });
  await expect(async () => {
    if (await select.isVisible()) await select.getByRole('button', { name: 'Done' }).click();
    await typed();
    await touch(page, await at(page, 'zebra'), 800);
    await expect(select).toBeVisible({ timeout: 1000 });
    expect(await page.evaluate(() => String(getSelection()))).toBe('zebra');
  }).toPass({ timeout: 15_000 });
  await select.getByRole('button', { name: 'Copy selection' }).click();
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe('zebra');
  await select.locator('.phone-term-link', { hasText: 'https://example.com/docs' }).click();
  await expect(sheet.locator('.phone-term-link-target')).toHaveText('https://example.com/docs');
  await page.keyboard.press('Escape');
  await expect(sheet).toBeHidden();
  await expect(select, 'Escape closed the sheet, not Select under it').toBeVisible();
  await select.getByRole('button', { name: 'Done' }).click();
  await expect(select).toBeHidden();

  await term.getByRole('button', { name: 'Select and copy text' }).click();
  await expect(select).toBeVisible();
  await select.getByRole('button', { name: 'Back to the terminal' }).click();
  await expect(select).toBeHidden();
  await expect(term).toBeVisible();
  expect(errors).toEqual([]);
});
