// Measures a phone swipe over a Claude Code like fullscreen view on a
// throttled connection, in a real browser: the sandbox attaches the terminal
// to a stand-in for Claude Code's view (RunSwipeTUIForTest) in a tmux of its own (TCLAUDE_SANDBOX_SWIPE_TUI),
// so each notch makes a real tmux redraw come back through the real socket.
// It prints numbers, and only checks that the screen answered the swipe.
//
//   TCLAUDE_SANDBOX_SWIPE_TUI=1 scripts/sandbox/ui.sh smoke phone-swipe-measure
//
// The network is Chrome DevTools' "Fast 4G" preset (9 Mbit/s down, 1.5 up,
// 165 ms round trip, as DevTools applies it), played by a TCP proxy in front of
// the sandbox: DevTools' own throttling leaves WebSocket frames alone. Times
// are from the first touchmove.
import { test, expect, devices } from '@playwright/test';
import net from 'node:net';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);
test.setTimeout(180_000);

const FAST_4G = { rttMs: 60 * 2.75, down: (9000 * 1000 / 8) * 0.9, up: (1500 * 1000 / 8) * 0.9 };

// throttle serves a proxy to target that delays each direction by half the
// round trip and paces it at the link's rate, in order.
function throttle(target, link) {
  const pipe = (from, to, rate) => {
    let free = 0;
    from.on('data', (chunk) => {
      const now = Date.now();
      free = Math.max(free, now) + (chunk.length / rate) * 1000;
      setTimeout(() => { if (!to.destroyed) to.write(chunk); }, free - now + link.rttMs / 2);
    });
    from.on('close', () => setTimeout(() => to.destroy(), link.rttMs / 2 + 50));
    from.on('error', () => to.destroy());
  };
  const server = net.createServer((client) => {
    const upstream = net.connect(target.port, target.hostname);
    pipe(client, upstream, link.up);
    pipe(upstream, client, link.down);
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server)));
}

// drag moves one finger over the terminal by dy px, a move every 16ms (a
// frame), at speed px per move, and returns what the screen did, frame by
// frame: when it first moved, when the first redraw landed, and the longest
// stretch without visible movement while the finger moved (from its first
// move, so the first visible move counts as a still stretch too).
async function drag(page, { dy, speed }) {
  return page.evaluate(async ({ dy, speed }) => {
    const host = document.querySelector('.phone-term-host');
    const screen = host.querySelector('.xterm-screen');
    const rowsText = () => host.querySelector('.xterm-rows')?.textContent || '';
    const box = host.getBoundingClientRect();
    const x = box.left + box.width / 2;
    const y0 = box.top + box.height / 3;
    const fire = (type, y) => {
      const touch = y === null ? [] : [new Touch({ identifier: 1, target: host, clientX: x, clientY: y })];
      host.dispatchEvent(new TouchEvent(type, { touches: touch, targetTouches: touch, changedTouches: touch, bubbles: true, cancelable: true }));
    };
    const frame = () => new Promise((resolve) => requestAnimationFrame(resolve));
    let text = rowsText();
    let shift = screen.style.translate;
    const seen = { firstMove: null, firstRedraw: null, redraws: 0, longestStill: 0, moves: 0 };
    let start = null;
    let lastChange = null;
    let dragging = true;
    const look = () => {
      const now = performance.now();
      const nextText = rowsText();
      const nextShift = screen.style.translate;
      const redraw = nextText !== text;
      if (redraw) { seen.redraws += 1; if (seen.firstRedraw === null) seen.firstRedraw = now - start; }
      if (redraw || nextShift !== shift) {
        if (seen.firstMove === null) seen.firstMove = now - start;
        if (dragging) seen.longestStill = Math.max(seen.longestStill, now - lastChange);
        lastChange = now;
      }
      text = nextText;
      shift = nextShift;
    };
    fire('touchstart', y0);
    await frame();
    const steps = Math.ceil(Math.abs(dy) / speed);
    for (let i = 1; i <= steps; i += 1) {
      fire('touchmove', y0 + Math.sign(dy) * Math.min(Math.abs(dy), i * speed));
      if (start === null) { start = performance.now(); lastChange = start; }
      seen.moves += 1;
      await frame();
      look();
    }
    // How long the screen had been still when the finger stopped counts too.
    seen.longestStill = Math.max(seen.longestStill, performance.now() - lastChange);
    dragging = false;
    // Rest, then lift: no fling. Keep watching while the last redraws land.
    const end = performance.now() + 1500;
    while (performance.now() < end) { await frame(); look(); }
    fire('touchend', null);
    seen.dragMs = steps * 16;
    return seen;
  }, { dy, speed });
}

