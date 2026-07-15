import { h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { makeModalResizable } from './helpers.js';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';

const html = htm.bind(h);

export function ManagementOverlay({
  id,
  manage = false,
  dialogClass = '',
  overlayClass = '',
  labelledby,
  onClose,
  onSubmitHotkey = null,
  onSubmitEnter = null,
  dirty = false,
  blocked = false,
  confirmDiscard,
  resizeKey = '',
  fitContent = true,
  guardBackdropDrag = false,
  onDragEnter = null,
  onDragOver = null,
  onDragLeave = null,
  onDrop = null,
  onPaste = null,
  initialFocusRef = null,
  children,
}) {
  const overlayRef = useRef(null);
  const pressedOnBackdrop = useRef(false);
  const close = async () => {
    if (blocked) return;
    if (!dirty || (await confirmDiscard())) onClose();
  };
  const { dialogRef } = useDialogFocus({
    open: true,
    initialFocusRef,
    onEscape: () => {
      void close();
    },
    shouldHandle: () => isTopmostOverlay(overlayRef.current),
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
    class=${`${manage ? 'manage-overlay show' : 'modal-overlay show'}${overlayClass ? ` ${overlayClass}` : ''}`}
    id=${id}
    onMouseDown=${(event) => {
      if (guardBackdropDrag) {
        pressedOnBackdrop.current = event.target === event.currentTarget;
      } else if (event.target === event.currentTarget) {
        void close();
      }
    }}
    onClick=${guardBackdropDrag ? (event) => {
      const dismiss = event.target === event.currentTarget && pressedOnBackdrop.current;
      pressedOnBackdrop.current = false;
      if (dismiss) void close();
    } : undefined}
    onDragEnter=${onDragEnter}
    onDragOver=${onDragOver}
    onDragLeave=${onDragLeave}
    onDrop=${onDrop}
    onPaste=${onPaste}
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
          !event.isComposing &&
          event.keyCode !== 229 &&
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
          !event.isComposing &&
          event.keyCode !== 229 &&
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
