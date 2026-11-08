// The Markdown editor (js/markdown-editor.js) where a brief is written: the
// task edit form and the New task form. Typing and the toolbar edit the text,
// Preview renders it and Write comes back, the box keeps its height across
// both, and dragging its corner still resizes it. No page errors throughout.
//
//   scripts/sandbox/ui.sh smoke markdown-editor
import { readFileSync } from 'node:fs';
import { test, expect } from '@playwright/test';

const fixture = process.env.SANDBOX_FIXTURE_JSON
  ? JSON.parse(readFileSync(process.env.SANDBOX_FIXTURE_JSON, 'utf8'))
  : null;

test.skip(!fixture, 'needs the full fixture (scripts/sandbox/ui.sh smoke)');

// Fail the test on any page error.
let pageErrors = [];
test.beforeEach(({ page }) => {
  pageErrors = [];
  page.on('pageerror', (err) => pageErrors.push(err.message));
});
test.afterEach(() => {
  expect(pageErrors, 'page errors').toEqual([]);
});

// briefEditor is a form's Brief editor, by the form's class for it (the edit
// form also has a Comment editor, and Preview hides the textbox itself).
async function briefEditor(page, cls) {
  const editor = page.getByRole('dialog').locator(`.md-ed.${cls}`);
  await expect(editor).toBeVisible();
  return editor;
}

// openTaskEditor opens the seeded redesign task's edit form by its link.
async function openTaskEditor(page) {
  await page.goto(`/?task=${fixture.task}:${fixture.group}`);
  const editor = await briefEditor(page, 'tb-edit-body');
  await expect(editor.locator('.cm-content')).toContainText('The address step is done');
  return editor;
}

// openNewTask opens the New task form from the board's ＋ menu.
async function openNewTask(page) {
  // The task link unfolds the group and its board; close its form first.
  await openTaskEditor(page);
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await page.locator(`[data-taskboard-new="${fixture.group}"]`).first().click();
  await page.locator('[data-taskboard-new-form]').click();
  return briefEditor(page, 'tb-new-body');
}

const height = async (box) => (await box.boundingBox()).height;

// exercise drives one editor: type, format, preview and back, resize.
async function exercise(page, editor) {
  const content = editor.locator('.cm-content');
  await expect(editor).toHaveClass(/md-ed-cm/); // CodeMirror took over from the textarea

  // Typing, then the toolbar: select the last word and make it bold.
  await content.click();
  await page.keyboard.press('ControlOrMeta+End');
  await page.keyboard.type('\n\nShip it today');
  for (let i = 0; i < 'today'.length; i++) await page.keyboard.press('Shift+ArrowLeft');
  await editor.locator('[data-md-tool="bold"]').click();
  await expect(content).toContainText('Ship it **today**');
  await editor.locator('[data-md-tool="bullets"]').click();
  await expect(content).toContainText('- Ship it **today**');

  // Preview renders it, in a box of the same height; Write comes back.
  const before = await height(editor);
  await editor.getByRole('tab', { name: 'Preview' }).click();
  const view = editor.locator('.md-ed-view');
  await expect(view.locator('li strong', { hasText: 'today' })).toBeVisible();
  await expect(editor.locator('.md-ed-edit')).toBeHidden();
  expect(Math.abs((await height(editor)) - before), 'Preview keeps the height').toBeLessThanOrEqual(1);
  await editor.getByRole('tab', { name: 'Write' }).click();
  await expect(content).toBeVisible();
  await expect(content).toContainText('- Ship it **today**');
  expect(Math.abs((await height(editor)) - before), 'Write keeps the height').toBeLessThanOrEqual(1);

  // The corner still resizes it, and Preview keeps the new height.
  const box = await editor.boundingBox();
  await page.mouse.move(box.x + box.width - 3, box.y + box.height - 3);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width - 3, box.y + box.height + 117, { steps: 8 });
  await page.mouse.up();
  const resized = await height(editor);
  expect(resized, 'dragging the corner makes it taller').toBeGreaterThan(before + 60);
  await editor.getByRole('tab', { name: 'Preview' }).click();
  expect(Math.abs((await height(editor)) - resized), 'Preview keeps the resized height').toBeLessThanOrEqual(1);
  await editor.getByRole('tab', { name: 'Write' }).click();
  expect(Math.abs((await height(editor)) - resized), 'Write keeps the resized height').toBeLessThanOrEqual(1);
}

test('the task edit form: typing, toolbar, preview and resize', async ({ page }) => {
  await exercise(page, await openTaskEditor(page));
});

test('the New task form: typing, toolbar, preview and resize', async ({ page }) => {
  await exercise(page, await openNewTask(page));
});
