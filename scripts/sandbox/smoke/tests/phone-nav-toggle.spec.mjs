// The ▾/▴ toggle for the phone's bottom nav (js/phone-nav-toggle.js) in a
// real browser: a terminal and the mail reader keep the nav under them by
// default; the toggle left of their field hides it, which gives its height
// to the view and puts the nav's count on the toggle; each view remembers
// its own choice across a reload; and a view picked in the nav under a
// terminal closes the terminal.
//
//   scripts/sandbox/ui.sh smoke phone-nav-toggle
//
// With SHOTS_DIR set it saves the four states for the mock comparison.
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

async function shot(page, name) {
  if (process.env.SHOTS_DIR) await page.screenshot({ path: `${process.env.SHOTS_DIR}/${name}.png` });
}

const height = (locator) => locator.evaluate((el) => el.getBoundingClientRect().height);

test('the terminal: the nav under it, hidden by the toggle and remembered; a slot closes it', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  const snapshot = await (await page.request.get('/api/snapshot')).json();
  const group = (snapshot.groups || []).find((g) => (g.members || []).some((m) => m.online));
  const agent = group.members.find((m) => m.online);
  await page.routeWebSocket(/-ws\//, (ws) => {
    ws.onMessage(() => {});
    ws.send(Buffer.from('\x1b[?1049h\x1b[2m● Bash go test ./pkg/api/\x1b[0m\r\n  ok  pkg/api  0.84s\r\n\r\n\x1b[33mDo you want to commit?\x1b[0m\r\n 1. Yes\r\n 2. Yes, and push\r\n 3. No\r\n'));
  });
  await page.goto(`/?terminal=${encodeURIComponent(`window:${agent.agent_id}`)}`);
  await booted(page);
  const term = page.locator('.phone-term[role=dialog]');
  const nav = page.getByRole('navigation', { name: 'Views' });
  const toggle = term.locator('.phone-term-input .phone-nav-toggle');
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await expect(toggle).toHaveAttribute('aria-label', 'Hide page selector');
  await expect(nav).toBeVisible();
  // The nav sits below the terminal's input row, not over it.
  const input = await term.locator('.phone-term-input').boundingBox();
  const navBox = await nav.boundingBox();
  expect(input.y + input.height).toBeLessThanOrEqual(navBox.y + 1);
  const bodyShown = await height(term.locator('.phone-term-body'));
  await shot(page, 'terminal-shown');

  await toggle.click();
  await expect(nav).toHaveCount(0);
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await expect(toggle.locator('.ui-badge')).toBeVisible();
  expect(await height(term.locator('.phone-term-body'))).toBeGreaterThan(bodyShown + 40);
  await shot(page, 'terminal-hidden');

  // Remembered across a reload; mail keeps its own choice.
  await page.reload();
  await booted(page);
  await expect(term.locator('.phone-term-input .phone-nav-toggle')).toHaveAttribute('aria-expanded', 'false');
  await expect(nav).toHaveCount(0);
  await term.locator('.phone-term-input .phone-nav-toggle').click();
  await expect(nav).toBeVisible();

  // Select covers the terminal, Copy at its bottom: the nav keeps away.
  await term.getByRole('button', { name: 'Select and copy text' }).click();
  await expect(nav).toHaveCount(0);
  await term.getByRole('button', { name: 'Done' }).click();
  await expect(nav).toBeVisible();

  // A view picked in the nav closes the terminal.
  await nav.getByRole('button', { name: /Agents/ }).click();
  await expect(term).toHaveCount(0);
  await expect(nav.getByRole('button', { name: /Agents/ })).toHaveAttribute('aria-current', 'page');

  // A terminal opened as a history step (from Agents), then Now picked
  // under it: the close steps history back and Now still lands; Back
  // returns to Agents, not to the terminal.
  await page.locator('.phone-agent-row', { hasText: agent.title || agent.name }).first().click();
  await expect(term).toBeVisible();
  await nav.getByRole('button', { name: /Now/ }).click();
  await expect(term).toHaveCount(0);
  await expect(nav.getByRole('button', { name: /Now/ })).toHaveAttribute('aria-current', 'page');
  await page.goBack();
  await expect(term).toHaveCount(0);
  await expect(nav.getByRole('button', { name: /Agents/ })).toHaveAttribute('aria-current', 'page');
  expect(errors).toEqual([]);
});

test('the mail reader: the nav under the reply bar, hidden by the toggle and remembered', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.goto('/messages/human');
  await booted(page);
  await page.locator('.phone-mail-open').first().click();
  const reader = page.locator('.phone-mail-reader');
  await expect(reader).toBeVisible();
  const nav = page.getByRole('navigation', { name: 'Views' });
  const toggle = page.locator('.phone-mail-replybar .phone-nav-toggle');
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await expect(nav).toBeVisible();
  const bar = await page.locator('.phone-mail-replybar').boundingBox();
  const navBox = await nav.boundingBox();
  expect(bar.y + bar.height).toBeLessThanOrEqual(navBox.y + 1);
  await shot(page, 'mail-shown');

  await toggle.click();
  await expect(nav).toHaveCount(0);
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  const hiddenBar = await page.locator('.phone-mail-replybar').boundingBox();
  expect(hiddenBar.y).toBeGreaterThan(bar.y + 40);
  await shot(page, 'mail-hidden');

  // Remembered across a reload (which comes back to the list), in any
  // message with a reply bar.
  await page.reload();
  await booted(page);
  if (!(await reader.isVisible())) await page.locator('.phone-mail-open').first().click();
  await expect(page.locator('.phone-mail-replybar .phone-nav-toggle')).toHaveAttribute('aria-expanded', 'false');
  await expect(nav).toHaveCount(0);
  await page.locator('.phone-mail-replybar .phone-nav-toggle').click();
  await expect(nav).toBeVisible();
  expect(errors).toEqual([]);
});
