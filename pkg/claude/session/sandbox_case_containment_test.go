package session

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Case-insensitive containment in the ENFORCEMENT layers (TCL-985), the
// follow-up to TCL-981's work on the validator.
//
// Every test here is a pure-function assertion about a renderer: the Seatbelt
// emitter's identity lookup is an injected function, and the bwrap argv builders
// take their paths as arguments. So these run and assert the same outcome on
// every platform and every volume, and none of them carries a semantic t.Skip.
// The volume-adaptive filesystem evidence lives with the validator, in
// sandboxpolicy's guard_path_test.go.
//
// The property under test is a BIAS, not a happy path: at every site where a
// true answer emits a deny, an unresolvable case/NFC-folded nomination must
// answer true. TCL-981 left the Seatbelt emitter answering false there, which is
// the opposite of what the validator does with the same ambiguity.

// TestSeatbeltFoldedPathIsTheValidatorNominationKey replaces the byte-for-byte
// formula pin that used to live in sandboxpolicy. The emitter no longer restates
// the folding rule, so the two layers cannot drift by construction; this asserts
// the construction rather than the formula.
func TestSeatbeltFoldedPathIsTheValidatorNominationKey(t *testing.T) {
	for _, path := range []string{
		"/Users/Dev/.TCLAUDE/data",
		"/users/./dev/",
		"/Users/ΟΔΟΣ",
		"/Users/İstanbul",
		"/Users/STRAẞE",
	} {
		assert.Equal(t, sandboxpolicy.FoldGuardPath(path), seatbeltFoldedPath(path),
			"the emitter must nominate exactly the spellings the validator does")
	}
}

// TestSeatbeltGuardContainmentFailsClosedOnUnresolvedIdentity is the core of
// TCL-985. The three identity outcomes must produce three different answers from
// the guard-biased rule, and the allow-biased rule must keep answering false for
// the unresolved one — a single rule for both would be the bug.
func TestSeatbeltGuardContainmentFailsClosedOnUnresolvedIdentity(t *testing.T) {
	same := func(path string) (seatbeltFileIdentity, bool) {
		switch path {
		case "/Users/Case", "/users/case":
			return seatbeltFileIdentity{dev: 7, ino: 9}, true
		default:
			return seatbeltFileIdentity{}, false
		}
	}
	distinct := func(path string) (seatbeltFileIdentity, bool) {
		if path == "/Users/Case" {
			return seatbeltFileIdentity{dev: 7, ino: 9}, true
		}
		return seatbeltFileIdentity{dev: 7, ino: 10}, true
	}
	unknown := func(string) (seatbeltFileIdentity, bool) {
		return seatbeltFileIdentity{}, false
	}

	// One folding volume: both rules agree, because identity settled it.
	assert.True(t, seatbeltGuardPathContains("/Users/Case", "/users/case/child", same))
	assert.True(t, seatbeltPathContains("/Users/Case", "/users/case/child", same))

	// Case-sensitive volume: identity REFUTES the nomination, so even the guard
	// answers false. This is what keeps the bias from degenerating into "always
	// re-hide every protected root".
	assert.False(t, seatbeltGuardPathContains("/Users/Case", "/users/case/child", distinct))
	assert.False(t, seatbeltPathContains("/Users/Case", "/users/case/child", distinct))

	// Unresolvable — a path that does not exist yet, an unreadable ancestor. The
	// two rules must part company here and nowhere else.
	for _, identity := range []seatbeltIdentityLookup{nil, unknown} {
		assert.True(t, seatbeltGuardPathContains("/Users/Case", "/users/case/child", identity),
			"an unsettled nomination must refuse at a deny-emitting site")
		assert.False(t, seatbeltPathContains("/Users/Case", "/users/case/child", identity),
			"an unsettled nomination must not reopen at an allow-emitting site")
	}

	// The zero-I/O fast paths are shared, so an unrelated pair never consults
	// identity at all and the bias cannot leak into ordinary comparisons.
	panics := func(string) (seatbeltFileIdentity, bool) {
		t.Fatal("identity consulted for a pair with no folded relation")
		return seatbeltFileIdentity{}, false
	}
	assert.True(t, seatbeltGuardPathContains("/Users/dev", "/Users/dev/child", panics))
	assert.False(t, seatbeltGuardPathContains("/Users/alpha", "/Users/beta/child", panics))
}

