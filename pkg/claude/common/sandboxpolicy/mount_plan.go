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
//
// Path is the SANDBOX-side path — the position the entry occupies inside the
// namespace. That is what ordering, shadowing and most-specific-wins are
// decided on, and for every entry authored before TCL-866 it is also the host
// path. Source is the optional host path to project onto Path; an empty Source
// means "same path inside as outside", which keeps every pre-TCL-866 plan
// literal valid and byte-identical.
//
// Appliers must read the host side through SourcePath() rather than Path.
// Reading Path as a host path would, for a remapped entry, name a location that
// may not exist on the host at all.
type MountEntry struct {
	Path   string
	Mode   MountMode
	Source string
}

// SourcePath is the HOST path this entry exposes. It is the value an applier
// binds FROM, and the one whose existence decides whether a positive entry is
// applied or skipped.
func (entry MountEntry) SourcePath() string {
	if entry.Source != "" {
		return entry.Source
	}
	return entry.Path
}

// IsRemapped reports whether the entry projects a host path onto a different
// sandbox path. Only a real mount namespace can enforce that; a path-filter
// sandbox must refuse the entry rather than approximate it.
func (entry MountEntry) IsRemapped() bool {
	return entry.Source != "" && entry.Source != entry.Path
}

// PlanHasRemappedEntry reports whether any entry in a plan is remapped, so a
// capability gate can decide before an applier starts emitting arguments.
func PlanHasRemappedEntry(plan MountPlan) bool {
	for _, entry := range plan.Entries {
		if entry.IsRemapped() {
			return true
		}
	}
	return false
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
	// NetworkFiltered applies the compiled allow list. Linux uses its
	// supervised filtered gateway; Darwin currently accepts only loopback-only
	// lists and renders them as native Seatbelt remote-IP rules.
	NetworkFiltered
)

// MountPlan is the harness-neutral mount intermediate representation.
//
// Entries are ordered: later entries shadow earlier ones. Renderers preserve
// that order so a more-specific path can override an ancestor uniformly.
// Aliases are auxiliary namespace setup rather than authority-bearing mounts;
// their targets remain governed by Entries.
type MountPlan struct {
	Entries         []MountEntry
	Aliases         []MountAlias
	NetworkPosture  NetworkPosture
	FilteredNetwork *FilteredNetworkRuleSet
}
