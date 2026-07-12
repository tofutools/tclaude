package sandboxpolicy

import (
	"fmt"
	"reflect"
	"sort"
	"time"
)

const SnapshotVersion = 1

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
// exact value; omission is a safe weakening.
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
	return nil
}

// HasCapabilities reports whether a resolved snapshot adds any filesystem or
// environment authority. Deny entries are restrictions, not capabilities.
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
	Version   int              `json:"version"`
	Effective EffectiveProfile `json:"effective"`
	Applied   []AppliedProfile `json:"applied"`
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
	if in.Version != SnapshotVersion {
		return Snapshot{}, fmt.Errorf("unsupported sandbox snapshot version %d", in.Version)
	}
	normalized, _, err := NormalizeForPersistence(Profile{
		Name:        "effective-sandbox-snapshot",
		Filesystem:  in.Effective.Filesystem,
		Environment: in.Effective.Environment,
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
	out := NewSnapshot(in.Effective, in.Applied)
	return out, nil
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
		Filesystem:  append([]FilesystemGrant{}, in.Filesystem...),
		Environment: append([]EnvironmentEntry{}, in.Environment...),
		Provenance: ResolutionProvenance{
			Applied:     append([]ProfileSource(nil), in.Provenance.Applied...),
			Filesystem:  make(map[string][]ProfileSource, len(in.Provenance.Filesystem)),
			Environment: make(map[string]ProfileSource, len(in.Provenance.Environment)),
		},
	}
	for path, sources := range in.Provenance.Filesystem {
		out.Provenance.Filesystem[path] = append([]ProfileSource(nil), sources...)
	}
	for name, source := range in.Provenance.Environment {
		out.Provenance.Environment[name] = source
	}
	sort.Slice(out.Filesystem, func(i, j int) bool { return out.Filesystem[i].Path < out.Filesystem[j].Path })
	sort.Slice(out.Environment, func(i, j int) bool { return out.Environment[i].Name < out.Environment[j].Name })
	return out
}
