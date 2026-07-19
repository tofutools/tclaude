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

// Rendered label metrics. The pin is an HTML overlay anchored at the SVG label's
// centre, so placing it beside the text needs the text's half-width -- and the
// SVG lives in a different component, so measuring it would mean reaching across
// at exactly the moment the label may not be rendered yet.
//
// Estimating instead keeps this pure, synchronous, and free of a first-frame
// jump. Being a few pixels out is invisible for a 22px button clearing a short
// outcome key; being one frame late is not.
const LABEL_FONT_SIZE = 11; // .process-edge-label text, dashboard.css
const LABEL_GLYPH_RATIO = 0.6; // average advance for the 650-weight face
const LABEL_HALO = 2.5; // half of the paint-order stroke that outlines the text
const PIN_GAP = 7;

// edgePinOffset returns the host-pixel shift from the label anchor to the pin.
//
// A label on a horizontal run hangs its pin BELOW, where the canvas is empty. On
// a vertical run below is where the next node sits, so the pin goes to the RIGHT
// of the text, clear of both the arrow and the node it points at.
export function edgePinOffset(outcome, orientation, zoom = 1) {
  if (orientation !== 'vertical') return { dx: 0, dy: 12 };
  const halfText = (String(outcome || '').length * LABEL_FONT_SIZE * LABEL_GLYPH_RATIO) / 2;
  return { dx: (halfText + LABEL_HALO + PIN_GAP) * (zoom || 1), dy: 0 };
}
