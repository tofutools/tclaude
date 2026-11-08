// The route tour: every top-level page and sub-page of the dashboard, loaded
// fresh against the sandbox's `full` fixture. It fails on a page error, a
// console error, a page that renders nothing, or an empty state the fixture
// should have filled. It guards the fixture against drift: a new page or tab is
// toured automatically (the list comes from the router), so it fails here until
// the fixture seeds it or ALLOW says why it stays empty.
//
//   scripts/sandbox/ui.sh tour
import { readFileSync } from 'node:fs';
import { test, expect } from '@playwright/test';

// The seeded entities the deep links open, written by the sandbox server
// (TestSandboxServe) for the full fixture; absent for the docshots fixtures.
const fixture = process.env.SANDBOX_FIXTURE_JSON
  ? JSON.parse(readFileSync(process.env.SANDBOX_FIXTURE_JSON, 'utf8'))
  : null;

// Tabs the tour skips, each with its reason.
const SKIP_TABS = {
  vegas: 'an easter egg behind a hotkey, not a page of the dashboard',
};

// Empty states that are expected in the sandbox, by route (the path the tour
// visits) and a pattern of the empty state's text. Each needs a reason; keep
// this list short, since every entry is a page the sandbox cannot show.
const NO_HUB = 'the sandbox has no federation hub and its one peer has no live node behind it';
const ALLOW = [
  // Exactly this: a peer never seen has no date (tk_7dcf4f84).
  { route: '/map', text: /^Unreachable\.$/, why: NO_HUB },
  { route: '/fleet-admin/peers', text: /No other instances visible on the hub/, why: NO_HUB },
  { route: '/fleet-admin/spawns', text: /No open spawn requests from peers|Nothing sent yet/, why: NO_HUB },
  { route: '/fleet-admin/invites', text: /Invites need a node profile/, why: NO_HUB },
  { route: '/fleet-admin/models', text: /No model gateways yet|No leases|No model requests on/, why: NO_HUB },
  { route: '/fleet-admin/sharing', text: /Federation is off|You share no model gateway/, why: NO_HUB },
  { route: '/fleet-admin/profiles', text: /No profiles yet/, why: NO_HUB },
  { route: '/fleet-admin/offers', text: /No offers (from peers|sent)/, why: NO_HUB },
  { route: '/fleet-admin/jobs', text: /No remote jobs|No repositories allowed/, why: NO_HUB },
  { route: '/fleet-admin/moves', text: /No agent moves or teleports/, why: NO_HUB },
  { route: '/fleet-admin/audit', text: /No federation activity in this window/, why: NO_HUB },
  { route: '/diagnostics/performance', text: /No captures yet/, why: 'a CPU capture needs a busy live daemon; the sandbox serves from a test binary' },
];

// Console messages that are not the page's fault, each with its reason.
const ALLOW_CONSOLE = [
  // { text: /pattern/, why: '...' },
];

// Failed requests (status 400 and up) that are expected in the sandbox, by a
// pattern of "<status> <path>", each with its reason.
const ALLOW_HTTP = [
  { text: /^409 \/api\/federation\/(away|boards)$/, why: NO_HUB },
  { text: /^502 \/api\/peer\/inst_[a-z0-9]+\//, why: NO_HUB },
  { text: /^429 \/api\/companion\/presence$/, why: 'every route is a fresh page load, faster than the 32-tab presence table ages tabs out' },
];

// Routes the router sends elsewhere on purpose, each with its reason. A route
// that lands on its own default sub-view (/config → /config/appearance) needs
// no entry.
const REDIRECTS = {
  '/drawing-boards': { to: '/terminals', why: 'a bare Collab selector opens the Collab home; the deep link below opens a board' },
  '/collab/processes': { to: '/terminals', why: 'a bare Collab selector opens the Collab home; the template deep link opens the editor' },
};

// A route renders at least this much visible text in its section.
const MIN_TEXT = 40;

const EMPTY_SELECTOR = '.empty, .ui-empty, .ui-empty-block';

async function routes(page) {
  await page.goto('/');
  return page.evaluate(async ({ skip, fx }) => {
    const nav = await import('/static/js/nav-history-core.js');
    const out = [];
    for (const tab of nav.KNOWN_TABS) {
      if (skip.includes(tab)) continue;
      out.push({ tab, path: nav.toPath({ tab }) });
      for (const subtab of nav.KNOWN_SUBTABS[tab] || []) {
        out.push({ tab, subtab, path: nav.toPath({ tab, subtab }) });
      }
    }
    // Deep links into the seeded world: each must show its entity.
    if (fx) {
      out.push(
        { path: `/?task=${fx.task}:${fx.group}`, expect: 'Checkout page redesign' },
        { path: `/?task=${fx.questionTask}:${fx.group}`, expect: 'Which carrier for international shipping?' },
        { path: nav.toPath({ tab: 'drawing-boards', selection: fx.drawingBoard }), expect: 'Checkout flow' },
        { path: nav.toPath({ tab: 'groups', subtab: 'runs', selection: fx.processRun }), expect: 'Release train' },
        { path: nav.toPath({ tab: 'processes', subtab: 'templates', selection: fx.template }), expect: 'Record outcome' },
      );
    }
    return out;
  }, { skip: Object.keys(SKIP_TABS), fx: fixture });
}

// settle waits until the active section's text stops changing: the dashboard
// fills in over several polls, and it never goes network-idle.
async function settle(page) {
  let last = -1;
  let stable = 0;
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    const len = await page.evaluate(() => document.querySelector('section[id^="tab-"].active')?.innerText.length ?? -1);
    stable = len === last && len > 0 ? stable + 1 : 0;
    if (stable >= 3) return;
    last = len;
    await page.waitForTimeout(250);
  }
}

