// A question card's "asks on <task title>" link wraps inside the card at a
// narrow column, clamped to two lines with the full title in its tooltip
// (tk_7760b2f1). The card is the real QuestionCard
// (js/groups-taskboard-answer.js) under the dashboard's own stylesheets,
// mounted in a lane-cell-wide column: the fixture has no agent task with an
// open question.
//
//   scripts/sandbox/ui.sh smoke question-card-wrap
import { test, expect } from '@playwright/test';

const TITLE = 'Migrate the international shipping rate tables to the new carrier aggregation service before the holiday freeze';

test('a long task title on a question card wraps inside the card', async ({ page }) => {
  await page.goto('/groups');
  await page.evaluate(async (title) => {
    const { h, render } = await import('preact');
    const { QuestionCard } = await import('/static/js/groups-taskboard-answer.js');
    const column = document.createElement('div');
    column.id = 'qc-probe';
    column.style.cssText = 'position:fixed;left:20px;top:80px;width:150px;z-index:99999;background:#0d1117;padding:6px';
    document.body.appendChild(column);
    render(h(QuestionCard, {
      task: { id: 'tk_probe', title, status: 'in_progress' },
      question: { n: 1, kind: 'question', title: 'Which aggregator?', options: [{ key: 'a', label: 'A' }] },
      askedBy: 'lead', onOpen: () => {}, onAnswer: () => {},
    }), column);
  }, TITLE);

  const card = page.locator('#qc-probe .tb-question-card');
  const link = card.locator('.tb-question-task');
  await expect(link).toBeVisible();
  await expect(link).toHaveAttribute('title', `Open the task: ${TITLE}`);
  const c = await card.boundingBox();
  const l = await link.boundingBox();
  expect(l.x + l.width, 'the title stays inside the card').toBeLessThanOrEqual(c.x + c.width + 0.5);
  const lineHeight = await link.evaluate((el) => parseFloat(getComputedStyle(el).lineHeight) || 16);
  expect(l.height, 'wrapped, at most two lines').toBeGreaterThan(lineHeight * 1.5);
  expect(l.height).toBeLessThanOrEqual(lineHeight * 2 + 1);
  await page.locator('#qc-probe').screenshot({ path: test.info().outputPath('question-card.png') });
});
