// resource-places.js -- where this browser shows each group resource, live:
// the overlay tabs (overlay-instances.js) and the Collab strip's pane keys,
// which the terminal shell publishes into stripPaneKeys. resource-model.js
// turns the two, with the snapshot's group_resources, into one state per
// resource. Kept apart from resource-model.js so the model stays pure.

import { computed, signal } from '@preact/signals';
import { openOverlayWindow, overlayTabs } from './overlay-instances.js';
import { browserPlaces, overlayResourceKey } from './resource-model.js';

// The Collab strip's pane keys, in strip order (terminal-shell-island.js).
export const stripPaneKeys = signal([]);

// resourcePlaces maps <kind>:<id> to open-tab, floating or minimized.
export const resourcePlaces = computed(() => browserPlaces({ overlays: overlayTabs.value, strip: stripPaneKeys.value }));

// revealResourceOverlay brings a resource's overlay tab forward (restoring it
// from the dock), and reports whether it had one.
export function revealResourceOverlay(key) {
  const tab = overlayTabs.peek().find((candidate) => overlayResourceKey(candidate) === key);
  if (!tab) return false;
  openOverlayWindow(tab);
  return true;
}
