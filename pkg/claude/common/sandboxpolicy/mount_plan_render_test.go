package sandboxpolicy

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The renderer is pure, so these tests use synthetic absolute paths that need
// not exist. That is deliberate: it is the property that lets the mount plan be
// tested exhaustively for shapes (deep reopen-under-deny nests) that would be
// impractical to build on a real filesystem, and which today's harness
// renderers cannot express at all.

func grant(path string, access Access) FilesystemGrant {
	return FilesystemGrant{Path: path, Access: access}
}

func entry(path string, mode MountMode) MountEntry {
	return MountEntry{Path: path, Mode: mode}
}

func renderGrants(t *testing.T, grants []FilesystemGrant) MountPlan {
	t.Helper()
	plan, err := RenderMountPlanFromGrants(grants)
	if err != nil {
		t.Fatalf("RenderMountPlanFromGrants: unexpected error: %v", err)
	}
	return plan
}

func assertEntries(t *testing.T, got MountPlan, want []MountEntry) {
	t.Helper()
	if len(got.Entries) != len(want) {
		t.Fatalf("plan has %d entries, want %d\n%s", len(got.Entries), len(want), got)
	}
	for i := range want {
		if got.Entries[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v\n%s", i, got.Entries[i], want[i], got)
		}
	}
}

// assertAncestorsFirst proves the ordering invariant the whole guarantee rests
// on: no entry may be preceded by one of its own descendants, because an
// applier that let a descendant be shadowed by its ancestor would silently
// invert most-specific-wins.
//
// Containment is checked on the FOLDED key, not on the raw path, so the
// invariant is asserted the way a case-insensitive filesystem would see it.
// That is strictly stronger than a byte-exact check, since a byte-exact
// ancestor is also a folded one. Pairs whose folded keys are equal are excluded:
// two spellings of one macOS directory cannot be ordered ancestor-first at all,
// and the renderer deliberately orders such spellings without merging them.
func assertAncestorsFirst(t *testing.T, plan MountPlan) {
	t.Helper()
	for i, earlier := range plan.Entries {
		for j := i + 1; j < len(plan.Entries); j++ {
			later := plan.Entries[j]
			earlierKey, laterKey := mountOrderKey(earlier.Path), mountOrderKey(later.Path)
			if earlierKey != laterKey && pathContainsOrEqual(laterKey, earlierKey) {
				t.Fatalf("entry %d (%q) is a descendant of later entry %d (%q); plan is not ancestors-first\n%s",
					i, earlier.Path, j, later.Path, plan)
			}
		}
	}
}

