// Measures a cold phone boot on a slow round trip, in a real browser: a fresh
// browser context (nothing cached) on an iPhone, through a TCP proxy that adds
// 100 ms to each round trip, from navigation to the boot curtain lifting. It
// prints the numbers, and checks the page booted in the phone layout with the
// phone shell's modules preloaded (tk_33321868), so their dynamic imports are
// not a discover → fetch waterfall.
//
//   TCLAUDE_SANDBOX_MEASURE=1 scripts/sandbox/ui.sh smoke phone-boot-measure
//
// With TCLAUDE_SANDBOX_REMOTE=1 as well, it boots through the remote-access
// listener instead (mTLS, the passphrase login, HTTP/2 where offered): the
// path a phone takes to a dashboard it reaches directly (tk_ec754b5e). Use
// TCLAUDE_SANDBOX_REMOTE=nocert: Playwright's own client-certificate proxy
// does not hold up under a boot's hundreds of requests, so the listener then
// only asks for the certificate (HTTP/1.1 then saves the six mTLS handshakes
// it would otherwise pay).
//
// The proxy delays each direction by half the round trip and paces it at a
// fast link (50 Mbit/s), so latency, not bandwidth, is what is measured.
import { test, expect, devices } from '@playwright/test';
import net from 'node:net';
import { readFileSync } from 'node:fs';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.setTimeout(240_000);

const LINK = { rttMs: 100, down: 50e6 / 8, up: 50e6 / 8 };
const RUNS = Number(process.env.TCLAUDE_MEASURE_RUNS) || 5;

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

const median = (values) => [...values].sort((a, b) => a - b)[Math.floor(values.length / 2)];

test('a cold phone boot at 100 ms RTT', async ({ browser, baseURL }) => {
  test.skip(!process.env.TCLAUDE_SANDBOX_MEASURE, 'set TCLAUDE_SANDBOX_MEASURE=1 to measure');
  const remote = process.env.SANDBOX_REMOTE_JSON ? JSON.parse(readFileSync(process.env.SANDBOX_REMOTE_JSON, 'utf8')) : null;
  const target = new URL(remote ? remote.url : baseURL);
  const proxy = await throttle({ hostname: target.hostname, port: Number(target.port) }, LINK);
  const base = `${target.protocol}//127.0.0.1:${proxy.address().port}`;
  // The remote listener asks for the client certificate, then a login.
  const withCert = remote && process.env.TCLAUDE_SANDBOX_REMOTE !== 'nocert';
  const contextOptions = remote
    ? { ...IPHONE, ignoreHTTPSErrors: true, ...(withCert ? { clientCertificates: [{ origin: base, pfxPath: remote.p12, passphrase: remote.p12Password }] } : {}) }
    : { ...IPHONE };
  const runs = [];
  try {
    for (let i = 0; i < RUNS; i += 1) {
      const context = await browser.newContext(contextOptions);
      if (remote) {
        const login = await context.request.post(`${base}/login`, { form: { passphrase: remote.passphrase }, headers: { Origin: base }, maxRedirects: 0 });
        expect(login.status(), 'the passphrase login').toBeLessThan(400);
      }
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', (error) => errors.push(error.message));
      page.on('requestfailed', (request) => errors.push(`${request.failure()?.errorText}: ${request.url()}`));
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
        const protocols = [...new Set(modules.map((r) => r.nextHopProtocol).filter(Boolean))].join(',');
        return {
          protocol: protocols,
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
  console.log(`cold phone boot at ${LINK.rttMs} ms RTT${remote ? ' via the remote-access listener' : ''}, median of ${RUNS}:`, JSON.stringify(summary));
  for (const run of runs) {
    expect(run.layout, 'the iPhone boots the phone layout').toBe('phone');
    expect(run.errors, 'no page errors').toEqual([]);
    expect(run.phoneShellPreloaded, 'the phone shell is in the boot preload').toBe(true);
    expect(run.desktopJobsPreloaded, 'the desktop Jobs island is not preloaded on a phone').toBe(false);
  }
});
