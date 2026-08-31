package sandboxpolicy

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// A sandbox profile may mount one or more temporary filesystems inside the
// sandbox (TCL-1218). A tmpfs row is the primitive the filesystem rows cannot
// express: writable scratch space that exists only for the life of the launch,
// backed by no host directory at all.
//
// It is a different KIND of rule from a filesystem grant, which is why it is a
// sibling profile field rather than a fourth `access` value. A grant's `path`
// is the authority-bearing HOST side — it is symlink-resolved, checked for
// directory-ness, and compared against the protected roots. A tmpfs row has no
// host side to bear authority: it confers access to nothing that already
// exists. What it names is purely a position inside the namespace, exactly like
// a grant's `mount_path`, and it is validated the same way — syntactically,
// because the namespace it names does not exist yet.
//
// The three things a tmpfs row IS, stated plainly, because each one is a
// property the appliers rely on:
//
//   - It grants no host authority. Nothing on the host becomes reachable.
//   - It SHADOWS whatever occupied that sandbox path before it, so it takes
//     part in the ordinary most-specific-wins ordering rather than sitting
//     outside it. A narrower grant beneath a tmpfs still lands on top.
//   - It is writable and stays writable. That is the whole point, and it is the
//     one way it differs mechanically from a `deny`, which the tclaude-layer
//     already renders as an empty tmpfs plus a read-only remount.

// MaxTmpfsMountCount bounds how many temporary filesystems one profile may
// author. Each one is a real kernel mount, and the bound is generous next to
// any plausible real profile.
const MaxTmpfsMountCount = 32

// TmpfsMount is one operator-authored temporary filesystem.
//
// Path is a SANDBOX-side path. Size retains the operator's authored spelling
// for portable export and UI editing, in the same shape ResourceLimits.Memory
// uses; SizeBytes is the derived value the appliers render and is never
// authored on its own.
//
// An omitted size is the kernel's own tmpfs default, which is half of RAM.
// That is deliberately not silently capped here: picking a number on the
// operator's behalf would be a ceiling they never authored and cannot see. It
// IS worth authoring — an agent that fills an uncapped tmpfs consumes host
// memory — and the docs say so.
type TmpfsMount struct {
	Path      string `json:"path"`
	Size      string `json:"size,omitempty"`
	SizeBytes uint64 `json:"size_bytes,omitempty"`
}

// ParseTmpfsSizeBytes accepts the same Kubernetes-like quantities as a memory
// limit, so an operator writes `512MiB` in both places and means the same
// thing.
func ParseTmpfsSizeBytes(input string) (uint64, error) {
	return parseByteQuantity("tmpfs size", input)
}

// normalizeTmpfs validates and canonicalizes the authored rows.
//
// Duplicate paths FOLD rather than erroring, matching the filesystem lattice's
// treatment of two spellings of one rule, and the surviving row is the
// strictest: a bounded size beats an unbounded one, and the smaller of two
// bounds wins. Folding to the strictest is what lets composition stay
// order-independent — see mergeTmpfsStrictest.
func normalizeTmpfs(in []TmpfsMount, protected []string) ([]TmpfsMount, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxTmpfsMountCount {
		return nil, fmt.Errorf("tmpfs has too many entries (maximum %d)", MaxTmpfsMountCount)
	}
	sockets := AgentdSocketFloor()
	byPath := make(map[string]TmpfsMount, len(in))
	for i, mount := range in {
		path, err := cleanDirectoryPath(strings.TrimSpace(mount.Path))
		if err != nil {
			return nil, fmt.Errorf("tmpfs[%d].path: %w", i, err)
		}
		// Mounting a fresh empty filesystem over the sandbox root would leave
		// the launch with no harness, no /usr, and no explanation. The renderer
		// owns the root; a profile row never does.
		if path == string(filepath.Separator) {
			return nil, fmt.Errorf("tmpfs[%d].path must not be the sandbox root %q", i, path)
		}
		// Same wall, and for the same reason, as a mount_path's guest side: a
		// tmpfs is unshadowable once mounted, so one placed over tclaude's own
		// machinery would either be overridden later — making the authored row
		// a lie — or cut the agent off from the thing it was placed over.
		// Compare BOTH the authored spelling and its canonical form, because
		// the protected wall surrounds real directories and one host directory
		// has several spellings.
		for _, candidate := range []string{path, canonicalGuestPathForComparison(path)} {
			for _, denied := range protected {
				if GuardPathsIntersect(candidate, denied) {
					return nil, fmt.Errorf(
						"tmpfs[%d].path %q intersects protected directory %q", i, path, denied)
				}
			}
			for _, socket := range sockets {
				if GuardContainsOrEqual(candidate, socket) ||
					GuardContainsOrEqual(candidate, canonicalGuestPathForComparison(socket)) {
					return nil, fmt.Errorf(
						"tmpfs[%d].path %q would shadow the agentd control socket %q", i, path, socket)
				}
			}
		}
		out := TmpfsMount{Path: path, Size: strings.TrimSpace(mount.Size)}
		if out.Size != "" {
			bytes, err := ParseTmpfsSizeBytes(out.Size)
			if err != nil {
				return nil, fmt.Errorf("tmpfs[%d].size: %w", i, err)
			}
			out.SizeBytes = bytes
		} else if mount.SizeBytes != 0 {
			return nil, fmt.Errorf(
				"tmpfs[%d].size_bytes is derived and requires an authored size", i)
		}
		byPath[path] = mergeTmpfsStrictest(byPath[path], out)
	}
	return sortedTmpfsMounts(byPath), nil
}

