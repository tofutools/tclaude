// The phone terminal's chrome and message sheet in a real browser on the
// sandbox's data (js/phone-terminal-island.js, js/phone-compose-sheet.js):
// the bar names the agent, its group and state with ⋯ for its sheet, the
// pinch chip shows the text size, and ✉ Message opens the phone's sheet,
// which asks before dropping a draft and sends through the mailbox. The
// pane's socket is mocked; the send is answered by the test.
//
//   scripts/sandbox/ui.sh smoke phone-terminal
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

test('the terminal bar, pinch chip and message sheet', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  const snapshot = await (await page.request.get('/api/snapshot')).json();
  const webshop = (snapshot.groups || []).find((g) => (g.members || []).some((m) => m.online));
  const agent = webshop.members.find((m) => m.online);
  const name = agent.title || agent.name;
  await page.routeWebSocket(/-ws\//, (ws) => { ws.onMessage(() => {}); ws.send(Buffer.from('\x1b[?1049hpane\r\n')); });
  const sent = [];
  await page.route('**/api/operator-message', async (route) => {
    sent.push(route.request().postDataJSON());
    await route.fulfill({ status: 200, contentType: 'application/json', body: '{"queued":true}' });
  });
  await page.goto(`/?terminal=${encodeURIComponent(`window:${agent.agent_id}`)}`);
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });

  const term = page.locator('.phone-term[role=dialog]');
  await expect(term.locator('.phone-bar-title')).toHaveText(`${name}${webshop.name}`);
  await expect(term.locator('.phone-term-bar .phone-pill')).toBeVisible();
  await expect(term.locator('.phone-term-zoom')).toHaveText('pinch · 100%');

  // ⋯: the agent's menu, its Terminal tile off (this is it).
  await term.getByRole('button', { name: `${name}: more` }).click();
  const sheet = page.locator('.phone-sheet[role=dialog]');
  await expect(sheet.locator('.phone-agent-tile[data-act="terminal"]')).toBeDisabled();
  await page.keyboard.press('Escape');
  await expect(sheet).toBeHidden();

  // ✉ Message: the phone's sheet, not the desktop dialog.
  await term.locator('.phone-term-compose').click();
  await expect(sheet).toHaveAttribute('aria-label', `Message ${name}`);
  await expect(page.locator('#operator-message-modal')).toHaveCount(0);
  const field = sheet.getByRole('textbox', { name: `Message to ${name}` });
  await expect(field).toBeFocused();
  await field.fill('Yes, migrate checkout');
  await page.keyboard.press('Escape');
  await expect(sheet.getByText('Discard this message?')).toBeVisible();
  await sheet.getByRole('button', { name: 'Keep' }).click();
  await expect(field).toHaveValue('Yes, migrate checkout');
  await sheet.getByRole('button', { name: 'Send ➤' }).click();
  await expect(sheet).toBeHidden();
  expect(sent).toEqual([expect.objectContaining({ to: agent.agent_id, body: 'Yes, migrate checkout' })]);
  await expect(term).toBeVisible();
  expect(errors).toEqual([]);
});
