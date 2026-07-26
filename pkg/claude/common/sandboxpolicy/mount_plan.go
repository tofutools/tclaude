package sandboxpolicy

// MountMode describes how one path is exposed inside an OS-level sandbox.
type MountMode int

const (
	// MountHide is the zero value so a partially initialized MountEntry fails
	// closed instead of silently granting read access.
	MountHide MountMode = iota
	MountRO
	MountRW
)

// MountEntry is one ordered entry in an OS-sandbox mount plan.
//
// The entry is intentionally a small intermediate representation rather than
// a bubblewrap-specific argument. Future non-filesystem policy classes belong
// in sibling MountPlan fields, leaving this ordered filesystem entry contract
// unchanged.
type MountEntry struct {
	Path string
	Mode MountMode
}

// MountAlias recreates one host symlink spelling inside a constructed
// filesystem root. Link is an absolute cleaned spelling and Target is its
// fully resolved absolute target prefix; appliers choose whether the active
// posture needs the alias materialized.
type MountAlias struct {
	Link   string `json:"link"`
	Target string `json:"target"`
}

// NetworkPosture describes how an OS-sandbox applier exposes the network
// namespace. It is deliberately separate from MountEntry: network isolation
// is not a filesystem entry, while Unix-socket allowlisting is expressed by
// ordinary read-only binds of the allowed socket paths.
type NetworkPosture int

const (
	// NetworkHostOpen preserves the caller's host network namespace. It is the
	// zero value so existing MountPlan literals retain the walking skeleton's
	// behavior.
	NetworkHostOpen NetworkPosture = iota
	// NetworkIsolatedWithAgentd creates a private network namespace and a
	// constructed filesystem root. Only explicitly bound pathname sockets,
	// including agentd, remain visible.
	NetworkIsolatedWithAgentd
	// NetworkFiltered is reserved for a future proxy-backed intermediate
	// posture. No current profile value renders it and appliers must refuse it.
	NetworkFiltered
)

// MountPlan is the harness-neutral mount intermediate representation.
//
// Entries are ordered: later entries shadow earlier ones. Renderers preserve
// that order so a more-specific path can override an ancestor uniformly.
// Aliases are auxiliary namespace setup rather than authority-bearing mounts;
// their targets remain governed by Entries.
type MountPlan struct {
	Entries        []MountEntry
	Aliases        []MountAlias
	NetworkPosture NetworkPosture
}
