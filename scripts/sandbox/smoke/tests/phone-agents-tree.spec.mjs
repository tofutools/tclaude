// The phone Agents tab's All groups tree and a group's page
// (js/phone-agents-island.js) in a real browser on the sandbox's data, with a
// subgroup, an empty group and an archived one made through the production
// routes: Active | All groups, the subgroup on its parent's guide line, the
// empty and archived tags, Show archived, a group's name opening its page,
// and the page's crumbs, subgroups and Back. Nothing scrolls sideways on an
// iPhone 13 or an iPhone SE. SHOTS=<dir> saves a screenshot of each view.
//
//   scripts/sandbox/ui.sh smoke phone-agents-tree
import { test, expect, devices } from '@playwright/test';
import path from 'node:path';

const PHONES = { 'iPhone 13': devices['iPhone 13'], 'iPhone SE': devices['iPhone SE'] };

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

// api calls the dashboard's own route from the page, as its buttons do.
function api(page, method, url, body) {
  return page.evaluate(async ({ method, url, body }) => {
    const response = await fetch(url, {
      method, credentials: 'same-origin',
      headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
    return { status: response.status, text: await response.text() };
  }, { method, url, body });
}

async function shot(page, name) {
  if (!process.env.SHOTS) return;
  await page.screenshot({ path: path.join(process.env.SHOTS, `${name}.png`), fullPage: false });
}

const SUB = 'smoke-sub';
const EMPTY = 'smoke-empty';
const OLD = 'smoke-old';

for (const [phone, device] of Object.entries(PHONES)) {
  test.describe(phone, () => {
    const { defaultBrowserType: _ignored, ...use } = device;
    test.use(use);
    const tag = phone.replace(/\s+/g, '-').toLowerCase();

    test('All groups: every group nested, empty and archived tagged; a name opens its page', async ({ page }) => {
      const errors = collectErrors(page);
      await page.goto('/groups');
      await booted(page);
      const made = [];
      try {
        for (const body of [{ name: SUB, parent: 'webshop' }, { name: EMPTY }, { name: OLD }]) {
          const out = await api(page, 'POST', '/api/groups', body);
          expect(out.status, `create ${body.name}: ${out.text}`).toBeLessThan(300);
          made.push(body.name);
        }
        const archived = await api(page, 'POST', `/api/groups/${OLD}/archive`);
        expect(archived.status, archived.text).toBeLessThan(300);
        // From a fresh load, so every view shows the groups just made.
        await page.goto('/groups');
        await booted(page);

        const seg = (label) => page.getByRole('group', { name: 'Show', exact: true }).getByRole('button', { name: new RegExp(`^${label}`) });
        // A run before may have left All groups picked.
        await seg('Active').click();
        await expect(seg('Active')).toHaveAttribute('aria-pressed', 'true');
        await shot(page, `${tag}-active`);
        const counts = () => page.locator('.phone-agents-seg button').allTextContents();
        const onActive = await counts();
        await seg('All groups').click();
        await expect(seg('All groups')).toHaveAttribute('aria-pressed', 'true');
        expect(await counts(), 'the same counts in either mode').toEqual(onActive);
        // Someone online first, then only offline agents, then empty groups.
        const roots = await page.locator('.phone-agents > .phone-agents-node').evaluateAll((els) => els.map((el) => ({
          name: el.getAttribute('aria-label'), dim: el.classList.contains('phone-agents-dim'), empty: el.classList.contains('phone-agents-empty'),
        })));
        const rank = (n) => (!n.dim ? 0 : n.empty ? 2 : 1);
        expect(roots.map(rank), roots.map((n) => n.name).join(', ')).toEqual(roots.map(rank).sort());
        const node = (name) => page.locator(`.phone-agents-node[aria-label="${name}"]`);
        await expect(node('webshop').locator(`.phone-agents-sub > .phone-agents-node[aria-label="${SUB}"]`)).toBeVisible();
        await expect(node(EMPTY).locator('.phone-agents-tag')).toHaveText('empty');
        await expect(node(EMPTY)).toHaveClass(/phone-agents-dim/);
        await expect(node(OLD)).toHaveCount(0);
        await expectNoSideScroll(page);
        await shot(page, `${tag}-all-groups`);

        await page.getByRole('button', { name: /^Show archived/ }).click();
        await expect(node(OLD).locator('.phone-agents-tag')).toHaveText(['empty', 'archived']);
        const order = await page.locator('.phone-agents > .phone-agents-node').evaluateAll((els) => els.map((el) => el.getAttribute('aria-label')));
        expect(order.at(-1), 'archived last').toBe(OLD);
        await page.getByRole('button', { name: 'Hide archived' }).click();
        await expect(node(OLD)).toHaveCount(0);

        // The name, not the caret, opens the group's page.
        await node('webshop').locator(':scope > h2 .phone-agents-fold').click();
        await expect(page.locator('.phone-bar-title')).toHaveText('webshop');
        const view = page.locator('.phone-group-page');
        await expect(view.locator('.phone-agents-subcard', { hasText: SUB })).toBeVisible();
        await expect(view.getByRole('button', { name: 'Board ›' })).toBeVisible();
        await expect(view.getByRole('button', { name: 'Mail ›' })).toBeVisible();
        await expect(view.locator('.phone-agent').first()).toBeVisible();
        await expectNoSideScroll(page);
        await shot(page, `${tag}-group-page`);
        await view.locator('.phone-agents-subcard', { hasText: SUB }).click();
        await expect(page.locator('.phone-bar-title')).toHaveText(SUB);
        await expect(view.locator('.phone-agents-crumbs')).toHaveText(`webshop › ${SUB}`);
        await expect(view.getByText('No agents yet.')).toBeVisible();
        await shot(page, `${tag}-subgroup-page`);
        await view.locator('.phone-agents-crumbs').getByRole('button', { name: 'webshop' }).click();
        await expect(page.locator('.phone-bar-title')).toHaveText('webshop');
        // Back goes through the pages opened, then to the list.
        const back = page.getByRole('button', { name: 'Back', exact: true });
        for (const title of [SUB, 'webshop']) {
          await back.click();
          await expect(page.locator('.phone-bar-title')).toHaveText(title);
        }
        await back.click();
        await expect(view).toHaveCount(0);
        // The browser's Back (a phone's back gesture) does the same.
        await node('webshop').locator(':scope > h2 .phone-agents-fold').click();
        await view.locator('.phone-agents-subcard', { hasText: SUB }).click();
        await expect(page.locator('.phone-bar-title')).toHaveText(SUB);
        await page.goBack();
        await expect(page.locator('.phone-bar-title')).toHaveText('webshop');
        await page.goBack();
        await expect(view).toHaveCount(0);
        await expect(seg('All groups')).toHaveAttribute('aria-pressed', 'true');
        await expect(seg('All groups')).toHaveAttribute('aria-pressed', 'true');

        // The pick survives a reload. The prefs write is debounced (prefs.js).
        await expect.poll(() => page.evaluate(() => fetch('/api/dashboard/prefs', { credentials: 'same-origin' })
          .then((r) => r.json()).then((prefs) => prefs['tclaude.dash.phone.agents.view'] ?? null))).toBe('all');
        await page.reload();
        await booted(page);
        await page.getByRole('navigation', { name: 'Views' }).getByRole('button', { name: /^Agents/ }).click();
        await expect(seg('All groups')).toHaveAttribute('aria-pressed', 'true');
        await seg('Active').click();
        await expect.poll(() => page.evaluate(() => fetch('/api/dashboard/prefs', { credentials: 'same-origin' })
          .then((r) => r.json()).then((prefs) => prefs['tclaude.dash.phone.agents.view'] ?? null))).toBe(null);
        expect(errors).toEqual([]);
      } finally {
        if (made.includes(OLD)) await api(page, 'POST', `/api/groups/${OLD}/unarchive`);
        for (const name of made.reverse()) await api(page, 'DELETE', `/api/groups/${name}`);
      }
    });
  });
}

test.describe('desktop', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test('the desktop Groups tab is unchanged: no phone toggle, its own tree', async ({ page }) => {
    const errors = collectErrors(page);
    await page.goto('/groups');
    await booted(page);
    await expect(page.locator('#groups-list')).toBeVisible();
    await expect(page.locator('.phone-agents-seg')).toBeHidden();
    await shot(page, 'desktop-groups');
    expect(errors).toEqual([]);
  });
});
