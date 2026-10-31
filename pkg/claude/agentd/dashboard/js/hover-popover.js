import { useEffect, useLayoutEffect, useRef } from 'preact/hooks';

// The one place the dashboard's hover popovers take their close timing from,
// the registry that keeps at most one of them on screen, and the close rule of
// the popovers a click opens (watchPopover).
//
// Every hover popover outlives the pointer leaving it by HOVER_CLOSE_MS, so a
// cursor that slips off the trigger or the panel for a moment does not drop
// it. CSS-only popovers mirror the value in --popover-close-grace
// (dashboard.css); keep the two in step.
export const HOVER_CLOSE_MS = 200;

// A popover opened by a click is one the operator asked for, so it waits longer
// for a slipped pointer to come back (watchPopover).
export const CLICK_CLOSE_MS = 600;

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

// isTextField is a control that takes typing. A checkbox or a range slider
// holds nothing that hover-away could lose.
function isTextField(el) {
  if (el?.isContentEditable) return true;
  const tag = el?.tagName;
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return tag === 'INPUT' && !/^(button|checkbox|color|file|hidden|image|radio|range|reset|submit)$/.test(el.type || '');
}

// holdsText says a popover has text in it that closing would throw away.
export function holdsText(root) {
  const fields = root?.querySelectorAll?.('input, textarea') || [];
  return [...fields].some((field) => isTextField(field) && field.value);
}

// Open watched popovers, oldest first: Escape takes the newest only, so it
// closes one layer of a popover opened from inside another.
const watched = [];

// watchPopover is the one close rule for popovers opened by a click: a press
// outside closes at once, Escape closes, and the pointer staying away for
// CLICK_CLOSE_MS closes. Text in the popover, a field the operator is using, or
// hold() keeps it through hover-away, and the check repeats while the pointer
// is away, so it closes once the reason ends. A press or Escape still closes.
// A popover that hover can open too passes hoverOpened(), and takes the hover
// delay for as long as that says so.
//
// A cog menu focuses its filter as it opens, which is why focus alone does not
// count: the field counts once the operator pressed it or used the keyboard.
//
// roots() gives the elements that are the popover, usually one holding both the
// trigger and the panel; a pointer moving between separate ones is not away.
// close(why) gets 'outside', 'escape' or 'away'. A shown dialog above the
// popover owns the page: its presses and Escape are its own, and the popover
// waits behind it. A popover inside that dialog carries on as usual. escape:
// false is for a menu whose Escape does something first, like clearing its
// filter. The returned function stops watching.
export function watchPopover({ roots, close, hold = null, hoverOpened = null, escape = true, doc = null }) {
  const parts = () => [roots()].flat().filter(Boolean);
  const within = (target) => parts().some((root) => root.contains?.(target));
  const owner = doc || parts()[0]?.ownerDocument || globalThis.document;
  const entry = {};
  let timer = 0;
  let away = false;
  let pressed = false;
  let engaged = false;
  const cancel = () => {
    if (timer) globalThis.clearTimeout(timer);
    timer = 0;
  };
  const dialogAbove = () => [...(owner?.querySelectorAll?.('.modal-overlay.show, .manage-overlay.show') || [])]
    .some((overlay) => !parts().some((root) => overlay.contains(root)));
  const held = () => {
    if (pressed || hold?.() || dialogAbove()) return true;
    const active = owner?.activeElement;
    return (engaged && isTextField(active) && within(active)) || parts().some(holdsText);
  };
  const schedule = () => {
    cancel();
    timer = globalThis.setTimeout(() => {
      timer = 0;
      if (!away) return;
      if (held()) schedule();
      else close('away');
    }, hoverOpened?.() ? HOVER_CLOSE_MS : CLICK_CLOSE_MS);
  };
  const onLeave = () => { away = true; schedule(); };
  const onEnter = () => { away = false; cancel(); };
  const onPointerDown = (event) => {
    if (dialogAbove()) return;
    if (within(event.target)) {
      pressed = true;
      if (isTextField(event.target)) engaged = true;
    } else {
      close('outside');
    }
  };
  // A drag that starts inside can end anywhere, a slider's or a selection's.
  const onPointerUp = () => { pressed = false; };
  const onKeyDown = (event) => {
    if (within(event.target)) engaged = true;
    if (!escape || event.key !== 'Escape' || watched.at(-1) !== entry || dialogAbove()) return;
    event.preventDefault();
    event.stopPropagation();
    close('escape');
  };
  const listening = parts();
  for (const root of listening) {
    root.addEventListener?.('mouseleave', onLeave);
    root.addEventListener?.('mouseenter', onEnter);
  }
  owner?.addEventListener?.('pointerdown', onPointerDown, true);
  owner?.addEventListener?.('pointerup', onPointerUp, true);
  owner?.addEventListener?.('keydown', onKeyDown, true);
  watched.push(entry);
  return () => {
    cancel();
    watched.splice(watched.indexOf(entry), 1);
    for (const root of listening) {
      root.removeEventListener?.('mouseleave', onLeave);
      root.removeEventListener?.('mouseenter', onEnter);
    }
    owner?.removeEventListener?.('pointerdown', onPointerDown, true);
    owner?.removeEventListener?.('pointerup', onPointerUp, true);
    owner?.removeEventListener?.('keydown', onKeyDown, true);
  };
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
    });
  }, [open]);
}
