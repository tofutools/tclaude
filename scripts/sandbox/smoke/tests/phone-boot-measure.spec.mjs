// Measures a cold phone boot on a slow round trip, in a real browser: a fresh
// browser context (nothing cached) on an iPhone, through a TCP proxy that adds
// 100 ms to each round trip, from navigation to the boot curtain lifting. It
// prints the numbers, and checks the page booted in the phone layout with the
// phone shell's modules preloaded (tk_33321868), so their dynamic imports are
// not a discover → fetch waterfall.
//
//   TCLAUDE_SANDBOX_MEASURE=1 scripts/sandbox/ui.sh smoke phone-boot-measure
//
// The proxy delays each direction by half the round trip and paces it at a
// fast link (50 Mbit/s), so latency, not bandwidth, is what is measured.
import { test, expect, devices } from '@playwright/test';
import net from 'node:net';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.setTimeout(240_000);

const LINK = { rttMs: 100, down: 50e6 / 8, up: 50e6 / 8 };
const RUNS = Number(process.env.TCLAUDE_MEASURE_RUNS) || 5;

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

const median = (values) => [...values].sort((a, b) => a - b)[Math.floor(values.length / 2)];

test('a cold phone boot at 100 ms RTT', async ({ browser, baseURL }) => {
  test.skip(!process.env.TCLAUDE_SANDBOX_MEASURE, 'set TCLAUDE_SANDBOX_MEASURE=1 to measure');
  const target = new URL(baseURL);
  const proxy = await throttle({ hostname: target.hostname, port: Number(target.port) }, LINK);
  const base = `http://127.0.0.1:${proxy.address().port}`;
  const runs = [];
  try {
    for (let i = 0; i < RUNS; i += 1) {
      const context = await browser.newContext({ ...IPHONE });
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', (error) => errors.push(error.message));
      await page.goto(`${base}/`, { waitUntil: 'commit' });
      // The curtain lifts when dashboard.js removes .dashboard-booting after
      // the first snapshot paints; the body must exist first.
      await page.waitForFunction(() => document.readyState !== 'loading' && document.body
        && !document.body.classList.contains('dashboard-booting') && !!window.__tclaudeLayout, null, { timeout: 120_000 });
      const run = await page.evaluate(() => {
        const nav = performance.getEntriesByType('navigation')[0];
        const modules = performance.getEntriesByType('resource').filter((r) => /\/static\/.*\.m?js$/.test(r.name));
        const preloaded = new Set([...document.querySelectorAll('link[rel="modulepreload"]')].map((l) => l.href));
        const shell = modules.find((r) => r.name.endsWith('/js/phone-shell-island.js'));
        return {
          layout: document.documentElement.dataset.layout || 'desktop',
          bootMs: Math.round(performance.now() - nav.startTime),
          modules: modules.length,
          preloads: preloaded.size,
          phoneShellPreloaded: [...preloaded].some((href) => href.endsWith('/js/phone-shell-island.js')),
          desktopJobsPreloaded: [...preloaded].some((href) => href.endsWith('/js/jobs-island.js')),
          phoneShellFetchedMs: shell ? Math.round(shell.startTime) : -1,
          lastModuleMs: Math.round(Math.max(...modules.map((r) => r.responseEnd))),
        };
      });
      run.errors = errors;
      runs.push(run);
      await context.close();
    }
  } finally {
    proxy.close();
  }
  console.table(runs.map(({ errors, ...run }) => run));
  const summary = {
    bootMs: median(runs.map((r) => r.bootMs)),
    phoneShellFetchedMs: median(runs.map((r) => r.phoneShellFetchedMs)),
    lastModuleMs: median(runs.map((r) => r.lastModuleMs)),
  };
  console.log(`cold phone boot at ${LINK.rttMs} ms RTT, median of ${RUNS}:`, JSON.stringify(summary));
  for (const run of runs) {
    expect(run.layout, 'the iPhone boots the phone layout').toBe('phone');
    expect(run.errors, 'no page errors').toEqual([]);
    expect(run.phoneShellPreloaded, 'the phone shell is in the boot preload').toBe(true);
    expect(run.desktopJobsPreloaded, 'the desktop Jobs island is not preloaded on a phone').toBe(false);
  }
});
