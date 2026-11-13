// The route tour: every top-level page and sub-page of the dashboard, loaded
// fresh against the sandbox's `full` fixture. It fails on a page error, a
// console error, a page that renders nothing, or an empty state the fixture
// should have filled. It guards the fixture against drift: a new page or tab is
// toured automatically (the list comes from the router), so it fails here until
// the fixture seeds it or ALLOW says why it stays empty.
//
// With a live federation peer (ui.sh tour --peer) it also opens the peer's
// pages, and the Fleet pages a linked peer fills must show it, not their empty
// states.
//
//   scripts/sandbox/ui.sh tour [--peer]
import { readFileSync } from 'node:fs';
import { test, expect } from '@playwright/test';

// The seeded entities the deep links open, written by the sandbox server
// (TestSandboxServe) for the full fixture; absent for the docshots fixtures.
const fixture = process.env.SANDBOX_FIXTURE_JSON
  ? JSON.parse(readFileSync(process.env.SANDBOX_FIXTURE_JSON, 'utf8'))
  : null;
// The live peer (ui.sh tour --peer), or null.
const peer = fixture?.peer ?? null;

// Tabs the tour skips, each with its reason.
const SKIP_TABS = {
  vegas: 'an easter egg behind a hotkey, not a page of the dashboard',
};

// Empty states that are expected in the sandbox, by route (the path the tour
// visits) and a pattern of the empty state's text. Each needs a reason; keep
// this list short, since every entry is a page the sandbox cannot show.
// An entry marked peer: false holds only without a live peer: linking one
// (--peer) fills that page, so there the empty state is a failure.
const NO_HUB = 'the sandbox has no federation hub and its one peer has no live node behind it';
const NOT_DONE = 'nothing in the sandbox has used this federation feature yet, live peer or not';
const ALLOW = [
  // Exactly this: a peer never seen has no date (tk_7dcf4f84).
  { route: '/map', text: /^Unreachable\.$/, why: NO_HUB, peer: false },
  { route: '/fleet-admin/peers', text: /No other instances visible on the hub/, why: NO_HUB, peer: false },
  { route: '/fleet-admin/spawns', text: /No open spawn requests from peers|Nothing sent yet/, why: NOT_DONE },
  { route: '/fleet-admin/invites', text: /Invites need a node profile/, why: NO_HUB, peer: false },
  { route: '/fleet-admin/models', text: /No model gateways yet|No leases|No model requests on/, why: NOT_DONE },
  { route: '/fleet-admin/sharing', text: /Federation is off/, why: NO_HUB, peer: false },
  { route: '/fleet-admin/sharing', text: /No peer shares usage with you|You share no model gateway/, why: NOT_DONE },
  { route: '/fleet-admin/profiles', text: /No profiles yet/, why: NO_HUB, peer: false },
  { route: '/fleet-admin/offers', text: /No offers (from peers|sent)/, why: NOT_DONE },
  { route: '/fleet-admin/jobs', text: /No remote jobs|No repositories allowed/, why: NOT_DONE },
  { route: '/fleet-admin/moves', text: /No agent moves or teleports/, why: NOT_DONE },
  { route: '/fleet-admin/audit', text: /No federation activity in this window/, why: NO_HUB, peer: false },
  { route: '/diagnostics/performance', text: /No captures yet/, why: 'a CPU capture needs a busy live daemon; the sandbox serves from a test binary' },
].filter((a) => !peer || a.peer !== false);

// Console messages that are not the page's fault, each with its reason.
const ALLOW_CONSOLE = [
  // { text: /pattern/, why: '...' },
];

// Failed requests (status 400 and up) that are expected in the sandbox, by a
// pattern of "<status> <path>", each with its reason.
const ALLOW_HTTP = [
  { text: /^409 \/api\/federation\/(away|boards)$/, why: NO_HUB, peer: false },
  { text: /^502 \/api\/peer\/inst_[a-z0-9]+\//, why: NO_HUB, peer: false },
  { text: /^403 \/api\/peer\/inst_[a-z0-9]+\//, why: 'the live peer trusts the sandbox at the restricted level; the dashboard asks for views it did not grant and shows the refusal', peer: true },
  { text: /^429 \/api\/companion\/presence$/, why: 'every route is a fresh page load, faster than the 32-tab presence table ages tabs out' },
].filter((a) => (a.peer === undefined) || a.peer === !!peer);

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
  return page.evaluate(async ({ skip, fx, peer }) => {
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
    // The live peer: the Fleet pages name it, and its own pages (?node=) show
    // what it shares, read through the hub.
    if (peer) {
      const node = `node=${encodeURIComponent(peer.id)}`;
      out.push(
        { path: '/map', expect: peer.label },
        { path: nav.toPath({ tab: 'fleet-admin', subtab: 'peers' }), expect: 'guest@laptop' },
        { path: nav.toPath({ tab: 'fleet-admin', subtab: 'harnesses' }), expect: peer.label },
        { path: `/groups?${node}`, expect: peer.group },
        { path: `/?${node}&task=${peer.task}:${peer.group}`, expect: peer.taskTitle },
      );
    }
    return out;
  }, { skip: Object.keys(SKIP_TABS), fx: fixture, peer });
}

// settle waits until the active section's text stops changing and no visible
// placeholder still says it is loading: the dashboard fills in over several
// polls, it never goes network-idle, and a slow read (a peer's summary through
// the hub) keeps its "Loading…" text unchanged until it answers.
async function settle(page) {
  let last = -1;
  let stable = 0;
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    const { len, loading } = await page.evaluate((selector) => {
      const section = document.querySelector('section[id^="tab-"].active');
      if (!section) return { len: -1, loading: false };
      const loading = [...section.querySelectorAll(selector)]
        .some((el) => el.offsetParent !== null && /^Loading\b/.test(el.innerText.trim()));
      return { len: section.innerText.length, loading };
    }, EMPTY_SELECTOR);
    stable = len === last && len > 0 && !loading ? stable + 1 : 0;
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
