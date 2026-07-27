package sandboxpolicy

// The protected-root invariant is absolute: no operator-authored profile input
// — direct grant, include chain, spelling alias, missing path, or ancestor —
// can produce an effective policy that reads or writes tclaude/harness
// protected state. This file is the guard for that property.
//
// It was written when the break-glass feature was removed in TCL-791.
// Break-glass was the one sanctioned exception, and removing it made the
// invariant unconditional: there is no profile, include, launch contract,
// acknowledgement, or CLI flag that reopens a protected root. An operator who
// must work without the wall disables the sandbox instead.
//
// If a future change reintroduces any reopen path, this file must fail.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustJSON quotes a path for embedding in a raw snapshot fixture.
func mustJSON(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// protectedHome installs an isolated $HOME containing the protected roots plus
// an ordinary ~/.codex tree so every boundary test operates on temporary
// state. Production tclaude/harness state is never read or written.
func protectedHome(t *testing.T) (home, tclaudeData, claudeSessions, codexHome string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	tclaudeData = filepath.Join(home, ".tclaude", "data")
	claudeSessions = filepath.Join(home, ".claude", "sessions")
	codexHome = filepath.Join(home, ".codex")
	for _, path := range []string{tclaudeData, claudeSessions, codexHome} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}
	canonical, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	// Callers compare against canonical output, and macOS /var → /private/var
	// makes the temp home a symlink.
	return canonical,
		filepath.Join(canonical, ".tclaude", "data"),
		filepath.Join(canonical, ".claude", "sessions"),
		filepath.Join(canonical, ".codex")
}

// protectedSpellings enumerates the ways an author could try to name a
// protected root. Each must be refused for read and write alike.
func protectedSpellings(t *testing.T, home string) map[string]string {
	t.Helper()
	protected, err := ProtectedPaths()
	require.NoError(t, err)
	require.NotEmpty(t, protected)

	// A descendant that exists, and one that does not yet exist. The missing
	// one matters because NormalizeForPersistence deliberately tolerates
	// missing paths, and that tolerance must not become a hole.
	existingChild := filepath.Join(protected[0], "child")
	require.NoError(t, os.MkdirAll(existingChild, 0o755))
	missingChild := filepath.Join(protected[0], "not-created-yet")

	// A symlink OUTSIDE the protected tree pointing INTO it. Canonicalization
	// has to resolve it before the containment check, or the link is a bypass.
	alias := filepath.Join(home, "alias-into-protected")
	require.NoError(t, os.Symlink(protected[0], alias))

	spellings := map[string]string{
		"protected root, exact":     protected[0],
		"protected root, trailing/": protected[0] + string(filepath.Separator),
		"existing descendant":       existingChild,
		"missing descendant":        missingChild,
		"symlink into protected":    alias,
		"ancestor: home":            home,
		"ancestor: tilde":           "~",
		"ancestor: filesystem root": string(filepath.Separator),
	}
	for i, path := range protected {
		spellings["protected root "+string(rune('A'+i))] = path
	}
	return spellings
}

// TestProtectedRootsRejectEveryReadWriteSpelling is layer 1: the gate itself.
// normalizeFilesystem is the anchor — pathsIntersect is bidirectional, so an
// ancestor grant is refused exactly like a descendant one.
func TestProtectedRootsRejectEveryReadWriteSpelling(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	for name, path := range protectedSpellings(t, home) {
		for _, access := range []Access{AccessRead, AccessWrite} {
			t.Run(name+"/"+string(access), func(t *testing.T) {
				in := Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: path, Access: access}}}

				_, err := Normalize(in)
				require.Error(t, err, "Normalize must refuse %s access to %q (%s)", access, path, name)

				// The permissive variant must refuse it too. A missing path is
				// allowed to survive persistence, but never one that lands on a
				// protected root.
				_, _, err = NormalizeForPersistence(in)
				require.Error(t, err,
					"NormalizeForPersistence must refuse %s access to %q (%s); tolerating missing paths must not tolerate protected ones",
					access, path, name)
			})
		}
	}
}

// TestProtectedRootsStillAcceptDeny proves the invariant is "nothing reopens a
// protected root", not the weaker "protected paths are unusable". A deny never
// widens, so it stays authorable — and this is what keeps the test above from
// passing for the wrong reason.
func TestProtectedRootsStillAcceptDeny(t *testing.T) {
	protectedHome(t)
	protected, err := ProtectedPaths()
	require.NoError(t, err)
	for _, path := range protected {
		out, err := Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: path, Access: AccessDeny}}})
		require.NoError(t, err, "deny on protected path %q must remain authorable", path)
		require.Len(t, out.Filesystem, 1)
		assert.Equal(t, AccessDeny, out.Filesystem[0].Access)
	}
}

