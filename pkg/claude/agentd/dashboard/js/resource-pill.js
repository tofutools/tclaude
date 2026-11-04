// resource-pill.js -- a group resource's state pill (tcl-k6vici): the same
// words on every row and kind, from resource-model.js's one state.

import { h } from 'preact';
import htm from 'htm';
import { resourceState } from './resource-model.js';
import { resourcePlaces } from './resource-places.js';

const html = htm.bind(h);

// The pill's words for each state (resource-model.js).
export const RESOURCE_PILL = Object.freeze({
  'open-tab': 'open in Collab', floating: 'floating', minimized: 'minimized', running: 'running', background: 'background', closed: 'closed',
});

// ResourcePill reads the same on every row and kind.
export function ResourcePill({ view }) {
  return html`<span class=${`ui-tag gr-pill gr-pill-${view}`} data-resource-state=${view}>${RESOURCE_PILL[view] || view}<//>`;
}

// resourceView is a resource's state here: the snapshot's group_resources
// say whether it runs, this browser's places what it has open. Without a
// snapshot entry, fallback is the server state the row's own data gives.
export function resourceView(snapshotValue, kind, id, fallback = 'closed') {
  const entry = (snapshotValue?.group_resources || []).find((r) => r.kind === kind && r.id === id);
  return resourceState(entry || { kind, id, state: fallback }, resourcePlaces.value);
}

