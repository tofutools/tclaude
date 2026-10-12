package harness

// PresetSessionIDKind declares how `tclaude session new --session-id` may pin
// a launch's conversation id before the pane starts. It replaces a name switch
// in the launch path so that a newly registered harness declares its shape
// instead of falling into an "unsupported" default by omission.
type PresetSessionIDKind uint8

const (
	// PresetSessionIDUnsupported is the zero value: the harness mints its own
	// id (Codex, at its first turn) and an explicit --session-id is refused.
	PresetSessionIDUnsupported PresetSessionIDKind = iota
	// PresetSessionIDUUID means the harness accepts a caller-chosen UUID on its
	// command line (`claude --session-id`, `copilot --session-id`,
	// `gemini --session-id`). The value must be a UUID: Copilot only creates a
	// session for an unmatched value when it is one, so a name or a prefix
	// would leave the pane on another conversation than the one enrolled.
	PresetSessionIDUUID
	// PresetSessionIDManagedServer means the id is minted by agentd through
	// the harness's managed server API (OpenCode's ses_ ids). Only a managed
	// launch may pass one; a human cannot invent it.
	PresetSessionIDManagedServer
)

// PresetSessionIDKind reports the harness's preset-session-id shape. Nil-safe: an
// unknown harness supports none.
func (h *Harness) PresetSessionIDKind() PresetSessionIDKind {
	if h == nil {
		return PresetSessionIDUnsupported
	}
	return h.PresetSessionID
}

// MintsFreshLaunchSessionID reports whether `session new` should mint a UUID
// for a fresh launch that was not handed one. See Harness.MintFreshSessionID.
func (h *Harness) MintsFreshLaunchSessionID() bool {
	return h != nil && h.MintFreshSessionID && h.PresetSessionID == PresetSessionIDUUID
}

// ResumesFullIDWithoutStore reports whether a full conversation UUID plus the
// launch cwd is enough to resume without the harness's ConvStore resolving it.
// See Harness.ResumeFullIDWithoutStore.
func (h *Harness) ResumesFullIDWithoutStore() bool {
	return h != nil && h.ResumeFullIDWithoutStore
}
