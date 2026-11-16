// The phone Agents tab's groups and the bottom nav (js/phone-agents-island.js,
// js/phone-shell-island.js) in a real browser on the sandbox's data: the
// chips filter by any number of groups, a group's caret folds it and the
// fold survives a reload, the nav's icons are one colour with the tab on
// screen in a filled pill, and More lists every entry after Fleet.
//
//   scripts/sandbox/ui.sh smoke phone-agents-groups
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

function collectErrors(page) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  page.on('console', (message) => {
    if (message.type() === 'error' && !/429 \/api\/companion\/presence/.test(message.text())) errors.push(`console: ${message.text()}`);
  });
  return errors;
}

async function expectNoSideScroll(page) {
  const { scroll, width } = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth, width: document.documentElement.clientWidth,
  }));
  expect(scroll, `the page is ${scroll}px wide on a ${width}px phone`).toBeLessThanOrEqual(width);
}

const nav = (page, name) => page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: new RegExp(`^${name}`) });
const chips = (page) => page.getByRole('group', { name: 'Show groups' });
const chip = (page, name) => chips(page).getByRole('button', { name: new RegExp(`^${name} \\d+$`) });
const sections = (page) => page.locator('.phone-agents > .phone-section');

test('Agents: the chips filter by several groups, a caret folds its group across a reload', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/groups');
  await booted(page);
  const names = await sections(page).evaluateAll((els) => els.map((el) => el.getAttribute('aria-label')));
  expect(names.length, 'the sandbox has several groups').toBeGreaterThan(1);
  const [first, second] = names;
  await expect(chip(page, 'All')).toHaveAttribute('aria-pressed', 'true');
  await chip(page, second).click();
  await expect(sections(page)).toHaveCount(1);
  await chip(page, first).click();
  await expect(sections(page)).toHaveCount(2);
  expect(await sections(page).evaluateAll((els) => els.map((el) => el.getAttribute('aria-label')))).toEqual([first, second]);
  await expect(chip(page, 'All')).toHaveAttribute('aria-pressed', 'false');
  await chip(page, first).click();
  await expect(sections(page)).toHaveCount(1);
  await chip(page, 'All').click();
  await expect(sections(page)).toHaveCount(names.length);
  await expect(chip(page, second)).toHaveAttribute('aria-pressed', 'false');

  const group = page.getByRole('group', { name: first, exact: true });
  const fold = group.locator('.phone-agents-caret-btn');
  await expect(fold).toHaveAttribute('aria-expanded', 'true');
  await fold.click();
  await expect(fold).toHaveAttribute('aria-expanded', 'false');
  await expect(group.locator('.phone-agent')).toHaveCount(0);
  // The fold is kept in the daemon's prefs: a reload keeps it folded.
  // The prefs write is debounced (prefs.js): wait until the daemon has it.
  await expect.poll(() => page.evaluate((key) => fetch('/api/dashboard/prefs', { credentials: 'same-origin' })
    .then((r) => r.json()).then((prefs) => prefs[key] ?? null), `tclaude.dash.phone.tree.fold.${first}`)).toBe('1');
  await page.reload();
  await booted(page);
  await nav(page, 'Agents').click();
  await expect(page.getByRole('group', { name: first, exact: true }).locator('.phone-agents-caret-btn')).toHaveAttribute('aria-expanded', 'false');
  await page.getByRole('group', { name: first, exact: true }).locator('.phone-agents-caret-btn').click();
  await expect(page.getByRole('group', { name: first, exact: true }).locator('.phone-agent').first()).toBeVisible();
  expect(errors).toEqual([]);
});

test('the nav: one-colour icons, the tab on screen in a filled pill', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/groups');
  await booted(page);
  const agents = nav(page, 'Agents');
  await expect(agents).toHaveAttribute('aria-current', 'page');
  await expect(agents.locator('svg')).toBeVisible();
  const look = (button) => button.evaluate((el) => {
    const icon = el.querySelector('.phone-nav-icon');
    return { color: getComputedStyle(el).color, bg: getComputedStyle(icon).backgroundColor };
  });
  const on = await look(agents);
  const off = await look(nav(page, 'Mail'));
  expect(on.color).not.toBe(off.color);
  expect(off.bg).toBe('rgba(0, 0, 0, 0)');
  expect(on.bg).not.toBe('rgba(0, 0, 0, 0)');
  expect(errors).toEqual([]);
});

test('More lists every entry after Fleet, on a node with a fleet', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/');
  await booted(page);
  const labels = () => page.getByRole('dialog', { name: 'More' }).locator('.phone-menu-label').allTextContents();
  await nav(page, 'More').click();
  const all = await labels();
  await page.getByRole('dialog', { name: 'More' }).locator('.phone-menu-item', { hasText: 'Fleet' }).click();
  await expect(page.locator('.phone-bar-title')).toHaveText('Fleet');
  // A node in a fleet (the desktop's chip row says so) hides the desktop
  // nav's other tabs while Fleet admin is open; a phone must not read that.
  await page.evaluate(() => document.querySelector('#node-chips-root')?.insertAdjacentHTML('beforeend', '<span class="has-fleet" data-smoke></span>'));
  await nav(page, 'More').click();
  await expect(page.getByRole('dialog', { name: 'More' })).toBeVisible();
  // The chip row is Preact's: the fleet mark must still be there for the
  // desktop rule to apply.
  expect(await page.evaluate(() => !!document.querySelector('#node-chips-root .has-fleet[data-smoke]'))).toBe(true);
  expect(await labels()).toEqual(all);
  expect(errors).toEqual([]);
});

test('Agents: each header counts its agents by state, folded too; workers sit under their lead', async ({ page }) => {
  const errors = collectErrors(page);
  await page.goto('/');
  await booted(page);
  await nav(page, 'Agents').click();
  const webshop = page.getByRole('group', { name: 'webshop', exact: true });
  const summary = webshop.locator('.phone-agents-sum');
  await expect(summary.first()).toBeVisible();
  // Loudest first: the errored db-migrations before the working agents.
  await expect(summary.first()).toHaveText(/^\d+ error$/);
  // The sandbox's lead spawned four of webshop's agents (seedSandboxLineage).
  const lead = webshop.locator('.phone-agent').filter({ has: page.locator('.phone-agent-title', { hasText: /^lead$/ }) });
  await expect(lead.locator('.phone-agent-kids')).toHaveText('4 workers');
  await expect(webshop.locator('.phone-agent-worker')).toHaveCount(4);
  const worker = webshop.locator('.phone-agent-worker').filter({ hasText: 'checkout-ui' });
  await expect(worker).toBeVisible();
  const box = await worker.locator('.phone-agent-row').boundingBox();
  expect(box.width, 'the indent leaves the row most of the phone').toBeGreaterThan(260);
  await expectNoSideScroll(page);
  // Folded, the summary stays, and the header stays where it was: folding
  // changes only what is under it (it used to jump 12px on every tap).
  const headTop = () => webshop.locator('.phone-agents-caret-btn')
    .evaluate((el) => Math.round(el.getBoundingClientRect().top + window.scrollY));
  const openTop = await headTop();
  await webshop.locator('.phone-agents-caret-btn').click();
  await expect(webshop.locator('.phone-agent')).toHaveCount(0);
  await expect(summary.first()).toBeVisible();
  expect(await headTop(), 'folding does not move the header').toBe(openTop);
  await webshop.locator('.phone-agents-caret-btn').click();
  await expect(webshop.locator('.phone-agent').first()).toBeVisible();
  expect(await headTop(), 'unfolding does not move the header').toBe(openTop);
  expect(errors).toEqual([]);
});
