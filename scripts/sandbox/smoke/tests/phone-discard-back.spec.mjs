// Back over a phone form holding a draft (js/phone-history.js,
// js/back-layers.js), in a real browser, for every guarded form a phone
// reaches. The rule: Back asks "Discard?" and the page behind never moves.
// Kept, the form stays and Back asks again; discarded, the form closes and
// the page behind is where it was. A form with no guard (a one-line sheet)
// just closes on Back, and the page behind stays too.
//
//   scripts/sandbox/ui.sh smoke phone-discard-back
import { test, expect, devices } from '@playwright/test';

const { defaultBrowserType: _ignored, ...IPHONE } = devices['iPhone 13'];
test.use(IPHONE);

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

const views = (page) => page.getByRole('navigation', { name: 'Views' });
// The page behind: its path, and what the phone bar says it is.
const behind = async (page) => ({ path: new URL(page.url()).pathname, bar: (await page.locator('.phone-bar-title').first().textContent()).trim() });

// Back is the browser's: a popstate, and a moment for what it sets off (a
// held form taking its step back) to land.
async function back(page) {
  await page.evaluate(() => new Promise((resolve) => {
    addEventListener('popstate', () => resolve(), { once: true });
    history.back();
  }));
  await page.waitForTimeout(250);
}

const sheet = (page) => page.locator('.phone-sheet[role=dialog]');
const sheetAsk = (page) => sheet(page).getByText('Discard this message?');
// The compose sheet's question: Keep is its Cancel.
const composeSheet = {
  form: sheet,
  ask: sheetAsk,
  keep: (page) => sheet(page).getByRole('button', { name: 'Keep' }).click(),
  discard: (page) => sheet(page).getByRole('button', { name: 'Discard' }).click(),
  field: (page) => sheet(page).locator('textarea.phone-compose-body'),
};
// A dialog's question is the shared confirmation (shellConfirmDiscard).
const confirm = (page) => page.getByRole('dialog', { name: 'Discard input?' });
const dialogForm = (id) => ({
  form: (page) => page.locator(`#${id}`),
  ask: confirm,
  keep: (page) => confirm(page).getByRole('button', { name: 'Cancel' }).click(),
  discard: (page) => confirm(page).getByRole('button', { name: 'Discard' }).click(),
});

const openMail = async (page) => {
  await views(page).getByRole('button', { name: /Mail/ }).click();
  await page.getByRole('group', { name: 'Mailboxes' }).getByRole('button', { name: /^Human/ }).click();
};
const openTerminal = async (page) => {
  await views(page).getByRole('button', { name: /Agents/ }).click();
  await page.getByRole('button', { name: /^lead, idle.*open its terminal/ }).click();
  await expect(page.locator('.phone-term[role=dialog]')).toBeVisible();
};

// GUARDED is every phone form that asks before dropping a draft.
const GUARDED = [
  {
    name: 'Mail: Reply',
    open: async (page) => {
      await openMail(page);
      await page.locator('.phone-mail-open').first().click();
      await page.locator('.phone-mail-reply').click();
    },
    type: (page) => composeSheet.field(page).fill('a reply'),
    ...composeSheet,
  },
  {
    name: 'Mail: Write',
    open: async (page) => { await openMail(page); await page.locator('.phone-mail-write').click(); },
    type: (page) => composeSheet.field(page).fill('a new message'),
    ...composeSheet,
  },
  {
    name: "Terminal: ✉ Message",
    open: async (page) => { await openTerminal(page); await page.locator('.phone-term-compose').click(); },
    type: (page) => composeSheet.field(page).fill('a message from the terminal'),
    ...composeSheet,
  },
  {
    name: "Agents: an agent's ⋯ › Message",
    open: async (page) => {
      await views(page).getByRole('button', { name: /Agents/ }).click();
      await page.getByRole('button', { name: 'lead: more' }).click();
      await sheet(page).locator('.phone-agent-tile[data-act="message"]').click();
    },
    type: (page) => composeSheet.field(page).fill('a message from the sheet'),
    ...composeSheet,
  },
  {
    name: 'Agents: New agent › All options…',
    open: async (page) => {
      await views(page).getByRole('button', { name: /Agents/ }).click();
      await page.getByRole('button', { name: 'New agent' }).click();
      await sheet(page).getByRole('button', { name: 'All options…' }).click();
    },
    type: (page) => page.locator('#agent-spawn-modal textarea').first().fill('do the thing'),
    ...dialogForm('agent-spawn-modal'),
  },
];

for (const form of GUARDED) {
  test(`${form.name}: Back asks to discard, and the page behind never moves`, async ({ page }) => {
    const errors = collectErrors(page);
    await page.goto('/');
    await booted(page);
    await form.open(page);
    await expect(form.form(page)).toBeVisible();
    await form.type(page);
    const before = await behind(page);

    await back(page);
    await expect(form.ask(page), 'Back asks').toBeVisible();
    await expect(form.form(page)).toBeVisible();
    expect(await behind(page), 'and the page behind stays').toEqual(before);

    await form.keep(page);
    await expect(form.ask(page)).toBeHidden();
    await expect(form.form(page), 'kept, the form stays').toBeVisible();
    expect(await behind(page)).toEqual(before);

    // Kept, the form took its step back: Back asks again, still in place.
    await back(page);
    await expect(form.ask(page), 'the next Back asks again').toBeVisible();
    expect(await behind(page)).toEqual(before);
    // Back again while it asks keeps the draft (the sheet's rule) or leaves
    // the question up (a dialog's); either way nothing behind moves.
    await back(page);
    expect(await behind(page), 'a Back while it asks moves nothing behind').toEqual(before);
    await expect(form.form(page)).toBeVisible();
    if (!(await form.ask(page).isVisible())) await back(page);

    await expect(form.ask(page)).toBeVisible();
    await form.discard(page);
    await expect(form.form(page), 'discarded, the form closes').toBeHidden();
    expect(await behind(page), 'and the page behind is where it was').toEqual(before);
    expect(errors).toEqual([]);
  });
}

// UNGUARDED forms keep no draft worth asking about: Back closes them, and
// only them.
const UNGUARDED = [
  {
    name: 'Terminal: › Type into the shell',
    open: async (page) => {
      await views(page).getByRole('button', { name: /Agents/ }).click();
      await page.getByRole('button', { name: 'steward: more' }).first().click();
      await sheet(page).locator('[data-act="shell"]').first().click();
      await page.locator('.phone-term-type').click();
    },
    field: (page) => sheet(page).locator('textarea'),
  },
  {
    name: 'Board: Add a task',
    open: async (page) => {
      await views(page).getByRole('button', { name: /Board/ }).click();
      await page.getByRole('button', { name: 'Add a task' }).click();
    },
    field: (page) => sheet(page).locator('input, textarea').first(),
  },
];

for (const form of UNGUARDED) {
  test(`${form.name}: Back closes it, and the page behind stays`, async ({ page }) => {
    const errors = collectErrors(page);
    await page.goto('/');
    await booted(page);
    await form.open(page);
    await form.field(page).fill('echo hi');
    const before = await behind(page);
    await back(page);
    await expect(sheet(page)).toBeHidden();
    expect(await behind(page)).toEqual(before);
    expect(errors).toEqual([]);
  });
}
