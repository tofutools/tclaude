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
    const overlays = document.querySelectorAll(
      '.manage-overlay.show, .modal-overlay.show',
    );
    return overlays[overlays.length - 1] === overlayRef.current;
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
