// process-edge-hint.js -- the tooltip text for a connector's pin toggle.
//
// Why the explanation exists at all: an outcome label looks like decoration but
// is the key of the node's `next` map, so renaming it re-points the run. The
// pin controls whether that key stays on screen when the connector is not
// selected, and the tooltip is where the consequence is spelled out.
//
// It is a tooltip on the button rather than a bubble on the canvas because the
// explanation is about the control, and a note floating beside every selected
// connector was more intrusive than the label it was explaining.

// edgePinTitle describes what clicking will do, and -- when pinning is what
// keeps a routing key visible -- why that matters. Pure so the wording is
// testable without a DOM.
export function edgePinTitle(outcome, pinned) {
  const action = pinned
    ? `Unpin "${outcome}": hide this label unless the connector is selected.`
    : `Pin "${outcome}": keep this label visible when the connector is not selected.`;
  return `${action} The label is this connector's outcome key -- the run takes the connector whose key matches the result -- so hiding it changes nothing, but renaming it changes where the run goes.`;
}

// Placement of the pin relative to the label it controls.

const PIN_GAP = 7;
// Fallback half-extents, used only when the label has not been measured (it is
// rendered before the pin publishes, so this is a defensive path, not the
// normal one). Mirrors .process-edge-label text in dashboard.css.
const LABEL_FONT_SIZE = 11;
const LABEL_GLYPH_RATIO = 0.6;

// edgePinPlacement returns the host-pixel CENTRE for the pin button.
//
// A label on a mostly-horizontal edge hangs its pin BELOW, where the canvas is
// empty. On a mostly-vertical edge below is where the target node sits, so the
// pin goes to the RIGHT of the text, clear of both the arrow and the node.
//
// `box` is the label's measured host-space rectangle. It is what makes the
// clearance correct: the layout's label anchor is a point on the edge geometry,
// and the text grows around it differently per edge -- `text-anchor: end` on
// back edges, `middle` elsewhere -- and scales with the viewport. Offsetting
// from the anchor instead collides with the label on exactly the edges whose
// text does not happen to be centred on it.
export function edgePinPlacement({
  anchor, box = null, outcome = '', orientation = 'horizontal', zoom = 1, half = 11,
} = {}) {
  const vertical = orientation === 'vertical';
  if (box) {
    return vertical
      ? { left: box.left + box.width + PIN_GAP + half, top: box.top + box.height / 2 }
      : { left: box.left + box.width / 2, top: box.top + box.height + PIN_GAP + half };
  }
  // No measurement: approximate the text extent from the key and the zoom.
  const halfWidth = (String(outcome).length * LABEL_FONT_SIZE * LABEL_GLYPH_RATIO * (zoom || 1)) / 2;
  const halfHeight = (LABEL_FONT_SIZE * (zoom || 1)) / 2;
  return vertical
    ? { left: anchor.left + halfWidth + PIN_GAP + half, top: anchor.top }
    : { left: anchor.left, top: anchor.top + halfHeight + PIN_GAP + half };
}
