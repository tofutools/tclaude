// The phone terminal's swipe in a real browser (real xterm.js, the real
// widget): a one-finger swipe sends the pane exactly what the desktop's mouse
// wheel sends, so each harness scrolls its own way. The pane's socket is
// mocked, playing a tmux client attached to a pane: the alternate screen, and
// SGR mouse reporting while the application asks for it (Claude Code's
// fullscreen TUI through tmux with mouse off, or tmux's own mouse mode for
// Codex and shells). While wheel reports have scrolled the view back, ↓ Live
// scrolls back down the same way.
//
//   scripts/sandbox/ui.sh smoke phone-terminal-swipe
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];

test.use(IPHONE);

// swipe drags one finger from y0 to y1 over the terminal, in steps.
async function swipe(page, y0, y1) {
  await page.evaluate(async ({ y0, y1 }) => {
    const host = document.querySelector('.phone-term-host');
    const box = host.getBoundingClientRect();
    const x = box.left + box.width / 2;
    const fire = (type, y) => {
      const touch = y === null ? [] : [new Touch({ identifier: 1, target: host, clientX: x, clientY: box.top + y })];
      host.dispatchEvent(new TouchEvent(type, { touches: touch, targetTouches: touch, changedTouches: touch, bubbles: true, cancelable: true }));
    };
    fire('touchstart', y0);
    const steps = 12;
    for (let i = 1; i <= steps; i += 1) {
      fire('touchmove', y0 + ((y1 - y0) * i) / steps);
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    // Rest before lifting: no fling, so the count of notches is the travel's.
    await new Promise((resolve) => setTimeout(resolve, 200));
    fire('touchend', null);
  }, { y0, y1 });
}

test('a swipe sends the pane the desktop wheel\'s own input, mouse reporting or not', async ({ page }) => {
  const sent = [];
  let pane = null;
  await page.routeWebSocket(/-ws\//, (ws) => {
    pane = ws;
    ws.onMessage((message) => {
      const text = typeof message === 'string' ? message : Buffer.from(message).toString('latin1');
      if (!text.startsWith('{"type":"resize"')) sent.push(text);
    });
    // A tmux client: the alternate screen, and the pane's application asks
    // for SGR mouse reporting.
    ws.send(Buffer.from('\x1b[?1049h\x1b[?1000h\x1b[?1006hclaude code\r\n'));
  });
  await page.goto('/');
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
  await page.getByRole('group', { name: 'Needs you' }).getByRole('button', { name: 'Open', exact: true }).first().click();
  const terminal = page.locator('.phone-term[role=dialog]');
  await expect(terminal.locator('.xterm')).toBeVisible();
  await expect.poll(() => pane !== null).toBe(true);
  await expect(terminal.locator('.xterm-rows')).toContainText('claude code');

  // Finger down is further back: wheel-up reports, one per five rows.
  await swipe(page, 60, 360);
  const up = sent.join('');
  expect(up, 'SGR wheel-up reports').toMatch(/^(\x1b\[<64;\d+;\d+M)+$/);
  sent.length = 0;
  const live = terminal.getByRole('button', { name: 'Back to live' });
  await expect(live, 'scrolled back: ↓ Live').toBeVisible();
  await swipe(page, 360, 60);
  expect(sent.join(''), 'SGR wheel-down reports').toMatch(/^(\x1b\[<65;\d+;\d+M)+$/);
  await expect(live, 'back down at live').toHaveCount(0);

  // ↓ Live scrolls back down with wheel-down reports, as the wheel would.
  await swipe(page, 60, 360);
  sent.length = 0;
  await live.click();
  await expect(live).toHaveCount(0);
  // Its notches go out a few a frame: wait until they all have.
  let count = -1;
  await expect.poll(() => { const now = sent.length; const settled = now === count; count = now; return settled; }, { intervals: [100] }).toBe(true);
  expect(sent.join('')).toMatch(/^(\x1b\[<65;\d+;\d+M)+$/);

  // With no mouse reporting the desktop wheel is an arrow key on the
  // alternate screen, and so is a swipe.
  sent.length = 0;
  pane.send(Buffer.from('\x1b[?1000l\x1b[?1006l'));
  await page.waitForTimeout(100);
  await swipe(page, 60, 360);
  expect(sent.join(''), 'arrow keys up').toMatch(/^(\x1b[[O]A)+$/);
  await expect(live, 'an arrow key scrolls nothing back').toHaveCount(0);
});
