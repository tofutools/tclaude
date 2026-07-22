package sandboxpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// denyLineageDirs materializes an isolated home with a workspace and a secret
// leaf, which is the canonical deny-plus-reopen shape TCL-623 replaced the
// read-baseline mechanism with.
func denyLineageDirs(t *testing.T) (home, workspace, secrets string) {
	t.Helper()
	home, _, _, _ = protectedHome(t)
	workspace = filepath.Join(home, "workspace")
	secrets = filepath.Join(home, "secrets")
	for _, dir := range []string{workspace, secrets} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	return home, workspace, secrets
}

func snapshotFor(t *testing.T, grants ...FilesystemGrant) Snapshot {
	t.Helper()
	effective, err := Resolve(Scopes{Explicit: &Profile{Name: "p", Filesystem: grants}})
	require.NoError(t, err)
	return NewSnapshot(effective, nil)
}

func TestReopensUnderDenyDetectsOnlyStrictDescendants(t *testing.T) {
	home, workspace, secrets := denyLineageDirs(t)

	shapes := ReopensUnderDeny([]FilesystemGrant{
		{Path: home, Access: AccessDeny},
		{Path: workspace, Access: AccessWrite},
	})
	require.Len(t, shapes, 1)
	assert.Equal(t, home, shapes[0].Deny)
	assert.Equal(t, workspace, shapes[0].Reopen.Path)
	assert.Equal(t, AccessWrite, shapes[0].Reopen.Access)

	// A deny with no reopen beneath it is an ordinary restriction.
	assert.False(t, HasReopenUnderDeny([]FilesystemGrant{{Path: home, Access: AccessDeny}}))
	// A grant ABOVE a deny is a broad grant the deny narrows, not a carve-out.
	assert.False(t, HasReopenUnderDeny([]FilesystemGrant{
		{Path: home, Access: AccessRead},
		{Path: secrets, Access: AccessDeny},
	}))
	// Sibling paths never form the shape, string-prefix similarity aside.
	assert.False(t, HasReopenUnderDeny([]FilesystemGrant{
		{Path: workspace, Access: AccessDeny},
		{Path: workspace + "-other", Access: AccessRead},
	}))
}

func TestEffectiveAccessAtPrefersTheMostSpecificRule(t *testing.T) {
	home, workspace, secrets := denyLineageDirs(t)
	grants := []FilesystemGrant{
		{Path: home, Access: AccessRead},
		{Path: secrets, Access: AccessDeny},
		{Path: workspace, Access: AccessWrite},
	}
	access, ok := EffectiveAccessAt(grants, filepath.Join(secrets, "key"))
	require.True(t, ok)
	assert.Equal(t, AccessDeny, access, "the nearer deny must beat the broader read")

	access, ok = EffectiveAccessAt(grants, filepath.Join(workspace, "src"))
	require.True(t, ok)
	assert.Equal(t, AccessWrite, access)

	_, ok = EffectiveAccessAt(grants, filepath.Dir(home))
	assert.False(t, ok, "an uncovered path leaves the decision to the harness baseline")
}

// The load-bearing lineage guarantee: a child may not carve authority out from
// beneath a deny its parent was running under.
func TestRequireContainedRefusesReopenBeneathAParentDeny(t *testing.T) {
	home, workspace, secrets := denyLineageDirs(t)

	parent := snapshotFor(t,
		FilesystemGrant{Path: home, Access: AccessDeny},
		FilesystemGrant{Path: workspace, Access: AccessWrite},
	)
	child := snapshotFor(t,
		FilesystemGrant{Path: home, Access: AccessDeny},
		FilesystemGrant{Path: workspace, Access: AccessWrite},
		FilesystemGrant{Path: secrets, Access: AccessRead},
	)

	err := RequireContained(parent, child)
	require.Error(t, err, "a child must not reopen a path beneath a deny the parent did not reopen")
	assert.Contains(t, err.Error(), "reopens a path the parent snapshot denies")

	require.NoError(t, RequireContained(parent, parent))
	// Dropping the reopen is a narrowing and always allowed.
	require.NoError(t, RequireContained(parent, snapshotFor(t, FilesystemGrant{Path: home, Access: AccessDeny})))
}

