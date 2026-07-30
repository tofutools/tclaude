export const CODEX_BUILTIN_FILTERED_NETWORK_SHORT = 'no filtered network sandbox yet';

export const CODEX_BUILTIN_FILTERED_NETWORK_HINT =
  'Codex’s built-in filesystem sandbox remains available, but it has no filtered network sandbox yet. '
  + 'Its upstream proxy is experimental and off by default and does not meet sandbox profiles’ '
  + 'ordinary TCP/UDP contract. Use tclaude-layer filtering on Linux, or choose network open (Allow all).';

// isCodexBuiltinSandboxOption marks the one implementation option that carries
// the disclosure above. It is deliberately NOT a label override: a selector
// option names which implementation is being chosen, and nothing else. The
// caveat rides on the option's description and on the hint under the row, where
// there is room to state it properly, rather than being squeezed into a
// parenthetical the closed <select> then truncates.
export function isCodexBuiltinSandboxOption(value, harnessName) {
  return value === 'harness-builtin' && harnessName === 'codex';
}
