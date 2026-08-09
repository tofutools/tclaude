// Client-side "pending wake" state: the set of agents this dashboard asked to
// resume that have not been seen online yet. Render code consults it, so the
// waking presentation is part of vdom truth rather than out-of-band DOM
// surgery — Preact's retained-tree prop diffing would otherwise skip repaints
// over surgically mutated nodes and strand a stale pulse (or erase a live
// one) on offline→offline re-renders.
//
// Keyed by the same handle the resume request carries (agent_id when the
// agent has one, else conv_id — exactly what the dot's data-agent holds).
// Entries clear when the row renders online, when the wake fails, or after a
// TTL so a launch that wedges before ever registering online cannot pulse
// forever.
const pending = new Map(); // handle -> expiry epoch ms

// Generous: covers managed-server boot + sandbox + pane fork + harness boot
// tail. A wedged launch stops pulsing when this lapses at a later render.
const PENDING_WAKE_TTL_MS = 90_000;

export function markPendingWake(handle) {
  if (handle) pending.set(handle, Date.now() + PENDING_WAKE_TTL_MS);
}

export function clearPendingWake(handle) {
  pending.delete(handle);
}

export function isPendingWake(handle) {
  const expiry = pending.get(handle);
  if (!expiry) return false;
  if (Date.now() > expiry) {
    pending.delete(handle);
    return false;
  }
  return true;
}
