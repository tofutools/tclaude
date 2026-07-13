import { h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { makeModalResizable } from './helpers.js';
import { useDialogFocus } from './dialog-focus.js';

const html = htm.bind(h);

export function ManagementOverlay({
  id,
  manage = false,
  dialogClass = '',
  labelledby,
  onClose,
  onSubmitHotkey = null,
  onSubmitEnter = null,
  dirty = false,
  blocked = false,
  confirmDiscard,
  resizeKey = '',
  fitContent = true,
  children,
}) {
  const overlayRef = useRef(null);
  const close = async () => {
    if (blocked) return;
    if (!dirty || (await confirmDiscard())) onClose();
  };
  const isTopmost = () => {
    // Stacked editors are not necessarily last in the DOM: the profile editor
    // host precedes the spawn dialog, then CSS lifts the editor above it.
    // Follow the painted stack (z-index, with DOM order breaking ties) so the
    // visually front-most dialog owns Escape.
    const overlays = Array.from(
      document.querySelectorAll('.manage-overlay.show, .modal-overlay.show'),
    );
    if (overlays.length <= 1) return true;
    const current = overlayRef.current;
    const zOf = (element) =>
      parseInt(
        globalThis.getComputedStyle?.(element)?.zIndex || element.style.zIndex,
        10,
      ) || 0;
    const currentZ = zOf(current);
    const currentIndex = overlays.indexOf(current);
    return !overlays.some((other, index) => {
      if (other === current) return false;
      const otherZ = zOf(other);
      if (otherZ !== currentZ) return otherZ > currentZ;
      return index > currentIndex;
    });
  };
  const { dialogRef } = useDialogFocus({
    open: true,
    onEscape: () => {
      void close();
    },
    shouldHandle: isTopmost,
  });
  useEffect(() => {
    if (!resizeKey) return undefined;
    return fitContent
      ? makeModalResizable(dialogRef.current, resizeKey)
      : makeModalResizable(dialogRef.current, resizeKey, { fitContent: false });
  }, [resizeKey, fitContent]);
  const wideEditor = [
    'template-editor-modal',
    'profile-editor-modal',
    'role-editor-modal',
  ].includes(id);
  const modalClass =
    dialogClass ||
    (wideEditor
      ? 'cron-create-modal template-editor-modal'
      : 'cron-create-modal');
  return html`<div
    ref=${overlayRef}
    class=${manage ? 'manage-overlay show' : 'modal-overlay show'}
    id=${id}
    onMouseDown=${(event) => {
      if (event.target === event.currentTarget) void close();
    }}
  >
    <div
      ref=${dialogRef}
      class=${manage ? 'manage-modal' : modalClass}
      role="dialog"
      aria-modal="true"
      aria-labelledby=${labelledby}
      onKeyDown=${(event) => {
        if (
          onSubmitEnter &&
          event.key === 'Enter' &&
          !event.ctrlKey &&
          !event.metaKey
        ) {
          event.preventDefault();
          onSubmitEnter();
          return;
        }
        if (
          onSubmitHotkey &&
          event.key === 'Enter' &&
          (event.ctrlKey || event.metaKey)
        ) {
          event.preventDefault();
          onSubmitHotkey();
        }
      }}
    >
      ${children}
    </div>
  </div>`;
}
