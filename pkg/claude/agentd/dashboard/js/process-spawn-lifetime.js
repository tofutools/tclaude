// How long an agent a run spawns for a step stays: the fallback's lifetime
// (model.PerformerFallback.Lifetime). The defaults mirror the engine's
// model.DefaultLifetime and model.DefaultDeciderLifetime: change all four
// together.

export const SPAWN_LIFETIMES = ['step', 'run', 'keep'];

// DEFAULT_LIFETIME is a step's new agent's lifetime when none is set;
// DEFAULT_DECIDER_LIFETIME a gate decider's (a vote is one-shot).
export const DEFAULT_LIFETIME = 'run';
export const DEFAULT_DECIDER_LIFETIME = 'step';

export const LIFETIME_LABELS = {
  step: 'retire it once this step is done',
  run: 'keep it for later steps, retire when the run ends',
  keep: 'keep it (never retired by the run)',
};

// lifetimeChoices are a lifetime picker's options, the default marked.
export function lifetimeChoices({ decider = false } = {}) {
  const fallback = decider ? DEFAULT_DECIDER_LIFETIME : DEFAULT_LIFETIME;
  return SPAWN_LIFETIMES.map((value) => ({ value, label: value === fallback ? `${LIFETIME_LABELS[value]} (default)` : LIFETIME_LABELS[value] }));
}

// effectiveLifetime is the lifetime a spawn fallback gives its agent.
export function effectiveLifetime(fallback, { decider = false } = {}) {
  return fallback?.lifetime || (decider ? DEFAULT_DECIDER_LIFETIME : DEFAULT_LIFETIME);
}

// keepLifetime carries a spawn's chosen lifetime over to the spawn fallback
// replacing it (a new profile, say): only a spawn has one.
export function keepLifetime(before, after) {
  if (!after || after.action !== 'spawn' || after.lifetime || before?.action !== 'spawn' || !before.lifetime) return after;
  return { ...after, lifetime: before.lifetime };
}

// withLifetime sets a spawn fallback's lifetime; the default is left unset.
export function withLifetime(fallback, lifetime, { decider = false } = {}) {
  if (fallback?.action !== 'spawn') return fallback;
  const { lifetime: _old, ...rest } = fallback;
  const fallbackDefault = decider ? DEFAULT_DECIDER_LIFETIME : DEFAULT_LIFETIME;
  return lifetime && lifetime !== fallbackDefault ? { ...rest, lifetime } : rest;
}