// TestSeatbeltProtectedRehideFailsClosedOnUnresolvedIdentity checks the call
// site rather than the predicate: an emitted hide region, not a boolean.
func TestSeatbeltProtectedRehideFailsClosedOnUnresolvedIdentity(t *testing.T) {
	protected := []string{"/Users/dev/.tclaude/data"}

	hasHide := func(regions []seatbeltRegion) bool {
		for _, region := range regions {
			if region.path == protected[0] && region.mode == sandboxpolicy.MountHide {
				return true
			}
		}
		return false
	}

	// A mounted path spelled as a case variant of the protected root's parent.
	// Nothing resolves it, so the protected root must be re-hidden anyway.
	assert.True(t, hasHide(appendSeatbeltProtectedRehides(
		nil, "/Users/Dev", protected, nil)),
		"an unresolved folded nomination must still re-hide the protected root")

	// Identity refutes it: two directories, no re-hide. Proving this branch is
	// what makes the assertion above evidence of a BIAS rather than of a
	// predicate that returns true unconditionally.
	assert.False(t, hasHide(appendSeatbeltProtectedRehides(
		nil, "/Users/Dev", protected,
		func(path string) (seatbeltFileIdentity, bool) {
			if path == "/Users/Dev" {
				return seatbeltFileIdentity{dev: 1, ino: 1}, true
			}
			return seatbeltFileIdentity{dev: 1, ino: 2}, true
		})),
		"a case-sensitive volume keeps the spellings apart and needs no re-hide")

	// Identity confirms it: one directory, re-hide.
	assert.True(t, hasHide(appendSeatbeltProtectedRehides(
		nil, "/Users/Dev", protected,
		func(string) (seatbeltFileIdentity, bool) {
			return seatbeltFileIdentity{dev: 1, ino: 1}, true
		})))

	// An unrelated mount is untouched by the bias.
	assert.False(t, hasHide(appendSeatbeltProtectedRehides(
		nil, "/Users/other", protected, nil)))
}

// TestSeatbeltProfileStillDeniesProtectedRootsUnderAFoldedMountSpelling is a
// sanity check on the whole renderer, NOT evidence about the bias. The bias is
// unobservable in the emitted profile today — see appendSeatbeltProtectedRehides
// on why — so this asserts only that widening the containment rule did not cost
// the profile a protected-root deny it previously had.
func TestSeatbeltProfileStillDeniesProtectedRootsUnderAFoldedMountSpelling(t *testing.T) {
	protectedRoot := "/Users/dev/.tclaude/data"
	profile, params, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{
				{Path: "/Users/Dev", Mode: sandboxpolicy.MountRW},
			},
		},
		netip.AddrPort{},
		[]string{protectedRoot},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)

	// Paths reach the profile as parameters, never interpolated into rule text.
	readDeny := ""
	for _, param := range params {
		if param.path == protectedRoot && strings.HasPrefix(param.name, "READ_DENY_") {
			readDeny = param.name
		}
	}
	require.NotEmpty(t, readDeny, "the protected root must still be read-denied")
	assert.Contains(t, profile, `(param "`+readDeny+`")`)
}

