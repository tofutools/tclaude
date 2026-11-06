// side-panel-tuning-fields.js -- the Config tab's fields for the side panel's
// edge reveal and predictive peek tuning (config dashboard.side_panel_*,
// tcl-wdwbm2). Each value's default and range match the daemon's
// (config.SidePanelTuningLimits, pinned by a Go test); blank stores nothing
// and runs the default.

export const SIDE_PANEL_TUNING_FIELDS = Object.freeze([
  { key: 'edge_zone_px', label: 'Edge zone width', unit: 'px', def: 32, min: 4, max: 200 },
  { key: 'dwell_ms', label: 'Edge zone dwell', unit: 'ms', def: 150, min: 0, max: 2000 },
  { key: 'retract_ms', label: 'Peek retract delay', unit: 'ms', def: 500, min: 50, max: 5000 },
  { key: 'predict_min_speed', label: 'Fling minimum speed', unit: 'px/s', def: 1200, min: 100, max: 10000 },
  { key: 'predict_speed_distance_px', label: 'Full speed needed from', unit: 'px', def: 600, min: 1, max: 4000 },
  { key: 'predict_min_speed_floor', label: 'Speed floor up close', unit: 'px/s', def: 600, min: 100, max: 10000 },
  { key: 'predict_min_readings', label: 'Readings needed', unit: 'moves', def: 4, min: 2, max: 20 },
  { key: 'min_travel_px', label: 'Fling minimum travel', unit: 'px', def: 80, min: 0, max: 1000 },
  { key: 'brake_percent', label: 'Braking threshold', unit: '%', def: 70, min: 0, max: 100 },
  { key: 'peek_min_px', label: 'Peek start width', unit: 'px', def: 48, min: 8, max: 400 },
].map(Object.freeze));

// SIDE_PANEL_TOGGLES are the switches, all default on, so only false is stored.
export const SIDE_PANEL_TOGGLES = Object.freeze([
  { key: 'side_panel_edge_reveal', id: 'cfg-dashboard-side-panel-edge-reveal' },
  { key: 'side_panel_predictive_peek', id: 'cfg-dashboard-side-panel-predictive-peek' },
  { key: 'side_panel_growing_peek', id: 'cfg-dashboard-side-panel-growing-peek' },
]);

export const sidePanelTuningId = (key) => `cfg-side-panel-${key.replace(/_/g, '-')}`;
