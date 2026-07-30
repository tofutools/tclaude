/* Canonical copy for the two resolution concepts the dashboard exposes side by
   side, and which used to be described by a different phrase on every surface —
   a differently-worded "resolved …" per selector, and a bare `inherit` standing
   in for an explanation (TCL-865).

   They are genuinely different things, and the wording has to keep them apart:

   - RESOLVED DEFAULTS is LAUNCH-PARAMETER resolution — which harness, sandbox
     mode, and sandbox implementation a real launch would end up with. It walks
     one ordered chain and the first tier that has a value wins.
   - SANDBOX-PROFILE COMPOSITION is POLICY composition — the global, group, and
     (when chosen) explicit sandbox profiles all apply, composed together rather
     than one winning.

   Every surface that means one of those imports its sentence from here, so the
   launch dialog and the sandbox-profile editor cannot drift into describing the
   same mechanism with two vocabularies. */

// RESOLVED_DEFAULTS_LABEL is the canonical short phrase for the automatic
// launch-resolution choice. Selector options read exactly this, optionally with
// a parenthetical narrowing ("this host"), never a synonym.
export const RESOLVED_DEFAULTS_LABEL = 'Resolved defaults';

// Kept on one line despite its length: the tier order IS the sentence, and a
// wrapped concatenation lets a later edit reorder or drop a tier in a way no
// literal-string assertion could see.
export const RESOLVED_DEFAULTS_CHAIN = 'Resolved defaults evaluate what a real launch would resolve, taking the first tier that sets a value: explicit launch choice → named spawn profile → group default spawn profile → global default spawn profile → harness default.';

// The sandbox-profile editor's preview resolves a launch it has no explicit or
// named-profile tier for: there is no spawn to make a choice, and no --profile.
// Stating the general chain there would promise two tiers that control cannot
// reach, so the preview gets its own sentence rather than the general one.
export const RESOLVED_DEFAULTS_CHAIN_PREVIEW = 'This preview has no explicit launch choice and no named spawn profile, so its resolved defaults run: group default spawn profile → global default spawn profile → harness default.';

// One line for the same reason as the chain above: the enumerated layers are
// the claim, and they have to be assertable as one string.
export const SANDBOX_PROFILE_COMPOSITION = 'Sandbox-profile policy is composed, not resolved from a single winner: the global sandbox profile, the group sandbox profile, and an explicit sandbox profile when one is chosen all apply together.';

// GLOBAL_SANDBOX_PROFILE_ROLE / GROUP_SANDBOX_PROFILE_ROLE describe one layer's
// place in that composition, for the toolbar chip and the group chip that assign
// them. They exist so a tooltip cannot quietly coin a fourth way to say it.
export const GLOBAL_SANDBOX_PROFILE_ROLE = "the first composed layer of every launch's sandbox policy, applied together with the group sandbox profile and any explicit one";
export const GROUP_SANDBOX_PROFILE_ROLE = "the group layer of a launch's composed sandbox policy, applied together with the global sandbox profile and any explicit one";

// GLOBAL_DEFAULT_PROFILE_ROLE / GROUP_DEFAULT_PROFILE_ROLE do the same for the
// two spawn-profile tiers of the resolved-defaults chain.
export const GLOBAL_DEFAULT_PROFILE_ROLE = 'the last resolved-defaults tier before the harness default, used when the chosen group has no default spawn profile of its own';
export const GROUP_DEFAULT_PROFILE_ROLE = 'the resolved-defaults tier above the global default spawn profile, filling launch fields a spawn left blank';

// CLAUDE_INHERIT_SANDBOX_LABEL replaces a bare `inherit` wherever Claude's
// sandbox mode is named in a control. The token stays in parentheses as
// secondary technical detail so an operator reading docs or a CLI flag can still
// map the two.
export const CLAUDE_INHERIT_SANDBOX_LABEL = 'Claude settings decide (inherit)';

export const CLAUDE_INHERIT_SANDBOX_PLAIN = "Claude's own settings decide whether its built-in "
  + 'sandbox is enabled for this launch.';

// sandboxModeLabel renders one harness sandbox mode for a human. Only Claude's
// `inherit` is rewritten: it is the one mode whose token says nothing about what
// the launch actually gets. Every other mode already names its own effect.
export function sandboxModeLabel(harnessName, mode) {
  return harnessName === 'claude' && mode === 'inherit' ? CLAUDE_INHERIT_SANDBOX_LABEL : mode;
}

// sandboxModeOptionLabel is the selectable form: the label plus the harness's
// own recommendation. It folds the two into ONE parenthetical for a rewritten
// mode, because a caller appending "(recommended)" to a label that already ends
// in "(inherit)" produces "…(inherit) (recommended)". Both dialogs call this
// rather than composing the two halves themselves.
export function sandboxModeOptionLabel(harnessName, mode, recommended) {
  const label = sandboxModeLabel(harnessName, mode);
  if (mode !== recommended) return label;
  return label.endsWith(')')
    ? `${label.slice(0, -1)}, recommended)`
    : `${label} (recommended)`;
}

// sandboxModeDetail is the read-only form used where a resolved mode is
// reported back rather than chosen — the preview's evaluation details.
export function sandboxModeDetail(harnessName, mode) {
  return harnessName === 'claude' && mode === 'inherit'
    ? `inherit — ${CLAUDE_INHERIT_SANDBOX_PLAIN}`
    : mode;
}

// SANDBOX_PROFILE_LAYERS_LABEL prefixes the layer list everywhere it appears —
// the spawn dialog's preview line and the editor's always-visible row — so the
// two read as the same statement about the same composition.
export const SANDBOX_PROFILE_LAYERS_LABEL = 'Composed sandbox-profile layers';

// sandboxProfileLayersText names the composed sandbox-profile layers of one
// effective context. It is deliberately not collapsed into "3 layers": which
// profile sits in which scope is the thing an operator cannot otherwise see.
// emptyText is the caller's word for "nothing composes here", which differs by
// surface: an unsaved editor draft stands alone, a launch applies nothing.
export function sandboxProfileLayersText(context = {}, emptyText = 'none') {
  const layers = ['global', 'group', 'explicit']
    .filter((scope) => context[scope])
    .map((scope) => `${scope} “${context[scope]}”`);
  return layers.length ? layers.join(' + ') : emptyText;
}

// sandboxProfileLayersInline is the same list for a flat single-line summary,
// where the surrounding text already uses " · " between sections. Two or more
// layers joined by " + " would run straight into the next section with nothing
// marking where the list ended, so the inline form brackets itself.
export function sandboxProfileLayersInline(context = {}, emptyText = 'none') {
  return `${SANDBOX_PROFILE_LAYERS_LABEL} (${sandboxProfileLayersText(context, emptyText)})`;
}
