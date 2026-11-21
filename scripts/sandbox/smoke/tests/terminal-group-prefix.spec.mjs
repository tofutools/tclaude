// An agent's terminal names its group (tk_c501cda3, tk_0a10cbcb): two agents
// named lead, one in webshop and one in infra, are "w/lead" and "i/lead" in
// the dashboard's terminal tabs, where they clash, and a third tab with a name
// of its own shows it bare. Title bars, a popped-out window and the phone show
// the full "infra/lead". The sandbox has one lead, so the snapshot this page
// reads renames db-migrations to lead and moves it to infra.
// SHOTS=<dir> saves desktop.png, solo.png and phone.png. Set BEFORE=1 against
// a build without this rule, to take the before shots only.
//
//   scripts/sandbox/ui.sh smoke terminal-group-prefix
import { test, expect, devices } from '@playwright/test';
import path from 'node:path';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
const before = !!process.env.BEFORE;
const shot = async (target, name, options) => { if (process.env.SHOTS) await target.screenshot({ path: path.join(process.env.SHOTS, name), ...options }); };

// twoLeads rewrites the snapshot: db-migrations is a second lead, in infra.
async function twoLeads(page) {
  const ids = {};
  await page.route('**/api/snapshot*', async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    for (const agent of body.agents || []) {
      if (agent.title === 'lead') ids.webshop = agent.agent_id;
      else if (!ids.other && agent.title && agent.title !== 'db-migrations' && agent.groups?.length) Object.assign(ids, { other: agent.agent_id, otherName: agent.title });
      if (agent.title === 'db-migrations') {
        ids.infra = agent.agent_id;
        Object.assign(agent, { title: 'lead', groups: ['infra'], spawn_group: 'infra', owned_groups: [] });
      }
    }
    // The group lists name it lead too (the phone reads its name there).
    const rename = (node) => {
      if (Array.isArray(node)) node.forEach(rename);
      else if (node && typeof node === 'object') {
        if (ids.infra && (node.agent_id === ids.infra) && node.title === 'db-migrations') node.title = 'lead';
        if (ids.infra && (node.agent_id === ids.infra) && node.name === 'db-migrations') node.name = 'lead';
        Object.values(node).forEach(rename);
      }
    };
    rename(body.groups);
    await route.fulfill({ response, json: body });
  });
  return ids;
}

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

