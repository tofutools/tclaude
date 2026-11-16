// Measures a phone swipe over a Claude Code like fullscreen view on a
// throttled connection, in a real browser: the sandbox attaches the terminal
// to a stand-in for Claude Code's view (RunSwipeTUIForTest) in a tmux of its own (TCLAUDE_SANDBOX_SWIPE_TUI),
// so each notch makes a real tmux redraw come back through the real socket.
// It prints numbers, and checks that only redraws move the screen (the page
// never shifts it, nor itself), that a steady drag scrolls one way, and that
// the view settles within 150ms of the last frame, at the bottom too.
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
  // Each direction is a FIFO drained by one timer at a time, so bytes keep
  // their order: a timer per chunk can fire out of order (Node truncates
  // fractional delays and keeps timers of different lengths apart), which
  // corrupts a TLS stream. A close is passed on once the queue has drained.
  const pipe = (from, to, rate) => {
    const queue = [];
    let free = 0;
    let timer = null;
    let closed = false;
    const drain = () => {
      timer = null;
      const now = Date.now();
      while (queue.length && queue[0].at <= now) {
        const { chunk } = queue.shift();
        if (!to.destroyed) to.write(chunk);
      }
      if (queue.length) timer = setTimeout(drain, Math.max(1, queue[0].at - now));
      else if (closed) to.destroy();
    };
    from.on('data', (chunk) => {
      const now = Date.now();
      free = Math.max(free, now) + (chunk.length / rate) * 1000;
      queue.push({ at: free + link.rttMs / 2, chunk });
      if (!timer) timer = setTimeout(drain, Math.max(1, queue[0].at - now));
    });
    from.on('close', () => {
      closed = true;
      if (!timer) setTimeout(() => { if (!queue.length) to.destroy(); }, link.rttMs / 2);
    });
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
// frame), at speed px per move, then rests and lifts (or flings, with fling),
// and returns what the screen did, frame by frame:
// - firstMove, firstRedraw: when the screen first changed, from the first move;
// - longestStill: the longest stretch without a visible change while the
//   finger moved (from its first move, so the first change counts too);
// - shifted: whether the page ever moved the screen itself (it must not:
//   Claude Code redraws it, tk_77cabfac);
// - pageMoved: whether the page or the terminal's box moved;
// - reversals: how often the view's top line went against the swipe;
// - settleMs: how long after the last terminal message the screen last
//   changed (a freeze shows here).
async function drag(page, { dy, speed, fling = false }) {
  return page.evaluate(async ({ dy, speed, fling }) => {
    const host = document.querySelector('.phone-term-host');
    const screen = host.querySelector('.xterm-screen');
    const rowsText = () => host.querySelector('.xterm-rows')?.textContent || '';
    // The stand-in's rows end in their line number.
    const topLine = () => {
      const first = host.querySelector('.xterm-rows')?.firstElementChild?.textContent || '';
      const m = first.trim().match(/(\d+)$/);
      return m ? Number(m[1]) : null;
    };
    const where = () => `${document.scrollingElement?.scrollTop || 0}|${window.visualViewport?.offsetTop || 0}|${Math.round(host.getBoundingClientRect().top)}`;
    const shiftOf = () => `${screen.style.translate || ''}${screen.style.transform || ''}`;
    const box = host.getBoundingClientRect();
    const x = box.left + box.width / 2;
    const y0 = box.top + (dy > 0 ? box.height / 5 : box.height * 4 / 5);
    const fire = (type, y) => {
      const touch = y === null ? [] : [new Touch({ identifier: 1, target: host, clientX: x, clientY: y })];
      host.dispatchEvent(new TouchEvent(type, { touches: touch, targetTouches: touch, changedTouches: touch, bubbles: true, cancelable: true }));
    };
    const frame = () => new Promise((resolve) => requestAnimationFrame(resolve));
    const messages = window.__termMessages || [];
    const firstMessage = messages.length;
    let text = rowsText();
    let shift = shiftOf();
    let top = topLine();
    const startWhere = where();
    const seen = { firstMove: null, firstRedraw: null, redraws: 0, longestStill: 0, moves: 0, shifted: false, pageMoved: false, reversals: 0 };
    // Finger down shows older lines: the top line number falls.
    const way = dy > 0 ? -1 : 1;
    let start = null;
    let lastChange = null;
    let dragging = true;
    const look = () => {
      const now = performance.now();
      const nextText = rowsText();
      const nextShift = shiftOf();
      if (nextShift) seen.shifted = true;
      if (where() !== startWhere) seen.pageMoved = true;
      // A shift of the screen is a visible change too (the page must make
      // none, but a measure of the old way counts them).
      if (nextShift !== shift) {
        if (seen.firstMove === null) seen.firstMove = now - start;
        if (dragging) seen.longestStill = Math.max(seen.longestStill, now - lastChange);
        lastChange = now;
        shift = nextShift;
      }
      if (nextText !== text) {
        seen.redraws += 1;
        if (seen.firstRedraw === null) seen.firstRedraw = now - start;
        if (seen.firstMove === null) seen.firstMove = now - start;
        if (dragging) seen.longestStill = Math.max(seen.longestStill, now - lastChange);
        lastChange = now;
        const nextTop = topLine();
        if (top !== null && nextTop !== null && Math.sign(nextTop - top) === -way) seen.reversals += 1;
        top = nextTop ?? top;
      }
      text = nextText;
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
    if (fling) fire('touchend', null);
    // Keep watching while the last redraws land.
    const end = performance.now() + 2500;
    while (performance.now() < end) { await frame(); look(); }
    if (!fling) fire('touchend', null);
    seen.dragMs = steps * 16;
    const lastMessage = messages.slice(firstMessage).at(-1);
    seen.settleMs = lastMessage === undefined || lastChange === null ? 0 : Math.max(0, lastChange - lastMessage);
    return seen;
  }, { dy, speed, fling });
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

  // When each terminal message arrived, for settleMs.
  await page.addInitScript(() => {
    window.__termMessages = [];
    const Native = window.WebSocket;
    window.WebSocket = class extends Native {
      constructor(...args) {
        super(...args);
        // Terminal output is binary; control messages (JSON) do not count.
        this.addEventListener('message', (event) => { if (typeof event.data !== 'string') window.__termMessages.push(performance.now()); });
      }
    };
  });
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
  // Down to the live bottom, then on past it, dragged and flung: the view
  // stays put and settles at once (tk_77cabfac: it froze, then jumped).
  for (let i = 0; i < 6; i += 1) await drag(page, { dy: -320, speed: 16, fling: true });
  const bottom = [];
  for (const fling of [false, true, false, true]) bottom.push(await drag(page, { dy: -320, speed: 16, fling }));
  console.log('past the bottom:', JSON.stringify(bottom.map(({ settleMs, redraws, shifted, pageMoved }) => ({ settleMs: Math.round(settleMs), redraws, shifted, pageMoved }))));
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
      settleMs: Math.round(median(of.map((r) => r.settleMs))),
      reversals: of.reduce((s, r) => s + r.reversals, 0),
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
  for (const run of [...runs, ...bottom]) {
    expect(run.shifted, 'the page never shifts the screen: only redraws move it').toBe(false);
    expect(run.pageMoved, 'the page and the terminal box stay put').toBe(false);
    expect(run.settleMs, 'the view settles within 150ms of the last frame').toBeLessThanOrEqual(150);
  }
  for (const run of runs) expect(run.reversals, `${run.name}: a steady drag scrolls one way`).toBe(0);
});
