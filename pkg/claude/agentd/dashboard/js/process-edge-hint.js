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
