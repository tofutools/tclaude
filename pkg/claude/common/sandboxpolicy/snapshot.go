package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"time"
)

// SnapshotVersion 9 adds Linux resource limits. Version 8 added the network and Unix-socket access axes plus their
// persisted access notices. Version 7 removes break_glass_filesystem, the one sanctioned
// exception to the protected-root wall (TCL-791). Version 6 added
// ProfilesOmitted, a lifecycle-significant launch contract that prevents
// ambient sandbox-profile tiers from reappearing on resume/reincarnation; that
// bump preserved the fail-closed downgrade property, where an older binary
// rejects a newer snapshot rather than ignoring a marker it does not
// understand. Version 5 removed the retired read-baseline mechanism (TCL-623).
const SnapshotVersion = 9

// AppliedProfile preserves stable registry provenance without making the
// registry row authoritative after resolution. The effective values in the
// snapshot are the launch authority; IDs and timestamps exist for audit only.
type AppliedProfile struct {
	Scope     Scope     `json:"scope"`
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RequireContained rejects authority that the parent snapshot did not already
// possess. Filesystem coverage is segment-aware AND specificity-aware: a child
// read/write grant is contained only when the parent's own most-specific rule
// at that path already conferred at least that access, and every parent deny
// must be preserved by an equal or broader child deny. Environment entries must
// match the parent's exact value; omission is a safe weakening. Agent-owned
// directories are not inherited host authority: agentd creates fresh private
// bindings for each child, so their declarations do not participate in
// containment.
//
// Specificity is what gives plain deny rows real lineage authority (TCL-623).
// A parent that denies ~ and reopens only its workspace must not be able to
// mint a child that reopens ~/.ssh beneath the same deny: the child grant would
// otherwise look "covered" by any broad parent read grant sitting above the
// deny, and resume/reincarnate/child-spawn would silently widen.
func RequireContained(parent, child Snapshot) error {
	parent, err := RevalidateSnapshot(parent)
	if err != nil {
		return fmt.Errorf("parent sandbox snapshot: %w", err)
	}
	child, err = RevalidateSnapshot(child)
	if err != nil {
		return fmt.Errorf("child sandbox snapshot: %w", err)
	}
	for _, childGrant := range child.Effective.Filesystem {
		if childGrant.Access == AccessDeny {
			continue
		}
		// Containment is checked in BOTH spaces once mount paths exist
		// (TCL-866), because a remapped grant has two sides and each can leak
		// independently. The namespace side asks whether the child may occupy
		// that sandbox path at all; the host side asks whether the parent ever
		// had authority over the directory whose contents the child is
		// exposing. Without the second check a child could mount an arbitrary
		// host directory at a sandbox path the parent happened to grant. For a
		// rule with no mount path the two paths are equal and the pair collapses
		// to exactly the pre-TCL-866 check.
		guestAccess, guestCovered := EffectiveAccessAt(
			parent.Effective.Filesystem, childGrant.GuestPath())
		if guestCovered && guestAccess == AccessDeny {
			return fmt.Errorf(
				"filesystem %s grant %q reopens a path the parent snapshot denies; a child may not carve out authority beneath a deny the parent did not itself reopen",
				childGrant.Access, childGrant.GuestPath())
		}
		if !guestCovered || (childGrant.Access == AccessWrite && guestAccess != AccessWrite) {
			return fmt.Errorf("filesystem %s grant %q is not contained by the parent snapshot", childGrant.Access, childGrant.GuestPath())
		}
		hostAccess, hostCovered := EffectiveHostAccessAt(
			parent.Effective.Filesystem, childGrant.Path)
		if hostCovered && hostAccess == AccessDeny {
			return fmt.Errorf(
				"filesystem %s grant for host path %q reopens a path the parent snapshot denies; a child may not carve out authority beneath a deny the parent did not itself reopen",
				childGrant.Access, childGrant.Path)
		}
		if !hostCovered || (childGrant.Access == AccessWrite && hostAccess != AccessWrite) {
			return fmt.Errorf(
				"filesystem %s grant for host path %q is not contained by the parent snapshot",
				childGrant.Access, childGrant.Path)
		}
	}
	for _, parentGrant := range parent.Effective.Filesystem {
		if parentGrant.Access != AccessDeny {
			continue
		}
		preserved := false
		for _, childGrant := range child.Effective.Filesystem {
			if childGrant.Access == AccessDeny && pathContainsOrEqual(childGrant.Path, parentGrant.Path) {
				preserved = true
				break
			}
		}
		if !preserved {
			return fmt.Errorf("filesystem deny %q from the parent snapshot is not preserved", parentGrant.Path)
		}
	}
	parentEnv := make(map[string]string, len(parent.Effective.Environment))
	for _, entry := range parent.Effective.Environment {
		parentEnv[entry.Name] = entry.Value
	}
	for _, entry := range child.Effective.Environment {
		if value, ok := parentEnv[entry.Name]; !ok || value != entry.Value {
			return fmt.Errorf("environment variable %q is new or changed from the parent snapshot", entry.Name)
		}
	}
	if err := requireResourceLimitsContained(parent.Effective.ResourceLimits, child.Effective.ResourceLimits); err != nil {
		return err
	}
	if child.Effective.DarwinAllowMachRegister && !parent.Effective.DarwinAllowMachRegister {
		return fmt.Errorf("darwin mach-register access is not present in the parent snapshot")
	}
	if parent.Effective.Network == nil && child.Effective.Network == nil &&
		!networkAccessContained(parent.Effective.NetworkAccess, child.Effective.NetworkAccess) {
		return fmt.Errorf("network access %q is not contained by parent access %q", child.Effective.NetworkAccess, parent.Effective.NetworkAccess)
	}
	parentAxes, err := deriveEffectiveAccessAxes(parent.Effective)
	if err != nil {
		return fmt.Errorf("parent access axes: %w", err)
	}
	childAxes, err := deriveEffectiveAccessAxes(child.Effective)
	if err != nil {
		return fmt.Errorf("child access axes: %w", err)
	}
	if !networkRulesContained(parentAxes.Network, childAxes.Network) {
		return fmt.Errorf("network rules are not contained by the parent snapshot")
	}
	if !unixSocketRulesContained(parentAxes.UnixSockets, childAxes.UnixSockets) {
		return fmt.Errorf("unix-socket rules are not contained by the parent snapshot")
	}
	return nil
}

func networkAccessContained(parent, child NetworkAccess) bool {
	// Inherit is unknown authority rather than guaranteed Internet access: the
	// harness or user config may impose a restrictive managed proxy. A child
	// may preserve that unknown posture or narrow it to none, but may not turn
	// it into explicit Internet access (which disables an inherited proxy).
	if child == NetworkAccessNone {
		return true
	}
	return parent == child
}

func deriveEffectiveAccessAxes(effective EffectiveProfile) (ResolvedAxes, error) {
	return DeriveAccessAxes(Profile{
		Name:          "effective-sandbox-access",
		NetworkAccess: effective.NetworkAccess,
		Network:       effective.Network,
		UnixSockets:   effective.UnixSockets,
		// The resolved filesystem authority rides along so capability planning
		// can answer the one question that spans the filesystem and the network
		// engine at once. This is the EFFECTIVE grant set — what the launch will
		// actually bind — which is why the resolver-socket check reaches the
		// same verdict here as at the launch boundary.
		Filesystem: effective.Filesystem,
	})
}

// EffectiveAccessAxes derives the concrete access intent from a resolved
// profile while preserving the legacy network_access compatibility table.
func EffectiveAccessAxes(effective EffectiveProfile) (ResolvedAxes, error) {
	return deriveEffectiveAccessAxes(effective)
}

// PlannedEffectiveAccessAxes applies persisted widening notices to authored
// intent at the final renderer seam. A renderer never guesses capability:
// without an explicit degradation record, a list remains a list and the
// reserved/unimplemented IR continues to refuse.
func PlannedEffectiveAccessAxes(effective EffectiveProfile) (ResolvedAxes, error) {
	axes, err := deriveEffectiveAccessAxes(effective)
	if err != nil {
		return ResolvedAxes{}, err
	}
	networkDenyOmitted := []int{}
	for _, notice := range effective.AccessNotices {
		if notice.Class != AccessNoticeClassDegradation {
			continue
		}
		switch notice.Axis {
		case "network":
			switch notice.Reason {
			case "no_mechanism", "selector_unsupported", "platform_path_blind":
				if axes.Network.Mode != AccessModeList {
					continue
				}
				// The engine rides through the widening. Widening changes
				// WHICH destinations survive, never the mechanism the surviving
				// deny rows are enforced with, and a launch that re-derived a
				// different engine here than the preview named would be exactly
				// the disclosure-does-not-match-rendered-surface bug.
				axes.Network = NetworkRules{
					Mode:   AccessModeOpen,
					Deny:   cloneNetworkRules(axes.Network).Deny,
					Engine: axes.Network.Engine,
				}
			case "ports_unsupported":
				if axes.Network.Mode != AccessModeList {
					continue
				}
				if len(notice.Entries) == 0 {
					for i := range axes.Network.Allow {
						axes.Network.Allow[i].Ports = nil
					}
				} else {
					for _, i := range notice.Entries {
						if i >= 0 && i < len(axes.Network.Allow) {
							axes.Network.Allow[i].Ports = nil
						}
					}
				}
			case "deny_selector_unsupported", "deny_ports_unsupported":
				networkDenyOmitted = append(
					networkDenyOmitted, notice.Entries...)
			}
		case "unix_sockets":
			if axes.UnixSockets.Mode != AccessModeList {
				continue
			}
			switch notice.Reason {
			case "no_mechanism", "platform_path_blind":
				axes.UnixSockets = UnixSocketRules{Mode: AccessModeOpen}
			}
		}
	}
	axes.Network.Deny = omitNetworkEntries(
		axes.Network.Deny, networkDenyOmitted)
	return axes, nil
}

func omitNetworkEntries(
	entries []NetworkAllowEntry,
	omitted []int,
) []NetworkAllowEntry {
	if len(omitted) == 0 {
		return entries
	}
	indices := make(map[int]struct{}, len(omitted))
	for _, index := range omitted {
		indices[index] = struct{}{}
	}
	out := make([]NetworkAllowEntry, 0, len(entries))
	for i, entry := range entries {
		if _, ok := indices[i]; ok {
			continue
		}
		entry.Ports = append([]int(nil), entry.Ports...)
		out = append(out, entry)
	}
	return out
}

func networkRulesContained(parent, child NetworkRules) bool {
	// A replacement must retain every launched deny. Requiring explicit
	// coverage is intentionally conservative even when the child baseline
	// would happen to make a particular deny redundant.
	for _, denied := range parent.Deny {
		if !networkEntryCoveredByAny(denied, child.Deny) {
			return false
		}
	}
	if child.Mode == AccessModeUnset {
		return parent.Mode == AccessModeUnset || parent.Mode == AccessModeOpen
	}
	if parent.Mode == AccessModeUnset {
		return child.Mode == AccessModeClosed || child.Mode == AccessModeList
	}
	if child.Mode == AccessModeClosed {
		return true
	}
	// An authored empty allow list is semantically as narrow as closed. It is
	// kept as list in the model so the composition warning remains truthful,
	// but it never adds authority beneath a closed parent.
	if child.Mode == AccessModeList && len(child.Allow) == 0 {
		return true
	}
	if parent.Mode == AccessModeOpen {
		return true
	}
	if parent.Mode == AccessModeClosed || child.Mode == AccessModeOpen {
		return false
	}
	if parent.Mode != AccessModeList || child.Mode != AccessModeList {
		return false
	}
	for _, entry := range child.Allow {
		covered := false
		for _, allowed := range parent.Allow {
			selector, ok := intersectNetworkSelector(allowed, entry)
			if !ok {
				continue
			}
			ports, ok := intersectPorts(allowed.Ports, entry.Ports)
			if !ok {
				continue
			}
			selector.Ports = ports
			if networkEntryKey(selector) != networkEntryKey(entry) {
				continue
			}
			covered = true
			break
		}
		if !covered {
			return false
		}
	}
	return true
}

func networkEntryCoveredByAny(
	entry NetworkAllowEntry,
	covers []NetworkAllowEntry,
) bool {
	for _, cover := range covers {
		selector, ok := intersectNetworkSelector(cover, entry)
		if !ok {
			continue
		}
		ports, ok := intersectPorts(cover.Ports, entry.Ports)
		if !ok {
			continue
		}
		selector.Ports = ports
		if networkEntryKey(selector) == networkEntryKey(entry) {
			return true
		}
	}
	return false
}

func unixSocketRulesContained(parent, child UnixSocketRules) bool {
	if child.Mode == AccessModeUnset {
		return parent.Mode == AccessModeUnset || parent.Mode == AccessModeOpen
	}
	if parent.Mode == AccessModeUnset {
		return child.Mode == AccessModeClosed || child.Mode == AccessModeList
	}
	if child.Mode == AccessModeClosed {
		return true
	}
	if child.Mode == AccessModeList && len(child.Allow) == 0 {
		return true
	}
	if parent.Mode == AccessModeOpen {
		return true
	}
	if parent.Mode == AccessModeClosed || child.Mode == AccessModeOpen {
		return false
	}
	if parent.Mode != AccessModeList || child.Mode != AccessModeList {
		return false
	}
	for _, entry := range child.Allow {
		covered := false
		for _, allowed := range parent.Allow {
			intersection, ok := intersectSocketSelector(allowed, entry)
			if ok && intersection == entry {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// HasCapabilities reports whether a resolved snapshot adds inherited host
// filesystem, literal environment, explicit Internet authority, or Darwin
// Mach service-registration authority. Deny and offline entries are
// restrictions, and agent-owned directories are fresh private bindings rather
// than capabilities inherited from the parent.
func HasCapabilities(snapshot Snapshot) bool {
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Access != AccessDeny {
			return true
		}
	}
	return len(snapshot.Effective.Environment) > 0 ||
		snapshot.Effective.DarwinAllowMachRegister ||
		snapshot.Effective.NetworkAccess == NetworkAccessInternet ||
		(snapshot.Effective.Network != nil &&
			(snapshot.Effective.Network.Mode == AccessModeOpen ||
				(snapshot.Effective.Network.Mode == AccessModeList && len(snapshot.Effective.Network.Allow) > 0))) ||
		(snapshot.Effective.UnixSockets != nil &&
			(snapshot.Effective.UnixSockets.Mode == AccessModeOpen ||
				(snapshot.Effective.UnixSockets.Mode == AccessModeList && len(snapshot.Effective.UnixSockets.Allow) > 0)))
}

// Snapshot is the immutable, versioned value passed across launch and
// lifecycle boundaries. Version zero means no trusted snapshot was recorded
// (for example, an agent created before snapshot support) and must not be
// interpreted as an empty-but-authorized policy.
type Snapshot struct {
	Version int `json:"version"`
	// ProfilesOmitted records an explicit launch contract that suppresses every
	// tclaude sandbox-profile tier. It keeps resume/reincarnate from later
	// reapplying newly assigned global or group values to an opted-out agent.
	ProfilesOmitted bool `json:"profiles_omitted,omitempty"`
	// ResolutionGroupID is the launch group whose sandbox assignment supplied
	// the group tier. It is recorded even when that group had no profile, so a
	// later resume can pick up a new assignment without guessing among an
	// actor's other memberships. Zero is the legacy/ungrouped sentinel.
	ResolutionGroupID int64            `json:"resolution_group_id,omitempty"`
	Effective         EffectiveProfile `json:"effective"`
	Applied           []AppliedProfile `json:"applied"`
	// UnixSocketMaterialization is launch-derived, not authored authority. It
	// freezes the one filesystem observation shared by disclosure and the
	// target adapter; every fresh launch replaces it.
	UnixSocketMaterialization *UnixSocketMaterialization `json:"unix_socket_materialization,omitempty"`
}

// NewSnapshot freezes a resolver result and its stable registry provenance.
// It clones every slice/map so later caller mutation cannot widen the launch.
func NewSnapshot(effective EffectiveProfile, applied []AppliedProfile) Snapshot {
	return Snapshot{
		Version:   SnapshotVersion,
		Effective: cloneEffectiveProfile(effective),
		Applied:   append([]AppliedProfile(nil), applied...),
	}
}

// SetUnixSocketLaunchMaterialization replaces the filesystem-dependent launch
// surface and its disclosure as one record. Passing nil clears both.
func SetUnixSocketLaunchMaterialization(
	snapshot *Snapshot,
	result *UnixSocketMaterialization,
) {
	if snapshot == nil {
		return
	}
	snapshot.UnixSocketMaterialization = cloneUnixSocketMaterialization(result)
	current := []AccessNotice{}
	if notice := UnixSocketLaunchNotice(result); notice != nil {
		current = append(current, *notice)
	}
	snapshot.Effective.AccessNotices = ReplaceAccessLaunchNotices(
		snapshot.Effective.AccessNotices, current...,
	)
}

// EmptySnapshot is an explicit resolved policy with no sandbox profiles. It
// differs from Snapshot{}: the latter is the fail-closed legacy/missing state.
func EmptySnapshot() Snapshot {
	effective, _ := Resolve(Scopes{})
	return NewSnapshot(effective, nil)
}

// OmittedProfilesSnapshot is an explicit resolved policy whose launch contract
// suppresses all ambient and explicit sandbox-profile tiers.
func OmittedProfilesSnapshot() Snapshot {
	out := EmptySnapshot()
	out.ProfilesOmitted = true
	return out
}

// UnconfinedLaunchSnapshot retains a profile's plain environment entries and
// audit provenance while withholding every confinement-dependent or
// host-authority field for a deliberately sandbox-off launch. The stable
// agent snapshot remains unchanged; this value exists only for the temporary
// process launch.
func UnconfinedLaunchSnapshot(in Snapshot) Snapshot {
	effective := cloneEffectiveProfile(in.Effective)
	effective.Filesystem = nil
	effective.MountAliases = nil
	effective.AgentDirectories = nil
	effective.NetworkAccess = NetworkAccessInherit
	effective.Network = nil
	effective.UnixSockets = nil
	effective.ResourceLimits = ResourceLimits{}
	effective.DarwinAllowMachRegister = false
	effective.AccessNotices = nil
	effective.Provenance.Filesystem = nil
	effective.Provenance.AgentDirectories = nil
	effective.Provenance.Network = nil
	effective.Provenance.UnixSockets = nil
	effective.Provenance.ResourceMemory = nil
	effective.Provenance.ResourceCPU = nil
	out := NewSnapshot(effective, in.Applied)
	out.ResolutionGroupID = in.ResolutionGroupID
	return out
}

// RevalidateSnapshot checks a frozen payload immediately before use. It
// re-runs canonical path, protected-root, environment, and aggregate checks.
// Missing paths remain valid and may later become ordinary directories at the
// same canonical path. They stay inactive for a launch while absent; a
// symlink/rename retarget that changes the normalized bytes is rejected rather
// than silently redirecting authority.
func RevalidateSnapshot(in Snapshot) (Snapshot, error) {
	var err error
	in, err = NormalizeSnapshotVersion(in)
	if err != nil {
		return Snapshot{}, err
	}
	if in.ProfilesOmitted && (len(in.Applied) > 0 ||
		len(in.Effective.Filesystem) > 0 ||
		len(in.Effective.MountAliases) > 0 ||
		len(in.Effective.Environment) > 0 ||
		len(in.Effective.AgentDirectories) > 0 ||
		in.Effective.NetworkAccess != NetworkAccessInherit ||
		in.Effective.Network != nil ||
		in.Effective.UnixSockets != nil ||
		in.Effective.ResourceLimits.Enabled() ||
		in.Effective.DarwinAllowMachRegister ||
		len(in.Effective.AccessNotices) > 0 ||
		in.UnixSocketMaterialization != nil) {
		return Snapshot{}, fmt.Errorf("omitted sandbox-profile snapshot contains profile values")
	}
	normalized, _, err := NormalizeForPersistence(Profile{
		Name:                    "effective-sandbox-snapshot",
		Filesystem:              in.Effective.Filesystem,
		Environment:             in.Effective.Environment,
		NetworkAccess:           in.Effective.NetworkAccess,
		UnixSockets:             in.Effective.UnixSockets,
		ResourceLimits:          in.Effective.ResourceLimits,
		DarwinAllowMachRegister: in.Effective.DarwinAllowMachRegister,
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("revalidate effective sandbox snapshot: %w", err)
	}
	normalized.Network, err = normalizeEffectiveNetworkRules(
		in.Effective.Network)
	if err != nil {
		return Snapshot{}, fmt.Errorf(
			"revalidate effective sandbox network rules: %w", err)
	}
	if normalized.Network != nil {
		if err := validateLegacyNetworkAgreement(
			normalized.NetworkAccess, normalized.Network.Mode,
		); err != nil {
			return Snapshot{}, fmt.Errorf(
				"revalidate effective sandbox network rules: %w", err)
		}
	}
	if !reflect.DeepEqual(normalized.Filesystem, in.Effective.Filesystem) {
		return Snapshot{}, fmt.Errorf("effective sandbox filesystem changed since resolution")
	}
	aliases, err := renderMountAliases(in.Effective.MountAliases)
	if err != nil {
		return Snapshot{}, fmt.Errorf("revalidate effective sandbox mount aliases: %w", err)
	}
	if !reflect.DeepEqual(aliases, in.Effective.MountAliases) {
		return Snapshot{}, fmt.Errorf("effective sandbox mount aliases changed since resolution")
	}
	for _, alias := range aliases {
		info, err := os.Lstat(alias.Link)
		if err != nil {
			return Snapshot{}, fmt.Errorf("revalidate effective sandbox mount alias %q: %w", alias.Link, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return Snapshot{}, fmt.Errorf("effective sandbox mount alias %q is no longer a symlink", alias.Link)
		}
		target, err := filepath.EvalSymlinks(alias.Link)
		if err != nil {
			return Snapshot{}, fmt.Errorf("revalidate effective sandbox mount alias %q: %w", alias.Link, err)
		}
		if filepath.Clean(target) != alias.Target {
			return Snapshot{}, fmt.Errorf(
				"effective sandbox mount alias %q changed target since resolution",
				alias.Link,
			)
		}
	}
	if !reflect.DeepEqual(normalized.Environment, in.Effective.Environment) {
		return Snapshot{}, fmt.Errorf("effective sandbox environment changed since resolution")
	}
	if normalized.NetworkAccess != in.Effective.NetworkAccess {
		return Snapshot{}, fmt.Errorf("effective sandbox network access changed since resolution")
	}
	if !reflect.DeepEqual(normalized.Network, in.Effective.Network) {
		return Snapshot{}, fmt.Errorf("effective sandbox network rules changed since resolution")
	}
	if !reflect.DeepEqual(normalized.UnixSockets, in.Effective.UnixSockets) {
		return Snapshot{}, fmt.Errorf("effective sandbox Unix-socket rules changed since resolution")
	}
	if !reflect.DeepEqual(normalized.ResourceLimits, in.Effective.ResourceLimits) {
		return Snapshot{}, fmt.Errorf("effective sandbox resource limits changed since resolution")
	}
	if in.UnixSocketMaterialization != nil {
		planned, err := PlannedEffectiveAccessAxes(in.Effective)
		if err != nil {
			return Snapshot{}, fmt.Errorf(
				"revalidate materialized Unix-socket launch surface: %w", err)
		}
		if planned.UnixSockets.Mode != AccessModeList {
			return Snapshot{}, fmt.Errorf(
				"materialized Unix-socket launch surface requires a rendered socket list")
		}
	}
	agentDirectories, err := normalizeAgentDirectories(in.Effective.AgentDirectories, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("revalidate effective sandbox agent directories: %w", err)
	}
	if !slices.Equal(agentDirectories, in.Effective.AgentDirectories) {
		return Snapshot{}, fmt.Errorf("effective sandbox agent directories changed since resolution")
	}
	out := NewSnapshot(in.Effective, in.Applied)
	out.ResolutionGroupID = in.ResolutionGroupID
	out.ProfilesOmitted = in.ProfilesOmitted
	out.UnixSocketMaterialization = cloneUnixSocketMaterialization(
		in.UnixSocketMaterialization)
	return out, nil
}

// NormalizeSnapshotVersion upgrades a structurally compatible legacy
// snapshot without touching the filesystem. Persistence readers use it before
// returning bookkeeping rows; authority-use boundaries must still call
// RevalidateSnapshot before applying the result.
func NormalizeSnapshotVersion(in Snapshot) (Snapshot, error) {
	switch in.Version {
	// v1 and v2 are structurally compatible: they simply carry neither TCL-609
	// field, which decodes to the zero value that means "today's behavior".
	// v3/v4 additionally carried read_baseline and read_baseline_exclusions.
	// TCL-623 removed that mechanism outright, so those fields simply do not
	// decode any more and the restriction is dropped rather than reinterpreted.
	// That is the deliberate operator decision — the feature had no users, and
	// silently claiming to enforce a mechanism this binary no longer implements
	// would be worse than dropping it. v5 carries no ProfilesOmitted marker, so
	// false preserves its ambient-resolution behavior.
	//
	// v3–v6 could additionally carry break_glass_filesystem rows and their
	// provenance. TCL-791 removed break-glass, so those fields no longer decode
	// and any authority they carried is DROPPED. The direction matters: dropping
	// a grant strictly narrows the snapshot, so the upgrade is fail-closed and a
	// resumed agent simply loses access it can no longer be given. Refusing such
	// snapshots instead would strand live agents on upgrade for no security
	// gain, and there is no operator present at decode time to receive an error
	// — the v163 migration's durable disclosure is what informs them.
	case 1, 2, 3, 4, 5, 6, 7, 8, SnapshotVersion:
		in.Version = SnapshotVersion
		return in, nil
	default:
		return Snapshot{}, fmt.Errorf("unsupported sandbox snapshot version %d", in.Version)
	}
}

// FilesystemForLaunch returns the rules safe to hand to a harness now. Missing
// read/write paths stay frozen but inactive until a later launch. Missing deny
// paths fail closed because omitting them would silently remove a restriction.
// Re-canonicalizing each path also detects an ancestor symlink substitution in
// the window after snapshot revalidation rather than activating a redirected
// textual rule.
func FilesystemForLaunch(in EffectiveProfile) ([]FilesystemGrant, error) {
	out := make([]FilesystemGrant, 0, len(in.Filesystem))
	for _, grant := range in.Filesystem {
		canonical, missing, err := canonicalDirectory(grant.Path, true)
		if err != nil {
			return nil, fmt.Errorf("prepare filesystem %s rule %q for launch: %w", grant.Access, grant.Path, err)
		}
		if canonical != grant.Path {
			return nil, fmt.Errorf("filesystem rule %q changed canonical target before launch", grant.Path)
		}
		if missing {
			if grant.Access == AccessDeny {
				return nil, fmt.Errorf("filesystem deny rule %q does not exist and cannot be enforced", grant.Path)
			}
			continue
		}
		out = append(out, grant)
	}
	return out, nil
}

func cloneEffectiveProfile(in EffectiveProfile) EffectiveProfile {
	out := EffectiveProfile{
		Filesystem:              append([]FilesystemGrant{}, in.Filesystem...),
		MountAliases:            append([]MountAlias(nil), in.MountAliases...),
		Environment:             append([]EnvironmentEntry{}, in.Environment...),
		AgentDirectories:        append([]string{}, in.AgentDirectories...),
		NetworkAccess:           in.NetworkAccess,
		Network:                 cloneNetworkRulesPtr(in.Network),
		UnixSockets:             cloneUnixSocketRulesPtr(in.UnixSockets),
		ResourceLimits:          cloneResourceLimits(in.ResourceLimits),
		DarwinAllowMachRegister: in.DarwinAllowMachRegister,
		AccessNotices:           cloneAccessNotices(in.AccessNotices),
		Provenance: ResolutionProvenance{
			Applied:          cloneProfileSources(in.Provenance.Applied),
			Filesystem:       make(map[string][]ProfileSource, len(in.Provenance.Filesystem)),
			Environment:      make(map[string]ProfileSource, len(in.Provenance.Environment)),
			AgentDirectories: make(map[string][]ProfileSource, len(in.Provenance.AgentDirectories)),
			Network:          nil,
			UnixSockets:      nil,
			ResourceMemory:   nil,
			ResourceCPU:      nil,
		},
	}
	for path, sources := range in.Provenance.Filesystem {
		out.Provenance.Filesystem[path] = cloneProfileSources(sources)
	}
	for name, source := range in.Provenance.Environment {
		out.Provenance.Environment[name] = cloneProfileSource(source)
	}
	for name, sources := range in.Provenance.AgentDirectories {
		out.Provenance.AgentDirectories[name] = cloneProfileSources(sources)
	}
	if in.Provenance.Network != nil {
		source := cloneProfileSource(*in.Provenance.Network)
		out.Provenance.Network = &source
	}
	if in.Provenance.UnixSockets != nil {
		source := cloneProfileSource(*in.Provenance.UnixSockets)
		out.Provenance.UnixSockets = &source
	}
	if in.Provenance.ResourceMemory != nil {
		source := cloneProfileSource(*in.Provenance.ResourceMemory)
		out.Provenance.ResourceMemory = &source
	}
	if in.Provenance.ResourceCPU != nil {
		source := cloneProfileSource(*in.Provenance.ResourceCPU)
		out.Provenance.ResourceCPU = &source
	}
	// The SAME canonical order normalizeFilesystem produces. RevalidateSnapshot
	// compares the two with an order-sensitive DeepEqual, so a snapshot sorted by
	// host path while resolution sorts by guest path would fail revalidation for
	// every profile that uses a mount path.
	sortFilesystemGrants(out.Filesystem)
	sort.Slice(out.Environment, func(i, j int) bool { return out.Environment[i].Name < out.Environment[j].Name })
	sort.Strings(out.AgentDirectories)
	return out
}

func cloneAccessNotices(in []AccessNotice) []AccessNotice {
	if in == nil {
		return nil
	}
	out := make([]AccessNotice, len(in))
	for i, notice := range in {
		out[i] = notice
		out[i].Entries = append([]int(nil), notice.Entries...)
		out[i].Tiers = append([]string(nil), notice.Tiers...)
	}
	return out
}

func cloneUnixSocketMaterialization(
	in *UnixSocketMaterialization,
) *UnixSocketMaterialization {
	if in == nil {
		return nil
	}
	out := *in
	out.Paths = append([]string(nil), in.Paths...)
	out.Unmaterialized = append([]string(nil), in.Unmaterialized...)
	out.Entries = append([]int(nil), in.Entries...)
	return &out
}

// cloneProfileSource is a value copy: ProfileSource has held only comparable
// scalar fields since TCL-791 removed the Chain slice, so there is nothing
// left to deep-copy. It stays as a named seam so a future non-scalar field
// gets cloned here rather than aliased into a frozen snapshot.
func cloneProfileSource(in ProfileSource) ProfileSource { return in }

func cloneProfileSources(in []ProfileSource) []ProfileSource {
	if in == nil {
		return nil
	}
	out := make([]ProfileSource, len(in))
	for i, source := range in {
		out[i] = cloneProfileSource(source)
	}
	return out
}
