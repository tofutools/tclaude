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
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { GroupsNotificationReader } from './groups-notification-reader.js';
import { OPEN_HUMAN_NOTIFICATION_EVENT } from './human-notification-attention.js';

const html = htm.bind(h);

export function HumanNotificationReader({ state, actions }) {
  const [descriptor, setDescriptor] = useState(null);

  useEffect(() => {
    const open = (event) => {
      const detail = event.detail || {};
      if (!detail.sender || !detail.messageId) return;
      setDescriptor({
        sender: detail.sender,
        messageId: detail.messageId,
        launcher: detail.launcher || null,
        returnFocus: detail.returnFocus || null,
      });
    };
    document.addEventListener(OPEN_HUMAN_NOTIFICATION_EVENT, open);
    return () => document.removeEventListener(OPEN_HUMAN_NOTIFICATION_EVENT, open);
  }, []);

  if (!descriptor) return null;
  // Focus returns to whatever raised the drawer, but only when the drawer still
  // holds focus. The operator may well have clicked into a terminal while the
  // reader stayed open, and yanking focus back to a tab-strip button would
  // steal their keystrokes.
  const close = (restoreFocus) => {
    const drawer = document.querySelector('.human-notification-drawer');
    const held = Boolean(drawer && document.activeElement
      && drawer.contains(document.activeElement));
    const focusTarget = descriptor.launcher?.isConnected
      ? descriptor.launcher
      : descriptor.returnFocus;
    setDescriptor(null);
    if (restoreFocus && held && focusTarget?.isConnected) {
      queueMicrotask(() => focusTarget.focus({ preventScroll: true }));
    }
  };
  return html`<${GroupsNotificationReader}
    descriptor=${descriptor}
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
