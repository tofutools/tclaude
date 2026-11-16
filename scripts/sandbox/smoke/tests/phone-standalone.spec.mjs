// The dashboard installed as an app (dashboard_pwa.go, js/standalone-app.js)
// in a real browser on the sandbox: the page links a manifest Chrome finds
// installable (no service worker needed), the manifest and its icons load
// without the dashboard's session, and, running standalone (where Chrome's
// own pull-to-refresh is off), a pull down on the header reloads.
//
//   scripts/sandbox/ui.sh smoke phone-standalone
import { test, expect, devices, request as playwrightRequest } from '@playwright/test';

const { defaultBrowserType: _ignored, ...PIXEL } = devices['Pixel 7'];
test.use(PIXEL);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

test('the manifest is linked, parses, and loads with its icons without a session', async ({ page, baseURL }) => {
  await page.goto('/');
  await booted(page);
  const cdp = await page.context().newCDPSession(page);
  const manifest = await cdp.send('Page.getAppManifest');
  expect(manifest.url).toBe(new URL('/manifest.webmanifest', baseURL).href);
  expect(manifest.errors).toEqual([]);

  // A fresh client: no dashboard cookie, no Origin. (That the rest of the
  // dashboard still needs the session is TestDashboardPWA_*: the sandbox
  // server runs without the session gate.)
  const anonymous = await playwrightRequest.newContext({ baseURL });
  const response = await anonymous.get('/manifest.webmanifest');
  expect(response.status()).toBe(200);
  expect(response.headers()['content-type']).toBe('application/manifest+json');
  const body = await response.json();
  expect(body).toMatchObject({ display: 'standalone', start_url: '/', scope: '/' });
  for (const icon of body.icons) {
    const got = await anonymous.get(icon.src);
    expect(got.status(), icon.src).toBe(200);
    expect(got.headers()['content-type']).toBe('image/png');
  }
  await anonymous.dispose();
});

test('running standalone, a pull down on the header reloads', async ({ page }) => {
  // The installed app's display mode (Playwright cannot install one).
  await page.addInitScript(() => {
    const real = window.matchMedia.bind(window);
    window.matchMedia = (query) => (query === '(display-mode: standalone)'
      ? { matches: true, media: query, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} }
      : real(query));
  });
  await page.goto('/');
  await booted(page);
  const bar = page.locator('.phone-bar');
  await expect(bar).toBeVisible();
  const box = await bar.boundingBox();
  const cdp = await page.context().newCDPSession(page);
  const at = (y) => [{ x: box.x + box.width / 2, y }];
  const y0 = box.y + box.height / 2;

  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: at(y0) });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: at(y0 + 50) });
  await expect(page.locator('.phone-pull')).toHaveText('↓ Pull to reload');
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: at(y0 + 120) });
  await expect(page.locator('.phone-pull')).toHaveText('↻ Release to reload');
  await page.screenshot({ path: test.info().outputPath('pull-ready.png') });
  const reloaded = page.waitForEvent('load');
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await reloaded;
  await booted(page);
  await expect(page.locator('.phone-pull')).toHaveCount(0);
});