// Deny-preservation and grant-containment computed as two INDEPENDENT tests are
// not equivalent to checking the reopen relation. Here the child's new grant is
// covered by the parent's broad read AND every parent deny is still present,
// yet it introduces a reopen the parent never had. Specificity-aware
// containment is what catches it.
func TestRequireContainedRefusesReopenCoveredByABroaderParentRead(t *testing.T) {
	home, _, _ := denyLineageDirs(t)
	// The broad read sits on an ordinary subtree rather than $HOME itself: a
	// non-deny grant may never intersect tclaude's protected state.
	opt := filepath.Join(home, "opt")
	secrets := filepath.Join(opt, "secrets")
	leaf := filepath.Join(secrets, "x")
	require.NoError(t, os.MkdirAll(leaf, 0o755))

	parent := snapshotFor(t,
		FilesystemGrant{Path: opt, Access: AccessRead},
		FilesystemGrant{Path: secrets, Access: AccessDeny},
	)
	child := snapshotFor(t,
		FilesystemGrant{Path: opt, Access: AccessRead},
		FilesystemGrant{Path: secrets, Access: AccessDeny},
		FilesystemGrant{Path: leaf, Access: AccessRead},
	)

	err := RequireContained(parent, child)
	require.Error(t, err, "the child's grant is covered by the parent's broad read and preserves every deny, but still reopens beneath one")
	assert.Contains(t, err.Error(), "reopens a path the parent snapshot denies")

	// A parent that ALREADY holds the reopen may of course pass it on.
	reopened := snapshotFor(t,
		FilesystemGrant{Path: opt, Access: AccessRead},
		FilesystemGrant{Path: secrets, Access: AccessDeny},
		FilesystemGrant{Path: leaf, Access: AccessRead},
	)
	require.NoError(t, RequireContained(reopened, child))
}

func TestRequireContainedStillRefusesDroppingAParentDeny(t *testing.T) {
	home, workspace, _ := denyLineageDirs(t)
	parent := snapshotFor(t,
		FilesystemGrant{Path: home, Access: AccessDeny},
		FilesystemGrant{Path: workspace, Access: AccessWrite},
	)
	child := snapshotFor(t, FilesystemGrant{Path: workspace, Access: AccessWrite})

	err := RequireContained(parent, child)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not preserved")
}

func TestRequireContainedAllowsTighteningToABroaderDeny(t *testing.T) {
	home, workspace, _ := denyLineageDirs(t)
	parent := snapshotFor(t,
		FilesystemGrant{Path: workspace, Access: AccessWrite},
	)
	child := snapshotFor(t,
		FilesystemGrant{Path: home, Access: AccessDeny},
		FilesystemGrant{Path: workspace, Access: AccessWrite},
	)
	// The child adds a deny AND keeps a reopen the parent already granted
	// outright, so it holds strictly less authority.
	require.NoError(t, RequireContained(parent, child))
}

// A v4 snapshot persisted by the previous binary still upgrades; its removed
// read_baseline/read_baseline_exclusions fields are dropped rather than
// reinterpreted, which is the deliberate TCL-623 decision.
func TestLegacyStrictBaselineSnapshotDropsRemovedFields(t *testing.T) {
	home, workspace, _ := denyLineageDirs(t)
	_ = home
	raw := `{"version":4,"effective":{` +
		`"filesystem":[{"path":` + mustJSON(t, workspace) + `,"access":"write"}],` +
		`"read_baseline":"minimal","read_baseline_exclusions":["home.directory"],` +
		`"environment":[],"agent_directories":[],` +
		`"provenance":{"applied":[],"filesystem":{},"environment":{},"agent_directories":{}}},"applied":[]}`
	var snapshot Snapshot
	require.NoError(t, json.Unmarshal([]byte(raw), &snapshot))

	upgraded, err := NormalizeSnapshotVersion(snapshot)
	require.NoError(t, err)
	assert.Equal(t, SnapshotVersion, upgraded.Version)

	revalidated, err := RevalidateSnapshot(snapshot)
	require.NoError(t, err, "a legacy strict-baseline snapshot must load, not error")
	assert.Len(t, revalidated.Effective.Filesystem, 1)

	encoded, err := json.Marshal(revalidated)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "read_baseline")
}

// The launch contract adds reopens beneath a deny that the authored profile
// never contained, so shape detection must run over the RENDERED rules. A
// profile of exactly `deny ~` — what the "Deny access to the Home directory"
// common rule inserts — renders as a split policy once the workspace is
// reopened, and must be gated as one.
func TestGrantsFromDirsExposesLaunchContractReopenShape(t *testing.T) {
	home, workspace, _ := denyLineageDirs(t)

	authored := []FilesystemGrant{{Path: home, Access: AccessDeny}}
	require.False(t, HasReopenUnderDeny(authored), "the authored profile alone has no reopen")

	rendered := GrantsFromDirs(nil, []string{workspace}, []string{home})
	assert.Equal(t, []FilesystemGrant{
		{Path: home, Access: AccessDeny},
		{Path: workspace, Access: AccessWrite},
	}, rendered)
	assert.True(t, HasReopenUnderDeny(rendered),
		"once tclaude reopens the workspace the rendered rules ARE a reopen-under-deny")
}

func TestGrantsFromDirsFoldsDuplicatesDenyDominating(t *testing.T) {
	home, workspace, _ := denyLineageDirs(t)
	// The same path arriving as both a read pairing and a write grant folds to
	// write; a deny on the same path dominates both.
	got := GrantsFromDirs([]string{workspace, home}, []string{workspace}, []string{home})
	assert.Equal(t, []FilesystemGrant{
		{Path: home, Access: AccessDeny},
		{Path: workspace, Access: AccessWrite},
	}, got)
	assert.Empty(t, GrantsFromDirs([]string{"", "  "}, nil, nil), "blank entries are skipped")
}
