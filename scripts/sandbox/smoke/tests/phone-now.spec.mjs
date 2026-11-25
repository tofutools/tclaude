// The phone's Now view (js/phone-now.js, js/phone-approval.js) in a real
// browser on the sandbox's data: the usage strip opens Usage, an access
// request is a card with Deny / +5 min / Approve, a decision's ⋯ opens the
// whole decision, and the agents online are rows that open their terminal.
// The chips over it switch what Now shows.
//
//   scripts/sandbox/ui.sh smoke phone-now
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

test('Now shows what needs you as cards, and its strip and rows go where they say', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.goto('/');
  await booted(page);
  const needs = page.getByRole('group', { name: 'Needs you' });
  const request = needs.locator('.phone-box-key').first();
  await expect(request).toBeVisible();
  await expect(request.locator('.phone-btns .ui-btn')).toHaveText(['Deny', '+5 min', /^Approve/]);
  await expect(request.locator('.phone-pill-key')).toHaveText(/^(\d+:\d\d|\d+h \d\dm)$/);

  const decision = needs.locator('.phone-box-ask').first();
  await decision.getByRole('button', { name: 'All options, a note, or dismiss' }).click();
  const sheet = page.locator('.phone-sheet[role=dialog]');
  await expect(sheet.getByRole('textbox', { name: 'Note to send with your choice' })).toBeVisible();
  await expect(sheet.getByRole('button', { name: 'Dismiss' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(sheet).toBeHidden();

  // No side scroll: the usage strip fits (B6). Polled: read once, just after
  // the sheet closed, it once caught the layout mid-settle (407 of 390).
  await expect.poll(() => page.evaluate(() => {
    const view = document.querySelector('.phone-view');
    return view.scrollWidth - view.clientWidth;
  }), { message: 'Now scrolls sideways', timeout: 3000 }).toBeLessThanOrEqual(0);

  await page.getByRole('group', { name: /^(Online|Active)$/ }).locator('.phone-row').first().click();
  await expect(page.locator('.phone-term[role=dialog]')).toBeVisible();
  await page.goBack();
  await expect(page.locator('.phone-term[role=dialog]')).toBeHidden();

  await page.getByRole('button', { name: /^Usage: .*Open Usage$/ }).click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Usage');
  expect(errors).toEqual([]);
});

test('with three subscriptions the usage strip wraps inside the phone instead of overflowing', async ({ page }) => {
  await page.route(/\/api\/snapshot/, async (route) => {
    const response = await route.fetch();
    const snapshot = await response.json();
    snapshot.usage = { ...(snapshot.usage || {}), available: true, five_hour: { pct: 62 }, seven_day: { pct: 46 },
      codex: { available: true, five_hour: { pct: 12 }, seven_day: { pct: 27 } }, copilot: { available: true, monthly: { pct: 40 } } };
    await route.fulfill({ response, json: snapshot });
  });
  await page.goto('/');
  await booted(page);
  const strip = page.getByRole('button', { name: /^Usage: Claude .*Codex .*Copilot .*Open Usage$/ });
  await expect(strip).toBeVisible();
  const fits = await strip.evaluate((el) => el.scrollWidth <= el.clientWidth && el.getBoundingClientRect().right <= document.documentElement.clientWidth);
  expect(fits, 'the strip stays inside the phone').toBe(true);
  await strip.screenshot({ path: test.info().outputPath('strip-three.png') });
});

test('the chips over Now switch its view, are kept fitted, and the pick survives a reload; a section head folds on a tap anywhere', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.goto('/');
  await booted(page);
  const chips = page.getByRole('tablist', { name: 'Show on Now' });
  await expect(chips.getByRole('tab')).toHaveText([/^All/, /^Needs you/, /^Idle/, /^Recent/, /^Problems/]);
  // The row scrolls sideways inside itself, never the page.
  expect(await chips.evaluate((el) => getComputedStyle(el).overflowX)).toBe('auto');

  // A few online agents: every group's are listed, the idle ones too.
  await expect(page.getByRole('group', { name: 'Online' }).locator('.phone-pill', { hasText: 'idle' }).first()).toBeVisible();

  // The whole Open PRs head row folds, at least a finger tall.
  const head = page.getByRole('group', { name: 'Open PRs' }).locator('.phone-section-fold');
  if (await head.count()) {
    expect((await head.boundingBox()).height).toBeGreaterThanOrEqual(44);
    await head.click({ position: { x: 20, y: 10 } });
    await expect(head).toHaveAttribute('aria-expanded', 'true');
  }

  await chips.getByRole('tab', { name: /^Idle/ }).click();
  await expect(page.getByRole('group', { name: 'Idle' })).toBeVisible();
  await expect(page.locator('.phone-usage-strip')).toHaveCount(0);
  await page.reload();
  await booted(page);
  await expect(page.getByRole('tablist', { name: 'Show on Now' }).getByRole('tab', { name: /^Idle/ })).toHaveAttribute('aria-selected', 'true');
  await page.getByRole('tablist', { name: 'Show on Now' }).getByRole('tab', { name: /^Problems/ }).click();
  await expect(page.getByRole('group', { name: 'Problems' })).toBeVisible();
  expect(errors).toEqual([]);
});

test('on a 360px phone the chips scroll inside their row: Problems scrolls into view, and the page never scrolls sideways', async ({ page }) => {
  await page.setViewportSize({ width: 360, height: 740 });
  await page.goto('/');
  await booted(page);
  const chips = page.getByRole('tablist', { name: 'Show on Now' });
  const problems = chips.getByRole('tab', { name: /^Problems/ });
  await problems.click();
  await expect(problems).toHaveAttribute('aria-selected', 'true');
  // The row may scroll smoothly: wait for the chip to come fully into view.
  await expect.poll(async () => { const box = await problems.boundingBox(); return box.x + box.width; },
    { message: 'the picked chip is on screen' }).toBeLessThanOrEqual(360);
  const { scroll, width } = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, width: document.documentElement.clientWidth }));
  expect(scroll).toBeLessThanOrEqual(width);
  // Problems lists the fixture's failing PR (payments-fix's #1469) and its error.
  await expect(page.getByRole('group', { name: 'Problems' }).locator('.phone-now-group')).not.toHaveCount(0);
});

