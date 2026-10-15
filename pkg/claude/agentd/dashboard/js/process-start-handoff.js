// process-start-handoff.js -- "Start in <group>" from outside the Groups tab
// (the dock's Processes library, tcl-rzik1e): open that group's start form
// on a given template in Groups › Processes. The request waits here until
// the group's Processes body takes it: at once when it is mounted (the
// tclaude:process-start event), or when it mounts after the switch.

let pending = null;

// requestProcessStart queues the start, opens the group and switches the
// Groups tab to its Processes perspective.
export function requestProcessStart({ group, templateId }, {
  doc = document, prefs = null, setFocus = null,
} = {}) {
  if (!group || !templateId) return;
  pending = { group, templateId };
  prefs?.setItem?.(`tclaude.dash.group.${group}`, '1');
  doc.querySelector('nav [data-tab="groups"]')?.click();
  setFocus?.('processes');
  doc.dispatchEvent(new CustomEvent('tclaude:process-start'));
}

// takeProcessStart returns the queued start for this group, once.
export function takeProcessStart(group) {
  if (!pending || pending.group !== group) return null;
  const request = pending;
  pending = null;
  return request;
}