func TestRenderMountPlanSemantics(t *testing.T) {
	tests := []struct {
		name   string
		grants []FilesystemGrant
		want   []MountEntry
	}{
		{
			name:   "empty policy renders an empty plan",
			grants: nil,
			want:   []MountEntry{},
		},
		{
			name:   "read renders read-only and write renders read-write",
			grants: []FilesystemGrant{grant("/srv/ro", AccessRead), grant("/srv/rw", AccessWrite)},
			want:   []MountEntry{entry("/srv/ro", MountRO), entry("/srv/rw", MountRW)},
		},
		{
			// The first shape today's harness renderers get wrong in one
			// direction: a writable tree with a hole punched in it.
			name: "deny child inside an allowed parent hides only the child",
			grants: []FilesystemGrant{
				grant("/home/dev", AccessWrite),
				grant("/home/dev/.ssh", AccessDeny),
			},
			want: []MountEntry{
				entry("/home/dev", MountRW),
				entry("/home/dev/.ssh", MountHide),
			},
		},
		{
			// The reopen-under-deny shape: Claude Code's permission layer cannot
			// represent it at all (strict deny-first) and Codex's Linux
			// enforcement drops the carve-out. Ordering makes it trivial here.
			name: "read reopen beneath a deny is rebound after the deny",
			grants: []FilesystemGrant{
				grant("/home/dev/work", AccessRead),
				grant("/home/dev", AccessDeny),
			},
			want: []MountEntry{
				entry("/home/dev", MountHide),
				entry("/home/dev/work", MountRO),
			},
		},
		{
			name: "write reopen beneath a deny is rebound after the deny",
			grants: []FilesystemGrant{
				grant("/home/dev", AccessDeny),
				grant("/home/dev/work", AccessWrite),
			},
			want: []MountEntry{
				entry("/home/dev", MountHide),
				entry("/home/dev/work", MountRW),
			},
		},
		{
			// The TCL-666 hole in concrete form: "deny the whole home directory,
			// reopen the workspace". The harness permission layer left the
			// built-in Read tool unconfined under exactly this shape; the mount
			// plan expresses it as two ordered entries with nothing left over.
			name: "deny home with a workspace reopen",
			grants: []FilesystemGrant{
				grant("/home/dev", AccessDeny),
				grant("/home/dev/git/project", AccessWrite),
				grant("/home/dev/.cache/go-build", AccessWrite),
			},
			want: []MountEntry{
				entry("/home/dev", MountHide),
				entry("/home/dev/.cache/go-build", MountRW),
				entry("/home/dev/git/project", MountRW),
			},
		},
		{
			name: "alternating allow and deny nests to arbitrary depth",
			grants: []FilesystemGrant{
				grant("/a", AccessRead),
				grant("/a/b", AccessDeny),
				grant("/a/b/c", AccessWrite),
				grant("/a/b/c/d", AccessDeny),
				grant("/a/b/c/d/e", AccessRead),
				grant("/a/b/c/d/e/f", AccessDeny),
			},
			want: []MountEntry{
				entry("/a", MountRO),
				entry("/a/b", MountHide),
				entry("/a/b/c", MountRW),
				entry("/a/b/c/d", MountHide),
				entry("/a/b/c/d/e", MountRO),
				entry("/a/b/c/d/e/f", MountHide),
			},
		},
		{
			// A sibling whose name sorts between an ancestor and its descendant
			// ('-' < '/') may interleave. That is harmless because shadowing only
			// happens between entries that contain one another, but the ordering
			// invariant must still hold for the real ancestor chain.
			name: "unrelated sibling names may interleave without breaking nesting",
			grants: []FilesystemGrant{
				grant("/a", AccessDeny),
				grant("/a-sibling", AccessWrite),
				grant("/a/b", AccessRead),
			},
			want: []MountEntry{
				entry("/a", MountHide),
				entry("/a-sibling", MountRW),
				entry("/a/b", MountRO),
			},
		},
		{
			name: "a redundant child at its inherited mode is still emitted",
			grants: []FilesystemGrant{
				grant("/srv", AccessRead),
				grant("/srv/data", AccessRead),
			},
			want: []MountEntry{
				entry("/srv", MountRO),
				entry("/srv/data", MountRO),
			},
		},
		{
			name: "duplicate paths fold with deny dominating write dominating read",
			grants: []FilesystemGrant{
				grant("/a", AccessRead),
				grant("/a", AccessWrite),
				grant("/b", AccessWrite),
				grant("/b", AccessDeny),
				grant("/b", AccessRead),
			},
			want: []MountEntry{
				entry("/a", MountRW),
				entry("/b", MountHide),
			},
		},
		{
			name:   "non-canonical spellings are cleaned before folding",
			grants: []FilesystemGrant{grant("/a/b/", AccessRead), grant("/a/./b", AccessWrite), grant("/a/c/../b", AccessRead)},
			want:   []MountEntry{entry("/a/b", MountRW)},
		},
		{
			name:   "the filesystem root is an ordinary ancestor",
			grants: []FilesystemGrant{grant("/etc", AccessRead), grant("/", AccessDeny)},
			want:   []MountEntry{entry("/", MountHide), entry("/etc", MountRO)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := renderGrants(t, tt.grants)
			assertEntries(t, plan, tt.want)
			assertAncestorsFirst(t, plan)
		})
	}
}

