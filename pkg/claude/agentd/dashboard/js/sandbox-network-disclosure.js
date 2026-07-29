export const CODEX_BUILTIN_FILTERED_NETWORK_SHORT = 'no filtered network sandbox yet';

export const CODEX_BUILTIN_FILTERED_NETWORK_HINT =
  'Codex’s built-in filesystem sandbox remains available, but it has no filtered network sandbox yet. '
  + 'Its upstream proxy is experimental and off by default and does not meet sandbox profiles’ '
  + 'ordinary TCP/UDP contract. Use tclaude-layer filtering on Linux, or choose network open (Allow all).';

export function codexBuiltinSandboxOptionLabel(value, harnessName) {
  return value === 'harness-builtin' && harnessName === 'codex'
    ? `Codex built-in (${CODEX_BUILTIN_FILTERED_NETWORK_SHORT})`
    : '';
}
