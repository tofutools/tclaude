// A task's phone page (js/phone-task-page.js, phone-task-parts.js) shows
// what the desktop's task form shows of who worked on it, its files and its
// comments: People (the assignee first, a tap opens its terminal), the
// images the brief names, the task's files, and a comment with its own file
// and "edited". The seeded redesign task gets a screenshot the brief names
// and an edited comment with a file; then its desktop form and its phone
// page are shot. SHOTS=<dir> saves both (BEFORE=1 skips the new assertions,
// for main).
//
//   scripts/sandbox/ui.sh smoke phone-task-parity
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { test, expect, devices } from '@playwright/test';

test.setTimeout(120_000);

const fixture = JSON.parse(readFileSync(process.env.SANDBOX_FIXTURE_JSON, 'utf8'));
const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
const BEFORE = !!process.env.BEFORE;
const shot = (page, name, opts = {}) => (process.env.SHOTS
  ? page.screenshot({ path: path.join(process.env.SHOTS, `${BEFORE ? 'before' : 'after'}-${name}.png`), ...opts }) : null);

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

// seed gives the task an image its brief names and an edited comment with a
// file, once per sandbox.
async function seed(page) {
  const task = `/v1/taskboard/tasks/${fixture.task}`;
  const read = await (await page.request.get(task)).json();
  if ((read.attachments || []).some((a) => a.name === 'address-step.png')) return;
  await page.setViewportSize({ width: 640, height: 360 });
  await page.goto('/');
  await booted(page);
  const png = await page.screenshot();
  let res = await page.request.post(`${task}/attachments`, { multipart: { file: { name: 'address-step.png', mimeType: 'image/png', buffer: png } } });
  expect(res.ok(), await res.text()).toBeTruthy();
  res = await page.request.patch(task, { data: { body: `${read.body || ''}\n\n![The address step](address-step.png)` } });
  expect(res.ok(), await res.text()).toBeTruthy();
  res = await page.request.post(`${task}/comments`, { multipart: {
    comment: { name: 'comment.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify({ body: 'Trace from the failing run.' })) },
    file: { name: 'checkout-trace.txt', mimeType: 'text/plain', buffer: Buffer.from('POST /checkout 502\n') },
  } });
  expect(res.ok(), await res.text()).toBeTruthy();
  const comment = await res.json();
  const id = comment.comment?.id;
  res = await page.request.patch(`${task}/comments/${id}`, { data: { body: 'Trace from the failing run (502 on POST /checkout).' } });
  expect(res.ok(), await res.text()).toBeTruthy();
}

test.describe('desktop', () => {
  test.use({ viewport: { width: 1280, height: 2600 } });
  test("the task's desktop form, for the composite", async ({ page }) => {
    await seed(page);
    await page.setViewportSize({ width: 1280, height: 2600 });
    await page.goto(`/?task=${fixture.task}:${fixture.group}`);
    const dialog = page.getByRole('dialog').first();
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText('Trace from the failing run');
    await dialog.getByRole('tab', { name: 'Preview' }).first().click();
    await page.waitForTimeout(800);
    await shot(page, 'desktop');
  });
});

test.describe('phone', () => {
  test.use(IPHONE);
  test("the task's phone page: People, brief image, Files and the comment's file", async ({ page }) => {
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await seed(page);
    await page.goto('/');
    await booted(page);
    await page.locator('.phone-nav').getByRole('button', { name: /Board/ }).click();
    const title = (await (await page.request.get(`/v1/taskboard/tasks/${fixture.task}`)).json()).title;
    await page.getByRole('tab', { name: /^In review/ }).first().click();
    await page.locator(`[data-task-id="${fixture.task}"] .phone-board-open`).click();
    const view = page.locator('.phone-task-page');
    await expect(view.locator('.phone-task-page-title')).toHaveText(title);
    await expect(view).toContainText('Trace from the failing run');
    if (!BEFORE) {
      const people = view.getByRole('group', { name: 'People' });
      await expect(people.locator('.phone-task-person').first()).toContainText('assignee');
      await expect(view.locator('.phone-task-md img[alt="The address step"]')).toBeVisible();
      const files = view.getByRole('group', { name: 'Files' });
      await expect(files).toContainText('notes.md');
      await expect(files).toContainText('address-step.png');
      await expect(view.locator('.phone-task-comment-files')).toContainText('checkout-trace.txt');
      await expect(view.locator('.phone-task-activity')).toContainText('edited');
      // The width holds: nothing scrolls sideways.
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
    }
    // The page scrolls inside its view: a tall phone shows all of it.
    await page.setViewportSize({ width: 390, height: 3000 });
    await page.waitForTimeout(600);
    await shot(page, 'phone');
    if (!BEFORE) {
      // A tap on the assignee opens its terminal.
      await view.getByRole('group', { name: 'People' }).locator('.phone-task-person-open').first().click();
      await expect(page.locator('.phone-task-page')).toHaveCount(0, { timeout: 10_000 }).catch(() => {});
      await shot(page, 'phone-jump');
    }
    expect(errors).toEqual([]);
  });
});
