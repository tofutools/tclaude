export const CODEX_BUILTIN_FILTERED_NETWORK_SHORT = 'no filtered network sandbox yet';

/* The long form of the caveat above. It is DISCLOSURE copy: the only places it
   may appear are behind a [!] or [?] trigger the operator opens deliberately.
   The spawn dialog and the sandbox-profile editor are dense enough already — a
   five-line paragraph parked under the implementation selector pushed every
   control below it off the first screen, for a caveat that applies to one
   harness. The trigger beside the selector carries it instead: visible,
   warning-coloured, and one click from the full text. */
export const CODEX_BUILTIN_FILTERED_NETWORK_DETAIL =
  'Codex’s built-in filesystem sandbox remains available, but it has no filtered network sandbox yet. '
  + 'Its upstream proxy is experimental and off by default and does not meet sandbox profiles’ '
  + 'ordinary TCP/UDP contract. Use tclaude-layer filtering on Linux, or choose network open (Allow all).';

// isCodexBuiltinSandboxOption marks the one implementation option that carries
// the disclosure above. It is deliberately NOT a label override: a selector
// option names which implementation is being chosen, and nothing else.
export function isCodexBuiltinSandboxOption(value, harnessName) {
  return value === 'harness-builtin' && harnessName === 'codex';
}
