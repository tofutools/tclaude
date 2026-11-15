// process-start-handoff.js -- a group's process work asked for from outside
// the Groups tab: "Start in <group>" (the dock's Processes library,
// tcl-rzik1e) opens that group's start form on a given template; `create`
// (the palette's New process) makes a new draft there and opens it inline;
// `edit` (the full editor's ← Back inline) opens a template in the group's
// inline editor. The request waits here until
// the group's Processes body takes it: at once when it is mounted (the
// tclaude:process-start event), or when it mounts after the switch.

import { revealGroupPath } from './groups-fused-sections.js';

let pending = null;

// A start nobody took within this window (the group is filtered out of the
// tree) is dropped, so it cannot pop open much later.
const PENDING_TTL_MS = 10_000;

// requestProcessStart queues the start, opens the group and brings the
// Groups tab's runs on screen (showRuns leaves the Route map). A
// nested group's ancestors open too (`groups`: the snapshot's), or its body
// never mounts to take the start.
export function requestProcessStart({ group, templateId, create = false, edit = false }, {
  doc = document, prefs = null, groups = [], showRuns = null, rerender = null, now = Date.now,
} = {}) {
  if (!group) return;
  // No template opens the start form on its picker (the palette's "Start a
  // process in <group>").
  pending = { group, templateId: templateId || '', create: !!create, edit: !!edit && !!templateId, at: now() };
  prefs?.setItem?.(`tclaude.dash.group.${group}`, '1');
  if (prefs) revealGroupPath(group, groups, prefs);
  doc.querySelector('nav [data-tab="groups"]')?.click();
  showRuns?.();
  // Re-render so a collapsed group reads its open pref and expands.
  rerender?.();
  doc.dispatchEvent(new CustomEvent('tclaude:process-start'));
}

// takeProcessStart returns the queued start for this group, once.
export function takeProcessStart(group, { now = Date.now } = {}) {
  if (pending && now() - pending.at > PENDING_TTL_MS) pending = null;
  if (!pending || pending.group !== group) return null;
  const { group: g, templateId, create, edit } = pending;
  pending = null;
  return { group: g, templateId, create, edit };
}

// openGroupProcess is requestProcessStart wired to the Groups tab, for a
// caller that does not hold its state (the overlay editor's ← Back inline,
// the palette's New process). Loaded on demand, so the caller's module does
// not pull the Groups tab in.
export async function openGroupProcess(request) {
  const [{ dashPrefs }, { groupsState }, { showGroupRuns }] = await Promise.all([
    import('./prefs.js'), import('./groups-state.js'), import('./groups-focus.js'),
  ]);
  requestProcessStart(request, {
    prefs: dashPrefs, groups: groupsState.snapshot.value?.groups || [],
    showRuns: () => showGroupRuns(groupsState), rerender: groupsState.rerender,
  });
}