// mergeTmpfsStrictest folds two rows for one sandbox path.
//
// "Strictest" is the smaller ceiling, and an authored ceiling always beats an
// omitted one — an omitted size is the kernel default of half of RAM, so it is
// the LOOSEST value on this axis rather than a neutral one. Keeping the
// strictest is what makes the fold commutative, which is what lets the
// cross-scope union in Resolve stay independent of tier order, exactly as the
// deny-dominates-write-dominates-read lattice does for grants.
//
// Equal ceilings need a tie-break, and it is not cosmetic. `1Mi` and `1048576`
// are the same number of bytes and different authored SPELLINGS, and the
// spelling is retained for export and UI editing — so without a tie-break the
// merged row's Size would depend on which tier happened to be folded first,
// and the same two profiles would persist and export differently depending on
// the order they were composed in. Picking the lexically smaller spelling is
// arbitrary but total, which is all commutativity needs.
//
// The zero value merges as "nothing here yet", so callers can fold into an
// absent map entry without a lookup dance.
func mergeTmpfsStrictest(current, next TmpfsMount) TmpfsMount {
	if current.Path == "" {
		return next
	}
	if next.Path == "" {
		return current
	}
	if current.SizeBytes == 0 {
		return next
	}
	if next.SizeBytes == 0 {
		return current
	}
	if next.SizeBytes != current.SizeBytes {
		if next.SizeBytes < current.SizeBytes {
			return next
		}
		return current
	}
	if next.Size < current.Size {
		return next
	}
	return current
}

func sortedTmpfsMounts(byPath map[string]TmpfsMount) []TmpfsMount {
	if len(byPath) == 0 {
		return nil
	}
	out := make([]TmpfsMount, 0, len(byPath))
	for _, mount := range byPath {
		out = append(out, mount)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func cloneTmpfsMounts(in []TmpfsMount) []TmpfsMount {
	if in == nil {
		return nil
	}
	return append([]TmpfsMount(nil), in...)
}

// SupportsTmpfsMounts reports whether an implementation on a platform can mount
// a temporary filesystem inside the sandbox.
//
// A tmpfs is a mount, so it needs a mount namespace whose whole boundary
// tclaude owns — the same requirement a single-file bind has, and refused in
// the same places:
//
//   - macOS Seatbelt is a path filter over the host namespace. It can allow or
//     deny a path; it cannot conjure a filesystem at one.
//   - harness-builtin sandboxes receive path lists and confine the harness in
//     the host namespace. There is nowhere to express a mount.
//   - `stacked` refuses too, even on Linux. tclaude's outer layer would mount
//     the tmpfs, and then the inner harness-native wall — fed from the same
//     directory lists, which carry host paths only — would deny the harness's
//     own tools every write to it. A scratch directory the agent can see and
//     cannot use is worse than a refusal, because only the refusal says why.
//   - `resource-only` and `off` stand every access boundary down and build no
//     namespace at all.
func SupportsTmpfsMounts(implementation Implementation, goos string) bool {
	return implementation == ImplementationTclaudeLayer && goos == "linux"
}

// ValidateTmpfsSupport refuses a launch whose profile needs a capability the
// resolved implementation does not have. Like the mount-path and file-grant
// gates it is a named refusal rather than a degradation: launching without the
// tmpfs would hand the agent whatever the host happens to have at that path,
// or nothing at all, and neither is the rule the operator wrote.
func ValidateTmpfsSupport(
	mounts []TmpfsMount,
	implementation Implementation,
	goos string,
) error {
	if len(mounts) == 0 || SupportsTmpfsMounts(implementation, goos) {
		return nil
	}
	return fmt.Errorf(
		"unsupported_sandbox_profile_tmpfs: sandbox implementation %q on %s cannot mount the temporary filesystem at %q; a tmpfs requires a mount namespace whose whole boundary tclaude owns, which only the Linux tclaude-layer provides",
		implementation, goos, mounts[0].Path)
}

// TmpfsPaths returns the authored sandbox paths in plan order, so a disclosure
// surface can say WHICH mounts a launch could not apply instead of only how
// many.
func TmpfsPaths(mounts []TmpfsMount) []string {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		out = append(out, mount.Path)
	}
	return out
}

// validateTmpfsAgainstFilesystem refuses the one overlap that is a conflict of
// intent rather than ordinary most-specific-wins: a tmpfs and a filesystem rule
// claiming the SAME sandbox path.
//
// Overlap in general is fine and is the point of the ordering — a `write` row
// beneath a tmpfs lands on top of it, and a tmpfs beneath a broad `read` hides
// that subtree behind scratch space. Equality is different, because the two
// rows then describe one mount and disagree about what it is: `deny` wants the
// path hidden and read-only, a read/write row wants a host directory projected
// there, and a tmpfs wants empty writable scratch. Folding them would silently
// discard one of the operator's rules, so refuse and name both.
func validateTmpfsAgainstFilesystem(mounts []TmpfsMount, grants []FilesystemGrant) error {
	if len(mounts) == 0 || len(grants) == 0 {
		return nil
	}
	byGuest := make(map[string]FilesystemGrant, len(grants))
	for _, grant := range grants {
		byGuest[grant.GuestPath()] = grant
	}
	for i, mount := range mounts {
		grant, exists := byGuest[mount.Path]
		if !exists {
			continue
		}
		return fmt.Errorf(
			"tmpfs[%d].path %q is also claimed by the %s filesystem rule for %q; a sandbox path is either a temporary filesystem or a filesystem rule, not both",
			i, mount.Path, grant.Access, grant.Path)
	}
	return nil
}
