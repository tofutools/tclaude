// Find › in the phone Mail reader (js/agent-route.js): a message from an
// offline agent (perf-audit, seedSandboxOfflineSender) goes to its row in
// Agents with its group unfolded and Show offline turned on, the row glowing,
// and Power on one tap away in its menu; Back returns to the message. The
// sender's Terminal › looks off but a tap says why. In All groups too.
//
//   scripts/sandbox/ui.sh smoke phone-agent-find
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

const nav = (page) => page.getByRole('navigation', { name: 'Views' });

// hideIt folds webshop and turns Show offline off in the Agents view on
// screen, so perf-audit's row is nowhere to be seen.
async function hideIt(page) {
  const offline = page.getByRole('button', { name: 'Show offline' });
  if (await offline.getAttribute('aria-pressed') === 'true') await offline.click();
  await expect(offline).toHaveAttribute('aria-pressed', 'false');
  const caret = page.getByRole('button', { name: /^(Fold|Unfold) webshop$/ });
  if (await caret.getAttribute('aria-expanded') === 'true') await caret.click();
  await expect(caret).toHaveAttribute('aria-expanded', 'false');
  await expect(page.locator('.phone-agent-title', { hasText: /^perf-audit$/ })).toHaveCount(0);
}

// openPerfMail opens perf-audit's notification in Mail's Human folder.
async function openPerfMail(page) {
  await nav(page).getByRole('button', { name: /Mail/ }).click();
  await page.getByRole('group', { name: 'Mailboxes' }).getByRole('button', { name: /^Human/ }).click();
  await page.locator('.phone-mail-open', { hasText: 'Lighthouse budget: paused' }).click();
  await expect(page.locator('.phone-mail-reader')).toBeVisible();
}

// expectFound checks perf-audit's row is drawn, in view and glowing.
async function expectFound(page) {
  // The link is one-shot: the location drops ?agent= once it is found
  // (Groups is the dashboard's root, so the path may be bare).
  await expect(page).not.toHaveURL(/[?&]agent=/);
  await expect(page.locator('.phone-mail-reader')).toBeHidden();
  const row = page.locator('.phone-agent', { has: page.locator('.phone-agent-title', { hasText: /^perf-audit$/ }) }).first();
  await expect(row).toHaveClass(/phone-agent-found/);
  await expect(row).toBeInViewport();
  await expect(page.getByRole('button', { name: 'Show offline' })).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByRole('button', { name: /^Fold webshop$/ })).toHaveAttribute('aria-expanded', 'true');
  // The glow goes after a while (FOUND_MS).
  await expect(row).not.toHaveClass(/phone-agent-found/, { timeout: 8_000 });
  return row;
}

test('Find › on an offline sender: its row in Agents, glowing, Power on a tap away; Back to the message', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  await page.goto('/groups');
  await booted(page);
  await hideIt(page);

  await openPerfMail(page);
  const terminal = page.locator('.phone-mail-terminal');
  await expect(terminal).toHaveAttribute('aria-disabled', 'true');
  // aria-disabled: Playwright would wait for it to be enabled.
  await terminal.click({ force: true });
  await expect(page.locator('.toast').filter({ hasText: /perf-audit is offline\. Find ›/ }).first()).toBeVisible();
  await expect(page.locator('.phone-terminal, .phone-term')).toHaveCount(0);

  await page.getByRole('button', { name: 'Find perf-audit in Agents' }).click();
  const row = await expectFound(page);
  await row.getByRole('button', { name: 'perf-audit: more' }).click();
  const menu = page.locator('.phone-sheet[role=dialog]');
  await expect(menu.locator('.phone-agent-tile').first()).toHaveText(/Power on/);
  await page.keyboard.press('Escape');
  await expect(menu).toHaveCount(0);

  await page.goBack();
  await expect(page.locator('.phone-mail-reader')).toBeVisible();
  await expect(page).toHaveURL(/\/messages\/human\/[^/?]+$/);
  expect(errors).toEqual([]);
});

test('Find › in All groups unfolds the tree to the agent', async ({ page }) => {
  await page.goto('/groups');
  await booted(page);
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: /^All groups/ }).click();
  try {
    await hideIt(page);
    await openPerfMail(page);
    await page.getByRole('button', { name: 'Find perf-audit in Agents' }).click();
    await expect(page.getByRole('group', { name: 'Show' }).getByRole('button', { name: /^All groups/ })).toHaveAttribute('aria-pressed', 'true');
    await expectFound(page);
  } finally {
    // The view is a daemon pref: leave Active for the smokes after this one.
    await page.goto('/groups');
    await booted(page);
    await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: /^Active/ }).click();
  }
});

test('a link to an agent that is not here says so', async ({ page }) => {
  await page.goto('/groups?agent=agt_nobodyhere');
  await booted(page);
  await expect(page.getByText(/No agent agt_nobodyhere here/).first()).toBeVisible();
  await expect(page).not.toHaveURL(/[?&]agent=/);
});
