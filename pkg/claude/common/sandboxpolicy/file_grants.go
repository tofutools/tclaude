package sandboxpolicy

import (
	"fmt"
	"os"
)

// A filesystem rule may name a single regular FILE rather than a directory
// (TCL-1041). The rule this unlocks is the one a directory grant cannot
// express: reopening exactly ~/.gitconfig beneath a denied Home, without also
// handing over the rest of the home directory it lives in.
//
// Enforcement is a bind of that one file, which needs a real mount namespace.
// Only tclaude's own outer layer on Linux builds one, and the layer's launch
// contract has bound individual files there since the harness-config floor
// existed (see sandbox_harness_config_floor.go), so the applier side of this is
// the path already in production rather than a new mechanism.
//
// Everything else refuses:
//
//   - macOS Seatbelt and the harness-builtin sandboxes take DIRECTORY lists.
//     A file dropped into one of those lists is at best ignored and at worst an
//     argument error, so the rule would look authored and enforce nothing.
//   - `stacked` refuses too, even on Linux. Its inner wall is a harness-native
//     sandbox fed from the same directory lists, so tclaude's outer bind would
//     land while the inner wall still blocked the path. A rule enforced by one
//     wall and silently dropped by the other is worse than a refusal, because
//     only the refusal says which.
//
// The refusals are named, never a fallback. Mounting the containing directory
// instead would expose paths the operator did not authorize, which is the same
// failure mode ValidateMountPathSupport exists to prevent.

// SupportsFileGrants reports whether an implementation on a platform can
// enforce a filesystem rule whose host path is a regular file.
func SupportsFileGrants(implementation Implementation, goos string) bool {
	return implementation == ImplementationTclaudeLayer && goos == "linux"
}

// IsFileGrantPath reports whether path currently names a regular file on this
// host. It is the one question that separates a file rule from a directory
// rule, and it is asked of the HOST path because that is the authority-bearing
// side of a grant — a remapped rule's guest path names a location in a
// namespace that does not exist yet.
//
// A path that is missing, unreadable, or anything other than a regular file
// answers false. Missing read/write rows are skipped at launch and missing deny
// rows fail closed, both decided by FilesystemForLaunch, so "not a file"
// is the answer that leaves those behaviors exactly as they were.
func IsFileGrantPath(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// IsFileGrant reports whether one rule names a regular file.
//
// The authoring-time commitment answers first: a rule stamped GrantKindFile is
// a file rule even if its path is momentarily absent, which is what keeps it out
// of the harness directory lists in every state. A rule with no commitment — one
// built from a bare path list, such as the launch contract's own file binds —
// falls back to asking the host.
func IsFileGrant(grant FilesystemGrant) bool {
	return grant.Kind == GrantKindFile || IsFileGrantPath(grant.Path)
}

// HasFileGrant reports whether any rule in a set names a regular file, so a
// capability gate can decide before a caller starts rendering.
func HasFileGrant(grants []FilesystemGrant) bool {
	for _, grant := range grants {
		if IsFileGrant(grant) {
			return true
		}
	}
	return false
}

// FileGrantPaths returns the host paths of the rules that name a regular file,
// in the order they appear. Disclosure surfaces use it to say WHICH rules a
// launch could not enforce instead of only how many.
func FileGrantPaths(grants []FilesystemGrant) []string {
	var out []string
	for _, grant := range grants {
		if IsFileGrant(grant) {
			out = append(out, grant.Path)
		}
	}
	return out
}

// DirectoryGrants returns the rules whose host path is not a regular file.
//
// It exists for the bare `[]string` directory lists a launch hands to a harness
// — `--add-dir` roots, Claude Code's sandbox filesystem arrays, Codex's
// writable roots. Those lists are directories by contract, and a file rule
// belongs to the mount plan instead. Dropping the file rule there is safe only
// because ValidateFileGrantSupport has already refused every implementation
// that would have needed the list to carry it.
func DirectoryGrants(grants []FilesystemGrant) []FilesystemGrant {
	out := make([]FilesystemGrant, 0, len(grants))
	for _, grant := range grants {
		if IsFileGrant(grant) {
			continue
		}
		out = append(out, grant)
	}
	return out
}

// ValidateFileGrantSupport refuses a launch whose rules need a capability the
// resolved implementation does not have. The message names the missing
// capability and the exact rule, following the wording pattern the other
// unsupported_sandbox_profile_* refusals use.
func ValidateFileGrantSupport(
	grants []FilesystemGrant,
	implementation Implementation,
	goos string,
) error {
	if SupportsFileGrants(implementation, goos) {
		return nil
	}
	for _, grant := range grants {
		if !IsFileGrant(grant) {
			continue
		}
		return fmt.Errorf(
			"unsupported_sandbox_profile_file_grant: sandbox implementation %q on %s cannot apply the %s rule for %q; that path is a regular file, and mounting a single file requires a mount namespace whose whole boundary tclaude owns, which only the Linux tclaude-layer provides",
			implementation, goos, grant.Access, grant.Path)
	}
	return nil
}
