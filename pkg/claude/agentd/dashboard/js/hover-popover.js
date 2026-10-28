import { useEffect, useRef } from 'preact/hooks';

// The one place the dashboard's hover popovers take their close timing from,
// and the registry that keeps at most one of them on screen.
//
// Every hover popover outlives the pointer leaving it by HOVER_CLOSE_MS, so a
// cursor that slips off the trigger or the panel for a moment does not drop
// it. CSS-only popovers mirror the value in --popover-close-grace
// (dashboard.css); keep the two in step.
export const HOVER_CLOSE_MS = 200;

// openPopovers holds every open popover registered via useExclusivePopover.
// Without it, sliding from one trigger to the next showed both panels
// overlapping until the first one's grace ran out. Only an opening dismisses
// others early; leaving a popover with nothing new opening keeps the grace.
const openPopovers = new Set();

// useExclusivePopover registers an open popover and, as it opens, closes every
// other one. An ancestor or descendant of the opener is left alone, so a
// popover nested inside another's panel (a PR card in the Open PRs list)
// never closes its host. A sticky popover — one the human pinned with a
// click or tap — is not closed by others opening; it still closes the rest
// when it opens itself.
export function useExclusivePopover(rootRef, open, close, sticky = false) {
  const latest = useRef({ close, sticky });
  latest.current = { close, sticky };
  useEffect(() => {
    if (!open) return undefined;
    const entry = {
      root: rootRef.current,
      close: () => { if (!latest.current.sticky) latest.current.close(); },
    };
    for (const other of openPopovers) {
      const nested = entry.root && other.root
        && (entry.root.contains(other.root) || other.root.contains(entry.root));
      if (!nested) other.close();
    }
    openPopovers.add(entry);
    return () => openPopovers.delete(entry);
  }, [open]);
}

// useLeaveGrace is the leave timer: schedule() runs onExpire after
// HOVER_CLOSE_MS unless cancel() comes first (the pointer came back). The
// timer is dropped on unmount.
export function useLeaveGrace(onExpire) {
  const timer = useRef(0);
  const expire = useRef(onExpire);
  expire.current = onExpire;
  const cancel = () => {
    if (timer.current) globalThis.clearTimeout(timer.current);
    timer.current = 0;
  };
  const schedule = () => {
    cancel();
    timer.current = globalThis.setTimeout(() => {
      timer.current = 0;
      expire.current();
    }, HOVER_CLOSE_MS);
  };
  useEffect(() => cancel, []);
  return { schedule, cancel };
}
