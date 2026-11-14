// Grouping a task board column by readiness (groups-taskboard-readiness.js):
// Next's ⋯ Group by readiness draws the ready cards first and the queued
// ones under a Queued heading, writes nothing, is kept across a reload, and
// its head chip puts the board's order back. Drives the webshop board's
// Lanes in the real dashboard.
//
//   scripts/sandbox/ui.sh smoke taskboard-readiness
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { test, expect } from '@playwright/test';

test.use({ viewport: { width: 1440, height: 1400 } });

const fixture = JSON.parse(readFileSync(process.env.SANDBOX_FIXTURE_JSON, 'utf8'));
const section = `[data-fused-taskboard="${fixture.group}"]`;

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

// The Unassigned Next cell, in the lanes by agent or in triage.
const unassignedNext = (board) => board.locator('.tb-cell[data-column="next"]:is([data-lane="unassigned"], [data-board-row="unassigned"])');
const cards = (cell) => cell.locator('.tb-card').evaluateAll((els) => els.map((el) => el.dataset.taskId));

// openAll shows every card of a cell past its cap.
async function openAll(cell) {
  const more = cell.locator('.tb-cell-more[aria-expanded="false"]');
  if (await more.count()) await more.click();
}

test('Next groups by readiness: ready first, queued under its heading, kept across a reload, nothing written', async ({ page }) => {
  let board = await openLanes(page);
  let cell = unassignedNext(board);
  await expect.poll(async () => (await cards(cell)).length).toBeGreaterThanOrEqual(3);
  await openAll(cell);
  const before = await cards(cell);
  // The second card waits on a task of another lane: it is now not ready.
  const queued = before[1];
  const res = await page.request.patch(`/v1/taskboard/tasks/${queued}`, { data: { add_deps: [fixture.task] } });
  expect(res.ok()).toBeTruthy();
  await page.reload();
  board = await openLanes(page);
  cell = unassignedNext(board);
  await openAll(cell);
  await expect.poll(() => cards(cell)).toEqual(before);

  const head = board.locator('.tb-lane-columns [data-col="next"]');
  await head.locator('.tb-col-more').click();
  await page.getByRole('menuitem', { name: 'Group by readiness' }).click();
  await openAll(cell);
  const grouped = [...before.filter((id) => id !== queued), queued];
  await expect.poll(() => cards(cell)).toEqual(grouped);
  await expect(cell.locator('[data-tb-ready-head="queued"]')).toBeVisible();
  await expect(head.locator('[data-tb-ready="next"]')).toBeVisible();
  if (process.env.SANDBOX_OUT) await board.locator('.tb-lanes').screenshot({ path: join(process.env.SANDBOX_OUT, 'taskboard-readiness-lanes.png') });

  // Kept across a reload, and the board's own order is untouched.
  await page.reload();
  board = await openLanes(page);
  cell = unassignedNext(board);
  await openAll(cell);
  await expect.poll(() => cards(cell)).toEqual(grouped);

  // The chip draws the board's order again.
  await board.locator('.tb-lane-columns [data-tb-ready="next"]').click();
  await openAll(cell);
  await expect.poll(() => cards(cell)).toEqual(before);
});

test('the List groups each section by readiness from the Filter', async ({ page }) => {
  await page.goto(`/?task=${fixture.task}:${fixture.group}`);
  await expect(page.getByRole('dialog')).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  const board = page.locator(section).first();
  await board.getByRole('button', { name: /List/ }).first().click();
  await board.locator('.tb-list-filter').click();
  await page.getByLabel('group by readiness').check();
  await expect(board.locator('tr.tb-ready-row').first()).toBeVisible();
  if (process.env.SANDBOX_OUT) await board.locator('.tb-table').first().screenshot({ path: join(process.env.SANDBOX_OUT, 'taskboard-readiness-list.png') });
  await page.getByLabel('group by readiness').uncheck();
  await expect(board.locator('tr.tb-ready-row')).toHaveCount(0);
});