test('a phone swipe over a Claude Code like view on Fast 4G', async ({ page, context, baseURL }) => {
  test.skip(!process.env.TCLAUDE_SANDBOX_SWIPE_TUI, 'set TCLAUDE_SANDBOX_SWIPE_TUI=1 to measure');
  const proxy = await throttle(new URL(baseURL), FAST_4G);
  const via = `http://127.0.0.1:${proxy.address().port}`;
  const snapshot = await (await page.request.get('/api/snapshot')).json();
  const members = [...(snapshot.groups || []).flatMap((g) => g.members || []), ...(snapshot.ungrouped || [])];
  const agent = members.find((m) => (m.state?.harness || m.harness) === 'claude');
  expect(agent, 'the sandbox has a claude agent').toBeTruthy();

  const cdp = await context.newCDPSession(page);
  await cdp.send('Network.enable');
  const frames = [];
  cdp.on('Network.webSocketFrameReceived', ({ timestamp, response }) => frames.push({ t: timestamp, bytes: response.payloadData.length }));

  await page.goto(`${via}/?terminal=${encodeURIComponent(`window:${agent.agent_id}`)}`);
  await expect(page.locator('.phone-term-host .xterm-rows')).toContainText('⏺', { timeout: 60_000 });
  await page.waitForTimeout(1000); // the attach's own redraws settle

  // SHOTS=<dir>: the screen a round trip into a medium drag, and once it rests.
  if (process.env.SHOTS) {
    const path = await import('node:path');
    const dragging = drag(page, { dy: 240, speed: 6 });
    await page.waitForTimeout(170);
    await page.screenshot({ path: path.join(process.env.SHOTS, 'swipe-mid-drag.png') });
    await dragging;
    await page.screenshot({ path: path.join(process.env.SHOTS, 'swipe-rest.png') });
  }
  const runs = [];
  for (const [name, dy, speed] of [['slow', 160, 2], ['medium', 240, 6], ['fast', 320, 16]]) {
    for (let i = 0; i < 3; i += 1) {
      for (const sign of [1, -1]) {
        const before = frames.length;
        const seen = await drag(page, { dy: sign * dy, speed });
        const received = frames.slice(before);
        runs.push({ name, ...seen, messages: received.length, bytes: received.reduce((sum, f) => sum + f.bytes, 0) });
      }
    }
  }
  const median = (values) => { const v = values.filter((x) => x !== null).sort((a, b) => a - b); return v.length ? v[Math.floor(v.length / 2)] : null; };
  const rows = [];
  for (const name of ['slow', 'medium', 'fast']) {
    const of = runs.filter((r) => r.name === name);
    const redraws = of.reduce((s, r) => s + r.redraws, 0);
    rows.push({
      swipe: name,
      dragMs: of[0].dragMs,
      firstMoveMs: Math.round(median(of.map((r) => r.firstMove))),
      firstRedrawMs: Math.round(median(of.map((r) => r.firstRedraw))),
      longestStillMs: Math.round(median(of.map((r) => r.longestStill))),
      messagesPerRedraw: +(of.reduce((s, r) => s + r.messages, 0) / Math.max(1, redraws)).toFixed(2),
      payloadBytesPerRedraw: Math.round(of.reduce((s, r) => s + r.bytes, 0) / Math.max(1, redraws)),
    });
  }
  console.log(`\nphone swipe on Fast 4G (medians of ${runs.length / 3} drags each):`);
  console.table(rows);
  if (process.env.SWIPE_MEASURE_OUT) {
    const fs = await import('node:fs');
    fs.writeFileSync(process.env.SWIPE_MEASURE_OUT, JSON.stringify({ rows, runs }, null, 2));
  }
  proxy.close();
  for (const row of rows) expect(row.firstRedrawMs, `${row.swipe}: a redraw came back`).not.toBeNull();
});
