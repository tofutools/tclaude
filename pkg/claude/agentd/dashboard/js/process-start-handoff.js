// process-start-handoff.js -- "Start in <group>" from outside the Groups tab
// (the dock's Processes library, tcl-rzik1e): open that group's start form
// on a given template in Groups › Processes. The request waits here until
// the group's Processes body takes it: at once when it is mounted (the
// tclaude:process-start event), or when it mounts after the switch.

let pending = null;

// A start nobody took within this window (the group is filtered out of the
// tree) is dropped, so it cannot pop open much later.
const PENDING_TTL_MS = 10_000;

// requestProcessStart queues the start, opens the group and switches the
// Groups tab to its Processes perspective.
export function requestProcessStart({ group, templateId }, {
  doc = document, prefs = null, setFocus = null, rerender = null, now = Date.now,
} = {}) {
  if (!group || !templateId) return;
  pending = { group, templateId, at: now() };
  prefs?.setItem?.(`tclaude.dash.group.${group}`, '1');
  doc.querySelector('nav [data-tab="groups"]')?.click();
  setFocus?.('processes');
  // setFocus is a no-op when Processes is already showing; re-render so a
  // collapsed group reads its open pref and expands.
  rerender?.();
  doc.dispatchEvent(new CustomEvent('tclaude:process-start'));
}

// takeProcessStart returns the queued start for this group, once.
export function takeProcessStart(group, { now = Date.now } = {}) {
  if (pending && now() - pending.at > PENDING_TTL_MS) pending = null;
  if (!pending || pending.group !== group) return null;
  const { group: g, templateId } = pending;
  pending = null;
  return { group: g, templateId };
}
