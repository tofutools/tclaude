// New process on the desktop Groups tab (groups-processes.js,
// groups-mini-editor.js) in a real browser on the sandbox's data: ＋ ▾ then
// New process opens an untitled draft of the group's in place, as a graph,
// in two clicks. Its name is typed straight away, the first step goes on the
// empty canvas, and it saves itself. ☰ Steps and ◈ Graph show the same
// draft, ⤢ Full editor opens it over the page with ← Back inline, ▶ Run…
// opens the start form on it, a starter fills an empty one, and one closed
// untouched is not kept. SHOTS=<dir> saves a screenshot of each step.
//
//   scripts/sandbox/ui.sh smoke process-new
import { test, expect } from '@playwright/test';
import path from 'node:path';

const GROUP = 'webshop';

async function booted(page) {
  await page.waitForFunction(() => !document.body.classList.contains('dashboard-booting'), null, { timeout: 60_000 });
}

function collectErrors(page) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(`page error: ${error.message}`));
  page.on('console', (message) => {
    if (message.type() === 'error' && !/429 \/api\/companion\/presence/.test(message.text())) errors.push(`console: ${message.text()}`);
  });
  return errors;
}

function api(page, method, url) {
  return page.evaluate(async ({ method, url }) => {
    const response = await fetch(url, { method, credentials: 'same-origin' });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { method, url });
}

async function shot(page, name) {
  if (!process.env.SHOTS) return;
  await page.screenshot({ path: path.join(process.env.SHOTS, `${name}.png`), fullPage: false });
}

const template = (page, id) => api(page, 'GET', `/v1/process/templates/${encodeURIComponent(id)}`);
const steps = (view) => Object.values(view?.template?.nodes || {}).filter((node) => node.type !== 'start' && node.type !== 'end').length;

test.use({ viewport: { width: 1440, height: 900 } });
test.setTimeout(120_000);

test('＋ ▾ New process opens a group draft as a graph in two clicks; it saves itself', async ({ page }) => {
  const errors = collectErrors(page);
  const made = [];
  await page.goto('/groups');
  await booted(page);
  const section = page.locator(`[data-fused-processes="${GROUP}"]`);
  // The fixture may start the group folded: open it (not one of the two).
  if (!(await section.isVisible())) await page.locator(`strong[data-group-name="${GROUP}"]`).click();
  await expect(section).toBeVisible();
  try {
    let clicks = 0;
    const click = async (locator) => { clicks += 1; await locator.click(); };
    await click(section.locator(`[data-resource-add="${GROUP}"]`).first());
    await expect(page.locator('[data-resource-new-process]')).toBeVisible();
    await expect(page.locator('[data-resource-run-process]')).toBeVisible();
    await shot(page, '01-plus-menu');
    await click(page.locator('[data-resource-new-process]'));
    const editor = section.locator('[data-mini-editor]');
    await expect(editor).toBeVisible();
    expect(clicks, 'clicks from ＋ to the graph editor').toBe(2);
    const id = await editor.getAttribute('data-mini-editor');
    made.push(id);
    // Graph first, with Steps beside it from the first second.
    await expect(editor.getByRole('button', { name: '◈ Graph' })).toHaveAttribute('aria-pressed', 'true');
    await expect(editor.locator('[data-mini-steps]')).toBeEnabled();
    const title = editor.locator('[data-mini-title]');
    await expect(title).toBeFocused();
    await expect(title).toHaveValue('Untitled process');
    await expect(editor.locator('[data-process-node="mini-first-step"]')).toBeVisible();
    await expect(editor.locator('[data-mini-start-from]')).toBeVisible();
    // Stored at once, the group's and not the library's.
    const stored = await template(page, id);
    expect(stored.status).toBe(200);
    expect(stored.body.listed).toBe(false);
    await shot(page, '02-new-draft');

    // Typed over at once; Enter keeps it, and it saves itself.
    await page.keyboard.type('Ship a feature');
    await page.keyboard.press('Enter');
    await editor.locator('[data-process-node="mini-first-step"]').click();
    await page.locator('[data-mini-picker] [data-mini-kind="agent"]').click();
    await expect(editor.locator('[data-mini-inspector]')).toBeVisible();
    await expect(editor.locator('[data-mini-start-from]')).toHaveCount(0);
    await expect.poll(async () => {
      const view = (await template(page, id)).body;
      return [view?.template?.name, steps(view)];
    }, { timeout: 10_000 }).toEqual(['Ship a feature', 1]);
    await expect(editor.locator('[data-mini-autosave]')).toHaveText(`draft · autosaved to ${GROUP}`);
    await shot(page, '03-first-step');

    // ☰ Steps is the same draft, and so is ◈ Graph again.
    await editor.locator('[data-mini-steps]').click();
    const list = section.locator(`[data-step-editor-saved="${id}"]`);
    await expect(list).toBeVisible();
    await expect(list.locator('.gq-name')).toHaveValue('Ship a feature');
    await shot(page, '04-steps');
    await list.locator('[data-mini-graph]').click();
    await expect(section.locator(`[data-mini-editor="${id}"]`)).toBeVisible();

    // ⤢ Full editor opens it over the page; ← Back inline brings it back.
    await section.locator('[data-mini-full]').click();
    const back = page.locator(`[data-process-back-inline="${GROUP}"]`);
    await expect(back).toBeVisible();
    await expect(page.locator(`[data-process-run-in="${GROUP}"]`)).toBeVisible();
    await shot(page, '05-full-editor');
    await back.click();
    await expect(back).toHaveCount(0);
    await expect(section.locator(`[data-mini-editor="${id}"]`)).toBeVisible();

    // ▶ Run… opens the start form on it, for the group or one agent.
    await section.locator('[data-mini-run]').click();
    const form = section.locator('form.gp-start-form');
    await expect(form).toBeVisible();
    await expect(form.locator('[data-start-scope-pick="group"]')).toHaveAttribute('aria-pressed', 'true');
    await expect(form.locator('.gp-start-template input')).toHaveValue('Ship a feature');
    await shot(page, '06-run');
    await form.locator('[data-start-scope-pick="agent"]').click();
    await expect(form).toHaveAttribute('data-start-scope', 'agent');
    await form.getByRole('button', { name: 'cancel' }).click();

    // A starter fills an empty draft and names it.
    await section.locator(`[data-resource-add="${GROUP}"]`).first().click();
    await page.locator('[data-resource-new-process]').click();
    const second = section.locator('[data-mini-editor]');
    await expect(second).toBeVisible();
    const id2 = await second.getAttribute('data-mini-editor');
    made.push(id2);
    await second.locator('[data-mini-starter="build-review-ship"]').click();
    await expect(second.locator('[data-mini-title]')).toHaveValue('Build → review → ship');
    await expect.poll(async () => steps((await template(page, id2)).body), { timeout: 10_000 }).toBe(4);
    await shot(page, '07-starter');
    await second.locator('[data-mini-close]').click();
    await expect(second).toHaveCount(0);

    // One closed untouched is not kept.
    await section.locator(`[data-resource-add="${GROUP}"]`).first().click();
    await page.locator('[data-resource-new-process]').click();
    const third = section.locator('[data-mini-editor]');
    await expect(third).toBeVisible();
    const id3 = await third.getAttribute('data-mini-editor');
    made.push(id3);
    await third.locator('[data-mini-close]').click();
    // Read from the listing: a read of the gone template is a console 404.
    await expect.poll(async () => ((await api(page, 'GET', '/v1/process/templates')).body?.templates || []).some((row) => row.id === id3),
      { timeout: 10_000 }).toBe(false);
    expect(errors).toEqual([]);
  } finally {
    const left = ((await api(page, 'GET', '/v1/process/templates')).body?.templates || []).map((row) => row.id);
    for (const id of made.filter((made) => left.includes(made))) await api(page, 'DELETE', `/v1/process/templates/${encodeURIComponent(id)}`);
  }
});
