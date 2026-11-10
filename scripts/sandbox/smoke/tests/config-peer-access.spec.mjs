// The Config tab as a peer's per-node page renders it: the real daemon's
// /api/config answer, with its access block narrowed to what a restricted
// peer holding node.config.read and node.config.write gets (config_remote.go)
// and a withheld notification command. Rows of other classes are read-only
// with the grant they need, node-only keys say to change them on the node
// itself, the withheld secret shows only as set, and the rail tags the
// panels. No page errors. CONFIG_PEER_SHOTS=dir saves screenshots.
//
//   scripts/sandbox/ui.sh smoke config-peer-access
import { test, expect } from '@playwright/test';

let pageErrors = [];
test.beforeEach(({ page }) => {
  pageErrors = [];
  page.on('pageerror', (err) => pageErrors.push(err.message));
});
test.afterEach(() => {
  expect(pageErrors, 'page errors').toEqual([]);
});

async function asRestrictedPeer(page) {
  await page.route(/\/api\/config(\?|$)/, async (route) => {
    if (route.request().method() !== 'GET') return route.continue();
    const res = await route.fetch();
    const data = await res.json();
    const classes = Object.fromEntries(Object.keys(data.access.classes).map((c) => [c, c === 'general' ? 'write' : 'read']));
    data.access = { ...data.access, level: 'restricted', classes };
    data.redacted = [{ path: 'notifications.notification_command', set: true }];
    delete data.path;
    await route.fulfill({ response: res, json: data });
  });
}

const shot = async (page, name) => {
  if (process.env.CONFIG_PEER_SHOTS) await page.screenshot({ path: `${process.env.CONFIG_PEER_SHOTS}/${name}.png`, fullPage: false });
};

test('a restricted peer sees what it may change, and why the rest is read-only', async ({ page }) => {
  await asRestrictedPeer(page);
  await page.setViewportSize({ width: 1400, height: 1000 });
  await page.goto('/config/agents');
  const perms = page.locator('[data-cfg-key="agent.default_permissions"]');
  await expect(perms).toHaveAttribute('data-cfg-access', 'read');
  await expect(perms.locator('.cfg-access-note')).toContainText('Needs node.config.agents on this node');
  await expect(perms.locator('fieldset.cfg-row-ctl')).toHaveAttribute('disabled', '');
  await expect(perms.locator('fieldset.cfg-row-ctl').locator('input, button, select, textarea').first()).toBeDisabled();
  await expect(page.locator('[data-cfg-panel-link="agents"] .cfg-rail-access')).toBeVisible();
  await expect(page.locator('[data-cfg-panel-link="appearance"] .cfg-rail-access')).toHaveCount(0);
  await shot(page, 'config-peer-agents');

  await page.locator('[data-cfg-panel-link="notifications"]').click();
  const cmd = page.locator('[data-cfg-key="notifications.notification_command"]');
  await expect(cmd.locator('.cfg-access-note')).toContainText('Change on the node itself');
  await expect(cmd.locator('.cfg-secret-state')).toHaveText('set');
  await expect(page.locator('[data-cfg-key="notifications.enabled"]')).toHaveAttribute('data-cfg-access', 'write');
  await cmd.scrollIntoViewIfNeeded();
  await shot(page, 'config-peer-notifications');
});
