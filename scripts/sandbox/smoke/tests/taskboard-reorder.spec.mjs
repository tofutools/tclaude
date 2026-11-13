// Reordering a task board's cards by hand (groups-taskboard-order.js): the
// card's ⋯ Move to top, Alt+↓ on a focused card, and a drag to a slot in
// its own cell, each kept across a reload; Done says it is ordered by close
// time. Drives the webshop board's Lanes in the real dashboard.
//
//   scripts/sandbox/ui.sh smoke taskboard-reorder
import { readFileSync } from 'node:fs';
import { test, expect } from '@playwright/test';

// Tall enough that the whole cell is on screen: the browser sends drag
// events only where the pointer can go.
test.use({ viewport: { width: 1440, height: 1400 } });

const fixture = JSON.parse(readFileSync(process.env.SANDBOX_FIXTURE_JSON, 'utf8'));
const section = `[data-fused-taskboard="${fixture.group}"]`;

// openLanes opens the board in Lanes: a task link unfolds its group and
// board, then its form is closed.
async function openLanes(page) {
  await page.goto(`/?task=${fixture.task}:${fixture.group}`);
  await expect(page.getByRole('dialog')).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  const board = page.locator(section).first();
  await expect(board).toBeVisible();
  const lanes = board.getByRole('button', { name: /Lanes/ }).first();
  if (await lanes.count()) await lanes.click();
  await expect(board.locator('.tb-lanes')).toBeVisible();
  return board;
}

// nextCell is the first Next cell holding at least three live cards, by the
// key its cell is drawn under; cards reads its card ids in order.
async function nextCell(board) {
  const cells = board.locator('.tb-cell[data-column="next"]');
  const n = await cells.count();
  for (let i = 0; i < n; i++) {
    if ((await cells.nth(i).locator('.tb-card').count()) >= 3) return cells.nth(i);
  }
  throw new Error('no Next cell with three cards on the fixture board');
}
const cards = (cell) => cell.locator('.tb-card').evaluateAll((els) => els.map((el) => el.dataset.taskId));

test('a card moves to the top from its ⋯, down with Alt+↓, and to a slot by a drag; the order is kept', async ({ page }) => {
  let board = await openLanes(page);
  let cell = await nextCell(board);
  const before = await cards(cell);
  const last = before[before.length - 1];

  // ⋯ Move to top.
  const card = cell.locator(`.tb-card[data-task-id="${last}"]`);
  await card.hover();
  await card.locator('.tb-card-more').click();
  await page.getByRole('menuitem', { name: 'Move to top' }).click();
  await expect.poll(() => cards(cell)).toEqual([last, ...before.slice(0, -1)]);
  await page.reload();
  board = await openLanes(page);
  cell = await nextCell(board);
  await expect.poll(() => cards(cell)).toEqual([last, ...before.slice(0, -1)]);

  // Alt+↓ with the card's title focused moves it one down and keeps the focus.
  await cell.locator(`.tb-card[data-task-id="${last}"] .tb-title`).first().focus();
  await page.keyboard.press('Alt+ArrowDown');
  await expect.poll(() => cards(cell)).toEqual([before[0], last, ...before.slice(1, -1)]);
  await expect(cell.locator(`.tb-card[data-task-id="${last}"] .tb-title`)).toBeFocused();

  // A drag onto the lower half of the last card puts it after that card.
  // Pressed on the card's edge: its title is a link, which a drag never
  // starts on.
  const now = await cards(cell);
  const from = cell.locator(`.tb-card[data-task-id="${now[0]}"]`);
  const onto = cell.locator(`.tb-card[data-task-id="${now[now.length - 1]}"]`);
  await cell.scrollIntoViewIfNeeded();
  const target = await onto.boundingBox();
  await from.dragTo(onto, { sourcePosition: { x: 3, y: 3 }, targetPosition: { x: target.width / 2, y: target.height * 0.8 } });
  await expect.poll(() => cards(cell)).toEqual([...now.slice(1), now[0]]);
  await page.reload();
  board = await openLanes(page);
  cell = await nextCell(board);
  await expect.poll(() => cards(cell)).toEqual([...now.slice(1), now[0]]);
});

test('Done says it is ordered by close time, and its cards offer no moves', async ({ page }) => {
  const board = await openLanes(page);
  await expect(board.locator('.tb-lane-columns .tb-col-note')).toHaveText('by close time');
  const done = board.locator('.tb-cell[data-column="done"] .tb-card').first();
  if (!(await done.count())) return;
  await done.hover();
  const more = done.locator('.tb-card-more');
  if (!(await more.count())) return;
  await more.click();
  await expect(page.getByRole('menuitem', { name: /^Move (to top|up|down|to bottom)$/ })).toHaveCount(0);
  await page.keyboard.press('Escape');
});
