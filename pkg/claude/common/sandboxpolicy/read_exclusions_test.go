package sandboxpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeReadBaselineExclusionsPreservesUnknownAndCanonicalizes(t *testing.T) {
	got, err := NormalizeReadBaselineExclusions([]string{"future.credentials", ReadExclusionSSH, ReadExclusionSSH})
	require.NoError(t, err)
	assert.Equal(t, []string{"future.credentials", ReadExclusionSSH}, got)
	_, err = NormalizeReadBaselineExclusions([]string{"Not/semantic"})
	assert.Error(t, err)
}

func TestReadExclusionCatalogResolvesAuditedPlatformPaths(t *testing.T) {
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	linux, err := ReadExclusionCatalog(home, "linux")
	require.NoError(t, err)
	byID := map[string]ReadExclusionCategory{}
	for _, category := range linux {
		byID[category.ID] = category
	}
	assert.Contains(t, byID[ReadExclusionBrowserProfiles].Paths, filepath.Join(canonicalHome, ".config", "google-chrome"))
	assert.Contains(t, byID[ReadExclusionToolchainCaches].Paths, filepath.Join(canonicalHome, "go", "pkg", "mod"))
	assert.NotEmpty(t, byID[ReadExclusionToolchainCaches].Warning)

	mac, err := ReadExclusionCatalog(home, "darwin")
	require.NoError(t, err)
	for _, category := range mac {
		byID[category.ID] = category
	}
	assert.Contains(t, byID[ReadExclusionBrowserProfiles].Paths, filepath.Join(canonicalHome, "Library", "Application Support", "Firefox", "Profiles"))
}

func TestReadExclusionCatalogCanonicalizesExistingSymlinkLeaves(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()
	canonicalTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, filepath.Join(home, ".ssh")))
	catalog, err := ReadExclusionCatalog(home, "linux")
	require.NoError(t, err)
	for _, category := range catalog {
		if category.ID == ReadExclusionSSH {
			assert.Equal(t, []string{canonicalTarget}, category.Paths)
			return
		}
	}
	t.Fatal("SSH category missing")
}

func TestReadExclusionCatalogCanonicalizesMissingLeafThroughSymlinkedAncestor(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()
	canonicalTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, filepath.Join(home, ".config")))

	catalog, err := ReadExclusionCatalog(home, "linux")
	require.NoError(t, err)
	for _, category := range catalog {
		if category.ID == ReadExclusionVCSTokens {
			assert.Contains(t, category.Paths, filepath.Join(canonicalTarget, "gh"))
			return
		}
	}
	t.Fatal("VCS token category missing")
}

func TestReadExclusionsUnionAcrossIncludesAndScopesWithProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registry := map[string]Profile{
		"leaf": {Name: "leaf", ReadBaselineExclusions: []string{ReadExclusionSSH}},
	}
	wrapper, err := Flatten(Profile{Name: "wrapper", Includes: []string{"leaf"}, ReadBaselineExclusions: []string{ReadExclusionHome}}, func(name string) (*Profile, error) {
		value := registry[name]
		return &value, nil
	})
	require.NoError(t, err)
	effective, err := Resolve(Scopes{Group: &wrapper, Explicit: &Profile{Name: "extra", ReadBaselineExclusions: []string{ReadExclusionCloud}}})
	require.NoError(t, err)
	assert.Equal(t, []string{ReadExclusionHome, ReadExclusionCloud, ReadExclusionSSH}, effective.ReadBaselineExclusions)
	sshSources := effective.Provenance.ReadBaselineExclusions[ReadExclusionSSH]
	require.Len(t, sshSources, 1)
	assert.Equal(t, "leaf", sshSources[0].Profile)
	assert.Equal(t, "wrapper", sshSources[0].IncludedBy)
	assert.Equal(t, []string{"leaf", "wrapper"}, sshSources[0].Chain)
}

func TestTierAReadExclusionRejectsIntersectingGrantButHomeManagesReopens(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ssh := filepath.Join(home, ".ssh")
	require.NoError(t, os.MkdirAll(ssh, 0o700))
	_, err := Resolve(Scopes{Explicit: &Profile{Name: "conflict", ReadBaselineExclusions: []string{ReadExclusionSSH}, Filesystem: []FilesystemGrant{{Path: ssh, Access: AccessRead}}}})
	assert.ErrorContains(t, err, "conflicts with read_baseline_exclusions")
	workspace := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	_, err = Resolve(Scopes{Explicit: &Profile{Name: "home", ReadBaselineExclusions: []string{ReadExclusionHome}, Filesystem: []FilesystemGrant{{Path: workspace, Access: AccessWrite}}}})
	require.NoError(t, err)
}

func TestReadExclusionLineageRequiresExactLeafIntentEvenWhenHomeIsPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	external := t.TempDir()
	require.NoError(t, os.Symlink(external, filepath.Join(home, ".ssh")))
	snapshot := func(ids []string) Snapshot {
		effective, err := Resolve(Scopes{Explicit: &Profile{Name: "p", ReadBaselineExclusions: ids}})
		require.NoError(t, err)
		return NewSnapshot(effective, nil)
	}
	assert.Error(t, RequireContained(snapshot([]string{ReadExclusionSSH}), snapshot([]string{ReadExclusionHome})),
		"Home does not cover an SSH category whose audited path resolves outside Home")
	assert.NoError(t, RequireContained(snapshot([]string{ReadExclusionSSH}), snapshot([]string{ReadExclusionHome, ReadExclusionSSH})))
	assert.Error(t, RequireContained(snapshot([]string{ReadExclusionHome}), snapshot([]string{ReadExclusionSSH})))
}

func TestSymlinkedAncestorMissingLeafConflictsBeforeAndAfterMaterialization(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := t.TempDir()
	canonicalTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, filepath.Join(home, ".config")))
	gh := filepath.Join(canonicalTarget, "gh")

	profile := &Profile{Name: "conflict", ReadBaselineExclusions: []string{ReadExclusionVCSTokens}, Filesystem: []FilesystemGrant{{Path: gh, Access: AccessRead}}}
	_, err = Resolve(Scopes{Explicit: profile})
	require.ErrorContains(t, err, "conflicts with read_baseline_exclusions")

	// Simulate a snapshot persisted by an older binary that missed the
	// symlinked-ancestor conflict. Authority-use revalidation must reject it
	// both while the audited leaf is absent and after it materializes.
	legacy := NewSnapshot(EffectiveProfile{
		Filesystem:             []FilesystemGrant{{Path: gh, Access: AccessRead}},
		ReadBaselineExclusions: []string{ReadExclusionVCSTokens},
	}, nil)
	_, err = RevalidateSnapshot(legacy)
	require.ErrorContains(t, err, "conflicts with read_baseline_exclusions")
	require.NoError(t, os.Mkdir(gh, 0o700))
	_, err = RevalidateSnapshot(legacy)
	require.ErrorContains(t, err, "conflicts with read_baseline_exclusions")
}

func TestOmittedReadExclusionsPreserveProfileJSONShape(t *testing.T) {
	b, err := json.Marshal(Profile{Name: "legacy"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "read_baseline_exclusions")
}