// TestRenderMountPlanIsDeterministic proves identical policies render
// byte-identically regardless of the order the grants arrived in. Resolve emits
// sorted grants today, but the plan is a launch input and a diffable audit
// artifact, so its stability must not depend on that.
func TestRenderMountPlanIsDeterministic(t *testing.T) {
	base := []FilesystemGrant{
		grant("/a", AccessDeny),
		grant("/a/b", AccessRead),
		grant("/a/b/c", AccessWrite),
		grant("/a/b/c/d", AccessDeny),
		grant("/z", AccessRead),
	}
	want := renderGrants(t, base).String()

	// Every rotation of the input, which covers each grant appearing first.
	for shift := 1; shift < len(base); shift++ {
		rotated := append(append([]FilesystemGrant{}, base[shift:]...), base[:shift]...)
		if got := renderGrants(t, rotated).String(); got != want {
			t.Fatalf("rotation by %d rendered differently:\ngot:\n%s\nwant:\n%s", shift, got, want)
		}
	}
	// Reversed, plus a duplicate that must fold rather than reorder anything.
	reversed := make([]FilesystemGrant, 0, len(base)+1)
	for _, g := range slices.Backward(base) {
		reversed = append(reversed, g)
	}
	reversed = append(reversed, grant("/a/b", AccessRead))
	if got := renderGrants(t, reversed).String(); got != want {
		t.Fatalf("reversed input rendered differently:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestEffectiveMountModeMatchesPolicySpecificity is the central guarantee:
// replaying the ordered plan (order only, no precedence logic) agrees with
// EffectiveAccessAt, the package's independent most-specific-rule-wins model,
// at every interesting path. If the ordering were wrong in either direction
// these would disagree.
func TestEffectiveMountModeMatchesPolicySpecificity(t *testing.T) {
	grants := []FilesystemGrant{
		grant("/home/dev", AccessDeny),
		grant("/home/dev/work", AccessWrite),
		grant("/home/dev/work/vendor", AccessRead),
		grant("/home/dev/work/vendor/secrets", AccessDeny),
		grant("/home/dev/work/vendor/secrets/public", AccessRead),
		grant("/home/dev-other", AccessRead),
		grant("/srv", AccessWrite),
	}
	plan := renderGrants(t, grants)
	assertAncestorsFirst(t, plan)

	probes := []string{
		"/home/dev",
		"/home/dev/.ssh",
		"/home/dev/work",
		"/home/dev/work/main.go",
		"/home/dev/work/vendor",
		"/home/dev/work/vendor/lib",
		"/home/dev/work/vendor/secrets",
		"/home/dev/work/vendor/secrets/key",
		"/home/dev/work/vendor/secrets/public",
		"/home/dev/work/vendor/secrets/public/readme",
		"/home/dev-other",
		"/home/dev-other/x",
		"/srv",
		"/srv/deep/deeper",
		"/opt",
		"/",
	}
	for _, probe := range probes {
		wantAccess, wantCovered := EffectiveAccessAt(grants, probe)
		gotMode, gotCovered := EffectiveMountModeAt(plan, probe)
		if gotCovered != wantCovered {
			t.Fatalf("%s: plan covered=%v, policy covered=%v", probe, gotCovered, wantCovered)
		}
		if !wantCovered {
			// Uncovered paths fall through to the applier's baseline, and the
			// returned mode must still be the fail-closed one.
			if gotMode != MountHide {
				t.Fatalf("%s: uncovered path reported mode %s, want hide", probe, gotMode)
			}
			continue
		}
		if want := mountModeForAccess(wantAccess); gotMode != want {
			t.Fatalf("%s: plan mode %s, policy access %s (want mode %s)", probe, gotMode, wantAccess, want)
		}
	}
}

// TestEffectiveMountModeAtDeepNesting checks the same agreement at a nesting
// depth no operator would hand-write, to be sure nothing about the ordering is
// only accidentally right for shallow shapes.
func TestEffectiveMountModeAtDeepNesting(t *testing.T) {
	const depth = 40
	grants := make([]FilesystemGrant, 0, depth)
	path := ""
	paths := make([]string, 0, depth)
	for i := range depth {
		path += fmt.Sprintf("/d%02d", i)
		paths = append(paths, path)
		access := AccessRead
		switch i % 3 {
		case 1:
			access = AccessDeny
		case 2:
			access = AccessWrite
		}
		grants = append(grants, grant(path, access))
	}
	// Render from the deepest-first order so sorting, not input order, is what
	// establishes the nesting.
	reversed := make([]FilesystemGrant, 0, len(grants))
	for _, g := range slices.Backward(grants) {
		reversed = append(reversed, g)
	}
	plan := renderGrants(t, reversed)
	assertAncestorsFirst(t, plan)

	for i, p := range paths {
		wantAccess, covered := EffectiveAccessAt(grants, p)
		if !covered {
			t.Fatalf("depth %d: policy does not cover %q", i, p)
		}
		gotMode, gotCovered := EffectiveMountModeAt(plan, p)
		if !gotCovered {
			t.Fatalf("depth %d: plan does not cover %q", i, p)
		}
		if want := mountModeForAccess(wantAccess); gotMode != want {
			t.Fatalf("depth %d (%q): plan mode %s, want %s", i, p, gotMode, want)
		}
		// A file directly inside each level inherits that level's mode.
		child := p + "/leaf"
		if gotMode, _ := EffectiveMountModeAt(plan, child); gotMode != mountModeForAccess(wantAccess) {
			t.Fatalf("depth %d (%q): child mode %s, want %s", i, child, gotMode, mountModeForAccess(wantAccess))
		}
	}
}

func TestEffectiveMountModeAtUncovered(t *testing.T) {
	plan := renderGrants(t, []FilesystemGrant{grant("/srv", AccessWrite)})
	for _, probe := range []string{"", "  ", "/opt", "/srv-other"} {
		mode, covered := EffectiveMountModeAt(plan, probe)
		if covered {
			t.Fatalf("%q: reported covered, want uncovered", probe)
		}
		if mode != MountHide {
			t.Fatalf("%q: mode %s, want the fail-closed hide", probe, mode)
		}
	}
}

func TestRenderMountPlanFromEffectiveProfile(t *testing.T) {
	effective := EffectiveProfile{
		Filesystem: []FilesystemGrant{
			grant("/home/dev", AccessDeny),
			grant("/home/dev/work", AccessWrite),
		},
		BreakGlassFilesystem: []BreakGlassGrant{
			{Path: "/home/dev/.tclaude/data/logs", Access: AccessRead},
		},
		// Non-path sections must not leak into the plan.
		Environment:      []EnvironmentEntry{{Name: "FOO", Value: "bar"}},
		AgentDirectories: []string{"AGENT_SCRATCH"},
		NetworkAccess:    NetworkAccessNone,
	}
	plan, err := RenderMountPlan(effective)
	if err != nil {
		t.Fatalf("RenderMountPlan: %v", err)
	}
	assertEntries(t, plan, []MountEntry{
		entry("/home/dev", MountHide),
		entry("/home/dev/.tclaude/data/logs", MountRO),
		entry("/home/dev/work", MountRW),
	})
	assertAncestorsFirst(t, plan)

	// The acknowledged protected path really is reachable through the plan, even
	// though a deny covers its whole ancestry.
	mode, covered := EffectiveMountModeAt(plan, "/home/dev/.tclaude/data/logs/agentd.log")
	if !covered || mode != MountRO {
		t.Fatalf("break-glass path: mode %s covered %v, want ro/true", mode, covered)
	}
	// And its protected siblings stay hidden.
	if mode, _ := EffectiveMountModeAt(plan, "/home/dev/.tclaude/data/db.sqlite"); mode != MountHide {
		t.Fatalf("protected sibling: mode %s, want hide", mode)
	}
}

// TestRenderMountPlanBreakGlassCollisionFailsClosed pins the one same-path
// collision rule: a deny and an acknowledged break-glass grant on the SAME
// canonical path fold to deny, like every other same-path collision in this
// package. An operator who wants the carve-out authors it strictly beneath the
// deny, which is the shape the model is built around.
func TestRenderMountPlanBreakGlassCollisionFailsClosed(t *testing.T) {
	plan, err := RenderMountPlan(EffectiveProfile{
		Filesystem:           []FilesystemGrant{grant("/home/dev/.tclaude/data", AccessDeny)},
		BreakGlassFilesystem: []BreakGlassGrant{{Path: "/home/dev/.tclaude/data", Access: AccessWrite}},
	})
	if err != nil {
		t.Fatalf("RenderMountPlan: %v", err)
	}
	assertEntries(t, plan, []MountEntry{entry("/home/dev/.tclaude/data", MountHide)})
}

// modeForAccess is the test's OWN access→mode table, written out literally.
// The tests must not route both sides of an assertion through the
// implementation's mountModeForAccess, or an inverted mapping would satisfy
// them.
var modeForAccess = map[Access]MountMode{
	AccessRead:  MountRO,
	AccessWrite: MountRW,
	AccessDeny:  MountHide,
}

// TestRenderMountPlanOrderingProperty drives the ordering invariant with
// generated input instead of hand-picked fixtures. Components come from an
// adversarial alphabet chosen around the path separator: characters that sort
// either side of '/' (0x2f), case pairs, a backslash, a space, and both the NFC
// and NFD spellings of the same accented letter. Hand-written cases cannot
// cover the combinations where a descendant would sort ahead of its ancestor.
//
// The seed is fixed so a failure reproduces exactly.
func TestRenderMountPlanOrderingProperty(t *testing.T) {
	components := []string{
		"a", "A", "b", "B", "z", "Z",
		"a-", "a.", "a0", "a_", "a~", "a b", `a\b`,
		"café", "café", "CAFÉ",
		"0", "9", "~", "-", "_",
	}
	accesses := []Access{AccessRead, AccessWrite, AccessDeny}
	rng := rand.New(rand.NewPCG(0x7c1, 0x751))

	for round := range 4000 {
		n := 1 + rng.IntN(6)
		grants := make([]FilesystemGrant, 0, n)
		var paths []string
		for range n {
			depth := 1 + rng.IntN(4)
			// Half the time extend an existing path, so ancestor/descendant
			// pairs actually occur instead of appearing only by chance.
			path := ""
			if len(paths) > 0 && rng.IntN(2) == 0 {
				path = paths[rng.IntN(len(paths))]
			}
			for range depth {
				path += "/" + components[rng.IntN(len(components))]
			}
			paths = append(paths, path)
			grants = append(grants, grant(path, accesses[rng.IntN(len(accesses))]))
		}

		plan, err := RenderMountPlanFromGrants(grants)
		if err != nil {
			t.Fatalf("round %d: unexpected error for %+v: %v", round, grants, err)
		}
		assertAncestorsFirst(t, plan)

		// Replaying the ordered plan must agree with the package's independent
		// most-specific-wins model at every path in play, plus synthesized
		// parents, children and near-misses.
		var probes []string
		for _, path := range paths {
			probes = append(probes, path, path+"/leaf", path+"x", filepathDir(path))
		}
		probes = append(probes, "/", "/unrelated")
		for _, probe := range probes {
			wantAccess, wantCovered := EffectiveAccessAt(grants, probe)
			gotMode, gotCovered := EffectiveMountModeAt(plan, probe)
			if gotCovered != wantCovered {
				t.Fatalf("round %d: %q: plan covered=%v, policy covered=%v\ngrants: %+v\n%s",
					round, probe, gotCovered, wantCovered, grants, plan)
			}
			if !wantCovered {
				if gotMode != MountHide {
					t.Fatalf("round %d: %q: uncovered path reported %s, want the fail-closed hide", round, probe, gotMode)
				}
				continue
			}
			if want := modeForAccess[wantAccess]; gotMode != want {
				t.Fatalf("round %d: %q: plan mode %s, policy access %s (want %s)\ngrants: %+v\n%s",
					round, probe, gotMode, wantAccess, want, grants, plan)
			}
		}
	}
}

func filepathDir(path string) string { return filepath.Dir(path) }

// TestRenderMountPlanOrdersCaseAndNormalizationVariants pins the macOS hazard.
// On a case-insensitive, normalization-insensitive filesystem the two spellings
// in each case below are ONE directory, so if the allow sorted after the deny
// the applier would rebind the very path the deny was meant to hide. Byte-wise
// sorting gets both of these wrong; the folded key gets them right, and on a
// case-sensitive filesystem the paths are unrelated so the order is free.
func TestRenderMountPlanOrdersCaseAndNormalizationVariants(t *testing.T) {
	tests := []struct {
		name   string
		grants []FilesystemGrant
		want   []MountEntry
	}{
		{
			// Byte-wise: "/Users/..." (0x55) sorts before "/users" (0x75), so
			// the deny would land first and be undone by the later read.
			name: "case-variant ancestor still sorts before its descendant",
			grants: []FilesystemGrant{
				grant("/Users/dev/.ssh", AccessDeny),
				grant("/users/dev", AccessRead),
			},
			want: []MountEntry{
				entry("/users/dev", MountRO),
				entry("/Users/dev/.ssh", MountHide),
			},
		},
		{
			// Byte-wise: NFD "café" has 'e' (0x65) where NFC "café"
			// has 0xc3, so the NFD descendant would sort before the NFC ancestor.
			name: "NFD descendant still sorts after its NFC ancestor",
			grants: []FilesystemGrant{
				grant("/Users/café/keys", AccessDeny),
				grant("/Users/café", AccessWrite),
			},
			want: []MountEntry{
				entry("/Users/café", MountRW),
				entry("/Users/café/keys", MountHide),
			},
		},
		{
			// Equal folded keys cannot be ordered ancestor-first; the renderer
			// orders them deterministically by raw path and does not merge them.
			// Documented behavior, pinned so a change is deliberate.
			name: "two spellings of one directory are ordered, not merged",
			grants: []FilesystemGrant{
				grant("/srv/Data", AccessDeny),
				grant("/srv/data", AccessWrite),
			},
			want: []MountEntry{
				entry("/srv/Data", MountHide),
				entry("/srv/data", MountRW),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := renderGrants(t, tt.grants)
			assertEntries(t, plan, tt.want)
			// Reversing the input must not change the result.
			reversed := make([]FilesystemGrant, 0, len(tt.grants))
			for _, g := range slices.Backward(tt.grants) {
				reversed = append(reversed, g)
			}
			assertEntries(t, renderGrants(t, reversed), tt.want)
		})
	}
}

// TestRenderMountPlanErrorsNameTheOperatorsSection checks that an error points
// at the profile field and index the operator actually wrote, rather than at an
// index into whatever slice the renderer built internally.
func TestRenderMountPlanErrorsNameTheOperatorsSection(t *testing.T) {
	_, err := RenderMountPlan(EffectiveProfile{
		Filesystem: []FilesystemGrant{
			grant("/a", AccessRead), grant("/b", AccessRead), grant("/c", AccessRead),
		},
		BreakGlassFilesystem: []BreakGlassGrant{{Path: "   ", Access: AccessRead}},
	})
	if err == nil {
		t.Fatalf("expected an error, got a plan")
	}
	if want := "break_glass_filesystem[0].path"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name %q; an operator with a one-row break-glass list has nothing to look at", err, want)
	}
}

// TestRenderMountPlanIsStricterThanGrantsFromDirs records which side is
// authoritative when the two disagree. GrantsFromDirs does not clean its rows,
// so a trailing-slash spelling reaches EffectiveAccessAt as a distinct, and by
// its length heuristic MORE specific, path than the deny on the same directory.
// The renderer cleans first, so the two rows fold and deny wins. The renderer
// is the stricter side; pinned so a later change to either function has to be
// deliberate.
func TestRenderMountPlanIsStricterThanGrantsFromDirs(t *testing.T) {
	grants := GrantsFromDirs(nil, []string{"/home/dev/work/"}, []string{"/home/dev/work"})
	if access, _ := EffectiveAccessAt(grants, "/home/dev/work"); access != AccessWrite {
		t.Fatalf("precondition changed: EffectiveAccessAt now reports %s for the uncleaned pair", access)
	}
	assertEntries(t, renderGrants(t, grants), []MountEntry{entry("/home/dev/work", MountHide)})
}

func TestRenderMountPlanRejectsMalformedGrants(t *testing.T) {
	tests := []struct {
		name    string
		grants  []FilesystemGrant
		wantErr string
	}{
		{
			name:    "unknown access",
			grants:  []FilesystemGrant{grant("/a", Access("append"))},
			wantErr: "is invalid",
		},
		{
			name:    "empty path",
			grants:  []FilesystemGrant{grant("   ", AccessDeny)},
			wantErr: "path is required",
		},
		{
			name:    "relative path",
			grants:  []FilesystemGrant{grant("relative/dir", AccessDeny)},
			wantErr: "is not absolute",
		},
		{
			name:    "home alias was never expanded",
			grants:  []FilesystemGrant{grant("~/work", AccessRead)},
			wantErr: "is not absolute",
		},
		{
			// A NUL would otherwise survive to the exec boundary and fail there
			// as something no operator can attribute to a rule.
			name:    "embedded NUL",
			grants:  []FilesystemGrant{grant("/home/dev/wo\x00rk", AccessRead)},
			wantErr: "without control characters",
		},
		{
			name:    "embedded newline",
			grants:  []FilesystemGrant{grant("/home/dev/wo\nrk", AccessDeny)},
			wantErr: "without control characters",
		},
		{
			name:    "invalid UTF-8",
			grants:  []FilesystemGrant{grant("/home/dev/\xff\xfe", AccessRead)},
			wantErr: "valid UTF-8",
		},
		{
			name:    "path beyond the profile layer's length bound",
			grants:  []FilesystemGrant{grant("/"+strings.Repeat("a", MaxPathBytes), AccessRead)},
			wantErr: "too long",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Rejecting rather than dropping matters most for deny rows: a
			// silently discarded deny fails OPEN.
			if _, err := RenderMountPlanFromGrants(tt.grants); err == nil {
				t.Fatalf("expected an error, got a plan")
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestRenderMountPlanRejectsDenyingBreakGlassGrant(t *testing.T) {
	_, err := RenderMountPlan(EffectiveProfile{
		BreakGlassFilesystem: []BreakGlassGrant{{Path: "/home/dev/.tclaude/data", Access: AccessDeny}},
	})
	if err == nil || !strings.Contains(err.Error(), "break_glass_filesystem[0].access") {
		t.Fatalf("error = %v, want a break-glass access rejection", err)
	}
}

func TestMountModeString(t *testing.T) {
	for mode, want := range map[MountMode]string{
		MountRO: "ro", MountRW: "rw", MountHide: "hide", MountMode(99): "mount-mode(99)",
	} {
		if got := mode.String(); got != want {
			t.Fatalf("MountMode(%d).String() = %q, want %q", int(mode), got, want)
		}
	}
}

// TestMountPlanSnapshot pins the diffable rendering. These strings are the
// groundwork for a dry-run/effective-plan surface, so a change to them is a
// change to an operator-facing contract and should be deliberate.
func TestMountPlanSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		grants []FilesystemGrant
		want   string
	}{
		{
			name:   "empty",
			grants: nil,
			want: strings.Join([]string{
				"mount-plan:",
				"  (empty)",
				"",
			}, "\n"),
		},
		{
			name: "reopen under deny",
			grants: []FilesystemGrant{
				grant("/home/dev/git/project", AccessWrite),
				grant("/home/dev", AccessDeny),
				grant("/home/dev/git/project/.git", AccessRead),
				grant("/usr/share", AccessRead),
			},
			want: strings.Join([]string{
				"mount-plan:",
				"  hide /home/dev",
				"  rw   /home/dev/git/project",
				"  ro   /home/dev/git/project/.git",
				"  ro   /usr/share",
				"",
			}, "\n"),
		},
		{
			name: "hole inside an allowed tree",
			grants: []FilesystemGrant{
				grant("/home/dev", AccessWrite),
				grant("/home/dev/.ssh", AccessDeny),
				grant("/home/dev/.aws", AccessDeny),
			},
			want: strings.Join([]string{
				"mount-plan:",
				"  rw   /home/dev",
				"  hide /home/dev/.aws",
				"  hide /home/dev/.ssh",
				"",
			}, "\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderGrants(t, tt.grants).String(); got != tt.want {
				t.Fatalf("plan snapshot mismatch:\ngot:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// TestRenderMountPlanFromLaunchDirs renders the grant set the launch contract
// actually assembles, through the existing GrantsFromDirs seam, rather than
// only profile-authored rows. This is the shape that made the reopen-under-deny
// capability gate necessary in the harness renderers.
func TestRenderMountPlanFromLaunchDirs(t *testing.T) {
	grants := GrantsFromDirs(
		[]string{"/usr/share/tclaude"},
		[]string{"/home/dev/git/project", "/home/dev/.cache/agent"},
		[]string{"/home/dev"},
	)
	if !HasReopenUnderDeny(grants) {
		t.Fatalf("expected the launch dir set to carry the reopen-under-deny shape")
	}
	plan := renderGrants(t, grants)
	assertAncestorsFirst(t, plan)
	assertEntries(t, plan, []MountEntry{
		entry("/home/dev", MountHide),
		entry("/home/dev/.cache/agent", MountRW),
		entry("/home/dev/git/project", MountRW),
		entry("/usr/share/tclaude", MountRO),
	})
}
