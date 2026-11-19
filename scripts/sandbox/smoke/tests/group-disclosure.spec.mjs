// The Groups tab's group boxes fold through a real button, not a <summary>
// (disclosure.js's Disclosure, groups-list.js's GroupBox): the fold button
// says whether it is open and is worked by Enter and Space, the fold is kept
// across a reload, the header's chips sit beside the button, and the header
// is still the reorder handle and a drop target for a member row.
//
//   scripts/sandbox/ui.sh smoke group-disclosure
import { test, expect } from '@playwright/test';

test.use({ viewport: { width: 1440, height: 1400 }, actionTimeout: 15000 });
test.describe.configure({ timeout: 90000 });

const box = (page, name) => page.locator(`#groups-list .group-box[data-group-key="${name}"]`);
const head = (page, name) => box(page, name).locator(':scope > .group-summary');
const toggle = (page, name) => head(page, name).locator(':scope > .group-toggle');
const body = (page, name) => box(page, name).locator(':scope > .subtable');
const order = (page) => page.locator('#groups-list > * > .group-box:not(.group-virtual), #groups-list .group-box:not(.group-virtual)')
  .evaluateAll((els) => [...new Set(els.filter((el) => !el.parentElement.closest('.group-box')).map((el) => el.dataset.groupKey))]);

async function openGroups(page) {
  await page.goto('/');
  await expect(box(page, 'webshop')).toBeVisible();
}

// setOpen folds or unfolds a group by its button.
async function setOpen(page, name, open) {
  if ((await toggle(page, name).getAttribute('aria-expanded')) !== String(open)) await toggle(page, name).click();
  await expect(toggle(page, name)).toHaveAttribute('aria-expanded', String(open));
}

test('no control sits inside a <summary> on the Groups tab', async ({ page }) => {
  await openGroups(page);
  await setOpen(page, 'webshop', true);
  const inSummary = await page.locator('#groups-list summary :is(button, a[href], input, select, textarea, [tabindex]:not([tabindex="-1"]))').count();
  expect(inSummary).toBe(0);
  // The cog and chips are the header's, beside the fold button.
  await expect(head(page, 'webshop').locator('.group-header-cog')).toHaveCount(1);
  await expect(toggle(page, 'webshop').locator('button, input, a')).toHaveCount(0);
});

test('the fold button folds by click, Enter and Space, names its body, and the fold survives a reload', async ({ page }) => {
  await openGroups(page);
  await setOpen(page, 'webshop', false);
  await expect(body(page, 'webshop')).toBeHidden();

  await toggle(page, 'webshop').click();
  await expect(toggle(page, 'webshop')).toHaveAttribute('aria-expanded', 'true');
  await expect(body(page, 'webshop')).toBeVisible();
  const controls = await toggle(page, 'webshop').getAttribute('aria-controls');
  expect(await body(page, 'webshop').getAttribute('id')).toBe(controls);

  await toggle(page, 'webshop').focus();
  await page.keyboard.press('Enter');
  await expect(toggle(page, 'webshop')).toHaveAttribute('aria-expanded', 'false');
  await expect(body(page, 'webshop')).toBeHidden();
  await page.keyboard.press('Space');
  await expect(toggle(page, 'webshop')).toHaveAttribute('aria-expanded', 'true');

  // A click on the header's empty space, or on a chip, does not fold.
  const hb = await head(page, 'webshop').boundingBox();
  await page.mouse.click(hb.x + hb.width - 4, hb.y + hb.height / 2);
  await expect(toggle(page, 'webshop')).toHaveAttribute('aria-expanded', 'true');

  // Kept across a reload, both ways.
  await page.reload();
  await expect(toggle(page, 'webshop')).toHaveAttribute('aria-expanded', 'true');
  await setOpen(page, 'webshop', false);
  await page.reload();
  await expect(toggle(page, 'webshop')).toHaveAttribute('aria-expanded', 'false');
  await expect(body(page, 'webshop')).toBeHidden();
});

test('typing a space in a header chip editor does not fold the group', async ({ page }) => {
  await openGroups(page);
  await setOpen(page, 'infra', true);
  // The default-directory chip's text opens its inline editor.
  await head(page, 'infra').locator('.group-default-cwd .qo-text').click();
  const input = head(page, 'infra').locator('.group-default-cwd-input');
  await expect(input).toBeFocused();
  await page.keyboard.type('a b');
  await expect(input).toHaveValue(/a b$/);
  await expect(toggle(page, 'infra')).toHaveAttribute('aria-expanded', 'true');
  await page.keyboard.press('Escape');
});

test('a member row dragged onto another group header moves there', async ({ page }) => {
  await openGroups(page);
  await setOpen(page, 'webshop', true);
  await setOpen(page, 'infra', true);
  const row = body(page, 'webshop').locator('tr.dnd-draggable[data-dnd-agent]').first();
  const key = await row.getAttribute('data-key');
  await row.locator('td').first().dragTo(head(page, 'infra'), { sourcePosition: { x: 3, y: 3 } });
  // A move may ask first.
  const confirm = page.getByRole('dialog').getByRole('button', { name: /^(move|ok|confirm)/i });
  if (await confirm.count()) await confirm.first().click();
  await expect(body(page, 'infra').locator(`tr[data-key="${key}"]`)).toHaveCount(1);
  await expect(body(page, 'webshop').locator(`tr[data-key="${key}"]`)).toHaveCount(0);
});

test('a group header dragged above another reorders the groups, and the order is kept', async ({ page }) => {
  await openGroups(page);
  const before = await order(page);
  const last = before[before.length - 1];
  const first = before[0];
  const target = await head(page, first).boundingBox();
  await head(page, last).dragTo(head(page, first), {
    sourcePosition: { x: (await head(page, last).boundingBox()).width - 6, y: 4 },
    targetPosition: { x: target.width / 2, y: 2 },
  });
  await expect.poll(() => order(page)).toEqual([last, ...before.filter((n) => n !== last)]);
  await page.reload();
  await expect(box(page, 'webshop')).toBeVisible();
  await expect.poll(() => order(page)).toEqual([last, ...before.filter((n) => n !== last)]);
});