// TestCompositionCannotLaunderProtectedAccess is layer 2: includes and scope
// resolution must not become the laundering route that the direct gate closes.
func TestCompositionCannotLaunderProtectedAccess(t *testing.T) {
	home, tclaudeData, _, _ := protectedHome(t)
	dangerous := Profile{Name: "leaf", Filesystem: []FilesystemGrant{{Path: tclaudeData, Access: AccessWrite}}}
	innocent := Profile{Name: "wrapper", Includes: []string{"leaf"}}
	nested := Profile{Name: "outer", Includes: []string{"wrapper"}}
	registry := map[string]Profile{"leaf": dangerous, "wrapper": innocent, "outer": nested}
	lookup := func(name string) (*Profile, error) {
		p, ok := registry[name]
		if !ok {
			return nil, nil
		}
		return &p, nil
	}

	t.Run("direct flatten", func(t *testing.T) {
		_, err := Flatten(dangerous, lookup)
		require.Error(t, err)
	})
	t.Run("one include deep", func(t *testing.T) {
		_, err := Flatten(innocent, lookup)
		require.Error(t, err, "an innocent-looking wrapper must not be able to pull in protected access")
	})
	t.Run("nested include", func(t *testing.T) {
		_, err := Flatten(nested, lookup)
		require.Error(t, err, "nesting must not launder protected access either")
	})

	// Every resolution tier refuses it, so a global or group assignment cannot
	// smuggle in what an explicit one cannot.
	for _, tier := range []struct {
		name   string
		scopes Scopes
	}{
		{"global", Scopes{Global: &dangerous}},
		{"group", Scopes{Group: &dangerous}},
		{"explicit", Scopes{Explicit: &dangerous}},
	} {
		t.Run("resolve/"+tier.name, func(t *testing.T) {
			_, err := Resolve(tier.scopes)
			require.Error(t, err, "%s scope must not resolve protected access", tier.name)
		})
	}

	// And the ancestor form through composition, which is the subtler shape:
	// $HOME contains the protected roots without naming them.
	ancestor := Profile{Name: "leaf", Filesystem: []FilesystemGrant{{Path: home, Access: AccessWrite}}}
	registry["leaf"] = ancestor
	_, err := Flatten(innocent, lookup)
	require.Error(t, err, "an ancestor grant reaching protected roots must be refused through includes too")
}

// TestNoProfileFieldCanCarryProtectedAccess is layer 3. The gate tests above
// prove that no VALUE defeats the check; this proves no new FIELD sidesteps it,
// which is how break-glass reached protected state in the first place — it was
// a second authority section that skipped normalizeFilesystem entirely.
func TestNoProfileFieldCanCarryProtectedAccess(t *testing.T) {
	const why = "\n\nThis test guards the absolute protected-root invariant (TCL-791). If you are " +
		"adding a legitimate field, this failure is the moment to think, not a formality to silence: " +
		"consciously update the expected list below AND confirm the new field cannot carry read/write " +
		"access to a path at or beneath ProtectedPaths(). Any field that names host paths must route " +
		"through a validator that checks every ProtectedPaths entry. A field that " +
		"bypasses it reintroduces break-glass under a new name."

	for _, tc := range []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{
			name: "Profile",
			typ:  reflect.TypeOf(Profile{}),
			want: []string{"Name", "Filesystem", "Environment", "AgentDirectories", "NetworkAccess", "Network", "UnixSockets", "Includes"},
		},
		{
			name: "EffectiveProfile",
			typ:  reflect.TypeOf(EffectiveProfile{}),
			want: []string{"Filesystem", "MountAliases", "Environment", "AgentDirectories", "NetworkAccess", "Network", "UnixSockets", "AccessNotices", "Provenance"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for i := range tc.typ.NumField() {
				field := tc.typ.Field(i)
				if field.IsExported() {
					got = append(got, field.Name)
				}
			}
			assert.Equal(t, tc.want, got, "the exported field set of %s changed.%s", tc.name, why)
		})
	}
}

// TestRenderedMountPlanNeverTouchesAProtectedRoot is layer 4: nothing survives
// resolution into the launch contract. The mount plan is what the bwrap and
// seatbelt appliers replay, so an entry at or beneath a protected root here
// would be a real reopen on a real launch.
func TestRenderedMountPlanNeverTouchesAProtectedRoot(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	protected, err := ProtectedPaths()
	require.NoError(t, err)

	// The broadest policy an author can legally express: writable siblings of
	// the protected roots, a deny over home, and an alias-bearing spelling.
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	link := filepath.Join(home, "workspace-link")
	require.NoError(t, os.Symlink(workspace, link))

	effective, err := Resolve(Scopes{Explicit: &Profile{
		Name: "broad",
		Filesystem: []FilesystemGrant{
			{Path: home, Access: AccessDeny},
			{Path: workspace, Access: AccessWrite},
			{Path: link, Access: AccessWrite},
		},
		AgentDirectories: []string{"SCRATCH"},
		NetworkAccess:    NetworkAccessInternet,
	}})
	require.NoError(t, err)

	plan, err := RenderMountPlan(effective)
	require.NoError(t, err)
	require.NotEmpty(t, plan.Entries, "the fixture must actually render something, or this proves nothing")

	// Only REOPENS are forbidden. A hide is the applier's own class-3 behavior
	// expressed as a plan row and never widens anything, so a deny covering
	// $HOME — which necessarily contains the protected roots — is correct and
	// must not trip this assertion. Anything readable or writable, however, may
	// not sit at, beneath, or above a protected root: normalizeFilesystem
	// forbids both directions, so either would mean the gate was defeated.
	sawReopen := false
	for _, entry := range plan.Entries {
		if entry.Mode == MountHide {
			continue
		}
		sawReopen = true
		for _, root := range protected {
			assert.False(t, pathsIntersect(entry.Path, root),
				"mount plan entry %q (mode %v) intersects protected root %q; nothing may reopen protected tclaude/harness state",
				entry.Path, entry.Mode, root)
		}
	}
	require.True(t, sawReopen,
		"the fixture rendered no read/write entry at all, so the reopen assertion above proved nothing")
	for _, alias := range plan.Aliases {
		for _, root := range protected {
			assert.False(t, pathsIntersect(alias.Link, root),
				"mount alias link %q intersects protected root %q", alias.Link, root)
			assert.False(t, pathsIntersect(alias.Target, root),
				"mount alias target %q intersects protected root %q", alias.Target, root)
		}
	}
}
