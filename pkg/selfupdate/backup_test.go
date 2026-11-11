package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// customFleet is a custom-method service over the three tclaude binaries
// in one bin dir, each at v1.0.0; the custom command installs *install
// (v1.1.0 unless changed) into all of them.
func customFleet(t *testing.T) (*Service, []Binary, *string) {
	t.Helper()
	dir := testutil.CanonicalTempDir(t)
	bins := []Binary{}
	path := map[string]string{}
	for _, name := range []string{"tclaude", "tclaude-agentd", "tclaude-hub"} {
		p := filepath.Join(dir, "bin", name)
		writeBin(t, p, "v1.0.0")
		bins = append(bins, Binary{Name: name, Path: p, Version: "v1.0.0", Method: "unrecognized"})
		path[name] = p
	}
	install := "v1.1.0"
	h := &fakeHost{path: path, run: func([]string, []string) ([]byte, error) {
		for _, b := range bins {
			writeBin(t, b.Path, install)
		}
		return nil, nil
	}}
	h.install(t)
	return versionedService(t, dir, bins, "v1.1.0"), bins, &install
}

func requireContents(t *testing.T, bins []Binary, want string) {
	t.Helper()
	for _, b := range bins {
		raw, err := os.ReadFile(b.Path)
		require.NoError(t, err)
		require.Equal(t, want, string(raw), b.Name)
	}
}

func backupDirs(t *testing.T, s *Service) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(s.dir, "backup-*"))
	require.NoError(t, err)
	return dirs
}

// Every binary a custom command replaces gets its own backup copy, and a
// rollback restores all of them.
func TestCustomMethodBacksUpEveryBinary(t *testing.T) {
	s, bins, _ := customFleet(t)
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "restarting", job.State, job.Error)
	requireContents(t, bins, "v1.1.0")
	var manifest backupManifest
	require.NoError(t, readJSON(filepath.Join(s.dir, "backup.json"), &manifest))
	require.Len(t, manifest.Entries, 3)
	seen := map[string]bool{}
	for _, e := range manifest.Entries {
		require.False(t, seen[e.Backup], "backup copies must not share a path")
		seen[e.Backup] = true
	}

	recovered := versionedService(t, filepath.Dir(s.dir), bins, "v1.1.0")
	rb, err := recovered.Start(Request{Action: "rollback"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Equal(t, "restarting", waitJob(t, recovered, rb.ID).State)
	requireContents(t, bins, "v1.0.0")
}

// An operator may remove the whole updates dir while agentd runs; the next
// apply recreates it instead of failing.
func TestApplyRecreatesARemovedUpdatesDir(t *testing.T) {
	s, bins, _ := customFleet(t)
	require.NoError(t, os.RemoveAll(s.dir))
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "restarting", job.State, job.Error)
	requireContents(t, bins, "v1.1.0")
	require.FileExists(t, filepath.Join(s.dir, "jobs", job.ID+".json"))
	require.FileExists(t, filepath.Join(s.dir, "backup.json"))
}

// Leftovers of earlier runs never fail a new one, and a later apply drops
// the backups the new backup.json no longer references.
func TestApplyPrunesStaleBackupsAndKeepsRollback(t *testing.T) {
	s, bins, install := customFleet(t)
	for _, name := range []string{"backup-deadbeef", "stage-123", "rollback-456"} {
		writeBin(t, filepath.Join(s.dir, name, "0"), "leftover")
	}
	first := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "restarting", first.State, first.Error)
	for _, name := range []string{"backup-deadbeef", "stage-123", "rollback-456"} {
		require.NoDirExists(t, filepath.Join(s.dir, name))
	}
	dirs := backupDirs(t, s)
	require.Len(t, dirs, 1, "only the backup backup.json references is kept")

	// A second update supersedes the first backup.
	*install = "v1.2.0"
	s2 := versionedService(t, filepath.Dir(s.dir), bins, "v1.2.0")
	for i := range s2.binaries {
		s2.binaries[i].Version = "v1.1.0"
	}
	second := applyCustom(t, s2, ApplyOptions{})
	require.Equal(t, "restarting", second.State, second.Error)
	now := backupDirs(t, s2)
	require.Len(t, now, 1)
	require.NotEqual(t, dirs[0], now[0], "the superseded backup is pruned")

	// Rollback still restores the release the latest backup holds.
	s3 := versionedService(t, filepath.Dir(s.dir), bins, "v1.2.0")
	rb, err := s3.Start(Request{Action: "rollback"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Equal(t, "restarting", waitJob(t, s3, rb.ID).State)
	requireContents(t, bins, "v1.1.0")
}

// A backup.json that cannot be read keeps every backup dir: pruning never
// guesses which copy a rollback might need.
func TestPruneKeepsBackupsWhenTheManifestIsUnreadable(t *testing.T) {
	s, _, _ := customFleet(t)
	writeBin(t, filepath.Join(s.dir, "backup-old", "0"), "copy")
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, "backup.json"), []byte("{"), 0o600))
	s.pruneStale("stage-", "backup-", "rollback-")
	require.DirExists(t, filepath.Join(s.dir, "backup-old"))
}

// A custom command that fails part way keeps the backups of what it
// replaced, so a rollback restores the binaries it left mixed.
func TestFailedCustomCommandKeepsBackupsOfReplacedBinaries(t *testing.T) {
	s, bins, _ := customFleet(t)
	h := &fakeHost{path: map[string]string{}, run: func([]string, []string) ([]byte, error) {
		writeBin(t, bins[0].Path, "v1.1.0")
		writeBin(t, bins[1].Path, "v1.1.0")
		return nil, errors.New("exit status 1")
	}}
	for _, b := range bins {
		h.path[b.Name] = b.Path
	}
	h.install(t)
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "failed", job.State)
	require.Contains(t, job.Error, "custom update command failed")
	require.True(t, s.Status().RollbackAvailable)
	var manifest backupManifest
	require.NoError(t, readJSON(filepath.Join(s.dir, "backup.json"), &manifest))
	require.Len(t, manifest.Entries, 2, "only the replaced binaries are kept")

	rb, err := s.Start(Request{Action: "rollback"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Equal(t, "restarting", waitJob(t, s, rb.ID).State)
	requireContents(t, bins, "v1.0.0")
}
