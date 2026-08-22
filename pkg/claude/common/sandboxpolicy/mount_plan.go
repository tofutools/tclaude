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

// RootPosture describes how an applier that owns a mount namespace builds the
// sandbox filesystem root. It is a SIBLING of NetworkPosture rather than more
// values on it, because the two answer different questions and TCL-798 stopped
// them from being the same answer.
//
// Until TCL-798 the constructed root existed only beneath an isolated network
// namespace, so one posture value could imply both. That coupling left the
// profile combination unix_sockets=closed|list + network=open with no
// enforcement path on Linux: the only mechanism that hides ambient filesystem
// sockets was reachable only by also taking the host network away. Separating
// the fields lets a plan ask for a constructed root while keeping host IP
// networking.
//
// Appliers without a mount namespace ignore this field. macOS Seatbelt is a
// path filter over the host namespace: it has no root to construct and
// expresses the same intent with native socket denies, which is why Darwin
// never had this gap.
type RootPosture int

const (
	// RootHostInherited binds the host root read-only. It is the zero value so
	// existing MountPlan literals keep the walking skeleton's behavior, and it
	// leaves every ambient filesystem socket reachable.
	RootHostInherited RootPosture = iota
	// RootConstructed starts from a fresh root, so filesystem AF_UNIX sockets
	// are absent unless the launch contract or the policy plan binds them back.
	RootConstructed
)

// MountPlan is the harness-neutral mount intermediate representation.
//
// Entries are ordered: later entries shadow earlier ones. Renderers preserve
// that order so a more-specific path can override an ancestor uniformly.
// Aliases are auxiliary namespace setup rather than authority-bearing mounts;
// their targets remain governed by Entries.
type MountPlan struct {
	Entries                 []MountEntry
	Aliases                 []MountAlias
	NetworkPosture          NetworkPosture
	RootPosture             RootPosture
	DarwinAllowMachRegister bool
	// NetworkEngine names the filtering engine this plan actually deploys, as
	// decided by DeployedNetworkEngine. It is unset whenever the policy needs
	// no engine at all, so a reader never has to re-derive that question — and
	// so the plan cannot claim a mechanism the launch does not run.
	NetworkEngine   NetworkEngine
	FilteredNetwork *FilteredNetworkRuleSet
	// PreserveCallerIdentity selects the opt-in Linux packet-sandbox user
	// namespace shape. It is inert for every other plan floor.
	PreserveCallerIdentity bool
}

// EffectiveRootPosture is the root an applier must actually build, and is what
// appliers and plan output read instead of the raw field.
//
// RootHostInherited is the zero value, which keeps pre-TCL-798 plan literals
// valid. But an isolated or filtered network posture has ALWAYS implied a
// constructed root, so a literal that names one of those postures and leaves
// the new field unset must not be read as a request for the host root: that
// would silently unbuild the root of the very postures whose whole point is
// building one. Restating the implication here means the coupling is broken in
// one direction only — a constructed root no longer requires network isolation,
// while network isolation still requires a constructed root.
func (p MountPlan) EffectiveRootPosture() RootPosture {
	if p.RootPosture == RootConstructed {
		return RootConstructed
	}
	return RootPostureFor(p.NetworkPosture, AccessModeUnset)
}
