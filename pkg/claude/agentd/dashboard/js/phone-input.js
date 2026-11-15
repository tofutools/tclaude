// phone-input.js -- tells phone.css whether the operator is using touch or a
// keyboard, as <html data-phone-input="touch|key">.
//
// A phone sheet hands focus back to what opened it when it closes (the More
// button, say). Browsers draw :focus-visible for such a programmatic focus,
// so on a touch screen a ring was left on the button after every sheet.
// Under data-phone-input="touch" phone.css draws no focus ring; the first key
// press (a keyboard paired with the phone, or a desktop preview) brings the
// rings back, since that is when they are needed.

const TOUCH = new Set(['touch', 'pen']);

// installInputModality starts tracking and returns what stops it.
export function installInputModality(doc = globalThis.document) {
  const root = doc?.documentElement;
  if (!root) return () => {};
  const onPointer = (event) => {
    const mode = TOUCH.has(event.pointerType) ? 'touch' : 'pointer';
    if (root.dataset.phoneInput !== mode) root.dataset.phoneInput = mode;
  };
  const onKey = (event) => {
    // A modifier alone is not keyboard navigation.
    if (['Shift', 'Control', 'Alt', 'Meta'].includes(event.key)) return;
    if (root.dataset.phoneInput !== 'key') root.dataset.phoneInput = 'key';
  };
  doc.addEventListener('pointerdown', onPointer, true);
  doc.addEventListener('keydown', onKey, true);
  return () => {
    doc.removeEventListener('pointerdown', onPointer, true);
    doc.removeEventListener('keydown', onKey, true);
    delete root.dataset.phoneInput;
  };
}
