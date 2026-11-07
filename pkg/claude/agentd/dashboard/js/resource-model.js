// resource-model.js -- one state for every group resource (tcl-1lth8e): a
// page, drawing board, process or shell (a group's or an agent's) reads the same everywhere it is listed
// (the Groups tab, the Collab rail). The snapshot's group_resources says what
// the server knows: running (a page's stage is live, a run is active, a
// group's shell is up) or closed (it exists, nothing is live). This browser
// knows what it has open, and that wins:
//
//   open here as a Collab tab     -> 'open-tab'
//   in an overlay window          -> 'floating'
//   minimized to the dock         -> 'minimized'
//   a running process             -> 'running'
//   a running page or shell       -> 'background'
//   otherwise                     -> 'closed'
//
// Window state stays in this browser: another browser's minimized window is
// not this one's.

export const RESOURCE_STATES = ['open-tab', 'floating', 'minimized', 'running', 'background', 'closed'];

// resourceKey is a resource's key here: <kind>:<id>, the shape the Collab
// strip's pane keys take for pages and processes.
export const resourceKey = (resource) => `${resource.kind}:${resource.id}`;

// overlayResourceKey is the resource an overlay tab shows, '' when none.
export function overlayResourceKey(tab) {
  if (!tab) return '';
  if (tab.kind === 'page' && tab.page) return `page:${tab.page}`;
  if (tab.kind === 'drawing_board' && tab.drawingBoard) return `drawing_board:${tab.drawingBoard}`;
  // A process editor's overlay tab names its template (process-editor-place.js).
  if (tab.kind === 'process' && (tab.template || tab.process)) return `process:${tab.template || tab.process}`;
  const pane = String(tab.paneKey || tab.key || '');
  if (tab.kind === 'terminal') return shellPaneResourceKey(pane);
  return '';
}

// shellPaneResourceKey is the shell resource a terminal pane key shows: a
// group's shell (groupterm:<group>, resource id <group>) or an agent's shell
// (term:<agent>:<which>:<group>, resource id the pane key itself); '' for
// any other pane.
export function shellPaneResourceKey(paneKey) {
  const k = String(paneKey || '');
  if (k.startsWith('groupterm:')) return `shell:${k.slice('groupterm:'.length)}`;
  if (k.startsWith('term:')) return `shell:${k}`;
  return '';
}

// shellPaneKey is the terminal pane key a shell resource opens under.
export const shellPaneKey = (resource) => (String(resource.id).startsWith('term:') ? resource.id : `groupterm:${resource.id}`);

// stripResourceKey is the resource a Collab strip pane key shows, '' when
// none.
export function stripResourceKey(key) {
  const k = String(key || '');
  return shellPaneResourceKey(k) || (/^(page|drawing_board|process):./.test(k) ? k : '');
}

// browserPlaces maps resource keys to where this browser shows them, from its
// overlay tabs and its Collab strip's pane keys. An overlay wins over a strip
// tab: it is the one in front.
export function browserPlaces({ overlays = [], strip = [] } = {}) {
  const places = new Map();
  for (const key of strip) {
    const k = stripResourceKey(key);
    if (k) places.set(k, 'open-tab');
  }
  for (const tab of overlays) {
    const k = overlayResourceKey(tab);
    if (k) places.set(k, tab.minimized ? 'minimized' : 'floating');
  }
  return places;
}

// resourceState is one resource's state, given browserPlaces.
export function resourceState(resource, places = new Map()) {
  const here = places.get(resourceKey(resource));
  if (here) return here;
  if (resource.state !== 'running') return 'closed';
  return resource.kind === 'process' ? 'running' : 'background';
}

// groupResourcesOf lists one group's resources from the snapshot, each with
// its state here as `view`.
export function groupResourcesOf(snapshot, groupId, places = new Map()) {
  const all = Array.isArray(snapshot?.group_resources) ? snapshot.group_resources : [];
  return all.filter((r) => r && r.group_id === groupId).map((r) => ({ ...r, view: resourceState(r, places) }));
}
