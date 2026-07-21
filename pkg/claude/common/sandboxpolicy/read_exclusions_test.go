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
	linux, err := ReadExclusionCatalog(home, "linux")
	require.NoError(t, err)
	byID := map[string]ReadExclusionCategory{}
	for _, category := range linux {
		byID[category.ID] = category
	}
	assert.Contains(t, byID[ReadExclusionBrowserProfiles].Paths, filepath.Join(home, ".config", "google-chrome"))
	assert.Contains(t, byID[ReadExclusionToolchainCaches].Paths, filepath.Join(home, "go", "pkg", "mod"))
	assert.NotEmpty(t, byID[ReadExclusionToolchainCaches].Warning)

	mac, err := ReadExclusionCatalog(home, "darwin")
	require.NoError(t, err)
	for _, category := range mac {
		byID[category.ID] = category
	}
	assert.Contains(t, byID[ReadExclusionBrowserProfiles].Paths, filepath.Join(home, "Library", "Application Support", "Firefox", "Profiles"))
}

func TestReadExclusionCatalogCanonicalizesExistingSymlinkLeaves(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.Symlink(target, filepath.Join(home, ".ssh")))
	catalog, err := ReadExclusionCatalog(home, "linux")
	require.NoError(t, err)
	for _, category := range catalog {
		if category.ID == ReadExclusionSSH {
			assert.Equal(t, []string{target}, category.Paths)
			return
		}
	}
	t.Fatal("SSH category missing")
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

func TestReadExclusionLineageUnderstandsHomeSubsumption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	snapshot := func(ids []string) Snapshot {
		effective, err := Resolve(Scopes{Explicit: &Profile{Name: "p", ReadBaselineExclusions: ids}})
		require.NoError(t, err)
		return NewSnapshot(effective, nil)
	}
	assert.NoError(t, RequireContained(snapshot([]string{ReadExclusionSSH}), snapshot([]string{ReadExclusionHome})))
	assert.Error(t, RequireContained(snapshot([]string{ReadExclusionHome}), snapshot([]string{ReadExclusionSSH})))
}

func TestOmittedReadExclusionsPreserveProfileJSONShape(t *testing.T) {
	b, err := json.Marshal(Profile{Name: "legacy"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "read_baseline_exclusions")
}
