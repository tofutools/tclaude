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

// Android's Back is not history.back(): Chrome skips every entry of a page
// that added one after a Back without a fresh tap (its history-manipulation
// intervention), so a form that re-recorded its step that way sent the next
// Back out of the app (tk_3739e29d). A smoke cannot press a phone's hardware
// Back, so it checks that rule instead: after a Back, no history entry is
// added before the next tap or key. gestureless() is every one that was.
async function watchGestureless(page) {
  await page.addInitScript(() => {
    const seen = { since: true, bad: [] };
    for (const type of ['pointerup', 'click', 'keydown', 'mousedown']) {
      addEventListener(type, (event) => { if (event.key !== 'Escape') seen.since = true; }, true);
    }
    addEventListener('popstate', () => { seen.since = false; }, true);
    const push = history.pushState.bind(history);
    history.pushState = (state, title, url) => {
      if (!seen.since) seen.bad.push(String(url));
      return push(state, title, url);
    };
    window.__gestureless = seen.bad;
  });
}
const gestureless = (page) => page.evaluate(() => window.__gestureless);

const views = (page) => page.getByRole('navigation', { name: 'Views' });
// The page behind: its path, and what the phone bar says it is.
const behind = async (page) => ({ path: new URL(page.url()).pathname, bar: (await page.locator('.phone-bar-title').first().textContent()).trim() });

// Back is the browser's: a popstate.
async function back(page) {
  await page.evaluate(() => new Promise((resolve) => {
    addEventListener('popstate', () => resolve(), { once: true });
    history.back();
  }));
}
// stepTaken waits for the form, still up, to have its step back in the
// address (?phone=N), which is what it does once Back is done with it.
const stepTaken = (page) => expect.poll(() => new URL(page.url()).searchParams.get('phone') || '').not.toBe('');

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

// A short draft's question (phone-draft-guard.js): the message sheet's rule
// for a comment, a note or an agent's prompt.
const draftAsk = (what) => (page) => sheet(page).getByRole('alertdialog', { name: `Discard ${what}?` });
const draftSheet = (what) => ({
  form: sheet,
  ask: draftAsk(what),
  keep: (page) => draftAsk(what)(page).getByRole('button', { name: 'Keep' }).click(),
  discard: (page) => draftAsk(what)(page).getByRole('button', { name: 'Discard' }).click(),
});

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
    // A Dialog's question (shellConfirmDiscard) still records a step of its
    // own right after the Back: the follow-up to tk_3739e29d.
    gesturelessKnown: true,
    open: async (page) => {
      await views(page).getByRole('button', { name: /Agents/ }).click();
      await page.getByRole('button', { name: 'New agent', exact: true }).click();
      await sheet(page).getByRole('button', { name: 'All options…' }).click();
    },
    type: (page) => page.locator('#agent-spawn-modal textarea').first().fill('do the thing'),
    ...dialogForm('agent-spawn-modal'),
  },
  {
    name: 'Agents: New agent (its prompt)',
    open: async (page) => {
      await views(page).getByRole('button', { name: /Agents/ }).click();
      await page.getByRole('button', { name: 'New agent', exact: true }).click();
    },
    type: (page) => sheet(page).getByLabel('Prompt').fill('migrate the checkout'),
    ...draftSheet('this prompt'),
  },
  {
    name: 'Board: a task › Comment',
    open: async (page) => {
      await views(page).getByRole('button', { name: /Board/ }).click();
      await page.locator('.phone-board-card').first().getByRole('button', { name: 'Comment' }).click();
    },
    type: (page) => sheet(page).getByLabel('Comment').fill('looks good, ship it'),
    ...draftSheet('this comment'),
  },
  {
    name: "Now: a decision's ⋯ (its note)",
    firstPage: true,
    open: async (page) => {
      await page.getByRole('button', { name: 'All options, a note, or dismiss' }).first().click();
    },
    type: (page) => sheet(page).getByLabel('Note to send with your choice').fill('by region, DHL first'),
    ...draftSheet('your note'),
  },
  {
    name: "Automations: a run's gate › a verdict's note",
    open: async (page) => {
      await page.goto('/automations/processes');
      await booted(page);
      // The seeded run's gate; phone-automations approves it, so after that
      // smoke on the same sandbox there is none left to write a note on.
      const run = page.getByRole('group', { name: 'Waiting on you' }).locator('.phone-run-open', { hasText: 'Release train' });
      // The list must have loaded (the seeded runs are there either way), so
      // a skip means the gate was answered, never that the page broke.
      await expect(page.locator('.phone-run-open', { hasText: 'Release train' }).first()).toBeVisible();
      test.skip(!(await run.count()), 'the seeded gate was already answered on this sandbox (phone-automations approves it)');
      await run.click();
      await page.locator('.phone-run-page [data-run-verdict][aria-haspopup="dialog"]').first().click();
    },
    type: (page) => sheet(page).getByLabel('Note for the record (optional)').fill('the checks were flaky'),
    ...draftSheet('your note'),
  },
];

for (const form of GUARDED) {
  test(`${form.name}: Back asks to discard, and the page behind never moves`, async ({ page }) => {
    const errors = collectErrors(page);
    await watchGestureless(page);
    await page.goto('/');
    await booted(page);
    await form.open(page);
    await expect(form.form(page)).toBeVisible();
    await form.type(page);
    const before = await behind(page);

    await back(page);
    await expect(form.ask(page), 'Back asks').toBeVisible();
    await stepTaken(page);
    await expect(form.form(page)).toBeVisible();
    expect(await behind(page), 'and the page behind stays').toEqual(before);

    await form.keep(page);
    await expect(form.ask(page)).toBeHidden();
    await expect(form.form(page), 'kept, the form stays').toBeVisible();
    expect(await behind(page)).toEqual(before);

    // Kept, the form took its step back: Back asks again, still in place.
    await back(page);
    await expect(form.ask(page), 'the next Back asks again').toBeVisible();
    await stepTaken(page);
    expect(await behind(page)).toEqual(before);
    // Back again while it asks is Keep (the sheet's rule; a dialog's
    // question is a layer Back cancels), and nothing behind moves.
    await back(page);
    await expect(form.ask(page), 'a Back while it asks keeps the form').toBeHidden();
    await expect(form.form(page)).toBeVisible();
    await stepTaken(page);
    expect(await behind(page), 'a Back while it asks moves nothing behind').toEqual(before);

    await back(page);
    await expect(form.ask(page)).toBeVisible();
    await stepTaken(page);
    await form.discard(page);
    await expect(form.form(page), 'discarded, the form closes').toBeHidden();
    expect(await behind(page), 'and the page behind is where it was').toEqual(before);
    if (!form.gesturelessKnown) expect(await gestureless(page), 'no entry added after a Back without a tap (Android would skip them all)').toEqual([]);

    // One more Back goes to the page before, inside tclaude (a form opened
    // on the first page has none before it).
    if (!form.firstPage) {
      const path = new URL(page.url()).pathname + new URL(page.url()).search;
      await back(page);
      await expect.poll(() => new URL(page.url()).pathname + new URL(page.url()).search, 'a further Back steps to the page before').not.toBe(path);
      await expect(views(page)).toBeVisible();
    }
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
      await page.getByRole('button', { name: 'steward: more', exact: true }).first().click();
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
    await expect.poll(() => new URL(page.url()).searchParams.get('phone') || '').toBe('');
    expect(await behind(page)).toEqual(before);
    expect(errors).toEqual([]);
  });
}