// TestBwrapProtectedRehideFailsClosedOnUnresolvedIdentity is the Linux twin of
// the Seatbelt call-site test. GuardContainsOrEqual consults the real filesystem,
// so these spellings are deliberately paths that do not exist on any test host:
// unresolvable is the state under test.
func TestBwrapProtectedRehideFailsClosedOnUnresolvedIdentity(t *testing.T) {
	protected := []string{"/nonexistent-tcl985/dev/.tclaude/data"}

	args := appendTclaudeLayerProtectedRehides(
		nil, "/nonexistent-tcl985/Dev", protected, &tclaudeLayerHideRemounts{})
	assert.Equal(t, []string{"--tmpfs", protected[0]}, args,
		"an unresolved folded nomination must still re-hide the protected root")

	assert.Empty(t, appendTclaudeLayerProtectedRehides(
		nil, "/nonexistent-tcl985/other", protected, &tclaudeLayerHideRemounts{}),
		"an unrelated mount must stay a free lexical no")

	// Real, distinct directories on the test host's volume. On a case-sensitive
	// volume these are two inodes and the guard refutes the nomination; on a
	// folding volume MkdirAll returns the one directory both spellings name and
	// the guard confirms it. Either way the answer follows filesystem identity
	// rather than the bias, which is the property that keeps the bias from
	// collapsing into an unconditional true.
	root := t.TempDir()
	lower := filepath.Join(root, "state", ".tclaude", "data")
	require.NoError(t, os.MkdirAll(lower, 0o755))
	upper := filepath.Join(root, "State")
	require.NoError(t, os.MkdirAll(upper, 0o755))

	rehidden := appendTclaudeLayerProtectedRehides(
		nil, upper, []string{lower}, &tclaudeLayerHideRemounts{})
	if sandboxpolicy.GuardContainsOrEqual(upper, filepath.Join(root, "state")) {
		assert.Equal(t, []string{"--tmpfs", lower}, rehidden,
			"a folding volume makes these one directory, so the re-hide is required")
	} else {
		assert.Empty(t, rehidden,
			"a case-sensitive volume keeps them apart, so no re-hide is emitted")
	}
}

// TestBwrapRefusalGuardsRefuseFoldedVariantSpellings covers the validation-side
// conversions in the Linux enforcement layer: a grant that reaches a protected
// or harness-state root only through a case variant must be refused rather than
// rendered.
func TestBwrapRefusalGuardsRefuseFoldedVariantSpellings(t *testing.T) {
	err := validateTclaudeLayerHarnessStateRules(
		[]tclaudeLayerHarnessStateRule{{
			Path: "/nonexistent-tcl985/state/claude", Access: sandboxpolicy.AccessWrite,
		}},
		[]sandboxpolicy.FilesystemGrant{
			{Path: "/nonexistent-tcl985/State/Claude/sessions", Access: sandboxpolicy.AccessRead},
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "harness state root")

	require.NoError(t, validateTclaudeLayerHarnessStateRules(
		[]tclaudeLayerHarnessStateRule{{
			Path: "/nonexistent-tcl985/state/claude", Access: sandboxpolicy.AccessWrite,
		}},
		[]sandboxpolicy.FilesystemGrant{
			{Path: "/nonexistent-tcl985/elsewhere", Access: sandboxpolicy.AccessWrite},
		},
	), "an unrelated grant must still be admitted")

	err = validateRemappedGuestPathsAgainstContract(
		[]sandboxpolicy.FilesystemGrant{
			{Path: "/nonexistent-tcl985/src", MountPath: "/nonexistent-tcl985/Work"},
		},
		[]string{"/nonexistent-tcl985/work/repo"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported_sandbox_profile_mount_path")
}

// TestSeatbeltRuntimeCarveoutGuardFailsClosed covers the file's second refusal
// guard. A policy region that reaches a baseline runtime carveout only through a
// folded spelling must still get the strict, carveout-free deny that makes
// operator authority outrank runtime compatibility.
func TestSeatbeltRuntimeCarveoutGuardFailsClosed(t *testing.T) {
	runtimeTempDir := "/private/var/folders/ab/runtime/T"
	variant := "/private/var/folders/AB/runtime/T/child"

	assert.True(t, seatbeltRuntimeCarveoutIntersects(variant, runtimeTempDir, nil),
		"an unsettled nomination must still produce the strict deny")
	assert.False(t, seatbeltRuntimeCarveoutIntersects(
		variant, runtimeTempDir,
		func(path string) (seatbeltFileIdentity, bool) {
			if strings.Contains(path, "AB") {
				return seatbeltFileIdentity{dev: 1, ino: 1}, true
			}
			return seatbeltFileIdentity{dev: 1, ino: 2}, true
		}),
		"identity refutes the nomination on a case-sensitive volume")
	assert.False(t, seatbeltRuntimeCarveoutIntersects(
		"/Users/dev/workspace", runtimeTempDir, nil),
		"an unrelated region must stay a free lexical no")
}
