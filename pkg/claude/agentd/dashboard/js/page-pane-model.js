// page-pane-model.js -- the pure half of a live page (tcl-r4sg0c): a page an
// agent showed with `tclaude agent show`, open as a Collab strip tab, an overlay
// tab or a separate window. Its seed names the page id and nothing else: the
// page's URL carries its origin's token, which lives only in the dashboard
// snapshot and only ever reaches an iframe's src. DOM-free, so the terminal
// core and the standalone page can read a seed without the frame's module
// graph, and the tests can read it directly.

export const PAGE_WINDOW_KIND = 'page';

// A page id, as the daemon mints it (pages.go newPageID).
const PAGE_ID = /^pg_[0-9a-f]{16}$/;

// A page origin, as the daemon builds it (pages.go pageOrigin): its own
// *.ui.localhost site, never the dashboard's.
const PAGE_ORIGIN = /^http:\/\/f-[0-9a-f]{32}\.ui\.localhost(?::\d{1,5})?$/;

export const isPageId = (id) => typeof id === 'string' && PAGE_ID.test(id);

export const pagePaneKey = (id) => `page:${id}`;
export const pageWindowId = (id) => `page:${id}`;

export function pagePaneSeed(id, label = '') {
  return { page: id, key: pagePaneKey(id), label: label || id };
}

// The frame's sandbox. Never allow-same-origin (the page would get its origin's
// storage and could lift the sandbox from a same-site parent), allow-popups,
// allow-top-navigation* or allow-downloads.
export const PAGE_FRAME_SANDBOX = 'allow-scripts allow-forms allow-modals';

// livePagesOf lists the snapshot's pages.
export function livePagesOf(snapshotValue) {
  const pages = snapshotValue?.live_pages;
  return Array.isArray(pages) ? pages.filter((page) => isPageId(page?.id)) : [];
}

export function findLivePage(snapshotValue, id) {
  return livePagesOf(snapshotValue).find((page) => page.id === id) || null;
}

// pageFrameSrc is the URL the frame may load: the page's own, on its own stage
// origin. Anything else (no dashboard listener, a malformed answer) loads
// nothing.
export function pageFrameSrc(page) {
  const origin = String(page?.origin || '');
  const url = String(page?.url || '');
  if (!PAGE_ORIGIN.test(origin)) return '';
  if (url !== origin && !url.startsWith(`${origin}/`)) return '';
  return url;
}

// pageLabel is how a page is named in a tab: the agent's title, or its name.
export const pageLabel = (page) => String(page?.title || page?.name || page?.id || 'page');

// pageSender names the agent that showed the page from the snapshot's groups,
// or falls back to its id.
export function pageSender(snapshotValue, page) {
  const owner = String(page?.owner || '');
  for (const group of snapshotValue?.groups || []) {
    for (const member of group?.members || []) {
      if (member?.agent_id === owner) {
        return { agent: owner, name: String(member.title || member.name || owner), online: member.online === true, harness: String(member.harness || '') };
      }
    }
  }
  return { agent: owner, name: owner ? owner.slice(0, 12) : 'an agent', online: false, harness: '' };
}

// pageOriginShown is the page's origin with its token masked: the token is the
// page's only key, and goes nowhere but the frame's src.
export const pageOriginShown = (origin) => String(origin || '').replace(/^(http:\/\/f-)[0-9a-f]{32}(\.ui\.localhost)/, '$1‹token›$2');

// pageSandboxRules is what the 🔒 chip says on hover: where the page runs and
// what it can and cannot do. Plain text, rendered as text.
export function pageSandboxRules(page) {
  const lines = [
    `Sandboxed frame on its own origin: ${pageOriginShown(page?.origin) || '(none)'}`,
    'No dashboard cookies, storage or API reach it; it can fetch only its own origin.',
    'It cannot open pop-ups or windows, navigate the dashboard, or download files.',
    'The agent that showed this page can read what you type in it.',
  ];
  if (page?.kind === 'static') lines.push('Files are served read-only; a saved file reloads the page.');
  else lines.push('Served from the agent’s dev server; its hot reload passes through.');
  return lines;
}

// pageShowEvents compares the snapshot's pages with the ones known before.
// known is a Map of id → rev, or null before the first snapshot. It answers the
// next map, the pages shown since (added), the ones shown again (reshown: a
// higher rev), and the ids that went away. The first snapshot shows nothing: what was open before
// the dashboard loaded is not news, and opens quietly (initial).
export function pageShowEvents(known, pages) {
  const next = new Map(pages.map((page) => [page.id, Number(page.rev) || 0]));
  if (!known) return { next, added: [], reshown: [], gone: [], initial: pages };
  const added = pages.filter((page) => !known.has(page.id));
  const reshown = pages.filter((page) => known.has(page.id) && (Number(page.rev) || 0) > known.get(page.id));
  const gone = [...known.keys()].filter((id) => !next.has(id));
  return { next, added, reshown, gone, initial: [] };
}