test.describe('desktop', () => {
  test.use({ viewport: { width: 1280, height: 760 } });
  test('two leads in different groups are told apart in their terminal tabs and title bar', async ({ page }) => {
    test.setTimeout(120_000);
    const ids = await twoLeads(page);
    await page.goto('/groups');
    await booted(page);
    await expect.poll(() => ids.infra && ids.webshop, { timeout: 30_000 }).toBeTruthy();
    await page.evaluate(async ({ webshop, infra, other, otherName }) => {
      const base = document.querySelector('script[type="module"][src$="/js/dashboard.js"]').src.replace(/js\/dashboard\.js$/, '');
      const { openWebWindowPane } = await import(`${base}js/terminals-tab.js`);
      openWebWindowPane(other, otherName, { harness: 'claude' });
      openWebWindowPane(webshop, 'lead', { harness: 'claude', group: 'webshop' });
      openWebWindowPane(infra, 'lead', { harness: 'claude' });
    }, ids);
    const labels = page.locator('[role="tab"] .mux-tab-label');
    await expect(labels.filter({ hasText: 'lead' })).toHaveCount(2, { timeout: 30_000 });
    if (!before) {
      // The two leads clash: one letter tells webshop from infra.
      await expect(labels.filter({ hasText: /^w\/lead$/ })).toHaveAttribute('title', 'webshop/lead');
      await expect(labels.filter({ hasText: /^i\/lead$/ })).toHaveAttribute('title', 'infra/lead');
      // A name no other tab has stays bare, its group on hover.
      await expect(labels.filter({ hasText: new RegExp(`^${ids.otherName}$`) })).toHaveCount(1);
      // The title bar has room for the full group, the slash right after it.
      const title = page.locator('.mux-pane.active .mux-pane-title');
      await expect(title).toHaveText('infra/lead');
      const gap = await title.evaluate((label) => label.querySelector('.agent-group-slash').getBoundingClientRect().left
        - label.querySelector('.agent-group-prefix').getBoundingClientRect().right);
      expect(Math.abs(gap), 'no gap between the group and its slash').toBeLessThan(0.5);
    }
    await page.mouse.move(0, 0);
    const strip = await page.locator('.mux-pane.active').boundingBox();
    await shot(page, 'desktop.png', { clip: { x: 0, y: Math.max(0, strip.y - 60), width: 1280, height: 150 } });
    if (before) return;
    // Floated over the dashboard, both leads' chat tabs clash the same way.
    await page.locator('.mux-pane.active').getByRole('button', { name: 'Open in overlay' }).click();
    await expect(page.locator('.companion-tab', { hasText: 'lead' })).toHaveCount(1, { timeout: 15_000 });
    await expect(page.locator('.companion-tab', { hasText: /^lead$/ })).toHaveCount(1, { timeout: 15_000 });
    await page.locator('.mux-tab', { hasText: 'lead' }).click();
    await page.locator('.mux-pane.active').getByRole('button', { name: 'Open in overlay' }).click();
    await expect(page.locator('.companion-tab', { hasText: 'i/lead' })).toHaveCount(1, { timeout: 15_000 });
    await expect(page.locator('.companion-tab', { hasText: 'w/lead' })).toHaveCount(1, { timeout: 15_000 });
  });

  test('a popped-out terminal names the full group in its title bar and browser title', async ({ page, context }) => {
    test.setTimeout(120_000);
    const ids = await twoLeads(page);
    await page.goto('/groups');
    await booted(page);
    await expect.poll(() => ids.infra, { timeout: 30_000 }).toBeTruthy();
    await page.evaluate(async ({ infra }) => {
      const base = document.querySelector('script[type="module"][src$="/js/dashboard.js"]').src.replace(/js\/dashboard\.js$/, '');
      const { openWebWindowPane } = await import(`${base}js/terminals-tab.js`);
      openWebWindowPane(infra, 'lead', { harness: 'claude' });
    }, ids);
    await expect(page.locator('.mux-pane.active .mux-pane-title')).toHaveText(before ? /lead$/ : 'infra/lead', { timeout: 30_000 });
    const popup = context.waitForEvent('page');
    await page.locator('.mux-pane.active').getByRole('button', { name: 'Open in separate window' }).click();
    const solo = await popup;
    await expect(solo.locator('.mux-pane-title')).toHaveText(before ? /lead$/ : 'infra/lead', { timeout: 30_000 });
    if (!before) await expect(solo).toHaveTitle(/infra\/lead — tclaude terminals/);
    await solo.mouse.move(0, 0);
    await shot(solo, 'solo.png', { clip: { x: 0, y: 0, width: 900, height: 80 } });
    await solo.close();
  });
});

test.describe('phone', () => {
  test.use(IPHONE);
  test('the phone terminal names the full group', async ({ page }) => {
    test.setTimeout(120_000);
    const ids = await twoLeads(page);
    await page.goto('/');
    await booted(page);
    await expect.poll(() => ids.infra, { timeout: 30_000 }).toBeTruthy();
    await page.goto(`/?terminal=${encodeURIComponent(`window:${ids.infra}`)}`);
    await booted(page);
    const title = page.locator('.phone-term[role=dialog] .phone-term-bar .phone-bar-title');
    await expect(title).toBeVisible({ timeout: 30_000 });
    if (!before) await expect(title).toHaveText('infra/lead');
    await shot(page.locator('.phone-term[role=dialog] .phone-term-bar'), 'phone.png');
  });
});