test('every route renders content without errors', async ({ page }) => {
  const list = await routes(page);
  expect(list.length, 'the router lists its routes').toBeGreaterThan(10);

  const problems = [];
  let route = '';
  page.on('pageerror', (err) => problems.push(`${route}: page error: ${err.message}`));
  page.on('console', (msg) => {
    if (msg.type() !== 'error') return;
    const text = msg.text();
    // The response listener reports these with their URL.
    if (/^Failed to load resource:/.test(text)) return;
    if (ALLOW_CONSOLE.some((a) => a.text.test(text))) return;
    problems.push(`${route}: console error: ${text}`);
  });

  page.on('response', (res) => {
    if (res.status() < 400) return;
    const u = new URL(res.url());
    const text = `${res.status()} ${u.pathname}`;
    if (ALLOW_HTTP.some((a) => a.text.test(text))) return;
    problems.push(`${route}: HTTP ${text}`);
  });

  page.on('requestfailed', (req) => {
    const failure = req.failure()?.errorText || 'failed';
    // A navigation cancels the previous page's in-flight polls.
    if (failure === 'net::ERR_ABORTED') return;
    problems.push(`${route}: request failed: ${failure} ${new URL(req.url()).pathname}`);
  });

  for (const r of list) {
    route = r.path;
    await page.goto(r.path);
    await settle(page);
    const seen = await page.evaluate(({ selector }) => {
      const section = document.querySelector('section[id^="tab-"].active');
      if (!section) return { section: null };
      const visible = (el) => el.offsetParent !== null && el.getClientRects().length > 0;
      const empties = [...section.querySelectorAll(selector)]
        .filter(visible)
        .map((el) => el.innerText.trim().replace(/\s+/g, ' ').slice(0, 120))
        .filter(Boolean);
      return { section: section.id, text: section.innerText.trim().length, empties, url: location.pathname };
    }, { selector: EMPTY_SELECTOR });

    await page.screenshot({ path: test.info().outputPath(`${r.path.replace(/\W+/g, '_') || 'root'}.png`), fullPage: false });
    if (!seen.section) {
      problems.push(`${r.path}: no active section`);
      continue;
    }
    // The router heals an unknown or hidden view to the nearest one it can
    // show: then this route was never toured.
    const own = seen.url === r.path || seen.url.startsWith(`${r.path}/`) || REDIRECTS[r.path]?.to === seen.url;
    if (!r.expect && !own) problems.push(`${r.path}: redirected to ${seen.url}`);
    if (r.expect && !(await page.getByText(r.expect).filter({ visible: true }).count())) {
      problems.push(`${r.path}: does not show "${r.expect}"`);
    }
    if (seen.text < MIN_TEXT) problems.push(`${r.path}: ${seen.section} shows only ${seen.text} characters`);
    for (const text of seen.empties) {
      if (!ALLOW.some((a) => a.route === r.path && a.text.test(text))) {
        problems.push(`${r.path}: empty state: "${text}"`);
      }
    }
  }
  expect(problems, problems.join('\n')).toEqual([]);
});
