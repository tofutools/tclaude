// The phone terminal's swipe in a real browser (real xterm.js, the real
// widget): a one-finger swipe sends the pane exactly what the desktop's mouse
// wheel sends, so each harness scrolls its own way. The pane's socket is
// mocked, playing a tmux client attached to a pane: the alternate screen, and
// SGR mouse reporting while the application asks for it (Claude Code's
// fullscreen TUI through tmux with mouse off, or tmux's own mouse mode for
// Codex and shells). Only where the wheel scrolls tmux copy-mode (Codex) does
// ↓ Live show and a key scroll back down first: Claude Code scrolls its own
// history and takes keys while scrolled back, as on the desktop.
//
//   scripts/sandbox/ui.sh smoke phone-terminal-swipe
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];

test.use(IPHONE);

const WHEEL_UP = /^(\x1b\[<64;\d+;\d+M)+$/;
const WHEEL_DOWN = /^(\x1b\[<65;\d+;\d+M)+$/;

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

// openPane opens the phone terminal of the sandbox's first agent on harness,
// over a mocked pane socket, and returns what the widget sends it.
async function openPane(page, harness) {
  const snapshot = await (await page.request.get('/api/snapshot')).json();
  const members = [...(snapshot.groups || []).flatMap((g) => g.members || []), ...(snapshot.ungrouped || [])];
  const agent = members.find((m) => (m.state?.harness || m.harness) === harness);
  expect(agent, `the sandbox has a ${harness} agent`).toBeTruthy();
  const pane = { sent: [], socket: null };
  await page.routeWebSocket(/-ws\//, (ws) => {
    pane.socket = ws;
    ws.onMessage((message) => {
      const text = typeof message === 'string' ? message : Buffer.from(message).toString('latin1');
      if (!text.startsWith('{"type":"resize"')) pane.sent.push(text);
    });
    // A tmux client: the alternate screen, and SGR mouse reporting.
    ws.send(Buffer.from(`\x1b[?1049h\x1b[?1000h\x1b[?1006h${harness} pane\r\n`));
  });
  await page.goto(`/?terminal=${encodeURIComponent(`window:${agent.agent_id}`)}`);
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
  const terminal = page.locator('.phone-term[role=dialog]');
  await expect(terminal.locator('.xterm-rows')).toContainText(`${harness} pane`);
  return { pane, terminal, live: terminal.getByRole('button', { name: 'Back to live' }) };
}

// settled waits until nothing more is sent.
async function settled(pane) {
  let count = -1;
  await expect.poll(() => { const now = pane.sent.length; const done = now === count; count = now; return done; }, { intervals: [100] }).toBe(true);
}

test('over Claude Code a swipe is the desktop wheel: its own scrolling, no ↓ Live, keys straight', async ({ page }) => {
  const { pane, terminal, live } = await openPane(page, 'claude');
  await swipe(page, 60, 360);
  expect(pane.sent.join(''), 'SGR wheel-up reports').toMatch(WHEEL_UP);
  await expect(live).toHaveCount(0);
  pane.sent.length = 0;
  await terminal.getByRole('group', { name: 'Keys' }).getByRole('button', { name: '1', exact: true }).click();
  await settled(pane);
  expect(pane.sent, 'the key, and nothing ahead of it').toEqual(['1']);
  await swipe(page, 360, 60);
  expect(pane.sent.slice(1).join(''), 'SGR wheel-down reports').toMatch(WHEEL_DOWN);
  expect(pane.sent.join('')).not.toContain('"scroll"');
});

test('over Codex a swipe scrolls tmux copy-mode: ↓ Live shows, and a key scrolls back down first', async ({ page }) => {
  const { pane, terminal, live } = await openPane(page, 'codex');
  await swipe(page, 60, 360);
  expect(pane.sent.join(''), 'SGR wheel-up reports').toMatch(WHEEL_UP);
  await expect(live, 'scrolled back: ↓ Live').toBeVisible();
  await swipe(page, 360, 60);
  await expect(live, 'back down at live').toHaveCount(0);

  // ↓ Live scrolls back down with wheel-down reports, as the wheel would.
  await swipe(page, 60, 360);
  pane.sent.length = 0;
  await live.click();
  await expect(live).toHaveCount(0);
  await settled(pane);
  expect(pane.sent.join('')).toMatch(WHEEL_DOWN);

  // A key while scrolled back: copy-mode would take it, so the wheel goes
  // back down first.
  await swipe(page, 60, 360);
  pane.sent.length = 0;
  await terminal.getByRole('group', { name: 'Keys' }).getByRole('button', { name: '1', exact: true }).click();
  await settled(pane);
  expect(pane.sent.at(-1)).toBe('1');
  expect(pane.sent.slice(0, -1).join('')).toMatch(WHEEL_DOWN);
  await expect(live).toHaveCount(0);

  // With no mouse reporting the desktop wheel is an arrow key on the
  // alternate screen, and so is a swipe; it scrolls nothing back.
  pane.socket.send(Buffer.from('\x1b[?1000l\x1b[?1006l'));
  await page.waitForTimeout(100);
  pane.sent.length = 0;
  await swipe(page, 60, 360);
  expect(pane.sent.join(''), 'arrow keys up').toMatch(/^(\x1b[[O]A)+$/);
  await expect(live).toHaveCount(0);
});