// A full context is no problem (the harness compacts, or the agent
// reincarnates): rows wear the desktop's context meter by their state pill,
// the agent menu's header has the exact figure, and Problems lists the
// fixture's db-migrations (93%) for its error only, never for its context.
// SHOTS=<dir> saves each view.
test('the context meter by the pill, its figure in the agent menu, and no context in Problems', async ({ page }) => {
  const shot = async (name) => { if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/${name}.png` }); };
  await page.goto('/');
  await booted(page);
  await page.getByRole('tablist', { name: 'Show on Now' }).getByRole('tab', { name: /^Recent/ }).click();
  const meter = page.locator('.phone-row > .ctx-meter.ctx-regular').first();
  await expect(meter).toBeVisible();
  expect(await meter.evaluate((el) => el.nextElementSibling.nextElementSibling.classList.contains('phone-pill')), 'by the pill').toBe(true);
  expect(await page.locator('.phone-ctx-tag').count(), 'no text tag').toBe(0);
  await shot('now-recent');
  await page.getByRole('tablist', { name: 'Show on Now' }).getByRole('tab', { name: /^Problems/ }).click();
  const problems = page.getByRole('group', { name: 'Problems' });
  await expect(problems.locator('.phone-now-group').first()).toBeVisible();
  await expect(problems.locator('.phone-now-group')).not.toContainText([/context/i]);
  await expect(problems.locator('.phone-now-when')).not.toContainText([/context \d+% full/]);
  await shot('now-problems');

  await page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: /^Groups/ }).click();
  const row = page.locator('.phone-agent').filter({ has: page.locator('.phone-agent-title', { hasText: /^db-migrations$/ }) });
  await expect(row.locator('.phone-agent-line > .ctx-meter.ctx-regular .ctx-seg.lit-red')).toHaveCount(1);
  await expect(row.locator('.phone-agent-meta')).not.toContainText('ctx');
  await shot('agents-rows');
  await row.getByRole('button', { name: 'db-migrations: more' }).click();
  await expect(page.locator('.phone-sheet[role=dialog] .phone-agent-sheet-meta').first()).toContainText(/ctx \d+%/);
  await shot('agent-menu');
});
