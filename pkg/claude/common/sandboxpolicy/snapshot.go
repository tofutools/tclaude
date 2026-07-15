package sandboxpolicy

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"
)

const SnapshotVersion = 2

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
// possess. Filesystem coverage is segment-aware: an ancestor grant covers
// descendants, write covers read, and every parent deny must be preserved by
// an equal or broader child deny. Environment entries must match the parent's
// exact value; omission is a safe weakening. Agent-owned directories are not
// inherited host authority: agentd creates fresh private bindings for each
// child, so their declarations do not participate in containment.
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
		covered := false
		for _, parentGrant := range parent.Effective.Filesystem {
			if parentGrant.Access == AccessDeny {
				continue
			}
			if !pathContainsOrEqual(parentGrant.Path, childGrant.Path) {
				continue
			}
			if childGrant.Access == AccessWrite && parentGrant.Access != AccessWrite {
				continue
			}
			covered = true
			break
		}
		if !covered {
			return fmt.Errorf("filesystem %s grant %q is not contained by the parent snapshot", childGrant.Access, childGrant.Path)
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
	if !networkAccessContained(parent.Effective.NetworkAccess, child.Effective.NetworkAccess) {
		return fmt.Errorf("network access %q is not contained by parent access %q", child.Effective.NetworkAccess, parent.Effective.NetworkAccess)
	}
	return nil
}

func networkAccessContained(parent, child NetworkAccess) bool {
	// Inherit means the harness default, which is the broadest authority: it
	// may include ordinary IP networking and arbitrary local Unix sockets.
	if parent == NetworkAccessInherit {
		return true
	}
	if child == NetworkAccessInherit {
		return false
	}
	return parent == NetworkAccessInternet || child == NetworkAccessNone
}

// HasCapabilities reports whether a resolved snapshot adds inherited host
// filesystem or literal environment authority. Deny entries are restrictions,
// and agent-owned directories are fresh private bindings, not capabilities
// inherited from the parent.
func HasCapabilities(snapshot Snapshot) bool {
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Access != AccessDeny {
			return true
		}
	}
	return len(snapshot.Effective.Environment) > 0
}

// Snapshot is the immutable, versioned value passed across launch and
// lifecycle boundaries. Version zero means no trusted snapshot was recorded
// (for example, an agent created before snapshot support) and must not be
// interpreted as an empty-but-authorized policy.
type Snapshot struct {
	Version int `json:"version"`
	// ResolutionGroupID is the launch group whose sandbox assignment supplied
	// the group tier. It is recorded even when that group had no profile, so a
	// later resume can pick up a new assignment without guessing among an
	// actor's other memberships. Zero is the legacy/ungrouped sentinel.
	ResolutionGroupID int64            `json:"resolution_group_id,omitempty"`
	Effective         EffectiveProfile `json:"effective"`
	Applied           []AppliedProfile `json:"applied"`
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

// EmptySnapshot is an explicit resolved policy with no sandbox profiles. It
// differs from Snapshot{}: the latter is the fail-closed legacy/missing state.
func EmptySnapshot() Snapshot {
	effective, _ := Resolve(Scopes{})
	return NewSnapshot(effective, nil)
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
	normalized, _, err := NormalizeForPersistence(Profile{
		Name:          "effective-sandbox-snapshot",
		Filesystem:    in.Effective.Filesystem,
		Environment:   in.Effective.Environment,
		NetworkAccess: in.Effective.NetworkAccess,
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("revalidate effective sandbox snapshot: %w", err)
	}
	if !reflect.DeepEqual(normalized.Filesystem, in.Effective.Filesystem) {
		return Snapshot{}, fmt.Errorf("effective sandbox filesystem changed since resolution")
	}
	if !reflect.DeepEqual(normalized.Environment, in.Effective.Environment) {
		return Snapshot{}, fmt.Errorf("effective sandbox environment changed since resolution")
	}
	if normalized.NetworkAccess != in.Effective.NetworkAccess {
		return Snapshot{}, fmt.Errorf("effective sandbox network access changed since resolution")
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
	return out, nil
}

// NormalizeSnapshotVersion upgrades a structurally compatible legacy
// snapshot without touching the filesystem. Persistence readers use it before
// returning bookkeeping rows; authority-use boundaries must still call
// RevalidateSnapshot before applying the result.
func NormalizeSnapshotVersion(in Snapshot) (Snapshot, error) {
	switch in.Version {
	case 1, SnapshotVersion:
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
		Filesystem:       append([]FilesystemGrant{}, in.Filesystem...),
		Environment:      append([]EnvironmentEntry{}, in.Environment...),
		AgentDirectories: append([]string{}, in.AgentDirectories...),
		NetworkAccess:    in.NetworkAccess,
		Provenance: ResolutionProvenance{
			Applied:          append([]ProfileSource(nil), in.Provenance.Applied...),
			Filesystem:       make(map[string][]ProfileSource, len(in.Provenance.Filesystem)),
			Environment:      make(map[string]ProfileSource, len(in.Provenance.Environment)),
			AgentDirectories: make(map[string][]ProfileSource, len(in.Provenance.AgentDirectories)),
			Network:          nil,
		},
	}
	for path, sources := range in.Provenance.Filesystem {
		out.Provenance.Filesystem[path] = append([]ProfileSource(nil), sources...)
	}
	for name, source := range in.Provenance.Environment {
		out.Provenance.Environment[name] = source
	}
	for name, sources := range in.Provenance.AgentDirectories {
		out.Provenance.AgentDirectories[name] = append([]ProfileSource(nil), sources...)
	}
	if in.Provenance.Network != nil {
		source := *in.Provenance.Network
		out.Provenance.Network = &source
	}
	sort.Slice(out.Filesystem, func(i, j int) bool { return out.Filesystem[i].Path < out.Filesystem[j].Path })
	sort.Slice(out.Environment, func(i, j int) bool { return out.Environment[i].Name < out.Environment[j].Name })
	sort.Strings(out.AgentDirectories)
	return out
}
