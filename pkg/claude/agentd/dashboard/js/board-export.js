// board-export.js -- renders a board's SVG to a PNG for its agents
// (tcl-4f86kp). Agents see a board two ways: the symbolic element JSON for
// precise edits, and this raster snapshot for a quick look, which the editor
// uploads (PUT /api/boards/{id}/snapshot) after changes settle.
// dashboard-imperative-boundary: browser-io

const MAX_SIDE = 1600;

// svgToPng rasterises an <svg> element's current drawing, cropped to `box`
// (board units) with a white background. Resolves to a PNG Blob, or null when
// the browser cannot render it.
export async function svgToPng(svg, box, { documentRef = globalThis.document, windowRef = globalThis.window } = {}) {
  if (!svg || !box || !documentRef?.createElement) return null;
  const pad = 24;
  const vb = { x: box.x - pad, y: box.y - pad, w: Math.max(box.width, 1) + pad * 2, h: Math.max(box.height, 1) + pad * 2 };
  const scale = Math.min(2, MAX_SIDE / Math.max(vb.w, vb.h));
  const width = Math.max(1, Math.round(vb.w * scale));
  const height = Math.max(1, Math.round(vb.h * scale));
  const clone = svg.cloneNode(true);
  clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
  clone.setAttribute('viewBox', `${vb.x} ${vb.y} ${vb.w} ${vb.h}`);
  clone.setAttribute('width', String(width));
  clone.setAttribute('height', String(height));
  // The live editor's pan/zoom group and selection chrome are not drawing.
  clone.querySelector('[data-board-viewport]')?.removeAttribute('transform');
  for (const node of clone.querySelectorAll('[data-board-chrome]')) node.remove();
  const markup = new windowRef.XMLSerializer().serializeToString(clone);
  const url = windowRef.URL.createObjectURL(new windowRef.Blob([markup], { type: 'image/svg+xml' }));
  try {
    const image = new windowRef.Image();
    await new Promise((resolve, reject) => {
      image.onload = resolve;
      image.onerror = reject;
      image.src = url;
    });
    const canvas = documentRef.createElement('canvas');
    canvas.width = width;
    canvas.height = height;
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = '#ffffff';
    ctx.fillRect(0, 0, width, height);
    ctx.drawImage(image, 0, 0, width, height);
    return await new Promise((resolve) => canvas.toBlob((blob) => resolve(blob), 'image/png'));
  } catch {
    return null;
  } finally {
    windowRef.URL.revokeObjectURL(url);
  }
}

// downloadText offers a text file to the operator (the .excalidraw export).
export function downloadText(name, text, { documentRef = globalThis.document, windowRef = globalThis.window } = {}) {
  const url = windowRef.URL.createObjectURL(new windowRef.Blob([text], { type: 'application/json' }));
  const a = documentRef.createElement('a');
  a.href = url;
  a.download = name;
  documentRef.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => windowRef.URL.revokeObjectURL(url), 1000);
}
