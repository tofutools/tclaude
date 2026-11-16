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

  // ⋯: the agent's menu, Select text where Terminal would be (this is it).
  await term.getByRole('button', { name: `${name}: more` }).click();
  const sheet = page.locator('.phone-sheet[role=dialog]');
  await expect(sheet.locator('.phone-agent-tile[data-act="select"]')).toHaveText(/Select text/);
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

// A shell (here an agent made a --harness shell one in the snapshot): the
// command-line key strip, and › Type into …, which types the text into the
// pane's socket as keystrokes, with no message header, then Enter if asked.
test('a shell terminal types straight in and has the command-line keys', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  const first = await (await page.request.get('/api/snapshot')).json();
  const agent = (first.groups || []).flatMap((g) => g.members || []).find((m) => m.online);
  await page.route('**/api/snapshot*', async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    for (const member of [...(body.groups || []).flatMap((g) => g.members || []), ...(body.ungrouped || [])]) {
      if (member?.agent_id !== agent.agent_id) continue;
      member.harness = 'shell';
      if (member.state) member.state.harness = 'shell';
    }
    await route.fulfill({ response, json: body });
  });
  const typed = [];
  await page.routeWebSocket(/-ws\//, (ws) => {
    ws.onMessage((data) => { if (typeof data !== 'string') typed.push(Buffer.from(data).toString('utf8')); });
    ws.send(Buffer.from('\x1b[?1049h$ \r\n'));
  });
  await page.goto(`/?terminal=${encodeURIComponent(`window:${agent.agent_id}`)}`);
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });

  const term = page.locator('.phone-term[role=dialog]');
  const keys = term.locator('.phone-term-key');
  await expect(keys).toHaveText(['←', '→', '↑', '↓', '⇥', '␣', '⏎', 'Esc', '^C', 'Copy']);
  // Every key fits its label on the phone: none is clipped.
  for (const box of await keys.evaluateAll((all) => all.map((k) => [k.scrollWidth, k.clientWidth]))) {
    expect(box[0]).toBeLessThanOrEqual(box[1]);
  }
  await expect(term.getByRole('button', { name: /^✉ Message/ })).toHaveCount(0);

  typed.length = 0;
  await term.getByRole('button', { name: 'Tab' }).click();
  await term.getByRole('button', { name: 'Left' }).click();
  await expect.poll(() => typed.join('')).toBe('\t\x1b[D');

  await term.locator('.phone-term-type').click();
  const sheet = page.locator('.phone-sheet[role=dialog]');
  const field = sheet.getByRole('textbox', { name: /^Text to type into/ });
  await expect(field).toBeFocused();
  await field.fill('git status');
  typed.length = 0;
  await sheet.getByRole('button', { name: 'Type + ⏎' }).click();
  await expect(sheet).toBeHidden();
  await expect.poll(() => typed.join('')).toBe('git status\r');
  expect(typed.join('')).not.toContain('message from');
  expect(errors).toEqual([]);
});
