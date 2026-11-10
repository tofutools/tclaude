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

// A remote viewer's page path (pages.go remotePageURL).
const REMOTE_PAGE_PATH = /^\/p\/[0-9a-f]{32}(?:[/?#]|$)/;

// remotePagePathOK checks a remote page path as the browser will resolve it:
// still under /p/<token>/ once dot segments (spelled any way) are resolved.
function remotePagePathOK(url) {
  if (!REMOTE_PAGE_PATH.test(url)) return false;
  try {
    const base = 'http://page.invalid';
    const u = new URL(url, base);
    return u.origin === base && REMOTE_PAGE_PATH.test(u.pathname) && url.startsWith(u.pathname.slice(0, 35));
  } catch {
    return false;
  }
}

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
//
// A dashboard on another machine (a LAN address, a VPN name, remote access)
// gets no origin and a path instead: /p/<remote token>/..., served by this
// dashboard's own listener (remote_stage.go), so the browser supplies its own
// scheme and host.
export function pageFrameSrc(page) {
  const origin = String(page?.origin || '');
  const url = String(page?.url || '');
  if (!origin) return remotePagePathOK(url) ? url : '';
  if (!PAGE_ORIGIN.test(origin)) return '';
  if (url !== origin && !url.startsWith(`${origin}/`)) return '';
  return url;
}

// pageLabel is how a page is named in a tab: the agent's title, or its name.
export const pageLabel = (page) => String(page?.title || page?.name || page?.id || 'page');

// pageSender names the agent that showed the page from the snapshot's groups,
// or falls back to its id.
export function pageSender(snapshotValue, page) {
  // A page the operator created in a group that no member has taken over
  // (tcl-20ihu3): the group's, not an agent's.
  if (page?.group_owned) return { agent: '', group: String(page.group || ''), name: page.group ? `group ${page.group}` : 'its group', online: false, harness: '' };
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

// pageWhereShown says where the page runs, its key masked: its own origin,
// or, for a remote viewer, its path on this dashboard's host.
export function pageWhereShown(page) {
  if (page?.origin) return `its own origin: ${pageOriginShown(page.origin)}`;
  if (REMOTE_PAGE_PATH.test(String(page?.url || ''))) return 'this host, under /p/‹token›/ (its address is its key: share it with no one)';
  return '(none)';
}

// pageSandboxRules is what the 🔒 chip says on hover: where the page runs and
// what it can and cannot do. Plain text, rendered as text.
export function pageSandboxRules(page) {
  const lines = [
    `Sandboxed frame on ${pageWhereShown(page)}`,
    'No dashboard cookies, storage or API reach it; it can fetch only its own origin.',
    'It cannot open pop-ups or windows, navigate the dashboard, or download files.',
    page?.group
      ? `Shared in group ${page.group}: the agent that showed it and every member of that group can read what you type in it.`
      : 'The agent that showed this page can read what you type in it.',
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
