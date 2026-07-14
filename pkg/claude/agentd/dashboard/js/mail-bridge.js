// Dependency-free bridge from the eager dashboard shell to the dynamically
// loaded Messages island. Callers keep stable imports while a missing/broken
// Preact asset remains isolated to #messages-root.
let controller = null;
const pending = [];
let suppressAttentionOnce = false;

export function registerMailController(next) {
  if (!next || typeof next !== 'object') throw new TypeError('Messages controller is required');
  if (controller) throw new Error('Messages controller is already registered');
  controller = next;
  for (const invoke of pending.splice(0)) invoke(next);
  return () => {
    if (controller === next) controller = null;
  };
}

export function renderMailTab() { return controller?.renderMailTab?.(); }
export function renderAccessRequests(list, pendingCount) {
  return controller?.renderAccessRequests?.(list, pendingCount);
}
export function senderOnline(fromAgent, fromConv, snapshot) {
  return controller?.senderOnline?.(fromAgent, fromConv, snapshot) || false;
}
export function openMailbox(id) {
  if (controller) return controller.openMailbox?.(id);
  pending.push((next) => next.openMailbox?.(id));
}
export function focusAccessRequest(id) {
  if (controller) return controller.focusAccessRequest?.(id);
  pending.push((next) => next.focusAccessRequest?.(id));
}

// Explicit Messages deep links (an agent/group mailbox or a particular access
// request) click the same nav anchor as a human. They suppress the generic
// badge target for that one synchronous click so the requested destination is
// not raced by the attention shortcut.
export function suppressNextMessagesAttention() {
  suppressAttentionOnce = true;
  queueMicrotask(() => { suppressAttentionOnce = false; });
}

// Called by the eager tab binder after every ordinary Messages-tab click.
// Failed/lazy Messages islands simply make this a no-op.
export function focusNextMessagesAttention(snapshot) {
  if (suppressAttentionOnce) {
    suppressAttentionOnce = false;
    return undefined;
  }
  return controller?.focusNextAttention?.(snapshot);
}
