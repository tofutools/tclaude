// A .paf or .zip dropped onto the page opens the ⤒ import… preview
// (tcl-jkmitn). Only file drags are taken: agent and group drag-and-drop are
// left alone, a drop zone that already handled the drop (an attachment
// field) wins, and an open dialog is never replaced by a stray drop.
export function bindPAFDrop({ open, refuse = () => false, doc = document }) {
  const isFileDrag = (event) => [...(event.dataTransfer?.types || [])].includes('Files');
  const onDragOver = (event) => {
    if (isFileDrag(event)) event.preventDefault();
  };
  const onDrop = (event) => {
    if (!isFileDrag(event) || event.defaultPrevented) return;
    if (doc.querySelector('[role="dialog"][aria-modal="true"]')) return;
    const file = [...(event.dataTransfer?.files || [])].find((f) => /\.(paf|zip)$/i.test(f.name));
    if (!file) return;
    event.preventDefault();
    if (refuse()) return;
    open(file);
  };
  doc.addEventListener('dragover', onDragOver);
  doc.addEventListener('drop', onDrop);
  return () => {
    doc.removeEventListener('dragover', onDragOver);
    doc.removeEventListener('drop', onDrop);
  };
}
