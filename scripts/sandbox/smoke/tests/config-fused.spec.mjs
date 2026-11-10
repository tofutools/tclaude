// The Config tab in a fused view over this node and the sandbox's trusted
// peer: Config of picks the node, this node first. The peer has no live node
// behind it, so it is marked online and its /api/peer/{node}/config answer is
// the sandbox's own, narrowed as config-peer-access.spec.mjs does. Picking it
// renames the header and the save confirmation; switching back with unsaved
// edits asks first. No page errors. CONFIG_PEER_SHOTS=dir saves screenshots.
//
//   scripts/sandbox/ui.sh smoke config-fused
import { test, expect } from '@playwright/test';

let pageErrors = [];
test.beforeEach(({ page }) => {
  pageErrors = [];
  page.on('pageerror', (err) => pageErrors.push(err.message));
});
test.afterEach(() => {
  expect(pageErrors, 'page errors').toEqual([]);
});

const shot = async (page, name) => {
  if (process.env.CONFIG_PEER_SHOTS) await page.screenshot({ path: `${process.env.CONFIG_PEER_SHOTS}/${name}.png`, fullPage: false });
};

test('a fused view picks the node its Config tab edits', async ({ page }) => {
  await page.route(/\/api\/federation\/status(\?|$)/, async (route) => {
    const res = await route.fetch();
    const data = await res.json();
    data.peers = (data.peers || []).map((p) => ({ ...p, online: true }));
    await route.fulfill({ response: res, json: data });
  });
  await page.route(/\/api\/peer\/[^/]+\/config(\?|$)/, async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const res = await route.fetch({ url: `${url.origin}/api/config${url.search}` });
    const data = await res.json();
    if (data.access) {
      const classes = Object.fromEntries(Object.keys(data.access.classes).map((c) => [c, c === 'general' ? 'write' : 'read']));
      data.access = { ...data.access, level: 'restricted', classes };
      data.redacted = [{ path: 'notifications.notification_command', set: true }];
    }
    delete data.path;
    // A dry run answers without writing; nothing else is sent in this test.
    await route.fulfill({ response: res, json: data });
  });
  await page.setViewportSize({ width: 1400, height: 1000 });
  await page.goto('/config/notifications?nodes=all');
  const picker = page.locator('.scope-config-picker');
  await expect(picker).toBeVisible();
  const nodes = picker.locator('.scope-config-node');
  await expect(nodes).toHaveCount(2);
  await expect(nodes.first()).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#cfg-path')).toHaveText(/’s config \(this node\)/);
  await shot(page, 'config-fused-this-node');

  await nodes.nth(1).click();
  await expect(nodes.nth(1)).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#cfg-path')).toHaveText(/’s config \(remote\)$/);
  const cmd = page.locator('[data-cfg-key="notifications.notification_command"]');
  await expect(cmd.locator('.cfg-access-note')).toContainText('Change on the node itself');
  await expect(cmd.locator('.cfg-secret-state')).toHaveText('set');
  await shot(page, 'config-fused-peer');

  await page.locator('[data-cfg-key="notifications.cooldown_seconds"] input').fill('7');
  await page.locator('#cfg-save').click();
  const dialog = page.locator('#config-diff-modal');
  await expect(dialog).toContainText(/Confirm config changes on .+ \(remote\)/);
  await shot(page, 'config-fused-confirm');
  await dialog.getByRole('button', { name: 'Cancel' }).click();

  // Back to this node with the edit unsaved: it asks first.
  await nodes.first().click();
  await expect(page.getByText('This form has unsaved changes')).toBeVisible();
  await shot(page, 'config-fused-switch');
  await page.getByRole('button', { name: 'Drop changes and switch' }).click();
  await expect(nodes.first()).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#cfg-path')).toHaveText(/’s config \(this node\)/);
});
