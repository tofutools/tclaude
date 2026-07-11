// A tiny single-review queue for independently running sandbox scribes. Drafts
// can finish in any order, but the dashboard has one profile editor: keep every
// result and reveal the next only after the current editor closes.
export function createSandboxDraftQueue({ canDeliver, deliver }) {
  const pending = [];
  let active = false;

  function flush() {
    if (active || !pending.length || !canDeliver()) return false;
    active = true;
    deliver(pending.shift());
    return true;
  }

  return {
    enqueue(item) {
      pending.push(item);
      return flush();
    },
    release() {
      active = false;
      return flush();
    },
    poke: flush,
    pendingCount() { return pending.length; },
  };
}
