package sandboxpolicy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The spawn-time deny overlay (tcl-ma3551): paths a spawner denies its child
// on top of whatever sandbox profiles the child resolves. It is stored on the
// child's snapshot, reapplied at every relaunch, and handed to the child's own
// children with their own denies added, so denies only accumulate down a
// lineage.
//
// A denied path covers its whole subtree. A more specific grant beneath it,
// which ordinary profile composition would keep (the most specific rule wins),
// is removed instead, and the removal is named in a composition notice. The
// overlay is a promise that nothing under the path stays reachable, and since
// it outlives the profiles it was laid over, a later profile edit that adds a
// grant beneath it must not quietly reopen it. Removing rather than refusing
// keeps the agent's relaunches working through such an edit.

// ScopeDenyOverlay names the overlay where provenance names a profile scope.
const ScopeDenyOverlay Scope = "deny_overlay"

// DenyOverlaySourceName is the overlay's name in provenance.
const DenyOverlaySourceName = "spawn deny overlay"

// AccessNoticeReasonDenyOverlayRemoved marks the notice naming what the
// overlay removed from this launch's composition.
const AccessNoticeReasonDenyOverlayRemoved = "deny_overlay_removed"

// AccessNoticeEffectRemoved is that notice's effect: the named access is gone.
const AccessNoticeEffectRemoved = "removed"

// MaxDenyOverlayCount bounds an overlay, inherited entries included.
const MaxDenyOverlayCount = 64

// ErrDenyOverlayUnenforced refuses an overlay on a launch that omits its
// sandbox profiles, where no filesystem policy exists to carry it.
var ErrDenyOverlayUnenforced = errors.New("this launch omits its sandbox profiles, so it cannot enforce the spawn deny overlay")

// NormalizeDenyOverlay canonicalizes overlay paths the way a profile's deny
// grants are canonicalized, sorted, with a path beneath another denied path
// folded into it.
func NormalizeDenyOverlay(paths []string) ([]string, error) {
	grants := make([]FilesystemGrant, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return nil, errors.New("a sandbox deny path is empty")
		}
		grants = append(grants, FilesystemGrant{Path: path, Access: AccessDeny})
	}
	if len(grants) == 0 {
		return nil, nil
	}
	normalized, _, err := normalizeFilesystem(grants, true)
	if err != nil {
		return nil, fmt.Errorf("sandbox deny: %w", err)
	}
	canonical := make([]string, 0, len(normalized))
	for _, grant := range normalized {
		canonical = append(canonical, grant.Path)
	}
	sort.Strings(canonical)
	out := canonical[:0]
	for _, path := range canonical {
		if len(out) > 0 && pathContainsOrEqual(out[len(out)-1], path) {
			continue
		}
		out = append(out, path)
	}
	if len(out) > MaxDenyOverlayCount {
		return nil, fmt.Errorf("sandbox deny has %d paths (maximum %d)", len(out), MaxDenyOverlayCount)
	}
	return out, nil
}

// DenyOverlayCovers returns the overlay path that covers path, or "".
func DenyOverlayCovers(overlay []string, path string) string {
	for _, deny := range overlay {
		if pathContainsOrEqual(deny, path) {
			return deny
		}
	}
	return ""
}

