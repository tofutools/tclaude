package sandboxpolicy

// MountMode describes how one path is exposed inside an OS-level sandbox.
type MountMode int

const (
	MountRO MountMode = iota
	MountRW
	MountHide
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

// MountPlan is the harness-neutral mount intermediate representation.
//
// Entries are ordered: later entries shadow earlier ones. Renderers preserve
// that order so a more-specific path can override an ancestor uniformly.
type MountPlan struct {
	Entries []MountEntry
}
