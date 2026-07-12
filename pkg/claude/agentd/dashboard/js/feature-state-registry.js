// Small core bridge between the authoritative poll and dynamically loaded
// feature islands. Keeping this registry dependency-free means a broken island
// asset cannot prevent the legacy dashboard graph from linking and booting.
const states = new Map();

export function registerFeatureState(name, state) {
  if (typeof name !== 'string' || !name || !state) {
    throw new TypeError('feature state registration requires a name and state');
  }
  if (states.has(name)) {
    throw new Error(`feature state already registered: ${name}`);
  }
  states.set(name, state);
  return () => {
    if (states.get(name) === state) states.delete(name);
  };
}

export function featureState(name) {
  return states.get(name) || null;
}
