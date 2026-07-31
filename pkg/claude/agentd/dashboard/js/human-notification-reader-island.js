// The human-notification quick reader: a right-hand drawer over whatever tab
// the operator is on.
//
// It is its own island, on its own body-level host, for two reasons. It is
// raised from more than one surface — the amber "!" on a Groups member row and
// the same glyph on a Terminals tab — so it cannot belong to either; and a
// `position: fixed` drawer rendered inside a hidden tab <section> is not
// visible at all. Surfaces ask for it by document event rather than by import,
// so no surface has to know where it is mounted.
import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { GroupsNotificationReader } from './groups-notification-reader.js';
import { OPEN_HUMAN_NOTIFICATION_EVENT } from './human-notification-attention.js';
import { applyChromeTopInset } from './chrome-inset.js';

const html = htm.bind(h);

// Must match the .human-notification-drawer.closing animation in dashboard.css.
// The panel slides itself in on mount; sliding OUT needs it to outlive the
// close by exactly that long, which is the one thing CSS cannot arrange for a
// component that unmounts. Under prefers-reduced-motion there is no animation
// to wait for, and holding the panel there anyway would read as a hang.
export const CLOSE_ANIMATION_MS = 180;

function reducedMotion() {
  return Boolean(globalThis.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches);
}

export function HumanNotificationReader({
  state, actions, closeAnimationMs = CLOSE_ANIMATION_MS,
}) {
  const [descriptor, setDescriptor] = useState(null);
  const [closing, setClosing] = useState(false);
  const closeTimer = useRef(null);
  const openCount = useRef(0);

  useEffect(() => {
    const open = (event) => {
      const detail = event.detail || {};
      if (!detail.sender || !detail.messageId) return;
      // A notification raised while the panel is still sliding out cancels the
      // exit and takes the panel over, rather than opening behind it.
      if (closeTimer.current) {
        clearTimeout(closeTimer.current);
        closeTimer.current = null;
      }
      setClosing(false);
      // The panel is about to appear, so the content-area inset it pins its top
      // to must be current NOW — a stale value would paint one frame under the
      // header. dock.js keeps this fresh while the page moves, but it is a
      // different subsystem and may not have run.
      applyChromeTopInset();
      openCount.current += 1;
      setDescriptor({
        // openID makes every open distinct even when the same launcher raises
        // the same message again: a reopen that cancels an in-flight exit keeps
        // the component mounted, and the panel's own autofocus is keyed on this.
        openID: openCount.current,
        sender: detail.sender,
        messageId: detail.messageId,
        launcher: detail.launcher || null,
        returnFocus: detail.returnFocus || null,
      });
    };
    document.addEventListener(OPEN_HUMAN_NOTIFICATION_EVENT, open);
    return () => {
      document.removeEventListener(OPEN_HUMAN_NOTIFICATION_EVENT, open);
      if (closeTimer.current) clearTimeout(closeTimer.current);
    };
  }, []);

  if (!descriptor) return null;
  // Focus returns to whatever raised the drawer, but only when the drawer still
  // holds focus. The operator may well have clicked into a terminal while the
  // reader stayed open, and yanking focus back to a tab-strip button would
  // steal their keystrokes.
  const close = (restoreFocus) => {
    // Guard on the timer ref, not on the batched `closing` state: two closes
    // landing in one batch would both read closing === false, and the second
    // would orphan the first timer to fire after a later reopen.
    if (closeTimer.current) return;
    const drawer = document.querySelector('.human-notification-drawer');
    const held = Boolean(drawer && document.activeElement
      && drawer.contains(document.activeElement));
    const focusTarget = descriptor.launcher?.isConnected
      ? descriptor.launcher
      : descriptor.returnFocus;
    // Focus moves at once, not when the slide finishes: the operator has
    // already dismissed the panel and their next keystroke belongs elsewhere.
    const hold = reducedMotion() ? 0 : closeAnimationMs;
    setClosing(hold > 0);
    closeTimer.current = setTimeout(() => {
      closeTimer.current = null;
      setClosing(false);
      setDescriptor(null);
    }, hold);
    if (restoreFocus && held && focusTarget?.isConnected) {
      queueMicrotask(() => focusTarget.focus({ preventScroll: true }));
    }
  };
  return html`<${GroupsNotificationReader}
    descriptor=${descriptor}
    closing=${closing}
    snapshot=${state.snapshot.value}
    state=${state}
    actions=${actions}
    onSelect=${(messageId) => setDescriptor({ ...descriptor, messageId })}
    onClose=${close}
  />`;
}

export function mountHumanNotificationReaderIsland({ host, state, actions, registerCleanup }) {
  render(html`<${HumanNotificationReader} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
