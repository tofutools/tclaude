// isTopmostOverlay reports whether element is the front-most shown overlay, so
// a document-level Escape dismisses only the dialog on top. Stacked editors
// are not necessarily last in the DOM: CSS can lift an earlier overlay above
// the dialog that opened it. Follow the painted stack (computed z-index, with
// DOM order breaking ties) instead.
export function isTopmostOverlay(element) {
  const shown = Array.from(
    document.querySelectorAll('.modal-overlay.show, .manage-overlay.show'),
  );
  if (shown.length <= 1) return true;
  const zOf = (overlay) =>
    parseInt(
      globalThis.getComputedStyle?.(overlay)?.zIndex || overlay.style.zIndex,
      10,
    ) || 0;
  const elementZ = zOf(element);
  const elementIndex = shown.indexOf(element);
  return !shown.some((other, index) => {
    if (other === element) return false;
    const otherZ = zOf(other);
    if (otherZ !== elementZ) return otherZ > elementZ;
    return index > elementIndex;
  });
}

// This is a focus/effect boundary, not application state: callers use it only
// to avoid routing a global shortcut underneath a currently painted overlay.
export function hasShownOverlay() {
  return !!document.querySelector('.modal-overlay.show, .manage-overlay.show');
}
