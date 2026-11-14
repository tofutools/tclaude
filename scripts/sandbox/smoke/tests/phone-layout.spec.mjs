// The phone/desktop layout decision (js/layout-boot.js) in a real browser:
// a desktop browser's page is untouched, a phone is detected, ?layout=
// switches and is remembered per browser, and phone mode lays out at the
// device width.
//
//   scripts/sandbox/ui.sh smoke phone-layout
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];

// layoutState reads what the page ended up with once it booted.
async function layoutState(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
  return page.evaluate(() => ({
    decision: { ...window.__tclaudeLayout },
    mark: document.documentElement.dataset.layout ?? null,
    viewportMetas: document.querySelectorAll('meta[name="viewport"]').length,
    phoneSheet: [...document.styleSheets].some((sheet) => (sheet.href || '').endsWith('/static/phone.css')),
    lastSheet: [...document.querySelectorAll('link[rel="stylesheet"]')].at(-1)?.getAttribute('href'),
    search: location.search,
    layoutWidth: document.documentElement.clientWidth,
    gutter: getComputedStyle(document.documentElement).getPropertyValue('--phone-gutter').trim(),
  }));
}

test('a desktop browser gets the desktop page, untouched', async ({ page }) => {
  await page.goto('/');
  const state = await layoutState(page);
  expect(state.decision).toEqual({ layout: 'desktop', detected: 'desktop', override: null });
  expect(state.mark).toBeNull();
  expect(state.viewportMetas).toBe(0);
  expect(state.phoneSheet).toBe(false);
  expect(state.gutter).toBe('');
});

test.describe('on a phone', () => {
  test.use(IPHONE);

  test('the phone is detected but keeps the desktop page until the phone shell exists', async ({ page }) => {
    await page.goto('/messages');
    const state = await layoutState(page);
    expect(state.decision).toEqual({ layout: 'desktop', detected: 'phone', override: null });
    expect(state.mark).toBeNull();
    expect(state.viewportMetas).toBe(0);
  });

  test('?layout=phone switches to the phone layout and is remembered; ?layout=auto forgets it', async ({ page }) => {
    await page.goto('/messages?layout=phone');
    let state = await layoutState(page);
    expect(state.decision).toEqual({ layout: 'phone', detected: 'phone', override: 'phone' });
    expect(state.mark).toBe('phone');
    expect(state.viewportMetas).toBe(1);
    expect(state.phoneSheet).toBe(true);
    expect(state.lastSheet).toBe('/static/phone.css');
    expect(state.gutter).toBe('16px');
    expect(state.search, '?layout= is stripped from the address bar').toBe('');
    expect(state.layoutWidth).toBe(390);

    await page.goto('/config');
    state = await layoutState(page);
    expect(state.decision.override).toBe('phone');
    expect(state.mark).toBe('phone');

    await page.goto('/config?layout=desktop');
    state = await layoutState(page);
    expect(state.decision).toEqual({ layout: 'desktop', detected: 'phone', override: 'desktop' });
    expect(state.mark).toBeNull();
    expect(state.viewportMetas).toBe(0);

    await page.goto('/config?layout=auto');
    state = await layoutState(page);
    expect(state.decision.override).toBeNull();
    expect(await page.evaluate(() => localStorage.getItem('tclaude.layout'))).toBeNull();
  });
});
