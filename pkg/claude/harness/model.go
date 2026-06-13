package harness

// ModelCatalog validates and normalizes the model + reasoning-effort
// selections for a harness, and lists the valid values for spawn UIs
// (the dashboard spawn dialog, shell completions). Each harness has its
// own model namespace (`claude-*` vs `gpt-5.*`) and its own effort scale,
// so this is the single place a spawn surface asks "is this selection
// valid for this harness, and what's the canonical form to forward".
//
// By convention the empty string is always valid and normalizes to ""
// ("omit the flag, use the harness's own default"); callers handle "" by
// not passing the flag at all.
type ModelCatalog interface {
	// ValidateModel normalizes and validates a user-supplied model. It
	// returns the cleaned token to forward, or an error naming the
	// accepted set. "" in → ("", nil).
	ValidateModel(s string) (string, error)
	// ValidateEffort normalizes and validates a user-supplied effort
	// level. It returns the cleaned token to forward, or an error naming
	// the accepted set. "" in → ("", nil).
	ValidateEffort(s string) (string, error)
	// Models lists the known model aliases for spawn UIs. A harness may
	// also accept values outside this list (e.g. full model IDs) — the
	// authoritative check is ValidateModel.
	Models() []string
	// EffortLevels lists the valid effort levels for spawn UIs, in
	// ascending order.
	EffortLevels() []string
}
