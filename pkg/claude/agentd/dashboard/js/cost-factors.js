// Harness keys match cost.harness_factors; provider-specific overrides are not
// part of this setting. OpenCode intentionally shares one factor across providers.
export const COST_FACTOR_HARNESSES = [
  { key: 'claude', label: 'Claude Code' },
  { key: 'codex', label: 'Codex' },
  { key: 'opencode', label: 'OpenCode' },
  { key: 'copilot', label: 'Copilot' },
];