// ApplyDenyOverlay lays the snapshot's own overlay plus add over its effective
// profile and records the result as the snapshot's overlay. Applying it again
// is a no-op, so a relaunch reapplies it to a freshly resolved chain and to
// stored contents alike.
func ApplyDenyOverlay(s Snapshot, add []string) (Snapshot, error) {
	denies, err := NormalizeDenyOverlay(append(append([]string(nil), s.DenyOverlay...), add...))
	if err != nil {
		return Snapshot{}, err
	}
	if len(denies) == 0 {
		return s, nil
	}
	if s.ProfilesOmitted {
		return Snapshot{}, ErrDenyOverlayUnenforced
	}
	out := s
	out.Effective = cloneEffectiveProfile(s.Effective)
	out.DenyOverlay = denies
	effective := &out.Effective
	source := ProfileSource{Scope: ScopeDenyOverlay, Profile: DenyOverlaySourceName}

	var removed []string
	grants := make([]FilesystemGrant, 0, len(effective.Filesystem)+len(denies))
	sources := map[string][]ProfileSource{}
	present := map[string]bool{}
	for _, grant := range effective.Filesystem {
		deny := DenyOverlayCovers(denies, grant.Path)
		if deny == "" {
			deny = DenyOverlayCovers(denies, grant.GuestPath())
		}
		switch {
		case deny == "":
			// Untouched.
		case grant.Path == deny && grant.GuestPath() == deny:
			// The denied path itself: it is a deny now, whatever it was.
			grant.Access = AccessDeny
			present[deny] = true
			sources[grant.Path] = append(sources[grant.Path], source)
		case grant.Access == AccessDeny:
			// A deny beneath the overlay narrows nothing further but takes
			// nothing away either.
		default:
			removed = append(removed, fmt.Sprintf("%s %s", grant.Access, describeGrantPath(grant)))
			continue
		}
		grants = append(grants, grant)
		sources[grant.Path] = append(sources[grant.Path], effective.Provenance.Filesystem[grant.Path]...)
	}
	for _, deny := range denies {
		if !present[deny] {
			grants = append(grants, FilesystemGrant{Path: deny, Access: AccessDeny})
			sources[deny] = append(sources[deny], source)
		}
	}
	// The composed set is checked as Resolve checks its own: canonical paths,
	// no two host directories on one sandbox path, and the cross-rule
	// mount-path invariants.
	normalized, _, err := normalizeFilesystem(grants, true)
	if err != nil {
		return Snapshot{}, fmt.Errorf("apply sandbox deny: %w", err)
	}
	protected, err := protectedPaths()
	if err != nil {
		return Snapshot{}, err
	}
	if err := validateMountPaths(normalized, protected); err != nil {
		return Snapshot{}, fmt.Errorf("apply sandbox deny: %w", err)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].GuestPath() < normalized[j].GuestPath() })
	effective.Filesystem = normalized
	provenance := make(map[string][]ProfileSource, len(normalized))
	for _, grant := range normalized {
		provenance[grant.Path] = canonicalSources(sources[grant.Path])
	}
	effective.Provenance.Filesystem = provenance

	var tmpfs []TmpfsMount
	for _, mount := range effective.Tmpfs {
		if DenyOverlayCovers(denies, mount.Path) != "" {
			removed = append(removed, "tmpfs "+mount.Path)
			delete(effective.Provenance.Tmpfs, mount.Path)
			continue
		}
		tmpfs = append(tmpfs, mount)
	}
	effective.Tmpfs = tmpfs
	if err := validateTmpfsAgainstFilesystem(effective.Tmpfs, effective.Filesystem); err != nil {
		return Snapshot{}, fmt.Errorf("apply sandbox deny: %w", err)
	}
	directories := effective.AgentDirectories[:0:0]
	for _, dir := range effective.AgentDirectories {
		if DenyOverlayCovers(denies, dir) != "" {
			removed = append(removed, "agent directory "+dir)
			delete(effective.Provenance.AgentDirectories, dir)
			continue
		}
		directories = append(directories, dir)
	}
	effective.AgentDirectories = directories
	aliases := effective.MountAliases[:0:0]
	for _, alias := range effective.MountAliases {
		if DenyOverlayCovers(denies, alias.Target) != "" || DenyOverlayCovers(denies, alias.Link) != "" {
			continue
		}
		aliases = append(aliases, alias)
	}
	effective.MountAliases = aliases
	if len(effective.MountAliases) == 0 {
		effective.MountAliases = nil
	}

	applied := false
	for _, existing := range effective.Provenance.Applied {
		if existing == source {
			applied = true
		}
	}
	if !applied {
		effective.Provenance.Applied = append(effective.Provenance.Applied, source)
	}
	// A freshly resolved chain names what this application removed. Stored
	// contents already had it removed at an earlier launch, so their notice
	// stands rather than being cleared by a no-op application.
	if len(removed) > 0 {
		notices := make([]AccessNotice, 0, len(effective.AccessNotices)+1)
		for _, notice := range effective.AccessNotices {
			if notice.Reason != AccessNoticeReasonDenyOverlayRemoved {
				notices = append(notices, notice)
			}
		}
		sort.Strings(removed)
		notices = append(notices, AccessNotice{
			Class:  AccessNoticeClassComposition,
			Axis:   "filesystem",
			Reason: AccessNoticeReasonDenyOverlayRemoved,
			Effect: AccessNoticeEffectRemoved,
			Detail: fmt.Sprintf("The spawn deny overlay (%s) covers its whole subtree, so it removed: %s",
				strings.Join(denies, ", "), strings.Join(removed, "; ")),
		})
		effective.AccessNotices = notices
	}
	return out, nil
}

func describeGrantPath(grant FilesystemGrant) string {
	if grant.GuestPath() != grant.Path {
		return fmt.Sprintf("%s (mounted at %s)", grant.Path, grant.GuestPath())
	}
	return grant.Path
}
