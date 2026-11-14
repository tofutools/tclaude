// Access, Usage › Costs and the desktop-only page on a phone
// (js/phone-access-island.js, js/phone-desktop-only.js) in a real browser:
// Sudo lists the seeded grant with Revoke behind a confirm, a deep link lands
// on its subtab, the slug registry is searched rather than listed, Costs and
// Access fit the phone, and Fleet admin's sections say they are desktop only.
//
//   scripts/sandbox/ui.sh smoke phone-access
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

async function expectNoSideScroll(page, where) {
  const { scroll, width, main } = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth, width: document.documentElement.clientWidth,
    main: document.querySelector('main')?.scrollWidth ?? 0,
  }));
  expect(scroll, `${where}: the page is ${scroll}px wide on a ${width}px phone`).toBeLessThanOrEqual(width);
  expect(main, `${where}: the tab content is ${main}px wide on a ${width}px phone`).toBeLessThanOrEqual(width);
}

function collectErrors(page) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  page.on('console', (message) => {
    if (message.type() === 'error' && !/429 \/api\/companion\/presence/.test(message.text())) errors.push(`console: ${message.text()}`);
  });
  return errors;
}

const views = (page) => page.getByRole('tablist', { name: 'Access views' });

test('Access: a deep link lands on Sudo, revoke asks first, and slugs are searched', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/access/sudo');
  await booted(page);
  await expect(views(page).getByRole('tab', { name: 'Sudo' })).toHaveAttribute('aria-selected', 'true');
  const grant = page.locator('.phone-access-row').filter({ hasText: 'agent.spawn' });
  await expect(grant).toBeVisible();
  await expectNoSideScroll(page, 'Access › Sudo');

  await grant.getByRole('button', { name: /^Revoke / }).click();
  const confirm = page.getByRole('dialog');
  await expect(confirm).toContainText('Revoke sudo grant?');
  await confirm.getByRole('button', { name: 'Cancel' }).click();
  await expect(grant).toBeVisible();

  await views(page).getByRole('tab', { name: 'Slugs' }).click();
  await expect(page).toHaveURL(/\/access\/slugs$/);
  await page.getByRole('searchbox', { name: 'Search permission slugs' }).fill('human.notify');
  await expect(page.locator('.phone-access-row')).toHaveCount(1);
  await expectNoSideScroll(page, 'Access › Slugs');

  await views(page).getByRole('tab', { name: 'Agents' }).click();
  await expect(page.locator('.phone-desktop-only:visible')).toContainText('desktop only');

  // Requests is the bare /access: Back onto it shows it again.
  await views(page).getByRole('tab', { name: 'Requests' }).click();
  await expect(page).toHaveURL(/\/access$/);
  await views(page).getByRole('tab', { name: 'Sudo' }).click();
  await expect(page).toHaveURL(/\/access\/sudo$/);
  await page.goBack();
  await expect(page).toHaveURL(/\/access$/);
  await expect(views(page).getByRole('tab', { name: 'Requests' })).toHaveAttribute('aria-selected', 'true');
  expect(errors).toEqual([]);
});

test('Costs fits the phone, and a Fleet admin section says it is desktop only', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/usage/costs');
  await booted(page);
  await expect(page.locator('#costs-table tbody tr').first()).toBeVisible();
  await expectNoSideScroll(page, 'Usage › Costs');

  // Fleet's own view is read-only (phone-diagnostics.spec.mjs); its admin
  // sections are desktop only.
  await page.goto('/fleet-admin/grants');
  await booted(page);
  await expect(page.locator('.phone-bar-title')).toHaveText('Fleet');
  await expect(page.locator('.phone-desktop-page:visible')).toContainText('Fleet admin is desktop only');
  expect(errors).toEqual([]);
  // The desktop Fleet admin it opens is not under test here (with no hub it
  // answers 409s, as the tour spec notes): only that the layout switched.
  await page.getByRole('button', { name: 'Desktop site' }).click();
  await page.waitForURL((url) => !url.searchParams.has('layout'));
  await booted(page);
  await expect(page.locator('html')).not.toHaveAttribute('data-layout', 'phone');
});
