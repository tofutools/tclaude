import { useEffect, useLayoutEffect, useRef } from 'preact/hooks';
import { CLICK_CLOSE_MS, HOVER_CLOSE_MS, holdsText, watchPopover } from './popover-watch.js';

export { CLICK_CLOSE_MS, HOVER_CLOSE_MS, holdsText, watchPopover };

// The hooks of the dashboard's popovers: the registry that keeps at most one
// hover popover on screen, the leave grace, and useDismissablePopover. The
// timing and the click-opened close rule live in popover-watch.js (no Preact)
// and are re-exported here.

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

// useDismissablePopover runs watchPopover while open is set. A changed open
// value is a different popover, so a keyed menu moves the watch along with it.
// roots is a ref, a list of refs, or a function returning elements.
export function useDismissablePopover(roots, open, close, options = {}) {
  const latest = useRef(null);
  latest.current = { roots, close, options };
  // At commit, not after paint: a press or Escape can follow the click at once.
  useLayoutEffect(() => {
    if (!open) return undefined;
    const resolve = () => {
      const given = latest.current.roots;
      if (typeof given === 'function') return given();
      return [given].flat().map((ref) => ref?.current);
    };
    return watchPopover({
      ...options,
      roots: resolve,
      close: (why) => latest.current.close(why),
      hold: () => !!latest.current.options.hold?.(),
      hoverOpened: () => !!latest.current.options.hoverOpened?.(),
      fading: (on) => latest.current.options.fading?.(on),
    });
  }, [open]);
}
