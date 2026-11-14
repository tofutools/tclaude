// Quick reloads must not use up the daemon's 32 dashboard presence slots
// (tk_66f0cc7b). A tab keeps its presence id across reloads (sessionStorage),
// a duplicated tab takes an id of its own, and when the table fills anyway the
// daemon forgets the tab heard from least recently instead of answering 429.
//
//   scripts/sandbox/ui.sh smoke presence-reload
import { test, expect } from '@playwright/test';

const KEY = 'tclaude.dash.presence-tab';

// watchPresence records every presence ping a page sends and its status.
function watchPresence(page) {
  const seen = [];
  page.on('response', async (response) => {
    const request = response.request();
    if (!request.url().endsWith('/api/companion/presence')) return;
    let body = {};
    try { body = JSON.parse(request.postData() || '{}'); } catch { /* a beacon */ }
    seen.push({ status: response.status(), ...body });
  });
  return seen;
}

// load (re)loads the page and waits for the new page load's first presence
// ping to answer: a late ping from the page it replaces carries a load id seen
// before, and does not count.
const loadsSeen = new WeakMap();
async function load(page, go) {
  if (!loadsSeen.has(page)) loadsSeen.set(page, new Set());
  const known = loadsSeen.get(page);
  const answered = page.waitForResponse((r) => {
    if (!r.url().endsWith('/api/companion/presence') || r.request().method() !== 'POST') return false;
    const body = JSON.parse(r.request().postData() || '{}');
    return !body.gone && !!body.load && !known.has(body.load);
  });
  await go();
  const response = await answered;
  known.add(JSON.parse(response.request().postData()).load);
  return response;
}

test('40 quick reloads keep one presence id and never get 429', async ({ page }) => {
  const seen = watchPresence(page);
  await load(page, () => page.goto('/groups'));
  for (let i = 0; i < 40; i++) await load(page, () => page.reload());
  const pings = seen.filter((p) => !p.gone);
  expect(pings.length).toBeGreaterThanOrEqual(41);
  expect(pings.filter((p) => p.status !== 200), 'no presence ping is refused').toEqual([]);
  expect(new Set(pings.map((p) => p.client)).size, 'every reload reports as the same tab').toBe(1);
  expect(new Set(pings.map((p) => p.load)).size, 'each page load is told apart').toBeGreaterThanOrEqual(41);
});

test('a page without a saved id (an older page) still never gets 429', async ({ page }) => {
  const seen = watchPresence(page);
  await load(page, () => page.goto('/groups'));
  for (let i = 0; i < 40; i++) {
    await page.evaluate((key) => sessionStorage.removeItem(key), KEY);
    await load(page, () => page.reload());
  }
  const pings = seen.filter((p) => !p.gone);
  expect(pings.filter((p) => p.status !== 200), 'a full table evicts instead of refusing').toEqual([]);
  expect(new Set(pings.map((p) => p.client)).size).toBeGreaterThanOrEqual(41);
});

test('a duplicated tab takes a presence id of its own', async ({ page, context }) => {
  const seen = watchPresence(page);
  await load(page, () => page.goto('/groups'));
  const original = seen.find((p) => !p.gone).client;
  // "Duplicate tab" copies session storage into the new tab before it loads.
  const storage = await page.evaluate(() => JSON.stringify({ ...sessionStorage }));
  const copy = await context.newPage();
  await copy.addInitScript((saved) => {
    if (sessionStorage.length) return;
    for (const [key, value] of Object.entries(JSON.parse(saved))) sessionStorage.setItem(key, value);
  }, storage);
  const copySeen = watchPresence(copy);
  await load(copy, () => copy.goto('/groups'));
  const duplicate = copySeen.find((p) => !p.gone).client;
  expect(duplicate).not.toBe(original);

  // Both keep their own id through a reload.
  await load(copy, () => copy.reload());
  await load(page, () => page.reload());
  expect(copySeen.filter((p) => !p.gone).at(-1).client).toBe(duplicate);
  expect(seen.filter((p) => !p.gone).at(-1).client).toBe(original);
  await copy.close();
});
